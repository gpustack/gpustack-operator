/*
 * hip_query.c — the reported-capacity family: every exported way a workload can ask how much
 * memory the card has.
 *
 * THERE ARE FOUR OF THEM AND THE OBVIOUS ONE IS NEVER CALLED. `libamdhip64` exports
 * `hipGetDeviceProperties@@hip_4.2`, `hipGetDevicePropertiesR0600@@hip_6.0` and
 * `hipGetDevicePropertiesR0000@@hip_4.2` at three distinct addresses, plus `hipDeviceTotalMem`.
 * ROCm 6+ headers macro-map the plain name onto `R0600`, and a tracer bullet registering both
 * logged only `R0600` ever binding. Intercepting the plain name alone virtualises nothing.
 *
 * AND THE TWO STRUCTS ARE NOT THE SAME SHAPE. Measured on ROCm 7.2.4: `hipDeviceProp_tR0600` is
 * 1472 bytes with `totalGlobalMem` at 288, `hipDeviceProp_tR0000` is 792 bytes with it at 256.
 * Writing the 6.0 field into a caller that passed the 4.2 struct would store 32 bytes past where
 * the caller expects — memory corruption in its frame, not a missed quota. So the pre-6.0 entries
 * take the pre-6.0 type, and the offsets are taken with `offsetof` and pinned below.
 *
 * MULTIPROCESSORCOUNT IS LEFT ALONE, deliberately. This library does not enforce compute; the
 * platform does, through a CU mask, and measured that mask does not change `multiProcessorCount`
 * either. Rewriting it here would invent a figure that matches neither the hardware nor the mask.
 */
#define _GNU_SOURCE

#include <stddef.h>

#include <hip/hip_runtime_api.h>
/* The pre-6.0 property struct is declared only here. It is an amd_detail/ header, which is a
 * dependency worth naming: if a future ROCm moves or drops it, this file stops compiling, which is
 * the right failure — the alternative is hard-coding 256 and finding out at run time. */
#include <hip/amd_detail/hip_prof_str.h>

#include "common/vrocm_ledger.h"
#include "common/vrocm_log.h"
#include "common/vrocm_quota.h"
#include "hip/hip_resolve.h"
#include "hip/hip_table.h"

/* The regression fixture, measured on ROCm 7.2 / gfx1101 and ROCm 7.2.4 / gfx942 — identical on
 * both — and confirmed against the ROCm 6.4 headers as well, which is what lets one build serve
 * every version. A release that changes any of these must be measured again, not guessed at, and
 * this is what stops it shipping silently. */
_Static_assert(sizeof(hipDeviceProp_tR0600) == 1472, "hipDeviceProp_t size changed; re-measure");
_Static_assert(offsetof(hipDeviceProp_tR0600, totalGlobalMem) == 288,
               "totalGlobalMem moved; re-measure");
_Static_assert(offsetof(hipDeviceProp_tR0600, multiProcessorCount) == 388,
               "multiProcessorCount moved; re-measure");
_Static_assert(sizeof(hipDeviceProp_tR0000) == 792, "pre-6.0 prop size changed; re-measure");
_Static_assert(offsetof(hipDeviceProp_tR0000, totalGlobalMem) == 256,
               "pre-6.0 totalGlobalMem moved; re-measure");

typedef hipError_t (*fn_mem_get_info)(size_t *, size_t *);
typedef hipError_t (*fn_total_mem)(size_t *, hipDevice_t);
typedef hipError_t (*fn_props_r0600)(hipDeviceProp_tR0600 *, int);
typedef hipError_t (*fn_props_r0000)(hipDeviceProp_tR0000 *, int);
typedef hipError_t (*fn_get_device)(int *);

/* vrocm_current_device — the container-local index of the card in force.
 *
 * Shared with the allocating families, which need the same answer for the same reason: every
 * figure this library holds is per card, and the index is the one after ROCR_VISIBLE_DEVICES has
 * filtered and reordered the list — the same index space HSA_CU_MASK's GPU_list uses. */
VROCM_INTERNAL int vrocm_current_device(void)
{
    VROCM_REAL(hipGetDevice, fn_get_device);

    int device = 0;

    if (real_hipGetDevice(&device) != hipSuccess) {
        return -1;
    }
    return device;
}

/* reported_total — the figure to answer a capacity question with: the slice, but never more of
 * it than the card has.
 *
 * ONLY EVER DOWNWARDS. A quota above what the runtime reports is a misconfiguration — the
 * allocator derives every figure from the card's own capacity and cannot produce one — and
 * answering it would advertise memory no allocation can obtain. The enforced ceiling is already
 * the smaller of the two, because a request past the card fails in the driver whatever the
 * ledger admitted; lowering here is what makes the reported figure and the enforced one the same
 * number. A framework sizing its arena from this would otherwise meet the difference as an
 * out-of-memory rather than as a wrong answer. */
