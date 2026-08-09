/*
 * hip_resolve.h — how a wrapper reaches the real entry point, and how it says who called it.
 *
 * TWO LOOKUPS, AND NEVER A FABRICATED RETURN CODE. `RTLD_NEXT` finds nothing when the framework
 * `dlopen()`s `libamdhip64` instead of linking it — PyTorch does exactly that — so the handle
 * lookup is not a belt-and-braces extra, it is the path real workloads take. And a resolve miss
 * must abort rather than return: the natural placeholder `1` **is** `hipErrorInvalidValue`, so
 * returning it turns "we could not find the symbol" into "the runtime rejected your arguments",
 * which is a false trail that costs an afternoon. That mistake was made once, during the
 * mechanism comparison, and it produced a convincing proof that preloading breaks ROCm.
 *
 * THE LOADER CALLS BELOW ARE VERSION-PINNED IN hip_resolve.c. glibc moved `libdl` into `libc` at
 * GLIBC_2.34, so `dlopen`, `dlsym` and `dladdr` bind at that version on any modern build host and
 * become the only symbols in the product above the GLIBC_2.4 floor. Three `.symver` directives
 * hold the floor; `build.sh check` is what proves they are still there.
 */
#ifndef VROCM_HIP_HIP_RESOLVE_H
#define VROCM_HIP_HIP_RESOLVE_H

#include "common/vrocm.h"

/* vrocm_resolve — the real entry point behind one name, or abort. Never returns NULL. */
VROCM_INTERNAL void *vrocm_resolve(const char *name);

/* vrocm_caller_of — the object that made a call, from a return address, or NULL.
 *
 * A preload cannot decline to fire on a call the HIP runtime makes into its own exported symbol,
 * so the mitigation is visibility rather than avoidance. Measured today the exposure is nil —
 * across a PyTorch run to OOM, every call into these wrappers came from the framework's objects
 * and none from `libamdhip64` — and this is what turns a change in that into a log line instead
 * of a double charge nobody can explain. */
VROCM_INTERNAL const char *vrocm_caller_of(void *return_address);

/* VROCM_REAL — the real function pointer behind one entry, resolved once per call site.
 *
 * Concurrent first calls do race, and every racing thread resolves the same name to the same
 * address, so no lock is needed and one here would serialise every interposed call to buy
 * nothing. The cache is still read and written through relaxed atomics rather than as a plain
 * object: a data race is undefined behaviour whatever the values involved, and the wrappers this
 * expands into are loaded into frameworks that run dozens of threads. Relaxed is all the ordering
 * this needs -- there is nothing to publish alongside the pointer -- and it costs a plain load on
 * every architecture ROCm runs on.
 *
 * `real_##fn` is a LOCAL copy, so the call site that follows uses a value no other thread can be
 * writing, and every wrapper keeps the name it already used.
 */
#define VROCM_REAL(fn, type)                                                             \
    static type real_##fn##_cache;                                                       \
    type real_##fn = __atomic_load_n(&real_##fn##_cache, __ATOMIC_RELAXED);              \
    if (real_##fn == NULL) {                                                             \
        real_##fn = (type)vrocm_resolve(#fn);                                            \
        __atomic_store_n(&real_##fn##_cache, real_##fn, __ATOMIC_RELAXED);               \
    }

#endif /* VROCM_HIP_HIP_RESOLVE_H */
