#define _GNU_SOURCE

#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>

#include "common/vrocm_log.h"
#include "hip/hip_resolve.h"

/* THE THREE PINS THAT HOLD THE GLIBC FLOOR.
 *
 * glibc 2.34 merged libdl into libc, so on any build host at or above that version these three
 * symbols bind at GLIBC_2.34 — and measured, they are the ONLY symbols in the whole product above
 * GLIBC_2.4. Pinning them to the versions that predate the merge keeps the artifact loadable in
 * workload images built on Ubuntu 20.04 and RHEL 8.
 *
 * `dladdr` is the one to forget, because it arrives with the caller-origin diagnostic rather than
 * with the resolver: the floor was clean until the diagnostic was added, and adding it silently
 * put the ceiling back to 2.34. `build.sh check` is what catches the next one. */
__asm__(".symver dlopen,dlopen@GLIBC_2.2.5");
__asm__(".symver dlsym,dlsym@GLIBC_2.2.5");
__asm__(".symver dladdr,dladdr@GLIBC_2.2.5");

/* The soname rather than the linker name: a workload image carries `libamdhip64.so.6` or `.so.7`
 * and only a devel image carries the unversioned symlink, so asking for the plain name would miss
 * on exactly the images this library is preloaded into. RTLD_NOLOAD means we never LOAD the
 * runtime — if it is not already mapped, there is nothing to interpose and nothing to find. */
static const char *const HIP_SONAMES[] = {
    "libamdhip64.so.7",
    "libamdhip64.so.6",
    "libamdhip64.so",
};

/* THE ANSWER IS PUBLISHED AFTER IT EXISTS, and that ordering is the whole point of the shape
 * below. Setting `tried` first leaves a window in which another thread reads "already resolved"
 * and a handle that is still NULL, and `vrocm_resolve` treats an unresolvable name as fatal -- so
 * a race on the first call through this path would abort the workload. Two threads may both
 * dlopen; RTLD_NOLOAD only takes a reference on a library that is already mapped, and they get
 * the same handle, so the cost of losing that race is a reference this process never drops
 * anyway. */
static void *hip_handle(void)
{
    static void *handle;
    static int tried;
    void *found = NULL;
    size_t i;

    if (__atomic_load_n(&tried, __ATOMIC_ACQUIRE)) {
        return __atomic_load_n(&handle, __ATOMIC_RELAXED);
    }
    for (i = 0; i < sizeof(HIP_SONAMES) / sizeof(HIP_SONAMES[0]); i++) {
        found = dlopen(HIP_SONAMES[i], RTLD_NOLOAD | RTLD_LAZY);
        if (found != NULL) {
            vrocm_log(VROCM_LOG_DEBUG, "resolving through %s\n", HIP_SONAMES[i]);
            break;
        }
    }
    /* Atomic even though the release below already orders this against every reader: TWO threads
     * can arrive here, because nothing elects one, and two plain stores to the same object are a
     * data race whatever they store -- these two store the same handle, and that does not make it
     * defined. Relaxed is enough; `tried` carries the ordering. */
    __atomic_store_n(&handle, found, __ATOMIC_RELAXED);
    __atomic_store_n(&tried, 1, __ATOMIC_RELEASE);
    return found;
}

VROCM_INTERNAL void *vrocm_resolve(const char *name)
{
    void *sym = dlsym(RTLD_NEXT, name);

    if (sym == NULL) {
        void *handle = hip_handle();

        if (handle != NULL) {
            sym = dlsym(handle, name);
        }
    }
    if (sym == NULL) {
        /* Loud and terminal, on purpose. Every alternative is worse: returning a status invents a
         * runtime error the runtime never raised, and returning success hands the caller an
         * uninitialised pointer. A wrapper that cannot find what it wraps has no correct answer.
         *
         * Written directly rather than through vrocm_log(), because this one line must survive
         * LIBVROCM_LOG_LEVEL=0: a process that is about to abort owes its operator the reason. */
        fprintf(stderr, VROCM_TAG "cannot resolve %s; aborting rather than inventing a status\n",
                name);
        abort();
    }
    return sym;
}

VROCM_INTERNAL const char *vrocm_caller_of(void *return_address)
{
    Dl_info info;

    if (return_address == NULL || dladdr(return_address, &info) == 0) {
        return NULL;
    }
    return info.dli_fname;
}
