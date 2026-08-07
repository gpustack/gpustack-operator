/*
 * hip_props_probe.c — every exported way a workload can ask how much memory the card has, and which
 * symbol each of them actually binds to.
 *
 * WHY THE BINDING MATTERS AS MUCH AS THE FIGURE. `libamdhip64` exports
 * `hipGetDeviceProperties@@hip_4.2`, `hipGetDevicePropertiesR0600@@hip_6.0` and
 * `hipGetDevicePropertiesR0000@@hip_4.2` at three DISTINCT addresses, and ROCm 6+ headers macro-map
 * the plain name onto `R0600`. So a wrapper that interposed only the plain name would compile,
 * link, load, and virtualise nothing — measured, a tracer bullet registering both logged only
 * `R0600` ever binding. Printing the object each name resolves to is what turns that from a claim
 * into an observation: with the library preloaded the intercepted names resolve into `libvrocm.so`,
 * and any that still resolve into `libamdhip64.so` is a hole.
 *
 * WHY IT READS FOUR ENTRY POINTS AND NOT ONE. `hipMemGetInfo`, `hipDeviceTotalMem` and
 * `hipDeviceProp_t.totalGlobalMem` are three independent paths to the card's capacity, and a
 * framework may take any of them. PyTorch's `get_device_properties().total_memory` takes the third,
 * so leaving it alone leaks the physical figure to exactly the code most likely to size an arena
 * from it.
 *
 * THE TWO PROPERTY STRUCTS ARE NOT THE SAME SHAPE, which is why this reports the sizes it compiled
 * against: `hipDeviceProp_tR0600` is 1472 bytes with `totalGlobalMem` at 288, `hipDeviceProp_tR0000`
 * is 792 with it at 256. A case comparing this output across ROCm versions is checking that the
 * regression fixture in `hip/hip_query.c` still describes the runtime in front of it.
 *
 * MULTIPROCESSORCOUNT IS REPORTED AND IS NOT EXPECTED TO CHANGE. The library does not rewrite it,
 * and measured, a CU mask does not either — so this figure is the card's, under any quota and any
 * mask. It is printed because a case that saw it change would have found either a library that
 * started inventing compute figures or a runtime that changed its mind.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>

#include <hip/hip_runtime.h>
#include <hip/hip_runtime_api.h>
/* The pre-6.0 property struct is declared only here, in an amd_detail/ header — the same dependency
 * `hip/hip_query.c` takes, and for the same reason: the alternative is hard-coding 256. */
#include <hip/amd_detail/hip_prof_str.h>

#define MIB (1024ULL * 1024ULL)

/* report_binding — which shared object defines the symbol this process would call.
 *
 * dlsym(RTLD_DEFAULT) walks the same search order the loader used, so this answers the question the
 * case is actually asking: not "does libvrocm export this" but "would a call land there". */
static void report_binding(const char *name)
{
    void *sym = dlsym(RTLD_DEFAULT, name);
    Dl_info info;

    if (sym == NULL) {
        printf("BIND name=%s object=<unresolved>\n", name);
        return;
    }
    if (dladdr(sym, &info) == 0 || info.dli_fname == NULL) {
        printf("BIND name=%s object=<unknown> addr=%p\n", name, sym);
        return;
    }
    printf("BIND name=%s object=%s addr=%p\n", name, info.dli_fname, sym);
}

int main(int argc, char **argv)
{
    hipDeviceProp_tR0600 prop600;
    hipDeviceProp_tR0000 prop000;
    size_t free_bytes = 0, total_bytes = 0, device_total = 0;
    int device = 0;
    hipError_t rc;

    if (argc > 1) {
        device = (int)strtol(argv[1], NULL, 10);
    }
    rc = hipSetDevice(device);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipSetDevice(%d): %s\n", device, hipGetErrorString(rc));
        return 2;
    }

    printf("LAYOUT r0600_size=%zu r0600_total_off=%zu r0600_mpc_off=%zu r0000_size=%zu"
           " r0000_total_off=%zu\n",
           sizeof(hipDeviceProp_tR0600), offsetof(hipDeviceProp_tR0600, totalGlobalMem),
           offsetof(hipDeviceProp_tR0600, multiProcessorCount), sizeof(hipDeviceProp_tR0000),
           offsetof(hipDeviceProp_tR0000, totalGlobalMem));

    report_binding("hipMemGetInfo");
    report_binding("hipDeviceTotalMem");
    report_binding("hipGetDeviceProperties");
    report_binding("hipGetDevicePropertiesR0600");
    report_binding("hipGetDevicePropertiesR0000");

    rc = hipMemGetInfo(&free_bytes, &total_bytes);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipMemGetInfo: %s\n", hipGetErrorString(rc));
        return 2;
    }
    printf("CAP entry=hipMemGetInfo total_mib=%llu free_mib=%llu\n",
           (unsigned long long)total_bytes / MIB, (unsigned long long)free_bytes / MIB);

    rc = hipDeviceTotalMem(&device_total, device);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipDeviceTotalMem: %s\n", hipGetErrorString(rc));
        return 2;
    }
    printf("CAP entry=hipDeviceTotalMem total_mib=%llu\n", (unsigned long long)device_total / MIB);

    /* Through the header's macro map, which is the call a real workload compiles to. */
    rc = hipGetDeviceProperties(&prop600, device);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipGetDeviceProperties: %s\n", hipGetErrorString(rc));
        return 2;
    }
    printf("CAP entry=hipGetDeviceProperties(mapped) total_mib=%llu mpc=%d name=%s\n",
           (unsigned long long)prop600.totalGlobalMem / MIB, prop600.multiProcessorCount,
           prop600.gcnArchName);

    rc = hipGetDevicePropertiesR0600(&prop600, device);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipGetDevicePropertiesR0600: %s\n", hipGetErrorString(rc));
        return 2;
    }
    printf("CAP entry=hipGetDevicePropertiesR0600 total_mib=%llu mpc=%d\n",
           (unsigned long long)prop600.totalGlobalMem / MIB, prop600.multiProcessorCount);

    /* The pre-6.0 entry, called with the pre-6.0 struct. Handing it the 6.0 struct would write 32
     * bytes past where a real pre-6.0 caller expects — corruption in the caller's frame, not a
     * missed quota — and this program exists partly to prove the wrapper takes the right one. */
    rc = hipGetDevicePropertiesR0000(&prop000, device);
    if (rc != hipSuccess) {
        fprintf(stderr, "hip_props_probe: hipGetDevicePropertiesR0000: %s\n", hipGetErrorString(rc));
        return 2;
    }
    printf("CAP entry=hipGetDevicePropertiesR0000 total_mib=%llu mpc=%d\n",
           (unsigned long long)prop000.totalGlobalMem / MIB, prop000.multiProcessorCount);

    return 0;
}
