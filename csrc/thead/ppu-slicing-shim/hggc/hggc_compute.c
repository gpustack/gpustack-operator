/*
 * hggc_compute.c — the compute half of the quota: one duty-cycle window per card, closed and
 * opened by a PID loop fed with this container's own utilisation.
 *
 * WHAT IT ACTUATES. Each card carries a repeating window of HGGC_SM_CONTROL_PERIOD_MS. A launch
 * arriving in the first `allow_ns` of the window goes through; one arriving later waits for the
 * next window. Under a workload that launches back to back that yields a duty cycle of
 * allow_ns / period, which is the container's share of the card. The window lives in common/'s
 * region, so every process in the container divides ONE window rather than each getting its own
 * — the cap is the container's, not a process's.
 *
 * ONE WINDOW PER CARD MEANS ONE FIGURE PER CARD. A container holding two cards can be given half
 * of one and the whole of the other, so whether a launch waits at all is answered for the card
 * that launch names, not for the container.
 *
 * WHY A LOOP AND NOT A FORMULA. The utilisation a duty cycle buys is not the duty cycle: a
 * kernel launched just before the window closes keeps the card busy after it, and how much it
 * overruns depends on the workload. So the allowance is FEEDBACK-controlled — measure what the
 * container actually used, and move the window until that matches the cap. The arithmetic is in
 * common/vppu_pid.c, where it is exercised against a simulated card; this file is the part that
 * needs a real one.
 *
 * WHAT IT MEASURES. hgmlDeviceGetProcessUtilization, summed over this container's processes
 * only. Card-total would couple every container's loop on a shared card and oscillate, and Gate
 * 3 established on real hardware that the per-process query is supported, non-empty under load,
 * and reports the caller's own pid — which is what retired the card-total fallback.
 *
 * THE WINDOW AND THE LOOP RUN AT DIFFERENT RATES, and the second rate is a property of that
 * sensor rather than a preference. Traced on a PPU: the figure moves by about ten percentage
 * points per hundred milliseconds and no faster, in both directions, so a card that went from
 * idle to pinned reads 0, 10, 22, 32 … and needs a full second to say 100. Stepping the loop once
 * per window therefore acts on a figure up to a second stale in the direction the loop had just
 * moved, and the first build that did oscillated between the whole window and the floor with a
 * period of some seconds. Restamping the window is cheap and happens every period;
 * stepping happens once per HGGC_SM_CONTROL_STEP_MS, which defaults to that measured second.
 *
 * WHY HGML IS REACHED THROUGH dlopen. The shipped library's DT_NEEDED may name nothing but
 * libc.so.6 — case 1 asserts it — so it cannot link libhgml.so however convenient that would
 * be. The types still come from hgml.h through __typeof__, so the pointers this file calls
 * through are the header's own and not a hand-written guess at four signatures.
 *
 * FAIL-CLOSED, in the three shapes this can fail:
 *   - the container's configuration is unusable, or there is no current context: the launch is
 *     REFUSED, exactly as an allocation would be;
 *   - utilisation cannot be read at all: the loop holds the allowance it has and never opens it
 *     past the quota's own share. A missing feedback signal must not become a missing cap, which
 *     is what a fixed sleep or an opened window would make it;
 *   - the ledger is unreachable: refused, because a window that is not shared is not a cap on a
 *     container with more than one process in it.
 *
 * WHAT IS NOT GATED. Host callbacks run on the CPU, so delaying them frees none of the card.
 * They are counted like everything else and passed straight through.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <errno.h>
#include <time.h>
#include <unistd.h>

/* Neither SDK header includes what its own declarations need. */
#include <stdbool.h>
#include <stddef.h>

#include <hgml.h>

#include "common/vppu.h"
#include "common/vppu_ledger.h"
#include "common/vppu_pid.h"
#include "common/vppu_quota.h"
#include "hggc/hggc_quota.h"

/* One query returns every sample the driver still holds, so the buffer is sized for a
 * container's processes several times over rather than for one. A truncated answer is treated as
 * no answer: a partial sum reads as a container using less of the card than it is. */
#define VPPU_UTIL_MAX_SAMPLES 128u

