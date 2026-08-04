/*
 * dlsym_stack.c — a SECOND dlsym-interposing library, so the visibility shim can be shown to
 * survive being stacked with a peer rather than assumed to.
 *
 * Gate 1's arm (c) preloads the vendor's libhggc_wrapper.so beside the hook. That proves one
 * peer in one order, which is not the same as proving no order recurses: the vendor's wrapper
 * does not interpose dlsym at all, so it never lands in the hook's own chain. This peer does
 * take dlsym, and the answer it produced is worth more than the one it was written for.
 *
 * What it is, precisely, because a straw man would prove nothing:
 *   - a LEGAL interposer. It takes dlsym, resolves the next one with dlvsym(RTLD_NEXT, ...) —
 *     the way the product shim must, since calling dlsym by name inside a dlsym hook calls the
 *     hook — and wraps the two HGML memory getters with pass-through wrappers, so it is in the
 *     CALL chain and not only in the resolution chain;
 *   - with one realistic detail. Once per symbol it also resolves that symbol through the GLOBAL
 *     scope, which is how a library looks up something it did not link, and reports via dladdr
 *     which object the pointer it got back lives in. That pointer must never live in
 *     hgml_dlsym_hook.so: handing a peer a pointer to the hook's own wrapper is what a call chain
 *     alternating between two libraries is made of.
 *
 * WHAT IT MEASURED. The two libraries do not chain through each other in either order. A
 * versioned dlvsym lookup does not match an unversioned definition in an object that carries a
 * version table — both of these do, from their libc imports — so each one's RTLD_NEXT steps over
 * the other and lands on libc. Whoever is preloaded first owns dlsym; the other is loaded,
 * initialised and never entered. cases/thead-case-2.sh asserts exactly that, in both orders,
 * which is how the hook's ordering constraint stopped being an assumption.
 *
 * The global lookup is MEMOISED, and the memo is set before the call rather than after: an
 * interposer that re-enters the global scope unconditionally recurses through itself, hangs on
 * its own account, and would say nothing about anyone else's guard.
 *
 * Links nothing but libc, like every other artifact here — dlvsym and dladdr resolve from
 * whatever glibc the container has.
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

#define STACK_TAG "[stack] "

static void *(*real_dlsym)(void *handle, const char *symbol);

/* The dlsym the GLOBAL scope resolves to — the first interposer loaded, whenever anything is
 * preloaded at all. Looked up explicitly rather than called by name, so what this file
 * exercises does not depend on whether a call to a symbol this object itself defines goes
 * through the PLT. */
static void *(*global_dlsym)(void *handle, const char *symbol);

static hgmlReturn_t (*real_get_memory_info)(hgmlDevice_t device, hgmlMemory_t *memory);
static hgmlReturn_t (*real_get_memory_info_v2)(hgmlDevice_t device, hgmlMemory_v2_t *memory);

static void resolve_chain(void)
{
    if (real_dlsym == NULL) {
        /* glibc 2.34 moved dlsym into libc.so.6; before that it lived in libdl.so.2. */
        real_dlsym = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
        if (real_dlsym == NULL) {
            real_dlsym = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
        }
    }
    if (global_dlsym == NULL && real_dlsym != NULL) {
        global_dlsym = real_dlsym(RTLD_DEFAULT, "dlsym");
    }
}

static hgmlReturn_t wrap_get_memory_info(hgmlDevice_t device, hgmlMemory_t *memory)
{
    if (real_get_memory_info == NULL) {
        return HGML_ERROR_FUNCTION_NOT_FOUND;
    }
    return real_get_memory_info(device, memory);
}

static hgmlReturn_t wrap_get_memory_info_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory)
{
    if (real_get_memory_info_v2 == NULL) {
        return HGML_ERROR_FUNCTION_NOT_FOUND;
    }
    return real_get_memory_info_v2(device, memory);
}

/* note_chain — resolve one symbol through the global scope, once, and report where it landed. */
static void note_chain(void *handle, const char *symbol, bool *noted)
{
    if (*noted || global_dlsym == NULL) {
        return;
    }
    *noted = true;

    void *fn = global_dlsym(handle, symbol);
    Dl_info info;
    const char *origin = "unresolved";

    if (fn != NULL && dladdr(fn, &info) != 0 && info.dli_fname != NULL) {
        origin = info.dli_fname;
    }
    fprintf(stderr, STACK_TAG "chained %s origin=%s\n", symbol, origin);
}

void *dlsym(void *handle, const char *symbol)
{
    static bool noted_v1;
    static bool noted_v2;

    resolve_chain();
    if (real_dlsym == NULL) {
        return NULL;
    }

    bool v2 = strcmp(symbol, "hgmlDeviceGetMemoryInfo_v2") == 0;
    if (!v2 && strcmp(symbol, "hgmlDeviceGetMemoryInfo") != 0) {
        return real_dlsym(handle, symbol);
    }

    note_chain(handle, symbol, v2 ? &noted_v2 : &noted_v1);

    void *real = real_dlsym(handle, symbol);
    if (real == NULL) {
        return NULL;
    }

    fprintf(stderr, STACK_TAG "wrapped %s\n", symbol);
    if (v2) {
        real_get_memory_info_v2 = real;
        return (void *)wrap_get_memory_info_v2;
    }
    real_get_memory_info = real;
    return (void *)wrap_get_memory_info;
}

__attribute__((constructor)) static void announce(void)
{
    fprintf(stderr, STACK_TAG "dlsym_stack loaded, a second dlsym interposer\n");
}
