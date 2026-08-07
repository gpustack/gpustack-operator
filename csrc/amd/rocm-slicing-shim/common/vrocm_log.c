#include <stdlib.h>

#include "vrocm_log.h"

/* Out-of-range and malformed values fall back to the default rather than making the container
 * unusable: verbosity is diagnostics, not quota, and a mistyped log level that refused every
 * allocation would be a far worse outcome than a mistyped log level that logs normally. */
VROCM_INTERNAL int vrocm_log_level(void)
{
    static int level = -1;

    const char *value;
    char *end = NULL;
    long parsed;
    int answer = __atomic_load_n(&level, __ATOMIC_RELAXED);

    if (answer >= 0) {
        return answer;
    }

    /* COMPUTED INTO A LOCAL AND PUBLISHED ONCE, which is two guarantees rather than one.
     *
     * Nothing elects a single reader, so two threads can arrive here together and both store.
     * Two plain stores to one object are a data race whatever they store -- these two read the
     * same environment and reach the same answer, and that does not make it defined. Relaxed is
     * the whole requirement: no other memory is ordered against this value.
     *
     * And the default no longer passes through the latch on its way to being replaced. Written
     * straight into `level` it would stand there as a settled figure for the length of a getenv
     * and a strtol, and a thread reading in that window would take it for the configured one. */
    answer = VROCM_LOG_DENY;
    value = getenv(VROCM_ENV_LOG_LEVEL);
    if (value != NULL && *value != '\0') {
        parsed = strtol(value, &end, 10);
        if (end != NULL && *end == '\0' && parsed >= VROCM_LOG_QUIET &&
            parsed <= VROCM_LOG_DEBUG) {
            answer = (int)parsed;
        }
    }
    __atomic_store_n(&level, answer, __ATOMIC_RELAXED);
    return answer;
}
