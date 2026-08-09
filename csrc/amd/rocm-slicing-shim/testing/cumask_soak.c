/*
 * cumask_soak.c — what a masked container actually gets: throughput, and the physical units it got
 * it from.
 *
 * THREE THINGS HERE ARE LOAD-BEARING RATHER THAN STYLISTIC, and each has a wrong measurement behind
 * it.
 *
 * THE BARRIER. Tenants that merely start at about the same time do not overlap for the whole run,
 * and each one measures part of its window with the card to itself. Without a rendezvous, N tenants
 * reported an aggregate ABOVE the card's peak — a physically impossible number that looks like a
 * result. So every tenant registers in one file, waits for the population to arrive, and reports the
 * instant it was released; a case that finds those instants far apart knows to discard the run
 * rather than believe it.
 *
 * THE SATURATING KERNEL. A latency-bound kernel does not fill a small partition, so a tenant given a
 * fraction of the card reports better than its share and every overlap reading is inflated. This one
 * runs eight independent FMA chains per thread, which keeps the ALUs busy on instruction-level
 * parallelism rather than on occupancy, so a small mask is as fully used as a large one. The chains
 * feed a sink the compiler cannot prove unreachable — without it the whole loop is dead code, and a
 * kernel that returns immediately measures the launch path.
 *
 * THE OCCUPANCY READOUT. Throughput alone cannot tell a mask that took effect from one that was
 * discarded on some XCCs: on a 304-CU part a one-bit mask measured a plausible 3.7 % of the card
 * while the container's waves reached 267 CUs, because the makespan is set by the most constrained
 * XCC and says nothing about the other seven. So every wave records where it ran, and each tenant
 * reports the number of distinct units it occupied beside its GFLOP/s. The decode lives in
 * `device/vrocm_hwid.h`, shared with `tools/rocm-cumask-check`, so the two cannot disagree about
 * what they measured.
 *
 * IT ASKS THE HARDWARE FOR NO TOPOLOGY AND LINKS NO HSA. Everything it reports about the card comes
 * out of its own waves. The XCC count it prints is the number of XCCs its waves were observed on,
 * which is the figure that matters here — an XCC the container never reached is one the mask kept
 * it out of, and that is the observation, not a gap in it.
 *
 * `--self-check` is the program asserting its own instrument: one tenant alone, then two tenants
 * under one mask, and the pair must add up to the one. If they add to more, the barrier is not
 * holding them together; if they add to much less, the kernel is not saturating. Either way every
 * overlap figure this program produces afterwards would be worthless.
 */
/* No _GNU_SOURCE here, unlike the C files in this tree: hipcc already defines it, and defining it
 * again is a warning -- and `build.sh` must be silent when it succeeds. */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include <hip/hip_runtime.h>

#include "device/vrocm_hwid.h"

#define TOOL "cumask_soak"

/* Eight chains keeps every ALU issue slot fed without spilling: the loop below carries one live
 * float per chain, and a wider set starts costing registers that occupancy needs more. */
#define CHAINS 8
#define BLOCKS 2048u
#define THREADS 256u
#define ITERS 1000u

/* Launches enqueued before each synchronisation, spread over several streams. Neither number is a
 * tuning knob; both correct a way one process understates the card.
 *
 * Synchronising after every launch drains the queue and leaves the card idle between them, and one
 * process pays that gap serially where two fill each other's. Measured at a batch of one: a whole
 * card read 21636 GFLOP/s solo while two tenants on it summed to 25015 — an aggregate above the
 * solo figure, the same shape of impossible number the barrier exists to prevent, arriving by a
 * different route.
 *
 * Batching alone did not close it, because launches on ONE stream are serialised against each
 * other: the tail wavefronts of each drain while the next cannot start. Two processes overlap those
 * drains through two queues, and a solo run needs several streams to do the same for itself. */
#define BATCH 16u
#define STREAMS 4u

#define SLOT_SINK VROCM_HWID_SLOTS

/* Two flops per fused multiply-add, once per chain, per thread, per iteration. */
#define FLOPS_PER_LAUNCH \
    ((double)BLOCKS * (double)THREADS * (double)CHAINS * (double)ITERS * 2.0)

__global__ void vrocm_saturate(float *sink, unsigned int *slots, unsigned int iters)
{
    float chain[CHAINS];
    float mul = 1.0000001f, add = 0.0000001f;
    unsigned int i;
    int k;

    for (k = 0; k < CHAINS; k++) {
        chain[k] = (float)(threadIdx.x + blockIdx.x + (unsigned int)k + 1u);
    }
    for (i = 0; i < iters; i++) {
        for (k = 0; k < CHAINS; k++) {
            chain[k] = fmaf(chain[k], mul, add);
        }
    }
    slots[vrocm_hwid_slot()] = 1u;

    {
        float total = 0.0f;

        for (k = 0; k < CHAINS; k++) {
            total += chain[k];
        }
        /* The chains all start positive and only grow, so this can never be taken — but the
         * compiler cannot prove that, which is exactly what keeps the loop above alive. */
        if (total < 0.0f) {
            sink[0] = total;
        }
    }
}

