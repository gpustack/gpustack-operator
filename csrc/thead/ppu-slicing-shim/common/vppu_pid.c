#include "vppu_pid.h"

/* clamp_i64 — bounded on both sides, so one expression cannot leave a value outside the range
 * it was clamped into. */
static int64_t clamp_i64(int64_t value, int64_t low, int64_t high)
{
    if (value < low) {
        return low;
    }
    return (value > high) ? high : value;
}

/* min_allow — the floor the window may never close below; see VPPU_PID_MIN_DIVISOR. Never zero
 * even for a period too short to divide, because zero is a stall rather than a cap. */
static uint64_t min_allow(uint64_t period_ns)
{
    uint64_t floor_ns = period_ns / VPPU_PID_MIN_DIVISOR;

    return (floor_ns == 0ULL) ? 1ULL : floor_ns;
}

uint64_t vppu_pid_floor(unsigned int target_percent, uint64_t period_ns)
{
    if (target_percent >= 100U) {
        return period_ns;
    }

    /* Multiplied BEFORE the division, or the claim above is not true: a period that does not
     * divide by 100 loses up to 99ns per point of the target. No reachable input does — the period
     * is configured in whole milliseconds, so its nanoseconds always divide — but a floor
     * documented as exact should be exact for the argument it was given, not for the arguments it
     * happens to get. The product cannot overflow: the largest period this loop accepts is
     * milliseconds, and 100 times that is nowhere near 2^64. */
    uint64_t share = period_ns * target_percent / 100ULL;

    return (share < min_allow(period_ns)) ? min_allow(period_ns) : share;
}

uint64_t vppu_pid_step(struct vppu_pid_state *state, const struct vppu_pid_gains *gains,
                       unsigned int target_percent, unsigned int measured_percent,
                       uint64_t period_ns)
{
    /* An unstepped state takes the quota's own share of the window and nothing else: the sensor
     * cannot yet have reported what this container is doing, and folding its not-yet-risen figure
     * in would open the window past the quota on the one step where a burst is waiting. */
    if (state->allow_ns == 0ULL) {
        state->allow_ns = vppu_pid_floor(target_percent, period_ns);
        return state->allow_ns;
    }

    uint64_t allow = state->allow_ns;
    int64_t error = (int64_t)target_percent - (int64_t)measured_percent;

    /* Clamped so the integral term alone can never ask for more than one whole window. Without
     * it a container held at its floor by a long kernel winds the integral up for as long as
     * the workload runs, and the window then stays wide open for as many steps as it took to
     * accumulate — long after the load has gone. */
    int64_t integral_limit = VPPU_PID_DIVISOR / ((gains->ki > 0) ? gains->ki : 1);
    int64_t integral = clamp_i64((int64_t)state->integral + error, -integral_limit,
                                integral_limit);
    int64_t derivative = error - (int64_t)state->last_error;

    int64_t weighted = (int64_t)gains->kp * error + (int64_t)gains->ki * integral
                       + (int64_t)gains->kd * derivative;
    int64_t delta = (int64_t)(period_ns / VPPU_PID_DIVISOR) * weighted;

    int64_t next = clamp_i64((int64_t)allow + delta, (int64_t)min_allow(period_ns),
                             (int64_t)period_ns);

    state->integral = (int32_t)integral;
    state->last_error = (int32_t)error;
    state->allow_ns = (uint64_t)next;
    return state->allow_ns;
}
