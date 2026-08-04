/*
 * vppu_test.c — the unit tests for common/, and the first ones this library has.
 *
 * They exist because common/ names no `hg*`, `hggc*` or `hgml*` type: the quota arithmetic,
 * the key map and the region can therefore be exercised with no SDK, no vendor library and no
 * device, which is true of nothing else in this tree. Everything else here can only be judged
 * on a PPU host inside the vendor's image.
 *
 * Built and run by thead-case-6.sh, and it prints the same three-column table every case
 * script prints, ending in FAILS=<n>, so the case relays it rather than reinterpreting it.
 *
 * Every test that touches the region runs in a forked child, and the parent never maps one —
 * with one deliberate exception, test_fork_while_locked(), which is last for exactly that
 * reason. This is not fussiness: the mapping is latched per process precisely so a broken ledger
 * costs one open() rather than one per allocation, so a parent that mapped once could not then
 * test a second region — and the cross-process tests need genuinely separate processes anyway.
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

#include "vppu_ledger.h"
#include "vppu_pid.h"
#include "vppu_quota.h"

#define MIB (1024ULL * 1024ULL)
#define QUOTA_MIB 4096ULL
#define QUOTA_BYTES (QUOTA_MIB * MIB)

/* The controller's window, in nanoseconds, at the default period. */
#define PERIOD_NS (VPPU_SM_PERIOD_MS_DEFAULT * 1000000ULL)

static int fails;
static char tmpdir[] = "/tmp/vppu-test-XXXXXX";

/* Call anything that writes an out-parameter in its OWN statement, never inside a check_that()
 * condition whose detail also formats that out-parameter: the order in which a call's arguments
 * are evaluated is unspecified, so the detail can be formatted from the value BEFORE the call.
 * Both orders were observed — clang evaluated the condition first, gcc the arguments — and the
 * result is a row that passes while printing a figure that never existed. */
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

/* region_path — a fresh ledger path per test, so no test can be decided by another's
 * leftovers. */
static const char *region_path(int which)
{
    static char path[256];

    snprintf(path, sizeof(path), "%s/ledger-%d", tmpdir, which);
    return path;
}

/* run_child — run one body in its own process against its own ledger, and report the exit
 * code it chose. 0 means the body was satisfied; anything else is its own diagnosis. */
static int run_child(int (*body)(void), const char *path)
{
    pid_t pid = fork();

    if (pid == 0) {
        setenv(VPPU_ENV_LEDGER_PATH, path, 1);
        _exit(body());
    }
    if (pid < 0) {
        return -1;
    }

    int status = 0;
    if (waitpid(pid, &status, 0) != pid || !WIFEXITED(status)) {
        return -1;
    }
    return WEXITSTATUS(status);
}

/* ---------------------------------------------------------------------------------------
 * Quota parsing
 * ------------------------------------------------------------------------------------- */

static void test_quota_parse(void)
{
    static const struct {
        const char *value;
        unsigned long long unit;
        unsigned long long want;
        const char *name;
    } cases[] = {
        {"4096", MIB, 4096ULL * MIB, "parse: a MiB figure scales"},
        {"25", 1ULL, 25ULL, "parse: a percentage is unscaled"},
        {NULL, MIB, 0ULL, "parse: unset is zero"},
        {"", MIB, 0ULL, "parse: empty is zero"},
        /* Zero is rejected rather than honoured as a cap of nothing: it is what an unset
         * template renders to, and a quota of zero denies every allocation for a reason
         * nobody would find. */
        {"0", MIB, 0ULL, "parse: zero is not a quota"},
        {"abc", MIB, 0ULL, "parse: a malformed figure is zero"},
        /* Trailing junk has to be rejected, not truncated: "4096MiB" parsing as 4096 would
         * make a unit the caller never chose look like it worked. */
        {"4096MiB", MIB, 0ULL, "parse: trailing junk is rejected"},
        {"-1", MIB, 0ULL, "parse: a negative figure is rejected"},
        /* The bound is the whole point: a wrapped product lands either on 0, where nothing is
         * enforced, or on a tiny figure that denies everything. */
        {"18014398509481985", MIB, 0ULL, "parse: an overflowing figure is rejected"},
    };

    for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
        unsigned long long got = vppu_quota_parse(cases[i].value, cases[i].unit);

        check_that(got == cases[i].want, cases[i].name, "\"%s\" x %llu -> %llu, want %llu",
                   (cases[i].value != NULL) ? cases[i].value : "(unset)", cases[i].unit, got,
                   cases[i].want);
    }
}

