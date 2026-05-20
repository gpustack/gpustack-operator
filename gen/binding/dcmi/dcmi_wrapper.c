#include "dcmi_wrapper.h"
#include <dlfcn.h>
#include <stdio.h>

static void *dcmiLib = NULL;

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
    clear_last_error();

    if (!path || path[0] == '\0') {
        set_last_error("invalid library path");
        return ERROR_LIBRARY_NOT_FOUND;
    }

    // Avoid leaking an existing handle if init is called more than once.
    if (dcmiLib) {
        dlclose(dcmiLib);
        dcmiLib = NULL;
    }

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
        return ERROR_LIBRARY_NOT_FOUND;
    }

    // Load dcmi_init
    dlerror();
    dcmi_init_func = dlsym(dcmiLib, "dcmi_init");
    err = dlerror();
    if (!dcmi_init_func) {
        set_last_errorf("dlsym(dcmi_init)", err);
        dlclose(dcmiLib);
        dcmiLib = NULL;
        return ERROR_FUNCTION_NOT_FOUND;
    }

    // Load all symbols
    #define LOAD_API(ret, name, decl_args, call_args) \
        name##_func = dlsym(dcmiLib, #name);
    DCMI_API_LIST(LOAD_API)
    #undef LOAD_API

    return dcmi_init_func();
}

int w_dcmi_shutdown(void)
{
    clear_last_error();

    if (!dcmiLib) return SUCCESS;

    if (dlclose(dcmiLib) != 0) {
        set_last_errorf("dlclose", dlerror());
        return ERROR_UNKNOWN;
    }

    dcmiLib = NULL;
    dcmi_init_func = NULL;

    #define RESET_API_PTR(ret, name, decl_args, call_args) \
        name##_func = NULL;
    DCMI_API_LIST(RESET_API_PTR)
    #undef RESET_API_PTR

    return SUCCESS;
}

const char* w_dcmi_last_error(void) {
    return dcmi_last_err[0] ? dcmi_last_err : "";
}
