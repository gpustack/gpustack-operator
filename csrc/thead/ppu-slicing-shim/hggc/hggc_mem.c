/*
 * hggc_mem.c — the driver layer's memory entries, current ABI.
 *
 * One definition per exported name in libhggc.so's memory surface, as the SDK's symbol
 * manifest lists it. Every name is here whether or not it is charged, because coverage is the
 * claim being made: a name left out is a way for a workload to take memory this quota never
 * sees, and there is no way to tell the two apart from the outside.
 *
 * WHAT A WRAPPER DOES is one of four things, and the enum in hggc_quota.h groups the names by
 * which:
 *   - charge: admit the request against the card's quota, call the vendor, commit or roll back
 *   - refund: call the vendor, then return the handle's bytes to whichever card was charged
 *   - report: answer with the quota's figures instead of the card's
 *   - count: call the vendor unchanged, having counted the crossing
 *
 * THE SIGNATURES ARE THE HEADER'S, NOT COPIES. hggc.h maps the plain source name onto the
 * versioned symbol (hgMemAlloc -> hgMemAlloc_v2, as cuda.h maps cuMemAlloc), so writing the
 * plain name here defines the versioned symbol AND has the header type-check every parameter.
 * The `_ptsz` variants get the same treatment through __typeof__ rather than a retyped
 * prototype: the header only maps the plain name onto the `_ptsz` symbol under
 * __HGGC_API_PER_THREAD_DEFAULT_STREAM, which this library must not define — that would move
 * every stream entry at once — so the type is taken from the plain declaration instead. A
 * variant whose parameters ever diverge then fails the build rather than corrupting a call.
 *
 * The v1 ABI names are in hggc_mem_v1.c, which has to #undef those mappings to reach them.
 */
#define _GNU_SOURCE

#include <limits.h>

/* hggc.h includes only <stdlib.h> and <stdint.h>, so it supplies neither NULL nor bool for
 * its own declarations. */
#include <dlfcn.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include <hggc.h>

#include "common/vppu.h"
#include "common/vppu_quota.h"
#include "hggc/hggc_quota.h"

/* product — a * b, refusing rather than wrapping.
 *
 * The pitched entries are the only ones handed two dimensions instead of a byte count, and a
 * product that wraps lands below the quota and is admitted. Refusing is the safe answer: a
 * request whose size does not fit a 64-bit count is not one the card could serve either. */
static bool product(unsigned long long a, unsigned long long b, unsigned long long *out)
{
    if (a != 0ULL && b > ULLONG_MAX / a) {
        return false;
    }
    *out = a * b;
    return true;
}

/* The stream variants, typed from the plain entries they mirror. */
extern __typeof__(hgMemAllocAsync) hgMemAllocAsync_ptsz;
extern __typeof__(hgMemAllocFromPoolAsync) hgMemAllocFromPoolAsync_ptsz;
extern __typeof__(hgMemFreeAsync) hgMemFreeAsync_ptsz;
extern __typeof__(hgMemMapArrayAsync) hgMemMapArrayAsync_ptsz;

/* named_card — the card a location names, or -1 when it names none. Only a DEVICE location names
 * one: host and NUMA locations are host memory, which this quota does not govern. */
static int named_card(const HGmemLocation *location)
{
    if (location == NULL || location->type != HG_MEM_LOCATION_TYPE_DEVICE) {
        return -1;
    }
    return location->id;
}

/* The pool -> card table, which has to sit above the allocation that reads it. A pool belongs to
 * the card named in its props and an allocation out of it lands there whatever the calling thread's
 * context is, so the card is remembered at creation — the handle carries no way to ask afterwards.
 *
 * Small, fixed and lock-free: a pool per card per purpose is the shape real workloads have, a table
 * that cannot grow cannot fail inside an interposed vendor call, and a compare-and-exchange on the
 * handle needs no lock on the glibc floor this library links against. A pool that does not fit, or
 * one created before this library loaded, is simply absent — and absent means "charge the calling
 * thread's context", which is the answer this path gave before the table existed. */