static void test_quota_knob(void)
{
    static const struct {
        const char *value;
        unsigned int want;
        const char *name;
    } cases[] = {
        {"250", 250U, "knob: a figure in range is taken"},
        /* Zero is a legitimate gain — the derivative term defaults to it — which is the whole
         * reason this parse exists beside vppu_quota_parse(), where zero means unusable. */
        {"0", 0U, "knob: zero is a value, not an absence"},
        {NULL, 42U, "knob: unset falls back"},
        {"", 42U, "knob: empty falls back"},
        {"abc", 42U, "knob: malformed falls back"},
        {"250x", 42U, "knob: trailing junk falls back"},
        {"-1", 42U, "knob: a negative figure falls back"},
        {"1001", 42U, "knob: above the range falls back"},
    };

    for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
        unsigned int got = vppu_quota_knob(cases[i].value, 0U, 1000U, 42U);

        check_that(got == cases[i].want, cases[i].name, "\"%s\" -> %u, want %u",
                   (cases[i].value != NULL) ? cases[i].value : "(unset)", got, cases[i].want);
    }

    setenv(VPPU_ENV_SM_PERIOD_MS, "0", 1);
    check_that(vppu_quota_sm_period_ms() == VPPU_SM_PERIOD_MS_DEFAULT,
               "knob: a window of zero is refused", "%s=0 -> %ums, the default",
               VPPU_ENV_SM_PERIOD_MS, vppu_quota_sm_period_ms());
    unsetenv(VPPU_ENV_SM_PERIOD_MS);

    setenv(VPPU_ENV_SM_KI, "77", 1);
    struct vppu_pid_gains gains;
    vppu_quota_sm_gains(&gains);
    check_that(gains.ki == 77 && gains.kp == VPPU_SM_KP_DEFAULT && gains.kd == VPPU_SM_KD_DEFAULT,
               "knob: one gain can be tuned alone", "kp=%d ki=%d kd=%d", gains.kp, gains.ki,
               gains.kd);
    unsetenv(VPPU_ENV_SM_KI);

    /* A weight of 0 would make a graph launch free, which is the opposite of what the knob is
     * for, so the range starts at 1. */
    setenv(VPPU_ENV_SM_GRAPH_WEIGHT, "0", 1);
    check_that(vppu_quota_graph_weight() == VPPU_SM_GRAPH_WEIGHT_DEFAULT,
               "knob: a graph weight of zero is refused", "%s=0 -> %u",
               VPPU_ENV_SM_GRAPH_WEIGHT, vppu_quota_graph_weight());
    unsetenv(VPPU_ENV_SM_GRAPH_WEIGHT);
}

static void test_quota_env(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "3", "2048", 1);
    check_that(vppu_quota_memory_bytes(3) == 2048ULL * MIB, "quota: a card's own figure",
               "%s3=2048 -> %llu bytes", VPPU_ENV_MEMORY_LIMIT_PREFIX,
               vppu_quota_memory_bytes(3));
    check_that(vppu_quota_memory_bytes(4) == 0ULL, "quota: an unconfigured card is zero",
               "card 4 carries no figure");
    check_that(vppu_quota_memory_bytes(-1) == 0ULL && vppu_quota_memory_bytes(VPPU_MAX_DEVICES) == 0ULL,
               "quota: an out-of-range index is zero", "-1 and %d both answer 0",
               VPPU_MAX_DEVICES);

    /* The un-indexed memory figure, read exactly as the compute one is — the same precedence on
     * both dimensions is the contract, not a convenience. */
    setenv(VPPU_ENV_MEMORY_LIMIT, "1024", 1);
    check_that(vppu_quota_memory_bytes(3) == 2048ULL * MIB
                   && vppu_quota_memory_bytes(4) == 1024ULL * MIB,
               "quota: a card's own memory figure wins over the un-indexed one",
               "%s3=2048 with %s=1024 -> card 3 %lluMiB, card 4 %lluMiB",
               VPPU_ENV_MEMORY_LIMIT_PREFIX, VPPU_ENV_MEMORY_LIMIT,
               vppu_quota_memory_bytes(3) / MIB, vppu_quota_memory_bytes(4) / MIB);

    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "3", "not-a-number", 1);
    check_that(vppu_quota_memory_bytes(3) == 0ULL,
               "quota: an unparsable per-card memory figure does not fall back",
               "%s3 is set, so %s=1024 must not rescue it", VPPU_ENV_MEMORY_LIMIT_PREFIX,
               VPPU_ENV_MEMORY_LIMIT);
    unsetenv(VPPU_ENV_MEMORY_LIMIT);
    check_that(vppu_quota_memory_bytes(4) == 0ULL,
               "quota: with neither form set a card carries no memory figure",
               "card 4 answers 0 once %s is gone", VPPU_ENV_MEMORY_LIMIT);
    unsetenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "3");

    setenv(VPPU_ENV_SM_LIMIT, "25", 1);
    check_that(vppu_quota_sm_percent(0) == 25U, "quota: a compute cap", "%s=25 -> %u",
               VPPU_ENV_SM_LIMIT, vppu_quota_sm_percent(0));
    setenv(VPPU_ENV_SM_LIMIT, "150", 1);
    check_that(vppu_quota_sm_percent(0) == 0U, "quota: a compute cap above 100 is not a cap",
               "150 is reported unset rather than clamped");
    unsetenv(VPPU_ENV_SM_LIMIT);

    /* The per-card compute figure, and the one rule that separates it from HAMi-core's: an
     * indexed figure that is set decides its card whatever it says, so a typo denies that card
     * instead of buying it the container-wide figure. */
    setenv(VPPU_ENV_SM_LIMIT, "50", 1);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "1", "25", 1);
    check_that(vppu_quota_sm_percent(1) == 25U && vppu_quota_sm_percent(0) == 50U,
               "quota: a card's own compute cap wins", "%s1=25 with %s=50 -> card 1 %u, card 0 %u",
               VPPU_ENV_SM_LIMIT_PREFIX, VPPU_ENV_SM_LIMIT, vppu_quota_sm_percent(1),
               vppu_quota_sm_percent(0));
    check_that(vppu_quota_sm_percent(2) == 50U, "quota: a card with no figure of its own falls back",
               "card 2 reads %s=50 -> %u", VPPU_ENV_SM_LIMIT, vppu_quota_sm_percent(2));

    setenv(VPPU_ENV_SM_LIMIT_PREFIX "1", "not-a-number", 1);
    check_that(vppu_quota_sm_percent(1) == 0U,
               "quota: an unparsable per-card compute cap does not fall back",
               "%s1 is set, so %s=50 must not rescue it", VPPU_ENV_SM_LIMIT_PREFIX,
               VPPU_ENV_SM_LIMIT);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "1", "150", 1);
    check_that(vppu_quota_sm_percent(1) == 0U,
               "quota: an out-of-range per-card compute cap does not fall back",
               "%s1=150 is not a cap, and %s=50 must not stand in for it",
               VPPU_ENV_SM_LIMIT_PREFIX, VPPU_ENV_SM_LIMIT);
    unsetenv(VPPU_ENV_SM_LIMIT_PREFIX "1");
    unsetenv(VPPU_ENV_SM_LIMIT);
}

