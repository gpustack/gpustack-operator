/*
 * rocm_cumask_check.c — decide whether a CU mask took effect, by running under it and reading the
 * hardware back.
 *
 * WHY THIS EXISTS. A CU mask fails OPEN, and it does so silently: the runtime that rejects one
 * returns no error, logs no line, and changes no return code. What the container loses is not some
 * of its isolation but all of it. Four such constructions were measured, and each looks perfectly
 * reasonable written down: a CU set that splits a WGP pair on RDNA discards the entire mask; a
 * `ROC_GLOBAL_CU_MASK` whose bits all sit at or above the WGP count is ignored in full; a
 * `GPU_list` written as a UUID rather than an index drops its whole segment; and on a multi-XCC
 * part a mask that places no bit in some XCC leaves that XCC running unmasked. Nothing on the
 * platform reports any of them, so this reports them.
 *
 * WHY IT JUDGES BY OCCUPANCY AND NOT BY THROUGHPUT. A throughput comparison passes the worst of
 * those failures. On a 304-CU `gfx942` part the one-bit mask `0:0` measured 3.7 % of the card's
 * throughput — exactly the slice its bit count suggests — while the container's waves were reaching
 * 267 of the card's 304 CUs, because seven of the eight XCCs had received no mask at all. The
 * makespan is set by the most constrained XCC and says nothing about the other seven. So this
 * launches its own kernel, has each wave read its own physical identity out of the hardware, and
 * compares the units it actually ran on against the units the mask asked for.
 *
 * WHY IT RE-EXECS ITSELF. ROCr reads `HSA_CU_MASK` once, while it initialises, which is before any
 * code here could set it. A probe that derived a mask and then launched a kernel in the same
 * process would measure the environment it started with. So the derivation writes the variable and
 * `execv`s, and the second run sees a mask in its environment and verifies it. That also gives the
 * tool its whole external shape: a mask already in the environment is verified as it stands, which
 * is how a case points this at one of the fail-open constructions above.
 *
 * THE UNIT COMPARED DIFFERS BY ARCHITECTURE — the CU on CDNA, the WGP on RDNA — and each matches
 * that architecture's allocation atom. `device/vrocm_hwid.h` holds the registers and the reasoning;
 * it is shared with the soak under testing/ so the two cannot disagree about what they measured.
 *
 * TOPOLOGY COMES FROM THE HSA AGENT-INFO API, never from KFD sysfs. Both agree, on both
 * architectures, so this is about contract stability rather than correctness — but one property of
 * what they report is a trap either way: the shader-engine count is DEVICE-WIDE and already carries
 * the XCC multiplier, so `NUM_SHADER_ENGINES` reading 32 on an eight-XCC part means four per XCC,
 * not 32. Nothing below divides by `NUM_XCC` to undo that, and that is the point rather than an
 * oversight: the only rule that uses the engine count is RDNA's alignment, where the count is
 * already per-XCC because there is one XCC, and the CDNA rules are stated in XCCs and CUs and never
 * need a per-XCC engine count at all.
 *
 * IT IS COMPILED AS HIP RATHER THAN AS C, because it carries device code. `build.sh` passes
 * `-x hip` explicitly: hipcc compiles a `.c` file as C — measured, it does not even put the HIP
 * headers on the include path when it does — and the kernel below would not survive that.
 *
 * EXIT CODES, because its intended caller is the detector, which declines to advertise a sliced
 * capability for a card that fails rather than advertise one the node will not honour: 0 the mask
 * took effect as asked; 1 it did not, which is the finding this tool exists to make; 2 the probe
 * could not run — no agent, a request that cannot be honoured at all, or a malformed argument.
 */
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <hip/hip_runtime.h>
#include <hsa/hsa.h>
#include <hsa/hsa_ext_amd.h>

#include "device/vrocm_hwid.h"

#define TOOL "rocm-cumask-check"

