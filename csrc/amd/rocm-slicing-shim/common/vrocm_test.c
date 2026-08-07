/*
 * vrocm_test.c — the unit tests for common/, and the only tests in this tree that need no
 * hardware.
 *
 * They exist because common/ names no `hip*` or `hsa*` type: the quota arithmetic, the key map
 * and the region can therefore be exercised with no ROCm, no HIP runtime and no device, which is
 * true of nothing else here. Everything else in this tree can only be judged on an AMD host.
 *
 * Built by `build.sh unit` and run by amd-case-6.sh. It prints the same three-column table every
 * case script prints, ending in FAILS=<n>, so the case relays it rather than reinterpreting it.
 *
 * EVERY TEST THAT TOUCHES THE REGION RUNS IN A FORKED CHILD, and the parent never maps one. This
 * is not fussiness: the mapping is latched per process precisely so a broken ledger costs one
 * open() rather than one per allocation, so a parent that mapped once could not then test a
 * second region — and the cross-process tests need genuinely separate processes anyway.
 */
#define _GNU_SOURCE

#include <fcntl.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include "vrocm_ledger.h"
#include "vrocm_log.h"
#include "vrocm_quota.h"

#define MIB VROCM_MEMORY_LIMIT_UNIT
#define GIB (1024ULL * MIB)

static int fails;
static char tmpdir[] = "/tmp/vrocm-test-XXXXXX";

/* Call anything that writes an out-parameter in its OWN statement, never inside a check_that()
 * condition whose detail also formats that out-parameter: the order in which a call's arguments
 * are evaluated is unspecified, so the detail can be formatted from the value BEFORE the call,
 * producing a row that passes while printing a figure that never existed. */
static void check_that(bool ok, const char *name, const char *fmt, ...)
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

/* region_path — a fresh ledger path per test, so no test can be decided by another's leftovers. */
static const char *region_path(int which)
{
    static char path[256];

    snprintf(path, sizeof(path), "%s/ledger-%d", tmpdir, which);
    return path;
}

/* run_child — run one body in its own process against its own ledger, and report the exit code
 * it chose. 0 means the body was satisfied; anything else is its own diagnosis. */
static int run_child(int (*body)(void), const char *path)
{
    pid_t pid = fork();
    int status = 0;

    if (pid == 0) {
        if (path != NULL) {
            setenv(VROCM_ENV_LEDGER_PATH, path, 1);
        }
        _exit(body());
    }
    if (pid < 0) {
        return -1;
    }
    if (waitpid(pid, &status, 0) != pid || !WIFEXITED(status)) {
        return -1;
    }
    return WEXITSTATUS(status);
}

/* A stand-in for the runtime's real allocation. It hands back a distinct key each time and
 * counts its own calls, which is how the fail-closed test proves the runtime was never reached. */
static unsigned long long fake_next_key = 0x1000;
static int fake_calls;

static bool fake_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    (void)ctx;
    (void)bytes;
    fake_calls++;
    *key = fake_next_key++;
    return true;
}

static bool refusing_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    (void)ctx;
    (void)key;
    (void)bytes;
    fake_calls++;
    return false;
}

/* A pitched allocation's stand-in: it takes twice what it was admitted for, the way a runtime
 * rounding a width up to a stride does. */
static bool overrunning_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    (void)ctx;
    fake_calls++;
    *key = fake_next_key++;
    *bytes *= 2;
    return true;
}

/* asserts_lock_held — an allocation callback that fails unless the card's lock is held around
 * it. This is the hook the "one lock acquisition" property is asserted with: a build that
 * released the lock between the check and the charge would run this with nothing held. */
static bool asserts_lock_held(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    int device = -1;

    (void)bytes;
    if (!vrocm_ledger_holding(&device) || device != *(int *)ctx) {
        return false;
    }
    *key = fake_next_key++;
    return true;
}

/* ---- quota ---------------------------------------------------------------------------- */