/* vppu_log_level() caches on first use, so each verdict needs its own process too. The level is
 * what decides whether a refusal is ever seen, so a malformed one must not silence the library:
 * strtol() answers 0 for "abc", and 0 is the level that hides denials as well. */
static int child_log_level_malformed(void)
{
    setenv(VPPU_ENV_LOG_LEVEL, "abc", 1);
    return (vppu_log_level() == VPPU_LOG_DENY) ? 0 : 1;
}

static int child_log_level_trailing_junk(void)
{
    setenv(VPPU_ENV_LOG_LEVEL, "2x", 1);
    return (vppu_log_level() == VPPU_LOG_DENY) ? 0 : 1;
}

static int child_log_level_set(void)
{
    setenv(VPPU_ENV_LOG_LEVEL, "2", 1);
    return (vppu_log_level() == VPPU_LOG_DEBUG) ? 0 : 1;
}

static int child_log_level_silenced(void)
{
    setenv(VPPU_ENV_LOG_LEVEL, "0", 1);
    return (vppu_log_level() == 0) ? 0 : 1;
}

static void test_log_level(void)
{
    check_that(run_child(child_log_level_malformed, region_path(0)) == 0,
               "log: a malformed level keeps the default", "%s=abc must not silence denials",
               VPPU_ENV_LOG_LEVEL);
    check_that(run_child(child_log_level_trailing_junk, region_path(0)) == 0,
               "log: a level with trailing junk keeps the default", "%s=2x is not a level",
               VPPU_ENV_LOG_LEVEL);
    check_that(run_child(child_log_level_set, region_path(0)) == 0,
               "log: a level that parses is used", "%s=2 raises the level to debug",
               VPPU_ENV_LOG_LEVEL);
    check_that(run_child(child_log_level_silenced, region_path(0)) == 0,
               "log: zero still silences everything", "%s=0 is a choice, not a typo",
               VPPU_ENV_LOG_LEVEL);
}

/* vppu_quota_validate() latches once per process, so each verdict needs its own. */
static int child_validate_none(void)
{
    unsetenv(VPPU_ENV_MEMORY_LIMIT);
    unsetenv(VPPU_ENV_SM_LIMIT);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

static int child_validate_bad(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "not-a-number", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

static int child_validate_no_compute(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "4096", 1);
    unsetenv(VPPU_ENV_SM_LIMIT);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

static int child_validate_bad_compute(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "4096", 1);
    setenv(VPPU_ENV_SM_LIMIT, "150", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

static int child_validate_good(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "4096", 1);
    setenv(VPPU_ENV_SM_LIMIT, "100", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 0 : 1;
}

/* The two-card container, which is what the per-card compute figure exists for. */
static int child_validate_two_cards(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "4096", 1);
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "1", "6144", 1);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "0", "50", 1);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "1", "25", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 0 : 1;
}

static int child_validate_one_card_no_compute(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "0", "4096", 1);
    setenv(VPPU_ENV_MEMORY_LIMIT_PREFIX "1", "6144", 1);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "0", "50", 1);
    unsetenv(VPPU_ENV_SM_LIMIT);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

/* The un-indexed pair alone, which is the whole configuration for a container whose allocator
 * knows only one figure per dimension — HAMi's own NVIDIA branch is such an allocator. */
static int child_validate_shared_only(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT, "4096", 1);
    setenv(VPPU_ENV_SM_LIMIT, "50", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 0 : 1;
}

static int child_validate_shared_no_compute(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT, "4096", 1);
    unsetenv(VPPU_ENV_SM_LIMIT);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

/* An un-indexed memory figure covers EVERY card, so a card whose own compute figure is malformed
 * is a card that would otherwise allocate against the shared figure and launch uncapped — the
 * launch path reads an unresolved cap as "nothing to wait for". */