/* Wide enough for any mask the platform can carry today (the largest part masks 304 CUs) with room
 * to spare, and small enough to keep the parse a flat array rather than a bitmap with arithmetic. */
#define MAX_MASK_BITS 1024u

#define MAX_XCC VROCM_HWID_MAX_XCC
#define SLOT_COUNT VROCM_HWID_SLOTS
#define SLOT_SINK SLOT_COUNT

#define MAX_AGENTS 64u

/* ---- the kernel ---------------------------------------------------------------------------- */

/* vrocm_occupy — spin, then record where the spin happened.
 *
 * The spin is what makes the readout complete rather than approximate: a kernel that returns
 * immediately retires before the dispatcher has finished spreading its workgroups, so it reports a
 * subset of the enabled units and a mask that took effect reads as one that did not. The sink store
 * is what keeps the spin alive — without a use of the accumulator the loop is dead code and the
 * compiler deletes it, and the condition can never be proven false because the seed is a thread
 * index. */
__global__ void vrocm_occupy(unsigned int *slots, unsigned int spin)
{
    unsigned int acc = threadIdx.x + blockIdx.x * blockDim.x + 1u;
    unsigned int i;

    for (i = 0; i < spin; i++) {
        acc = acc * 1664525u + 1013904223u;
    }
    slots[vrocm_hwid_slot()] = 1u;
    if (acc == 0u) {
        slots[SLOT_SINK] = acc;
    }
}

/* ---- reporting ---------------------------------------------------------------------------- */

static int fails;

static void check_that(int ok, const char *name, const char *fmt, ...)
{
    va_list ap;

    printf("%s | %s | ", ok ? "PASS" : "FAIL", name);
    va_start(ap, fmt);
    vprintf(fmt, ap);
    va_end(ap);
    printf("\n");
    if (!ok) {
        fails++;
    }
}

/* note — a property that costs the container throughput but not isolation. It is reported and not
 * counted, because a mask that wastes capacity still confines the workload, and failing on it would
 * make the probe refuse allocations the platform honours. */
static void note(const char *name, const char *fmt, ...)
{
    va_list ap;

    printf("WARN | %s | ", name);
    va_start(ap, fmt);
    vprintf(fmt, ap);
    va_end(ap);
    printf("\n");
}

/* ---- topology ----------------------------------------------------------------------------- */

struct topology {
    char name[64];
    unsigned int cu;
    unsigned int se;        /* device-wide, already multiplied by the XCC count */
    unsigned int sa_per_se; /* per shader engine, NOT multiplied */
    unsigned int xcc;
};

struct agent_list {
    hsa_agent_t agents[MAX_AGENTS];
    unsigned int count;
};

/* collect_gpu — accumulate the GPU agents in enumeration order, skipping an agent that will not say
 * what it is rather than aborting the walk: one unreadable agent must not cost the card behind it. */
static hsa_status_t collect_gpu(hsa_agent_t agent, void *data)
{
    struct agent_list *list = (struct agent_list *)data;
    hsa_device_type_t type;

    if (hsa_agent_get_info(agent, HSA_AGENT_INFO_DEVICE, &type) != HSA_STATUS_SUCCESS) {
        return HSA_STATUS_SUCCESS;
    }
    if (type == HSA_DEVICE_TYPE_GPU && list->count < MAX_AGENTS) {
        list->agents[list->count++] = agent;
    }
    return HSA_STATUS_SUCCESS;
}

/* topology_read — the four agent-info fields the derivation needs, for one card.
 *
 * The index is a position in the GPU-agent enumeration, which is the index space HSA_CU_MASK's
 * GPU_list uses: ROCr has already applied ROCR_VISIBLE_DEVICES by the time agents are iterated, so
 * filtering and reordering are reflected here without this having to read the variable. */
