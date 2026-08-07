/*
 * vrocm_log.h — the one output channel, and the level that gates it.
 *
 * THE LEVELS ARE A CONTRACT WITH THE VERIFICATION CASES, not a preference. Level 1 is
 * per-DENIAL, not per-call: it is the single line that answers "why was my allocation refused",
 * and it is the default because a workload that hits its quota with no explanation is the one
 * support case this library exists to prevent. Level 0 exists for a workload that wants absolute
 * quiet. Level 2 adds the load marker and the counter dump, and the cases decide rows by
 * grepping exactly those, so a build that could not reach level 2 would silence most of them.
 *
 * THE TAG IS THE LIBRARY NAME rather than the project name, because every case greps it out of
 * output interleaved with ROCm's own — the CANN and ROCm runtimes both write to stderr
 * unprompted — where short and unique matters more than branding.
 */
#ifndef VROCM_COMMON_VROCM_LOG_H
#define VROCM_COMMON_VROCM_LOG_H

#include <stdio.h>

#include "vrocm.h"

#define VROCM_TAG "[vrocm] "

#define VROCM_ENV_LOG_LEVEL "LIBVROCM_LOG_LEVEL"

#define VROCM_LOG_QUIET 0
#define VROCM_LOG_DENY 1
#define VROCM_LOG_DEBUG 2

/* vrocm_log_level — the level in force, read from the environment once and latched.
 *
 * Latched rather than re-read because this sits on the denial path of every allocation, and
 * because a level that changed under a running process would make a case's grep depend on when
 * it looked. */
VROCM_INTERNAL int vrocm_log_level(void);

#define vrocm_log(lvl, ...)                             \
    do {                                                \
        if (vrocm_log_level() >= (lvl)) {               \
            fprintf(stderr, VROCM_TAG __VA_ARGS__);     \
        }                                               \
    } while (0)

#endif /* VROCM_COMMON_VROCM_LOG_H */