static int child_validate_shared_bad_card_compute(void)
{
    setenv(VPPU_ENV_MEMORY_LIMIT, "4096", 1);
    setenv(VPPU_ENV_SM_LIMIT, "50", 1);
    setenv(VPPU_ENV_SM_LIMIT_PREFIX "1", "not-a-number", 1);
    vppu_quota_validate();
    return vppu_quota_usable() ? 1 : 0;
}

static void test_quota_validate(void)
{
    check_that(run_child(child_validate_none, region_path(0)) == 0,
               "validate: no figure at all is unusable",
               "a sliced container with no %s<i> must not run unlimited",
               VPPU_ENV_MEMORY_LIMIT_PREFIX);
    check_that(run_child(child_validate_bad, region_path(0)) == 0,
               "validate: an unparsable figure is unusable",
               "a typo must not read as a card that was never sliced");
    /* The compute half is a figure the allocator must inject even at 100%, because its own
     * helper defaults that request to 100 — so an omitted variable would otherwise buy a whole
     * card's compute silently. It decides usability only now that the controller enforces it. */
    check_that(run_child(child_validate_no_compute, region_path(0)) == 0,
               "validate: a memory figure without a compute figure is unusable",
               "%s absent must not read as uncapped compute", VPPU_ENV_SM_LIMIT);
    check_that(run_child(child_validate_bad_compute, region_path(0)) == 0,
               "validate: an out-of-range compute figure is unusable", "%s=150 is not a cap",
               VPPU_ENV_SM_LIMIT);
    check_that(run_child(child_validate_good, region_path(0)) == 0,
               "validate: both figures are usable", "%s0=4096 with %s=100 is accepted",
               VPPU_ENV_MEMORY_LIMIT_PREFIX, VPPU_ENV_SM_LIMIT);
    check_that(run_child(child_validate_two_cards, region_path(0)) == 0,
               "validate: two cards at their own compute caps are usable",
               "%s0=50 beside %s1=25 is accepted", VPPU_ENV_SM_LIMIT_PREFIX,
               VPPU_ENV_SM_LIMIT_PREFIX);
    /* Every card the container was GIVEN needs a compute figure, so one card's figure cannot
     * stand in for the other's — the whole point of validating per card rather than per
     * container. */
    check_that(run_child(child_validate_one_card_no_compute, region_path(0)) == 0,
               "validate: a card with memory and no compute figure is unusable",
               "%s0=50 must not cover card 1", VPPU_ENV_SM_LIMIT_PREFIX);
    check_that(run_child(child_validate_shared_only, region_path(0)) == 0,
               "validate: the un-indexed pair alone is usable", "%s=4096 with %s=50 is accepted",
               VPPU_ENV_MEMORY_LIMIT, VPPU_ENV_SM_LIMIT);
    check_that(run_child(child_validate_shared_no_compute, region_path(0)) == 0,
               "validate: an un-indexed memory figure without a compute figure is unusable",
               "%s=4096 alone must not read as uncapped compute", VPPU_ENV_MEMORY_LIMIT);
    check_that(run_child(child_validate_shared_bad_card_compute, region_path(0)) == 0,
               "validate: one card's malformed compute figure spoils the un-indexed pair",
               "%s covers every card, so %s1 cannot be left unresolved", VPPU_ENV_MEMORY_LIMIT,
               VPPU_ENV_SM_LIMIT_PREFIX);
}

/* ---------------------------------------------------------------------------------------
 * The compute controller
 *
 * This is the half of the compute quota that can be judged without a card, and the half where
 * being wrong looks like "it oscillates" on hardware. The plant below is deliberately not the
 * identity: a kernel launched inside the open window keeps the card busy after it closes, so the
 * utilisation a duty cycle buys is some multiple of that duty cycle — which is exactly the error
 * the feed-forward floor cannot know and the loop exists to remove.
 * ------------------------------------------------------------------------------------- */

/* simulate — run the loop against a card whose utilisation is `spill_tenths`/10 times the duty
 * cycle it was given, with one window of measurement lag. Reports the last measurement and the
 * last allowance. */
static unsigned int simulate(unsigned int target, unsigned int spill_tenths, int steps,
                             unsigned long long *allow_out)
{
    struct vppu_pid_gains gains;
    struct vppu_pid_state state;
    unsigned int measured = 0U;
    unsigned long long allow = 0ULL;

    memset(&state, 0, sizeof(state));
    vppu_quota_sm_gains(&gains);

    for (int i = 0; i < steps; i++) {
        allow = vppu_pid_step(&state, &gains, target, measured, PERIOD_NS);

        unsigned long long util = allow * spill_tenths * 100ULL / (PERIOD_NS * 10ULL);
        measured = (util > 100ULL) ? 100U : (unsigned int)util;
    }
    *allow_out = allow;
    return measured;
}