#define VPPU_MAX_POOLS 64

static struct {
    HGmemoryPool pool;
    int device;
} pool_cards[VPPU_MAX_POOLS];

/* A handle value no vendor pool can have, held in a slot between claiming it and filling it in.
 * Without it two threads filing different pools collide on the same empty slot: both find it empty,
 * both write their card, and whichever wins the exchange publishes its handle against the other's
 * card — an allocation then charged to a card it never touched, which is the whole bug this table
 * exists to prevent. */
#define VPPU_POOL_CLAIMING ((HGmemoryPool)(uintptr_t)1)

/* pool_remember — file a pool's card. The slot is CLAIMED first and the real handle published
 * LAST, so a reader either does not match the slot at all or sees the card that goes with it. */
static void pool_remember(HGmemoryPool pool, int device)
{
    if (pool == NULL || device < 0) {
        return;
    }
    for (unsigned int i = 0; i < VPPU_MAX_POOLS; i++) {
        HGmemoryPool empty = NULL;

        if (!__atomic_compare_exchange_n(&pool_cards[i].pool, &empty, VPPU_POOL_CLAIMING, false,
                                         __ATOMIC_ACQUIRE, __ATOMIC_RELAXED)) {
            continue;
        }
        pool_cards[i].device = device;
        __atomic_store_n(&pool_cards[i].pool, pool, __ATOMIC_RELEASE);
        return;
    }
}

/* pool_forget — a destroyed pool's slot goes back, so a long-lived process that cycles pools does
 * not fill the table with handles the vendor may reuse for a different card. */
static void pool_forget(HGmemoryPool pool)
{
    for (unsigned int i = 0; i < VPPU_MAX_POOLS; i++) {
        if (__atomic_load_n(&pool_cards[i].pool, __ATOMIC_ACQUIRE) == pool) {
            __atomic_store_n(&pool_cards[i].pool, NULL, __ATOMIC_RELEASE);
            return;
        }
    }
}

/* default_pool_card — which held card's DEFAULT pool this handle is, by asking the vendor for each
 * of them and comparing. The default pool is the one a workload gets without creating anything, so
 * it is the common case, and it never passes through hgMemPoolCreate — leaving it to fall back to
 * the calling thread's context would half-fix the very bypass this table exists to close.
 *
 * Asked rather than interposed on purpose: hgDeviceGetDefaultMemPool is a device entry, and adding
 * it to the interposed set would widen the module's exported surface for a fact that can simply be
 * queried. This is a caller of the vendor, the same shape as resolving the current context, and it
 * runs BEFORE the card's lock is taken. Only cards with a configured figure are asked: those are
 * the ones the container holds, and an allocation on any other is refused whatever its pool says.
 */
static int default_pool_card(HGmemoryPool pool)
{
    static HGresult (*real_default_pool)(HGmemoryPool *, HGdevice);
    static bool resolved;

    if (!resolved) {
        real_default_pool = dlsym(RTLD_NEXT, "hgDeviceGetDefaultMemPool");
        resolved = true;
    }
    if (real_default_pool == NULL) {
        return -1;
    }

    for (int device = 0; device < VPPU_MAX_DEVICES; device++) {
        HGmemoryPool theirs = NULL;

        if (vppu_quota_memory_bytes(device) == 0ULL) {
            continue;
        }
        if (real_default_pool(&theirs, (HGdevice)device) == HGGC_SUCCESS && theirs == pool) {
            return device;
        }
    }
    return -1;
}

/* pool_card — the card a pool belongs to, or -1 when neither the table nor the vendor can say. */
static int pool_card(HGmemoryPool pool)
{
    if (pool == NULL) {
        return -1;
    }
    for (unsigned int i = 0; i < VPPU_MAX_POOLS; i++) {
        if (__atomic_load_n(&pool_cards[i].pool, __ATOMIC_ACQUIRE) == pool) {
            return pool_cards[i].device;
        }
    }

    /* Cached on the way out, so the vendor is asked once per pool rather than once per allocation. */
    int device = default_pool_card(pool);
    if (device >= 0) {
        pool_remember(pool, device);
    }
    return device;
}