static int topology_read(unsigned int device, struct topology *out)
{
    struct agent_list list;
    hsa_agent_t agent;

    list.count = 0;
    if (hsa_init() != HSA_STATUS_SUCCESS) {
        fprintf(stderr, TOOL ": cannot initialise ROCr\n");
        return 0;
    }
    if (hsa_iterate_agents(collect_gpu, &list) != HSA_STATUS_SUCCESS) {
        fprintf(stderr, TOOL ": cannot iterate agents\n");
        return 0;
    }
    if (device >= list.count) {
        fprintf(stderr, TOOL ": device %u of %u GPU agents\n", device, list.count);
        return 0;
    }
    agent = list.agents[device];

    memset(out, 0, sizeof(*out));
    if (hsa_agent_get_info(agent, HSA_AGENT_INFO_NAME, out->name) != HSA_STATUS_SUCCESS ||
        hsa_agent_get_info(agent, (hsa_agent_info_t)HSA_AMD_AGENT_INFO_COMPUTE_UNIT_COUNT,
                           &out->cu) != HSA_STATUS_SUCCESS ||
        hsa_agent_get_info(agent, (hsa_agent_info_t)HSA_AMD_AGENT_INFO_NUM_SHADER_ENGINES,
                           &out->se) != HSA_STATUS_SUCCESS ||
        hsa_agent_get_info(agent, (hsa_agent_info_t)HSA_AMD_AGENT_INFO_NUM_SHADER_ARRAYS_PER_SE,
                           &out->sa_per_se) != HSA_STATUS_SUCCESS) {
        fprintf(stderr, TOOL ": device %u does not report its topology\n", device);
        return 0;
    }
    /* The one field an older ROCr may not carry. One XCC is the honest default rather than a
     * failure: it is what every part predating the attribute has. */
    if (hsa_agent_get_info(agent, (hsa_agent_info_t)HSA_AMD_AGENT_INFO_NUM_XCC, &out->xcc) !=
            HSA_STATUS_SUCCESS ||
        out->xcc == 0u) {
        out->xcc = 1u;
    }
    if (out->cu == 0u || out->se == 0u || out->xcc > MAX_XCC) {
        fprintf(stderr, TOOL ": device %u reports an unusable topology: cu=%u se=%u xcc=%u\n",
                device, out->cu, out->se, out->xcc);
        return 0;
    }
    return 1;
}

/* ---- the mask ----------------------------------------------------------------------------- */

struct cu_mask {
    unsigned char bit[MAX_MASK_BITS];
    unsigned int highest; /* one past the highest index set; 0 when nothing is set */
    unsigned int count;
    int applies;     /* some segment named this device */
    int non_numeric; /* a GPU_list item was not a decimal index */
    int overflow;    /* an index beyond what this parser holds */
    int malformed;
};

static const char *read_uint(const char *s, const char *end, unsigned int *out, int *ok)
{
    unsigned int value = 0;
    int digits = 0;

    while (s < end && *s >= '0' && *s <= '9') {
        value = value * 10u + (unsigned int)(*s - '0');
        s++;
        digits++;
    }
    *ok = digits > 0;
    *out = value;
    return s;
}

static const char *skip_item(const char *s, const char *end)
{
    while (s < end && *s != ',') {
        s++;
    }
    return (s < end) ? s + 1 : s;
}

/* gpu_list_names — does this comma-separated list name `device`?
 *
 * A non-decimal item is recorded rather than merely skipped, because it is a failure mode in its
 * own right: ROCr's parser takes indices only, and a `GPU-<uuid>` segment it cannot parse is
 * dropped in full, leaving the card unmasked with nothing said. */
static int gpu_list_names(const char *s, const char *end, unsigned int device, int *non_numeric)
{
    int found = 0;

    while (s < end) {
        unsigned int lo, hi;
        int ok;

        s = read_uint(s, end, &lo, &ok);
        if (!ok) {
            *non_numeric = 1;
            s = skip_item(s, end);
            continue;
        }
        hi = lo;
        if (s < end && *s == '-') {
            s = read_uint(s + 1, end, &hi, &ok);
            if (!ok) {
                *non_numeric = 1;
            }
        }
        if (device >= lo && device <= hi) {
            found = 1;
        }
        s = skip_item(s, end);
    }
    return found;
}

