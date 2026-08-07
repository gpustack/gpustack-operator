/*
 * hip_mem.c — the classic allocating family, and the frees that give its bytes back.
 *
 * EVERY NAME HERE IS A DOOR SOMEBODY MEASURED OPEN. A shim that wraps `hipMalloc` alone is not a
 * memory quota: under a 256 MiB cap, `hipMallocManaged`, `hipExtMallocWithFlags` and
 * `hipMallocPitch` each satisfied a 512 MiB request straight past it. They are separate exported
 * entry points into the same device memory, not layers over `hipMalloc`, so each needs its own
 * wrapper.
 *
 * THE RUNTIME API AND THE DRIVER API ARE TWO SETS OF DOORS INTO ONE ROOM. `hipMallocPitch` and
 * `hipMemAllocPitch` are different exported symbols; so are `hipMallocArray` and `hipArrayCreate`,
 * and `hipFreeArray` and `hipArrayDestroy`. Each pair reaches the same memory, and each driver-API
 * half was measured taking 512 MiB out of a 64 MiB quota while its runtime-API half was correctly
 * refused. Whichever half a caller happens to use has to be charged, and either free has to refund
 * whichever create was used.
 *
 * HOST MEMORY IS COUNTED AND NEVER CHARGED. `hipHostMalloc` pins pages in system RAM; charging
 * them against a card's VRAM figure would refuse device allocations over host pressure. The
 * counters still tick, so the dump shows the entry was reached.
 */
#define _GNU_SOURCE

#include <stdint.h>

#include <hip/hip_runtime_api.h>

#include "common/vrocm_ledger.h"
#include "common/vrocm_log.h"
#include "hip/hip_resolve.h"
#include "hip/hip_table.h"

VROCM_INTERNAL int vrocm_current_device(void);

typedef hipError_t (*fn_malloc)(void **, size_t);
typedef hipError_t (*fn_managed)(void **, size_t, unsigned int);
typedef hipError_t (*fn_pitch)(void **, size_t *, size_t, size_t);
typedef hipError_t (*fn_drv_pitch)(hipDeviceptr_t *, size_t *, size_t, size_t, unsigned int);
typedef hipError_t (*fn_malloc3d)(struct hipPitchedPtr *, struct hipExtent);
typedef hipError_t (*fn_drv_array)(hipArray_t *, const HIP_ARRAY_DESCRIPTOR *);
typedef hipError_t (*fn_drv_array3d)(hipArray_t *, const HIP_ARRAY3D_DESCRIPTOR *);
typedef hipError_t (*fn_drv_array_destroy)(hipArray_t);
typedef hipError_t (*fn_ext_flags)(void **, size_t, unsigned int);
typedef hipError_t (*fn_array)(hipArray_t *, const struct hipChannelFormatDesc *, size_t, size_t,
                               unsigned int);
typedef hipError_t (*fn_array3d)(hipArray_t *, const struct hipChannelFormatDesc *,
                                 struct hipExtent, unsigned int);
typedef hipError_t (*fn_free)(void *);
typedef hipError_t (*fn_free_array)(hipArray_t);
typedef hipError_t (*fn_host_malloc)(void **, size_t, unsigned int);

/* vrocm_admit_status — one admission's outcome as a HIP status.
 *
 * Every refusal that is ours becomes `hipErrorOutOfMemory`, because that is what it is from the
 * caller's side and it is the one status every framework already handles — PyTorch turns it into
 * its own "HIP out of memory" with the virtualised capacity in the message. A refusal that came
 * from the runtime is passed through untouched: inventing a status for it would hide what the
 * driver actually said. */
VROCM_INTERNAL hipError_t vrocm_admit_status(enum vrocm_admit rc, hipError_t runtime_status)
{
    switch (rc) {
    case VROCM_ADMIT_OK:
        return hipSuccess;
    case VROCM_ADMIT_ALLOC_FAILED:
        return runtime_status;
    default:
        return hipErrorOutOfMemory;
    }
}

/* ---- the classic family ------------------------------------------------------------------ */

