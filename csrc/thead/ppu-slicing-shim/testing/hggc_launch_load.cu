/*
 * hggc_launch_load.cu — Gate 4's workload half: saturate one card with kernel launches and
 * report the utilisation this process was measured using.
 *
 * The compute cap is the one dimension no exerciser here could reach before: every other
 * testing/ artifact allocates memory or reads a figure, and neither spends compute. Throttling
 * can only be judged against a workload that would otherwise take the whole card, so this one
 * launches a deliberately long kernel back to back for a fixed wall-clock time.
 *
 * IT MEASURES ITSELF, on purpose. cases/thead-case-7.sh judges the shim on this process's own
 * hgmlDeviceGetProcessUtilization figure rather than on a card total, for the same reason the
 * controller is fed that way: a card total cannot tell one container's share from its
 * neighbour's, so a card total would pass whether the cap held or not. The figure is printed
 * beside the launch count, so a low utilisation caused by throttling is distinguishable from one
 * caused by launching nothing.
 *
 * This is the only file here written in the vendor's device dialect and built with hgcc, because
 * a kernel is the point: there is no way to occupy a PPU from plain C. It links the SDK freely
 * like the rest of testing/ — it only ever runs inside gpustack/thead-ppu-devel — and it is never
 * shipped.
 *
 * Output is one `LOAD <key>=<value>` line per fact, so the case parses it rather than scraping
 * prose. Exit status is 0 whenever the run completed, including when the verdict is negative:
 * the verdict lives in the output.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

/* Neither SDK header includes what its own declarations need. */
#include <stdbool.h>
#include <stddef.h>

#include <hgml.h>

#define LOAD_MAX_SAMPLES 128u
#define LOAD_BLOCKS 32
#define LOAD_THREADS 64

/* Long enough that one launch occupies the card for a measurable slice of a control window, so a
 * window that closes actually leaves the card idle rather than merely reordering launches. */
#define LOAD_ITERS 2000000ULL

/* spin — arithmetic with a data dependency, so the compiler cannot fold the loop away and the
 * card is genuinely busy for its duration. */
__global__ void spin(unsigned long long iters, int seed, int *out)
{
    int acc = seed;

    for (unsigned long long i = 0; i < iters; i++) {
        acc = acc * 31 + (int)(i & 7);
    }
    if (out != NULL) {
        *out = acc;
    }
}

/* now_seconds — the monotonic clock, or a NEGATIVE sentinel when it cannot be read.
 *
 * Negative rather than zero, because this whole run is bounded by wall clock alone: zero is a
 * reading like any other, so folding a failed one into `now - started < seconds` would extend the
 * loop instead of ending it. Callers test the sign. */
static double now_seconds(void)
{
    struct timespec ts;

    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) {
        return -1.0;
    }
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

/* sample_self — the newest utilisation sample the driver holds for this process.
 *
 * Newest rather than a sum over history for the reason the shim's own sampler gives: the query is
 * asked for all history, so one process appears repeatedly and adding those up would report a
 * multiple of what it used. */
static bool sample_self(hgmlDevice_t device, unsigned int *sm_util)
{
    hgmlProcessUtilizationSample_t samples[LOAD_MAX_SAMPLES];
    unsigned int count = LOAD_MAX_SAMPLES;

    if (hgmlDeviceGetProcessUtilization(device, samples, &count, 0ULL) != HGML_SUCCESS) {
        return false;
    }
    if (count > LOAD_MAX_SAMPLES) {
        count = LOAD_MAX_SAMPLES;
    }

    bool found = false;
    unsigned long long newest = 0ULL;
    for (unsigned int i = 0; i < count; i++) {
        if ((int)samples[i].pid != (int)getpid()) {
            continue;
        }
        if (!found || samples[i].timeStamp >= newest) {
            newest = samples[i].timeStamp;
            *sm_util = samples[i].smUtil;
            found = true;
        }
    }
    return found;
}