static void test_quota_parse(void)
{
    check_that(vrocm_quota_parse("4096", MIB) == 4096ULL * MIB, "quota_parse/plain",
               "4096 MiB -> %llu bytes", vrocm_quota_parse("4096", MIB));
    check_that(vrocm_quota_parse("0", MIB) == 0, "quota_parse/zero",
               "a quota of zero is not a quota");
    check_that(vrocm_quota_parse("-1", MIB) == 0, "quota_parse/negative",
               "a leading minus must not wrap into a huge positive figure");
    check_that(vrocm_quota_parse("4096MiB", MIB) == 0, "quota_parse/trailing",
               "a trailing unit is not silently ignored");
    check_that(vrocm_quota_parse("", MIB) == 0, "quota_parse/empty", "an empty value is unusable");
    check_that(vrocm_quota_parse(NULL, MIB) == 0, "quota_parse/unset", "an unset value is 0");
    check_that(vrocm_quota_parse("18446744073709551615", MIB) == 0, "quota_parse/overflow",
               "a figure that would wrap the scale is refused, not multiplied");
    /* strtoull skips leading whitespace and then reads a sign, so neither is reachable by a test
     * on the first character alone -- which is why the parser requires a DIGIT there instead.
     * The positive case is the one that discriminates: " 10" was accepted as 10 MiB before. */
    check_that(vrocm_quota_parse(" 10", MIB) == 0, "quota_parse/leading_space",
               "leading whitespace is junk in a figure, the same as trailing junk is");
    check_that(vrocm_quota_parse(" -10", MIB) == 0, "quota_parse/space_then_minus",
               "whitespace hides a sign from a first-character test; the digit rule sees it");
    check_that(vrocm_quota_parse("+10", MIB) == 0, "quota_parse/leading_plus",
               "a sign is not a digit, whichever sign it is");
}

static void test_quota_precedence(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "1024", 1);
    setenv(VROCM_ENV_MEMORY_LIMIT_PREFIX "1", "2048", 1);

    check_that(vrocm_quota_memory_bytes(1) == 2048ULL * MIB, "quota_precedence/indexed_wins",
               "card 1 takes its own 2048 MiB over the container's 1024");
    check_that(vrocm_quota_memory_bytes(0) == 1024ULL * MIB, "quota_precedence/unindexed_covers",
               "card 0 carries no figure of its own and takes the container's");

    /* SET but unusable must deny that card, never fall through to the level above: falling
     * through is how a mistyped per-card figure quietly buys somebody else's allowance. */
    setenv(VROCM_ENV_MEMORY_LIMIT_PREFIX "2", "not-a-number", 1);
    check_that(vrocm_quota_memory_bytes(2) == 0, "quota_precedence/malformed_does_not_fall_through",
               "a malformed indexed figure denies its card rather than inheriting 1024 MiB");

    check_that(vrocm_quota_memory_bytes(-1) == 0 &&
                   vrocm_quota_memory_bytes(VROCM_MAX_DEVICES) == 0,
               "quota_precedence/bounds", "an out-of-range index answers 0 rather than reading on");

    unsetenv(VROCM_ENV_MEMORY_LIMIT_PREFIX "1");
    unsetenv(VROCM_ENV_MEMORY_LIMIT_PREFIX "2");
    unsetenv(VROCM_ENV_MEMORY_LIMIT);
}

