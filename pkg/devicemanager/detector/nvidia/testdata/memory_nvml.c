// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

#include "nvml.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Every fixture knob is an env read, and the subprocess tests set only the knobs their cases
// need, so reads default to empty instead of dereferencing what getenv returns for an unset name.
static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

nvmlReturn_t nvmlInit_v2(void) { return NVML_SUCCESS; }

const char *nvmlErrorString(nvmlReturn_t result) { return "NVML fixture"; }

nvmlReturn_t nvmlDeviceGetCount_v2(unsigned int *count) {
    *count = 1;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device) {
    device->handle = (struct nvmlDevice_st *)1;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetUUID(nvmlDevice_t device, char *uuid, unsigned int length) {
    snprintf(uuid, length, "GPU-memory-fixture");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetPciInfo_v2(nvmlDevice_t device, nvmlPciInfo_t *pci) {
    snprintf(pci->busId, sizeof(pci->busId), "0000:01:00.0");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetName(nvmlDevice_t device, char *name, unsigned int length) {
    snprintf(name, length, "NVIDIA memory fixture");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_V1"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    memory->total = strtoull(fixture_env("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
    memory->total = strtoull(fixture_env("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryBusWidth(nvmlDevice_t device, unsigned int *width) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_BUS_WIDTH_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *width = strtoul(fixture_env("GPUSTACK_TEST_NVML_BUS_WIDTH"), NULL, 10);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetEccMode(nvmlDevice_t device, nvmlEnableState_t *current,
                                nvmlEnableState_t *pending) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_ECC_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *current = strcmp(fixture_env("GPUSTACK_TEST_NVML_ECC"), "true") == 0
        ? NVML_FEATURE_ENABLED : NVML_FEATURE_DISABLED;
    *pending = *current;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetCudaComputeCapability(nvmlDevice_t device, int *major, int *minor) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_CC_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *major = atoi(fixture_env("GPUSTACK_TEST_NVML_CC_MAJOR"));
    *minor = atoi(fixture_env("GPUSTACK_TEST_NVML_CC_MINOR"));
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryErrorCounter(nvmlDevice_t device, nvmlMemoryErrorType_t errorType,
                                             nvmlEccCounterType_t counterType,
                                             nvmlMemoryLocation_t locationType,
                                             unsigned long long *count) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_ECC_UE_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *count = strtoull(fixture_env("GPUSTACK_TEST_NVML_ECC_UE_COUNTER"), NULL, 10);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetFieldValues(nvmlDevice_t device, int valuesCount,
                                      nvmlFieldValue_t *values) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_FIELD_VALUES_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    for (int i = 0; i < valuesCount; i++) {
        if (values[i].fieldId != NVML_FI_DEV_GET_GPU_RECOVERY_ACTION)
            continue;
        if (strcmp(fixture_env("GPUSTACK_TEST_NVML_RECOVERY_ACTION_ERROR"), "true") == 0) {
            values[i].nvmlReturn = NVML_ERROR_NOT_SUPPORTED;
            continue;
        }
        values[i].valueType = (nvmlValueType_t)atoi(fixture_env("GPUSTACK_TEST_NVML_RECOVERY_ACTION_TYPE"));
        values[i].value.uiVal =
            (unsigned int)strtoul(fixture_env("GPUSTACK_TEST_NVML_RECOVERY_ACTION"), NULL, 10);
        values[i].nvmlReturn = NVML_SUCCESS;
    }
    return NVML_SUCCESS;
}