/* The vendor entries this file needs, typed from hgml.h rather than retyped. */
static __typeof__(hgmlInit_v2) *hgml_init;
static __typeof__(hgmlDeviceGetHandleByIndex_v2) *hgml_handle_by_index;
static __typeof__(hgmlDeviceGetProcessUtilization) *hgml_process_utilization;

static bool hgml_ready;
static bool hgml_tried;
static int hgml_pid;

static hgmlDevice_t device_handle[VPPU_MAX_DEVICES];
static bool device_handle_ok[VPPU_MAX_DEVICES];

/* The container's configuration, read once per process.
 *
 * WHETHER compute is capped cannot change during a process's life — the library is preloaded
 * into a container that was sliced or was not — so it is latched here rather than re-read on a
 * path where getenv() would be a linear scan of the environment per launch. HOW TIGHT the cap is
 * IS re-read, once per control step, so a changed figure still takes effect: never freezing a
 * quota into the ledger is the trap this design avoids in HAMi-core.
 *
 * `any_capped` is the container's answer and `card_capped` each card's, because the cap is per
 * card: a container may hold one card at half a card's compute and another whole one. The
 * container-level latch is what keeps the launch path of an uncapped container free of even a
 * device query. */
static bool config_ready;
static bool any_capped;
static bool card_capped_known[VPPU_MAX_DEVICES];
static bool card_capped[VPPU_MAX_DEVICES];
static unsigned long long period_ns;
static unsigned long long step_ns;
static unsigned int graph_weight;
static struct vppu_pid_gains gains;

/* Set while this thread is inside a control step, so utilisation sampling that reaches back into
 * an interposed launch entry is counted and passed through instead of throttled — and cannot
 * recurse into another step. initial-exec for the reason common/'s ledger gives: the general
 * dynamic TLS model would put the dynamic linker in DT_NEEDED. */
static __thread bool in_control __attribute__((tls_model("initial-exec")));

/* The launch mix behind the utilisation figure, per process and per step interval.
 *
 * The driver reports ONE utilisation figure for a process, so graph and non-graph utilisation
 * cannot be read apart directly. What can be separated is the measurement itself: an interval in
 * which a graph launch was issued is accumulated apart from one in which none was, and the two
 * averages are reported side by side. That is the measurement the graph coefficient is waiting
 * on — if graphs escape the cap, the graph average sits above the plain one. */
static unsigned long long interval_launches;
static unsigned long long interval_graph_launches;
static unsigned long long graph_util_sum;
static unsigned long long graph_util_steps;
static unsigned long long plain_util_sum;
static unsigned long long plain_util_steps;

static unsigned long long now_ns(void)
{
    struct timespec ts;

    /* CLOCK_MONOTONIC because the window is a duration and a stepped wall clock must not open
     * or close it; it is also comparable across the processes sharing the region, which a
     * per-process clock would not be. */
    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) {
        return 0ULL;
    }
    return (unsigned long long)ts.tv_sec * 1000000000ULL + (unsigned long long)ts.tv_nsec;
}

static void sleep_ns(unsigned long long duration)
{
    struct timespec req;

    req.tv_sec = (time_t)(duration / 1000000000ULL);
    req.tv_nsec = (long)(duration % 1000000000ULL);
    while (nanosleep(&req, &req) != 0 && errno == EINTR) {
        /* A signal is not the end of the wait: a workload that takes SIGPROF for profiling would
         * otherwise be handed its launch early on every sample. */
    }
}

/* is_capped — whether a compute figure gives the launch path anything to wait for.
 *
 * 100 is a configured cap, not an absent one, and it caps nothing: leave the loop out of the
 * launch path entirely rather than run a controller whose error can only be positive. 0 means
 * unusable configuration, which vppu_hggc_gate() has already refused on. */
static bool is_capped(unsigned int target)
{
    return target > 0U && target < 100U;
}

static void load_config(void)
{
    /* Two threads racing here read the same environment and store the same values, so the
     * unguarded flag costs a duplicated parse at worst — the same trade the entry-point caches
     * in hggc_quota.c make, and for the same reason: a lock on the launch path is not free. */
    if (config_ready) {
        return;
    }
    config_ready = true;

    /* One scan of the table so an uncapped container answers the launch path from a single
     * boolean, as it did when the cap was one container-wide figure. Which card is capped is
     * asked per card below, once the launch has named one. */
    for (int i = 0; i < VPPU_MAX_DEVICES && !any_capped; i++) {
        any_capped = is_capped(vppu_quota_sm_percent(i));
    }
    period_ns = (unsigned long long)vppu_quota_sm_period_ms() * 1000000ULL;
    step_ns = (unsigned long long)vppu_quota_sm_step_ms() * 1000000ULL;
    graph_weight = vppu_quota_graph_weight();
    vppu_quota_sm_gains(&gains);
}