/* ---------------------------------------------------------------------------------------
 * Allocations. Charged before the vendor sees the request; the charge is given back if the
 * allocation does not happen, so no window exists in which memory is held unaccounted.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemAlloc(HGdeviceptr *dptr, size_t bytesize)
{
    int device = -1;

    vppu_hggc_count(VPPU_MEM_ALLOC);
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr *, size_t) = vppu_hggc_next(VPPU_MEM_ALLOC);
    if (real == NULL) {
        vppu_hggc_rollback(device, bytesize);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, bytesize);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        vppu_hggc_commit(device, (unsigned long long)*dptr, bytesize);
    } else {
        vppu_hggc_rollback(device, bytesize);
    }
    return rc;
}

/* alloc_async / alloc_from_pool — the bodies the plain and _ptsz variants share. The two
 * differ only in which default stream the driver substitutes, which is the vendor's business;
 * the accounting is identical, so it is written once and the entry tells the counters and the
 * dlsym() apart. */
static HGresult alloc_async(enum vppu_entry entry, HGdeviceptr *dptr, size_t bytesize,
                            HGstream hStream)
{
    int device = -1;

    vppu_hggc_count(entry);
    if (!vppu_hggc_admit(entry, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr *, size_t, HGstream) = vppu_hggc_next(entry);
    if (real == NULL) {
        vppu_hggc_rollback(device, bytesize);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, bytesize, hStream);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        vppu_hggc_commit(device, (unsigned long long)*dptr, bytesize);
    } else {
        vppu_hggc_rollback(device, bytesize);
    }
    return rc;
}

HGresult HGGCAPI hgMemAllocAsync(HGdeviceptr *dptr, size_t bytesize, HGstream hStream)
{
    return alloc_async(VPPU_MEM_ALLOC_ASYNC, dptr, bytesize, hStream);
}

HGresult HGGCAPI hgMemAllocAsync_ptsz(HGdeviceptr *dptr, size_t bytesize, HGstream hStream)
{
    return alloc_async(VPPU_MEM_ALLOC_ASYNC_PTSZ, dptr, bytesize, hStream);
}

static HGresult alloc_from_pool(enum vppu_entry entry, HGdeviceptr *dptr, size_t bytesize,
                                HGmemoryPool pool, HGstream hStream)
{
    int device = -1;

    vppu_hggc_count(entry);
    /* The pool's card, not the caller's context: they are the same in a container holding one
     * card and need not be in one holding two. */
    if (!vppu_hggc_admit_on(pool_card(pool), entry, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr *, size_t, HGmemoryPool, HGstream) = vppu_hggc_next(entry);
    if (real == NULL) {
        vppu_hggc_rollback(device, bytesize);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, bytesize, pool, hStream);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        vppu_hggc_commit(device, (unsigned long long)*dptr, bytesize);
    } else {
        vppu_hggc_rollback(device, bytesize);
    }
    return rc;
}

HGresult HGGCAPI hgMemAllocFromPoolAsync(HGdeviceptr *dptr, size_t bytesize, HGmemoryPool pool,
                                         HGstream hStream)
{
    return alloc_from_pool(VPPU_MEM_ALLOC_FROM_POOL_ASYNC, dptr, bytesize, pool, hStream);
}

HGresult HGGCAPI hgMemAllocFromPoolAsync_ptsz(HGdeviceptr *dptr, size_t bytesize,
                                              HGmemoryPool pool, HGstream hStream)
{
    return alloc_from_pool(VPPU_MEM_ALLOC_FROM_POOL_ASYNC_PTSZ, dptr, bytesize, pool, hStream);
}

