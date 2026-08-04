/*
 * hgml_util_probe.c — Gate 3: is hgmlDeviceGetProcessUtilization usable as the compute
 * controller's feedback input, and whose PID does it report?
 *
 * This is a design input, not a shim. The compute quota is a PID loop, and the decision
 * it settles is whether that loop can be fed PER-PROCESS utilisation (this container's
 * own share) or has to fall back to card-total. Card-total couples every container's
 * controller on the same card and oscillates, so the answer changes the design.
 *
 * Unlike the shims here, this is a test binary that only ever runs inside the SDK
 * container, so it LINKS libhgml and libhggc instead of interposing anything. That also
 * makes the compiler check every signature and lets the linker resolve the header's
 * _v2/_v4 macro mappings, rather than a hand-written dlsym table guessing at ABI names.
 *
 * It answers two questions and refuses to conflate them:
 *   1. supported at runtime — the call returns HGML_SUCCESS *and* at least one sample.
 *      A success with zero samples is reported as `empty`, never as support: that is the
 *      exact shape a false PASS would take here.
 *   2. which PID namespace — decided by matching a reported pid against getpid(). The
 *      case runs this in a container with its own PID namespace, so the probe's own pid
 *      is small while a host pid is not; the raw NSpid line is printed too so the
 *      comparison can be re-checked rather than trusted.
 *
 * To have anything to sample it creates its own controlled load: a context, one device
 * buffer, and host-to-device copies between samples. If the driver only accounts compute
 * work, the copies yield no sample and question 1 is answered `empty` — which is a
 * finding, and hgmlDeviceGetComputeRunningProcesses still answers question 2.
 *
 * Output is one `PROBE <key>=<value>` line per fact so cases/thead-case-4.sh parses it
 * rather than scraping prose. Exit status is 0 whenever the probe ran to completion,
 * including when the verdict is negative — the verdict lives in the output.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Neither SDK header includes what its own declarations need. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>
#include <hgml.h>

#define PROBE_MAX_SAMPLES 64u
#define PROBE_MAX_PROCS 64u
#define PROBE_LOAD_BYTES (32u * 1024u * 1024u)
#define PROBE_COPIES_PER_ROUND 32

static void print_nspid(void)
{
    FILE *status = fopen("/proc/self/status", "re");
    if (status == NULL) {
        printf("PROBE self_nspid=unavailable\n");
        return;
    }

    char line[256];
    while (fgets(line, sizeof(line), status) != NULL) {
        if (strncmp(line, "NSpid:", 6) == 0) {
            line[strcspn(line, "\n")] = '\0';
            printf("PROBE self_nspid=%s\n", line + 7);
            fclose(status);
            return;
        }
    }
    fclose(status);
    printf("PROBE self_nspid=absent\n");
}

/* start_load — context + device buffer, so the process holds a real allocation and can
 * generate copy traffic. Returns false with a printed reason on the first failure. */
