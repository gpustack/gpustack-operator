/*
 * vppu_pid.h — the compute controller's arithmetic, and nothing else.
 *
 * WHY THIS IS ITS OWN FILE IN common/. The controller has two halves with very different
 * verifiability. Reading a card's utilisation and gating a launch need a PPU, the vendor's
 * library and a workload; deciding what the next allowance should be needs neither, and it is
 * the half that can be wrong in ways hardware would only show as "it oscillates". So the
 * decision lives here, where the rule this directory follows — no `hg*`, `hggc*` or `hgml*`
 * type may appear — makes it exercisable with no device at all, including a convergence run
 * against a simulated card.
 *
 * WHAT THE CONTROLLER ACTUATES is a duty cycle, not a token bucket. Each card has a repeating
 * window of `period_ns`; launches are admitted during the first `allow_ns` of it and made to
 * wait out the rest. Two properties decided this over HAMi-core's token bucket:
 *   - the cold-start allowance is EXACT. `period_ns * limit / 100` is the container's share of
 *     wall time by construction, so a burst arriving before the first measurement cannot take
 *     the whole card. A token bucket needs a launches-per-period ceiling to derive the same
 *     floor from, and that figure is hardware this project has never profiled;
 *   - when utilisation cannot be read at all, holding the last allowance still enforces a
 *     quota-derived duty cycle. Fail-closed is then the natural behaviour rather than an extra
 *     mechanism.
 *
 * TWO TIMESCALES, AND THE SECOND ONE WAS MEASURED. The window repeats every few tens of
 * milliseconds so a throttled workload is held back in small pieces rather than long stalls. The
 * LOOP, though, may only step as fast as its sensor can answer: this driver's per-process
 * utilisation figure is slew-rate limited to about ten percentage points per hundred
 * milliseconds, in both directions, so it needs a full second to travel from 0 to 100 however
 * abruptly the card's real load changed. A loop stepping every window would act on a figure up to
 * a second stale in the direction it had just moved, and it does exactly what that implies — it
 * oscillates across the whole range. So stepping is a separate, slower cadence, and the two are
 * configured apart.
 *
 * THE GAINS ARE NOT INHERITED. flexai carries two hardcoded triples fitted to their own NVIDIA
 * hardware; copying them would be fitting this loop to a card it has never seen. The defaults
 * below were chosen against the simulated first-order plant in the unit tests, which is honest
 * about what they are: a stable starting point, not a fit. That is also why the loop's state is
 * published in the usage region — an unobservable loop cannot be tuned on unfamiliar hardware.
 */
#ifndef VPPU_COMMON_VPPU_PID_H
#define VPPU_COMMON_VPPU_PID_H

#include <stdint.h>

#include "vppu.h"

/* Gains are integers in hundredths, so a gain of 1.0 is written 100. Integer arithmetic
 * throughout: a floating-point multiply here would buy nothing and this library is loaded into
 * arbitrary workloads, where the fewer surprises about FPU state the better. */
#define VPPU_PID_SCALE 100

/* The error is a percentage and the gains are hundredths, so a term has to be divided by both
 * before it scales the window. */
#define VPPU_PID_DIVISOR (VPPU_PID_SCALE * 100)

/* The smallest allowance the loop may settle on, as a fraction of the window. A window that
 * closed completely would stall a workload for as long as the quota stood, so the loop is
 * allowed to squeeze compute and never to stop it: a kernel long enough to keep the card busy
 * on its own cannot be made shorter by refusing the NEXT launch, and refusing them all would
 * turn a compute cap into a hang. */
#define VPPU_PID_MIN_DIVISOR 100

/* The three gains, in the units above. Carried as one value because they are configured,
 * reported and applied together, and a caller that could set one of them alone would be
 * tuning half a loop. */
struct vppu_pid_gains {
    int32_t kp;
    int32_t ki;
    int32_t kd;
};

/* One card's controller state, laid out to occupy the usage region's reserved words exactly.
 *
 * It is shared by every process in the container on purpose: the compute cap is the
 * container's, so two processes launching on one card have to divide one window rather than
 * each get their own. Whichever of them notices that the window has expired restamps it, and
 * whichever notices the step is due runs it; the others read the result.
 *
 * The two timestamps belong to the CALLER, which is the half of this that owns a clock. The
 * arithmetic here never reads or writes them. */
struct vppu_pid_state {
    uint64_t window_start_ns; /* when the current window opened, on CLOCK_MONOTONIC */
    uint64_t allow_ns;        /* how much of that window admits launches; 0 = never stepped */
    uint64_t last_step_ns;    /* when the loop last stepped, on the same clock */
    int32_t integral;         /* accumulated error, clamped against windup */
    int32_t last_error;       /* the previous step's error, for the derivative term */
};

_Static_assert(sizeof(struct vppu_pid_state) == 32, "the controller state must fit the region");

/* vppu_pid_floor — the allowance a container starts with, before anything has been measured:
 * its quota's own share of the window. Also the answer for a target of 0, which no caller
 * should be enforcing, and 100, where the window is the whole period. */
VPPU_INTERNAL uint64_t vppu_pid_floor(unsigned int target_percent, uint64_t period_ns);

/* vppu_pid_step — one control step: fold a measurement into the state and return the allowance
 * for the next window.
 *
 * `target_percent` is the configured cap and `measured_percent` what the container was observed
 * using; both are percentages of one card. `period_ns` is the WINDOW, because that is what the
 * allowance is a fraction of — how often this is called is the caller's business.
 *
 * A state whose `allow_ns` is still 0 has never been stepped. That call takes vppu_pid_floor()
 * and returns, deliberately ignoring the measurement: the sensor is slew-rate limited from zero,
 * so the first reading of a card that is already busy is near zero whatever it is really doing,
 * and acting on it would open the window wider than the quota at exactly the moment a burst could
 * use it. This is the feed-forward half of the loop, and it is why a cold start cannot be handed
 * the whole card.
 *
 * The returned allowance is clamped to [period_ns / VPPU_PID_MIN_DIVISOR, period_ns] and also
 * stored in the state, so a caller may use either.
 */
VPPU_INTERNAL uint64_t vppu_pid_step(struct vppu_pid_state *state,
                                     const struct vppu_pid_gains *gains,
                                     unsigned int target_percent, unsigned int measured_percent,
                                     uint64_t period_ns);

#endif /* VPPU_COMMON_VPPU_PID_H */
