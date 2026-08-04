#include <limits.h>
#include <stdio.h>
#include <stdlib.h>

#include "vppu_quota.h"

#define VPPU_MIB (1024ULL * 1024ULL)

/* Long enough for the longest prefix plus the widest index the bound above allows. The memory
 * prefix is the longer of the two, so one bound serves both. */
#define VPPU_ENV_NAME_MAX (sizeof(VPPU_ENV_MEMORY_LIMIT_PREFIX) + 16)

/* How a card is named in the load-time report: "card <i>", or the sentence the un-indexed figures
 * stand for. */
#define VPPU_LABEL_MAX 40

static bool config_usable = true;

unsigned long long vppu_quota_parse(const char *value, unsigned long long unit)
{
    if (value == NULL || *value == '\0' || unit == 0ULL) {
        return 0ULL;
    }

    char *end = NULL;
    unsigned long long parsed = strtoull(value, &end, 10);

    /* Trailing junk is rejected rather than tolerated: "4096MiB" parsing as 4096 would make
     * a unit the caller did not choose look like it worked. */
    if (end == value || *end != '\0') {
        return 0ULL;
    }
    if (parsed == 0ULL || parsed > ULLONG_MAX / unit) {
        return 0ULL;
    }
    return parsed * unit;
}

unsigned int vppu_quota_knob(const char *value, unsigned int low, unsigned int high,
                             unsigned int fallback)
{
    if (value == NULL || *value == '\0') {
        return fallback;
    }

    char *end = NULL;
    unsigned long long parsed = strtoull(value, &end, 10);

    if (end == value || *end != '\0' || parsed < low || parsed > high) {
        return fallback;
    }
    return (unsigned int)parsed;
}

/* env_name — the variable that carries one card's figure of the given kind, or NULL for an index
 * out of range. */
static const char *env_name(const char *prefix, int device, char *buf, size_t len)
{
    if (device < 0 || device >= VPPU_MAX_DEVICES) {
        return NULL;
    }
    if (snprintf(buf, len, "%s%d", prefix, device) < 0) {
        return NULL;
    }
    return buf;
}

/* quota_source — which variable decides one card's figure of a dimension, and what it holds.
 *
 * The one place the precedence lives, for BOTH dimensions, so the figure the enforcement paths
 * use and the name the load-time report prints can never disagree. `buf` carries the indexed name
 * when that is the answer, so it must outlive the returned pointer. */
static const char *quota_source(const char *prefix, const char *plain, int device, char *buf,
                                size_t len, const char **value_out)
{
    const char *name = env_name(prefix, device, buf, len);
    const char *value = (name != NULL) ? getenv(name) : NULL;

    /* Being SET is what stops the search, not being usable: an indexed figure that cannot be
     * parsed must deny its card rather than fall through to the container-wide one. */
    if (value != NULL && *value != '\0') {
        *value_out = value;
        return name;
    }
    *value_out = getenv(plain);
    return plain;
}

unsigned long long vppu_quota_memory_bytes(int device)
{
    char buf[VPPU_ENV_NAME_MAX];
    const char *value = NULL;

    if (device < 0 || device >= VPPU_MAX_DEVICES) {
        return 0ULL;
    }
    quota_source(VPPU_ENV_MEMORY_LIMIT_PREFIX, VPPU_ENV_MEMORY_LIMIT, device, buf, sizeof(buf),
                 &value);
    return vppu_quota_parse(value, VPPU_MIB);
}

unsigned int vppu_quota_sm_percent(int device)
{
    char buf[VPPU_ENV_NAME_MAX];
    const char *value = NULL;

    if (device < 0 || device >= VPPU_MAX_DEVICES) {
        return 0U;
    }
    quota_source(VPPU_ENV_SM_LIMIT_PREFIX, VPPU_ENV_SM_LIMIT, device, buf, sizeof(buf), &value);

    unsigned long long percent = vppu_quota_parse(value, 1ULL);

    /* A figure above 100 is not a cap, so it is treated as unset and reported by
     * vppu_quota_validate() rather than clamped into something the operator did not ask
     * for. */
    if (percent > 100ULL) {
        return 0U;
    }
    return (unsigned int)percent;
}

