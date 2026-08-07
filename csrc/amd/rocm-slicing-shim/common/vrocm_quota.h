/*
 * vrocm_quota.h — the configuration half of common/: which figures the container was given,
 * where its ledger lives, and whether any of it is usable at all.
 *
 * ONLY MEMORY LIVES HERE. The compute quota is enforced by the platform, in hardware, through a
 * CU mask this library never reads and could not override; what makes that mask correct is a
 * derivation contract that belongs to the allocator, not to a preloaded object. So there is no
 * compute figure to parse and no control loop to tune — the one dimension this library enforces
 * is device memory.
 *
 * TWO TRAPS ARE AVOIDED HERE RATHER THAN IN THE CALLERS:
 *   - the quota is read from the environment on EVERY admission, never written once into the
 *     region by whichever process created it. A region that cached the first writer's figure
 *     means a container restarting with a smaller quota silently keeps the larger one, and the
 *     only way out is deleting a file nobody knows about;
 *   - a figure that is SET but unparsable is an error for the card it was set on, never "no
 *     limit" and never a fall-through to the level above. Reading a missing or broken
 *     configuration as "unlimited" is the one outcome this design forbids outright, because it
 *     turns a typo into a whole card.
 */
#ifndef VROCM_COMMON_VROCM_QUOTA_H
#define VROCM_COMMON_VROCM_QUOTA_H

#include <stdbool.h>

#include "vrocm.h"

/* The library's own namespace, deliberately NOT under `HIP_`, `HSA_` or `ROCR_`. Those three
 * belong to ROCm, are actively parsed by its runtime and are still growing; a private variable
 * placed there is a future collision. ROCm's own variables are used under their real names
 * elsewhere because they are ROCm's, not ours.
 *
 * BOTH forms exist, and the pair is what makes them a contract rather than two conventions:
 *   - the INDEXED form, suffixed with the CONTAINER-LOCAL device index — the index a card has
 *     after ROCR_VISIBLE_DEVICES has filtered and reordered the list, which is the same index
 *     space the compute mask's GPU_list uses, so numbering everything by position in that list
 *     makes the two agree automatically;
 *   - the UN-INDEXED form, which decides every card carrying no indexed figure of its own.
 *
 * The prefixes are derived from the plain names rather than spelled twice, so the two forms of
 * the dimension cannot drift apart. */
#define VROCM_ENV_MEMORY_LIMIT "VROCM_DEVICE_MEMORY_LIMIT"
#define VROCM_ENV_MEMORY_LIMIT_PREFIX VROCM_ENV_MEMORY_LIMIT "_"

/* The cross-process region's path. The allocator hands down a PER-CONTAINER one, because the
 * region is addressed by the card's position in ROCR_VISIBLE_DEVICES and that position is
 * container-local: two containers sharing a region would charge two different physical cards into
 * the slot they both call index 0.
 *
 * It has NO DEFAULT, and that is the point. A default under /tmp is either private to one
 * container, which silently accounts a slice the operator never configured, or on a shared host
 * mount, where it lets unrelated containers collide in one region. Absence is a configuration
 * error instead: reported once, and then every allocation refused. */
#define VROCM_ENV_LEDGER_PATH "VROCM_LEDGER_PATH"

/* The unit the allocator emits both forms in. Mebibytes rather than bytes because that is what
 * the request API's `.sliced.memory-*` dimension is denominated in, and a unit conversion in the
 * allocator is one place to get wrong instead of two. */
#define VROCM_MEMORY_LIMIT_UNIT (1024ULL * 1024ULL)

/* vrocm_quota_parse — a positive figure scaled by `unit`, or 0 for "unset or unusable".
 *
 * Pure and deliberately silent, so the caller decides how loudly to report each variable and the
 * unit tests can exercise the arithmetic with no environment at all. The overflow bound is not
 * decoration: a wrapped product silently becomes either 0 (nothing is enforced) or a tiny figure
 * that denies everything, and both read as a product defect rather than as bad configuration. */
VROCM_INTERNAL unsigned long long vrocm_quota_parse(const char *value, unsigned long long unit);

/* vrocm_quota_memory_bytes — one card's VRAM cap in bytes, or 0 when it carries none.
 *
 * THE PRECEDENCE: VROCM_DEVICE_MEMORY_LIMIT_<i> where that is SET, the un-indexed form
 * otherwise. Being set is what decides, not being valid — a malformed indexed figure answers 0
 * for that card and never falls through to the container-wide one, so a mistyped per-card figure
 * denies that card instead of quietly buying it somebody else's allowance.
 *
 * Out-of-range indices answer 0 rather than reading past the table. */
VROCM_INTERNAL unsigned long long vrocm_quota_memory_bytes(int device);

/* vrocm_quota_ledger_path — the configured region path, or NULL when none was given. */
VROCM_INTERNAL const char *vrocm_quota_ledger_path(void);

/* vrocm_quota_validate — report the configuration once, at load, and latch whether it is usable.
 *
 * Called from the shipped library's own constructor rather than from a constructor here, so the
 * order stays explicit instead of depending on link order.
 *
 * It does not terminate the process, and that is a deliberate departure from "fail at load":
 * this library arrives through /etc/ld.so.preload, so exiting would kill every process in the
 * container — including the shell a human would use to diagnose the misconfiguration. Reporting
 * at load and refusing every allocation afterwards is the same fail-closed outcome without
 * taking the container down with it. */
VROCM_INTERNAL void vrocm_quota_validate(void);

/* vrocm_quota_usable — false when vrocm_quota_validate() found the configuration unusable: no
 * ledger path, or no memory figure at all in either form, or one that is set and cannot be
 * parsed. Enforcement paths must refuse while this is false. */
VROCM_INTERNAL bool vrocm_quota_usable(void);

#endif /* VROCM_COMMON_VROCM_QUOTA_H */
