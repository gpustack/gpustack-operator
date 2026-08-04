/*
 * hggc_launch.c — the driver layer's launch entries.
 *
 * One definition per exported name in libhggc.so's launch surface, as the SDK's symbol manifest
 * lists it. Sixteen of them, and every one is here whether it is gated or only counted, for the
 * reason the memory surface gives: coverage is the claim, and a name left out is a way for a
 * workload to spend compute this cap never sees.
 *
 * WHAT A WRAPPER DOES is one of three things, and the enum in hggc_quota.h groups the names by
 * which:
 *   - gate: hold the caller until its card's compute window admits the launch, then call the
 *     vendor. The decision is in hggc_compute.c;
 *   - gate as a graph: the same, charged a configurable multiple of the window, because one
 *     graph launch runs however many kernels were captured into it;
 *   - count: call the vendor unchanged, having counted the crossing. Host callbacks run on the
 *     CPU, so delaying them frees none of the card.
 *
 * A REFUSAL IS NOT A THROTTLE. vppu_hggc_gate() waits for a launch that merely has to wait and
 * answers false only when the container's configuration cannot be enforced at all — no usable
 * quota, no current context, no reachable ledger. That is what HGGC_ERROR_NOT_PERMITTED below
 * means: the same fail-closed position the memory entries take with
 * HGGC_ERROR_OUT_OF_MEMORY, never a launch let through unmeasured.
 *
 * THE SIGNATURES ARE THE HEADER'S, NOT COPIES, exactly as on the memory side. The launch entries
 * carry no version mapping — libhggc.so exports `hgLaunchKernel` and not `hgLaunchKernel_v2` —
 * so the plain name defines the plain symbol and the header still type-checks every parameter.
 * The `_ptsz` variants take their type from the plain entry through __typeof__ rather than a
 * retyped prototype: the header maps a plain name onto its `_ptsz` symbol only under
 * __HGGC_API_PER_THREAD_DEFAULT_STREAM, which this library must not define.
 */
#define _GNU_SOURCE

/* hggc.h includes only <stdlib.h> and <stdint.h>, so it supplies neither NULL nor bool for its
 * own declarations. */
#include <stdbool.h>
#include <stddef.h>

#include <hggc.h>

/* hgLaunchKernelExAD and its _ptsz variant are exported by libhggc.so but declared in the AD
 * header rather than hggc.h, so covering them means including it — the alternative is two
 * hand-written prototypes for a config struct this file never looks inside. */
#include <hggc_ad.h>

#include "common/vppu.h"
#include "hggc/hggc_quota.h"

/* The stream variants, typed from the plain entries they mirror. */
extern __typeof__(hgLaunchKernel) hgLaunchKernel_ptsz;
extern __typeof__(hgLaunchKernelEx) hgLaunchKernelEx_ptsz;
extern __typeof__(hgLaunchKernelExAD) hgLaunchKernelExAD_ptsz;
extern __typeof__(hgLaunchCooperativeKernel) hgLaunchCooperativeKernel_ptsz;
extern __typeof__(hgGraphLaunch) hgGraphLaunch_ptsz;
extern __typeof__(hgLaunchHostFunc) hgLaunchHostFunc_ptsz;

/* ---------------------------------------------------------------------------------------
 * Kernel launches. Gated: this is where a workload spends the card.
 * --------------------------------------------------------------------------------------- */

