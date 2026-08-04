/*
 * hggc_quota.c — the enforcement half of libvppu.so: a per-card driver-layer VRAM quota, and
 * the entry table both quotas resolve and count themselves through.
 *
 * Product code, injected into sliced workload containers. This file holds the memory admission
 * decision; the compute one is in hggc_compute.c, and the interposed entries themselves are in
 * hggc_mem.c, hggc_mem_v1.c, hggc_entry.c and hggc_launch.c.
 *
 * Distinct from the hgml shim, and it has to be: that one makes a quota VISIBLE to ppu-smi
 * through HGML, which can neither enforce nor observe an allocation. This one interposes the
 * driver layer, where allocations actually happen.
 *
 * It needs no dlsym hook. libhggcrt (the runtime layer the workload calls) lists libhggc.so
 * in DT_NEEDED and reaches it through the PLT, so an LD_PRELOADed definition wins by plain
 * symbol interposition. HGML needed the dlsym hook only because ppu-smi dlopen()s it and
 * resolves on the explicit handle.
 *
 * The interposed names are the DRIVER-layer ones. That is the measurement, not an oversight:
 * the workload calls hggcMalloc, which lives in libhggcrt.13.0.so, and the counters here are
 * what prove that call funnels down into libhggc.so. Interposing hggcMalloc itself would
 * answer nothing.
 *
 * The quota is PER CARD, keyed by the device an allocation actually lands on. That is not a
 * naming detail: none of the allocation entries takes a device argument — they charge whatever
 * device the calling thread's current context sits on — so the only way to know which card to
 * charge is to ask, which is what vppu_hggc_device() does. A container holding several cards
 * gets one figure per card from the allocator, exactly as the NVIDIA branch injects
 * CUDA_DEVICE_MEMORY_LIMIT_<i>, and a single container-wide total could not express that.
 *
 * The accounting itself lives in common/'s cross-process region, and the card's lock is held
 * from the admission decision until the vendor's allocation returns. Both matter: a ledger
 * private to one process grants every process in the container the whole figure, and a lock
 * released before the allocation returns lets two callers pass the same check and exceed the
 * quota together. The bytes are charged BEFORE the real call and refunded if it fails, so
 * there is no window in which memory is held without being accounted for.
 *
 * Two outputs exist so the memory-path gate cannot pass for the wrong reason:
 *   - a per-entry CALL COUNTER, dumped at exit, so "the call crossed libhggc.so" is decided
 *     by counting a call rather than inferred from link or symbol evidence;
 *   - an explicit DENIED marker on refusal, so a refusal by this quota is distinguishable
 *     from a failure for any other reason.
 * Both are diagnostics rather than steady-state output, so they sit at log level 2 while
 * denials stay at level 1 — see common/vppu.h.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <stdio.h>

/* hggc.h includes only <stdlib.h> and <stdint.h>, so it supplies neither NULL nor bool for
 * its own declarations; pull both in ahead of it, as hgml.h's consumers must. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>

#include "common/vppu_ledger.h"
#include "common/vppu_quota.h"
#include "hggc/hggc_quota.h"

/* The exported ABI names, not the source-level ones: hggc.h maps hgMemAlloc onto
 * hgMemAlloc_v2 the way cuda.h maps cuMemAlloc, and dlsym() has to be given what the library
 * actually exports. The plain forms below are the v1 ABI, which libhggc.so exports too. */
static const char *const entry_names[] = {
    "hgMemAlloc_v2",
    "hgMemAlloc",
    "hgMemAllocAsync",
    "hgMemAllocAsync_ptsz",
    "hgMemAllocFromPoolAsync",
    "hgMemAllocFromPoolAsync_ptsz",
    "hgMemAllocManaged",
    "hgMemAllocPitch_v2",
    "hgMemAllocPitch",
    "hgMemCreate",

    "hgMemFree_v2",
    "hgMemFree",
    "hgMemFreeAsync",
    "hgMemFreeAsync_ptsz",
    "hgMemRelease",

    "hgMemGetInfo_v2",
    "hgMemGetInfo",

    "hgMemAllocHost_v2",
    "hgMemAllocHost",
    "hgMemFreeHost",

    "hgMemMap",
    "hgMemMapArrayAsync",
    "hgMemMapArrayAsync_ptsz",
    "hgMemUnmap",

    "hgMemPoolCreate",
    "hgMemPoolDestroy",
    "hgMemPoolExportPointer",
    "hgMemPoolExportToShareableHandle",
    "hgMemPoolGetAccess",
    "hgMemPoolGetAttribute",
    "hgMemPoolImportFromShareableHandle",
    "hgMemPoolImportPointer",
    "hgMemPoolSetAccess",
    "hgMemPoolSetAttribute",
    "hgMemPoolTrimTo",

    "hgGetProcAddress_v2",
    "hgGetProcAddress",
    "hgGetExportTable",

    "hgLaunchKernel",
    "hgLaunchKernel_ptsz",
    "hgLaunchKernelEx",
    "hgLaunchKernelEx_ptsz",
    "hgLaunchKernelExAD",
    "hgLaunchKernelExAD_ptsz",
    "hgLaunchCooperativeKernel",
    "hgLaunchCooperativeKernel_ptsz",
    "hgLaunchCooperativeKernelMultiDevice",
    "hgLaunch",
    "hgLaunchGrid",
    "hgLaunchGridAsync",

    "hgGraphLaunch",
    "hgGraphLaunch_ptsz",

    "hgLaunchHostFunc",
    "hgLaunchHostFunc_ptsz",
};

