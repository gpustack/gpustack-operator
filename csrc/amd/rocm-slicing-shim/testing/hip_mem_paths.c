/*
 * hip_mem_paths.c — one allocation family per invocation, so a case can name which path crossed.
 *
 * WHY ONE PATH PER PROCESS. Every observation has to be independent of the others' accounting: if
 * two families ran in one process, a refusal on the second could come from the first's charge
 * rather than from the size under test, and the case could not tell which. So the family is an
 * argument and each run is a fresh ledger attachment. `refund` is the deliberate exception — it
 * needs two live allocations and a free between them, which is the only way a refund is observable
 * at all.
 *
 * WHY THE LIST IS THIS LIST. Every name here is a door somebody measured open, not a precaution.
 * With only `hipMalloc` and the pool family wrapped, `hipMallocManaged`, `hipExtMallocWithFlags`
 * and `hipMallocPitch` each satisfied a 512 MiB request under a 256 MiB quota; and with only the
 * classic family wrapped, `hipMallocFromPoolAsync` took another 10 GiB on RDNA and 50 GiB on CDNA
 * out of a card whose quota was 2 GiB. The pool family is not a layer over `hipMalloc`.
 *
 * WHY `host` IS HERE AND IS EXPECTED TO SUCCEED. Pinned host pages are not device VRAM, so the
 * library counts them and charges nothing. Running it is how a case proves that is still true —
 * a `host` path that started failing under quota would mean the library had begun charging system
 * memory against a device figure.
 *
 * IT LINKS THE HIP RUNTIME, unlike the product. This is a workload, not an interposer: letting the
 * linker resolve the headers' R0600 mappings and type-check every signature is the point, where a
 * hand-written dlsym table would be a second place to get an ABI name wrong.
 *
 * Exit status is 0 whenever the program ran what it was asked to; whether the allocation succeeded
 * is in the `result=` line, because "refused" is a PASS for half the case's arms and a FAIL for the
 * other half, and only the case knows which arm it is in.
 */
#define _GNU_SOURCE

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <hip/hip_runtime.h>

#define MIB (1024ULL * 1024ULL)

static void step(const char *path, const char *name, hipError_t rc)
{
    printf("PATH %s step=%s rc=%d (%s)\n", path, name, (int)rc, hipGetErrorString(rc));
}

static int finish(const char *path, int ok, hipError_t rc)
{
    printf("PATH %s result=%s rc=%d\n", path, ok ? "success" : "failed", (int)rc);
    return 0;
}

static int path_plain(size_t bytes)
{
    void *ptr = NULL;
    hipError_t rc = hipMalloc(&ptr, bytes);

    step("plain", "hipMalloc", rc);
    if (rc != hipSuccess) {
        return finish("plain", 0, rc);
    }
    (void)hipFree(ptr);
    return finish("plain", 1, hipSuccess);
}

static int path_managed(size_t bytes)
{
    void *ptr = NULL;
    hipError_t rc = hipMallocManaged(&ptr, bytes, hipMemAttachGlobal);

    step("managed", "hipMallocManaged", rc);
    if (rc != hipSuccess) {
        return finish("managed", 0, rc);
    }
    (void)hipFree(ptr);
    return finish("managed", 1, hipSuccess);
}

static int path_ext(size_t bytes)
{
    void *ptr = NULL;
    hipError_t rc = hipExtMallocWithFlags(&ptr, bytes, hipDeviceMallocDefault);

    step("ext", "hipExtMallocWithFlags", rc);
    if (rc != hipSuccess) {
        return finish("ext", 0, rc);
    }
    (void)hipFree(ptr);
    return finish("ext", 1, hipSuccess);
}

/* path_pitch — the one family whose real footprint is not the number the caller passed.
 *
 * The runtime pads each row to its own alignment, so `width x height` understates what the card
 * gave out; the library admits on the caller's figure and reconciles to `pitch x height` under the
 * same lock. Printing the pitch is what lets a case check the reconciliation rather than assume it. */
static int path_pitch(size_t bytes)
{
    size_t height = 1024;
    size_t width = bytes / height;
    size_t pitch = 0;
    void *ptr = NULL;
    hipError_t rc;

    if (width == 0) {
        width = 1;
    }
    rc = hipMallocPitch(&ptr, &pitch, width, height);
    step("pitch", "hipMallocPitch", rc);
    printf("PATH pitch width=%zu height=%zu pitch=%zu asked_mib=%llu took_mib=%llu\n", width, height,
           pitch, (unsigned long long)(width * height) / MIB,
           (unsigned long long)(pitch * height) / MIB);
    if (rc != hipSuccess) {
        return finish("pitch", 0, rc);
    }
    (void)hipFree(ptr);
    return finish("pitch", 1, hipSuccess);
}

/* device_limit — one texture-dimension ceiling, or a conservative default if it cannot be read.
 *
 * The array families are the only ones whose request has a SHAPE as well as a size, and a shape the
 * hardware will not accept is refused with `hipErrorInvalidValue` — which a case must not confuse
 * with the quota's `hipErrorOutOfMemory`. Asking the device rather than hard-coding a shape is what
 * keeps that distinction clean on a part with different ceilings. */