static void mask_set(struct cu_mask *m, unsigned int index)
{
    if (index >= MAX_MASK_BITS) {
        m->overflow = 1;
        return;
    }
    if (!m->bit[index]) {
        m->bit[index] = 1;
        m->count++;
    }
    if (index + 1u > m->highest) {
        m->highest = index + 1u;
    }
}

/* cu_list_set — set the bits a comma-separated CU list names, `n` or `a-b` per item. An item it
 * cannot read stops the walk rather than skipping to the next: the rest of the list is no longer
 * trustworthy, and half a mask read as a whole one is a wrong expectation rather than a missing
 * one. */
static void cu_list_set(const char *s, const char *end, struct cu_mask *m)
{
    while (s < end) {
        unsigned int lo, hi, i;
        int ok;

        s = read_uint(s, end, &lo, &ok);
        if (!ok) {
            m->malformed = 1;
            return;
        }
        hi = lo;
        if (s < end && *s == '-') {
            s = read_uint(s + 1, end, &hi, &ok);
            if (!ok) {
                m->malformed = 1;
                return;
            }
        }
        for (i = lo; i <= hi && i < MAX_MASK_BITS; i++) {
            mask_set(m, i);
        }
        if (hi >= MAX_MASK_BITS) {
            m->overflow = 1;
        }
        s = skip_item(s, end);
    }
}

/* parse_hsa_cu_mask — `GPU_list:CU_list[;GPU_list:CU_list...]`, where bit i of a CU_list is CU i. */
static void parse_hsa_cu_mask(const char *spec, unsigned int device, struct cu_mask *m)
{
    const char *p = spec;

    while (*p != '\0') {
        const char *seg_end = strchr(p, ';');
        const char *colon;

        if (seg_end == NULL) {
            seg_end = p + strlen(p);
        }
        colon = (const char *)memchr(p, ':', (size_t)(seg_end - p));
        if (colon == NULL) {
            m->malformed = 1;
        } else if (gpu_list_names(p, colon, device, &m->non_numeric)) {
            m->applies = 1;
            cu_list_set(colon + 1, seg_end, m);
        }
        p = (*seg_end == ';') ? seg_end + 1 : seg_end;
    }
}

/* parse_global_cu_mask — a plain hex bitmask, whose bit i is a WGP on RDNA and a CU on CDNA. That
 * asymmetry is the same one that makes `multiProcessorCount` mean different things on the two
 * architectures, and it is why this parser leaves the unit to the caller. */
static void parse_global_cu_mask(const char *spec, struct cu_mask *m)
{
    size_t len, i;

    if (spec[0] == '0' && (spec[1] == 'x' || spec[1] == 'X')) {
        spec += 2;
    }
    len = strlen(spec);
    if (len == 0) {
        m->malformed = 1;
        return;
    }
    for (i = 0; i < len; i++) {
        char c = spec[len - 1 - i];
        unsigned int nibble, b;

        if (c >= '0' && c <= '9') {
            nibble = (unsigned int)(c - '0');
        } else if (c >= 'a' && c <= 'f') {
            nibble = (unsigned int)(c - 'a') + 10u;
        } else if (c >= 'A' && c <= 'F') {
            nibble = (unsigned int)(c - 'A') + 10u;
        } else {
            m->malformed = 1;
            return;
        }
        for (b = 0; b < 4u; b++) {
            if ((nibble & (1u << b)) != 0u) {
                mask_set(m, (unsigned int)i * 4u + b);
            }
        }
    }
    m->applies = 1;
}

/* expand_wgps_to_cus — restate a WGP-indexed mask in CU indices, so one set of rules reads both
 * variables. Rebuilt into a fresh mask rather than doubled in place: the counters have to come out
 * consistent with the bits, and an in-place expansion writes over indices it has yet to read. */