/* ---- clocks ------------------------------------------------------------------------------- */

static double monotonic_seconds(void)
{
    struct timespec ts;

    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

/* wall_seconds — CLOCK_REALTIME, deliberately, because the release instant is compared ACROSS
 * processes and a monotonic clock has no shared origin to compare against. */
static double wall_seconds(void)
{
    struct timespec ts;

    clock_gettime(CLOCK_REALTIME, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

/* ---- the barrier -------------------------------------------------------------------------- */

/* barrier_wait — register in the file, then wait for `tenants` registrations. Returns the instant
 * this process was released.
 *
 * One appended byte per tenant, and the file's size is the count. O_APPEND makes a one-byte write
 * atomic against every other tenant with no lock to take and nothing to clean up if a tenant dies
 * before it arrives — the wait simply times out, which is the outcome a case wants to see rather
 * than a hang. */
static double barrier_wait(const char *path, unsigned int tenants, double timeout)
{
    double deadline;
    int fd;

    fd = open(path, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0600);
    if (fd < 0) {
        fprintf(stderr, TOOL ": barrier %s: %s\n", path, strerror(errno));
        return -1.0;
    }
    if (write(fd, "1", 1) != 1) {
        fprintf(stderr, TOOL ": barrier %s: cannot register: %s\n", path, strerror(errno));
        close(fd);
        return -1.0;
    }
    close(fd);

    deadline = monotonic_seconds() + timeout;
    for (;;) {
        struct stat st;

        if (stat(path, &st) == 0 && (unsigned long long)st.st_size >= tenants) {
            return wall_seconds();
        }
        if (monotonic_seconds() > deadline) {
            fprintf(stderr, TOOL ": barrier %s: only %lld of %u tenants arrived in %.0fs\n", path,
                    (long long)(stat(path, &st) == 0 ? st.st_size : 0), tenants, timeout);
            return -1.0;
        }
        usleep(2000);
    }
}

/* ---- the measurement ---------------------------------------------------------------------- */

struct result {
    double gflops;
    double seconds;
    double released;
    unsigned int units;
    unsigned int xccs;
};

/* soak — launch until the deadline, then report the rate and where the work ran.
 *
 * Timed as a whole rather than per launch: the loop keeps the queue fed, so a per-launch timing
 * would charge each one for the previous one's drain. The slot table accumulates across every
 * launch, so what comes back is the union of every unit any wave touched. */
static int soak(int device, double seconds, struct result *out)
{
    unsigned int host[VROCM_HWID_SLOTS + 1];
    unsigned int *slots = NULL;
    unsigned int seen_xcc[VROCM_HWID_MAX_XCC];
    hipStream_t stream[STREAMS];
    float *sink = NULL;
    double started, elapsed;
    unsigned long long launches = 0;
    unsigned int i;
    hipError_t rc;

    rc = hipSetDevice(device);
    if (rc != hipSuccess) {
        fprintf(stderr, TOOL ": hipSetDevice(%d): %s\n", device, hipGetErrorString(rc));
        return 0;
    }
    if (hipMalloc((void **)&slots, sizeof(host)) != hipSuccess ||
        hipMalloc((void **)&sink, sizeof(float) * 4) != hipSuccess ||
        hipMemset(slots, 0, sizeof(host)) != hipSuccess) {
        fprintf(stderr, TOOL ": cannot set up the device buffers\n");
        return 0;
    }
    for (i = 0; i < STREAMS; i++) {
        if (hipStreamCreate(&stream[i]) != hipSuccess) {
            fprintf(stderr, TOOL ": cannot create stream %u\n", i);
            return 0;
        }
    }

    /* One launch outside the window, because the first one pays for module load and code-object
     * relocation and would otherwise be charged to the card. */
    vrocm_saturate<<<BLOCKS, THREADS, 0, stream[0]>>>(sink, slots, ITERS);
    rc = hipDeviceSynchronize();
    if (rc != hipSuccess) {
        fprintf(stderr, TOOL ": warm-up launch failed: %s\n", hipGetErrorString(rc));
        return 0;
    }

    started = monotonic_seconds();
    do {
        for (i = 0; i < BATCH; i++) {
            vrocm_saturate<<<BLOCKS, THREADS, 0, stream[i % STREAMS]>>>(sink, slots, ITERS);
        }
        rc = hipDeviceSynchronize();
        if (rc != hipSuccess) {
            fprintf(stderr, TOOL ": launch failed: %s\n", hipGetErrorString(rc));
            return 0;
        }
        launches += BATCH;
        elapsed = monotonic_seconds() - started;
    } while (elapsed < seconds);

    for (i = 0; i < STREAMS; i++) {
        (void)hipStreamDestroy(stream[i]);
    }

    if (hipMemcpy(host, slots, sizeof(host), hipMemcpyDeviceToHost) != hipSuccess) {
        fprintf(stderr, TOOL ": cannot read the slot table back\n");
        return 0;
    }
    (void)hipFree(slots);
    (void)hipFree(sink);

    memset(seen_xcc, 0, sizeof(seen_xcc));
    out->units = 0;
    for (i = 0; i < VROCM_HWID_SLOTS; i++) {
        if (host[i]) {
            out->units++;
            seen_xcc[VROCM_HWID_SLOT_XCC(i)] = 1;
        }
    }
    out->xccs = 0;
    for (i = 0; i < VROCM_HWID_MAX_XCC; i++) {
        out->xccs += seen_xcc[i];
    }
    out->seconds = elapsed;
    out->gflops = (double)launches * FLOPS_PER_LAUNCH / elapsed / 1e9;
    return 1;
}

static void print_result(const char *label, int device, const struct result *r)
{
    printf("TENANT label=%s device=%d seconds=%.3f gflops=%.1f units=%u xccs=%u released=%.6f\n",
           label, device, r->seconds, r->gflops, r->units, r->xccs, r->released);
    fflush(stdout);
}

/* result_write / result_read — how a forked tenant hands its numbers back.
 *
 * A file per tenant rather than a pipe, because the tenants are exec'd rather than forked and a
 * pipe would have to survive the exec through an inherited descriptor number. A file is one path
 * argument and the case can read it too. */
static void result_write(const char *path, const struct result *r)
{
    FILE *f = fopen(path, "w");

    if (f == NULL) {
        fprintf(stderr, TOOL ": cannot write %s: %s\n", path, strerror(errno));
        return;
    }
    fprintf(f, "%.6f %.6f %.6f %u %u\n", r->gflops, r->seconds, r->released, r->units, r->xccs);
    fclose(f);
}

static int result_read(const char *path, struct result *r)
{
    FILE *f = fopen(path, "r");
    int fields;

    if (f == NULL) {
        return 0;
    }
    fields = fscanf(f, "%lf %lf %lf %u %u", &r->gflops, &r->seconds, &r->released, &r->units,
                    &r->xccs);
    fclose(f);
    return fields == 5;
}

/* ---- the self-check ------------------------------------------------------------------------ */

static int fails;

static void check_that(int ok, const char *name, const char *detail)
{
    printf("%s | %s | %s\n", ok ? "PASS" : "FAIL", name, detail);
    if (!ok) {
        fails++;
    }
}

/* spawn_tenant — one tenant, in its own process with its own ROCr init.
 *
 * exec rather than fork alone, deliberately: the mask is read when ROCr initialises, and this
 * process has already initialised. A forked child would inherit that state and measure the parent's
 * environment no matter what it was given. */
static pid_t spawn_tenant(const char *exe, int device, double seconds, const char *barrier,
                          unsigned int tenants, const char *label, const char *result_path)
{
    char s_device[32], s_seconds[32], s_tenants[32];
    pid_t pid = fork();

    if (pid != 0) {
        return pid;
    }
    snprintf(s_device, sizeof(s_device), "%d", device);
    snprintf(s_seconds, sizeof(s_seconds), "%.3f", seconds);
    snprintf(s_tenants, sizeof(s_tenants), "%u", tenants);

    execl(exe, exe, "--device", s_device, "--seconds", s_seconds, "--barrier", barrier, "--tenants",
          s_tenants, "--label", label, "--result", result_path, (char *)NULL);
    fprintf(stderr, TOOL ": cannot exec %s: %s\n", exe, strerror(errno));
    _exit(127);
}

static int self_check(int device, double seconds)
{
    struct result solo, a, b;
    char barrier[] = "/tmp/vrocm-soak-barrier-XXXXXX";
    char path_a[128], path_b[128];
    double sum, ratio, spread;
    char detail[256];
    pid_t pid_a, pid_b;
    int fd, status;

    fd = mkstemp(barrier);
    if (fd < 0) {
        fprintf(stderr, TOOL ": cannot make a barrier file: %s\n", strerror(errno));
        return 2;
    }
    close(fd);
    unlink(barrier); /* barrier_wait creates it; mkstemp only reserved the name */
    snprintf(path_a, sizeof(path_a), "%s.a", barrier);
    snprintf(path_b, sizeof(path_b), "%s.b", barrier);

    memset(&solo, 0, sizeof(solo));
    solo.released = wall_seconds();
    if (!soak(device, seconds, &solo)) {
        return 2;
    }
    print_result("solo", device, &solo);

    pid_a = spawn_tenant("/proc/self/exe", device, seconds, barrier, 2, "a", path_a);
    pid_b = spawn_tenant("/proc/self/exe", device, seconds, barrier, 2, "b", path_b);
    if (pid_a < 0 || pid_b < 0) {
        fprintf(stderr, TOOL ": cannot start the tenants\n");
        return 2;
    }
    waitpid(pid_a, &status, 0);
    waitpid(pid_b, &status, 0);

    if (!result_read(path_a, &a) || !result_read(path_b, &b)) {
        fprintf(stderr, TOOL ": a tenant produced no result\n");
        return 2;
    }
    /* The tenants printed their own lines on the inherited stdout; re-printing them here would
     * double every figure a case has to parse. */
    unlink(barrier);
    unlink(path_a);
    unlink(path_b);

    /* The saturation check. Two tenants under one mask divide it, so their sum is one tenant's
     * figure. A sum well above it means they were not overlapping — the barrier is not doing its
     * job. A sum well below means the kernel leaves the partition idle, which would understate
     * every slice this program ever measures. */
    sum = a.gflops + b.gflops;
    ratio = solo.gflops > 0.0 ? sum / solo.gflops : 0.0;
    snprintf(detail, sizeof(detail), "solo %.1f, a+b %.1f, ratio %.3f (want 0.85..1.15)",
             solo.gflops, sum, ratio);
    check_that(ratio >= 0.85 && ratio <= 1.15, "soak/two_tenants_sum_to_one", detail);

    /* And the barrier itself, reported as a number rather than assumed. */
    spread = a.released > b.released ? a.released - b.released : b.released - a.released;
    snprintf(detail, sizeof(detail), "released %.0f ms apart (want < 100)", spread * 1000.0);
    check_that(spread < 0.100, "soak/barrier_released_together", detail);

    snprintf(detail, sizeof(detail), "solo %u, a %u, b %u", solo.units, a.units, b.units);
    check_that(a.units == solo.units && b.units == solo.units, "soak/tenants_see_the_same_units",
               detail);

    printf("FAILS=%d\n", fails);
    return fails == 0 ? 0 : 1;
}

/* ---- entry point -------------------------------------------------------------------------- */

static void usage(void)
{
    fprintf(stderr,
            "usage: " TOOL " [--device N] [--seconds S] [--label L]\n"
            "               [--barrier PATH --tenants N] [--result PATH]\n"
            "       " TOOL " --self-check [--device N] [--seconds S]\n"
            "  With --barrier, waits for N tenants to register before measuring.\n"
            "  With --self-check, measures one tenant then two and asserts they add up.\n");
}

int main(int argc, char **argv)
{
    const char *barrier = NULL, *result_path = NULL, *label = "solo";
    unsigned int tenants = 1;
    double seconds = 3.0;
    struct result r;
    int device = 0, check = 0, i;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--device") == 0 && i + 1 < argc) {
            device = (int)strtol(argv[++i], NULL, 10);
        } else if (strcmp(argv[i], "--seconds") == 0 && i + 1 < argc) {
            seconds = strtod(argv[++i], NULL);
        } else if (strcmp(argv[i], "--label") == 0 && i + 1 < argc) {
            label = argv[++i];
        } else if (strcmp(argv[i], "--barrier") == 0 && i + 1 < argc) {
            barrier = argv[++i];
        } else if (strcmp(argv[i], "--tenants") == 0 && i + 1 < argc) {
            tenants = (unsigned int)strtoul(argv[++i], NULL, 10);
        } else if (strcmp(argv[i], "--result") == 0 && i + 1 < argc) {
            result_path = argv[++i];
        } else if (strcmp(argv[i], "--self-check") == 0) {
            check = 1;
        } else {
            usage();
            return 2;
        }
    }
    if (seconds <= 0.0) {
        fprintf(stderr, TOOL ": --seconds must be positive\n");
        return 2;
    }
    if (check) {
        return self_check(device, seconds);
    }

    memset(&r, 0, sizeof(r));
    if (barrier != NULL) {
        /* Generous against a tenant that has to page in a ROCm runtime before it can register, and
         * short enough that a case wedges for seconds rather than forever when one never does. */
        r.released = barrier_wait(barrier, tenants, 60.0);
        if (r.released < 0.0) {
            return 2;
        }
    } else {
        r.released = wall_seconds();
    }
    if (!soak(device, seconds, &r)) {
        return 2;
    }
    print_result(label, device, &r);
    if (result_path != NULL) {
        result_write(result_path, &r);
    }
    return 0;
}
