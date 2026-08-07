/*
 * hip_mem.c — the classic allocating family, and the frees that give its bytes back.
 *
 * EVERY NAME HERE IS A DOOR SOMEBODY MEASURED OPEN. A shim that wraps `hipMalloc` alone is not a
 * memory quota: under a 256 MiB cap, `hipMallocManaged`, `hipExtMallocWithFlags` and
 * `hipMallocPitch` each satisfied a 512 MiB request straight past it. They are separate exported
 * entry points into the same device memory, not layers over `hipMalloc`, so each needs its own
 * wrapper.
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