static void expand_wgps_to_cus(struct cu_mask *m, unsigned int wgps)
{
    struct cu_mask out;
    unsigned int w;

    memset(&out, 0, sizeof(out));
    out.applies = m->applies;
    out.non_numeric = m->non_numeric;
    out.overflow = m->overflow;
    out.malformed = m->malformed;
    for (w = 0; w < wgps && w < MAX_MASK_BITS; w++) {
        if (m->bit[w]) {
            mask_set(&out, 2u * w);
            mask_set(&out, 2u * w + 1u);
        }
    }
    *m = out;
}

/* ---- the conformance rules ---------------------------------------------------------------- */

struct expectation {
    unsigned int units;
    unsigned int per_xcc[MAX_XCC];
};

/* check_bits_in_range — a bit at or above the CU count is outside what the runtime accepts, and
 * whether it drops that bit or the whole mask was never measured; either reading is a mask that
 * does not confine what the caller thinks it confines. */
static void check_bits_in_range(const struct topology *t, const struct cu_mask *m)
{
    if (m->overflow) {
        /* Reported rather than left to the comparison downstream, which would notice the mismatch
         * and blame the mask for a limit of this parser's. */
        check_that(0, "mask/bits_in_range", "an index beyond the %u this reader holds", MAX_MASK_BITS);
        return;
    }
    if (m->count == 0u) {
        check_that(1, "mask/bits_in_range", "no bits set");
        return;
    }
    check_that(m->highest <= t->cu, "mask/bits_in_range", "highest index %u against %u CUs",
               m->highest - 1u, t->cu);
}

/* check_rdna — the mask rules for a single-XCC part, where the atom is the WGP pair. */
static void check_rdna(const struct topology *t, const struct cu_mask *m, struct expectation *e)
{
    unsigned int wgps = t->cu / 2u;
    unsigned int split = 0, whole = 0, w;

    check_bits_in_range(t, m);

    for (w = 0; w < wgps; w++) {
        unsigned int a = m->bit[2u * w], b = m->bit[2u * w + 1u];

        if (a != b) {
            split++;
        } else if (a) {
            whole++;
        }
    }
    /* An orphaned CU does not shrink the mask by one CU — the runtime judges the whole set invalid
     * and hands back the entire card. Measured: `0:0-14` occupies all 60 CUs, while `0:0-13`, one
     * CU smaller, correctly confines the container to 7 WGPs. */
    check_that(split == 0, "mask/wgp_pairs_whole",
               "%u whole WGP pairs, %u split", whole, split);

    /* The kernel hands mask bits to shader engines round-robin, so a WGP count that is not a
     * multiple of the engine count leaves a remainder that yields no throughput at all. Measured
     * across 18 sample points: 3 and 4 WGPs deliver the same figure, as do 6 and 8, and 28 and 29. */
    if (whole % t->se != 0u) {
        note("mask/shader_engine_aligned",
             "a %u-WGP set is not a multiple of %u shader engines; the remainder yields nothing",
             whole, t->se);
    }

    memset(e, 0, sizeof(*e));
    e->units = whole;
}

/* check_global_mask — the rules that belong to ROC_GLOBAL_CU_MASK alone, and the restatement that
 * lets the per-architecture rules read it.
 *
 * Its bit i is a WGP on RDNA and a CU on CDNA — the same asymmetry that makes
 * `hipDeviceProp_t.multiProcessorCount` report half the CU count on one architecture and all of it
 * on the other. Either way, a bit landing at or above the width is dropped without a word, so a
 * mask whose bits ALL land there leaves the card entirely unmasked. */