/* device_capped — whether this card's window has to be waited on, latched per card for the reason
 * the configuration above is latched at all. */
static bool device_capped(int device)
{
    if (!card_capped_known[device]) {
        card_capped[device] = is_capped(vppu_quota_sm_percent(device));
        card_capped_known[device] = true;
    }
    return card_capped[device];
}

/* resolve_hgml — bring up the management library this process reads utilisation from.
 *
 * Re-done in a forked child rather than inherited: an HGML handle is per process in the same way
 * an NVML one is, and a child that went on using the parent's would read whatever the driver
 * makes of a descriptor it did not open. The pid stamp is the same shape common/'s ledger uses
 * for the same reason — pthread_atfork would need -lpthread. */
static bool resolve_hgml(void)
{
    int self = (int)getpid();

    if (hgml_pid != self) {
        hgml_pid = self;
        hgml_tried = false;
        hgml_ready = false;
        for (int i = 0; i < VPPU_MAX_DEVICES; i++) {
            device_handle_ok[i] = false;
        }
    }
    if (hgml_tried) {
        return hgml_ready;
    }
    hgml_tried = true;

    /* RTLD_LOCAL: this handle is for reading a figure, and letting the whole process resolve
     * through it would change what the workload's own symbols bind to. */
    void *lib = dlopen("libhgml.so", RTLD_LAZY | RTLD_LOCAL);
    if (lib == NULL) {
        vppu_log(VPPU_LOG_DENY,
                 "cannot load libhgml.so (%s) — the compute window holds the quota's own share "
                 "and is never opened past it\n",
                 dlerror());
        return false;
    }

    hgml_init = dlsym(lib, "hgmlInit_v2");
    hgml_handle_by_index = dlsym(lib, "hgmlDeviceGetHandleByIndex_v2");
    hgml_process_utilization = dlsym(lib, "hgmlDeviceGetProcessUtilization");
    if (hgml_init == NULL || hgml_handle_by_index == NULL || hgml_process_utilization == NULL) {
        vppu_log(VPPU_LOG_DENY,
                 "libhgml.so lacks the utilisation entries — the compute window holds the "
                 "quota's own share\n");
        return false;
    }

    hgmlReturn_t rc = hgml_init();
    if (rc != HGML_SUCCESS) {
        vppu_log(VPPU_LOG_DENY,
                 "hgmlInit_v2 failed (%d) — the compute window holds the quota's own share\n",
                 (int)rc);
        return false;
    }

    hgml_ready = true;
    return true;
}

static bool device_by_index(int device, hgmlDevice_t *out)
{
    if (!device_handle_ok[device]) {
        /* The container-local index, exactly as the memory quota keys its figures: the SDK
         * renumbers devices inside a container, so a card passed through is index 0 whatever its
         * host ordinal names. */
        if (hgml_handle_by_index((unsigned int)device, &device_handle[device]) != HGML_SUCCESS) {
            return false;
        }
        device_handle_ok[device] = true;
    }
    *out = device_handle[device];
    return true;
}

/* newest_for_pid — is sample `index` the most recent one the driver holds for its process?
 *
 * The query is asked for all history, which is what Gate 3 measured it doing, so one process can
 * appear several times. Summing every appearance would multiply a container's utilisation by how
 * long the driver has been keeping samples. A scan rather than a map because the buffer is small
 * and this runs once a window: a hash table here would be more code to be wrong in. */
static bool newest_for_pid(const hgmlProcessUtilizationSample_t *samples, unsigned int count,
                           unsigned int index)
{
    for (unsigned int i = 0; i < count; i++) {
        if (i == index || samples[i].pid != samples[index].pid) {
            continue;
        }
        if (samples[i].timeStamp > samples[index].timeStamp) {
            return false;
        }
        /* Equal timestamps: keep exactly one of them, the first. */
        if (samples[i].timeStamp == samples[index].timeStamp && i < index) {
            return false;
        }
    }
    return true;
}