static size_t device_limit(hipDeviceAttribute_t which, int device, size_t fallback)
{
    int value = 0;

    if (hipDeviceGetAttribute(&value, which, device) != hipSuccess || value <= 0) {
        return fallback;
    }
    return (size_t)value;
}

/* shape_2d — the widest shape holding `elems` that fits inside the device's 2D ceilings.
 *
 * Halve the width and double the height until the width fits; the result is as close to the
 * hardware's preferred layout as a test needs, and it is reported so a refusal can be read.
 *
 * A request too large to shape at all is a real ceiling rather than a quota outcome, and the two
 * must not be confused: `max_w * max_h * 4` is roughly 1 GiB on a part reporting 16384 each way, so
 * an array arm has to be exercised under a quota BELOW that. The failure prints the ceiling for
 * exactly that reason. */
static int shape_2d(size_t elems, int device, size_t *width, size_t *height, size_t *ceiling)
{
    size_t max_w = device_limit(hipDeviceAttributeMaxTexture2DWidth, device, 16384);
    size_t max_h = device_limit(hipDeviceAttributeMaxTexture2DHeight, device, 16384);

    *ceiling = max_w * max_h;
    *height = 1;
    *width = elems;
    while (*width > max_w && *height * 2 <= max_h) {
        *height *= 2;
        *width = elems / *height;
    }
    return *width <= max_w && *width > 0 && *height <= max_h;
}

static int path_array(size_t bytes, int device)
{
    hipChannelFormatDesc desc = hipCreateChannelDesc(32, 0, 0, 0, hipChannelFormatKindFloat);
    hipArray_t array = NULL;
    size_t width = 0, height = 0, ceiling = 0;
    hipError_t rc;

    if (!shape_2d(bytes / 4, device, &width, &height, &ceiling)) {
        printf("PATH array shape=unavailable asked_mib=%llu ceiling_mib=%llu\n",
               (unsigned long long)bytes / MIB, (unsigned long long)(ceiling * 4) / MIB);
        return finish("array", 0, hipErrorInvalidValue);
    }
    printf("PATH array width=%zu height=%zu\n", width, height);
    rc = hipMallocArray(&array, &desc, width, height, hipArrayDefault);
    step("array", "hipMallocArray", rc);
    /* Measured: this returns "operation not supported" on gfx942 whatever the shape and whatever
     * the quota, so its coverage can only be proven on RDNA. A case reading this must branch on the
     * architecture rather than treat the refusal as the quota working. */
    if (rc != hipSuccess) {
        return finish("array", 0, rc);
    }
    (void)hipFreeArray(array);
    return finish("array", 1, hipSuccess);
}

static int path_array3d(size_t bytes, int device)
{
    hipChannelFormatDesc desc = hipCreateChannelDesc(32, 0, 0, 0, hipChannelFormatKindFloat);
    size_t max_d = device_limit(hipDeviceAttributeMaxTexture3DDepth, device, 2048);
    hipArray_t array = NULL;
    size_t width = 0, height = 0, depth = 64, ceiling = 0;
    hipExtent extent;
    hipError_t rc;

    if (depth > max_d) {
        depth = max_d;
    }
    if (!shape_2d(bytes / (4 * depth), device, &width, &height, &ceiling)) {
        printf("PATH array3d shape=unavailable asked_mib=%llu ceiling_mib=%llu\n",
               (unsigned long long)bytes / MIB, (unsigned long long)(ceiling * depth * 4) / MIB);
        return finish("array3d", 0, hipErrorInvalidValue);
    }
    /* The 3D ceilings are far lower than the 2D ones, so a shape that fits a 2D array may not fit
     * a volume; clamping here rather than failing keeps the path exercisable at any size. */
    if (width > device_limit(hipDeviceAttributeMaxTexture3DWidth, device, 2048)) {
        width = device_limit(hipDeviceAttributeMaxTexture3DWidth, device, 2048);
    }
    if (height > device_limit(hipDeviceAttributeMaxTexture3DHeight, device, 2048)) {
        height = device_limit(hipDeviceAttributeMaxTexture3DHeight, device, 2048);
    }
    extent.width = width;
    extent.height = height;
    extent.depth = depth;
    printf("PATH array3d width=%zu height=%zu depth=%zu took_mib=%llu\n", width, height, depth,
           (unsigned long long)(width * height * depth * 4) / MIB);

    rc = hipMalloc3DArray(&array, &desc, extent, hipArrayDefault);
    step("array3d", "hipMalloc3DArray", rc);
    if (rc != hipSuccess) {
        return finish("array3d", 0, rc);
    }
    (void)hipFreeArray(array);
    return finish("array3d", 1, hipSuccess);
}