static void check_global_mask(const struct topology *t, struct cu_mask *m)
{
    unsigned int width = (t->xcc > 1u) ? t->cu : t->cu / 2u;
    unsigned int within = 0, b;

    for (b = 0; b < width && b < MAX_MASK_BITS; b++) {
        within += m->bit[b];
    }
    check_that(within > 0, "mask/bits_within_width", "%u of %u bits below the %u-%s width", within,
               m->count, width, t->xcc > 1u ? "CU" : "WGP");
    if (m->highest > width) {
        note("mask/bits_above_width", "bits at or above %u are ignored", width);
    }
    if (t->xcc == 1u) {
        expand_wgps_to_cus(m, width);
    }
}

/* check_cdna — the mask rules for a multi-XCC part, where the atom is one CU in every XCC.
 *
 * The bit mapping is read out of the hardware rather than inferred: bit i lands on XCC (i mod X),
 * so eight consecutive bits are eight different XCCs with one CU each — never eight CUs of one
 * XCC. Everything below follows from that one fact. */
static void check_cdna(const struct topology *t, const struct cu_mask *m, struct expectation *e)
{
    unsigned int covered = 0, i;

    check_bits_in_range(t, m);

    memset(e, 0, sizeof(*e));
    for (i = 0; i < m->highest && i < t->cu; i++) {
        if (m->bit[i]) {
            e->per_xcc[i % t->xcc]++;
            e->units++;
        }
    }
    for (i = 0; i < t->xcc; i++) {
        if (e->per_xcc[i] != 0u) {
            covered++;
        }
    }
    /* The failure with no RDNA analogue and no single-XCC witness: an XCC that receives no bit is
     * not given a small share, it is left unmasked. Measured, the one-bit mask `0:0` occupied 267
     * of 304 CUs and `0:0-3` occupied 156, in both cases because the XCCs the mask never mentioned
     * ran the container's waves at full width. */
    check_that(covered == t->xcc, "mask/every_xcc_covered", "%u of %u XCCs carry a bit", covered,
               t->xcc);

    if (e->units % t->xcc != 0u) {
        note("mask/xcc_atom_aligned",
             "a %u-CU set is not a multiple of %u XCCs; the remainder is occupied but unusable",
             e->units, t->xcc);
    }
}

/* ---- occupancy ---------------------------------------------------------------------------- */

/* occupancy — the set of physical units this process's own waves ran on.
 *
 * Oversubscribed eight to one and repeated, because the comparison downstream is an exact one: the
 * risk this guards is a unit the mask enabled that no workgroup happened to land on, which would
 * report a working mask as a broken one. Repeats accumulate into the same table, so the result is
 * the union across launches. */
static int occupancy(unsigned int device, const struct topology *t, unsigned int *found,
                     unsigned int *per_xcc)
{
    unsigned int host[SLOT_COUNT + 1];
    unsigned int *slots = NULL;
    unsigned int blocks = 8u * t->cu;
    unsigned int round, i;
    hipError_t rc;

    /* The kernel has to land on the card whose topology was read, and HIP's current device is 0
     * until something says otherwise -- so without this a --device 1 run would report card 1's
     * topology against card 0's occupancy and call the disagreement a mask failure. */
    rc = hipSetDevice((int)device);
    if (rc != hipSuccess) {
        fprintf(stderr, TOOL ": cannot select HIP device %u: %s\n", device, hipGetErrorString(rc));
        return 0;
    }
    rc = hipMalloc((void **)&slots, sizeof(host));
    if (rc != hipSuccess) {
        fprintf(stderr, TOOL ": cannot allocate the slot table: %s\n", hipGetErrorString(rc));
        return 0;
    }
    rc = hipMemset(slots, 0, sizeof(host));
    for (round = 0; rc == hipSuccess && round < 3u; round++) {
        vrocm_occupy<<<blocks, 64u>>>(slots, 200000u);
        rc = hipDeviceSynchronize();
    }
    if (rc == hipSuccess) {
        rc = hipMemcpy(host, slots, sizeof(host), hipMemcpyDeviceToHost);
    }
    (void)hipFree(slots);
    if (rc != hipSuccess) {
        fprintf(stderr, TOOL ": the occupancy probe failed: %s\n", hipGetErrorString(rc));
        return 0;
    }

    *found = 0;
    for (i = 0; i < MAX_XCC; i++) {
        per_xcc[i] = 0;
    }
    for (i = 0; i < SLOT_COUNT; i++) {
        if (host[i]) {
            (*found)++;
            per_xcc[VROCM_HWID_SLOT_XCC(i)]++;
        }
    }
    return 1;
}

