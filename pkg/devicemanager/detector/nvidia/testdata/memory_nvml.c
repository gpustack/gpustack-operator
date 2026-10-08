// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

#include "nvml.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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
    if (strcmp(getenv("GPUSTACK_TEST_NVML_V1"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    memory->total = strtoull(getenv("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
    memory->total = strtoull(getenv("GPUSTACK_TEST_NVML_TOTAL_MIB"), NULL, 10) << 20;
    memory->free = memory->total;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryBusWidth(nvmlDevice_t device, unsigned int *width) {
    if (strcmp(getenv("GPUSTACK_TEST_NVML_BUS_WIDTH_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *width = strtoul(getenv("GPUSTACK_TEST_NVML_BUS_WIDTH"), NULL, 10);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetEccMode(nvmlDevice_t device, nvmlEnableState_t *current,
                                nvmlEnableState_t *pending) {
    if (strcmp(getenv("GPUSTACK_TEST_NVML_ECC_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *current = strcmp(getenv("GPUSTACK_TEST_NVML_ECC"), "true") == 0
        ? NVML_FEATURE_ENABLED : NVML_FEATURE_DISABLED;
    *pending = *current;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetCudaComputeCapability(nvmlDevice_t device, int *major, int *minor) {
    if (strcmp(getenv("GPUSTACK_TEST_NVML_CC_ERROR"), "true") == 0)
        return NVML_ERROR_NOT_SUPPORTED;
    *major = atoi(getenv("GPUSTACK_TEST_NVML_CC_MAJOR"));
    *minor = atoi(getenv("GPUSTACK_TEST_NVML_CC_MINOR"));
    return NVML_SUCCESS;
}
