#include "dcmi_wrapper.h"
#include <dlfcn.h>
#include <pthread.h>
#include <stdatomic.h>
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

// Which generation's init answered for the library currently held. Written only under the mutex
// above, but declared _Atomic because it is read on every adapted query in the Go layer: taking the
// init mutex there would serialize the whole per-API call path, which is otherwise lock-free.
static _Atomic int dcmiApiVersion = API_VERSION_UNKNOWN;

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
DCMI_V2_API_LIST(DECL_FUNC_PTR)

#undef DECL_FUNC_PTR

// Neither generation's init goes through the macro lists: the sequence below is the one place that
// has to know which generation answered, and the macro lists exist precisely so that no other
// wrapper does.
static int (*dcmi_init_func) (void) = NULL;
static int (*dcmiv2_init_func) (void) = NULL;

// vendor_init runs the V1 init and, only if its answer means this driver does not serve V1 at all,
// the V2 one. It records which generation answered, and is the only writer of dcmiApiVersion.
//
// The fallback keys on the two refusal codes rather than on "anything other than SUCCESS". A driver
// serving V2 exports dcmi_init and refuses it with ERROR_NOT_SUPPORT, so a refusal is the signal;
// a transient V1 failure -- a device still resetting, a busy driver, a timeout -- is not, and must
// not latch the process onto V2 for the rest of its life. ERROR_FUNCTION_NOT_FOUND is the second
// code because a library need not export both inits, and an unresolved pointer is reported with
// exactly the code IMPL_WRAPPER uses for one.
//
// Called with dcmiInitMutex held.
static int vendor_init(void)
{
    int rc = dcmi_init_func ? dcmi_init_func() : ERROR_FUNCTION_NOT_FOUND;
    int v2rc = SUCCESS;
    char msg[128];

    if (rc == SUCCESS) {
        dcmiApiVersion = API_VERSION_V1;
        return rc;
    }
    if (rc != ERROR_NOT_SUPPORT && rc != ERROR_FUNCTION_NOT_FOUND) {
        return rc;
    }

    v2rc = dcmiv2_init_func ? dcmiv2_init_func() : ERROR_FUNCTION_NOT_FOUND;
    if (v2rc == SUCCESS) {
        dcmiApiVersion = API_VERSION_V2;
        return v2rc;
    }

    // Neither generation serves this driver, so report the V1 code: that is the one an operator on
    // a V1 fleet has to act on, while the V2 code says only that a generation they do not run also
    // refused. The V2 code is not lost -- it goes out through the error string, which is what the
    // Go binding logs beside the library path.
    snprintf(msg, sizeof(msg), "dcmi_init: %d, dcmiv2_init: %d", rc, v2rc);
    set_last_error(msg);

    return rc;
}

#define IMPL_WRAPPER(ret, name, decl_args, call_args) \
    ret w_##name decl_args { \
        if (!name##_func) { \
            return ERROR_FUNCTION_NOT_FOUND; \
        } \
        return name##_func call_args; \
    }

DCMI_API_LIST(IMPL_WRAPPER)
DCMI_V2_API_LIST(IMPL_WRAPPER)

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
            rc = vendor_init();
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
    dcmiApiVersion = API_VERSION_UNKNOWN;

    // Reset all cached function pointers before loading.
    #define RESET_API_PTR(ret, name, decl_args, call_args) \
        name##_func = NULL;
    DCMI_API_LIST(RESET_API_PTR)
    DCMI_V2_API_LIST(RESET_API_PTR)
    #undef RESET_API_PTR
    dcmi_init_func = NULL;
    dcmiv2_init_func = NULL;

    // Load the library
    dlerror();
    dcmiLib = dlopen(path, RTLD_LAZY | RTLD_GLOBAL);
    err = dlerror();
    if (!dcmiLib) {
        set_last_errorf("dlopen", err);
        rc = ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

    // Load both generations' init. Only one has to resolve: a library serving V2 need not export
    // dcmi_init, and one serving V1 predates dcmiv2_init entirely. Refusing the load when either is
    // missing would refuse a whole generation of driver, so the check is that neither resolved --
    // that library offers no way in at all.
    dlerror();
    dcmi_init_func = dlsym(dcmiLib, "dcmi_init");
    dcmiv2_init_func = dlsym(dcmiLib, "dcmiv2_init");
    err = dlerror();
    if (!dcmi_init_func && !dcmiv2_init_func) {
        set_last_errorf("dlsym(dcmi_init, dcmiv2_init)", err);
        dlclose(dcmiLib);
        dcmiLib = NULL;
        dcmi_init_func = NULL;
        dcmiv2_init_func = NULL;
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
        dcmiv2_init_func = NULL;
        rc = ERROR_LIBRARY_NOT_FOUND;
        goto out;
    }

    // Load all symbols. A generation the driver does not serve simply leaves its pointers NULL,
    // which IMPL_WRAPPER reports as ERROR_FUNCTION_NOT_FOUND -- so both lists are loaded
    // unconditionally and neither generation has to be known yet at this point.
    #define LOAD_API(ret, name, decl_args, call_args) \
        name##_func = dlsym(dcmiLib, #name);
    DCMI_API_LIST(LOAD_API)
    DCMI_V2_API_LIST(LOAD_API)
    #undef LOAD_API

    rc = vendor_init();
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
    dcmiApiVersion = API_VERSION_UNKNOWN;
    dcmi_init_func = NULL;
    dcmiv2_init_func = NULL;

    #define RESET_API_PTR(ret, name, decl_args, call_args) \
        name##_func = NULL;
    DCMI_API_LIST(RESET_API_PTR)
    DCMI_V2_API_LIST(RESET_API_PTR)
    #undef RESET_API_PTR

out:
    pthread_mutex_unlock(&dcmiInitMutex);
    return rc;
}

int w_dcmi_api_version(void)
{
    // Read without the init mutex, which is what the _Atomic declaration is for: this is on the
    // per-query path of every adapted call in the Go layer, and taking the init lock there would
    // serialize a call path that is otherwise lock-free.
    return dcmiApiVersion;
}

const char* w_dcmi_last_error(void) {
    return dcmi_last_err[0] ? dcmi_last_err : "";
}