static size_t reported_total(size_t real_total, unsigned long long quota)
{
    return quota != 0 && quota < real_total ? (size_t)quota : real_total;
}

VROCM_EXPORT hipError_t hipMemGetInfo(size_t *free_bytes, size_t *total_bytes)
{
    VROCM_REAL(hipMemGetInfo, fn_mem_get_info);
    VROCM_ENTRY(hipMemGetInfo);

    hipError_t status;
    unsigned long long quota, used;
    int device;

    VROCM_HIT(hipMemGetInfo);
    status = real_hipMemGetInfo(free_bytes, total_bytes);
    if (status != hipSuccess) {
        return status;
    }

    device = vrocm_current_device();
    quota = vrocm_quota_memory_bytes(device);
    if (quota == 0) {
        return status;
    }

    used = vrocm_ledger_used(device);
    if (total_bytes != NULL) {
        *total_bytes = reported_total(*total_bytes, quota);
    }
    if (free_bytes != NULL) {
        size_t remaining = (size_t)(used < quota ? quota - used : 0);

        /* The smaller of what the slice has left and what the card has left. Reporting the slice's
         * figure alone would promise memory the card cannot supply once its neighbours have filled
         * it, and a framework that sizes its arena from this would then fail at the allocation
         * rather than at the question. */
        *free_bytes = remaining < *free_bytes ? remaining : *free_bytes;
    }
    return status;
}

VROCM_EXPORT hipError_t hipDeviceTotalMem(size_t *bytes, hipDevice_t device)
{
    VROCM_REAL(hipDeviceTotalMem, fn_total_mem);
    VROCM_ENTRY(hipDeviceTotalMem);

    hipError_t status;
    unsigned long long quota;

    VROCM_HIT(hipDeviceTotalMem);
    status = real_hipDeviceTotalMem(bytes, device);
    if (status != hipSuccess || bytes == NULL) {
        return status;
    }
    quota = vrocm_quota_memory_bytes((int)device);
    *bytes = reported_total(*bytes, quota);
    return status;
}

VROCM_EXPORT hipError_t hipGetDevicePropertiesR0600(hipDeviceProp_tR0600 *prop, int device)
{
    VROCM_REAL(hipGetDevicePropertiesR0600, fn_props_r0600);
    VROCM_ENTRY(hipGetDevicePropertiesR0600);

    hipError_t status;
    unsigned long long quota;

    VROCM_HIT(hipGetDevicePropertiesR0600);
    status = real_hipGetDevicePropertiesR0600(prop, device);
    if (status != hipSuccess || prop == NULL) {
        return status;
    }
    quota = vrocm_quota_memory_bytes(device);
    prop->totalGlobalMem = reported_total(prop->totalGlobalMem, quota);
    return status;
}

VROCM_EXPORT hipError_t hipGetDevicePropertiesR0000(hipDeviceProp_tR0000 *prop, int device)
{
    VROCM_REAL(hipGetDevicePropertiesR0000, fn_props_r0000);
    VROCM_ENTRY(hipGetDevicePropertiesR0000);

    hipError_t status;
    unsigned long long quota;

    VROCM_HIT(hipGetDevicePropertiesR0000);
    status = real_hipGetDevicePropertiesR0000(prop, device);
    if (status != hipSuccess || prop == NULL) {
        return status;
    }
    quota = vrocm_quota_memory_bytes(device);
    prop->totalGlobalMem = reported_total(prop->totalGlobalMem, quota);
    return status;
}

/* The plain name is a THIRD implementation at its own address, not an alias of either versioned
 * one, and it carries the 4.2 ABI — so it takes the pre-6.0 struct. The header macro-maps this
 * name onto R0600, so defining it needs the map removed first. */
#undef hipGetDeviceProperties

VROCM_EXPORT hipError_t hipGetDeviceProperties(hipDeviceProp_tR0000 *prop, int device)
{
    VROCM_REAL(hipGetDeviceProperties, fn_props_r0000);
    VROCM_ENTRY(hipGetDeviceProperties);

    hipError_t status;
    unsigned long long quota;

    VROCM_HIT(hipGetDeviceProperties);
    status = real_hipGetDeviceProperties(prop, device);
    if (status != hipSuccess || prop == NULL) {
        return status;
    }
    quota = vrocm_quota_memory_bytes(device);
    prop->totalGlobalMem = reported_total(prop->totalGlobalMem, quota);
    return status;
}
