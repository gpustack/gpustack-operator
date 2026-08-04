/*
 * hgml_dlsym_hook.c — the slice-visibility shim: make a card's VRAM quota, and this
 * container's accounted usage of it, the figures ppu-smi reports for that card.
 *
 * ppu-smi does not link HGML. It dlopen()s libhgml.so and resolves every entry point
 * with dlsym() on that explicit handle, so a preloaded library that merely DEFINES
 * hgmlDeviceGetMemoryInfo is inert: nothing ever looks the name up in the global scope.
 * Interposing dlsym() itself is what works — we see the (handle, symbol) pair, fetch the
 * real function through the caller's own handle, and hand back a wrapper. Confirmed
 * against the 2.1.1 libraries with dladdr, next to testing/hgml_nohook.c, which is the
 * same idea WITHOUT the dlsym hook and therefore the control that proves the point.
 *
 * BOTH PUBLIC GETTERS ARE WRAPPED SEPARATELY, and that is not redundancy: libhgml.so's
 * hgmlDeviceGetMemoryInfo and hgmlDeviceGetMemoryInfo_v2 share a helper that is FUNC LOCAL,
 * so there is no single inner symbol to interpose instead. Their structs differ too — _v2
 * opens with a caller-set .version — so one wrapper could not serve both anyway.
 *
 * THE FIGURES COME FROM WHERE THE QUOTA IS ENFORCED. `total` is the card's configured quota
 * and `used` is common/'s ledger total for that card, so the figure ppu-smi shows and the
 * figure an allocation is admitted against are one number rather than two that drift. Handing
 * back the vendor's own `used` — which counts every container on the card — was the
 * placeholder this replaces: on a shared card it shows a slice as busy while its own quota is
 * untouched. `free` is the remainder, and `used` is clamped to the quota so a slice that
 * overran it (row padding the driver chose, which the enforcement half reports rather than
 * refuses) reads as full instead of leaving `free` to wrap.
 *
 * A card with NO configured figure is left transparent, which is exactly what the enforcement
 * half's own hgMemGetInfo view does: both report the vendor's figures when there is no quota
 * to report instead, so the driver-layer query and ppu-smi agree in every state. Admission is
 * the half that refuses; reporting stays a report. A container-wide misconfiguration is
 * announced once at load by common/, at the denial level.
 *
 * Per card, not per container. Unlike the allocation entries, the getters are handed the
 * device they are being asked about, so the card is known — it just has to be turned into
 * an index, which is what hgmlDeviceGetIndex is for. That index is resolved through the
 * caller's own dlopen handle, the same way the getters are, so this shim still links
 * nothing but libc.
 *
 * TWO GUARDS, because a preloaded dlsym hook is never the only thing in a container: the vendor
 * ships libhggc_wrapper.so, and a monitoring agent may bring an interposer of its own.
 *   - RE-ENTRANCY, per thread. While this thread is already inside an interception we forward
 *     without wrapping, so a caller that arrives back here mid-resolution receives the vendor's
 *     pointer rather than ours. Handing anyone a pointer to our own wrapper is what a call chain
 *     that alternates between two libraries is made of.
 *   - ORIGIN, per pointer. We never wrap an address that lives in this object. The pointers
 *     below are per process while the guard above is per thread, so a resolution on a second
 *     thread could otherwise store our own wrapper as `real_*` — and a wrapper whose real
 *     function is itself is an unbounded recursion the next query walks into.
 *
 * MEASURED, and it is not what the module design assumed: two libraries that interpose dlsym
 * this way do not chain through each other at all. The lookup below asks for dlsym under an
 * explicit GLIBC_ version — it has to, since calling dlsym by name here would call this
 * function — and a versioned lookup does not match an unversioned definition in an object that
 * carries a version table, which every one of these does from its libc imports. So RTLD_NEXT
 * steps over any peer straight to libc, in either preload order. Two consequences:
 *   - a peer cannot recurse back in through this chain, which is a stronger answer than the
 *     re-entrancy guard was written for. The guard stays: it is one thread-local test on a path
 *     that is not hot, and it is the only thing covering a caller that reaches here by some
 *     other route;
 *   - THIS SHIM IS INERT BEHIND ANOTHER DLSYM INTERPOSER, because the loader gives the symbol
 *     to whoever is preloaded first and that one's chain never reaches us. The injection
 *     contract therefore has an ordering constraint — our preload comes first — and it is
 *     nothing this library can enforce from the inside. cases/thead-case-2.sh pins both
 *     directions so the constraint is a checked fact rather than folklore.
 *
 * Links nothing but libc on purpose — the workload container brings its own SDK, so
 * dlsym/dlvsym stay undefined here and resolve from whatever glibc that container has.
 *
 * Still to build: an optional UKI fallback (ppuGetDeviceRuntimeInfo) for callers that bypass
 * HGML. The function pointers below live in globals with no lock, which holds while the only
 * caller is a single-threaded SMI tool. They are also one set per PROCESS rather than per
 * dlopen handle, so a process that opened two different libhgml copies would route both
 * through the last one resolved — theoretical in a container carrying one SDK.
 */
