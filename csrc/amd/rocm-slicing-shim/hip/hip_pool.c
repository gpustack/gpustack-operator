/*
 * hip_pool.c — the stream-ordered allocator, which is the hole a `hipMalloc`-only ledger leaves.
 *
 * THIS FAMILY IS NOT A LAYER OVER hipMalloc. Measured: under a 2 GiB cap with only the classic
 * family charged, `hipMalloc` correctly stopped at 2.000 GiB and `hipMallocFromPoolAsync` then
 * took another 10 GiB on RDNA and 50 GiB on CDNA from the same card — in both cases the figure is
 * where the test loop stopped, not where the card did. `hipDeviceGetDefaultMemPool` hands out a
 * usable pool with no special privilege, and PyTorch's mempool path goes straight through it. A
 * quota that does not charge these entries is not a quota.
 *
 * AN IMPORTED POINTER IS DELIBERATELY NOT RECORDED. `hipMemPoolImportPointer` maps memory another
 * process already allocated and already paid for; charging it would bill the same bytes twice, and
 * — worse — recording it would let this container's free refund a charge it never made.
 */
#define _GNU_SOURCE

#include <stdint.h>

#include <hip/hip_runtime_api.h>

#include "common/vrocm_ledger.h"
#include "common/vrocm_log.h"
#include "hip/hip_resolve.h"
#include "hip/hip_table.h"

VROCM_INTERNAL int vrocm_current_device(void);
VROCM_INTERNAL hipError_t vrocm_admit_status(enum vrocm_admit rc, hipError_t runtime_status);

typedef hipError_t (*fn_malloc_async)(void **, size_t, hipStream_t);
typedef hipError_t (*fn_malloc_from_pool)(void **, size_t, hipMemPool_t, hipStream_t);
typedef hipError_t (*fn_free_async)(void *, hipStream_t);
typedef hipError_t (*fn_import)(void **, hipMemPool_t, hipMemPoolPtrExportData *);

struct pool_ctx {
    hipError_t status;
    void **ptr;
    size_t size;
    hipMemPool_t pool;
    hipStream_t stream;
    fn_malloc_async async;
    fn_malloc_from_pool from_pool;
};

static bool call_async(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct pool_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->async(ctx->ptr, ctx->size, ctx->stream);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

static bool call_from_pool(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct pool_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->from_pool(ctx->ptr, ctx->size, ctx->pool, ctx->stream);
    if (ctx->status != hipSuccess) {
        return false;
    }
    *key = (unsigned long long)(uintptr_t)*ctx->ptr;
    return true;
}

VROCM_EXPORT hipError_t hipMallocAsync(void **dev_ptr, size_t size, hipStream_t stream)
{
    VROCM_REAL(hipMallocAsync, fn_malloc_async);
    VROCM_ENTRY(hipMallocAsync);

    struct pool_ctx ctx = { .ptr = dev_ptr, .size = size, .stream = stream,
                            .async = real_hipMallocAsync };
    enum vrocm_admit rc;

    VROCM_HIT(hipMallocAsync);
    rc = vrocm_ledger_admit(vrocm_current_device(), size, call_async, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMallocAsync);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipMallocFromPoolAsync(void **dev_ptr, size_t size, hipMemPool_t mem_pool,
                                  hipStream_t stream)
{
    VROCM_REAL(hipMallocFromPoolAsync, fn_malloc_from_pool);
    VROCM_ENTRY(hipMallocFromPoolAsync);

    struct pool_ctx ctx = { .ptr = dev_ptr, .size = size, .pool = mem_pool, .stream = stream,
                            .from_pool = real_hipMallocFromPoolAsync };
    enum vrocm_admit rc;

    VROCM_HIT(hipMallocFromPoolAsync);
    rc = vrocm_ledger_admit(vrocm_current_device(), size, call_from_pool, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMallocFromPoolAsync);
    }
    return vrocm_admit_status(rc, ctx.status);
}

/* Refunded when the free is ISSUED, not when the stream reaches it. The alternative — holding the
 * charge until a synchronisation this library never sees — would leave a container that frees and
 * re-allocates in a loop climbing towards its cap for no reason a user could act on. The cost is
 * that the accounted total can sit briefly below the driver's, which errs towards admitting rather
 * than towards refusing, and the next admission's own check is what still bounds the card.
 *
 * Issued is still not the same as accepted, so the refund follows the call and depends on its
 * status: a free the runtime refuses queues nothing, and crediting the charge back for it would
 * hand the container bytes the card is still holding. */
VROCM_EXPORT hipError_t hipFreeAsync(void *dev_ptr, hipStream_t stream)
{
    hipError_t status;

    VROCM_REAL(hipFreeAsync, fn_free_async);
    VROCM_ENTRY(hipFreeAsync);

    VROCM_HIT(hipFreeAsync);
    status = real_hipFreeAsync(dev_ptr, stream);
    if (status == hipSuccess && dev_ptr != NULL) {
        vrocm_ledger_release((unsigned long long)(uintptr_t)dev_ptr, NULL, NULL);
    }
    return status;
}

VROCM_EXPORT hipError_t hipMemPoolImportPointer(void **dev_ptr, hipMemPool_t mem_pool,
                                   hipMemPoolPtrExportData *export_data)
{
    VROCM_REAL(hipMemPoolImportPointer, fn_import);
    VROCM_ENTRY(hipMemPoolImportPointer);

    /* Counted so the dump shows the entry was reached, and charged nothing. See the header. */
    VROCM_HIT(hipMemPoolImportPointer);
    return real_hipMemPoolImportPointer(dev_ptr, mem_pool, export_data);
}
