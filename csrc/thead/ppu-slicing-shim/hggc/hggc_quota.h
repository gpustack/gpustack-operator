/*
 * hggc_quota.h — the hggc module's internal contract: the interposed entry list, the two
 * admission decisions behind it (memory and compute), and the per-entry call counters.
 *
 * WHY A MODULE AND NOT ONE FILE. Once every suffix variant is counted, the driver layer's
 * memory surface is 35 exported names and its launch surface another 16, all in one shared
 * object. The wrappers over those names are mechanical; the two decisions they funnel into are
 * not, and they are the part worth reading. Splitting them keeps each decision in one small
 * file — hggc_quota.c for memory, hggc_compute.c for compute — and leaves the mechanical
 * surface where it can be scanned name by name against the SDK's symbol manifest.
 *
 * NOTHING HERE IS EXPORTED. Every declaration carries VPPU_INTERNAL, so the module's own
 * seam does not become an interposable surface inside the container this library is
 * preloaded into — case 1 asserts that rather than trusting it.
 */
#ifndef VPPU_HGGC_HGGC_QUOTA_H
#define VPPU_HGGC_HGGC_QUOTA_H

#include <stdbool.h>

#include "common/vppu.h"

/* The interposed entries, one per exported ABI name.
 *
 * The list is the SDK's rather than a selection: every name `libhggc.so` exports on the
 * memory path is here, the plain v1 forms and the `_ptsz` variants included, because a
 * workload reaching an uncovered one takes memory this quota never sees. Ordered by what
 * the wrapper DOES — charge, refund, report, count — since that is the only property a
 * reader has to check per name.
 */
enum vppu_entry {
    /* Allocations. Charged against the card's quota before the vendor sees the request. */
    VPPU_MEM_ALLOC = 0,
    VPPU_MEM_ALLOC_V1,
    VPPU_MEM_ALLOC_ASYNC,
    VPPU_MEM_ALLOC_ASYNC_PTSZ,
    VPPU_MEM_ALLOC_FROM_POOL_ASYNC,
    VPPU_MEM_ALLOC_FROM_POOL_ASYNC_PTSZ,
    VPPU_MEM_ALLOC_MANAGED,
    VPPU_MEM_ALLOC_PITCH,
    VPPU_MEM_ALLOC_PITCH_V1,
    VPPU_MEM_CREATE,

    /* Frees. Refunded to whichever card was charged, taken from the key map. */
    VPPU_MEM_FREE,
    VPPU_MEM_FREE_V1,
    VPPU_MEM_FREE_ASYNC,
    VPPU_MEM_FREE_ASYNC_PTSZ,
    VPPU_MEM_RELEASE,

    /* Queries. Answered with the quota's figures, never the card's. */
    VPPU_MEM_GET_INFO,
    VPPU_MEM_GET_INFO_V1,

    /* Host memory. Counted and never charged: pinned host pages are not device VRAM, so
     * charging them would refuse an allocation that costs the card nothing. */
    VPPU_MEM_ALLOC_HOST,
    VPPU_MEM_ALLOC_HOST_V1,
    VPPU_MEM_FREE_HOST,

    /* Address mapping. Counted and never charged: hgMemCreate is where the VMM path takes
     * physical memory, and these only bind a handle that was already paid for. */
    VPPU_MEM_MAP,
    VPPU_MEM_MAP_ARRAY_ASYNC,
    VPPU_MEM_MAP_ARRAY_ASYNC_PTSZ,
    VPPU_MEM_UNMAP,