#define _GNU_SOURCE

#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

/* hgml.h carries zero #include lines: it provides neither NULL nor the bool its own
 * declarations use, so a consumer has to supply both before including it. */
#include <stdbool.h>
#include <stddef.h>

#include <hgml.h>

#include "common/vppu_ledger.h"
#include "common/vppu_quota.h"

static void *(*real_dlsym)(void *handle, const char *symbol);

/* The real getters, as resolved through the handle the caller passed to dlsym(). */
static hgmlReturn_t (*real_get_memory_info)(hgmlDevice_t device, hgmlMemory_t *memory);
static hgmlReturn_t (*real_get_memory_info_v2)(hgmlDevice_t device, hgmlMemory_v2_t *memory);

/* Resolved alongside them, through the same handle: without it a per-card quota cannot be
 * applied, and applying one card's figure to every card would be worse than not
 * intercepting at all. */
static hgmlReturn_t (*real_get_index)(hgmlDevice_t device, unsigned int *index);

/* in_dlsym — this thread is already inside an interception.
 *
 * initial-exec because the default TLS model resolves through the dynamic linker, which would
 * put a second entry in DT_NEEDED and break the "nothing but libc.so.6" guarantee case 1
 * asserts. Legitimate here: this library arrives by preload, so it is in the initial set. */
static __thread bool in_dlsym __attribute__((tls_model("initial-exec")));

/* device_index — which card this handle is, or -1 if that cannot be established. */
static int device_index(hgmlDevice_t device)
{
    if (real_get_index == NULL) {
        return -1;
    }

    unsigned int index = 0;
    if (real_get_index(device, &index) != HGML_SUCCESS) {
        return -1;
    }
    if (index >= (unsigned int)VPPU_MAX_DEVICES) {
        return -1;
    }
    return (int)index;
}

/* apply_slice — the slice's figures in place of the vendor's, for the card at `index`.
 *
 * The ledger read takes no lock, by that module's design: a total one allocation stale is
 * worth far more in a reporting path than a query that can block behind a vendor allocation
 * that has hung. An unreachable region reads as zero used — common/ has already said why at
 * the denial level, and inventing a figure here would be worse than a low one. */
static void apply_slice(int index, unsigned long long quota, unsigned long long *total,
                        unsigned long long *used, unsigned long long *available)
{
    unsigned long long accounted = vppu_ledger_used(index);

    if (accounted > quota) {
        accounted = quota;
    }
    *total = quota;
    *used = accounted;
    *available = quota - accounted;
}

/* slice_of — the card this call is about and its configured quota, or false to leave the
 * vendor's own figures alone. */
static bool slice_of(hgmlDevice_t device, int *index_out, unsigned long long *quota_out)
{
    int index = device_index(device);
    if (index < 0) {
        return false;
    }

    unsigned long long quota = vppu_quota_memory_bytes(index);
    if (quota == 0ULL) {
        return false;
    }

    *index_out = index;
    *quota_out = quota;
    return true;
}

static hgmlReturn_t hook_get_memory_info(hgmlDevice_t device, hgmlMemory_t *memory)
{
    if (real_get_memory_info == NULL) {
        return HGML_ERROR_FUNCTION_NOT_FOUND;
    }

    hgmlReturn_t rc = real_get_memory_info(device, memory);
    if (rc == HGML_SUCCESS && memory != NULL) {
        int index = -1;
        unsigned long long quota = 0ULL;

        if (slice_of(device, &index, &quota)) {
            apply_slice(index, quota, &memory->total, &memory->used, &memory->free);
        }
    }
    return rc;
}

static hgmlReturn_t hook_get_memory_info_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory)
{
    if (real_get_memory_info_v2 == NULL) {
        return HGML_ERROR_FUNCTION_NOT_FOUND;
    }

    /* The caller writes .version before the call and the API contract returns it
     * unchanged, so the wrapper must not touch it: rewriting it would break the
     * caller's struct-version negotiation, and restoring the caller's own value would
     * mask a mismatch the vendor is reporting. .reserved is left alone for the same
     * reason — it is a driver figure, not part of the quota. */
    hgmlReturn_t rc = real_get_memory_info_v2(device, memory);
    if (rc == HGML_SUCCESS && memory != NULL) {
        int index = -1;
        unsigned long long quota = 0ULL;

        if (slice_of(device, &index, &quota)) {
            apply_slice(index, quota, &memory->total, &memory->used, &memory->free);
        }
    }
    return rc;
}