static HGresult launch_kernel(enum vppu_entry entry, HGfunction f, unsigned int gridDimX,
                              unsigned int gridDimY, unsigned int gridDimZ,
                              unsigned int blockDimX, unsigned int blockDimY,
                              unsigned int blockDimZ, unsigned int sharedMemBytes,
                              HGstream hStream, void **kernelParams, void **extra)
{
    vppu_hggc_count(entry);
    if (!vppu_hggc_gate(entry, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGfunction, unsigned int, unsigned int, unsigned int, unsigned int,
                     unsigned int, unsigned int, unsigned int, HGstream, void **, void **) =
        vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(f, gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ, sharedMemBytes,
                hStream, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernel(HGfunction f, unsigned int gridDimX, unsigned int gridDimY,
                                unsigned int gridDimZ, unsigned int blockDimX,
                                unsigned int blockDimY, unsigned int blockDimZ,
                                unsigned int sharedMemBytes, HGstream hStream,
                                void **kernelParams, void **extra)
{
    return launch_kernel(VPPU_LAUNCH_KERNEL, f, gridDimX, gridDimY, gridDimZ, blockDimX,
                         blockDimY, blockDimZ, sharedMemBytes, hStream, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernel_ptsz(HGfunction f, unsigned int gridDimX, unsigned int gridDimY,
                                     unsigned int gridDimZ, unsigned int blockDimX,
                                     unsigned int blockDimY, unsigned int blockDimZ,
                                     unsigned int sharedMemBytes, HGstream hStream,
                                     void **kernelParams, void **extra)
{
    return launch_kernel(VPPU_LAUNCH_KERNEL_PTSZ, f, gridDimX, gridDimY, gridDimZ, blockDimX,
                         blockDimY, blockDimZ, sharedMemBytes, hStream, kernelParams, extra);
}

static HGresult launch_kernel_ex(enum vppu_entry entry, const HGlaunchConfig *config,
                                 HGfunction f, void **kernelParams, void **extra)
{
    vppu_hggc_count(entry);
    if (!vppu_hggc_gate(entry, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(const HGlaunchConfig *, HGfunction, void **, void **) =
        vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(config, f, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernelEx(const HGlaunchConfig *config, HGfunction f,
                                  void **kernelParams, void **extra)
{
    return launch_kernel_ex(VPPU_LAUNCH_KERNEL_EX, config, f, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernelEx_ptsz(const HGlaunchConfig *config, HGfunction f,
                                       void **kernelParams, void **extra)
{
    return launch_kernel_ex(VPPU_LAUNCH_KERNEL_EX_PTSZ, config, f, kernelParams, extra);
}

/* The AD form takes its own config struct, so it cannot share the body above however alike the
 * two read: the parameter this file passes through has a different type. */
static HGresult launch_kernel_ex_ad(enum vppu_entry entry, const HGlaunchConfigAD *config,
                                    HGfunction f, void **kernelParams, void **extra)
{
    vppu_hggc_count(entry);
    if (!vppu_hggc_gate(entry, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(const HGlaunchConfigAD *, HGfunction, void **, void **) =
        vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(config, f, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernelExAD(const HGlaunchConfigAD *config, HGfunction f,
                                    void **kernelParams, void **extra)
{
    return launch_kernel_ex_ad(VPPU_LAUNCH_KERNEL_EX_AD, config, f, kernelParams, extra);
}

HGresult HGGCAPI hgLaunchKernelExAD_ptsz(const HGlaunchConfigAD *config, HGfunction f,
                                         void **kernelParams, void **extra)
{
    return launch_kernel_ex_ad(VPPU_LAUNCH_KERNEL_EX_AD_PTSZ, config, f, kernelParams, extra);
}

static HGresult launch_cooperative(enum vppu_entry entry, HGfunction f, unsigned int gridDimX,
                                   unsigned int gridDimY, unsigned int gridDimZ,
                                   unsigned int blockDimX, unsigned int blockDimY,
                                   unsigned int blockDimZ, unsigned int sharedMemBytes,
                                   HGstream hStream, void **kernelParams)
{
    vppu_hggc_count(entry);
    if (!vppu_hggc_gate(entry, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGfunction, unsigned int, unsigned int, unsigned int, unsigned int,
                     unsigned int, unsigned int, unsigned int, HGstream, void **) =
        vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(f, gridDimX, gridDimY, gridDimZ, blockDimX, blockDimY, blockDimZ, sharedMemBytes,
                hStream, kernelParams);
}

HGresult HGGCAPI hgLaunchCooperativeKernel(HGfunction f, unsigned int gridDimX,
                                           unsigned int gridDimY, unsigned int gridDimZ,
                                           unsigned int blockDimX, unsigned int blockDimY,
                                           unsigned int blockDimZ, unsigned int sharedMemBytes,
                                           HGstream hStream, void **kernelParams)
{
    return launch_cooperative(VPPU_LAUNCH_COOPERATIVE_KERNEL, f, gridDimX, gridDimY, gridDimZ,
                              blockDimX, blockDimY, blockDimZ, sharedMemBytes, hStream,
                              kernelParams);
}

HGresult HGGCAPI hgLaunchCooperativeKernel_ptsz(HGfunction f, unsigned int gridDimX,
                                                unsigned int gridDimY, unsigned int gridDimZ,
                                                unsigned int blockDimX, unsigned int blockDimY,
                                                unsigned int blockDimZ,
                                                unsigned int sharedMemBytes, HGstream hStream,
                                                void **kernelParams)
{
    return launch_cooperative(VPPU_LAUNCH_COOPERATIVE_KERNEL_PTSZ, f, gridDimX, gridDimY,
                              gridDimZ, blockDimX, blockDimY, blockDimZ, sharedMemBytes, hStream,
                              kernelParams);
}

/* The multi-device form launches on every context in its list, and the window it is gated
 * against is the calling context's card. Decoding the list to gate each card separately would
 * mean waiting on several windows at once, which is a lock order this library has no reason to
 * define; the calling context's window is the honest approximation, and the entry is counted so
 * a workload that uses it is visible rather than assumed absent. */
HGresult HGGCAPI hgLaunchCooperativeKernelMultiDevice(HGGC_LAUNCH_PARAMS *launchParamsList,
                                                      unsigned int numDevices, unsigned int flags)
{
    vppu_hggc_count(VPPU_LAUNCH_COOPERATIVE_KERNEL_MULTI_DEVICE);
    if (!vppu_hggc_gate(VPPU_LAUNCH_COOPERATIVE_KERNEL_MULTI_DEVICE, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGGC_LAUNCH_PARAMS *, unsigned int, unsigned int) =
        vppu_hggc_next(VPPU_LAUNCH_COOPERATIVE_KERNEL_MULTI_DEVICE);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(launchParamsList, numDevices, flags);
}

/* The three legacy entries: a function launched with the grid dimensions set by earlier calls
 * rather than passed in. Gated like any other kernel — the dimensions are the vendor's business,
 * and the window does not depend on them. */
HGresult HGGCAPI hgLaunch(HGfunction f)
{
    vppu_hggc_count(VPPU_LAUNCH);
    if (!vppu_hggc_gate(VPPU_LAUNCH, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGfunction) = vppu_hggc_next(VPPU_LAUNCH);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(f);
}

HGresult HGGCAPI hgLaunchGrid(HGfunction f, int grid_width, int grid_height)
{
    vppu_hggc_count(VPPU_LAUNCH_GRID);
    if (!vppu_hggc_gate(VPPU_LAUNCH_GRID, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGfunction, int, int) = vppu_hggc_next(VPPU_LAUNCH_GRID);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(f, grid_width, grid_height);
}

HGresult HGGCAPI hgLaunchGridAsync(HGfunction f, int grid_width, int grid_height,
                                   HGstream hStream)
{
    vppu_hggc_count(VPPU_LAUNCH_GRID_ASYNC);
    if (!vppu_hggc_gate(VPPU_LAUNCH_GRID_ASYNC, false)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGfunction, int, int, HGstream) = vppu_hggc_next(VPPU_LAUNCH_GRID_ASYNC);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(f, grid_width, grid_height, hStream);
}

/* ---------------------------------------------------------------------------------------
 * Graph launches. Gated as graphs, so the window they are admitted in can be tightened by
 * HGGC_SM_GRAPH_WEIGHT once measurement says whether graphs escape the cap.
 * --------------------------------------------------------------------------------------- */

static HGresult graph_launch(enum vppu_entry entry, HGgraphExec hGraphExec, HGstream hStream)
{
    vppu_hggc_count(entry);
    if (!vppu_hggc_gate(entry, true)) {
        return HGGC_ERROR_NOT_PERMITTED;
    }

    HGresult (*real)(HGgraphExec, HGstream) = vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(hGraphExec, hStream);
}

HGresult HGGCAPI hgGraphLaunch(HGgraphExec hGraphExec, HGstream hStream)
{
    return graph_launch(VPPU_GRAPH_LAUNCH, hGraphExec, hStream);
}

HGresult HGGCAPI hgGraphLaunch_ptsz(HGgraphExec hGraphExec, HGstream hStream)
{
    return graph_launch(VPPU_GRAPH_LAUNCH_PTSZ, hGraphExec, hStream);
}

/* ---------------------------------------------------------------------------------------
 * Host callbacks. Counted, never gated: they run on the CPU, so holding one back would delay
 * the workload without freeing any of the card the cap is about.
 * --------------------------------------------------------------------------------------- */

static HGresult launch_host_func(enum vppu_entry entry, HGstream hStream, HGhostFn fn,
                                 void *userData)
{
    vppu_hggc_count(entry);

    HGresult (*real)(HGstream, HGhostFn, void *) = vppu_hggc_next(entry);
    if (real == NULL) {
        return HGGC_ERROR_NOT_FOUND;
    }
    return real(hStream, fn, userData);
}

HGresult HGGCAPI hgLaunchHostFunc(HGstream hStream, HGhostFn fn, void *userData)
{
    return launch_host_func(VPPU_LAUNCH_HOST_FUNC, hStream, fn, userData);
}

HGresult HGGCAPI hgLaunchHostFunc_ptsz(HGstream hStream, HGhostFn fn, void *userData)
{
    return launch_host_func(VPPU_LAUNCH_HOST_FUNC_PTSZ, hStream, fn, userData);
}