/* The one guard that keeps the enum and this table from drifting apart. Without it a new
 * entry appended to only one of them shifts every name after it, and the wrappers would
 * silently count and resolve the wrong symbol. */
_Static_assert(sizeof(entry_names) / sizeof(*entry_names) == VPPU_ENTRY_COUNT,
               "every interposed entry needs its exported ABI name");

static unsigned long long entry_calls[VPPU_ENTRY_COUNT];

/* The vendor's definition of each entry, resolved on first use. Cached because a dlsym() per
 * allocation is a cost the hot path does not need, and because the resolver entries compare
 * against every one of these on a single call. Two threads racing here resolve the same
 * address twice and store the same value; `resolved` is set afterwards so a racing reader
 * never sees a NULL that is merely not filled in yet. */
static void *next_fn[VPPU_ENTRY_COUNT];
static bool next_resolved[VPPU_ENTRY_COUNT];

/* This library's own definition of each entry, cached the same way. Only the resolver entries
 * need it, and they need it for every symbol a caller asks about — a binding pass over a few
 * hundred names would otherwise repeat the whole lookup table per name. */
static void *self_fn[VPPU_ENTRY_COUNT];
static bool self_resolved[VPPU_ENTRY_COUNT];

/* Whether this process ever reached the ledger. The counter dump asks the ledger for its
 * totals, and asking maps the region — a process that allocated nothing (every process in the
 * container carries this preload) must not create the file on its way out. */
static bool ledger_touched;

const char *vppu_hggc_name(enum vppu_entry entry)
{
    return entry_names[entry];
}

void vppu_hggc_count(enum vppu_entry entry)
{
    entry_calls[entry]++;
}

void *vppu_hggc_next(enum vppu_entry entry)
{
    if (!next_resolved[entry]) {
        void *fn = dlsym(RTLD_NEXT, entry_names[entry]);

        if (fn == NULL) {
            vppu_log(VPPU_LOG_DENY, "no next %s — libhggc.so not in the search order?\n",
                     entry_names[entry]);
        }
        next_fn[entry] = fn;
        next_resolved[entry] = true;
    }
    return next_fn[entry];
}

void *vppu_hggc_self(enum vppu_entry entry)
{
    if (!self_resolved[entry]) {
        /* RTLD_DEFAULT, not the address of the function: this file cannot name the definitions,
         * they live in the other translation units, and a table of addresses would be one more
         * thing to keep in step with the enum. The global search order answers it instead —
         * this library is preloaded, so it comes first and the answer is its own definition,
         * the very one a caller reaching the symbol through the linker would get. */
        self_fn[entry] = dlsym(RTLD_DEFAULT, entry_names[entry]);
        self_resolved[entry] = true;
    }
    return self_fn[entry];
}

/* vppu_hggc_device — which card the calling thread's work is about to land on.
 *
 * hgCtxGetDevice reports the device of the calling thread's current context, which is what
 * the allocation entries charge against and the launch entries spend compute on; none of them
 * takes a device argument. Note the plain name is the ABI name here: hggc.h maps hgMemAlloc to
 * hgMemAlloc_v2 but leaves hgCtxGetDevice alone, and hgCtxGetDevice_v2 is a different function
 * taking an explicit context. Returns -1 when there is no current context or the answer is out
 * of range, and the caller treats that as a refusal rather than as "no quota". */