/* sample_util — this container's share of one card, or false when it cannot be read.
 *
 * "This container's" is decided by the ledger's own process table plus the caller's pid, not by
 * whether a pid resolves in this namespace: the region file is per container, so the pids in it
 * are this container's by construction, where a host pid may well also be a valid pid here. */
static bool sample_util(int device, unsigned int *out)
{
    if (!resolve_hgml()) {
        return false;
    }

    hgmlDevice_t handle;
    if (!device_by_index(device, &handle)) {
        return false;
    }

    hgmlProcessUtilizationSample_t samples[VPPU_UTIL_MAX_SAMPLES];
    unsigned int count = VPPU_UTIL_MAX_SAMPLES;
    if (hgml_process_utilization(handle, samples, &count, 0ULL) != HGML_SUCCESS) {
        return false;
    }
    if (count > VPPU_UTIL_MAX_SAMPLES) {
        return false;
    }

    int self = (int)getpid();
    unsigned long long total = 0ULL;
    for (unsigned int i = 0; i < count; i++) {
        int pid = (int)samples[i].pid;

        if (pid != self && !vppu_ledger_has_process(device, pid)) {
            continue;
        }
        if (!newest_for_pid(samples, count, i)) {
            continue;
        }
        total += samples[i].smUtil;
    }

    *out = (total > 100ULL) ? 100U : (unsigned int)total;
    return true;
}

/* note_launch_mix — fold one interval's measurement into the graph / non-graph averages and start
 * the next interval's counting. */
static void note_launch_mix(unsigned int measured)
{
    unsigned long long launches = __atomic_exchange_n(&interval_launches, 0ULL, __ATOMIC_RELAXED);
    unsigned long long graphs = __atomic_exchange_n(&interval_graph_launches, 0ULL,
                                                    __ATOMIC_RELAXED);

    if (launches == 0ULL) {
        return;
    }
    if (graphs > 0ULL) {
        graph_util_sum += measured;
        graph_util_steps++;
    } else {
        plain_util_sum += measured;
        plain_util_steps++;
    }
}

static unsigned long long average(unsigned long long sum, unsigned long long count)
{
    return (count == 0ULL) ? 0ULL : sum / count;
}

/* control_step — one pass of the loop, run by whichever thread of whichever process claimed the
 * step. Publishes the new allowance for every other process to read.
 *
 * The target is re-read here rather than cached with the rest of the configuration, so a changed
 * cap takes effect: freezing a quota into the ledger is HAMi-core's trap, where changing a limit
 * means deleting the cache file. */
static void control_step(int device, struct vppu_pid_state *state)
{
    struct vppu_pid_state local;
    unsigned int target = vppu_quota_sm_percent(device);
    unsigned int measured = 0U;

    in_control = true;
    bool sampled = sample_util(device, &measured);
    in_control = false;

    /* Only the stepping process writes these, so a plain read of the diagnostic words is safe;
     * the allowance is the one word other processes read on their launch path, so it is taken
     * and published with atomics. The two timestamps belong to this file, not to the
     * arithmetic — hence the zeroes. */
    local.window_start_ns = 0ULL;
    local.last_step_ns = 0ULL;
    local.allow_ns = __atomic_load_n(&state->allow_ns, __ATOMIC_ACQUIRE);
    local.integral = state->integral;
    local.last_error = state->last_error;

    if (sampled) {
        vppu_ledger_note_util(device, measured);
        vppu_pid_step(&local, &gains, target, measured, period_ns);
    } else {
        /* No measurement, so no loop: fall back to the quota's own share of the window — and
         * only ever downwards. A loop that had already squeezed below that share did so because
         * the workload overruns its windows, and forgetting that on a lost sample would hand
         * back compute the container was measured not to be entitled to. */
        unsigned long long floor_ns = vppu_pid_floor(target, period_ns);

        if (local.allow_ns == 0ULL || floor_ns < local.allow_ns) {
            local.allow_ns = floor_ns;
        }
    }
    __atomic_store_n(&state->allow_ns, local.allow_ns, __ATOMIC_RELEASE);
    state->integral = local.integral;
    state->last_error = local.last_error;

    note_launch_mix(measured);
    vppu_log(VPPU_LOG_DEBUG,
             "compute device=%d target=%u measured=%u%s allow_us=%llu period_us=%llu "
             "step_us=%llu graph_util=%llu/%llu plain_util=%llu/%llu\n",
             device, target, measured, sampled ? "" : " (unread)", local.allow_ns / 1000ULL,
             period_ns / 1000ULL, step_ns / 1000ULL, average(graph_util_sum, graph_util_steps),
             graph_util_steps, average(plain_util_sum, plain_util_steps), plain_util_steps);
}