static void test_pid_feed_forward(void)
{
    check_that(vppu_pid_floor(25U, PERIOD_NS) == PERIOD_NS / 4ULL,
               "pid: the cold-start allowance is the quota's share of the window",
               "25%% of %llums is %lluns", (unsigned long long)VPPU_SM_PERIOD_MS_DEFAULT,
               vppu_pid_floor(25U, PERIOD_NS));
    check_that(vppu_pid_floor(100U, PERIOD_NS) == PERIOD_NS,
               "pid: an uncapped container gets the whole window", "100%% is the whole period");

    /* The first step must be the floor EXACTLY, measurement ignored. The sensor is slew-rate
     * limited from zero, so a card already pinned reads near nothing on the first sample; folding
     * that in would open the window past the quota on the one step where a burst is waiting for
     * it. Passing a measurement of 0 here is that worst case. */
    struct vppu_pid_gains gains;
    struct vppu_pid_state state;
    memset(&state, 0, sizeof(state));
    vppu_quota_sm_gains(&gains);
    unsigned long long first = vppu_pid_step(&state, &gains, 25U, 0U, PERIOD_NS);
    check_that(first == vppu_pid_floor(25U, PERIOD_NS),
               "pid: the first step is the floor and ignores the measurement",
               "%lluns, floor is %lluns", first, vppu_pid_floor(25U, PERIOD_NS));
    check_that(state.integral == 0 && state.last_error == 0,
               "pid: the first step accumulates nothing",
               "integral=%d last_error=%d, so the second step starts from the measurement",
               (int)state.integral, (int)state.last_error);

    /* The step after it does act on the measurement, or the loop would only ever feed forward. */
    unsigned long long second = vppu_pid_step(&state, &gains, 25U, 0U, PERIOD_NS);
    check_that(second > first, "pid: the second step follows the measurement",
               "%lluns after %lluns, with the container measured below its cap", second, first);
}

static void test_pid_clamps(void)
{
    struct vppu_pid_gains gains;
    struct vppu_pid_state state;
    unsigned long long allow = 0ULL;

    memset(&state, 0, sizeof(state));
    vppu_quota_sm_gains(&gains);

    /* A card pinned at 100% however little it is given — one long kernel does this. The window
     * must squeeze and must never close: refusing every launch would turn a compute cap into a
     * hang, and no later launch can shorten a kernel already running. */
    for (int i = 0; i < 200; i++) {
        allow = vppu_pid_step(&state, &gains, 25U, 100U, PERIOD_NS);
    }
    check_that(allow >= PERIOD_NS / VPPU_PID_MIN_DIVISOR && allow > 0ULL,
               "pid: a pinned card squeezes the window but never closes it",
               "%lluns, floor is %lluns", allow, (unsigned long long)PERIOD_NS / VPPU_PID_MIN_DIVISOR);

    /* The integral must stay bounded while it is stuck against that floor, or the window then
     * stays wide open for as many windows as it took to wind up — long after the load is gone. */
    long long limit = (long long)VPPU_PID_DIVISOR / (gains.ki > 0 ? gains.ki : 1);
    check_that(state.integral >= -limit && state.integral <= limit,
               "pid: the integral is clamped against windup", "%lld, limit %lld",
               (long long)state.integral, limit);

    /* A workload that cannot reach its cap even with the whole window must be given the whole
     * window, and must sit there rather than oscillate around it. */
    unsigned long long open = 0ULL;
    unsigned int measured = simulate(25U, 2U, 200, &open);
    check_that(open == PERIOD_NS, "pid: a workload under its cap keeps the whole window",
               "measured %u%% against a 25%% cap, allowance %lluns of %lluns", measured, open,
               (unsigned long long)PERIOD_NS);
}

static void test_pid_convergence(void)
{
    static const struct {
        unsigned int target;
        unsigned int spill_tenths;
        const char *name;
    } cases[] = {
        {25U, 20U, "pid: converges on a 25% cap when work spills a window over"},
        {50U, 15U, "pid: converges on a 50% cap"},
        {10U, 40U, "pid: converges on a 10% cap against four windows of spill"},
    };

    for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
        unsigned long long allow = 0ULL;
        unsigned int measured = simulate(cases[i].target, cases[i].spill_tenths, 400, &allow);
        int drift = (int)measured - (int)cases[i].target;

        check_that(drift >= -5 && drift <= 5, cases[i].name,
                   "settled at %u%% against a %u%% cap (allowance %lluns of %lluns)", measured,
                   cases[i].target, allow, (unsigned long long)PERIOD_NS);
    }
}

/* ---------------------------------------------------------------------------------------
 * The key map
 * ------------------------------------------------------------------------------------- */

