/*
 * hgml_nohook.c — Gate 1's negative control: the same HGML symbols, no dlsym hook.
 *
 * This exists to make Gate 1's result unambiguous. Preloaded, it exports
 * hgmlDeviceGetMemoryInfo and hgmlDeviceGetMemoryInfo_v2 exactly as hgml_dlsym_hook.c
 * does, but it does not interpose dlsym() — so if ppu-smi still reports the physical
 * card size while this is loaded, the "defining the symbols alone is inert" claim
 * holds, and the hook arm's success is the hook rather than a coincidence.
 *
 * Two properties make it a control rather than a way to pass for the wrong reason:
 * it announces its own load, because a library that silently failed to load also
 * shows the physical value; and its symbol bodies print a marker if they are ever
 * entered, because that would mean something DOES resolve HGML through the global
 * scope and the premise of the whole design is wrong.
 *
 * Links nothing but libc, same as the hook shim.
 */
#include <stdio.h>
#include <stdlib.h>

/* hgml.h carries zero #include lines: it provides neither NULL nor the bool its own
 * declarations use, so a consumer has to supply both before including it. */
#include <stdbool.h>
#include <stddef.h>

#include <hgml.h>

#define VPPU_ENV_MEMORY_LIMIT_MIB "VPPU_DEVICE_MEMORY_LIMIT_MIB"
#define VPPU_TAG "[vppu] "

/* quota_bytes — duplicated from hgml_dlsym_hook.c on purpose: each artifact here stays
 * one self-contained translation unit so its linkage assertions are its own. */
static unsigned long long quota_bytes(void)
{
    const char *value = getenv(VPPU_ENV_MEMORY_LIMIT_MIB);
    if (value == NULL || *value == '\0') {
        return 0ULL;
    }

    char *end = NULL;
    unsigned long long mib = strtoull(value, &end, 10);
    if (end == value || mib == 0ULL) {
        return 0ULL;
    }
    return mib * 1024ULL * 1024ULL;
}

hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory)
{
    (void)device;

    fprintf(stderr, VPPU_TAG "hgml_nohook: hgmlDeviceGetMemoryInfo CALLED unexpectedly\n");
    if (memory == NULL) {
        return HGML_ERROR_INVALID_ARGUMENT;
    }

    unsigned long long quota = quota_bytes();
    memory->total = quota;
    memory->used = 0ULL;
    memory->free = quota;
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetMemoryInfo_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory)
{
    (void)device;

    fprintf(stderr, VPPU_TAG "hgml_nohook: hgmlDeviceGetMemoryInfo_v2 CALLED unexpectedly\n");
    if (memory == NULL) {
        return HGML_ERROR_INVALID_ARGUMENT;
    }

    /* .version is the caller's and comes back unchanged; .reserved is a driver figure. */
    unsigned long long quota = quota_bytes();
    memory->total = quota;
    memory->used = 0ULL;
    memory->free = quota;
    return HGML_SUCCESS;
}

__attribute__((constructor)) static void announce(void)
{
    fprintf(stderr, VPPU_TAG "hgml_nohook loaded, quota=%llu bytes, dlsym NOT hooked\n",
            quota_bytes());
}
