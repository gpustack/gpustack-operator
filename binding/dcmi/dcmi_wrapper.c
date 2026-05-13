#include "dcmi_wrapper.h"
#include <dlfcn.h>
#include <stdio.h>

static void *dcmiLib = NULL;

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
    if (!path) return ERROR_LIBRARY_NOT_FOUND;

    // Load the library
    dcmiLib = dlopen(path, RTLD_LAZY | RTLD_GLOBAL);
    if (!dcmiLib) {
        return ERROR_LIBRARY_NOT_FOUND;
    }

    // Load all symbols
    #define LOAD_API(ret, name, decl_args, call_args) \
        name##_func = dlsym(dcmiLib, #name);
    DCMI_API_LIST(LOAD_API)
    #undef LOAD_API

    // Load dcmi_init
    dcmi_init_func = dlsym(dcmiLib, "dcmi_init");
    if (!dcmi_init_func) {
        dlclose(dcmiLib);
        dcmiLib = NULL;
        return ERROR_FUNCTION_NOT_FOUND;
    }
    return dcmi_init_func();
}

int w_dcmi_shutdown(void)
{
    if (!dcmiLib) return SUCCESS;

    // There is not dcmi_shutdown function in the API, so we just close the library
    return dlclose(dcmiLib) ? ERROR_UNKNOWN : SUCCESS;
}

const char* w_dcmi_last_error(void) {
    return dlerror();
}