static void test_alloc_map(void)
{
    int device = -1;
    unsigned long long bytes = 0ULL;

    vppu_alloc_record(0x10000ULL, 2, 4096ULL);
    bool took = vppu_alloc_take(0x10000ULL, &device, &bytes);
    check_that(took && device == 2 && bytes == 4096ULL,
               "map: a record comes back with its card", "device=%d bytes=%llu", device,
               bytes);
    check_that(!vppu_alloc_take(0x10000ULL, &device, &bytes),
               "map: a key can only be taken once", "the second take reports nothing");
    check_that(!vppu_alloc_take(0xdeadbeefULL, &device, &bytes),
               "map: an unrecorded key is not an error", "nothing to refund, reported as such");

    /* The tombstone case, and the defect it guards. Deleting an entry by emptying its slot
     * truncates the probe chain of every key that hashed past it, so a later free finds
     * nothing and its bytes stay charged for the life of the process. A full table makes
     * every chain long, which is what turns that from an edge case into the common one. */
    const unsigned long long first = 0x1000ULL;
    for (unsigned long long i = 0; i < 1024ULL; i++) {
        vppu_alloc_record(first + i * 0x1000ULL, 0, 1ULL);
    }
    check_that(vppu_alloc_take(first, &device, &bytes),
               "map: a full table still yields the first key", "1024 page-aligned keys stored");

    unsigned int lost = 0;
    for (unsigned long long i = 1; i < 1024ULL; i++) {
        if (!vppu_alloc_take(first + i * 0x1000ULL, &device, &bytes)) {
            lost++;
        }
    }
    check_that(lost == 0, "map: a deletion does not orphan the keys behind it",
               "%u of 1023 remaining keys could not be taken after one deletion", lost);
}

/* ---------------------------------------------------------------------------------------
 * The region
 * ------------------------------------------------------------------------------------- */

static int child_create_region(void)
{
    if (!vppu_ledger_lock(0)) {
        return 1;
    }
    vppu_ledger_note_config(0, QUOTA_BYTES, 25U);
    if (!vppu_ledger_charge(0, MIB)) {
        vppu_ledger_unlock(0);
        return 2;
    }
    vppu_ledger_unlock(0);

    if (vppu_ledger_used(0) != MIB) {
        return 3;
    }
    if (!vppu_ledger_lock(0)) {
        return 4;
    }
    vppu_ledger_refund(0, MIB);
    vppu_ledger_unlock(0);
    return (vppu_ledger_used(0) == 0ULL) ? 0 : 5;
}

/* read_u32 / read_u64 — the region read the way an outside scraper will read it: by
 * documented offset out of the raw file, never through this library's struct. */
static bool read_at(const char *path, off_t offset, void *out, size_t len)
{
    int fd = open(path, O_RDONLY);

    if (fd < 0) {
        return false;
    }
    bool ok = (pread(fd, out, len, offset) == (ssize_t)len);
    close(fd);
    return ok;
}

static void test_region_layout(void)
{
    const char *path = region_path(1);

    check_that(run_child(child_create_region, path) == 0,
               "region: charge and refund round trip", "1MiB charged then refunded to zero");

    struct stat st;
    memset(&st, 0, sizeof(st));
    bool sized = (stat(path, &st) == 0);
    check_that(sized && (size_t)st.st_size >= sizeof(struct vppu_region),
               "region: the file is at least the layout's size", "%llu bytes, layout is %zu",
               (unsigned long long)st.st_size, sizeof(struct vppu_region));

    char magic[8] = {0};
    uint32_t version = 0;
    uint32_t header_bytes = 0;
    uint32_t device_slots = 0;
    uint32_t process_slots = 0;
    bool got = read_at(path, 0, magic, sizeof(magic))
               && read_at(path, 8, &version, sizeof(version))
               && read_at(path, 12, &header_bytes, sizeof(header_bytes))
               && read_at(path, 16, &device_slots, sizeof(device_slots))
               && read_at(path, 20, &process_slots, sizeof(process_slots));

    check_that(got && memcmp(magic, VPPU_REGION_MAGIC, sizeof(magic)) == 0,
               "region: the magic is at offset 0", "%.8s", got ? magic : "unread");
    check_that(version == VPPU_REGION_VERSION, "region: the layout version is at offset 8",
               "%u", version);
    check_that(header_bytes == 96U, "region: the header size is at offset 12",
               "%u, the documented offset of the device table", header_bytes);
    check_that(device_slots == VPPU_MAX_DEVICES && process_slots == VPPU_MAX_PROCESSES_PER_DEVICE,
               "region: the slot counts are at offsets 16 and 20", "%u cards, %u processes",
               device_slots, process_slots);
}

/* A value no arithmetic here could produce, so finding it in the file proves the parent read the
 * word the child wrote rather than a coincidence. */
#define CONTROL_WINDOW_MARK 0x1122334455667788ULL

static int child_control_round_trip(void)
{
    struct vppu_pid_gains gains;

    /* One lock and release, which is what creates and stamps the region. */
    if (!vppu_ledger_lock(0)) {
        return 1;
    }
    vppu_ledger_unlock(0);

    struct vppu_pid_state *state = vppu_ledger_control(0);
    if (state == NULL) {
        return 2;
    }

    vppu_quota_sm_gains(&gains);
    state->window_start_ns = CONTROL_WINDOW_MARK;
    vppu_pid_step(state, &gains, 25U, 100U, PERIOD_NS);
    vppu_ledger_note_util(0, 100U);
    return 0;
}

/* The loop has to be readable from outside the process running it, because its gains are not
 * fitted to any PPU and tuning them on hardware nobody has profiled needs the loop's own state.
 * This is that read, done the way tools/ and a scraper will do it: by documented offset out of
 * the file, never through this library's struct. */