int vppu_hggc_device(void)
{
    static HGresult (*real_get_device)(HGdevice *);
    static bool resolved;

    if (!resolved) {
        real_get_device = dlsym(RTLD_NEXT, "hgCtxGetDevice");
        resolved = true;
    }
    if (real_get_device == NULL) {
        return -1;
    }

    HGdevice device = 0;
    if (real_get_device(&device) != HGGC_SUCCESS) {
        return -1;
    }
    if (device < 0 || device >= VPPU_MAX_DEVICES) {
        return -1;
    }
    return (int)device;
}

/* remaining — what is left of a card's quota.
 *
 * A remainder rather than `used + bytes > quota`: that sum wraps on a large enough request,
 * lands below the quota, and is admitted with no DENIED marker — the one outcome the gate
 * reads as "the quota does not apply to this path". */
static unsigned long long remaining(unsigned long long quota, unsigned long long used)
{
    return (used >= quota) ? 0ULL : quota - used;
}

static bool admit_on(int named, enum vppu_entry entry, unsigned long long bytes, int *device_out)
{
    /* A named card is bounded HERE because it arrives out of the caller's own struct, and the
     * ledger indexes its per-card lock arena by this value. */
    if (named >= VPPU_MAX_DEVICES) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s request=%llu: card %d is beyond this build's %u\n",
                 entry_names[entry], bytes, named, (unsigned int)VPPU_MAX_DEVICES);
        return false;
    }

    /* Resolved before the lock is taken: this calls into the vendor, and the ledger's
     * re-entrancy counting exists to survive a vendor call made under our lock, not to invite
     * one. */
    int device = (named >= 0) ? named : vppu_hggc_device();
    if (device < 0) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s request=%llu: no current device\n",
                 entry_names[entry], bytes);
        return false;
    }

    if (!vppu_quota_usable()) {
        vppu_log(VPPU_LOG_DENY,
                 "DENIED %s request=%llu device=%d: the container's quota configuration is "
                 "unusable — see the load-time report\n",
                 entry_names[entry], bytes, device);
        return false;
    }

    unsigned long long quota = vppu_quota_memory_bytes(device);
    if (quota == 0ULL) {
        /* Both variables are named because either could have carried this card: the refusal is
         * the same whether the indexed figure is missing, the un-indexed one it would have fallen
         * back to is missing, or the indexed one is set to something unusable. */
        vppu_log(VPPU_LOG_DENY,
                 "DENIED %s request=%llu device=%d: no usable %s%d and no usable %s\n",
                 entry_names[entry], bytes, device, VPPU_ENV_MEMORY_LIMIT_PREFIX, device,
                 VPPU_ENV_MEMORY_LIMIT);
        return false;
    }

    if (!vppu_ledger_lock(device)) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s request=%llu device=%d: the ledger is unavailable\n",
                 entry_names[entry], bytes, device);
        return false;
    }
    ledger_touched = true;
    vppu_ledger_note_config(device, quota, vppu_quota_sm_percent(device));

    unsigned long long used = vppu_ledger_used(device);
    if (bytes > remaining(quota, used)) {
        /* One sweep, and only on the path that would otherwise refuse: a process killed
         * mid-allocation leaves its charge in the region, and that must not shrink the
         * container's quota for the life of the ledger file. */
        if (vppu_ledger_reclaim(device) > 0ULL) {
            used = vppu_ledger_used(device);
        }
    }
    if (bytes > remaining(quota, used)) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s device=%d request=%llu accounted=%llu quota=%llu\n",
                 entry_names[entry], device, bytes, used, quota);
        vppu_ledger_unlock(device);
        return false;
    }

    /* Charged before the vendor sees the request, so no window exists in which the memory is
     * held and unaccounted. A refused charge means the card's process table is full, which is
     * a refusal too: an allocation nobody is charged for can never be reclaimed either. */
    if (!vppu_ledger_charge(device, bytes)) {
        vppu_log(VPPU_LOG_DENY,
                 "DENIED %s device=%d request=%llu: the ledger cannot account for it\n",
                 entry_names[entry], device, bytes);
        vppu_ledger_unlock(device);
        return false;
    }

    *device_out = device;
    return true;
}

bool vppu_hggc_admit(enum vppu_entry entry, unsigned long long bytes, int *device_out)
{
    return admit_on(-1, entry, bytes, device_out);
}

bool vppu_hggc_admit_on(int device, enum vppu_entry entry, unsigned long long bytes,
                        int *device_out)
{
    return admit_on(device, entry, bytes, device_out);
}

void vppu_hggc_commit(int device, unsigned long long key, unsigned long long bytes)
{
    vppu_alloc_record(key, device, bytes);
    vppu_ledger_unlock(device);
}