unsigned int vppu_quota_sm_period_ms(void)
{
    return vppu_quota_knob(getenv(VPPU_ENV_SM_PERIOD_MS), VPPU_SM_PERIOD_MS_MIN,
                           VPPU_SM_PERIOD_MS_MAX, VPPU_SM_PERIOD_MS_DEFAULT);
}

unsigned int vppu_quota_sm_step_ms(void)
{
    return vppu_quota_knob(getenv(VPPU_ENV_SM_STEP_MS), VPPU_SM_STEP_MS_MIN, VPPU_SM_STEP_MS_MAX,
                           VPPU_SM_STEP_MS_DEFAULT);
}

void vppu_quota_sm_gains(struct vppu_pid_gains *gains)
{
    gains->kp = (int32_t)vppu_quota_knob(getenv(VPPU_ENV_SM_KP), 0U, VPPU_SM_GAIN_MAX,
                                         VPPU_SM_KP_DEFAULT);
    gains->ki = (int32_t)vppu_quota_knob(getenv(VPPU_ENV_SM_KI), 0U, VPPU_SM_GAIN_MAX,
                                         VPPU_SM_KI_DEFAULT);
    gains->kd = (int32_t)vppu_quota_knob(getenv(VPPU_ENV_SM_KD), 0U, VPPU_SM_GAIN_MAX,
                                         VPPU_SM_KD_DEFAULT);
}

unsigned int vppu_quota_graph_weight(void)
{
    return vppu_quota_knob(getenv(VPPU_ENV_SM_GRAPH_WEIGHT), 1U, VPPU_SM_GRAPH_WEIGHT_MAX,
                           VPPU_SM_GRAPH_WEIGHT_DEFAULT);
}

/* report_compute — name the compute figure that will act on one card and the tuning behind it, and
 * answer whether it is a cap at all.
 *
 * Asked of every card the environment names, because naming a card is how the container says it
 * was given one. A card with no usable compute figure makes the configuration unusable: a sliced
 * container running with no cap on compute is precisely flexai's missing-config outcome, which
 * this design forbids outright — and the allocator's own helper defaults the request to 100%, so
 * an omitted variable cannot be read as "this container was not sliced".
 *
 * `label` is how the card is named in the report: its index, or the sentence that stands for every
 * card the un-indexed figures cover, since how many those are is not in the environment.
 */
static bool report_compute(int device, const char *label)
{
    char buf[VPPU_ENV_NAME_MAX];
    const char *value = NULL;
    const char *name = quota_source(VPPU_ENV_SM_LIMIT_PREFIX, VPPU_ENV_SM_LIMIT, device, buf,
                                    sizeof(buf), &value);
    unsigned int sm = vppu_quota_sm_percent(device);

    if (value == NULL || *value == '\0') {
        vppu_log(VPPU_LOG_DENY,
                 "no %s%d and no %s configured — this library is only preloaded into sliced "
                 "containers, so every allocation and every launch on %s will be denied\n",
                 VPPU_ENV_SM_LIMIT_PREFIX, device, VPPU_ENV_SM_LIMIT, label);
        return false;
    }
    if (sm == 0U) {
        vppu_log(VPPU_LOG_DENY, "unusable %s=%s\n", name, value);
        return false;
    }

    /* The tuning in force, reported beside the figure it applies to: the gains are not fitted to
     * this hardware, so the first thing anyone diagnosing a throttled workload needs is which
     * ones were actually used. Per card, because each card carries its own window and its own
     * cap — one line for the container could name neither. */
    if (sm >= 100U) {
        vppu_log(VPPU_LOG_DEBUG, "%s=%u — %s's compute is uncapped, launches are counted only\n",
                 name, sm, label);
    } else {
        struct vppu_pid_gains gains;

        vppu_quota_sm_gains(&gains);
        vppu_log(VPPU_LOG_DEBUG,
                 "%s=%u on %s, window %ums, step %ums, gains kp=%d ki=%d kd=%d, graph "
                 "weight %u\n",
                 name, sm, label, vppu_quota_sm_period_ms(), vppu_quota_sm_step_ms(), gains.kp,
                 gains.ki, gains.kd, vppu_quota_graph_weight());
    }
    return true;
}