/* Managed memory is charged like any other allocation: it is backed by the card even though
 * the driver may migrate pages, so a container that could take it uncharged would hold device
 * memory outside its quota. */
HGresult HGGCAPI hgMemAllocManaged(HGdeviceptr *dptr, size_t bytesize, unsigned int flags)
{
    int device = -1;

    vppu_hggc_count(VPPU_MEM_ALLOC_MANAGED);
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC_MANAGED, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr *, size_t, unsigned int) =
        vppu_hggc_next(VPPU_MEM_ALLOC_MANAGED);
    if (real == NULL) {
        vppu_hggc_rollback(device, bytesize);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, bytesize, flags);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        vppu_hggc_commit(device, (unsigned long long)*dptr, bytesize);
    } else {
        vppu_hggc_rollback(device, bytesize);
    }
    return rc;
}

/* The pitched entry is the one allocation whose real size is not known until it returns: the
 * driver picks the row stride, and what it takes from the card is stride x height, not the
 * width the caller asked for. Admission is therefore decided on the caller's figure and the
 * charge reconciled to the driver's — see vppu_hggc_commit_sized(). */
HGresult HGGCAPI hgMemAllocPitch(HGdeviceptr *dptr, size_t *pPitch, size_t WidthInBytes,
                                 size_t Height, unsigned int ElementSizeBytes)
{
    int device = -1;
    unsigned long long request = 0ULL;

    vppu_hggc_count(VPPU_MEM_ALLOC_PITCH);
    if (!product(WidthInBytes, Height, &request)) {
        vppu_log(VPPU_LOG_DENY, "DENIED %s: %zu x %zu overflows a byte count\n",
                 vppu_hggc_name(VPPU_MEM_ALLOC_PITCH), WidthInBytes, Height);
        return HGGC_ERROR_INVALID_VALUE;
    }
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC_PITCH, request, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr *, size_t *, size_t, size_t, unsigned int) =
        vppu_hggc_next(VPPU_MEM_ALLOC_PITCH);
    if (real == NULL) {
        vppu_hggc_rollback(device, request);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        unsigned long long actual = request;

        /* A stride that does not multiply out leaves the admitted figure charged rather than
         * an arbitrary one: the allocation succeeded, so the driver's own product cannot
         * overflow, and guessing would be worse than keeping what was decided. */
        if (pPitch != NULL) {
            (void)product(*pPitch, Height, &actual);
        }
        vppu_hggc_commit_sized(device, (unsigned long long)*dptr, request, actual);
    } else {
        vppu_hggc_rollback(device, request);
    }
    return rc;
}

/* hgMemCreate is where the VMM path takes physical memory. The handle it returns is what a
 * later hgMemRelease gives back, so the handle is the key. */
