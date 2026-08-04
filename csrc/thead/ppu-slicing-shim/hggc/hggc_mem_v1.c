/*
 * hggc_mem_v1.c — the v1 ABI memory entries.
 *
 * libhggc.so exports both `hgMemAlloc` and `hgMemAlloc_v2`, and likewise for Free, GetInfo,
 * AllocPitch and AllocHost. Only the second of each pair is what a source-level call compiles
 * to, because hggc.h maps the plain name onto the versioned one — so a shim written against
 * the header alone covers half of each pair and leaves the other half open. A workload built
 * against an older SDK, or a library that resolves the plain name by hand, reaches that half.
 *
 * THE V1 FORMS ARE NOT THE V2 FORMS WITH A DIFFERENT NAME. `HGdeviceptr_v1` is `unsigned int`
 * where `HGdeviceptr` is `unsigned long long`, and the sizes are `unsigned int` rather than
 * `size_t`. Reusing a v2 prototype for a v1 symbol would read the caller's arguments off by
 * whole registers, so each signature below is the v1 one.
 *
 * WHY THEY ARE IN THEIR OWN FILE. Defining a plain symbol means #undef'ing the header's
 * mapping first, and a file that both keeps and cancels that mapping would depend on the order
 * its definitions happen to sit in. Here the five #undefs stand at the top and this file
 * defines nothing else.
 *
 * The prototypes come from hggc.h itself, which declares them under
 * __HGGC_API_VERSION_INTERNAL. That macro is deliberately NOT defined: it also changes
 * unrelated declarations and enum members across the header, which is too broad a change to
 * make in order to reach five signatures.
 */
#define _GNU_SOURCE

#include <limits.h>

/* hggc.h includes only <stdlib.h> and <stdint.h>, so it supplies neither NULL nor bool for
 * its own declarations. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>

#include "common/vppu.h"
#include "hggc/hggc_quota.h"

/* Reach the v1 symbols by cancelling the header's mapping onto the versioned ones. */
#undef hgMemAlloc
#undef hgMemAllocHost
#undef hgMemAllocPitch
#undef hgMemFree
#undef hgMemGetInfo

HGresult HGGCAPI hgMemAlloc(HGdeviceptr_v1 *dptr, unsigned int bytesize)
{
    int device = -1;

    vppu_hggc_count(VPPU_MEM_ALLOC_V1);
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC_V1, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr_v1 *, unsigned int) = vppu_hggc_next(VPPU_MEM_ALLOC_V1);
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

/* Both dimensions are 32-bit here, so their product cannot overflow the 64-bit count the
 * ledger works in — the overflow guard the v2 form needs has nothing to check. */
HGresult HGGCAPI hgMemAllocPitch(HGdeviceptr_v1 *dptr, unsigned int *pPitch,
                                 unsigned int WidthInBytes, unsigned int Height,
                                 unsigned int ElementSizeBytes)
{
    int device = -1;
    unsigned long long request = (unsigned long long)WidthInBytes * Height;

    vppu_hggc_count(VPPU_MEM_ALLOC_PITCH_V1);
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC_PITCH_V1, request, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;
    }

    HGresult (*real)(HGdeviceptr_v1 *, unsigned int *, unsigned int, unsigned int,
                     unsigned int) = vppu_hggc_next(VPPU_MEM_ALLOC_PITCH_V1);
    if (real == NULL) {
        vppu_hggc_rollback(device, request);
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr, pPitch, WidthInBytes, Height, ElementSizeBytes);
    if (rc == HGGC_SUCCESS && dptr != NULL) {
        unsigned long long actual = request;

        if (pPitch != NULL) {
            actual = (unsigned long long)*pPitch * Height;
        }
        vppu_hggc_commit_sized(device, (unsigned long long)*dptr, request, actual);
    } else {
        vppu_hggc_rollback(device, request);
    }
    return rc;
}

HGresult HGGCAPI hgMemFree(HGdeviceptr_v1 dptr)
{
    vppu_hggc_count(VPPU_MEM_FREE_V1);

    HGresult (*real)(HGdeviceptr_v1) = vppu_hggc_next(VPPU_MEM_FREE_V1);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }

    HGresult rc = real(dptr);
    if (rc == HGGC_SUCCESS) {
        vppu_hggc_refund((unsigned long long)dptr);
    }
    return rc;
}

/* The v1 query reports in `unsigned int`, so a quota above 4 GiB does not fit. Saturating is
 * the only honest answer left: reporting the low 32 bits would tell a workload it has a few
 * hundred megabytes when it has tens of gigabytes. */
HGresult HGGCAPI hgMemGetInfo(unsigned int *free, unsigned int *total)
{
    vppu_hggc_count(VPPU_MEM_GET_INFO_V1);

    HGresult (*real)(unsigned int *, unsigned int *) = vppu_hggc_next(VPPU_MEM_GET_INFO_V1);
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
        *total = (quota > UINT_MAX) ? UINT_MAX : (unsigned int)quota;
    }
    if (free != NULL) {
        *free = (available > UINT_MAX) ? UINT_MAX : (unsigned int)available;
    }
    return rc;
}

/* Host memory, so counted and never charged — see hggc_mem.c for why. */
HGresult HGGCAPI hgMemAllocHost(void **pp, unsigned int bytesize)
{
    vppu_hggc_count(VPPU_MEM_ALLOC_HOST_V1);

    HGresult (*real)(void **, unsigned int) = vppu_hggc_next(VPPU_MEM_ALLOC_HOST_V1);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(pp, bytesize);
}