/* indexed_value — what one card's own variable of a dimension holds, or NULL when it is not set.
 * Distinguishing "not set" from "set to nothing usable" is the whole precedence rule, so the two
 * answers stay apart here as well. */
static const char *indexed_value(const char *prefix, int device, char *buf, size_t len)
{
    const char *name = env_name(prefix, device, buf, len);
    const char *value = (name != NULL) ? getenv(name) : NULL;

    return (value != NULL && *value != '\0') ? value : NULL;
}

/* fallback_stand_in — a card whose compute figure comes from the un-indexed variable, so that
 * validating it validates the FALLBACK rather than some card's own override. -1 when every card
 * carries an override and the un-indexed compute figure therefore decides nothing. */
static int fallback_stand_in(void)
{
    for (int i = 0; i < VPPU_MAX_DEVICES; i++) {
        char buf[VPPU_ENV_NAME_MAX];

        if (indexed_value(VPPU_ENV_SM_LIMIT_PREFIX, i, buf, sizeof(buf)) == NULL) {
            return i;
        }
    }
    return -1;
}

void vppu_quota_validate(void)
{
    static bool done;

    if (done) {
        return;
    }
    done = true;

    int configured = 0;
    int unusable = 0;

    /* A card is looked at when EITHER dimension names it by index, because either variable being
     * present says the container was given that card. Both of its figures are then validated
     * together: a card resolving a memory figure but no compute figure would otherwise launch with
     * compute uncapped, since the launch path reads an unresolved cap as "nothing to wait for". */
    for (int i = 0; i < VPPU_MAX_DEVICES; i++) {
        char mem_buf[VPPU_ENV_NAME_MAX];
        char sm_buf[VPPU_ENV_NAME_MAX];
        char label[VPPU_LABEL_MAX];
        const char *memory = indexed_value(VPPU_ENV_MEMORY_LIMIT_PREFIX, i, mem_buf,
                                           sizeof(mem_buf));

        if (memory == NULL && indexed_value(VPPU_ENV_SM_LIMIT_PREFIX, i, sm_buf, sizeof(sm_buf))
                                  == NULL) {
            continue;
        }
        if (memory != NULL && vppu_quota_parse(memory, VPPU_MIB) == 0ULL) {
            /* Set but unusable has to be named. The enforcement path treats 0 as "not
             * configured", so a typo would otherwise be indistinguishable from a card that
             * was never sliced. */
            vppu_log(VPPU_LOG_DENY, "unusable %s%d=%s\n", VPPU_ENV_MEMORY_LIMIT_PREFIX, i, memory);
            unusable++;
            continue;
        }
        if (memory != NULL) {
            configured++;
        }
        snprintf(label, sizeof(label), "card %d", i);
        if (!report_compute(i, label)) {
            unusable++;
        }
    }

    /* Then the un-indexed memory figure, which decides every card carrying none of its own. HOW
     * MANY those are is not in the environment — nothing here enumerates the container's cards —
     * so it is validated once, standing for all of them, beside the compute figure that has to
     * pair with it. */
    const char *shared = getenv(VPPU_ENV_MEMORY_LIMIT);
    if (shared != NULL && *shared != '\0') {
        if (vppu_quota_parse(shared, VPPU_MIB) == 0ULL) {
            vppu_log(VPPU_LOG_DENY, "unusable %s=%s\n", VPPU_ENV_MEMORY_LIMIT, shared);
            unusable++;
        } else {
            int stand_in = fallback_stand_in();

            configured++;
            if (stand_in >= 0
                && !report_compute(stand_in, "every card with no figure of its own")) {
                unusable++;
            }
        }
    }

    if (configured == 0) {
        vppu_log(VPPU_LOG_DENY,
                 "no %s<i> and no %s configured — this library is only preloaded into sliced "
                 "containers, so every allocation will be denied\n",
                 VPPU_ENV_MEMORY_LIMIT_PREFIX, VPPU_ENV_MEMORY_LIMIT);
    }

    config_usable = (configured > 0 && unusable == 0);
}

bool vppu_quota_usable(void)
{
    return config_usable;
}