int main(int argc, char **argv)
{
    unsigned int index = (argc > 1) ? (unsigned int)strtoul(argv[1], NULL, 10) : 0u;
    double seconds = (argc > 2) ? strtod(argv[2], NULL) : 10.0;

    printf("LOAD device=%u seconds=%.1f pid=%d\n", index, seconds, (int)getpid());
    fflush(stdout);

    int *out = NULL;
    hggcError_t crc = hggcSetDevice((int)index);
    printf("LOAD set_device rc=%d\n", (int)crc);
    if (crc != hggcSuccess) {
        printf("LOAD result=failed reason=set_device\n");
        return 0;
    }
    crc = hggcMalloc((void **)&out, sizeof(int));
    printf("LOAD malloc rc=%d\n", (int)crc);
    if (crc != hggcSuccess) {
        printf("LOAD result=failed reason=malloc\n");
        return 0;
    }

    /* HGML is brought up separately from the load: a failure to read utilisation must not look
     * like a failure to generate it. */
    hgmlDevice_t device;
    bool measurable = (hgmlInit_v2() == HGML_SUCCESS)
                      && (hgmlDeviceGetHandleByIndex_v2(index, &device) == HGML_SUCCESS);
    printf("LOAD measurable=%s\n", measurable ? "yes" : "no");

    unsigned long long launches = 0ULL;
    unsigned long long failed = 0ULL;
    unsigned int sm_util = 0u;
    unsigned int samples = 0u;
    unsigned long long sm_util_sum = 0ULL;
    double started = now_seconds();
    double last_sample = started;
    double now = started;

    /* An unreadable clock is refused before the first launch rather than throttled around: this
     * run has no other stopping condition, so it would otherwise occupy the card until the case's
     * own timeout killed it, and a killed run prints no verdict at all. */
    if (started < 0.0) {
        printf("LOAD result=failed reason=clock\n");
        hggcFree(out);
        return 0;
    }

    for (;;) {
        now = now_seconds();
        if (now < 0.0) {
            printf("LOAD result=failed reason=clock\n");
            hggcFree(out);
            return 0;
        }
        if (now - started >= seconds) {
            break;
        }

        /* One launch per iteration, then a synchronise every few so the queue cannot grow without
         * bound while the shim holds the window shut. */
        spin<<<LOAD_BLOCKS, LOAD_THREADS>>>(LOAD_ITERS, (int)launches, out);
        if (hggcGetLastError() != hggcSuccess) {
            failed++;
        }
        launches++;

        if (launches % 4ULL == 0ULL) {
            hggcDeviceSynchronize();
        }

        /* Sampled after the first second and once a second after that: the first window is the
         * feed-forward one, and averaging it in would report the loop's cold start rather than
         * where it settled. */
        if (measurable && now - started > 1.0 && now - last_sample >= 1.0) {
            last_sample = now;
            if (sample_self(device, &sm_util)) {
                sm_util_sum += sm_util;
                samples++;
            }
        }
    }
    hggcDeviceSynchronize();

    /* Measured after the drain, as it was before, but falling back to the reading that ended the
     * loop so a clock that fails on this one last call cannot turn a completed run into a negative
     * elapsed time. */
    double finished = now_seconds();
    double elapsed = ((finished < 0.0) ? now : finished) - started;
    unsigned int mean = (samples > 0u) ? (unsigned int)(sm_util_sum / samples) : 0u;

    printf("LOAD launches=%llu failed=%llu elapsed_ms=%.0f rate_per_s=%.0f\n", launches, failed,
           elapsed * 1000.0, (elapsed > 0.0) ? (double)launches / elapsed : 0.0);
    printf("LOAD sm_util_mean=%u samples=%u last=%u\n", mean, samples, sm_util);
    printf("LOAD result=%s\n", (launches > 0ULL && failed == 0ULL) ? "success" : "failed");

    hggcFree(out);
    if (measurable) {
        hgmlShutdown();
    }
    return 0;
}