struct plain_ctx {
    hipError_t status;
    void **ptr;
    size_t size;
    unsigned int flags;
    fn_malloc plain;
    fn_managed managed;
    fn_ext_flags ext;
};

static bool call_plain(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct plain_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->plain(ctx->ptr, ctx->size);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

static bool call_managed(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct plain_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->managed(ctx->ptr, ctx->size, ctx->flags);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

static bool call_ext(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct plain_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->ext(ctx->ptr, ctx->size, ctx->flags);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

VROCM_EXPORT hipError_t hipMalloc(void **ptr, size_t size)
{
    VROCM_REAL(hipMalloc, fn_malloc);
    VROCM_ENTRY(hipMalloc);

    struct plain_ctx ctx = { .ptr = ptr, .size = size, .plain = real_hipMalloc };
    enum vrocm_admit rc;

    VROCM_HIT(hipMalloc);
    rc = vrocm_ledger_admit(vrocm_current_device(), size, call_plain, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMalloc);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipMallocManaged(void **dev_ptr, size_t size, unsigned int flags)
{
    VROCM_REAL(hipMallocManaged, fn_managed);
    VROCM_ENTRY(hipMallocManaged);

    struct plain_ctx ctx = { .ptr = dev_ptr, .size = size, .flags = flags,
                             .managed = real_hipMallocManaged };
    enum vrocm_admit rc;

    VROCM_HIT(hipMallocManaged);
    rc = vrocm_ledger_admit(vrocm_current_device(), size, call_managed, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMallocManaged);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipExtMallocWithFlags(void **ptr, size_t size_bytes, unsigned int flags)
{
    VROCM_REAL(hipExtMallocWithFlags, fn_ext_flags);
    VROCM_ENTRY(hipExtMallocWithFlags);

    struct plain_ctx ctx = { .ptr = ptr, .size = size_bytes, .flags = flags,
                             .ext = real_hipExtMallocWithFlags };
    enum vrocm_admit rc;

    VROCM_HIT(hipExtMallocWithFlags);
    rc = vrocm_ledger_admit(vrocm_current_device(), size_bytes, call_ext, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipExtMallocWithFlags);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* The pitched entry is the reason the admission's byte count is in-out. The caller asks for a
 * width; the runtime picks a stride and the allocation is `stride × height`, which it only reports
 * on the way back. Admitting on the width and settling on the stride keeps both inside one hold of
 * the card's lock. */
struct pitch_ctx {
    hipError_t status;
    void **ptr;
    size_t *pitch;
    size_t width;
    size_t height;
    fn_pitch real;
};

static bool call_pitch(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct pitch_ctx *ctx = c;

    ctx->status = ctx->real(ctx->ptr, ctx->pitch, ctx->width, ctx->height);
    if (ctx->status != hipSuccess) {
        return false;
    }
    if (ctx->pitch != NULL && ctx->height != 0 && *ctx->pitch > ctx->width) {
        *bytes = (unsigned long long)*ctx->pitch * ctx->height;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

VROCM_EXPORT hipError_t hipMallocPitch(void **ptr, size_t *pitch, size_t width, size_t height)
{
    VROCM_REAL(hipMallocPitch, fn_pitch);
    VROCM_ENTRY(hipMallocPitch);

    struct pitch_ctx ctx = { .ptr = ptr, .pitch = pitch, .width = width, .height = height,
                             .real = real_hipMallocPitch };
    enum vrocm_admit rc;

    VROCM_HIT(hipMallocPitch);
    rc = vrocm_ledger_admit(vrocm_current_device(), (unsigned long long)width * height, call_pitch,
                            &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMallocPitch);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* The DRIVER-API pitched entry. It is a different exported symbol from `hipMallocPitch`, not an
 * alias, and measured it satisfied a 512 MiB request under a 64 MiB quota while its runtime-API
 * twin was refused one. The element size it takes is an alignment hint the runtime folds into the
 * stride, so the footprint is still `pitch × height` and the reconciliation is the same. */
struct drv_pitch_ctx {
    hipError_t status;
    hipDeviceptr_t *ptr;
    size_t *pitch;
    size_t width;
    size_t height;
    unsigned int element;
    fn_drv_pitch real;
};

static bool call_drv_pitch(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct drv_pitch_ctx *ctx = c;

    ctx->status = ctx->real(ctx->ptr, ctx->pitch, ctx->width, ctx->height, ctx->element);
    if (ctx->status != hipSuccess) {
        return false;
    }
    if (ctx->pitch != NULL && ctx->height != 0 && *ctx->pitch > ctx->width) {
        *bytes = (unsigned long long)*ctx->pitch * ctx->height;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

VROCM_EXPORT hipError_t hipMemAllocPitch(hipDeviceptr_t *dptr, size_t *pitch, size_t width_in_bytes,
                                         size_t height, unsigned int element_size_bytes)
{
    VROCM_REAL(hipMemAllocPitch, fn_drv_pitch);
    VROCM_ENTRY(hipMemAllocPitch);

    struct drv_pitch_ctx ctx = { .ptr = dptr, .pitch = pitch, .width = width_in_bytes,
                                 .height = height, .element = element_size_bytes,
                                 .real = real_hipMemAllocPitch };
    enum vrocm_admit rc;

    VROCM_HIT(hipMemAllocPitch);
    rc = vrocm_ledger_admit(vrocm_current_device(), (unsigned long long)width_in_bytes * height,
                            call_drv_pitch, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMemAllocPitch);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* The 3D pitched entry is `hipMallocPitch` with a depth, and it is here because it was measured
 * open: under a 64 MiB quota it satisfied a 512 MiB request while `hipMalloc` was correctly
 * refusing one, exactly the way the pool family did before it was wrapped. It is a separate
 * exported entry into the same linear device memory, not a layer over any of the others.
 *
 * Its extent is in BYTES on the width axis, unlike the array entries below, whose extent is in
 * elements and needs the channel description to become a size. */
struct malloc3d_ctx {
    hipError_t status;
    struct hipPitchedPtr *ptr;
    struct hipExtent extent;
    fn_malloc3d real;
};

static bool call_malloc3d(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct malloc3d_ctx *ctx = c;

    ctx->status = ctx->real(ctx->ptr, ctx->extent);
    if (ctx->status != hipSuccess) {
        return false;
    }
    if (ctx->ptr->pitch > ctx->extent.width) {
        *bytes = (unsigned long long)ctx->ptr->pitch * (ctx->extent.height ? ctx->extent.height : 1) *
                 (ctx->extent.depth ? ctx->extent.depth : 1);
    }
    *key = (unsigned long long)(uintptr_t)ctx->ptr->ptr;
    return true;
}

VROCM_EXPORT hipError_t hipMalloc3D(struct hipPitchedPtr *pitched_dev_ptr, struct hipExtent extent)
{
    VROCM_REAL(hipMalloc3D, fn_malloc3d);
    VROCM_ENTRY(hipMalloc3D);

    struct malloc3d_ctx ctx = { .ptr = pitched_dev_ptr, .extent = extent,
                                .real = real_hipMalloc3D };
    enum vrocm_admit rc;

    VROCM_HIT(hipMalloc3D);
    rc = vrocm_ledger_admit(vrocm_current_device(),
                            (unsigned long long)extent.width * (extent.height ? extent.height : 1) *
                                (extent.depth ? extent.depth : 1),
                            call_malloc3d, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMalloc3D);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* An array's size is not a parameter, it is the format description times the extent. The runtime
 * may pad beyond that and does not say by how much, so this is the caller's figure rather than the
 * allocation's — which is why arrays are charged approximately and everything else exactly. */
static unsigned long long array_bytes(const struct hipChannelFormatDesc *desc, size_t width,
                                      size_t height, size_t depth)
{
    unsigned long long bits;

    if (desc == NULL) {
        return 0;
    }
    bits = (unsigned long long)desc->x + desc->y + desc->z + desc->w;
    return ((bits + 7ULL) / 8ULL) * (width ? width : 1) * (height ? height : 1) *
           (depth ? depth : 1);
}

struct array_ctx {
    hipError_t status;
    hipArray_t *array;
    const struct hipChannelFormatDesc *desc;
    size_t width;
    size_t height;
    struct hipExtent extent;
    unsigned int flags;
    fn_array real2d;
    fn_array3d real3d;
};

static bool call_array(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct array_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->real2d(ctx->array, ctx->desc, ctx->width, ctx->height, ctx->flags);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->array;
    return true;
}

static bool call_array3d(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct array_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->real3d(ctx->array, ctx->desc, ctx->extent, ctx->flags);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->array;
    return true;
}

VROCM_EXPORT hipError_t hipMallocArray(hipArray_t *array, const struct hipChannelFormatDesc *desc, size_t width,
                          size_t height, unsigned int flags)
{
    VROCM_REAL(hipMallocArray, fn_array);
    VROCM_ENTRY(hipMallocArray);

    struct array_ctx ctx = { .array = array, .desc = desc, .width = width, .height = height,
                             .flags = flags, .real2d = real_hipMallocArray };
    enum vrocm_admit rc;

    VROCM_HIT(hipMallocArray);
    rc = vrocm_ledger_admit(vrocm_current_device(), array_bytes(desc, width, height, 1), call_array,
                            &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMallocArray);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipMalloc3DArray(hipArray_t *array, const struct hipChannelFormatDesc *desc,
                            struct hipExtent extent, unsigned int flags)
{
    VROCM_REAL(hipMalloc3DArray, fn_array3d);
    VROCM_ENTRY(hipMalloc3DArray);

    struct array_ctx ctx = { .array = array, .desc = desc, .extent = extent, .flags = flags,
                             .real3d = real_hipMalloc3DArray };
    enum vrocm_admit rc;

    VROCM_HIT(hipMalloc3DArray);
    rc = vrocm_ledger_admit(vrocm_current_device(),
                            array_bytes(desc, extent.width, extent.height, extent.depth),
                            call_array3d, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMalloc3DArray);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* The DRIVER-API array entries. Same memory, a second set of exported symbols, and measured open:
 * `hipArrayCreate` satisfied a 512 MiB request under a 64 MiB quota. The size arrives differently
 * — a format enum and a channel count rather than a channel description — so the only shared
 * thing is the admission itself. */
static unsigned long long format_bytes(hipArray_Format format)
{
    switch (format) {
    case HIP_AD_FORMAT_UNSIGNED_INT8:
    case HIP_AD_FORMAT_SIGNED_INT8:
        return 1;
    case HIP_AD_FORMAT_UNSIGNED_INT16:
    case HIP_AD_FORMAT_SIGNED_INT16:
    case HIP_AD_FORMAT_HALF:
        return 2;
    default:
        /* Every remaining format in the enum is four bytes wide, and an unknown one is charged as
         * four as well: over-charging an addition nobody has seen yet refuses an allocation, where
         * under-charging it hands out memory nobody accounted for. */
        return 4;
    }
}

static unsigned long long drv_array_bytes(hipArray_Format format, unsigned int channels,
                                          size_t width, size_t height, size_t depth)
{
    return format_bytes(format) * (channels ? channels : 1) * (width ? width : 1) *
           (height ? height : 1) * (depth ? depth : 1);
}

struct drv_array_ctx {
    hipError_t status;
    hipArray_t *array;
    const HIP_ARRAY_DESCRIPTOR *desc2d;
    const HIP_ARRAY3D_DESCRIPTOR *desc3d;
    fn_drv_array real2d;
    fn_drv_array3d real3d;
};

static bool call_drv_array(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct drv_array_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->real2d(ctx->array, ctx->desc2d);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->array;
    return true;
}

static bool call_drv_array3d(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct drv_array_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->real3d(ctx->array, ctx->desc3d);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->array;
    return true;
}

VROCM_EXPORT hipError_t hipArrayCreate(hipArray_t *array, const HIP_ARRAY_DESCRIPTOR *desc)
{
    VROCM_REAL(hipArrayCreate, fn_drv_array);
    VROCM_ENTRY(hipArrayCreate);

    struct drv_array_ctx ctx = { .array = array, .desc2d = desc, .real2d = real_hipArrayCreate };
    enum vrocm_admit rc;
    unsigned long long bytes = 0;

    VROCM_HIT(hipArrayCreate);
    if (desc != NULL) {
        bytes = drv_array_bytes(desc->Format, desc->NumChannels, desc->Width, desc->Height, 1);
    }
    rc = vrocm_ledger_admit(vrocm_current_device(), bytes, call_drv_array, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipArrayCreate);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipArray3DCreate(hipArray_t *array, const HIP_ARRAY3D_DESCRIPTOR *desc)
{
    VROCM_REAL(hipArray3DCreate, fn_drv_array3d);
    VROCM_ENTRY(hipArray3DCreate);

    struct drv_array_ctx ctx = { .array = array, .desc3d = desc, .real3d = real_hipArray3DCreate };
    enum vrocm_admit rc;
    unsigned long long bytes = 0;

    VROCM_HIT(hipArray3DCreate);
    if (desc != NULL) {
        bytes = drv_array_bytes(desc->Format, desc->NumChannels, desc->Width, desc->Height,
                                desc->Depth);
    }
    rc = vrocm_ledger_admit(vrocm_current_device(), bytes, call_drv_array3d, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipArray3DCreate);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* ---- the frees ---------------------------------------------------------------------------- */

VROCM_EXPORT hipError_t hipFree(void *ptr)
{
    VROCM_REAL(hipFree, fn_free);
    VROCM_ENTRY(hipFree);

    VROCM_HIT(hipFree);
    /* A pointer this library never recorded is refunded nothing and passed straight through: an
     * allocation made before the preload loaded is exactly that case, and so is one the workload
     * invented. Refunding an unknown key would credit memory that was never charged. */
    if (ptr != NULL) {
        vrocm_ledger_release((unsigned long long)(uintptr_t)ptr, NULL, NULL);
    }
    return real_hipFree(ptr);
}

VROCM_EXPORT hipError_t hipFreeArray(hipArray_t array)
{
    VROCM_REAL(hipFreeArray, fn_free_array);
    VROCM_ENTRY(hipFreeArray);

    VROCM_HIT(hipFreeArray);
    if (array != NULL) {
        vrocm_ledger_release((unsigned long long)(uintptr_t)array, NULL, NULL);
    }
    return real_hipFreeArray(array);
}

/* The driver-API free. It takes the same handle `hipFreeArray` does and either entry accepts an
 * array from either creating entry, so both refund by the same key and a release of a key that was
 * never charged is a no-op. Wrapping only one of the two would leave whichever a caller happened
 * to use holding its charge for the life of the process. */
VROCM_EXPORT hipError_t hipArrayDestroy(hipArray_t array)
{
    VROCM_REAL(hipArrayDestroy, fn_drv_array_destroy);
    VROCM_ENTRY(hipArrayDestroy);

    VROCM_HIT(hipArrayDestroy);
    if (array != NULL) {
        vrocm_ledger_release((unsigned long long)(uintptr_t)array, NULL, NULL);
    }
    return real_hipArrayDestroy(array);
}

/* ---- host memory: counted, never charged --------------------------------------------------- */

VROCM_EXPORT hipError_t hipHostMalloc(void **ptr, size_t size, unsigned int flags)
{
    VROCM_REAL(hipHostMalloc, fn_host_malloc);
    VROCM_ENTRY(hipHostMalloc);

    VROCM_HIT(hipHostMalloc);
    return real_hipHostMalloc(ptr, size, flags);
}

VROCM_EXPORT hipError_t hipHostFree(void *ptr)
{
    VROCM_REAL(hipHostFree, fn_free);
    VROCM_ENTRY(hipHostFree);

    VROCM_HIT(hipHostFree);
    return real_hipHostFree(ptr);
}