/* resolve_real_dlsym — which library carries dlsym, and under which version tag,
 * depends on the glibc of the container this shim is loaded INTO rather than the one
 * it was built on: glibc 2.34 moved dlsym into libc.so.6 as GLIBC_2.34, before that
 * it lived in libdl.so.2 as GLIBC_2.2.5. Try both. */
static void resolve_real_dlsym(void)
{
    if (real_dlsym != NULL) {
        return;
    }

    real_dlsym = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    if (real_dlsym == NULL) {
        real_dlsym = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
    }
}

/* is_own_address — does this pointer live in the object this code is part of?
 *
 * Answered against one of our own wrappers rather than a linker-supplied base, so it holds
 * however the object was mapped. A pointer dladdr cannot place counts as ours: declining to
 * intercept reports a wrong figure loudly, where wrapping our own wrapper recurses until the
 * stack ends. */
static bool is_own_address(void *address)
{
    Dl_info mine;
    Dl_info theirs;

    if (dladdr((void *)hook_get_memory_info, &mine) == 0 || dladdr(address, &theirs) == 0) {
        return true;
    }
    return mine.dli_fbase == theirs.dli_fbase;
}

/* can_intercept — is a per-card rewrite possible for this handle?
 *
 * Only once the index lookup is in hand: a wrapper that cannot tell one card from another
 * would have to apply a single figure to all of them, which on a container holding several
 * cards reports the wrong number for every card but one. Declining to intercept is the
 * honest failure, and it says so rather than reporting a plausible wrong figure. */
static bool can_intercept(void *handle, const char *symbol)
{
    if (real_get_index == NULL) {
        real_get_index = real_dlsym(handle, "hgmlDeviceGetIndex");
    }
    if (real_get_index == NULL) {
        vppu_log(VPPU_LOG_DENY,
                 "not intercepting %s: hgmlDeviceGetIndex unavailable, so a per-card quota "
                 "cannot be applied\n",
                 symbol);
        return false;
    }

    vppu_log(VPPU_LOG_DEBUG, "intercepted dlsym(%s)\n", symbol);
    return true;
}

/* intercept — resolve one memory getter through the caller's own handle, then decide between
 * handing back a wrapper and handing back what the chain returned. */
static void *intercept(void *handle, const char *symbol, bool v2)
{
    void *real = real_dlsym(handle, symbol);

    if (real == NULL) {
        return NULL;
    }
    if (is_own_address(real)) {
        /* Already wrapped by this object — on another thread, or through a peer that resolved
         * the same symbol a second way. Wrapping it again would make a wrapper its own real
         * function. */
        vppu_log(VPPU_LOG_DEBUG,
                 "dlsym(%s) already resolves into this shim, not wrapping it twice\n", symbol);
        return real;
    }
    if (!can_intercept(handle, symbol)) {
        return real;
    }

    if (v2) {
        real_get_memory_info_v2 = real;
        return (void *)hook_get_memory_info_v2;
    }
    real_get_memory_info = real;
    return (void *)hook_get_memory_info;
}

void *dlsym(void *handle, const char *symbol)
{
    resolve_real_dlsym();
    if (real_dlsym == NULL) {
        return NULL;
    }

    /* The re-entrancy guard: forwarded, not intercepted, while this thread is already inside
     * an interception. See the two-guard note at the top — a peer that resolves through the
     * global scope arrives back here, and wrapping that inner resolution is what would hand it
     * a pointer into this object. */
    if (in_dlsym) {
        return real_dlsym(handle, symbol);
    }

    /* No NULL check on `symbol`: glibc declares it __nonnull, so the comparison is dead
     * code the compiler rejects under -Wnonnull-compare. */
    bool v2 = strcmp(symbol, "hgmlDeviceGetMemoryInfo_v2") == 0;
    if (!v2 && strcmp(symbol, "hgmlDeviceGetMemoryInfo") != 0) {
        return real_dlsym(handle, symbol);
    }

    in_dlsym = true;
    void *wrapper = intercept(handle, symbol, v2);
    in_dlsym = false;
    return wrapper;
}

/* Announce the load on stderr: Gate 1's arms are decided by parsed output, and an arm
 * whose library silently failed to load must not be mistaken for one that loaded and
 * did nothing. */
__attribute__((constructor)) static void announce(void)
{
    /* No figure for any card, in a shim that is only preloaded into sliced containers, is a
     * misconfiguration rather than a state to report physical figures for. common/ makes that
     * report, at the denial level, so turning the shim quiet does not turn it off. */
    vppu_quota_validate();

    vppu_log(VPPU_LOG_DEBUG, "hgml_dlsym_hook loaded, per-card %s<i>\n",
             VPPU_ENV_MEMORY_LIMIT_PREFIX);
}
