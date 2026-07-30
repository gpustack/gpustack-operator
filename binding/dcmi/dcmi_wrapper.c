#include "dcmi_wrapper.h"
#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

static void *dcmiLib = NULL;

// The path dcmiLib was opened from, plus whether the vendor's dcmi_init() has since succeeded
// against it. Together they free independent callers in one process from having to be ordered:
// a second w_dcmi_init for the library already held never unloads it, because unloading blanks
// every cached function pointer and would break whatever calls the first caller has in flight.
// The two are tracked separately rather than as one flag: the pointers are resolved before
// dcmi_init() runs and stay usable if it fails, so a failed vendor init must neither report
// success nor be a reason to unload -- it is retried in place instead.
static char dcmiLibPath[1024];
static int dcmiReady = 0;

// Guards dcmiLib, dcmiLibPath, dcmiReady and the load itself, so two concurrent first-time
// initializations cannot both dlopen, nor one read readiness while the other clears it. The
// per-API call path stays lock-free.
static pthread_mutex_t dcmiInitMutex = PTHREAD_MUTEX_INITIALIZER;

// Thread-local cached error string to avoid unrelated dlerror() noise.
static __thread char dcmi_last_err[1024];

static void clear_last_error(void) {
    dcmi_last_err[0] = '\0';
}

static void set_last_error(const char *msg) {
    if (!msg || msg[0] == '\0') {
        dcmi_last_err[0] = '\0';
        return;
    }
    snprintf(dcmi_last_err, sizeof(dcmi_last_err), "%s", msg);
}

static void set_last_errorf(const char *prefix, const char *msg) {
    if (!msg) msg = "unknown dynamic loader error";
    if (!prefix) prefix = "dcmi";
    snprintf(dcmi_last_err, sizeof(dcmi_last_err), "%s: %s", prefix, msg);
}

#define DECL_FUNC_PTR(ret, name, decl_args, call_args) \
    static ret (*name##_func) decl_args = NULL;

DCMI_API_LIST(DECL_FUNC_PTR)

#undef DECL_FUNC_PTR

static int (*dcmi_init_func) (void) = NULL;

#define IMPL_WRAPPER(ret, name, decl_args, call_args) \
    ret w_##name decl_args { \
        if (!name##_func) { \
            return ERROR_FUNCTION_NOT_FOUND; \
        } \
        return name##_func call_args; \
    }

DCMI_API_LIST(IMPL_WRAPPER)

#undef IMPL_WRAPPER

int w_dcmi_init(const char *path)
{
    const char *err = NULL;
    int rc = ERROR_UNKNOWN;

    clear_last_error();

    if (!path || path[0] == '\0') {
        set_last_error("invalid library path");
        return ERROR_LIBRARY_NOT_FOUND;
    }

    pthread_mutex_lock(&dcmiInitMutex);

    // The library asked for is the one already held, so do not unload it under whoever else in
    // this process is calling through its pointers. Only the vendor init can still be missing,
    // and retrying that alone is safe: it touches no pointer.
    if (dcmiLib != NULL && strcmp(dcmiLibPath, path) == 0) {
        rc = SUCCESS;
        if (!dcmiReady) {
            rc = dcmi_init_func();
            if (rc == SUCCESS) {
                dcmiReady = 1;
            }
        }
        goto out;
    }

    // Avoid leaking an existing handle when the library being asked for is not the one open.
    if (dcmiLib) {
        dlclose(dcmiLib);
        dcmiLib = NULL;
    }
    dcmiReady = 0;
    dcmiLibPath[0] = '\0';

    // Reset all cached function pointers before loading.
    #define RESET_API_PTR(ret, name, decl_args, call_args) \
        name##_func = NULL;
    DCMI_API_LIST(RESET_API_PTR)
    #undef RESET_API_PTR
    dcmi_init_func = NULL;

    // Load the library
    dlerror();
    dcmiLib = dlopen(path, RTLD_LAZY | RTLD_GLOBAL);
    err = dlerror();
    if (!dcmiLib) {
        set_last_errorf("dlopen", err);
        rc = ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

    // Load dcmi_init
    dlerror();
    dcmi_init_func = dlsym(dcmiLib, "dcmi_init");
    err = dlerror();
    if (!dcmi_init_func) {
        set_last_errorf("dlsym(dcmi_init)", err);
        dlclose(dcmiLib);
        dcmiLib = NULL;
        rc = ERROR_FUNCTION_NOT_FOUND;
        goto out;
    }

    // Record which library is held, before anything can return with it open. A path too long to
    // store would never compare equal again, so every later init would unload and reload and the
    // guarantee above would disappear without a word; refuse the load instead.
    if (snprintf(dcmiLibPath, sizeof(dcmiLibPath), "%s", path) >= (int)sizeof(dcmiLibPath)) {
        set_last_error("library path is too long to track");
        dcmiLibPath[0] = '\0';
        dlclose(dcmiLib);
        dcmiLib = NULL;
        dcmi_init_func = NULL;
        rc = ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

    // Load all symbols
    #define LOAD_API(ret, name, decl_args, call_args) \
        name##_func = dlsym(dcmiLib, #name);
    DCMI_API_LIST(LOAD_API)
    #undef LOAD_API

    rc = dcmi_init_func();
    if (rc == SUCCESS) {
        dcmiReady = 1;
    }

out:
    pthread_mutex_unlock(&dcmiInitMutex);
    return rc;
}

// w_dcmi_shutdown undoes the guarantee above rather than taking part in it: it unloads the library
// and blanks every cached function pointer, so it is safe only when the caller knows nothing else
// in the process still holds the library. Nothing calls it today -- the Go binding keeps it
// unexported for this reason -- so the asymmetry is documented rather than closed.
//
// TODO: reference count the library before exposing any unload path. Under the same mutex,
// increment on a successful load and on every w_dcmi_init that reuses the library already held,
// decrement here, and dlclose only once the count reaches zero -- returning SUCCESS while it is
// still positive, so a caller releasing its own use is not told the close failed. It has to be a
// count and not a flag because the two callers in one device-manager process, the detector and the
// allocator's container-share driver, initialize this library independently and neither knows of
// the other.
int w_dcmi_shutdown(void)
{
    int rc = SUCCESS;

    clear_last_error();

    pthread_mutex_lock(&dcmiInitMutex);

    if (!dcmiLib) {
        goto out;
    }

    if (dlclose(dcmiLib) != 0) {
        set_last_errorf("dlclose", dlerror());
        rc = ERROR_UNKNOWN;
        goto out;
    }

    dcmiLib = NULL;
    dcmiReady = 0;
    dcmiLibPath[0] = '\0';
    dcmi_init_func = NULL;

    #define RESET_API_PTR(ret, name, decl_args, call_args) \
        name##_func = NULL;
    DCMI_API_LIST(RESET_API_PTR)
    #undef RESET_API_PTR

out:
    pthread_mutex_unlock(&dcmiInitMutex);
    return rc;
}

const char* w_dcmi_last_error(void) {
    return dcmi_last_err[0] ? dcmi_last_err : "";
}
