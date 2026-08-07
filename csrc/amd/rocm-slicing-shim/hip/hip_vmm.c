/*
 * hip_vmm.c — the virtual-memory-management family, which allocates without any `hipMalloc` in it.
 *
 * THIS IS THE DOOR A REAL FRAMEWORK WALKS THROUGH. PyTorch's expandable-segments allocator is
 * built on exactly this sequence — reserve an address range once, then create and map physical
 * memory into it as the arena grows — so a quota that ignores it is not a quota for the
 * configuration a tuned training job ships with. Measured against a build that wrapped every
 * `hipMalloc*` name there is: under a 64 MiB quota the sequence below took 512 MiB and reported
 * no error at any step.
 *
 * ONLY `hipMemCreate` ALLOCATES. The rest of the family moves addresses around:
 * `hipMemAddressReserve` takes virtual address space and no memory, `hipMemMap` binds an existing
 * handle into that space, `hipMemSetAccess` sets permissions, and their inverses undo those. So
 * one entry is charged and one is refunded, and wrapping the other four would count the same
 * bytes several times — a handle mapped at two addresses is still one allocation.
 *
 * THE CHARGE FOLLOWS THE PROPERTY, NOT THE CONTEXT. `hipMemAllocationProp.location.id` names the
 * device the memory is being created on, and a caller may name a card other than the one its
 * thread's context is set to. Reading the current device instead would charge the wrong card in
 * exactly the case per-card accounting exists for.
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

typedef hipError_t (*fn_mem_create)(hipMemGenericAllocationHandle_t *, size_t,
                                    const hipMemAllocationProp *, unsigned long long);
typedef hipError_t (*fn_mem_release)(hipMemGenericAllocationHandle_t);

struct vmm_ctx {
    hipError_t status;
    hipMemGenericAllocationHandle_t *handle;
    size_t size;
    const hipMemAllocationProp *prop;
    unsigned long long flags;
    fn_mem_create real;
};

static bool call_mem_create(void *c, unsigned long long *key, unsigned long long *bytes)
{
    struct vmm_ctx *ctx = c;

    (void)bytes;
    ctx->status = ctx->real(ctx->handle, ctx->size, ctx->prop, ctx->flags);
    if (ctx->status != hipSuccess) {
        return false;
    }
    /* The handle is the key, because there is no pointer yet: the address this memory will answer
     * to is chosen later by `hipMemMap`, and `hipMemRelease` gives the handle back rather than an
     * address. Keying on the mapping would also lose the charge for memory created and never
     * mapped, which is memory the card has given out all the same. */
    *key = (unsigned long long)(uintptr_t)*ctx->handle;
    return true;
}

/* vmm_device — the card the property names, or the calling thread's if it names none. */
static int vmm_device(const hipMemAllocationProp *prop)
{
    if (prop != NULL && prop->location.type == hipMemLocationTypeDevice) {
        return prop->location.id;
    }
    return vrocm_current_device();
}

VROCM_EXPORT hipError_t hipMemCreate(hipMemGenericAllocationHandle_t *handle, size_t size,
                                     const hipMemAllocationProp *prop, unsigned long long flags)
{
    VROCM_REAL(hipMemCreate, fn_mem_create);
    VROCM_ENTRY(hipMemCreate);

    struct vmm_ctx ctx = { .handle = handle, .size = size, .prop = prop, .flags = flags,
                           .real = real_hipMemCreate };
    enum vrocm_admit rc;

    VROCM_HIT(hipMemCreate);
    rc = vrocm_ledger_admit(vmm_device(prop), size, call_mem_create, &ctx);
    if (rc != VROCM_ADMIT_OK) {
        VROCM_DENIED(hipMemCreate);
    }
    return vrocm_admit_status(rc, ctx.status);
}

VROCM_EXPORT hipError_t hipMemRelease(hipMemGenericAllocationHandle_t handle)
{
    VROCM_REAL(hipMemRelease, fn_mem_release);
    VROCM_ENTRY(hipMemRelease);

    VROCM_HIT(hipMemRelease);
    vrocm_ledger_release((unsigned long long)(uintptr_t)handle, NULL, NULL);
    return real_hipMemRelease(handle);
}