    /* Pools. Counted and never charged: a pool's memory is taken by
     * hgMemAllocFromPoolAsync, which is charged above, and returned by trimming. They are
     * interposed anyway so the pool path's crossing into libhggc.so is a counted fact
     * rather than an assumption. */
    VPPU_MEM_POOL_CREATE,
    VPPU_MEM_POOL_DESTROY,
    VPPU_MEM_POOL_EXPORT_POINTER,
    VPPU_MEM_POOL_EXPORT_TO_SHAREABLE_HANDLE,
    VPPU_MEM_POOL_GET_ACCESS,
    VPPU_MEM_POOL_GET_ATTRIBUTE,
    VPPU_MEM_POOL_IMPORT_FROM_SHAREABLE_HANDLE,
    VPPU_MEM_POOL_IMPORT_POINTER,
    VPPU_MEM_POOL_SET_ACCESS,
    VPPU_MEM_POOL_SET_ATTRIBUTE,
    VPPU_MEM_POOL_TRIM_TO,

    /* Entry-point resolvers. A caller that asks one of these for an allocation entry walks
     * straight past the interposition of that entry, so they are covered too. */
    VPPU_GET_PROC_ADDRESS,
    VPPU_GET_PROC_ADDRESS_V1,
    VPPU_GET_EXPORT_TABLE,

    /* Launches. Gated against the card's compute window before the vendor sees them: this is
     * where the compute cap is spent, and an uncovered launch entry is a way to spend it
     * unthrottled. Every name libhggc.so exports on the launch path is here for the reason the
     * memory list gives — coverage is the claim, and a name silently missing looks exactly like
     * a name the workload never called. */
    VPPU_LAUNCH_KERNEL,
    VPPU_LAUNCH_KERNEL_PTSZ,
    VPPU_LAUNCH_KERNEL_EX,
    VPPU_LAUNCH_KERNEL_EX_PTSZ,
    VPPU_LAUNCH_KERNEL_EX_AD,
    VPPU_LAUNCH_KERNEL_EX_AD_PTSZ,
    VPPU_LAUNCH_COOPERATIVE_KERNEL,
    VPPU_LAUNCH_COOPERATIVE_KERNEL_PTSZ,
    VPPU_LAUNCH_COOPERATIVE_KERNEL_MULTI_DEVICE,
    VPPU_LAUNCH,
    VPPU_LAUNCH_GRID,
    VPPU_LAUNCH_GRID_ASYNC,

    /* Graph launches. Gated like the rest, and additionally charged a configurable multiple of
     * the window: one of these runs however many kernels were captured into the graph. */
    VPPU_GRAPH_LAUNCH,
    VPPU_GRAPH_LAUNCH_PTSZ,

    /* Host callbacks. Counted and never gated: these run on the CPU, so throttling them would
     * delay a workload without freeing any of the card the cap is about. */
    VPPU_LAUNCH_HOST_FUNC,
    VPPU_LAUNCH_HOST_FUNC_PTSZ,

    VPPU_ENTRY_COUNT
};

/* vppu_hggc_name — the entry's exported ABI name, as dlsym() must be given it. */
VPPU_INTERNAL const char *vppu_hggc_name(enum vppu_entry entry);

/* vppu_hggc_count — record that this entry was called. The dump at exit is what decides
 * "the call reached libhggc.so" by counting rather than by inferring it from linkage. */
VPPU_INTERNAL void vppu_hggc_count(enum vppu_entry entry);

/* vppu_hggc_next — the vendor's definition of this entry, resolved once and cached.
 *
 * NULL means libhggc.so is not behind this library in the search order. Every wrapper must
 * treat that as a failure rather than as a silent no-op: returning success without calling
 * anything would hand the caller an unwritten out-parameter. */
VPPU_INTERNAL void *vppu_hggc_next(enum vppu_entry entry);

/* vppu_hggc_self — this library's own definition of this entry, resolved once and cached.
 * What the resolvers hand out in place of the vendor's address. */
VPPU_INTERNAL void *vppu_hggc_self(enum vppu_entry entry);

/* vppu_hggc_admit — decide one allocation against its card's quota and, on success, leave
 * the card LOCKED with the bytes already charged. The caller must then call exactly one of
 * vppu_hggc_commit(), vppu_hggc_commit_sized() or vppu_hggc_rollback().
 *
 * Fail-closed in every failure mode, and false is always a refusal the caller must return:
 * this library is only preloaded into containers that are being sliced, so an unresolvable
 * device, an unusable configuration, a card with no figure and an unreachable ledger are
 * each a misconfiguration rather than a reason to let the allocation through. */