/* maybe_step — run the loop if it is due, at most once per step interval across the whole
 * container.
 *
 * The exchange is what makes that "at most once": several processes reaching this in the same
 * interval all try, one wins, the rest carry on with what it publishes. A lock would be a system
 * call on a path that already pays for a clock read per launch. */
static void maybe_step(int device, struct vppu_pid_state *state, unsigned long long now)
{
    unsigned long long last = __atomic_load_n(&state->last_step_ns, __ATOMIC_ACQUIRE);

    /* `now < last` is not paranoia: the region file outlives a reboot, and a stamp from the
     * previous boot's monotonic clock can sit in the future. Treating it as due restamps it,
     * where waiting for it would leave the loop frozen until the clock caught up. */
    if (last != 0ULL && now >= last && now - last < step_ns) {
        return;
    }
    if (!__atomic_compare_exchange_n(&state->last_step_ns, &last, now, false, __ATOMIC_ACQ_REL,
                                     __ATOMIC_ACQUIRE)) {
        return;
    }
    control_step(device, state);
}

/* wait_for_window — hold the caller until its card's window admits this launch. */
static void wait_for_window(int device, struct vppu_pid_state *state, bool graph)
{
    for (;;) {
        unsigned long long now = now_ns();
        unsigned long long start = __atomic_load_n(&state->window_start_ns, __ATOMIC_ACQUIRE);

        /* A window that has run out is restamped by whoever notices, which is cheap and happens
         * every period; whether the LOOP also steps is a slower question, and asking it here is
         * what keeps the two rates apart. `now < start` covers a stamp left in the future by a
         * region file that outlived a reboot. */
        if (now < start || now - start >= period_ns) {
            if (__atomic_compare_exchange_n(&state->window_start_ns, &start, now, false,
                                            __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)) {
                maybe_step(device, state, now);
            }
            continue;
        }

        unsigned long long allow = __atomic_load_n(&state->allow_ns, __ATOMIC_ACQUIRE);
        if (graph && graph_weight > 1U) {
            /* A graph launch runs however many kernels were captured into it, so it is admitted
             * only in a fraction of the window a single kernel would get. */
            allow /= graph_weight;
        }
        if (now - start < allow) {
            return;
        }
        sleep_ns(start + period_ns - now);
    }
}

bool vppu_hggc_gate(enum vppu_entry entry, bool graph)
{
    if (!vppu_quota_usable()) {
        vppu_log(VPPU_LOG_DENY,
                 "DENIED %s: the container's quota configuration is unusable — see the "
                 "load-time report\n",
                 vppu_hggc_name(entry));
        return false;
    }

    load_config();
    if (!any_capped) {
        return true;
    }

    /* Two callers that must never be made to wait. A thread holding a card's ledger lock is
     * inside an admission, and the vendor's own allocation runs under that lock: throttling
     * there would hold it for a whole window and stall every other process's allocations on the
     * card. A thread inside a control step is this library sampling utilisation. */
    if (in_control || vppu_ledger_holding()) {
        return true;
    }

    int device = vppu_hggc_device();
    if (device < 0) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s: no current device\n", vppu_hggc_name(entry));
        return false;
    }

    /* Asked after the card is named, because a container may hold one capped card beside an
     * uncapped one — and counted only for a capped card, since the launch mix exists to explain
     * a measured utilisation the loop acted on. */
    if (!device_capped(device)) {
        return true;
    }

    __atomic_fetch_add(&interval_launches, 1ULL, __ATOMIC_RELAXED);
    if (graph) {
        __atomic_fetch_add(&interval_graph_launches, 1ULL, __ATOMIC_RELAXED);
    }

    struct vppu_pid_state *state = vppu_ledger_control(device);
    if (state == NULL) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s device=%d: the ledger is unavailable\n",
                 vppu_hggc_name(entry), device);
        return false;
    }

    wait_for_window(device, state, graph);
    return true;
}