HGresult HGGCAPI hgMemCreate(HGmemGenericAllocationHandle *handle, size_t size,
                             const HGmemAllocationProp *prop, unsigned long long flags)
{
    int device = -1;

    vppu_hggc_count(VPPU_MEM_CREATE);
    /* The VMM path names its target card in the prop, and it need not be the calling thread's:
     * charging the context would take the memory from one card and the quota from another. */
    if (!vppu_hggc_admit_on(named_card(prop == NULL ? NULL : &prop->location), VPPU_MEM_CREATE,
                            size, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGmemGenericAllocationHandle *, size_t, const HGmemAllocationProp *,
                     unsigned long long) = vppu_hggc_next(VPPU_MEM_CREATE);
    if (real == NULL) {
        vppu_hggc_rollback(device, size);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(handle, size, prop, flags);
    if (rc == HGGC_SUCCESS && handle != NULL) {
        vppu_hggc_commit(device, (unsigned long long)*handle, size);
    } else {
        vppu_hggc_rollback(device, size);
    }
    return rc;
}

/* ---------------------------------------------------------------------------------------
 * Frees. Refunded only when the vendor reports the memory gone, and only for a key this
 * library recorded — anything else is an allocation made before it loaded.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemFree(HGdeviceptr dptr)
{
    vppu_hggc_count(VPPU_MEM_FREE);

    HGresult (*real)(HGdeviceptr) = vppu_hggc_next(VPPU_MEM_FREE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr);
    if (rc == HGGC_SUCCESS) {
        vppu_hggc_refund((unsigned long long)dptr);
    }
    return rc;
}

static HGresult free_async(enum vppu_entry entry, HGdeviceptr dptr, HGstream hStream)
{
    vppu_hggc_count(entry);

    HGresult (*real)(HGdeviceptr, HGstream) = vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, hStream);
    if (rc == HGGC_SUCCESS) {
        vppu_hggc_refund((unsigned long long)dptr);
    }
    return rc;
}

HGresult HGGCAPI hgMemFreeAsync(HGdeviceptr dptr, HGstream hStream)
{
    return free_async(VPPU_MEM_FREE_ASYNC, dptr, hStream);
}

HGresult HGGCAPI hgMemFreeAsync_ptsz(HGdeviceptr dptr, HGstream hStream)
{
    return free_async(VPPU_MEM_FREE_ASYNC_PTSZ, dptr, hStream);
}

HGresult HGGCAPI hgMemRelease(HGmemGenericAllocationHandle handle)
{
    vppu_hggc_count(VPPU_MEM_RELEASE);

    HGresult (*real)(HGmemGenericAllocationHandle) = vppu_hggc_next(VPPU_MEM_RELEASE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(handle);
    if (rc == HGGC_SUCCESS) {
        vppu_hggc_refund((unsigned long long)handle);
    }
    return rc;
}

/* ---------------------------------------------------------------------------------------
 * Queries. The quota's figures, so a workload that sizes itself from the card sees the slice
 * it was given rather than the whole card.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemGetInfo(size_t *free, size_t *total)
{
    vppu_hggc_count(VPPU_MEM_GET_INFO);

    HGresult (*real)(size_t *, size_t *) = vppu_hggc_next(VPPU_MEM_GET_INFO);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(free, total);
    if (rc != HGGC_SUCCESS) {
        return rc;
    }

    unsigned long long quota = 0ULL;
    unsigned long long available = 0ULL;
    if (!vppu_hggc_view(&quota, &available)) {
        return rc;
    }
    if (total != NULL) {
        *total = (size_t)quota;
    }
    if (free != NULL) {
        *free = (size_t)available;
    }
    return rc;
}

/* ---------------------------------------------------------------------------------------
 * Host memory. Counted, never charged: pinned host pages are not device VRAM, so charging
 * them would refuse an allocation that costs the card nothing.
 *
 * The frees here deliberately do NOT refund. A host pointer is an address in this process's
 * own space and nothing recorded it, so looking it up could only ever match a device handle
 * that happens to carry the same number — a refund for memory that was never freed.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemAllocHost(void **pp, size_t bytesize)
{
    vppu_hggc_count(VPPU_MEM_ALLOC_HOST);

    HGresult (*real)(void **, size_t) = vppu_hggc_next(VPPU_MEM_ALLOC_HOST);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pp, bytesize);
}

HGresult HGGCAPI hgMemFreeHost(void *p)
{
    vppu_hggc_count(VPPU_MEM_FREE_HOST);

    HGresult (*real)(void *) = vppu_hggc_next(VPPU_MEM_FREE_HOST);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(p);
}

/* ---------------------------------------------------------------------------------------
 * Address mapping. Counted, never charged: hgMemCreate is where the VMM path takes physical
 * memory, and these only bind a handle that has already been paid for.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemMap(HGdeviceptr ptr, size_t size, size_t offset,
                          HGmemGenericAllocationHandle handle, unsigned long long flags)
{
    vppu_hggc_count(VPPU_MEM_MAP);

    HGresult (*real)(HGdeviceptr, size_t, size_t, HGmemGenericAllocationHandle,
                     unsigned long long) = vppu_hggc_next(VPPU_MEM_MAP);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(ptr, size, offset, handle, flags);
}

static HGresult map_array_async(enum vppu_entry entry, HGarrayMapInfo *mapInfoList,
                                unsigned int count, HGstream hStream)
{
    vppu_hggc_count(entry);

    HGresult (*real)(HGarrayMapInfo *, unsigned int, HGstream) = vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(mapInfoList, count, hStream);
}

HGresult HGGCAPI hgMemMapArrayAsync(HGarrayMapInfo *mapInfoList, unsigned int count,
                                    HGstream hStream)
{
    return map_array_async(VPPU_MEM_MAP_ARRAY_ASYNC, mapInfoList, count, hStream);
}

HGresult HGGCAPI hgMemMapArrayAsync_ptsz(HGarrayMapInfo *mapInfoList, unsigned int count,
                                         HGstream hStream)
{
    return map_array_async(VPPU_MEM_MAP_ARRAY_ASYNC_PTSZ, mapInfoList, count, hStream);
}

HGresult HGGCAPI hgMemUnmap(HGdeviceptr ptr, size_t size)
{
    vppu_hggc_count(VPPU_MEM_UNMAP);

    HGresult (*real)(HGdeviceptr, size_t) = vppu_hggc_next(VPPU_MEM_UNMAP);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(ptr, size);
}

/* ---------------------------------------------------------------------------------------
 * Pools. Counted, never charged: a pool's memory is taken by hgMemAllocFromPoolAsync, which
 * is charged above, and given back by trimming. They are interposed so that the pool path's
 * crossing into libhggc.so is a counted fact rather than an assumption — and so that an
 * allocation arriving through a pool this library never saw created is still an anomaly the
 * counters can show.
 *
 * Creation now also RECORDS THE POOL'S CARD, which is the second reason to interpose it: a pool
 * belongs to the card named in its props, an allocation out of it lands there whatever the calling
 * thread's context is, and charging the context would spend the wrong card's quota. The handle
 * carries no way to ask afterwards, so the answer has to be kept from the one call that knew it.
 *
 * The table itself sits with the allocations above, because that is where it is read.
 * --------------------------------------------------------------------------------------- */

HGresult HGGCAPI hgMemPoolCreate(HGmemoryPool *pool, const HGmemPoolProps *poolProps)
{
    vppu_hggc_count(VPPU_MEM_POOL_CREATE);

    HGresult (*real)(HGmemoryPool *, const HGmemPoolProps *) =
        vppu_hggc_next(VPPU_MEM_POOL_CREATE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(pool, poolProps);
    if (rc == HGGC_SUCCESS && pool != NULL) {
        pool_remember(*pool, named_card(poolProps == NULL ? NULL : &poolProps->location));
    }
    return rc;
}

HGresult HGGCAPI hgMemPoolDestroy(HGmemoryPool pool)
{
    vppu_hggc_count(VPPU_MEM_POOL_DESTROY);

    HGresult (*real)(HGmemoryPool) = vppu_hggc_next(VPPU_MEM_POOL_DESTROY);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(pool);
    if (rc == HGGC_SUCCESS) {
        pool_forget(pool);
    }
    return rc;
}

HGresult HGGCAPI hgMemPoolExportPointer(HGmemPoolPtrExportData *shareData_out, HGdeviceptr ptr)
{
    vppu_hggc_count(VPPU_MEM_POOL_EXPORT_POINTER);

    HGresult (*real)(HGmemPoolPtrExportData *, HGdeviceptr) =
        vppu_hggc_next(VPPU_MEM_POOL_EXPORT_POINTER);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(shareData_out, ptr);
}

HGresult HGGCAPI hgMemPoolExportToShareableHandle(void *handle_out, HGmemoryPool pool,
                                                  HGmemAllocationHandleType handleType,
                                                  unsigned long long flags)
{
    vppu_hggc_count(VPPU_MEM_POOL_EXPORT_TO_SHAREABLE_HANDLE);

    HGresult (*real)(void *, HGmemoryPool, HGmemAllocationHandleType, unsigned long long) =
        vppu_hggc_next(VPPU_MEM_POOL_EXPORT_TO_SHAREABLE_HANDLE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(handle_out, pool, handleType, flags);
}

HGresult HGGCAPI hgMemPoolGetAccess(HGmemAccess_flags *flags, HGmemoryPool memPool,
                                    HGmemLocation *location)
{
    vppu_hggc_count(VPPU_MEM_POOL_GET_ACCESS);

    HGresult (*real)(HGmemAccess_flags *, HGmemoryPool, HGmemLocation *) =
        vppu_hggc_next(VPPU_MEM_POOL_GET_ACCESS);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(flags, memPool, location);
}

HGresult HGGCAPI hgMemPoolGetAttribute(HGmemoryPool pool, HGmemPool_attribute attr, void *value)
{
    vppu_hggc_count(VPPU_MEM_POOL_GET_ATTRIBUTE);

    HGresult (*real)(HGmemoryPool, HGmemPool_attribute, void *) =
        vppu_hggc_next(VPPU_MEM_POOL_GET_ATTRIBUTE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pool, attr, value);
}

HGresult HGGCAPI hgMemPoolImportFromShareableHandle(HGmemoryPool *pool_out, void *handle,
                                                    HGmemAllocationHandleType handleType,
                                                    unsigned long long flags)
{
    vppu_hggc_count(VPPU_MEM_POOL_IMPORT_FROM_SHAREABLE_HANDLE);

    HGresult (*real)(HGmemoryPool *, void *, HGmemAllocationHandleType, unsigned long long) =
        vppu_hggc_next(VPPU_MEM_POOL_IMPORT_FROM_SHAREABLE_HANDLE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pool_out, handle, handleType, flags);
}

/* An imported pointer maps an allocation another process already paid for, so it is not
 * charged here — and it is deliberately not recorded either, which is what makes a later free
 * of it refund nothing rather than credit this container for memory it never took. */
HGresult HGGCAPI hgMemPoolImportPointer(HGdeviceptr *ptr_out, HGmemoryPool pool,
                                        HGmemPoolPtrExportData *shareData)
{
    vppu_hggc_count(VPPU_MEM_POOL_IMPORT_POINTER);

    HGresult (*real)(HGdeviceptr *, HGmemoryPool, HGmemPoolPtrExportData *) =
        vppu_hggc_next(VPPU_MEM_POOL_IMPORT_POINTER);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(ptr_out, pool, shareData);
}

HGresult HGGCAPI hgMemPoolSetAccess(HGmemoryPool pool, const HGmemAccessDesc *map, size_t count)
{
    vppu_hggc_count(VPPU_MEM_POOL_SET_ACCESS);

    HGresult (*real)(HGmemoryPool, const HGmemAccessDesc *, size_t) =
        vppu_hggc_next(VPPU_MEM_POOL_SET_ACCESS);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pool, map, count);
}

HGresult HGGCAPI hgMemPoolSetAttribute(HGmemoryPool pool, HGmemPool_attribute attr, void *value)
{
    vppu_hggc_count(VPPU_MEM_POOL_SET_ATTRIBUTE);

    HGresult (*real)(HGmemoryPool, HGmemPool_attribute, void *) =
        vppu_hggc_next(VPPU_MEM_POOL_SET_ATTRIBUTE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pool, attr, value);
}

HGresult HGGCAPI hgMemPoolTrimTo(HGmemoryPool pool, size_t minBytesToKeep)
{
    vppu_hggc_count(VPPU_MEM_POOL_TRIM_TO);

    HGresult (*real)(HGmemoryPool, size_t) = vppu_hggc_next(VPPU_MEM_POOL_TRIM_TO);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pool, minBytesToKeep);
}