VPPU_INTERNAL bool vppu_hggc_admit(enum vppu_entry entry, unsigned long long bytes,
                                   int *device_out);

/* vppu_hggc_admit_on — vppu_hggc_admit() for an entry that NAMES its card instead of inheriting
 * the calling thread's context. The VMM path carries `prop->location.id` and a pool allocation
 * belongs to the card its pool was created on, and neither has to be the current context: charging
 * those to the context spends one card's quota on another card's memory, which a container holding
 * a single card can never notice and a container holding two is broken by.
 *
 * `device` below 0 means "no card was named", so a caller may pass what it found and let this
 * decide; the answer is then the context, exactly as before. A card named ABOVE the layout's bound
 * is refused rather than folded back onto the context — it arrives from the caller's own struct,
 * and quietly charging a different card than the one asked for is the bug this exists to fix. */
VPPU_INTERNAL bool vppu_hggc_admit_on(int device, enum vppu_entry entry, unsigned long long bytes,
                                      int *device_out);

/* vppu_hggc_commit — the vendor's allocation succeeded: remember what this handle owes and
 * release the card. */
VPPU_INTERNAL void vppu_hggc_commit(int device, unsigned long long key,
                                    unsigned long long bytes);

/* vppu_hggc_commit_sized — commit an allocation whose true size was only knowable after the
 * call, reconciling the ledger to it. For the pitched entries, where the driver picks the
 * row stride: admission can only be decided on the caller's width, and the charge has to
 * end up on the stride the driver actually returned. */
VPPU_INTERNAL void vppu_hggc_commit_sized(int device, unsigned long long key,
                                          unsigned long long admitted,
                                          unsigned long long actual);

/* vppu_hggc_rollback — the vendor's allocation did not happen: give the provisional charge
 * back and release the card. */
VPPU_INTERNAL void vppu_hggc_rollback(int device, unsigned long long bytes);

/* vppu_hggc_refund — give a freed handle's bytes back to the card that was charged for
 * them. A key that was never recorded is nothing to refund, not an error: an allocation
 * made before this library loaded is exactly that case. */
VPPU_INTERNAL void vppu_hggc_refund(unsigned long long key);

/* vppu_hggc_device — the card the calling thread's context sits on, or -1.
 *
 * Both quotas need it and neither is handed it: no allocation entry and no launch entry takes a
 * device argument, so the only way to know which card an operation belongs to is to ask the
 * current context. -1 is a refusal, never "no quota". */
VPPU_INTERNAL int vppu_hggc_device(void);

/* vppu_hggc_gate — hold a launch until the calling thread's card admits it, and report whether
 * it may proceed at all.
 *
 * A false answer is a refusal the caller must return: an unusable configuration, no current
 * context, or an unreachable ledger are each a misconfiguration rather than a reason to let a
 * workload take a card it was capped on. THROTTLING IS NOT A REFUSAL — a launch that has to
 * wait for its share of the card waits inside this call and then answers true, because refusing
 * it would break a working workload rather than slow it down.
 *
 * `graph` marks the launch as a captured graph, which is charged a configurable multiple of the
 * window because it runs however many kernels were captured into it. */
VPPU_INTERNAL bool vppu_hggc_gate(enum vppu_entry entry, bool graph);

/* vppu_hggc_view — the figures a memory query must report for the calling thread's card.
 *
 * False means there is nothing to substitute — no current device, or no quota configured
 * for it — and the caller must pass the vendor's own answer through unchanged. A query is
 * not an allocation: refusing to say how much memory exists would break a workload that
 * never allocates past its quota at all. */
VPPU_INTERNAL bool vppu_hggc_view(unsigned long long *quota_out,
                                  unsigned long long *free_out);

#endif /* VPPU_HGGC_HGGC_QUOTA_H */
