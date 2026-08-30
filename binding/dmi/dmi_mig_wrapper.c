#include "dmi_mig_wrapper.h"
#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

// Why this wrapper exists at all.
//
// The vendor exports its Multi-Instance API under NVML's symbol names -- nvmlDeviceGetCount,
// nvmlDeviceGetMigMode, and so on -- while implementing something else entirely: the return enum is
// NVML's, but nvmlGpuInstanceProfileInfo_t has a different layout and a different field set. The
// generated bindings in this repository call their C functions by name and binding/types.go opens
// libraries with RTLD_GLOBAL, so a binding generated straight from this header would emit calls to
// the very symbols libnvidia-ml.so also defines. In a process that has loaded both, one package's
// calls would land in the other package's library, silently, with structs that do not agree.
//
// So nothing here references those symbols at link time. The library is opened with RTLD_LOCAL,
// which keeps its nvml* names out of the global namespace, every function is reached through a
// pointer this file resolved with dlsym against that one handle, and Go only ever calls the
// w_-prefixed wrappers. The collision is made impossible rather than unlikely.

static void *migLib = NULL;

// The path migLib was opened from. It is what makes a second w_dmi_mig_init for the library already
// held a no-op: unloading would blank every cached pointer under whatever calls the first caller has
// in flight. The detector and the allocator initialize this library independently and neither knows
// of the other, so they must not have to be ordered.
static char migLibPath[1024];

// Guards migLib, migLibPath and the load itself, so two concurrent first-time initializations cannot
// both dlopen. The per-API call path stays lock-free.
static pthread_mutex_t migInitMutex = PTHREAD_MUTEX_INITIALIZER;

// Thread-local cached error string, so an unrelated dlerror() elsewhere cannot be read as ours.
static __thread char mig_last_err[1024];

static void clear_last_error(void) { mig_last_err[0] = '\0'; }

static void set_last_error(const char *msg) {
    if (!msg || msg[0] == '\0') {
        mig_last_err[0] = '\0';
        return;
    }
    snprintf(mig_last_err, sizeof(mig_last_err), "%s", msg);
}

static void set_last_errorf(const char *prefix, const char *msg) {
    if (!msg) msg = "unknown dynamic loader error";
    if (!prefix) prefix = "dmi_mig";
    snprintf(mig_last_err, sizeof(mig_last_err), "%s: %s", prefix, msg);
}

#define DECL_FUNC_PTR(ret, name, decl_args, call_args) \
    static ret (*name##_func) decl_args = NULL;

DMI_MIG_API_LIST(DECL_FUNC_PTR)

#undef DECL_FUNC_PTR

// A function the library does not export leaves its pointer NULL and is reported as
// NVML_ERROR_FUNCTION_NOT_FOUND rather than crashing. That is a real case, not a defensive one:
// nvmlDeviceGetUtilizationRates is absent from the vendor header, so a future driver that drops it
// is indistinguishable from one that never had it, and both have to be survivable.
#define IMPL_WRAPPER(ret, name, decl_args, call_args)  \
    ret w_##name decl_args {                           \
        if (!name##_func) {                            \
            return NVML_ERROR_FUNCTION_NOT_FOUND;      \
        }                                              \
        return name##_func call_args;                  \
    }

DMI_MIG_API_LIST(IMPL_WRAPPER)

#undef IMPL_WRAPPER

nvmlReturn_t w_dmi_mig_init(const char *path) {
    const char *err = NULL;
    nvmlReturn_t rc = NVML_ERROR_UNKNOWN;

    clear_last_error();

    if (!path || path[0] == '\0') {
        set_last_error("invalid library path");
        return NVML_ERROR_LIBRARY_NOT_FOUND;
    }

    pthread_mutex_lock(&migInitMutex);

    // Already holding exactly this library: succeed without touching it.
    if (migLib != NULL && strcmp(migLibPath, path) == 0) {
        rc = NVML_SUCCESS;
        goto out;
    }

    // A different library was held; release it before replacing it.
    if (migLib) {
        dlclose(migLib);
        migLib = NULL;
    }
    migLibPath[0] = '\0';

#define RESET_API_PTR(ret, name, decl_args, call_args) name##_func = NULL;
    DMI_MIG_API_LIST(RESET_API_PTR)
#undef RESET_API_PTR

    // RTLD_LOCAL is the load-bearing flag, not a default: with RTLD_GLOBAL the vendor's nvml* names
    // would join the process-wide namespace and every other binding's unresolved nvml* reference
    // could bind to them.
    dlerror();
    migLib = dlopen(path, RTLD_LAZY | RTLD_LOCAL);
    err = dlerror();
    if (!migLib) {
        set_last_errorf("dlopen", err);
        rc = NVML_ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

    // Record which library is held before anything can return with it open. A path too long to store
    // would never compare equal again, so every later init would unload and reload and the
    // no-ordering guarantee above would disappear without a word; refuse the load instead.
    if (snprintf(migLibPath, sizeof(migLibPath), "%s", path) >= (int)sizeof(migLibPath)) {
        set_last_error("library path is too long to track");
        migLibPath[0] = '\0';
        dlclose(migLib);
        migLib = NULL;
        rc = NVML_ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

#define LOAD_API(ret, name, decl_args, call_args) name##_func = dlsym(migLib, #name);
    DMI_MIG_API_LIST(LOAD_API)
#undef LOAD_API

    // One symbol has to resolve for the library to be worth holding. Requiring all of them would
    // refuse a driver over a single function it does not serve, and the wrapper already reports a
    // missing one per call; requiring none would report success for a shared object that is not this
    // API at all.
    if (!nvmlGetSystemMigMode_func && !nvmlDeviceGetCount_func) {
        set_last_error("library exports neither nvmlGetSystemMigMode nor nvmlDeviceGetCount");
        dlclose(migLib);
        migLib = NULL;
        migLibPath[0] = '\0';
        rc = NVML_ERROR_FUNCTION_NOT_FOUND;
        goto out;
    }

    rc = NVML_SUCCESS;

out:
    pthread_mutex_unlock(&migInitMutex);
    return rc;
}

// w_dmi_mig_shutdown undoes the reuse guarantee above rather than taking part in it: it unloads the
// library and blanks every cached pointer, so it is safe only when the caller knows nothing else in
// the process still holds it. The Go binding keeps it unexported for that reason.
//
// TODO: reference count the library before exposing any unload path -- increment under the mutex on
// a successful load and on every init that reuses the library already held, decrement here, and
// dlclose only at zero, returning success while the count is still positive so a caller releasing
// its own use is not told the close failed.
nvmlReturn_t w_dmi_mig_shutdown(void) {
    nvmlReturn_t rc = NVML_SUCCESS;

    clear_last_error();

    pthread_mutex_lock(&migInitMutex);

    if (!migLib) {
        goto out;
    }

    if (dlclose(migLib) != 0) {
        set_last_errorf("dlclose", dlerror());
        rc = NVML_ERROR_UNKNOWN;
        goto out;
    }

    migLib = NULL;
    migLibPath[0] = '\0';

#define RESET_API_PTR(ret, name, decl_args, call_args) name##_func = NULL;
    DMI_MIG_API_LIST(RESET_API_PTR)
#undef RESET_API_PTR

out:
    pthread_mutex_unlock(&migInitMutex);
    return rc;
}

const char *w_dmi_mig_last_error(void) {
    return mig_last_err[0] ? mig_last_err : "";
}