static int path_host(size_t bytes)
{
    void *ptr = NULL;
    hipError_t rc = hipHostMalloc(&ptr, bytes, hipHostMallocDefault);

    step("host", "hipHostMalloc", rc);
    if (rc != hipSuccess) {
        return finish("host", 0, rc);
    }
    (void)hipHostFree(ptr);
    return finish("host", 1, hipSuccess);
}

static int path_async(size_t bytes)
{
    void *ptr = NULL;
    hipStream_t stream = NULL;
    hipError_t rc = hipStreamCreate(&stream);

    step("async", "hipStreamCreate", rc);
    if (rc != hipSuccess) {
        return finish("async", 0, rc);
    }
    rc = hipMallocAsync(&ptr, bytes, stream);
    step("async", "hipMallocAsync", rc);
    if (rc != hipSuccess) {
        return finish("async", 0, rc);
    }
    (void)hipFreeAsync(ptr, stream);
    (void)hipStreamSynchronize(stream);
    (void)hipStreamDestroy(stream);
    return finish("async", 1, hipSuccess);
}

/* path_pool — the measured 6x overrun, and the reason the stream-ordered family is wrapped at all.
 *
 * `hipDeviceGetDefaultMemPool` hands out a usable pool with no special privilege, and PyTorch's
 * mempool path goes straight through it. */
static int path_pool(size_t bytes)
{
    void *ptr = NULL;
    hipMemPool_t pool = NULL;
    hipStream_t stream = NULL;
    hipError_t rc = hipDeviceGetDefaultMemPool(&pool, 0);

    step("pool", "hipDeviceGetDefaultMemPool", rc);
    if (rc != hipSuccess) {
        return finish("pool", 0, rc);
    }
    rc = hipStreamCreate(&stream);
    step("pool", "hipStreamCreate", rc);
    if (rc != hipSuccess) {
        return finish("pool", 0, rc);
    }
    rc = hipMallocFromPoolAsync(&ptr, bytes, pool, stream);
    step("pool", "hipMallocFromPoolAsync", rc);
    if (rc != hipSuccess) {
        return finish("pool", 0, rc);
    }
    (void)hipFreeAsync(ptr, stream);
    (void)hipStreamSynchronize(stream);
    (void)hipStreamDestroy(stream);
    return finish("pool", 1, hipSuccess);
}

/* path_refund — take the quota, give it back, take it again.
 *
 * The second allocation is the assertion: it can only succeed if the free actually refunded, and it
 * is the one observation in this program that needs two allocations in one process. Call it with a
 * size just over half the quota, so the pair cannot both be held. */
static int path_refund(size_t bytes)
{
    void *first = NULL, *second = NULL;
    hipError_t rc = hipMalloc(&first, bytes);

    step("refund", "hipMalloc/1", rc);
    if (rc != hipSuccess) {
        return finish("refund", 0, rc);
    }
    rc = hipFree(first);
    step("refund", "hipFree/1", rc);

    rc = hipMalloc(&second, bytes);
    step("refund", "hipMalloc/2", rc);
    if (rc != hipSuccess) {
        return finish("refund", 0, rc);
    }
    (void)hipFree(second);
    return finish("refund", 1, hipSuccess);
}

static void usage(void)
{
    fprintf(stderr, "usage: hip_mem_paths <path> <mib> [device]\n"
                    "  paths: plain managed ext pitch array array3d host async pool refund\n");
}

int main(int argc, char **argv)
{
    const char *path;
    unsigned long long mib;
    size_t bytes;
    int device = 0;
    hipError_t rc;

    if (argc < 3) {
        usage();
        return 2;
    }
    path = argv[1];
    mib = strtoull(argv[2], NULL, 10);
    if (argc > 3) {
        device = (int)strtol(argv[3], NULL, 10);
    }
    if (mib == 0) {
        fprintf(stderr, "hip_mem_paths: <mib> must be non-zero\n");
        return 2;
    }
    bytes = (size_t)(mib * MIB);

    rc = hipSetDevice(device);
    step(path, "hipSetDevice", rc);
    if (rc != hipSuccess) {
        return finish(path, 0, rc);
    }
    printf("PATH %s device=%d mib=%llu\n", path, device, mib);

    if (strcmp(path, "plain") == 0) {
        return path_plain(bytes);
    }
    if (strcmp(path, "managed") == 0) {
        return path_managed(bytes);
    }
    if (strcmp(path, "ext") == 0) {
        return path_ext(bytes);
    }
    if (strcmp(path, "pitch") == 0) {
        return path_pitch(bytes);
    }
    if (strcmp(path, "array") == 0) {
        return path_array(bytes, device);
    }
    if (strcmp(path, "array3d") == 0) {
        return path_array3d(bytes, device);
    }
    if (strcmp(path, "host") == 0) {
        return path_host(bytes);
    }
    if (strcmp(path, "async") == 0) {
        return path_async(bytes);
    }
    if (strcmp(path, "pool") == 0) {
        return path_pool(bytes);
    }
    if (strcmp(path, "refund") == 0) {
        return path_refund(bytes);
    }
    usage();
    return 2;
}