static bool start_load(int index, HGcontext *ctx, HGdeviceptr *dptr)
{
    HGresult rc = hgInit(0);
    printf("PROBE driver_init rc=%d\n", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    HGdevice dev = 0;
    rc = hgDeviceGet(&dev, index);
    printf("PROBE driver_device_get rc=%d index=%d\n", (int)rc, index);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    rc = hgCtxCreate(ctx, NULL, 0, dev);
    printf("PROBE driver_ctx_create rc=%d\n", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    rc = hgMemAlloc(dptr, PROBE_LOAD_BYTES);
    printf("PROBE driver_mem_alloc rc=%d bytes=%u\n", (int)rc, PROBE_LOAD_BYTES);
    if (rc != HGGC_SUCCESS) {
        /* The context is destroyed here rather than left to main()'s teardown, which only runs
         * when the load actually started: an orphan context holds card state that the rest of this
         * probe then goes on to measure. */
        hgCtxDestroy(*ctx);
        *ctx = NULL;
        return false;
    }
    return true;
}

static void run_copies(HGdeviceptr dptr, const void *host)
{
    for (int i = 0; i < PROBE_COPIES_PER_ROUND; i++) {
        hgMemcpyHtoD(dptr, host, PROBE_LOAD_BYTES);
    }
    hgCtxSynchronize();
}

int main(int argc, char **argv)
{
    unsigned int index = (argc > 1) ? (unsigned int)strtoul(argv[1], NULL, 10) : 0u;
    int rounds = (argc > 2) ? atoi(argv[2]) : 3;

    printf("PROBE self_pid=%d\n", (int)getpid());
    print_nspid();
    printf("PROBE device_index=%u rounds=%d\n", index, rounds);

    hgmlReturn_t hrc = hgmlInit_v2();
    printf("PROBE hgml_init rc=%d msg=%s\n", (int)hrc, hgmlErrorString(hrc));
    if (hrc != HGML_SUCCESS) {
        printf("PROBE VERDICT util=unavailable\nPROBE VERDICT pidns=unknown\n");
        return 0;
    }

    unsigned int device_count = 0;
    hrc = hgmlDeviceGetCount_v2(&device_count);
    printf("PROBE device_count rc=%d n=%u\n", (int)hrc, device_count);

    hgmlDevice_t device;
    hrc = hgmlDeviceGetHandleByIndex_v2(index, &device);
    printf("PROBE device_handle rc=%d msg=%s\n", (int)hrc, hgmlErrorString(hrc));
    if (hrc != HGML_SUCCESS) {
        printf("PROBE VERDICT util=unavailable\nPROBE VERDICT pidns=unknown\n");
        hgmlShutdown();
        return 0;
    }

    void *host = malloc(PROBE_LOAD_BYTES);
    HGcontext ctx = NULL;
    HGdeviceptr dptr = 0;
    bool loaded = (host != NULL) && start_load((int)index, &ctx, &dptr);
    printf("PROBE load_started=%s\n", loaded ? "yes" : "no");

    /* A sample's pid matching ours is the whole PID-namespace answer, so track both
     * halves separately: whether any sample arrived at all, and whether one was ours. */
    unsigned int total_samples = 0;
    bool saw_self = false;
    bool saw_other = false;
    /* Tracked apart from saw_self, which the process list also sets: the query below passes
     * lastSeenTimeStamp=0 and so returns ALL history, so a neighbouring container's stale
     * sample is enough to make total_samples non-zero while this process never appeared.
     * The design input is whether the loop can read its OWN utilisation, so only a sample
     * carrying our pid answers it. */
    bool util_saw_self = false;
    hgmlReturn_t util_rc = HGML_ERROR_UNKNOWN;

    for (int round = 0; round < rounds; round++) {
        if (loaded) {
            run_copies(dptr, host);
        }

        hgmlProcessUtilizationSample_t samples[PROBE_MAX_SAMPLES];
        unsigned int count = PROBE_MAX_SAMPLES;
        util_rc = hgmlDeviceGetProcessUtilization(device, samples, &count, 0ULL);
        printf("PROBE util_call round=%d rc=%d count=%u msg=%s\n",
               round, (int)util_rc, count, hgmlErrorString(util_rc));
        if (util_rc != HGML_SUCCESS) {
            continue;
        }

        if (count > PROBE_MAX_SAMPLES) {
            count = PROBE_MAX_SAMPLES;
        }
        for (unsigned int i = 0; i < count; i++) {
            printf("PROBE sample round=%d pid=%u smUtil=%u memUtil=%u timeStamp=%llu\n",
                   round, samples[i].pid, samples[i].smUtil, samples[i].memUtil,
                   samples[i].timeStamp);
            if ((int)samples[i].pid == (int)getpid()) {
                saw_self = true;
                util_saw_self = true;
            } else {
                saw_other = true;
            }
            total_samples++;
        }
    }

    /* The process list answers the PID-namespace question even when utilisation
     * accounting reports nothing, because merely holding a context is enough to appear. */
    hgmlProcessInfo_t procs[PROBE_MAX_PROCS];
    unsigned int proc_count = PROBE_MAX_PROCS;
    hgmlReturn_t proc_rc = hgmlDeviceGetComputeRunningProcesses_v3(device, &proc_count, procs);
    printf("PROBE proc_call rc=%d count=%u msg=%s\n",
           (int)proc_rc, proc_count, hgmlErrorString(proc_rc));
    if (proc_rc == HGML_SUCCESS) {
        if (proc_count > PROBE_MAX_PROCS) {
            proc_count = PROBE_MAX_PROCS;
        }
        for (unsigned int i = 0; i < proc_count; i++) {
            printf("PROBE proc pid=%u usedGpuMemory=%llu\n", procs[i].pid, procs[i].usedGpuMemory);
            if ((int)procs[i].pid == (int)getpid()) {
                saw_self = true;
            } else {
                saw_other = true;
            }
        }
    }

    if (util_rc != HGML_SUCCESS) {
        printf("PROBE VERDICT util=unsupported\n");
    } else if (total_samples == 0) {
        printf("PROBE VERDICT util=empty\n");
    } else if (!util_saw_self) {
        printf("PROBE VERDICT util=others-only samples=%u\n", total_samples);
    } else {
        printf("PROBE VERDICT util=supported samples=%u\n", total_samples);
    }

    if (saw_self) {
        printf("PROBE VERDICT pidns=container\n");
    } else if (saw_other) {
        printf("PROBE VERDICT pidns=host\n");
    } else {
        printf("PROBE VERDICT pidns=unknown\n");
    }

    if (loaded) {
        hgMemFree(dptr);
        hgCtxDestroy(ctx);
    }
    free(host);
    hgmlShutdown();
    return 0;
}
