/*
 * vppu_quota.h — the configuration half of common/: which figures the container was given,
 * and whether they are usable at all.
 *
 * Two traps from the reference implementations are avoided here rather than in the callers:
 *   - the quota is read from the environment on EVERY call, never written once into the
 *     ledger by whichever process created it. HAMi-core does the latter, so a stale cache
 *     file freezes the old limit and changing a quota means deleting the file;
 *   - a missing or unparsable figure is an error, never "no limit". flexai reads a missing
 *     config as "not in a container" and imposes no limit at all, which is the one outcome
 *     this design forbids outright.
 */
#ifndef VPPU_COMMON_VPPU_QUOTA_H
#define VPPU_COMMON_VPPU_QUOTA_H

#include <stdbool.h>

#include "vppu.h"
#include "vppu_pid.h"

/* The SDK's own env namespace — HGGC_INJECTION64_PATH already exists as the
 * CUDA_INJECTION64_PATH analogue — so a sliced container carries one word stem rather than a
 * GPUStack-invented one beside it.
 *
 * BOTH dimensions come in two forms, and the pair is what makes them a contract rather than two
 * conventions:
 *   - the INDEXED form, suffixed with the CONTAINER-LOCAL device index, because the SDK
 *     renumbers devices inside a container: pass one card node through and it is index 0
 *     whatever its host ordinal;
 *   - the UN-INDEXED form, which decides every card carrying no indexed figure of its own.
 * HAMi-core reads CUDA_DEVICE_{MEMORY,SM}_LIMIT{,_<i>} in exactly that order, and GPUStack's
 * NVIDIA allocator emits the un-indexed compute figure alone — so the fallback is also what a
 * container sliced by an allocator that knows only one figure keeps running on.
 *
 * The prefixes are derived from the plain names rather than spelled twice, so the two forms of a
 * dimension cannot drift apart. */
#define VPPU_ENV_MEMORY_LIMIT "HGGC_DEVICE_MEMORY_LIMIT"
#define VPPU_ENV_MEMORY_LIMIT_PREFIX VPPU_ENV_MEMORY_LIMIT "_"
#define VPPU_ENV_SM_LIMIT "HGGC_DEVICE_SM_LIMIT"
#define VPPU_ENV_SM_LIMIT_PREFIX VPPU_ENV_SM_LIMIT "_"

/* The compute controller's own knobs. They are tuning rather than quota, so each falls back to
 * its default when unset or out of range instead of making the container unusable: a mistyped
 * gain still leaves a working loop, where a mistyped cap would leave no cap.
 *
 * They exist at all because the gains are deliberately not inherited from the reference
 * implementation — see vppu_pid.h — so the one place they can be fitted is the hardware they
 * run on. */
#define VPPU_ENV_SM_PERIOD_MS "HGGC_SM_CONTROL_PERIOD_MS"
#define VPPU_ENV_SM_STEP_MS "HGGC_SM_CONTROL_STEP_MS"
#define VPPU_ENV_SM_KP "HGGC_SM_CONTROL_KP"
#define VPPU_ENV_SM_KI "HGGC_SM_CONTROL_KI"
#define VPPU_ENV_SM_KD "HGGC_SM_CONTROL_KD"
#define VPPU_ENV_SM_GRAPH_WEIGHT "HGGC_SM_GRAPH_WEIGHT"

/* The gating window: 100 ms is short enough that a throttled workload waits in small pieces
 * rather than long stalls. The bounds keep a typo from producing a window a workload would appear
 * to hang in. */
#define VPPU_SM_PERIOD_MS_DEFAULT 100U
#define VPPU_SM_PERIOD_MS_MIN 1U
#define VPPU_SM_PERIOD_MS_MAX 10000U

/* How often the loop steps, which is a property of the SENSOR rather than of the window. This
 * driver's per-process utilisation figure moves at about ten percentage points per hundred
 * milliseconds however abruptly the load changed, so a full swing takes a second — measured, not
 * assumed. Stepping faster than that means acting on a figure that has not caught up yet, and
 * a loop that does oscillates across the whole range instead of settling. */
#define VPPU_SM_STEP_MS_DEFAULT 1000U
#define VPPU_SM_STEP_MS_MIN 10U
#define VPPU_SM_STEP_MS_MAX 60000U

/* Gains in hundredths (see VPPU_PID_SCALE), chosen against the simulated card in common/'s unit
 * tests rather than fitted to a PPU. The derivative term is off by default because the feedback
 * is a sampled utilisation figure, where differentiating mostly amplifies sampling noise; the
 * knob exists so it can be tried on hardware. */
#define VPPU_SM_KP_DEFAULT 25
#define VPPU_SM_KI_DEFAULT 8
#define VPPU_SM_KD_DEFAULT 0
#define VPPU_SM_GAIN_MAX 10000U

/* A graph launch runs however many kernels were captured into it, so it may consume far more of
 * the card than one launch. The weight is how many launches' worth of the window it is charged
 * for, and it defaults to 1 — off — because whether graphs escape the cap on this hardware is a
 * measurement nobody has made yet. */