void vppu_hggc_commit_sized(int device, unsigned long long key, unsigned long long admitted,
                            unsigned long long actual)
{
    /* Still under the card's lock, so the difference lands before any other process can read
     * the total. The charge is corrected rather than re-admitted: the memory is already the
     * caller's, and freeing it behind their back to hold a figure exactly would break a
     * working allocation over padding the driver chose. A stride that overruns the quota is
     * therefore reported instead of refused. */
    if (actual > admitted) {
        unsigned long long extra = actual - admitted;

        if (!vppu_ledger_charge(device, extra)) {
            vppu_log(VPPU_LOG_DENY,
                     "device %d: %llu bytes of row padding not accounted, the ledger is "
                     "full\n",
                     device, extra);
        }
    } else if (admitted > actual) {
        vppu_ledger_refund(device, admitted - actual);
    }

    vppu_alloc_record(key, device, actual);
    vppu_ledger_unlock(device);
}

void vppu_hggc_rollback(int device, unsigned long long bytes)
{
    vppu_ledger_refund(device, bytes);
    vppu_ledger_unlock(device);
}

void vppu_hggc_refund(unsigned long long key)
{
    int device = -1;
    unsigned long long bytes = 0ULL;

    /* Taken from the key map rather than from the current context: a free may well run with a
     * different current device than the allocation did. */
    if (!vppu_alloc_take(key, &device, &bytes)) {
        return;
    }
    if (!vppu_ledger_lock(device)) {
        /* Put the record back rather than lose it: the ledger being unreachable is a state
         * that denies every allocation anyway, and a record kept is a refund that can still
         * land if it recovers. */
        vppu_alloc_record(key, device, bytes);
        vppu_log(VPPU_LOG_DENY, "device %d: %llu freed bytes not refunded, the ledger is "
                                "unavailable\n",
                 device, bytes);
        return;
    }
    vppu_ledger_refund(device, bytes);
    vppu_ledger_unlock(device);
}

bool vppu_hggc_view(unsigned long long *quota_out, unsigned long long *free_out)
{
    int device = vppu_hggc_device();
    if (device < 0) {
        return false;
    }

    unsigned long long quota = vppu_quota_memory_bytes(device);
    if (quota == 0ULL) {
        return false;
    }

    /* No lock. A figure one allocation stale is worth far more than a query that can block
     * behind a vendor allocation that has hung. */
    *quota_out = quota;
    *free_out = remaining(quota, vppu_ledger_used(device));
    return true;
}

__attribute__((constructor)) static void announce(void)
{
    /* Reports the container's configuration once and latches whether it is usable, so a
     * misconfiguration is diagnosed at load rather than at the first allocation. Called from
     * here rather than from a constructor in common/ so the order stays explicit. */
    vppu_quota_validate();

    vppu_log(VPPU_LOG_DEBUG, "hggc_quota loaded, per-card %s<i>, %d entries\n",
             VPPU_ENV_MEMORY_LIMIT_PREFIX, (int)VPPU_ENTRY_COUNT);
}

/* The counter dump is the evidence that a runtime-layer call reached the driver layer, so it
 * must survive a normal exit: printed from a destructor, every entry named even at zero — a
 * missing name is indistinguishable from a zero count. Wrapped across lines with the marker
 * repeated on each, because one line of 38 entries is unreadable and the cases match the
 * marker per line. */
__attribute__((destructor)) static void dump_counters(void)
{
    if (vppu_log_level() < VPPU_LOG_DEBUG) {
        return;
    }

    for (int i = 0; i < VPPU_ENTRY_COUNT; i++) {
        if (i % 6 == 0) {
            fprintf(stderr, "%s" VPPU_TAG "hggc_quota counters:", (i == 0) ? "" : "\n");
        }
        fprintf(stderr, " %s=%llu", entry_names[i], entry_calls[i]);
    }

    /* The totals are the container's, not this process's, and they come from the ledger —
     * asked for only if this process ever reached it, so a process that allocated nothing does
     * not create the region file on its way out. Only cards carrying a charge are named, so an
     * idle table does not print 64 zeroes. */
    if (ledger_touched) {
        for (int i = 0; i < VPPU_MAX_DEVICES; i++) {
            unsigned long long used = vppu_ledger_used(i);

            if (used != 0ULL) {
                fprintf(stderr, " accounted[%d]=%llu", i, used);
            }
        }
    }
    fprintf(stderr, " ledger_overflows=%llu\n", vppu_alloc_overflows());
}
