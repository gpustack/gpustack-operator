/*
 * hggc_mem_paths.c — Gate 2's exerciser: one memory path, one size, per invocation.
 *
 * Gate 2 asks whether a VRAM quota enforced at the driver layer actually covers every way
 * a workload can take device memory. This program is the workload half of that question:
 * it drives ONE named path and reports whether the allocation succeeded, so
 * cases/thead-case-3.sh can run it three times per path — under quota with the shim, over
 * quota without the shim, over quota with the shim — and compare.
 *
 * One path per process on purpose. It keeps each observation independent of the others'
 * accounting, so a refusal can only come from the size under test. `refund` is the
 * deliberate exception — it needs two live allocations and their frees inside one process,
 * because that is the only way a refund can be observed at all.
 *
 * The plain and async paths deliberately call the RUNTIME entries (hggcMalloc,
 * hggcMallocAsync in libhggcrt), because the crossing from the runtime layer into
 * libhggc.so is exactly what Gate 2 measures. The pool and VMM paths have no runtime
 * equivalent, so they call the driver entries directly. The procaddr path calls neither by
 * name: it asks the driver for an entry's address and allocates through that pointer.
 *
 * Like the utilisation probe this is a test binary that links the SDK, not a shim: the
 * linker resolves the headers' _v2/_v4 mappings and type-checks every call.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Neither SDK header includes what its own declarations need. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>
#include <hggc_runtime_api.h>

static void report(const char *path, const char *step, int rc)
{
    printf("PATH %s step=%s rc=%d\n", path, step, rc);
}

static int finish(const char *path, bool ok, int rc)
{
    printf("PATH %s result=%s rc=%d\n", path, ok ? "success" : "failed", rc);
    return 0;
}

/* driver_context — hgInit + a context, for the paths with no runtime equivalent. */
static bool driver_context(const char *path, int index, HGdevice *dev, HGcontext *ctx)
{
    HGresult rc = hgInit(0);
    report(path, "hgInit", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    rc = hgDeviceGet(dev, index);
    report(path, "hgDeviceGet", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    rc = hgCtxCreate(ctx, NULL, 0, *dev);
    report(path, "hgCtxCreate", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return false;
    }

    rc = hgCtxSetCurrent(*ctx);
    report(path, "hgCtxSetCurrent", (int)rc);
    return rc == HGGC_SUCCESS;
}

static int path_plain(int index, size_t bytes)
{
    const char *path = "plain";
    hggcError_t rc = hggcSetDevice(index);
    report(path, "hggcSetDevice", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    void *ptr = NULL;
    rc = hggcMalloc(&ptr, bytes);
    report(path, "hggcMalloc", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    hggcFree(ptr);
    return finish(path, true, 0);
}

static int path_async(int index, size_t bytes)
{
    const char *path = "async";
    hggcError_t rc = hggcSetDevice(index);
    report(path, "hggcSetDevice", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    hggcStream_t stream = NULL;
    rc = hggcStreamCreate(&stream);
    report(path, "hggcStreamCreate", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    void *ptr = NULL;
    rc = hggcMallocAsync(&ptr, bytes, stream);
    report(path, "hggcMallocAsync", (int)rc);
    /* Synchronise before judging: an async allocation may only fail on the stream. */
    hggcError_t sync_rc = hggcStreamSynchronize(stream);
    report(path, "hggcStreamSynchronize", (int)sync_rc);
    if (rc != hggcSuccess || sync_rc != hggcSuccess) {
        return finish(path, false, (int)(rc != hggcSuccess ? rc : sync_rc));
    }

    hggcFree(ptr);
    return finish(path, true, 0);
}

/* path_refund — the one path that holds TWO allocations at once and then frees both.
 *
 * Every other path here allocates once and exits, so the shim's refund bookkeeping is never
 * exercised: a ledger that gives nothing back on free still passes them, because the process
 * dies before a second allocation could be denied. This path takes `bytes` twice (the caller
 * sizes it at half the quota, so the pair fills the quota exactly), frees both, then asks for
 * `bytes * 2` — the whole quota. That last request is admitted only if BOTH refunds landed,
 * so a ledger that loses the second one refuses it.
 */
static int path_refund(int index, size_t bytes)
{
    const char *path = "refund";
    hggcError_t rc = hggcSetDevice(index);
    report(path, "hggcSetDevice", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    void *first = NULL;
    rc = hggcMalloc(&first, bytes);
    report(path, "hggcMalloc.first", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    void *second = NULL;
    rc = hggcMalloc(&second, bytes);
    report(path, "hggcMalloc.second", (int)rc);
    if (rc != hggcSuccess) {
        hggcFree(first);
        return finish(path, false, (int)rc);
    }

    /* Freed in insertion order, which is the order that breaks a ledger deleting entries by
     * emptying their slot: the first free truncates the probe chain the second one needs. */
    rc = hggcFree(first);
    report(path, "hggcFree.first", (int)rc);
    rc = hggcFree(second);
    report(path, "hggcFree.second", (int)rc);

    void *whole = NULL;
    rc = hggcMalloc(&whole, bytes * 2);
    report(path, "hggcMalloc.whole", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    hggcFree(whole);
    return finish(path, true, 0);
}

/* path_hold — allocate and KEEP the memory, so another process in the same container meets a
 * quota that is genuinely already spent.
 *
 * Every other path frees before it exits, which leaves cross-process accounting invisible: the
 * charge is gone by the time a second process asks for anything. The verdict is printed BEFORE
 * the wait, so a case can read it without waiting for the hold to end.
 */
static int path_hold(int index, size_t bytes, unsigned int seconds)
{
    const char *path = "hold";
    hggcError_t rc = hggcSetDevice(index);
    report(path, "hggcSetDevice", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    void *ptr = NULL;
    rc = hggcMalloc(&ptr, bytes);
    report(path, "hggcMalloc", (int)rc);
    if (rc != hggcSuccess) {
        return finish(path, false, (int)rc);
    }

    finish(path, true, 0);
    fflush(stdout);
    sleep(seconds);
    hggcFree(ptr);
    return 0;
}

/* path_procaddr — take memory through a pointer the DRIVER handed out, not one the linker did.
 *
 * Every other path calls an allocation entry by name, so the dynamic linker resolves it and a
 * preloaded definition wins. hgGetProcAddress is the way past that: it returns the driver's own
 * address for a named entry, and a caller holding that pointer never consults the linker again.
 * libhggcrt binds driver entries exactly this way, so a quota that does not cover the resolver
 * covers nothing the runtime layer resolved through it.
 *
 * The version asked for is the SDK's own (HGGC_VERSION), which is what makes the driver return
 * the current form of the entry rather than its v1 ancestor.
 */
static int path_procaddr(int index, size_t bytes)
{
    const char *path = "procaddr";
    HGdevice dev = 0;
    HGcontext ctx = NULL;
    if (!driver_context(path, index, &dev, &ctx)) {
        return finish(path, false, -1);
    }

    void *pfn = NULL;
    HGdriverProcAddressQueryResult status = HG_GET_PROC_ADDRESS_SUCCESS;
    HGresult rc = hgGetProcAddress("hgMemAlloc", &pfn, HGGC_VERSION,
                                   HG_GET_PROC_ADDRESS_DEFAULT, &status);
    printf("PATH %s step=hgGetProcAddress rc=%d status=%d pfn=%s\n", path, (int)rc, (int)status,
           (pfn == NULL) ? "null" : "resolved");
    if (rc != HGGC_SUCCESS || pfn == NULL) {
        return finish(path, false, (int)rc);
    }

    HGresult (*alloc)(HGdeviceptr *, size_t) = (HGresult(*)(HGdeviceptr *, size_t))pfn;
    HGdeviceptr dptr = 0;
    rc = alloc(&dptr, bytes);
    report(path, "hgMemAlloc.resolved", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return finish(path, false, (int)rc);
    }

    hgMemFree(dptr);
    return finish(path, true, 0);
}

static int path_pool(int index, size_t bytes, int target)
{
    const char *path = "pool";
    HGdevice dev = 0;
    HGcontext ctx = NULL;
    if (!driver_context(path, index, &dev, &ctx)) {
        return finish(path, false, -1);
    }

    /* Same crossing as the VMM path, through the OTHER handle that carries a card: the default
     * pool of a card that need not be the context's. */
    HGdevice pool_dev = (target >= 0) ? (HGdevice)target : dev;
    HGmemoryPool pool = NULL;
    HGresult rc = hgDeviceGetDefaultMemPool(&pool, pool_dev);
    printf("PATH %s context=%d target=%d\n", path, (int)dev, (int)pool_dev);
    report(path, "hgDeviceGetDefaultMemPool", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return finish(path, false, (int)rc);
    }

    HGstream stream = NULL;
    rc = hgStreamCreate(&stream, 0);
    report(path, "hgStreamCreate", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return finish(path, false, (int)rc);
    }

    HGdeviceptr dptr = 0;
    rc = hgMemAllocFromPoolAsync(&dptr, bytes, pool, stream);
    report(path, "hgMemAllocFromPoolAsync", (int)rc);
    HGresult sync_rc = hgStreamSynchronize(stream);
    report(path, "hgStreamSynchronize", (int)sync_rc);
    if (rc != HGGC_SUCCESS || sync_rc != HGGC_SUCCESS) {
        return finish(path, false, (int)(rc != HGGC_SUCCESS ? rc : sync_rc));
    }

    hgMemFreeAsync(dptr, stream);
    hgStreamSynchronize(stream);
    /* Trimming is part of the pool path Gate 2 names, and the shim counts it. */
    rc = hgMemPoolTrimTo(pool, 0);
    report(path, "hgMemPoolTrimTo", (int)rc);
    hgStreamDestroy(stream);
    return finish(path, true, 0);
}

static int path_vmm(int index, size_t bytes, int target)
{
    const char *path = "vmm";
    HGdevice dev = 0;
    HGcontext ctx = NULL;
    if (!driver_context(path, index, &dev, &ctx)) {
        return finish(path, false, -1);
    }

    HGmemAllocationProp prop;
    memset(&prop, 0, sizeof(prop));
    prop.type = HG_MEM_ALLOCATION_TYPE_PINNED;
    prop.location.type = HG_MEM_LOCATION_TYPE_DEVICE;
    /* The card the ALLOCATION is for, which a caller may aim somewhere other than the context it
     * is calling from — that is the one shape that tells a quota charged to the prop's card from
     * one charged to the calling thread's, and it needs a container holding two cards to see. */
    prop.location.id = (target >= 0) ? target : (int)dev;
    printf("PATH %s context=%d target=%d\n", path, (int)dev, prop.location.id);

    size_t granularity = 0;
    HGresult rc = hgMemGetAllocationGranularity(&granularity, &prop,
                                                HG_MEM_ALLOC_GRANULARITY_RECOMMENDED);
    report(path, "hgMemGetAllocationGranularity", (int)rc);
    if (rc != HGGC_SUCCESS || granularity == 0) {
        return finish(path, false, (int)rc);
    }

    /* The VMM path rejects an unaligned size, so round up — and report the rounded size,
     * because the quota is charged against what hgMemCreate actually asks for. */
    size_t size = ((bytes + granularity - 1) / granularity) * granularity;
    printf("PATH %s granularity=%zu rounded_bytes=%zu\n", path, granularity, size);

    HGmemGenericAllocationHandle handle = 0;
    rc = hgMemCreate(&handle, size, &prop, 0);
    report(path, "hgMemCreate", (int)rc);
    if (rc != HGGC_SUCCESS) {
        return finish(path, false, (int)rc);
    }

    HGdeviceptr ptr = 0;
    rc = hgMemAddressReserve(&ptr, size, granularity, 0, 0);
    report(path, "hgMemAddressReserve", (int)rc);
    if (rc == HGGC_SUCCESS) {
        rc = hgMemMap(ptr, size, 0, handle, 0);
        report(path, "hgMemMap", (int)rc);
        if (rc == HGGC_SUCCESS) {
            hgMemUnmap(ptr, size);
        }
        hgMemAddressFree(ptr, size);
    }

    hgMemRelease(handle);
    return finish(path, rc == HGGC_SUCCESS, (int)rc);
}

int main(int argc, char **argv)
{
    if (argc < 4) {
        fprintf(stderr,
                "usage: %s <device-index> <bytes> "
                "<plain|async|pool|vmm|procaddr|refund|hold> [hold-seconds | target-card]\n",
                argv[0]);
        return 2;
    }

    int index = (int)strtol(argv[1], NULL, 10);
    size_t bytes = (size_t)strtoull(argv[2], NULL, 10);
    const char *path = argv[3];
    /* The fourth argument is the hold's seconds for `hold` and the ALLOCATION's card for the two
     * paths that carry one of their own; -1 means "the context's", which is what they did before. */
    int target = (argc > 4) ? (int)strtol(argv[4], NULL, 10) : -1;
    printf("PATHS device=%d bytes=%zu path=%s\n", index, bytes, path);

    if (strcmp(path, "plain") == 0) {
        return path_plain(index, bytes);
    }
    if (strcmp(path, "async") == 0) {
        return path_async(index, bytes);
    }
    if (strcmp(path, "pool") == 0) {
        return path_pool(index, bytes, target);
    }
    if (strcmp(path, "vmm") == 0) {
        return path_vmm(index, bytes, target);
    }
    if (strcmp(path, "procaddr") == 0) {
        return path_procaddr(index, bytes);
    }
    if (strcmp(path, "refund") == 0) {
        return path_refund(index, bytes);
    }
    if (strcmp(path, "hold") == 0) {
        unsigned int seconds = (argc > 4) ? (unsigned int)strtoul(argv[4], NULL, 10) : 5u;

        return path_hold(index, bytes, seconds);
    }

    fprintf(stderr, "unknown path: %s\n", path);
    return 2;
}