static void test_control_state_published(void)
{
    const char *path = region_path(7);

    check_that(run_child(child_control_round_trip, path) == 0,
               "control: a step is written to the region", "one window and one step recorded");

    off_t control_offset = (off_t)offsetof(struct vppu_region, devices)
                           + (off_t)offsetof(struct vppu_device_usage, control);
    check_that(control_offset == 128,
               "control: the controller words sit at the documented offset",
               "%lld, card 0's controller state", (long long)control_offset);

    uint64_t window_start = 0;
    uint64_t allow = 0;
    uint32_t util = 0;
    bool got = read_at(path, control_offset + (off_t)offsetof(struct vppu_pid_state, window_start_ns),
                       &window_start, sizeof(window_start))
               && read_at(path, control_offset + (off_t)offsetof(struct vppu_pid_state, allow_ns),
                          &allow, sizeof(allow))
               && read_at(path,
                          (off_t)offsetof(struct vppu_region, devices)
                              + (off_t)offsetof(struct vppu_device_usage, sm_util_percent),
                          &util, sizeof(util));

    struct vppu_pid_gains gains;
    struct vppu_pid_state expect;
    memset(&expect, 0, sizeof(expect));
    vppu_quota_sm_gains(&gains);
    unsigned long long want_allow = vppu_pid_step(&expect, &gains, 25U, 100U, PERIOD_NS);

    check_that(got && window_start == CONTROL_WINDOW_MARK,
               "control: the window the loop is in is readable", "0x%llx",
               (unsigned long long)window_start);
    check_that(got && allow == want_allow,
               "control: the allowance the loop decided is readable", "%lluns, want %lluns",
               (unsigned long long)allow, want_allow);
    check_that(got && util == 100U, "control: the measured utilisation is readable", "%u%%",
               util);
}

static int child_expect_refusal(void)
{
    /* A refusal must be a refusal, not a fresh region beside the unreadable one. */
    return vppu_ledger_lock(0) ? 1 : 0;
}

static void test_region_refusals(void)
{
    const char *path = region_path(2);

    /* A region stamped by a layout this build does not know. Refusing it is what keeps the
     * next field added to the contract from silently misparsing in an old reader. */
    struct vppu_region seed;
    memset(&seed, 0, sizeof(seed));
    memcpy(seed.magic, VPPU_REGION_MAGIC, sizeof(seed.magic));
    seed.layout_version = VPPU_REGION_VERSION + 99U;
    int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);
    bool wrote = (fd >= 0 && write(fd, &seed, sizeof(seed)) == (ssize_t)sizeof(seed));
    if (fd >= 0) {
        close(fd);
    }
    check_that(wrote && run_child(child_expect_refusal, path) == 0,
               "region: an unknown layout version is refused",
               "version %u refused rather than misparsed", seed.layout_version);

    /* A file that exists but is not ours is never overwritten — and, since it is shorter than the
     * region, never resized either. The size is the sharp half: the refusal used to come AFTER an
     * ftruncate that had already grown somebody's file to 36960 bytes, which is a modification
     * whatever the refusal then said. */
    const char *foreign = region_path(3);
    fd = open(foreign, O_RDWR | O_CREAT | O_TRUNC, 0600);
    wrote = (fd >= 0 && write(fd, "not a ledger", 12) == 12);
    if (fd >= 0) {
        close(fd);
    }
    bool refused = (wrote && run_child(child_expect_refusal, foreign) == 0);
    struct stat after;
    bool sized = (stat(foreign, &after) == 0);
    check_that(refused, "region: a foreign file is refused", "refused rather than overwritten");
    check_that(refused && sized && after.st_size == 12,
               "region: a foreign file is not even resized", "%llu bytes, was 12",
               (unsigned long long)(sized ? after.st_size : -1));
}

/* ---------------------------------------------------------------------------------------
 * Two processes, one quota
 * ------------------------------------------------------------------------------------- */

/* contend — take the card's lock, decide against the quota, wait, then charge.
 *
 * The wait is the test. It widens the window between the decision and the charge to something
 * a scheduler cannot miss, so a ledger that is per process rather than per container, or a
 * lock that is released before the allocation returns, grants BOTH callers the whole quota.
 * That is exactly the check-then-allocate race the lock exists to close. */
static int contend(void)
{
    if (!vppu_ledger_lock(0)) {
        return 2;
    }
    unsigned long long used = vppu_ledger_used(0);
    unsigned long long remaining = (used >= QUOTA_BYTES) ? 0ULL : QUOTA_BYTES - used;

    usleep(150000);

    int verdict = 1;
    if (remaining >= QUOTA_BYTES && vppu_ledger_charge(0, QUOTA_BYTES)) {
        verdict = 0;
    }
    vppu_ledger_unlock(0);
    return verdict;
}