static void test_quota_usable_latch(void)
{
    setenv(VROCM_ENV_LEDGER_PATH, region_path(90), 1);
    setenv(VROCM_ENV_MEMORY_LIMIT, "1024", 1);
    vrocm_quota_validate();
    check_that(vrocm_quota_usable(), "quota_latch/unindexed_only_is_complete",
               "the un-indexed figure alone is a complete configuration");

    unsetenv(VROCM_ENV_MEMORY_LIMIT);
    vrocm_quota_validate();
    check_that(!vrocm_quota_usable(), "quota_latch/no_figure_is_unusable",
               "a container with no figure in either form refuses everything");

    setenv(VROCM_ENV_MEMORY_LIMIT, "1024", 1);
    unsetenv(VROCM_ENV_LEDGER_PATH);
    vrocm_quota_validate();
    check_that(!vrocm_quota_usable(), "quota_latch/no_ledger_path_is_unusable",
               "%s has no default and its absence is a refusal", VROCM_ENV_LEDGER_PATH);

    unsetenv(VROCM_ENV_MEMORY_LIMIT);
}

/* ---- log ------------------------------------------------------------------------------ */

/* The level is latched on first use, which is right for the product -- it sits on the denial path
 * of every allocation -- and a trap for a test that forks. Any test above this one that logs
 * anything latches the level in the PARENT, and a forked child inherits the latched value rather
 * than reading its own environment; every level then reads as the default. So the child here is a
 * fresh EXEC of this binary, not a fork of it, which makes the result independent of what ran
 * before it. Both orders were tried and the fork version passed only while it happened to run
 * first. */
static const char *self;

static int exec_log_level(const char *value)
{
    pid_t pid = fork();
    int status = 0;

    if (pid == 0) {
        if (value != NULL) {
            setenv(VROCM_ENV_LOG_LEVEL, value, 1);
        } else {
            unsetenv(VROCM_ENV_LOG_LEVEL);
        }
        execl(self, self, "--log-level", (char *)NULL);
        _exit(127);
    }
    if (pid < 0 || waitpid(pid, &status, 0) != pid || !WIFEXITED(status)) {
        return -1;
    }
    return WEXITSTATUS(status);
}

static void test_log_levels(void)
{
    check_that(exec_log_level("0") == VROCM_LOG_QUIET, "log/quiet", "0 is silent");
    check_that(exec_log_level("2") == VROCM_LOG_DEBUG, "log/debug",
               "2 carries the load marker and the counter dump the cases grep");
    check_that(exec_log_level("9") == VROCM_LOG_DENY, "log/out_of_range",
               "an out-of-range level falls back to the default rather than refusing anything");
    check_that(exec_log_level(NULL) == VROCM_LOG_DENY, "log/default",
               "the default is 1: one line per denial, which is the question a user asks");
}

/* ---- region ---------------------------------------------------------------------------- */

static int child_creates_region(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    return vrocm_ledger_admit(0, MIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 1;
}

static void test_region_layout(void)
{
    const char *path = region_path(1);
    struct vrocm_region header;
    int fd, got;

    check_that(run_child(child_creates_region, path) == 0, "region/created",
               "the first admission creates and initialises the region");

    fd = open(path, O_RDONLY);
    got = (fd >= 0 && read(fd, &header, sizeof(header)) == (ssize_t)sizeof(header)) ? 0 : -1;
    if (fd >= 0) {
        close(fd);
    }
    check_that(got == 0 && memcmp(header.magic, VROCM_REGION_MAGIC, VROCM_REGION_MAGIC_BYTES) == 0,
               "region/magic", "a reader identifies the file by its magic alone");
    check_that(got == 0 && header.layout_version == VROCM_REGION_VERSION &&
                   header.header_bytes == offsetof(struct vrocm_region, devices) &&
                   header.device_slots == VROCM_MAX_DEVICES &&
                   header.process_slots == VROCM_MAX_PROCESSES_PER_DEVICE,
               "region/header", "version %u, devices at +%u, %u cards, %u processes",
               (unsigned)header.layout_version, (unsigned)header.header_bytes,
               (unsigned)header.device_slots, (unsigned)header.process_slots);
    check_that(offsetof(struct vrocm_region, lock_arena) == 32, "region/lock_arena_frozen",
               "two builds must lock the same byte for the same card, so this offset may not move");
}

static int child_foreign_region(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    /* Refusing is the pass: the path is operator-supplied, so a typo that lands on somebody
     * else's file must not be overwritten. */
    return vrocm_ledger_admit(0, MIB, fake_alloc, NULL) == VROCM_ADMIT_DENIED_CONFIG ? 0 : 1;
}

static void test_region_foreign(void)
{
    const char *path = region_path(2);
    int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);

    if (fd >= 0) {
        (void)!write(fd, "NOTALEDGERnotaledger", 20);
        close(fd);
    }
    check_that(run_child(child_foreign_region, path) == 0, "region/foreign_refused",
               "a file that is not a ledger is refused rather than overwritten");
}

