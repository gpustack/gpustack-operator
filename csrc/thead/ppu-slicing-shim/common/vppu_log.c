/*
 * vppu_log.c — the verbosity level, read once.
 *
 * Read lazily rather than from a constructor: this library is preloaded, so the order its
 * constructors run relative to the vendor's is not ours to choose, and a level cached on
 * first use is correct whichever of them runs first.
 */
#include <stdlib.h>

#include "vppu.h"

int vppu_log_level(void)
{
    static int level = -1;

    int answer = __atomic_load_n(&level, __ATOMIC_RELAXED);

    if (answer >= 0) {
        return answer;
    }

    /* COMPUTED INTO A LOCAL AND PUBLISHED ONCE, which is two guarantees rather than one.
     *
     * Nothing elects a single reader, so two of this library's threads can arrive here together
     * and both store. Two plain stores to one object are a data race whatever they store — these
     * two read the same environment and reach the same answer, and that does not make it defined.
     * Relaxed is the whole requirement: no other memory is ordered against this value.
     *
     * And the default no longer passes through the latch on its way to being replaced. Written
     * straight into `level` it would stand there as a settled figure for the length of a getenv
     * and a strtol, and a thread reading in that window would take it for the configured one. */
    answer = VPPU_LOG_DENY;
    {
        const char *value = getenv(VPPU_ENV_LOG_LEVEL);
        char *end = NULL;

        if (value != NULL && *value != '\0') {
            long parsed = strtol(value, &end, 10);

            /* A level that cannot be parsed keeps the default instead of silencing the library.
             * strtol() answers 0 for "abc", and 0 is the one level that hides denials as well — so
             * without this check a typo in a verbosity knob would take the fail-closed diagnostics
             * with it, and a container refusing every allocation would say nothing about why.
             * Trailing junk is rejected for the reason vppu_quota_parse() rejects it: "2x" is not
             * a level somebody chose. A negative figure is not one either. */
            if (end != value && *end == '\0' && parsed >= 0) {
                answer = (int)parsed;
            }
        }
    }
    __atomic_store_n(&level, answer, __ATOMIC_RELAXED);
    return answer;
}
