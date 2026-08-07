/*
 * ledger_lifecycle.c — hold a charge, die without releasing it, and prove the next process can take
 * it back.
 *
 * WHY THIS IS NOT A UNIT TEST. `common/vrocm_test.c` already covers the sweep, from a forked child
 * that the test itself kills. What a fork cannot reproduce is the case the sweep actually exists
 * for: a charge left behind in a region file that OUTLIVES every process that knew about it, taken
 * back by a process started later with no memory of the first. That needs two real processes and a
 * signal from outside, so it needs a case script, so it needs a program the case can run.
 *
 * The failure it guards is not hypothetical. Measured before the sweep existed: a process SIGKILLed
 * while holding 4 GiB of a 6 GiB quota left the next process able to claim only 2, and the shortfall
 * survived for as long as the region file did — for a Pod, until it was deleted.
 *
 * WHY IT DRIVES THE LEDGER DIRECTLY AND LINKS NO ROCm. The subject is `common/`'s reclaim, not the
 * interposer's: going through `hipMalloc` would put the runtime, the preload and the quota
 * variables between the test and the thing under test, and a failure anywhere in that chain would
 * read as a reclaim failure. Driving the ledger with a fake allocation isolates it — and means this
 * program runs on a host with no card at all, which is where a case can run it cheaply.
 *
 * It needs `VROCM_LEDGER_PATH` and a `VROCM_DEVICE_MEMORY_LIMIT` figure in its environment, exactly
 * as the library does; without them the admission is refused for configuration and says so.
 */
#define _GNU_SOURCE

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "common/vrocm_ledger.h"
#include "common/vrocm_quota.h"

#define MIB (1024ULL * 1024ULL)

/* fake_alloc — stand in for the runtime, and succeed.
 *
 * The key has to be unique per charge within the process, and a counter is enough: the ledger only
 * ever compares keys for equality. Using the address of anything real would be worse, since two
 * charges could land on one address after a free. */
static bool fake_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    unsigned long long *next = (unsigned long long *)ctx;

    (void)bytes;
    *key = ++(*next);
    return true;
}

static const char *admit_name(enum vrocm_admit rc)
{
    switch (rc) {
    case VROCM_ADMIT_OK:
        return "ok";
    case VROCM_ADMIT_DENIED_QUOTA:
        return "denied-quota";
    case VROCM_ADMIT_DENIED_CONFIG:
        return "denied-config";
    case VROCM_ADMIT_DENIED_TRACKING:
        return "denied-tracking";
    case VROCM_ADMIT_ALLOC_FAILED:
        return "alloc-failed";
    }
    return "unknown";
}

static void report(int device)
{
    printf("LEDGER device=%d quota_mib=%llu used_mib=%llu pid=%d\n", device,
           vrocm_ledger_quota(device) / MIB, vrocm_ledger_used(device) / MIB, (int)getpid());
    fflush(stdout);
}

/* do_admit — one charge, reported the same way whether it was taken or refused. */
static enum vrocm_admit do_admit(int device, unsigned long long mib, unsigned long long *keys)
{
    enum vrocm_admit rc = vrocm_ledger_admit(device, mib * MIB, fake_alloc, keys);

    printf("ADMIT device=%d mib=%llu result=%s\n", device, mib, admit_name(rc));
    return rc;
}

static void usage(void)
{
    fprintf(stderr,
            "usage: ledger_lifecycle <command> [args]\n"
            "  hold <device> <mib>      take the charge, report, then wait to be killed\n"
            "  acquire <device> <mib>   take the charge and release it before exiting\n"
            "  probe <device>           report the card's quota and accounted usage\n"
            "  reclaim <device> <mib>   admit at a size that forces the sweep, and report\n");
}

int main(int argc, char **argv)
{
    unsigned long long keys = 0;
    unsigned long long mib = 0;
    const char *command;
    int device = 0;

    if (argc < 3) {
        usage();
        return 2;
    }
    command = argv[1];
    device = (int)strtol(argv[2], NULL, 10);
    if (argc > 3) {
        mib = strtoull(argv[3], NULL, 10);
    }

    /* The library reaches this through its own constructor; a program that links common/ directly
     * has no constructor and must do it by hand. Skipping it is not a quiet degradation — the
     * usability latch stays false and every admission is refused for configuration, which reads
     * exactly like the environment being wrong. */
    vrocm_quota_validate();
    if (!vrocm_quota_usable()) {
        fprintf(stderr, "ledger_lifecycle: unusable configuration; set %s and %s\n",
                VROCM_ENV_LEDGER_PATH, VROCM_ENV_MEMORY_LIMIT);
        return 2;
    }

    if (strcmp(command, "probe") == 0) {
        report(device);
        return 0;
    }

    if (strcmp(command, "hold") == 0) {
        if (do_admit(device, mib, &keys) != VROCM_ADMIT_OK) {
            return 1;
        }
        report(device);
        /* Deliberately never released. The case sends SIGKILL, which is the whole point: a signal
         * a process can neither catch nor clean up after is what leaves the charge stranded, and
         * anything gentler would let an atexit path tidy up and prove nothing. */
        printf("HOLD ready pid=%d\n", (int)getpid());
        fflush(stdout);
        for (;;) {
            pause();
        }
    }

    if (strcmp(command, "acquire") == 0) {
        enum vrocm_admit rc = do_admit(device, mib, &keys);

        if (rc != VROCM_ADMIT_OK) {
            report(device);
            return 1;
        }
        report(device);
        if (!vrocm_ledger_release(keys, NULL, NULL)) {
            printf("RELEASE device=%d result=not-recorded\n", device);
            return 1;
        }
        printf("RELEASE device=%d result=ok\n", device);
        report(device);
        return 0;
    }

    if (strcmp(command, "reclaim") == 0) {
        /* The sweep runs inside the admission, only when the charge would not otherwise fit — so
         * the way to exercise it is to ask for a size that collides with the stranded charge. A
         * refusal here means the sweep did not happen or did not recover enough. */
        enum vrocm_admit rc = do_admit(device, mib, &keys);

        report(device);
        return rc == VROCM_ADMIT_OK ? 0 : 1;
    }

    usage();
    return 2;
}
