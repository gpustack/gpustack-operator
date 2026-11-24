// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

#include "nvml.h"
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;

// Every fixture knob is an env read, and the subprocess tests set only the knobs their cases
// need, so reads default to empty instead of dereferencing what getenv returns for an unset name.
static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static unsigned int device_index(nvmlDevice_t device) {
    return (unsigned int)((uintptr_t)device.handle - 1U);
}

static nvmlReturn_t maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_FAIL_STAGE"), stage) != 0)
        return NVML_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_NVML_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return NVML_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return NVML_SUCCESS;
    return (nvmlReturn_t)atoi(fixture_env("GPUSTACK_TEST_NVML_FAIL_ERR"));
}

nvmlReturn_t nvmlInit_v2(void) {
    const char *err_env = fixture_env("GPUSTACK_TEST_NVML_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_NVML_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (nvmlReturn_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    const char *ok_ret_env = fixture_env("GPUSTACK_TEST_NVML_INIT_OK_RET");
    if (ok_ret_env[0] != '\0')
        return (nvmlReturn_t)atoi(ok_ret_env);
    return NVML_SUCCESS;
}

const char *nvmlErrorString(nvmlReturn_t result) { return "NVML fixture"; }

nvmlReturn_t nvmlDeviceGetCount_v2(unsigned int *count) {
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return NVML_ERROR_UNINITIALIZED;
    const char *err_env = fixture_env("GPUSTACK_TEST_NVML_COUNT_ERR");
    if (err_env[0] != '\0')
        return (nvmlReturn_t)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_NVML_COUNT");
    *count = cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device) {
    nvmlReturn_t ret = maybe_fail_device("handle", index);
    if (ret != NVML_SUCCESS)
        return ret;
    device->handle = (struct nvmlDevice_st *)(uintptr_t)(index + 1U);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetUUID(nvmlDevice_t device, char *uuid, unsigned int length) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("uuid", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    if (fixture_env("GPUSTACK_TEST_NVML_COUNT")[0] != '\0') {
        snprintf(uuid, length, "GPU-%u", idx);
        return NVML_SUCCESS;
    }
    snprintf(uuid, length, "GPU-memory-fixture");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetPciInfo_v2(nvmlDevice_t device, nvmlPciInfo_t *pci) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("pci", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    memset(pci, 0, sizeof(*pci));
    pci->domain = 0;
    pci->bus = idx + 1U;
    pci->device = 0;
    snprintf(pci->busId, sizeof(pci->busId), "0000:%02x:00.0", idx + 1U);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetName(nvmlDevice_t device, char *name, unsigned int length) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("name", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    snprintf(name, length, "NVIDIA memory fixture");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    if (strcmp(fixture_env("GPUSTACK_TEST_NVML_V1"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    memory->total = strtoull(fixture_env("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    memory->used = 0;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    memory->total = strtoull(fixture_env("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    memory->used = 0;
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
    const char *unhealthy_idx = fixture_env("GPUSTACK_TEST_NVML_UNHEALTHY_INDEX");
    if (unhealthy_idx[0] != '\0') {
        *count = ((unsigned int)strtoul(unhealthy_idx, NULL, 10) == device_index(device)) ? 1ULL : 0ULL;
        return NVML_SUCCESS;
    }
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