static void test_one_quota_two_processes(void)
{
    const char *path = region_path(4);
    pid_t pids[2];

    for (int i = 0; i < 2; i++) {
        pids[i] = fork();
        if (pids[i] == 0) {
            setenv(VPPU_ENV_LEDGER_PATH, path, 1);
            _exit(contend());
        }
    }

    int granted = 0;
    int broken = 0;
    for (int i = 0; i < 2; i++) {
        int status = 0;
        if (pids[i] < 0 || waitpid(pids[i], &status, 0) != pids[i] || !WIFEXITED(status)) {
            broken++;
            continue;
        }
        if (WEXITSTATUS(status) == 0) {
            granted++;
        } else if (WEXITSTATUS(status) != 1) {
            broken++;
        }
    }

    check_that(broken == 0 && granted == 1, "quota: two processes cannot both take it whole",
               "%d of 2 granted the whole %lluMiB (%d could not run the test)", granted,
               QUOTA_MIB, broken);

    uint64_t used = 0;
    off_t used_offset = (off_t)offsetof(struct vppu_region, devices)
                        + (off_t)offsetof(struct vppu_device_usage, memory_used_bytes);
    bool read_used = read_at(path, used_offset, &used, sizeof(used));
    check_that(read_used && used == QUOTA_BYTES, "quota: the ledger holds one charge, not two",
               "%llu bytes accounted, want %llu", (unsigned long long)used,
               (unsigned long long)QUOTA_BYTES);
}

/* ---------------------------------------------------------------------------------------
 * A charge left behind by a process that died
 * ------------------------------------------------------------------------------------- */

static int child_charge_and_die(void)
{
    if (!vppu_ledger_lock(0)) {
        return 1;
    }
    bool charged = vppu_ledger_charge(0, QUOTA_BYTES);
    vppu_ledger_unlock(0);
    return charged ? 0 : 2;
}

static int child_reclaim(void)
{
    if (!vppu_ledger_lock(0)) {
        return 1;
    }
    if (vppu_ledger_used(0) != QUOTA_BYTES) {
        vppu_ledger_unlock(0);
        return 2;
    }
    unsigned long long recovered = vppu_ledger_reclaim(0);
    unsigned long long left = vppu_ledger_used(0);
    vppu_ledger_unlock(0);

    if (recovered != QUOTA_BYTES) {
        return 3;
    }
    return (left == 0ULL) ? 0 : 4;
}

static void test_reclaim_after_death(void)
{
    const char *path = region_path(5);

    check_that(run_child(child_charge_and_die, path) == 0,
               "reclaim: a process charges the whole quota and exits",
               "%lluMiB charged, never refunded", QUOTA_MIB);

    /* Without the sweep this charge holds the container's quota down for as long as the
     * ledger file lives, which is HAMi-core's stale cache arriving by another route. */
    check_that(run_child(child_reclaim, path) == 0,
               "reclaim: a dead process's charge is recovered",
               "the next process reclaims %lluMiB and sees an empty card", QUOTA_MIB);
}

/* ---------------------------------------------------------------------------------------
 * fork() while a lock is held
 * ------------------------------------------------------------------------------------- */

static int child_after_fork(void)
{
    /* A hang is one of the two failures this test exists to catch, so bound it rather than let
     * it stall the suite: a child that inherited a spinlock flag waits on a holder that does
     * not exist in this process. */
    alarm(10);

    if (!vppu_ledger_lock(0)) {
        return 2;
    }
    unsigned long long used = vppu_ledger_used(0);
    vppu_ledger_unlock(0);

    /* The parent charges the whole quota before releasing, so an empty card here means this
     * process was let through while the parent still held the lock — an inherited lock counted
     * as its own re-entry. */
    return (used == QUOTA_BYTES) ? 0 : 1;
}

/* Runs last on purpose: it is the only test in which the PARENT maps a region, and the mapping
 * is latched per process. */
static void test_fork_while_locked(void)
{
    setenv(VPPU_ENV_LEDGER_PATH, region_path(6), 1);

    if (!vppu_ledger_lock(0)) {
        check_that(false, "fork: a child does not inherit the parent's lock",
                   "the parent could not take a card lock to fork under");
        return;
    }

    pid_t pid = fork();
    if (pid == 0) {
        _exit(child_after_fork());
    }

    usleep(200000);
    bool charged = vppu_ledger_charge(0, QUOTA_BYTES);
    vppu_ledger_unlock(0);

    int status = 0;
    bool exited = (pid > 0 && waitpid(pid, &status, 0) == pid && WIFEXITED(status));
    int code = exited ? WEXITSTATUS(status) : -1;

    check_that(charged && exited && code == 0,
               "fork: a child does not inherit the parent's lock",
               "child verdict %d (0 = waited for the release and then saw the quota spent, "
               "1 = let through while the parent still held it, 2 = could not lock, "
               "-1 = hung or was killed)",
               code);
}

int main(void)
{
    if (mkdtemp(tmpdir) == NULL) {
        printf("FAIL | setup | cannot create a temporary directory\n");
        printf("FAILS=1\n");
        return 1;
    }

    /* Level 2, so a failing test shows the library's own diagnosis beside the row. */
    setenv(VPPU_ENV_LOG_LEVEL, "2", 1);

    test_quota_parse();
    test_quota_knob();
    test_quota_env();
    test_log_level();
    test_quota_validate();
    test_pid_feed_forward();
    test_pid_clamps();
    test_pid_convergence();
    test_alloc_map();
    test_region_layout();
    test_control_state_published();
    test_region_refusals();
    test_one_quota_two_processes();
    test_reclaim_after_death();
    test_fork_while_locked();

    printf("FAILS=%d\n", fails);
    return (fails == 0) ? 0 : 1;
}
