#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>

#include "vrocm_log.h"
#include "vrocm_quota.h"

/* env_indexed — the indexed form's value for one card, or NULL when the variable is not set.
 *
 * NULL means "not set", which is what the precedence turns on; an empty string is a value the
 * caller must reject, not a variable it may ignore. */
static const char *env_indexed(int device)
{
    char name[sizeof(VROCM_ENV_MEMORY_LIMIT_PREFIX) + 16];

    snprintf(name, sizeof(name), VROCM_ENV_MEMORY_LIMIT_PREFIX "%d", device);
    return getenv(name);
}

VROCM_INTERNAL unsigned long long vrocm_quota_parse(const char *value, unsigned long long unit)
{
    char *end = NULL;
    unsigned long long parsed;

    if (value == NULL || *value == '\0' || unit == 0) {
        return 0;
    }
    /* A figure starts with a digit, and anything else is refused before strtoull sees it.
     *
     * The rule is written that way rather than as a test for '-' because strtoull skips leading
     * whitespace and then accepts a sign: " -10" therefore never reaches a test on the first
     * character, and wraps into a huge positive figure. The overflow test below does catch that
     * one -- a wrapped negative is far above ULLONG_MAX/MiB -- but only because the unit is
     * large, and a quota is not the place to depend on an unrelated check for a whole class of
     * input. Refusing outright also matches how the tail is treated: trailing junk is rejected,
     * so leading junk should be too. */
    if (*value < '0' || *value > '9') {
        return 0;
    }

    errno = 0;
    parsed = strtoull(value, &end, 10);
    if (errno != 0 || end == NULL || *end != '\0' || parsed == 0) {
        return 0;
    }
    if (parsed > ULLONG_MAX / unit) {
        return 0;
    }
    return parsed * unit;
}

VROCM_INTERNAL unsigned long long vrocm_quota_memory_bytes(int device)
{
    const char *value;

    if (device < 0 || device >= VROCM_MAX_DEVICES) {
        return 0;
    }

    value = env_indexed(device);
    if (value == NULL) {
        value = getenv(VROCM_ENV_MEMORY_LIMIT);
    }
    return vrocm_quota_parse(value, VROCM_MEMORY_LIMIT_UNIT);
}

VROCM_INTERNAL const char *vrocm_quota_ledger_path(void)
{
    const char *path = getenv(VROCM_ENV_LEDGER_PATH);

    return (path != NULL && *path != '\0') ? path : NULL;
}

static bool usable;

/* any_figure_named — whether either form of the memory figure names anything at all.
 *
 * "Given no quota" and "given a quota this build cannot use" are different states, and only the
 * second is worth a line at the default level. Answered by asking rather than by remembering,
 * because it is read once per process at load and never on an allocation path. */
static bool any_figure_named(void)
{
    int device;

    if (getenv(VROCM_ENV_MEMORY_LIMIT) != NULL) {
        return true;
    }
    for (device = 0; device < VROCM_MAX_DEVICES; device++) {
        if (env_indexed(device) != NULL) {
            return true;
        }
    }
    return false;
}

/* Reporting walks the indexed forms as well as the un-indexed one so a container is told about
 * every figure it was actually given, rather than only about the one this process happens to
 * look up first. A card carrying nothing at all is not reported: with only the un-indexed form
 * set, every card is a card the container was given, and listing 64 of them would bury the line
 * that matters. */
VROCM_INTERNAL void vrocm_quota_validate(void)
{
    const char *path = vrocm_quota_ledger_path();
    const char *shared = getenv(VROCM_ENV_MEMORY_LIMIT);
    bool any_figure = false;
    bool any_broken = false;
    int device;

    usable = false;

    if (path == NULL) {
        /* Loud when this process was asked to be policed and quiet when it was not, because the
         * two absences are different problems. THIS LIBRARY LOADS INTO EVERY PROCESS IN THE
         * CONTAINER -- `/etc/ld.so.preload` does not distinguish a workload from the `sed` in a
         * start-up script -- so a container that carries it and no configuration at all would
         * otherwise print this line hundreds of times and say nothing an operator can act on. A
         * figure without a path is the case that matters: something was configured, incompletely.
         *
         * Nothing is lost by going quiet, because the refusal itself now says so: a process that
         * actually asks for memory and is refused prints one line from `vrocm_ledger_admit`. */
        vrocm_log(any_figure_named() ? VROCM_LOG_DENY : VROCM_LOG_DEBUG,
                  "%s is unset; every allocation will be refused\n", VROCM_ENV_LEDGER_PATH);
        return;
    }

    if (shared != NULL) {
        unsigned long long bytes = vrocm_quota_parse(shared, VROCM_MEMORY_LIMIT_UNIT);

        if (bytes == 0) {
            vrocm_log(VROCM_LOG_DENY, "%s=\"%s\" is not a usable figure\n",
                      VROCM_ENV_MEMORY_LIMIT, shared);
            any_broken = true;
        } else {
            any_figure = true;
            vrocm_log(VROCM_LOG_DEBUG, "%s = %llu MiB (every card carrying no figure of its own)\n",
                      VROCM_ENV_MEMORY_LIMIT, bytes / VROCM_MEMORY_LIMIT_UNIT);
        }
    }

    for (device = 0; device < VROCM_MAX_DEVICES; device++) {
        const char *value = env_indexed(device);
        unsigned long long bytes;

        if (value == NULL) {
            continue;
        }
        bytes = vrocm_quota_parse(value, VROCM_MEMORY_LIMIT_UNIT);
        if (bytes == 0) {
            vrocm_log(VROCM_LOG_DENY, "%s%d=\"%s\" is not a usable figure; card %d is refused\n",
                      VROCM_ENV_MEMORY_LIMIT_PREFIX, device, value, device);
            any_broken = true;
            continue;
        }
        any_figure = true;
        vrocm_log(VROCM_LOG_DEBUG, "%s%d = %llu MiB\n", VROCM_ENV_MEMORY_LIMIT_PREFIX, device,
                  bytes / VROCM_MEMORY_LIMIT_UNIT);
    }

    if (!any_figure) {
        vrocm_log(VROCM_LOG_DENY, "no usable %s figure in either form; "
                                  "every allocation will be refused\n",
                  VROCM_ENV_MEMORY_LIMIT);
        return;
    }

    /* A container that carries at least one usable figure is usable, even if another card's
     * figure is broken: that card answers 0 from vrocm_quota_memory_bytes() and is refused on
     * its own, which is narrower than refusing the whole container for one typo. */
    usable = true;
    vrocm_log(VROCM_LOG_DEBUG, "ledger %s%s\n", path,
              any_broken ? " (some cards carry an unusable figure and are refused)" : "");
}

VROCM_INTERNAL bool vrocm_quota_usable(void)
{
    return usable;
}