/* ---- derivation --------------------------------------------------------------------------- */

/* derive_and_reexec — turn a percentage into a mask, put it in the environment and start over.
 *
 * The two branches share no arithmetic, deliberately. Both are correct-looking and each is silently
 * wrong on the other architecture: carrying RDNA's pairing rule onto CDNA doubles every slice
 * without failing, and carrying CDNA's atom onto RDNA splits pairs and loses the mask entirely. */
static int derive_and_reexec(char **argv, unsigned int device, unsigned int percent,
                             const struct topology *t)
{
    char spec[64];
    unsigned int last;

    if (t->xcc > 1u) {
        unsigned int n = (t->cu * percent + 50u) / 100u;

        n = (n / t->xcc) * t->xcc;
        if (n == 0u) {
            /* Not a clamp. A sub-atom mask does not confine the container to a small slice, it
             * leaves most of the card open, so a request below one atom cannot be honoured at all —
             * on an eight-XCC 304-CU part the smallest honourable slice is 8 CUs, 2.63 %. */
            fprintf(stderr,
                    TOOL ": %u%% of %u CUs is below one %u-CU atom; this request cannot be"
                         " honoured, and rounding it up or down would not confine the container\n",
                    percent, t->cu, t->xcc);
            return 2;
        }
        last = n - 1u;
    } else {
        unsigned int wgps = t->cu / 2u;
        unsigned int n = (wgps * percent + 50u) / 100u;

        n = (n / t->se) * t->se;
        if (n < t->se) {
            n = t->se;
        }
        if (n > wgps) {
            n = wgps;
        }
        last = 2u * n - 1u;
    }
    snprintf(spec, sizeof(spec), "%u:0-%u", device, last);
    printf("derive percent=%u mask=%s\n", percent, spec);
    fflush(stdout);

    if (setenv("HSA_CU_MASK", spec, 1) != 0) {
        fprintf(stderr, TOOL ": cannot set HSA_CU_MASK\n");
        return 2;
    }
    /* /proc/self/exe rather than argv[0], which is not a usable path when the tool was found on
     * PATH. The child sees a mask in its environment and takes the verifying path, so this
     * terminates after exactly one hop. */
    execv("/proc/self/exe", argv);
    fprintf(stderr, TOOL ": cannot re-exec to apply the mask: ROCr reads HSA_CU_MASK when it"
                    " initialises, which has already happened here\n");
    return 2;
}

/* ---- entry point -------------------------------------------------------------------------- */

static void usage(void)
{
    fprintf(stderr,
            "usage: " TOOL " [--device N] [--percent P]\n"
            "  With no mask in the environment, derives a P%% mask (default 50) for device N and\n"
            "  re-execs under it. With HSA_CU_MASK or ROC_GLOBAL_CU_MASK already set, verifies\n"
            "  that mask as it stands.\n");
}

