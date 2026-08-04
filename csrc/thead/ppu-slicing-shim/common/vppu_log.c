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

    if (level < 0) {
        const char *value = getenv(VPPU_ENV_LOG_LEVEL);
        char *end = NULL;
        level = VPPU_LOG_DENY;
        if (value != NULL && *value != '\0') {
            long parsed = strtol(value, &end, 10);

            /* A level that cannot be parsed keeps the default instead of silencing the library.
             * strtol() answers 0 for "abc", and 0 is the one level that hides denials as well — so
             * without this check a typo in a verbosity knob would take the fail-closed diagnostics
             * with it, and a container refusing every allocation would say nothing about why.
             * Trailing junk is rejected for the reason vppu_quota_parse() rejects it: "2x" is not
             * a level somebody chose. A negative figure is not one either. */
            if (end != value && *end == '\0' && parsed >= 0) {
                level = (int)parsed;
            }
        }
    }
    return level;
}
