#ifndef DMI_MIG_WRAPPER_H
#define DMI_MIG_WRAPPER_H

#include "dmi_mig.h"

#ifdef __cplusplus
extern "C" {
#endif

// nvmlDeviceGetUtilizationRates is exported by the shared object but declared nowhere in the vendor
// header, so its signature is asserted here rather than inherited. It is NVML's, and it was
// validated on hardware: called with a MIG device handle it returns that instance's own compute and
// memory percentages -- 95% on an instance running a matmul loop while its three idle siblings on
// the same card read 0% -- which is where the per-instance compute figure comes from.
nvmlReturn_t nvmlDeviceGetUtilizationRates(nvmlDevice_t device,
                                           nvmlUtilization_t *utilization);

#include "dmi_mig_wrapper_api.def"

// w_dmi_mig_init loads the vendor library from path and resolves every function in the API list.
//
// It returns NVML_SUCCESS, NVML_ERROR_LIBRARY_NOT_FOUND when the library cannot be opened or its
// path cannot be tracked, or NVML_ERROR_FUNCTION_NOT_FOUND when the library opened but serves none
// of the API. Calling it again for the library already held is a no-op that succeeds, so two
// independent callers in one process -- the detector and the allocator -- need no ordering between
// them.
nvmlReturn_t w_dmi_mig_init(const char *path);

// w_dmi_mig_shutdown unloads the library and blanks every cached pointer. It is safe only when
// nothing else in the process still holds the library; see the note on its definition.
nvmlReturn_t w_dmi_mig_shutdown(void);

// w_dmi_mig_last_error returns this thread's last load-time failure, or an empty string. It reports
// the dynamic loader's own message, which is the only thing that distinguishes a missing file from
// an unreadable one from a wrong architecture.
const char *w_dmi_mig_last_error(void);

#define DECL_API(ret, name, decl_args, call_args) ret w_##name decl_args;
DMI_MIG_API_LIST(DECL_API)
#undef DECL_API

#ifdef __cplusplus
}  // extern "C"
#endif

#endif  // DMI_MIG_WRAPPER_H