int main(int argc, char **argv)
{
    unsigned int device = 0, percent = 50;
    unsigned int occupied = 0, occupied_per_xcc[MAX_XCC];
    const char *hsa_spec, *global_spec;
    struct expectation expect;
    struct topology topo;
    struct cu_mask mask;
    int i;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--device") == 0 && i + 1 < argc) {
            device = (unsigned int)strtoul(argv[++i], NULL, 10);
        } else if (strcmp(argv[i], "--percent") == 0 && i + 1 < argc) {
            percent = (unsigned int)strtoul(argv[++i], NULL, 10);
        } else {
            usage();
            return 2;
        }
    }
    if (percent == 0u || percent > 100u) {
        fprintf(stderr, TOOL ": --percent %u is outside 1..100\n", percent);
        return 2;
    }

    if (!topology_read(device, &topo)) {
        return 2;
    }
    printf("topology device=%u name=%s cu=%u se=%u sa_per_se=%u xcc=%u unit=%s units=%u\n", device,
           topo.name, topo.cu, topo.se, topo.sa_per_se, topo.xcc, topo.xcc > 1u ? "cu" : "wgp",
           topo.xcc > 1u ? topo.cu : topo.cu / 2u);

    hsa_spec = getenv("HSA_CU_MASK");
    global_spec = getenv("ROC_GLOBAL_CU_MASK");
    if (hsa_spec == NULL && global_spec == NULL) {
        return derive_and_reexec(argv, device, percent, &topo);
    }

    /* HIP applies HIP_VISIBLE_DEVICES on top of the filtering ROCr has already done, which would
     * put the kernel on a different card than the one whose topology was just read. */
    if (getenv("HIP_VISIBLE_DEVICES") != NULL || getenv("CUDA_VISIBLE_DEVICES") != NULL) {
        note("env/hip_visible_devices",
             "set, so the HIP device index may not be the HSA agent index this masked");
    }

    memset(&mask, 0, sizeof(mask));
    if (hsa_spec != NULL) {
        printf("mask source=HSA_CU_MASK value=%s\n", hsa_spec);
        if (global_spec != NULL) {
            note("env/both_masks_set",
                 "ROC_GLOBAL_CU_MASK is set too; their interaction is not measured, so only"
                 " HSA_CU_MASK is verified here");
        }
        parse_hsa_cu_mask(hsa_spec, device, &mask);
        check_that(!mask.malformed, "mask/parses", "syntax is GPU_list:CU_list[;...]");
        /* The segment is dropped whole, so the card runs unmasked. A UUID GPU_list is the measured
         * way to arrive here by accident: it looks like an identifier the runtime would accept. */
        check_that(mask.applies, "mask/applies_to_device",
                   "%s", mask.applies ? "a segment names this device"
                                      : (mask.non_numeric
                                             ? "no segment names device by index; a GPU_list that"
                                               " is not a decimal index is discarded in full"
                                             : "no segment names this device"));
    } else {
        printf("mask source=ROC_GLOBAL_CU_MASK value=%s\n", global_spec);
        parse_global_cu_mask(global_spec, &mask);
        check_that(!mask.malformed, "mask/parses", "expected a hexadecimal bitmask");
        check_global_mask(&topo, &mask);
    }

    if (topo.xcc > 1u) {
        check_cdna(&topo, &mask, &expect);
    } else {
        check_rdna(&topo, &mask, &expect);
    }
    if (!occupancy(device, &topo, &occupied, occupied_per_xcc)) {
        return 2;
    }
    /* Compared exactly, including when the mask asks for nothing: a discarded mask leaves the
     * expectation at zero and the readout at the whole card, which states the fail-open as two
     * numbers rather than as an absence of evidence. */
    check_that(occupied == expect.units, "occupancy/units_match", "%s: masked %u, occupied %u",
               topo.xcc > 1u ? "CU" : "WGP", expect.units, occupied);

    if (topo.xcc > 1u) {
        unsigned int x, mismatched = 0;

        for (x = 0; x < topo.xcc; x++) {
            if (occupied_per_xcc[x] != expect.per_xcc[x]) {
                mismatched++;
            }
            printf("xcc %u expected=%u occupied=%u\n", x, expect.per_xcc[x], occupied_per_xcc[x]);
        }
        /* The per-XCC breakdown is what a throughput comparison cannot see: an XCC left unmasked
         * runs at full width while the makespan, set by the most constrained XCC, looks healthy. */
        check_that(mismatched == 0, "occupancy/per_xcc_match", "%u of %u XCCs differ", mismatched,
                   topo.xcc);
    }

    printf("FAILS=%d\n", fails);
    return fails == 0 ? 0 : 1;
}