static int child_wrong_version(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    return vrocm_ledger_admit(0, MIB, fake_alloc, NULL) == VROCM_ADMIT_DENIED_CONFIG ? 0 : 1;
}

static void test_region_version(void)
{
    const char *path = region_path(3);
    struct vrocm_region header;
    int fd;

    memset(&header, 0, sizeof(header));
    memcpy(header.magic, VROCM_REGION_MAGIC, VROCM_REGION_MAGIC_BYTES);
    header.layout_version = VROCM_REGION_VERSION + 1;

    fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);
    if (fd >= 0) {
        (void)!write(fd, &header, sizeof(header));
        close(fd);
    }
    check_that(run_child(child_wrong_version, path) == 0, "region/version_refused",
               "a layout version this build does not speak is refused, not misparsed");
}

/* ---- the four behavioural properties ---------------------------------------------------- */

/* 1. The quota is re-read on attach, never frozen by whichever process created the region. */
static int child_creates_at_4gib(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    return vrocm_ledger_admit(0, 3ULL * GIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 1;
}

static int child_attaches_at_2gib(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "2048", 1);
    vrocm_quota_validate();
    /* The region was created by a 4 GiB process which is now gone. A build that froze the
     * creator's figure would admit this; the sweep drops the dead charge and the 2 GiB figure
     * then decides, so 3 GiB must be refused. */
    if (vrocm_ledger_admit(0, 3ULL * GIB, fake_alloc, NULL) != VROCM_ADMIT_DENIED_QUOTA) {
        return 1;
    }
    return vrocm_ledger_admit(0, 2ULL * GIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 2;
}

static void test_quota_reread_on_attach(void)
{
    const char *path = region_path(4);

    check_that(run_child(child_creates_at_4gib, path) == 0, "ledger/creator_admitted",
               "a 4 GiB container takes 3 GiB");
    check_that(run_child(child_attaches_at_2gib, path) == 0,
               "ledger/quota_reread_on_attach",
               "a later 2 GiB container is decided against 2 GiB, not the creator's 4");
}

/* 2. Check, allocate and charge happen under ONE acquisition of the card's lock. */
static int child_one_lock_epoch(void)
{
    unsigned long long before, after;
    int device = 0;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();

    /* The first admission also maps the region, so measure the second one. */
    if (vrocm_ledger_admit(0, MIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    before = vrocm_ledger_lock_epochs();
    if (vrocm_ledger_admit(0, MIB, asserts_lock_held, &device) != VROCM_ADMIT_OK) {
        return 2; /* the callback ran with the lock released */
    }
    after = vrocm_ledger_lock_epochs();
    return (after - before) == 1 ? 0 : 3;
}

static void test_one_lock_acquisition(void)
{
    int rc = run_child(child_one_lock_epoch, region_path(5));

    check_that(rc == 0, "ledger/check_allocate_charge_under_one_lock",
               "%s", rc == 2 ? "the allocation ran with the card unlocked"
                             : rc == 3 ? "the admission took the lock more than once"
                                       : "one acquisition covers check, allocate and charge");
}

/* 3. A charge that cannot be recorded is refused, and the runtime is never reached. */
static int child_tracking_fail_closed(void)
{
    int i;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4194304", 1); /* 4 TiB: the quota must not be what refuses */
    vrocm_quota_validate();

    for (i = 0; i < 200000; i++) {
        enum vrocm_admit rc = vrocm_ledger_admit(0, 1, fake_alloc, NULL);

        if (rc == VROCM_ADMIT_DENIED_TRACKING) {
            int calls_before = fake_calls;

            /* The refusal must not have reached the runtime, and must keep not reaching it. */
            if (vrocm_ledger_admit(0, 1, fake_alloc, NULL) != VROCM_ADMIT_DENIED_TRACKING) {
                return 2;
            }
            if (fake_calls != calls_before) {
                return 3;
            }
            return vrocm_ledger_tracking_refusals() >= 2 ? 0 : 4;
        }
        if (rc != VROCM_ADMIT_OK) {
            return 1;
        }
    }
    return 5; /* the table never filled, so nothing was proven */
}

static void test_tracking_fail_closed(void)
{
    int rc = run_child(child_tracking_fail_closed, region_path(6));

    check_that(rc == 0, "ledger/tracking_insert_is_fail_closed",
               "%s", rc == 3 ? "the runtime was called for a charge that could not be recorded"
                             : rc == 5 ? "the key map never filled; the property was not exercised"
                                       : "a charge with nowhere to be recorded is refused");
}

/* 4. A dead process's charge is swept and the card's total re-derived. */
static int child_dies_holding(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    if (vrocm_ledger_admit(0, 3ULL * GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    /* Leaves without refunding, exactly as a SIGKILLed workload does. */
    _exit(0);
}

static int child_after_death(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    /* A build with no sweep would find 3 GiB still charged and refuse this. */
    return vrocm_ledger_admit(0, 4ULL * GIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 1;
}

static void test_dead_charge_swept(void)
{
    const char *path = region_path(7);

    check_that(run_child(child_dies_holding, path) == 0, "ledger/charge_survives_its_process",
               "a process takes 3 GiB of a 4 GiB card and leaves without refunding");
    check_that(run_child(child_after_death, path) == 0, "ledger/dead_charge_swept",
               "the next container reclaims it and takes the whole 4 GiB");
}

/* ---- accounting and locking ------------------------------------------------------------- */

static int child_charge_refund(void)
{
    unsigned long long key = 0;
    int device = -1;
    unsigned long long bytes = 0;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();

    key = fake_next_key;
    if (vrocm_ledger_admit(0, GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    if (vrocm_ledger_used(0) != GIB) {
        return 2;
    }
    if (!vrocm_ledger_release(key, &device, &bytes) || device != 0 || bytes != GIB) {
        return 3;
    }
    if (vrocm_ledger_used(0) != 0) {
        return 4;
    }
    /* A second release of the same key is "nothing to refund", never a second refund: a double
     * free must not hand the container an unbounded quota. */
    if (vrocm_ledger_release(key, NULL, NULL)) {
        return 5;
    }
    return vrocm_ledger_used(0) == 0 ? 0 : 6;
}

static void test_charge_refund(void)
{
    int rc = run_child(child_charge_refund, region_path(8));

    check_that(rc == 0, "ledger/charge_refund_roundtrip",
               "%s", rc == 5 ? "a double free was refunded twice" : "a charge round-trips exactly once");
}

static int child_alloc_refused(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    if (vrocm_ledger_admit(0, GIB, refusing_alloc, NULL) != VROCM_ADMIT_ALLOC_FAILED) {
        return 1;
    }
    /* Nothing was charged, so the card is untouched and the reserved slot went back. */
    return vrocm_ledger_used(0) == 0 ? 0 : 2;
}

static void test_alloc_refused_charges_nothing(void)
{
    check_that(run_child(child_alloc_refused, region_path(9)) == 0,
               "ledger/runtime_refusal_charges_nothing",
               "an allocation the runtime refused leaves the card's total unchanged");
}

static int child_quota_denies(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "1024", 1);
    vrocm_quota_validate();
    if (vrocm_ledger_admit(0, 512ULL * MIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    if (vrocm_ledger_admit(0, 1024ULL * MIB, fake_alloc, NULL) != VROCM_ADMIT_DENIED_QUOTA) {
        return 2;
    }
    /* Precision matters at the boundary: the remaining 512 MiB is admissible, exactly. */
    return vrocm_ledger_admit(0, 512ULL * MIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 3;
}

static void test_quota_boundary(void)
{
    check_that(run_child(child_quota_denies, region_path(10)) == 0, "ledger/quota_boundary",
               "the card admits exactly up to its figure and refuses the byte past it");
}

static int child_quota_wraps(void)
{
    setenv(VROCM_ENV_MEMORY_LIMIT, "1024", 1);
    vrocm_quota_validate();
    if (vrocm_ledger_admit(0, 512ULL * MIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    /* The one size that makes `used + bytes` land back on zero. Asked as that sum the card reads
     * as empty and this is admitted; asked as a subtraction it is refused for what it is. The
     * size is absurd and no allocation of it would succeed -- but a request that reaches the
     * runtime is a request this library decided it could afford, and that decision is the thing
     * under test. */
    if (vrocm_ledger_admit(0, ~0ULL - (512ULL * MIB) + 1ULL, fake_alloc, NULL) !=
        VROCM_ADMIT_DENIED_QUOTA) {
        return 2;
    }
    /* And the refusal left the card as it found it. */
    return vrocm_ledger_admit(0, 512ULL * MIB, fake_alloc, NULL) == VROCM_ADMIT_OK ? 0 : 3;
}

static void test_quota_wraps(void)
{
    check_that(run_child(child_quota_wraps, region_path(17)) == 0, "ledger/quota_wraps",
               "a request that wraps the running total is refused, not read as an empty card");
}

static int child_reentrant_lock(void)
{
    int device = -1;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    if (vrocm_ledger_admit(0, MIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    /* Outside an admission nothing is held; a stray unlock or a leaked depth would show here. */
    if (vrocm_ledger_holding(&device)) {
        return 2;
    }
    return 0;
}

static void test_lock_released(void)
{
    check_that(run_child(child_reentrant_lock, region_path(11)) == 0, "ledger/lock_balanced",
               "an admission leaves nothing held, so the depth counter cannot leak");
}

static int child_keymap_is_local(void)
{
    unsigned long long key;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    key = fake_next_key;
    if (vrocm_ledger_admit(0, GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    /* The sibling charged the same key value against the same card. If the map were shared, one
     * of the two releases would refund the other's allocation. */
    if (!vrocm_ledger_release(key, NULL, NULL)) {
        return 2;
    }
    return vrocm_ledger_release(key, NULL, NULL) ? 3 : 0;
}

/* A pitched allocation is decided on the caller's width and settled on the runtime's stride, so
 * the charge has to follow the stride -- under the same lock, or another process decides against
 * a total that is already stale. */
static int child_overrun(void)
{
    unsigned long long key;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    key = fake_next_key;
    if (vrocm_ledger_admit(0, GIB, overrunning_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }
    if (vrocm_ledger_used(0) != 2 * GIB) {
        return 2; /* charged the request rather than what was taken */
    }
    /* And the refund must give back what was charged, not what was asked for. */
    if (!vrocm_ledger_release(key, NULL, NULL)) {
        return 3;
    }
    return vrocm_ledger_used(0) == 0 ? 0 : 4;
}

static void test_overrun_is_charged(void)
{
    int rc = run_child(child_overrun, region_path(13));

    check_that(rc == 0, "ledger/allocation_charged_for_what_it_took",
               "%s", rc == 2 ? "an over-running allocation was charged its request, not its size"
                             : rc == 4 ? "the refund gave back the request rather than the charge"
                                       : "a stride wider than the width is charged and refunded whole");
}

static void test_keymap_process_local(void)
{
    const char *path = region_path(12);

    check_that(run_child(child_keymap_is_local, path) == 0 &&
                   run_child(child_keymap_is_local, path) == 0,
               "ledger/keymap_is_process_local",
               "two processes holding the same key value refund only their own charge");
}

/* ---- zero-size allocations ---------------------------------------------------------------- */

/* zero_key_alloc — what the runtime answers a zero-size request with: success, and no pointer. */
static bool zero_key_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    (void)ctx;
    (void)bytes;
    fake_calls++;
    *key = 0;
    return true;
}

static int child_zero_size_keeps_no_slot(void)
{
    int i;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();

    /* More of them than the key map holds. Every one is a successful allocation that no free can
     * ever match -- freeing a null pointer is defined to do nothing -- so a slot kept for any one
     * of them is kept for the life of the process, and the map fills with allocations that took
     * no memory at all. A batch that happens to be empty is how a framework makes these. */
    for (i = 0; i < 20000; i++) {
        if (vrocm_ledger_admit(0, 0, zero_key_alloc, NULL) != VROCM_ADMIT_OK) {
            return 1;
        }
    }
    if (vrocm_ledger_tracking_refusals() != 0) {
        return 2;
    }
    /* The card never gave anything out, so a real request must still be served. */
    if (vrocm_ledger_admit(0, GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 3;
    }
    return vrocm_ledger_used(0) == GIB ? 0 : 4;
}

static void test_zero_size_keeps_no_slot(void)
{
    int rc = run_child(child_zero_size_keeps_no_slot, region_path(14));

    check_that(rc == 0, "ledger/zero_size_keeps_no_slot", "%s",
               rc == 1   ? "zero-size allocations filled the key map and were then refused"
               : rc == 3 ? "1 GiB was refused on an empty card after 20000 zero-size allocations"
                         : "a zero-size allocation is served, charged nothing and keeps no slot");
}

/* ---- fork ----------------------------------------------------------------------------------
 *
 * A forked child inherits the key map, the re-entrancy counters and the spinlocks, and every one
 * of those copies is wrong in the child. The two tests below assert the consequences through the
 * public entries rather than the reset itself, because the consequences are what a workload
 * meets: a child that refunds its parent's charge, and a child that hangs on a lock nothing will
 * release. */

/* The gate between a forked child and the parent that forked it. The child cannot take the card
 * while the parent still holds it -- an fcntl record lock belongs to the PROCESS, so the child
 * would block on the parent while the parent waits for the child. The parent therefore finishes
 * its allocation, releases, and only then lets the child go. */
static int fork_gate[2] = { -1, -1 };

/* forking_alloc — an allocation callback that forks while the card's lock is held.
 *
 * That is the moment worth testing and it is not contrived: the lock is held across the whole of
 * the runtime's real allocation, which is the window a data loader's worker is spawned in. The
 * child reports what it found through its exit code. */
static bool forking_alloc(void *ctx, unsigned long long *key, unsigned long long *bytes)
{
    pid_t *child = ctx;

    (void)bytes;
    *child = fork();
    if (*child == 0) {
        int device = -1;
        char go = 0;

        /* The spin below has no timeout of its own, so without this a regression would hang the
         * suite rather than fail it. */
        alarm(10);
        /* Inherited re-entrancy counters make this true, and a child that believes it holds the
         * card goes on to account with no lock held at all. */
        if (vrocm_ledger_holding(&device)) {
            _exit(1);
        }
        if (read(fork_gate[0], &go, 1) != 1) {
            _exit(2);
        }
        /* The parent has released the card by now, so this must complete. An inherited
         * `device_lock` entry, still set from the fork, has no owner in this process and nothing
         * will ever release it: this is the call that spins until the alarm ends it. */
        if (vrocm_ledger_admit(0, GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
            _exit(3);
        }
        _exit(0);
    }
    if (*child < 0) {
        return false;
    }
    *key = fake_next_key++;
    return true;
}

static int child_fork_under_lock(void)
{
    pid_t child = -1;
    int status = 0;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    if (pipe(fork_gate) != 0) {
        return 1;
    }
    if (vrocm_ledger_admit(0, GIB, forking_alloc, &child) != VROCM_ADMIT_OK) {
        return 2;
    }
    if (write(fork_gate[1], "g", 1) != 1) {
        return 3;
    }
    if (waitpid(child, &status, 0) != child) {
        return 4;
    }
    if (!WIFEXITED(status)) {
        return 5;
    }
    return WEXITSTATUS(status) == 0 ? 0 : 5 + WEXITSTATUS(status);
}

static void test_fork_under_lock(void)
{
    int rc = run_child(child_fork_under_lock, region_path(15));

    check_that(rc == 0, "ledger/fork_under_lock", "%s",
               rc == 5   ? "a child forked under the card's lock spun on it until the alarm"
               : rc == 6 ? "a child forked under the card's lock believed it held it"
               : rc == 8 ? "a child forked under the card's lock could not go on to take it"
                         : "a child forked while a card was locked takes it cleanly afterwards");
}

static int child_fork_refunds_nothing(void)
{
    unsigned long long key;
    pid_t child;
    int status = 0;

    setenv(VROCM_ENV_MEMORY_LIMIT, "4096", 1);
    vrocm_quota_validate();
    key = fake_next_key;
    if (vrocm_ledger_admit(0, GIB, fake_alloc, NULL) != VROCM_ADMIT_OK) {
        return 1;
    }

    child = fork();
    if (child == 0) {
        /* The child inherited the key map, the pointer that key stands for, and the mapped
         * region. Releasing an inherited key must find nothing: the charge belongs to a process
         * this one is not, and refunding it here takes bytes off a card that is still holding
         * them -- and the next sweep, which re-derives the total from the slots, makes it stick. */
        _exit(vrocm_ledger_release(key, NULL, NULL) ? 1 : 0);
    }
    if (child < 0 || waitpid(child, &status, 0) != child || !WIFEXITED(status)) {
        return 2;
    }
    if (WEXITSTATUS(status) != 0) {
        return 3;
    }
    return vrocm_ledger_used(0) == GIB ? 0 : 4;
}

static void test_fork_refunds_nothing(void)
{
    int rc = run_child(child_fork_refunds_nothing, region_path(16));

    check_that(rc == 0, "ledger/fork_refunds_nothing", "%s",
               rc == 3   ? "a forked child refunded a charge its parent still holds"
               : rc == 4 ? "the parent's charge did not survive its child"
                         : "an inherited key is not the child's to give back");
}

int main(int argc, char **argv)
{
    self = argv[0];

    /* The re-exec arm: report the log level this environment produces, and nothing else. */
    if (argc == 2 && strcmp(argv[1], "--log-level") == 0) {
        return vrocm_log_level();
    }

    if (mkdtemp(tmpdir) == NULL) {
        printf("FAIL | setup | cannot create a temporary directory\n");
        printf("FAILS=1\n");
        return 1;
    }

    test_quota_parse();
    test_quota_precedence();
    test_quota_usable_latch();
    test_log_levels();

    test_region_layout();
    test_region_foreign();
    test_region_version();

    test_quota_reread_on_attach();
    test_one_lock_acquisition();
    test_tracking_fail_closed();
    test_dead_charge_swept();

    test_charge_refund();
    test_alloc_refused_charges_nothing();
    test_quota_boundary();
    test_quota_wraps();
    test_lock_released();
    test_keymap_process_local();
    test_overrun_is_charged();
    test_zero_size_keeps_no_slot();
    test_fork_under_lock();
    test_fork_refunds_nothing();

    printf("FAILS=%d\n", fails);
    return fails == 0 ? 0 : 1;
}