#define VPPU_SM_GRAPH_WEIGHT_DEFAULT 1U
#define VPPU_SM_GRAPH_WEIGHT_MAX 1024U

/* vppu_quota_parse — a positive figure scaled by `unit`, or 0 for "unset or unusable".
 *
 * Pure, and deliberately silent, so the caller decides how loudly to report each variable
 * and the unit tests can exercise the arithmetic with no environment at all. The overflow
 * bound is not decoration: a wrapped product silently becomes either 0 (nothing is enforced)
 * or a tiny figure that denies everything, and both read as a product defect rather than as
 * bad configuration.
 */
VPPU_INTERNAL unsigned long long vppu_quota_parse(const char *value, unsigned long long unit);

/* vppu_quota_knob — a bounded integer setting, or `fallback` when it is unset, malformed or out
 * of [low, high].
 *
 * Separate from vppu_quota_parse() because zero is a legitimate value for a gain while a quota
 * of zero is not a quota, so the two cannot share the "0 means unusable" convention. Pure and
 * silent for the same reason: the caller decides how loudly to report a value it rejected. */
VPPU_INTERNAL unsigned int vppu_quota_knob(const char *value, unsigned int low, unsigned int high,
                                           unsigned int fallback);

/* THE PRECEDENCE BOTH FIGURES FOLLOW, stated once because both getters below implement it:
 * HGGC_DEVICE_<dimension>_LIMIT_<i> where that is SET, the un-indexed form otherwise.
 *
 * Being set is what decides, not being valid. A malformed figure answers 0 — "this card is
 * unusable" — at the level it was set on and never falls through to the level above, so a
 * mistyped per-card figure denies that card instead of quietly buying it the container-wide one.
 * HAMi-core does fall through there, and for compute then defaults to 100, which turns a typo
 * into a whole card's worth of compute.
 *
 * Out-of-range indices answer 0 rather than reading past the table. */

/* vppu_quota_memory_bytes — one card's VRAM cap in bytes, or 0 when it carries none. */
VPPU_INTERNAL unsigned long long vppu_quota_memory_bytes(int device);

/* vppu_quota_sm_percent — one card's compute cap in percent, or 0 when it carries none. A figure
 * above 100 is not a cap and answers 0 too. */
VPPU_INTERNAL unsigned int vppu_quota_sm_percent(int device);

/* vppu_quota_sm_period_ms — the compute controller's gating window, in milliseconds. */
VPPU_INTERNAL unsigned int vppu_quota_sm_period_ms(void);

/* vppu_quota_sm_step_ms — how often the loop may step, in milliseconds. */
VPPU_INTERNAL unsigned int vppu_quota_sm_step_ms(void);

/* vppu_quota_sm_gains — the controller's gains, each defaulted independently so tuning one does
 * not require restating the others. */
VPPU_INTERNAL void vppu_quota_sm_gains(struct vppu_pid_gains *gains);

/* vppu_quota_graph_weight — how many launches' worth of the window a graph launch is charged
 * for; 1 leaves graph launches gated exactly like any other. */
VPPU_INTERNAL unsigned int vppu_quota_graph_weight(void);

/* vppu_quota_validate — report the configuration once, at load, and latch whether it is
 * usable.
 *
 * Called from the shipped library's own constructor rather than from a constructor here, so
 * the order stays explicit instead of depending on link order.
 *
 * It does not terminate the process, and that is a deliberate departure from "fail at load":
 * this library arrives through /etc/ld.so.preload, so exiting would kill every process in
 * the container — the shell a human would use to diagnose the misconfiguration included.
 * Reporting at load and refusing every allocation afterwards is the same fail-closed outcome
 * without taking the container down with it.
 */
VPPU_INTERNAL void vppu_quota_validate(void);

/* vppu_quota_usable — false when vppu_quota_validate() found the container's configuration
 * unusable: no memory figure at all in either form, one that is set and cannot be parsed, or a
 * card carrying a memory figure whose compute figure is missing or unparsable. Enforcement paths
 * must refuse while this is false.
 *
 * With only the un-indexed memory figure set, EVERY card is a card the container was given —
 * nothing in the environment says how many it holds — so the un-indexed compute figure has to be
 * usable too. Pairing the un-indexed forms is therefore the whole configuration; pairing an
 * indexed memory figure with the un-indexed compute figure works the same way, per card.
 *
 * The compute figure counts towards this only now that the controller enforces it. Until then a
 * missing one was reported and nothing more, because refusing over a dimension the library did
 * not implement would have failed closed on the wrong thing. The allocator has to inject it
 * explicitly even at 100%: its own helper defaults that request to 100, so an omitted variable
 * is indistinguishable from a whole card's worth of compute. */
VPPU_INTERNAL bool vppu_quota_usable(void);

#endif /* VPPU_COMMON_VPPU_QUOTA_H */
