typedef _Bool bool;
/*
 * Copyright (c) 2022-2026 T-Head (Shanghai) Semiconductor Co., Ltd.
 * All rights reserved.
 *
 * This software and the accompanying documentation are proprietary to
 * T-Head and are furnished pursuant to, and subject to, a written
 * license agreement executed between T-Head and the recipient.
 * Unauthorized copying, distribution, or disclosure in whole or in
 * part is strictly prohibited.
 *
 * THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS. T-HEAD EXPRESSLY
 * DISCLAIMS ALL WARRANTIES, WHETHER EXPRESS, IMPLIED, STATUTORY, OR
 * OTHERWISE, INCLUDING BUT NOT LIMITED TO WARRANTIES OF
 * MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, TITLE, AND
 * NON-INFRINGEMENT.
 *
 * IN NO EVENT SHALL T-HEAD BE LIABLE FOR ANY DIRECT, INDIRECT,
 * INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES ARISING
 * OUT OF OR IN CONNECTION WITH THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGES.
 *
 * Recipients are required to retain this notice in all copies and
 * derivative works.
 */

#ifndef __HGML_H__
#define __HGML_H__

#ifdef __cplusplus
extern "C" {
#endif

#define HGML_API_VERSION            13
#define HGML_API_VERSION_STR       "13"

#define HGML_STRUCT_VERSION(data, ver) (unsigned int)(sizeof(hgml ## data ## _v ## ver ## _t) | (ver << 24U))

#define HGML_VALUE_NOT_AVAILABLE (-1)

#define DEPRECATED(ver)

typedef struct
{
    struct hgmlDevice_st* handle;
} hgmlDevice_t;

typedef struct
{
    struct hgmlGpuInstance_st* handle;
} hgmlGpuInstance_t;

#define HGML_DEVICE_PCI_BUS_ID_BUFFER_SIZE      (32 - 4)

#define HGML_DEVICE_PCI_BUS_ID_BUFFER_V2_SIZE   16

typedef struct
{
    unsigned int version;
    unsigned int domain;
    unsigned int bus;
    unsigned int device;
    unsigned int pciDeviceId;
    unsigned int pciSubSystemId;
    unsigned int baseClass;
    unsigned int subClass;
    char busId[HGML_DEVICE_PCI_BUS_ID_BUFFER_SIZE];
    unsigned int globalPpuId;
} hgmlPciInfoExt_v1_t;
typedef hgmlPciInfoExt_v1_t  hgmlPciInfoExt_t;
#define hgmlPciInfoExt_v1 HGML_STRUCT_VERSION(PciInfoExt, 1)

typedef struct
{
    char busIdLegacy[HGML_DEVICE_PCI_BUS_ID_BUFFER_V2_SIZE];
    unsigned int domain;
    unsigned int bus;
    unsigned int device;
    unsigned int pciDeviceId;
    unsigned int pciSubSystemId;
    char busId[HGML_DEVICE_PCI_BUS_ID_BUFFER_SIZE];
    unsigned int globalPpuId;
} hgmlPciInfo_t;

#define HGML_DEVICE_PCI_BUS_ID_LEGACY_FMT           "%04X:%02X:%02X.%01X"

#define HGML_DEVICE_PCI_BUS_ID_FMT                  "%08X:%02X:%02X.%01X"

#define HGML_DEVICE_PCI_BUS_ID_FMT_ARGS(pciInfo)    (pciInfo)->domain, (pciInfo)->bus, (pciInfo)->device

typedef struct
{
    unsigned long long l1Cache;
    unsigned long long l2Cache;
    unsigned long long deviceMemory;
    unsigned long long registerFile;
} hgmlEccErrorCounts_t;

typedef struct
{
    unsigned int gpu;
    unsigned int memory;
} hgmlUtilization_t;

typedef struct
{
    unsigned long long total;
    unsigned long long free;
    unsigned long long used;

} hgmlMemory_t;

typedef struct
{
    unsigned int version;
    unsigned long long total;
    unsigned long long reserved;
    unsigned long long free;
    unsigned long long used;
} hgmlMemory_v2_t;

#define hgmlMemory_v2 HGML_STRUCT_VERSION(Memory, 2)

typedef struct
{
    unsigned long long bar1Total;
    unsigned long long bar1Free;
    unsigned long long bar1Used;
}hgmlBAR1Memory_t;

typedef struct
{
    unsigned int        pid;
    unsigned long long  usedGpuMemory;
} hgmlProcessInfo_v1_t;

typedef struct
{
    unsigned int        pid;
    unsigned long long  usedGpuMemory;
    unsigned int        gpuInstanceId;
    unsigned int        computeInstanceId;
} hgmlProcessInfo_v2_t, hgmlProcessInfo_t;

typedef struct
{
    unsigned int        pid;
    unsigned long long  usedGpuMemory;
    unsigned int        gpuInstanceId;
    unsigned int        computeInstanceId;
    unsigned long long  usedGpuCcProtectedMemory;
} hgmlProcessDetail_v1_t;

typedef struct
{
    unsigned int           version;
    unsigned int           mode;
    unsigned int           numProcArrayEntries;
    hgmlProcessDetail_v1_t *procArray;
} hgmlProcessDetailList_v1_t;

typedef hgmlProcessDetailList_v1_t hgmlProcessDetailList_t;

#define hgmlProcessDetailList_v1 HGML_STRUCT_VERSION(ProcessDetailList, 1)

typedef struct
{
    unsigned int multiprocessorCount;
    unsigned int sharedCopyEngineCount;
    unsigned int sharedDecoderCount;
    unsigned int sharedEncoderCount;
    unsigned int sharedJpegCount;
    unsigned int sharedOfaCount;
    unsigned int gpuInstanceSliceCount;
    unsigned int computeInstanceSliceCount;
    unsigned long long memorySizeMB;
} hgmlDeviceAttributes_t;

typedef struct
{
    unsigned int isC2cEnabled;
} hgmlC2cModeInfo_v1_t;

#define hgmlC2cModeInfo_v1 HGML_STRUCT_VERSION(C2cModeInfo, 1)

typedef enum
{
    HGML_DEVICE_ADDRESSING_MODE_NONE = 0,
    HGML_DEVICE_ADDRESSING_MODE_HMM  = 1,
    HGML_DEVICE_ADDRESSING_MODE_ATS  = 2,
} hgmlDeviceAddressingModeType_t;

typedef struct
{
    unsigned int version;
    unsigned int value;
} hgmlDeviceAddressingMode_v1_t;
typedef hgmlDeviceAddressingMode_v1_t hgmlDeviceAddressingMode_t;

#define hgmlDeviceAddressingMode_v1 HGML_STRUCT_VERSION(DeviceAddressingMode, 1)

typedef struct
{
   unsigned int version;
   unsigned int bChannelRepairPending;
   unsigned int bTpcRepairPending;
} hgmlRepairStatus_v1_t;
typedef hgmlRepairStatus_v1_t hgmlRepairStatus_t;

#define hgmlRepairStatus_v1 HGML_STRUCT_VERSION(RepairStatus, 1)

typedef struct
{
    unsigned int max;
    unsigned int high;
    unsigned int partial;
    unsigned int low;
    unsigned int none;
} hgmlRowRemapperHistogramValues_t;

typedef enum
{
    HGML_BRIDGE_CHIP_PLX = 0,
    HGML_BRIDGE_CHIP_BRO4 = 1
}hgmlBridgeChipType_t;

#define HGML_ICNLINK_MAX_LINKS 8

typedef enum
{
    HGML_ICNLINK_COUNTER_UNIT_CYCLES =  0,
    HGML_ICNLINK_COUNTER_UNIT_PACKETS = 1,
    HGML_ICNLINK_COUNTER_UNIT_BYTES   = 2,
    HGML_ICNLINK_COUNTER_UNIT_RESERVED = 3,

    HGML_ICNLINK_COUNTER_UNIT_COUNT
} hgmlIcnLinkUtilizationCountUnits_t;

typedef enum
{
    HGML_ICNLINK_COUNTER_PKTFILTER_NOP        = 0x1,
    HGML_ICNLINK_COUNTER_PKTFILTER_READ       = 0x2,
    HGML_ICNLINK_COUNTER_PKTFILTER_WRITE      = 0x4,
    HGML_ICNLINK_COUNTER_PKTFILTER_RATOM      = 0x8,
    HGML_ICNLINK_COUNTER_PKTFILTER_NRATOM     = 0x10,
    HGML_ICNLINK_COUNTER_PKTFILTER_FLUSH      = 0x20,
    HGML_ICNLINK_COUNTER_PKTFILTER_RESPDATA   = 0x40,
    HGML_ICNLINK_COUNTER_PKTFILTER_RESPNODATA = 0x80,
    HGML_ICNLINK_COUNTER_PKTFILTER_ALL        = 0xFF
} hgmlIcnLinkUtilizationCountPktTypes_t;

typedef struct
{
    hgmlIcnLinkUtilizationCountUnits_t units;
    hgmlIcnLinkUtilizationCountPktTypes_t pktfilter;
} hgmlIcnLinkUtilizationControl_t;

typedef enum
{
    HGML_ICNLINK_CAP_P2P_SUPPORTED = 0,
    HGML_ICNLINK_CAP_SYSMEM_ACCESS = 1,
    HGML_ICNLINK_CAP_P2P_ATOMICS   = 2,
    HGML_ICNLINK_CAP_SYSMEM_ATOMICS= 3,
    HGML_ICNLINK_CAP_SLI_BRIDGE    = 4,
    HGML_ICNLINK_CAP_VALID         = 5,

    HGML_ICNLINK_CAP_COUNT
} hgmlIcnLinkCapability_t;

typedef enum
{
    HGML_ICNLINK_ERROR_DL_REPLAY   = 0,
    HGML_ICNLINK_ERROR_DL_RECOVERY = 1,
    HGML_ICNLINK_ERROR_DL_CRC_FLIT = 2,
    HGML_ICNLINK_ERROR_DL_CRC_DATA = 3,
    HGML_ICNLINK_ERROR_DL_ECC_DATA = 4,

    HGML_ICNLINK_ERROR_COUNT
} hgmlIcnLinkErrorCounter_t;

typedef enum
{
    HGML_ICNLINK_DEVICE_TYPE_PPU     = 0x00,
    HGML_ICNLINK_DEVICE_TYPE_IBMNPU  = 0x01,
    HGML_ICNLINK_DEVICE_TYPE_SWITCH  = 0x02,
    HGML_ICNLINK_DEVICE_TYPE_UNKNOWN = 0xFF
} hgmlIntIcnLinkDeviceType_t;

typedef enum
{
    HGML_TOPOLOGY_INTERNAL           = 0,
    HGML_TOPOLOGY_SINGLE             = 10,
    HGML_TOPOLOGY_MULTIPLE           = 20,
    HGML_TOPOLOGY_HOSTBRIDGE         = 30,
    HGML_TOPOLOGY_NODE               = 40,
    HGML_TOPOLOGY_SYSTEM             = 50
} hgmlGpuTopologyLevel_t;

#define HGML_TOPOLOGY_CPU HGML_TOPOLOGY_NODE

typedef enum
{
    HGML_P2P_STATUS_OK     = 0,
    HGML_P2P_STATUS_CHIPSET_NOT_SUPPORED,
    HGML_P2P_STATUS_CHIPSET_NOT_SUPPORTED = HGML_P2P_STATUS_CHIPSET_NOT_SUPPORED,
    HGML_P2P_STATUS_GPU_NOT_SUPPORTED,
    HGML_P2P_STATUS_IOH_TOPOLOGY_NOT_SUPPORTED,
    HGML_P2P_STATUS_DISABLED_BY_REGKEY,
    HGML_P2P_STATUS_NOT_SUPPORTED,
    HGML_P2P_STATUS_UNKNOWN
} hgmlGpuP2PStatus_t;

typedef enum
{
    HGML_P2P_CAPS_INDEX_READ = 0,
    HGML_P2P_CAPS_INDEX_WRITE = 1,
    HGML_P2P_CAPS_INDEX_ICNLINK = 2,
    HGML_P2P_CAPS_INDEX_ATOMICS = 3,
    HGML_P2P_CAPS_INDEX_PCI = 4,
    HGML_P2P_CAPS_INDEX_PROP = HGML_P2P_CAPS_INDEX_PCI,

    HGML_P2P_CAPS_INDEX_UNKNOWN = 5,
}hgmlGpuP2PCapsIndex_t;

#define HGML_MAX_PHYSICAL_BRIDGE                         (128)

typedef struct
{
    hgmlBridgeChipType_t type;
    unsigned int fwVersion;
}hgmlBridgeChipInfo_t;

typedef struct
{
    unsigned char  bridgeCount;
    hgmlBridgeChipInfo_t bridgeChipInfo[HGML_MAX_PHYSICAL_BRIDGE];
}hgmlBridgeChipHierarchy_t;

typedef enum
{
    HGML_TOTAL_POWER_SAMPLES        = 0,
    HGML_GPU_UTILIZATION_SAMPLES    = 1,
    HGML_MEMORY_UTILIZATION_SAMPLES = 2,
    HGML_ENC_UTILIZATION_SAMPLES    = 3,
    HGML_DEC_UTILIZATION_SAMPLES    = 4,
    HGML_PROCESSOR_CLK_SAMPLES      = 5,
    HGML_MEMORY_CLK_SAMPLES         = 6,
    HGML_MODULE_POWER_SAMPLES       = 7,
    HGML_JPG_UTILIZATION_SAMPLES    = 8,
    HGML_OFA_UTILIZATION_SAMPLES    = 9,

    HGML_SAMPLINGTYPE_COUNT
} hgmlSamplingType_t;

typedef enum
{
    HGML_PCIE_UTIL_TX_BYTES             = 0,
    HGML_PCIE_UTIL_RX_BYTES             = 1,

    HGML_PCIE_UTIL_COUNT
} hgmlPcieUtilCounter_t;

typedef enum
{
    HGML_VALUE_TYPE_DOUBLE = 0,
    HGML_VALUE_TYPE_UNSIGNED_INT = 1,
    HGML_VALUE_TYPE_UNSIGNED_LONG = 2,
    HGML_VALUE_TYPE_UNSIGNED_LONG_LONG = 3,
    HGML_VALUE_TYPE_SIGNED_LONG_LONG = 4,
    HGML_VALUE_TYPE_SIGNED_INT = 5,
    HGML_VALUE_TYPE_UNSIGNED_SHORT = 6,

    HGML_VALUE_TYPE_COUNT
}hgmlValueType_t;

typedef union hgmlValue_st
{
    double dVal;
    int siVal;
    unsigned int uiVal;
    unsigned long ulVal;
    unsigned long long ullVal;
    signed long long sllVal;
    unsigned short usVal;
}hgmlValue_t;

typedef struct
{
    unsigned long long timeStamp;
    hgmlValue_t sampleValue;
}hgmlSample_t;

typedef enum
{
    HGML_PERF_POLICY_POWER = 0,
    HGML_PERF_POLICY_THERMAL = 1,
    HGML_PERF_POLICY_SYNC_BOOST = 2,
    HGML_PERF_POLICY_BOARD_LIMIT = 3,
    HGML_PERF_POLICY_LOW_UTILIZATION = 4,
    HGML_PERF_POLICY_RELIABILITY = 5,
    HGML_PERF_POLICY_TOTAL_APP_CLOCKS = 10,
    HGML_PERF_POLICY_TOTAL_BASE_CLOCKS = 11,

    HGML_PERF_POLICY_COUNT
}hgmlPerfPolicyType_t;

typedef struct
{
    unsigned long long referenceTime;
    unsigned long long violationTime;
} hgmlViolationTime_t;

#define HGML_MAX_THERMAL_SENSORS_PER_GPU  3

typedef enum
{
    HGML_THERMAL_TARGET_NONE          = 0,
    HGML_THERMAL_TARGET_GPU           = 1,
    HGML_THERMAL_TARGET_MEMORY        = 2,
    HGML_THERMAL_TARGET_POWER_SUPPLY  = 4,
    HGML_THERMAL_TARGET_BOARD         = 8,
    HGML_THERMAL_TARGET_VCD_BOARD     = 9,
    HGML_THERMAL_TARGET_VCD_INLET     = 10,
    HGML_THERMAL_TARGET_VCD_OUTLET    = 11,

    HGML_THERMAL_TARGET_ALL           = 15,
    HGML_THERMAL_TARGET_UNKNOWN       = -1,
} hgmlThermalTarget_t;

typedef enum
{
    HGML_THERMAL_CONTROLLER_NONE = 0,
    HGML_THERMAL_CONTROLLER_GPU_INTERNAL,
    HGML_THERMAL_CONTROLLER_ADM1032,
    HGML_THERMAL_CONTROLLER_ADT7461,
    HGML_THERMAL_CONTROLLER_MAX6649,
    HGML_THERMAL_CONTROLLER_MAX1617,
    HGML_THERMAL_CONTROLLER_LM99,
    HGML_THERMAL_CONTROLLER_LM89,
    HGML_THERMAL_CONTROLLER_LM64,
    HGML_THERMAL_CONTROLLER_G781,
    HGML_THERMAL_CONTROLLER_ADT7473,
    HGML_THERMAL_CONTROLLER_SBMAX6649,
    HGML_THERMAL_CONTROLLER_VBIOSEVT,
    HGML_THERMAL_CONTROLLER_OS,
    HGML_THERMAL_CONTROLLER_HGSYSCON_CANOAS,
    HGML_THERMAL_CONTROLLER_HGSYSCON_E551,
    HGML_THERMAL_CONTROLLER_MAX6649R,
    HGML_THERMAL_CONTROLLER_ADT7473S,
    HGML_THERMAL_CONTROLLER_UNKNOWN = -1,
} hgmlThermalController_t;

typedef struct
{
    unsigned int   count;
    struct
    {
        hgmlThermalController_t controller;
        int defaultMinTemp;
        int defaultMaxTemp;
        int currentTemp;
        hgmlThermalTarget_t target;
    } sensor[HGML_MAX_THERMAL_SENSORS_PER_GPU];

} hgmlGpuThermalSettings_t;

typedef enum
{
    HGML_THERMAL_COOLER_SIGNAL_NONE        = 0,
    HGML_THERMAL_COOLER_SIGNAL_TOGGLE      = 1,
    HGML_THERMAL_COOLER_SIGNAL_VARIABLE    = 2,

    HGML_THERMAL_COOLER_SIGNAL_COUNT
} hgmlCoolerControl_t;

typedef enum
{
    HGML_THERMAL_COOLER_TARGET_NONE          = 1 << 0,
    HGML_THERMAL_COOLER_TARGET_GPU           = 1 << 1,
    HGML_THERMAL_COOLER_TARGET_MEMORY        = 1 << 2,
    HGML_THERMAL_COOLER_TARGET_POWER_SUPPLY  = 1 << 3,
    HGML_THERMAL_COOLER_TARGET_GPU_RELATED   = (HGML_THERMAL_COOLER_TARGET_GPU | HGML_THERMAL_COOLER_TARGET_MEMORY | HGML_THERMAL_COOLER_TARGET_POWER_SUPPLY)
} hgmlCoolerTarget_t;

typedef struct
{
    unsigned int version;
    unsigned int index;
    hgmlCoolerControl_t signalType;
    hgmlCoolerTarget_t target;
} hgmlCoolerInfo_v1_t;
typedef hgmlCoolerInfo_v1_t hgmlCoolerInfo_t;

#define hgmlCoolerInfo_v1 HGML_STRUCT_VERSION(CoolerInfo, 1)

#define HGML_DEVICE_UUID_ASCII_LEN  41

#define HGML_DEVICE_UUID_BINARY_LEN 16

typedef enum
{
    HGML_UUID_TYPE_NONE   = 0,
    HGML_UUID_TYPE_ASCII  = 1,
    HGML_UUID_TYPE_BINARY = 2,
} hgmlUUIDType_t;

typedef union
{
    char str[HGML_DEVICE_UUID_ASCII_LEN];
    unsigned char bytes[HGML_DEVICE_UUID_BINARY_LEN];
} hgmlUUIDValue_t;

typedef struct
{
   unsigned int version;
   unsigned int type;
   hgmlUUIDValue_t value;
} hgmlUUID_v1_t;
typedef hgmlUUID_v1_t hgmlUUID_t;

#define hgmlUUID_v1 HGML_STRUCT_VERSION(UUID, 1)

typedef struct
{
   unsigned int version;
   unsigned long long value;
} hgmlPdi_v1_t;
typedef hgmlPdi_v1_t hgmlPdi_t;

#define hgmlPdi_v1 HGML_STRUCT_VERSION(Pdi, 1)

typedef enum
{
    HGML_FEATURE_DISABLED    = 0,
    HGML_FEATURE_ENABLED     = 1
} hgmlEnableState_t;

#define hgmlFlagDefault     0x00

#define hgmlFlagForce       0x01

typedef struct
{
    unsigned int version;
    hgmlEnableState_t encryptionState;
} hgmlDramEncryptionInfo_v1_t;
typedef hgmlDramEncryptionInfo_v1_t hgmlDramEncryptionInfo_t;

#define hgmlDramEncryptionInfo_v1 HGML_STRUCT_VERSION(DramEncryptionInfo, 1)

typedef enum
{
    HGML_BRAND_UNKNOWN     = 0,
    HGML_BRAND_BEETHOVEN   = 14,

    HGML_BRAND_COUNT
} hgmlBrandType_t;

typedef enum
{
    HGML_TEMPERATURE_THRESHOLD_SHUTDOWN      = 0,
    HGML_TEMPERATURE_THRESHOLD_SLOWDOWN      = 1,
    HGML_TEMPERATURE_THRESHOLD_MEM_MAX       = 2,
    HGML_TEMPERATURE_THRESHOLD_GPU_MAX       = 3,
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_MIN  = 4,
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_CURR = 5,
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_MAX  = 6,
    HGML_TEMPERATURE_THRESHOLD_GPS_CURR      = 7,

    HGML_TEMPERATURE_THRESHOLD_COUNT
} hgmlTemperatureThresholds_t;

typedef enum
{
    HGML_TEMPERATURE_GPU      = 0,

    HGML_TEMPERATURE_COUNT
} hgmlTemperatureSensors_t;

typedef struct
{
    unsigned int version;
    int marginTemperature;
} hgmlMarginTemperature_v1_t;

typedef hgmlMarginTemperature_v1_t hgmlMarginTemperature_t;

#define hgmlMarginTemperature_v1 HGML_STRUCT_VERSION(MarginTemperature, 1)

typedef enum
{
    HGML_COMPUTEMODE_DEFAULT           = 0,
    HGML_COMPUTEMODE_EXCLUSIVE_THREAD  = 1,
    HGML_COMPUTEMODE_PROHIBITED        = 2,
    HGML_COMPUTEMODE_EXCLUSIVE_PROCESS = 3,

    HGML_COMPUTEMODE_COUNT
} hgmlComputeMode_t;

#define MAX_CLK_DOMAINS                 32

typedef struct
{
    unsigned int   clkApiDomain;
    unsigned int   clkDomainFaultMask;
} hgmlClkMonFaultInfo_t;

typedef struct
{
    unsigned int  bGlobalStatus;
    unsigned int   clkMonListSize;
    hgmlClkMonFaultInfo_t clkMonList[MAX_CLK_DOMAINS];
} hgmlClkMonStatus_t;

#define hgmlEccBitType_t hgmlMemoryErrorType_t

#define HGML_SINGLE_BIT_ECC HGML_MEMORY_ERROR_TYPE_CORRECTED

#define HGML_DOUBLE_BIT_ECC HGML_MEMORY_ERROR_TYPE_UNCORRECTED

typedef enum
{
    HGML_MEMORY_ERROR_TYPE_CORRECTED = 0,
    HGML_MEMORY_ERROR_TYPE_UNCORRECTED = 1,
    HGML_MEMORY_ERROR_TYPE_COUNT

} hgmlMemoryErrorType_t;

typedef enum
{
    HGML_ICNLINK_VERSION_INVALID = 0,
    HGML_ICNLINK_VERSION_1_0     = 1,
    HGML_ICNLINK_VERSION_2_0     = 2,
    HGML_ICNLINK_VERSION_2_2     = 3,
    HGML_ICNLINK_VERSION_3_0     = 4,
    HGML_ICNLINK_VERSION_3_1     = 5,
    HGML_ICNLINK_VERSION_4_0     = 6,
    HGML_ICNLINK_VERSION_5_0     = 7,
}hgmlIcnLinkVersion_t;

typedef enum
{
    HGML_VOLATILE_ECC      = 0,
    HGML_AGGREGATE_ECC     = 1,

    HGML_ECC_COUNTER_TYPE_COUNT
} hgmlEccCounterType_t;

typedef enum
{
    HGML_CLOCK_GRAPHICS  = 0,
    HGML_CLOCK_SM        = 1,
    HGML_CLOCK_MEM       = 2,
    HGML_CLOCK_VIDEO     = 3,

    HGML_CLOCK_COUNT
} hgmlClockType_t;

typedef enum
{
    HGML_CLOCK_ID_CURRENT            = 0,
    HGML_CLOCK_ID_APP_CLOCK_TARGET   = 1,
    HGML_CLOCK_ID_APP_CLOCK_DEFAULT  = 2,
    HGML_CLOCK_ID_CUSTOMER_BOOST_MAX = 3,

    HGML_CLOCK_ID_COUNT
} hgmlClockId_t;

typedef enum
{
    HGML_DRIVER_LINUX = 0xff,
} hgmlDriverModel_t;

#define HGML_MAX_GPU_PERF_PSTATES 16

typedef enum
{
    HGML_PSTATE_0               = 0,
    HGML_PSTATE_1               = 1,
    HGML_PSTATE_2               = 2,
    HGML_PSTATE_3               = 3,
    HGML_PSTATE_4               = 4,
    HGML_PSTATE_5               = 5,
    HGML_PSTATE_6               = 6,
    HGML_PSTATE_7               = 7,
    HGML_PSTATE_8               = 8,
    HGML_PSTATE_9               = 9,
    HGML_PSTATE_10              = 10,
    HGML_PSTATE_11              = 11,
    HGML_PSTATE_12              = 12,
    HGML_PSTATE_13              = 13,
    HGML_PSTATE_14              = 14,
    HGML_PSTATE_15              = 15,
    HGML_PSTATE_UNKNOWN         = 32
} hgmlPstates_t;

typedef struct
{
    unsigned int version;
    hgmlClockType_t type;
    hgmlPstates_t pstate;
    int clockOffsetMHz;
    int minClockOffsetMHz;
    int maxClockOffsetMHz;
} hgmlClockOffset_v1_t;

typedef hgmlClockOffset_v1_t hgmlClockOffset_t;

#define hgmlClockOffset_v1 HGML_STRUCT_VERSION(ClockOffset, 1)

typedef struct
{
    unsigned int version;
    unsigned int fan;
    unsigned int speed;
} hgmlFanSpeedInfo_v1_t;
typedef hgmlFanSpeedInfo_v1_t hgmlFanSpeedInfo_t;

#define hgmlFanSpeedInfo_v1 HGML_STRUCT_VERSION(FanSpeedInfo, 1)

#define HGML_PERF_MODES_BUFFER_SIZE       2048

typedef struct
{
    unsigned int version;
    char         str[HGML_PERF_MODES_BUFFER_SIZE];
} hgmlDevicePerfModes_v1_t;
typedef hgmlDevicePerfModes_v1_t hgmlDevicePerfModes_t;

#define hgmlDevicePerfModes_v1 HGML_STRUCT_VERSION(DevicePerfModes, 1)

typedef struct
{
    unsigned int version;
    char         str[HGML_PERF_MODES_BUFFER_SIZE];
} hgmlDeviceCurrentClockFreqs_v1_t;
typedef hgmlDeviceCurrentClockFreqs_v1_t hgmlDeviceCurrentClockFreqs_t;

#define hgmlDeviceCurrentClockFreqs_v1 HGML_STRUCT_VERSION(DeviceCurrentClockFreqs, 1)

#define    HGML_POWER_MIZER_MODE_ADAPTIVE                      0
#define    HGML_POWER_MIZER_MODE_PREFER_MAXIMUM_PERFORMANCE    1
#define    HGML_POWER_MIZER_MODE_AUTO                          2
#define    HGML_POWER_MIZER_MODE_PREFER_CONSISTENT_PERFORMANCE 3

typedef struct
{
    unsigned int currentMode;
    unsigned int mode;
    unsigned int supportedPowerMizerModes;
} hgmlDevicePowerMizerModes_v1_t;

typedef enum
{
    HGML_GOM_ALL_ON                    = 0,
    HGML_GOM_COMPUTE                   = 1,
    HGML_GOM_LOW_DP                    = 2
} hgmlGpuOperationMode_t;

typedef enum
{
    HGML_INFOROM_OEM            = 0,
    HGML_INFOROM_ECC            = 1,
    HGML_INFOROM_POWER          = 2,
    HGML_INFOROM_DEN            = 3,

    HGML_INFOROM_COUNT
} hgmlInforomObject_t;

typedef enum
{
    HGML_SUCCESS = 0,
    HGML_ERROR_UNINITIALIZED = 1,
    HGML_ERROR_INVALID_ARGUMENT = 2,
    HGML_ERROR_NOT_SUPPORTED = 3,
    HGML_ERROR_NO_PERMISSION = 4,
    HGML_ERROR_ALREADY_INITIALIZED = 5,
    HGML_ERROR_NOT_FOUND = 6,
    HGML_ERROR_INSUFFICIENT_SIZE = 7,
    HGML_ERROR_INSUFFICIENT_POWER = 8,
    HGML_ERROR_DRIVER_NOT_LOADED = 9,
    HGML_ERROR_TIMEOUT = 10,
    HGML_ERROR_IRQ_ISSUE = 11,
    HGML_ERROR_LIBRARY_NOT_FOUND = 12,
    HGML_ERROR_FUNCTION_NOT_FOUND = 13,
    HGML_ERROR_CORRUPTED_INFOROM = 14,
    HGML_ERROR_GPU_IS_LOST = 15,
    HGML_ERROR_RESET_REQUIRED = 16,
    HGML_ERROR_OPERATING_SYSTEM = 17,
    HGML_ERROR_LIB_RM_VERSION_MISMATCH = 18,
    HGML_ERROR_IN_USE = 19,
    HGML_ERROR_MEMORY = 20,
    HGML_ERROR_NO_DATA = 21,
    HGML_ERROR_VGPU_ECC_NOT_SUPPORTED = 22,
    HGML_ERROR_INSUFFICIENT_RESOURCES = 23,
    HGML_ERROR_FREQ_NOT_SUPPORTED = 24,
    HGML_ERROR_ARGUMENT_VERSION_MISMATCH = 25,
    HGML_ERROR_DEPRECATED  = 26,
    HGML_ERROR_NOT_READY = 27,
    HGML_ERROR_GPU_NOT_FOUND = 28,
    HGML_ERROR_INVALID_STATE = 29,
    HGML_ERROR_RESET_TYPE_NOT_SUPPORTED = 30,
    HGML_ERROR_UNKNOWN = 999
} hgmlReturn_t;

typedef enum
{
    HGML_MEMORY_LOCATION_L1_CACHE        = 0,
    HGML_MEMORY_LOCATION_L2_CACHE        = 1,
    HGML_MEMORY_LOCATION_DRAM            = 2,
    HGML_MEMORY_LOCATION_DEVICE_MEMORY   = 2,
    HGML_MEMORY_LOCATION_REGISTER_FILE   = 3,
    HGML_MEMORY_LOCATION_TEXTURE_MEMORY  = 4,
    HGML_MEMORY_LOCATION_TEXTURE_SHM     = 5,
    HGML_MEMORY_LOCATION_CBU             = 6,
    HGML_MEMORY_LOCATION_SRAM            = 7,

    HGML_MEMORY_LOCATION_COUNT
} hgmlMemoryLocation_t;

typedef enum
{
    HGML_PAGE_RETIREMENT_CAUSE_MULTIPLE_SINGLE_BIT_ECC_ERRORS = 0,
    HGML_PAGE_RETIREMENT_CAUSE_DOUBLE_BIT_ECC_ERROR = 1,

    HGML_PAGE_RETIREMENT_CAUSE_COUNT
} hgmlPageRetirementCause_t;

typedef enum
{
    HGML_RESTRICTED_API_SET_APPLICATION_CLOCKS = 0,
    HGML_RESTRICTED_API_SET_AUTO_BOOSTED_CLOCKS = 1,

    HGML_RESTRICTED_API_COUNT
} hgmlRestrictedAPI_t;

typedef struct
{
    unsigned int        pid;
    unsigned long long  timeStamp;
    unsigned int        smUtil;
    unsigned int        memUtil;
    unsigned int        encUtil;
    unsigned int        decUtil;
} hgmlProcessUtilizationSample_t;

typedef struct
{
    unsigned long long  timeStamp;
    unsigned int        pid;
    unsigned int        smUtil;
    unsigned int        memUtil;
    unsigned int        encUtil;
    unsigned int        decUtil;
    unsigned int        jpgUtil;
    unsigned int        ofaUtil;
} hgmlProcessUtilizationInfo_v1_t;

typedef struct
{
    unsigned int version;
    unsigned int processSamplesCount;
    unsigned long long lastSeenTimeStamp;
    hgmlProcessUtilizationInfo_v1_t *procUtilArray;
} hgmlProcessesUtilizationInfo_v1_t;
typedef hgmlProcessesUtilizationInfo_v1_t hgmlProcessesUtilizationInfo_t;
#define hgmlProcessesUtilizationInfo_v1 HGML_STRUCT_VERSION(ProcessesUtilizationInfo, 1)

typedef struct
{
    unsigned int version;
    unsigned long long aggregateUncParity;
    unsigned long long aggregateUncSecDed;
    unsigned long long aggregateCor;
    unsigned long long volatileUncParity;
    unsigned long long volatileUncSecDed;
    unsigned long long volatileCor;
    unsigned long long aggregateUncBucketL2;
    unsigned long long aggregateUncBucketSm;
    unsigned long long aggregateUncBucketPcie;
    unsigned long long aggregateUncBucketMcu;
    unsigned long long aggregateUncBucketOther;
    unsigned int bThresholdExceeded;
} hgmlEccSramErrorStatus_v1_t;

typedef hgmlEccSramErrorStatus_v1_t hgmlEccSramErrorStatus_t;
#define hgmlEccSramErrorStatus_v1 HGML_STRUCT_VERSION(EccSramErrorStatus, 1)

typedef struct
{
    unsigned int version;
    unsigned char ibGuid[16];
    unsigned char rackGuid[16];
    unsigned char chassisPhysicalSlotNumber;
    unsigned char computeSlotIndex;
    unsigned char nodeIndex;
    unsigned char peerType;
    unsigned char moduleId;
} hgmlPlatformInfo_v1_t;
#define hgmlPlatformInfo_v1 HGML_STRUCT_VERSION(PlatformInfo, 1)

typedef struct
{
    unsigned int version;
    unsigned char ibGuid[16];
    unsigned char chassisSerialNumber[16];
    unsigned char slotNumber;
    unsigned char trayIndex;
    unsigned char hostId;
    unsigned char peerType;
    unsigned char moduleId;
} hgmlPlatformInfo_v2_t;

typedef hgmlPlatformInfo_v2_t hgmlPlatformInfo_t;
#define hgmlPlatformInfo_v2 HGML_STRUCT_VERSION(PlatformInfo, 2)

#define HGML_DEVICE_HOSTNAME_BUFFER_SIZE 64

typedef struct
{
    char value[HGML_DEVICE_HOSTNAME_BUFFER_SIZE];
} hgmlHostname_v1_t;

typedef struct
{
    unsigned int unit;
    unsigned int location;
    unsigned int sublocation;
    unsigned int extlocation;
    unsigned int address;
    unsigned int isParity;
    unsigned int count;
} hgmlEccSramUniqueUncorrectedErrorEntry_v1_t;

typedef struct
{
    unsigned int version;
    unsigned int entryCount;
    hgmlEccSramUniqueUncorrectedErrorEntry_v1_t *entries;
} hgmlEccSramUniqueUncorrectedErrorCounts_v1_t;

typedef hgmlEccSramUniqueUncorrectedErrorCounts_v1_t hgmlEccSramUniqueUncorrectedErrorCounts_t;
#define hgmlEccSramUniqueUncorrectedErrorCounts_v1 HGML_STRUCT_VERSION(EccSramUniqueUncorrectedErrorCounts, 1)

#define HGML_GSP_FIRMWARE_VERSION_BUF_SIZE 0x40

#define HGML_DEVICE_ARCH_BEETHOVEN   7

#define HGML_DEVICE_ARCH_UNKNOWN   0xffffffff

typedef unsigned int hgmlDeviceArchitecture_t;

#define HGML_BUS_TYPE_UNKNOWN  0
#define HGML_BUS_TYPE_PCI      1
#define HGML_BUS_TYPE_PCIE     2
#define HGML_BUS_TYPE_FPCI     3
#define HGML_BUS_TYPE_AGP      4

typedef unsigned int hgmlBusType_t;

#define HGML_FAN_POLICY_TEMPERATURE_CONTINOUS_SW 0
#define HGML_FAN_POLICY_MANUAL                   1

typedef unsigned int hgmlFanControlPolicy_t;

#define HGML_POWER_SOURCE_AC         0x00000000
#define HGML_POWER_SOURCE_BATTERY    0x00000001
#define HGML_POWER_SOURCE_UNDERSIZED 0x00000002

typedef unsigned int hgmlPowerSource_t;

#define HGML_PCIE_LINK_MAX_SPEED_INVALID   0x00000000
#define HGML_PCIE_LINK_MAX_SPEED_2500MBPS  0x00000001
#define HGML_PCIE_LINK_MAX_SPEED_5000MBPS  0x00000002
#define HGML_PCIE_LINK_MAX_SPEED_8000MBPS  0x00000003
#define HGML_PCIE_LINK_MAX_SPEED_16000MBPS 0x00000004
#define HGML_PCIE_LINK_MAX_SPEED_32000MBPS 0x00000005
#define HGML_PCIE_LINK_MAX_SPEED_64000MBPS 0x00000006

#define HGML_ADAPTIVE_CLOCKING_INFO_STATUS_DISABLED 0x00000000
#define HGML_ADAPTIVE_CLOCKING_INFO_STATUS_ENABLED  0x00000001

#define HGML_MAX_GPU_UTILIZATIONS 8

typedef enum
{
    HGML_GPU_UTILIZATION_DOMAIN_GPU    = 0,
    HGML_GPU_UTILIZATION_DOMAIN_FB     = 1,
    HGML_GPU_UTILIZATION_DOMAIN_VID    = 2,
    HGML_GPU_UTILIZATION_DOMAIN_BUS    = 3,
} hgmlGpuUtilizationDomainId_t;

typedef struct
{
    unsigned int       flags;
    struct
    {
        unsigned int   bIsPresent;
        unsigned int   percentage;
        unsigned int   incThreshold;
        unsigned int   decThreshold;
    } utilization[HGML_MAX_GPU_UTILIZATIONS];
} hgmlGpuDynamicPstatesInfo_t;

#define HGML_PCIE_ATOMICS_CAP_FETCHADD32  0x01
#define HGML_PCIE_ATOMICS_CAP_FETCHADD64  0x02
#define HGML_PCIE_ATOMICS_CAP_SWAP32      0x04
#define HGML_PCIE_ATOMICS_CAP_SWAP64      0x08
#define HGML_PCIE_ATOMICS_CAP_CAS32       0x10
#define HGML_PCIE_ATOMICS_CAP_CAS64       0x20
#define HGML_PCIE_ATOMICS_CAP_CAS128      0x40
#define HGML_PCIE_ATOMICS_OPS_MAX         7

#define HGML_POWER_SCOPE_GPU     0U
#define HGML_POWER_SCOPE_MODULE  1U
#define HGML_POWER_SCOPE_MEMORY  2U

typedef unsigned char hgmlPowerScopeType_t;

typedef struct
{
    unsigned int         version;
    hgmlPowerScopeType_t powerScope;
    unsigned int         powerValueMw;
} hgmlPowerValue_v2_t;

#define hgmlPowerValue_v2 HGML_STRUCT_VERSION(PowerValue, 2)

typedef enum
{
    HGML_GPU_VIRTUALIZATION_MODE_NONE = 0,
    HGML_GPU_VIRTUALIZATION_MODE_PASSTHROUGH = 1,
    HGML_GPU_VIRTUALIZATION_MODE_VGPU = 2,
    HGML_GPU_VIRTUALIZATION_MODE_HOST_VGPU = 3,
    HGML_GPU_VIRTUALIZATION_MODE_HOST_VSGA = 4
} hgmlGpuVirtualizationMode_t;

typedef enum
{
    HGML_HOST_VGPU_MODE_NON_SRIOV    = 0,
    HGML_HOST_VGPU_MODE_SRIOV        = 1
} hgmlHostVgpuMode_t;

typedef enum
{
    HGML_VGPU_VM_ID_DOMAIN_ID = 0,
    HGML_VGPU_VM_ID_UUID = 1
} hgmlVgpuVmIdType_t;

typedef enum
{
    HGML_VGPU_INSTANCE_GUEST_INFO_STATE_UNINITIALIZED = 0,
    HGML_VGPU_INSTANCE_GUEST_INFO_STATE_INITIALIZED   = 1
} hgmlVgpuGuestInfoState_t;

typedef enum
{
    HGML_VGPU_CAP_ICNLINK_P2P                   = 0,
    HGML_VGPU_CAP_GPUDIRECT                     = 1,
    HGML_VGPU_CAP_MULTI_VGPU_EXCLUSIVE          = 2,
    HGML_VGPU_CAP_EXCLUSIVE_TYPE                = 3,
    HGML_VGPU_CAP_EXCLUSIVE_SIZE                = 4,

    HGML_VGPU_CAP_COUNT
} hgmlVgpuCapability_t;

typedef enum
{
    HGML_VGPU_DRIVER_CAP_HETEROGENEOUS_MULTI_VGPU = 0,
    HGML_VGPU_DRIVER_CAP_WARM_UPDATE              = 1,

    HGML_VGPU_DRIVER_CAP_COUNT
} hgmlVgpuDriverCapability_t;

typedef enum
{
    HGML_DEVICE_VGPU_CAP_FRACTIONAL_MULTI_VGPU            = 0,
    HGML_DEVICE_VGPU_CAP_HETEROGENEOUS_TIMESLICE_PROFILES = 1,
    HGML_DEVICE_VGPU_CAP_HETEROGENEOUS_TIMESLICE_SIZES    = 2,
    HGML_DEVICE_VGPU_CAP_READ_DEVICE_BUFFER_BW            = 3,
    HGML_DEVICE_VGPU_CAP_WRITE_DEVICE_BUFFER_BW           = 4,
    HGML_DEVICE_VGPU_CAP_DEVICE_STREAMING                 = 5,
    HGML_DEVICE_VGPU_CAP_MINI_QUARTER_GPU                 = 6,
    HGML_DEVICE_VGPU_CAP_COMPUTE_MEDIA_ENGINE_GPU         = 7,
    HGML_DEVICE_VGPU_CAP_WARM_UPDATE                      = 8,
    HGML_DEVICE_VGPU_CAP_HOMOGENEOUS_PLACEMENTS           = 9,
    HGML_DEVICE_VGPU_CAP_MIG_TIMESLICING_SUPPORTED        = 10,
    HGML_DEVICE_VGPU_CAP_MIG_TIMESLICING_ENABLED          = 11,

    HGML_DEVICE_VGPU_CAP_COUNT
} hgmlDeviceVgpuCapability_t;

#define HGML_VGPU_NAME_BUFFER_SIZE          64

#define INVALID_GPU_INSTANCE_PROFILE_ID     0xFFFFFFFF

#define INVALID_GPU_INSTANCE_ID             0xFFFFFFFF

#define HGML_INVALID_VGPU_PLACEMENT_ID      0xFFFF

#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION         0:0
#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION_NO      0x0
#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION_YES     0x1

#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION         0:0
#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION_NO      0x0
#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION_YES     0x1

#define HGML_VGPU_PGPU_HETEROGENEOUS_MODE    0
#define HGML_VGPU_PGPU_HOMOGENEOUS_MODE      1

typedef unsigned int hgmlVgpuTypeId_t;

typedef unsigned int hgmlVgpuInstance_t;

typedef struct
{
    unsigned int version;
    unsigned int mode;
} hgmlVgpuHeterogeneousMode_v1_t;
typedef hgmlVgpuHeterogeneousMode_v1_t hgmlVgpuHeterogeneousMode_t;
#define hgmlVgpuHeterogeneousMode_v1 HGML_STRUCT_VERSION(VgpuHeterogeneousMode, 1)

typedef struct
{
    unsigned int version;
    unsigned int placementId;
} hgmlVgpuPlacementId_v1_t;
typedef hgmlVgpuPlacementId_v1_t hgmlVgpuPlacementId_t;
#define hgmlVgpuPlacementId_v1 HGML_STRUCT_VERSION(VgpuPlacementId, 1)

typedef struct
{
    unsigned int version;
    unsigned int placementSize;
    unsigned int count;
    unsigned int *placementIds;
} hgmlVgpuPlacementList_v1_t;
#define hgmlVgpuPlacementList_v1 HGML_STRUCT_VERSION(VgpuPlacementList, 1)

typedef struct
{
    unsigned int version;
    unsigned int placementSize;
    unsigned int count;
    unsigned int *placementIds;
    unsigned int mode;
} hgmlVgpuPlacementList_v2_t;
typedef hgmlVgpuPlacementList_v2_t hgmlVgpuPlacementList_t;
#define hgmlVgpuPlacementList_v2 HGML_STRUCT_VERSION(VgpuPlacementList, 2)

typedef struct
{
    unsigned int version;
    unsigned long long  bar1Size;
} hgmlVgpuTypeBar1Info_v1_t;
typedef hgmlVgpuTypeBar1Info_v1_t hgmlVgpuTypeBar1Info_t;
#define hgmlVgpuTypeBar1Info_v1 HGML_STRUCT_VERSION(VgpuTypeBar1Info, 1)

typedef struct
{
    hgmlVgpuInstance_t  vgpuInstance;
    unsigned long long  timeStamp;
    hgmlValue_t         smUtil;
    hgmlValue_t         memUtil;
    hgmlValue_t         encUtil;
    hgmlValue_t         decUtil;
} hgmlVgpuInstanceUtilizationSample_t;

typedef struct
{
    unsigned long long  timeStamp;
    hgmlVgpuInstance_t  vgpuInstance;
    hgmlValue_t         smUtil;
    hgmlValue_t         memUtil;
    hgmlValue_t         encUtil;
    hgmlValue_t         decUtil;
    hgmlValue_t         jpgUtil;
    hgmlValue_t         ofaUtil;
} hgmlVgpuInstanceUtilizationInfo_v1_t;

typedef struct
{
    unsigned int version;
    hgmlValueType_t sampleValType;
    unsigned int vgpuInstanceCount;
    unsigned long long lastSeenTimeStamp;
    hgmlVgpuInstanceUtilizationInfo_v1_t *vgpuUtilArray;
} hgmlVgpuInstancesUtilizationInfo_v1_t;
typedef hgmlVgpuInstancesUtilizationInfo_v1_t hgmlVgpuInstancesUtilizationInfo_t;
#define hgmlVgpuInstancesUtilizationInfo_v1 HGML_STRUCT_VERSION(VgpuInstancesUtilizationInfo, 1)

typedef struct
{
    hgmlVgpuInstance_t  vgpuInstance;
    unsigned int        pid;
    char                processName[HGML_VGPU_NAME_BUFFER_SIZE];
    unsigned long long  timeStamp;
    unsigned int        smUtil;
    unsigned int        memUtil;
    unsigned int        encUtil;
    unsigned int        decUtil;
} hgmlVgpuProcessUtilizationSample_t;

typedef struct
{
    char                processName[HGML_VGPU_NAME_BUFFER_SIZE];
    unsigned long long  timeStamp;
    hgmlVgpuInstance_t  vgpuInstance;
    unsigned int        pid;
    unsigned int        smUtil;
    unsigned int        memUtil;
    unsigned int        encUtil;
    unsigned int        decUtil;
    unsigned int        jpgUtil;
    unsigned int        ofaUtil;
} hgmlVgpuProcessUtilizationInfo_v1_t;

typedef struct
{
    unsigned int version;
    unsigned int vgpuProcessCount;
    unsigned long long lastSeenTimeStamp;
    hgmlVgpuProcessUtilizationInfo_v1_t *vgpuProcUtilArray;
} hgmlVgpuProcessesUtilizationInfo_v1_t;
typedef hgmlVgpuProcessesUtilizationInfo_v1_t hgmlVgpuProcessesUtilizationInfo_t;
#define hgmlVgpuProcessesUtilizationInfo_v1 HGML_STRUCT_VERSION(VgpuProcessesUtilizationInfo, 1)

typedef struct
{
    unsigned int version;
    unsigned long long size;
} hgmlVgpuRuntimeState_v1_t;
typedef hgmlVgpuRuntimeState_v1_t hgmlVgpuRuntimeState_t;
#define hgmlVgpuRuntimeState_v1 HGML_STRUCT_VERSION(VgpuRuntimeState, 1)

#define HGML_VGPU_SCHEDULER_POLICY_UNKNOWN      0
#define HGML_VGPU_SCHEDULER_POLICY_BEST_EFFORT  1
#define HGML_VGPU_SCHEDULER_POLICY_EQUAL_SHARE  2
#define HGML_VGPU_SCHEDULER_POLICY_FIXED_SHARE  3

#define HGML_SUPPORTED_VGPU_SCHEDULER_POLICY_COUNT 3

#define HGML_SCHEDULER_SW_MAX_LOG_ENTRIES 200

#define HGML_VGPU_SCHEDULER_ARR_DEFAULT   0
#define HGML_VGPU_SCHEDULER_ARR_DISABLE   1
#define HGML_VGPU_SCHEDULER_ARR_ENABLE    2

#define HGML_VGPU_SCHEDULER_ENGINE_TYPE_GRAPHICS  1

typedef union
{
    struct
    {
        unsigned int    avgFactor;
        unsigned int    timeslice;
    } vgpuSchedDataWithARR;

    struct
    {
        unsigned int    timeslice;
    } vgpuSchedData;

} hgmlVgpuSchedulerParams_t;

typedef struct
{
    unsigned long long          timestamp;
    unsigned long long          timeRunTotal;
    unsigned long long          timeRun;
    unsigned int                swRunlistId;
    unsigned long long          targetTimeSlice;
    unsigned long long          cumulativePreemptionTime;
} hgmlVgpuSchedulerLogEntry_t;

typedef struct
{
    unsigned int                engineId;
    unsigned int                schedulerPolicy;
    unsigned int                arrMode;
    hgmlVgpuSchedulerParams_t   schedulerParams;
    unsigned int                entriesCount;
    hgmlVgpuSchedulerLogEntry_t logEntries[HGML_SCHEDULER_SW_MAX_LOG_ENTRIES];
} hgmlVgpuSchedulerLog_t;

typedef struct
{
    unsigned int                schedulerPolicy;
    unsigned int                arrMode;
    hgmlVgpuSchedulerParams_t   schedulerParams;
} hgmlVgpuSchedulerGetState_t;

typedef union
{
    struct
    {
        unsigned int    avgFactor;
        unsigned int    frequency;
    } vgpuSchedDataWithARR;

    struct
    {
        unsigned int    timeslice;
    } vgpuSchedData;

} hgmlVgpuSchedulerSetParams_t;

typedef struct
{
    unsigned int                    schedulerPolicy;
    unsigned int                    enableARRMode;
    hgmlVgpuSchedulerSetParams_t    schedulerParams;
} hgmlVgpuSchedulerSetState_t;

typedef struct
{
    unsigned int        supportedSchedulers[HGML_SUPPORTED_VGPU_SCHEDULER_POLICY_COUNT];
    unsigned int        maxTimeslice;
    unsigned int        minTimeslice;
    unsigned int        isArrModeSupported;
    unsigned int        maxFrequencyForARR;
    unsigned int        minFrequencyForARR;
    unsigned int        maxAvgFactorForARR;
    unsigned int        minAvgFactorForARR;
} hgmlVgpuSchedulerCapabilities_t;

typedef struct
{
    unsigned int    year;
    unsigned short  month;
    unsigned short  day;
    unsigned short  hour;
    unsigned short  min;
    unsigned short  sec;
    unsigned char   status;
} hgmlVgpuLicenseExpiry_t;

typedef struct
{
    unsigned char               isLicensed;
    hgmlVgpuLicenseExpiry_t     licenseExpiry;
    unsigned int                currentState;
} hgmlVgpuLicenseInfo_t;

typedef enum
{
    HGML_GPU_RECOVERY_ACTION_NONE = 0,
    HGML_GPU_RECOVERY_ACTION_GPU_RESET = 1,
    HGML_GPU_RECOVERY_ACTION_NODE_REBOOT = 2,
    HGML_GPU_RECOVERY_ACTION_DRAIN_P2P = 3,
    HGML_GPU_RECOVERY_ACTION_DRAIN_AND_RESET = 4,

    HGML_GPU_RECOVERY_ACTION_COLD_REBOOT = 20,
} hgmlDeviceGpuRecoveryAction_t;

typedef struct
{
    unsigned int        version;
    unsigned int        vgpuCount;
    hgmlVgpuTypeId_t    *vgpuTypeIds;
} hgmlVgpuTypeIdInfo_v1_t;
typedef hgmlVgpuTypeIdInfo_v1_t hgmlVgpuTypeIdInfo_t;
#define hgmlVgpuTypeIdInfo_v1 HGML_STRUCT_VERSION(VgpuTypeIdInfo, 1)

typedef struct
{
    unsigned int        version;
    hgmlVgpuTypeId_t    vgpuTypeId;
    unsigned int        maxInstancePerGI;
} hgmlVgpuTypeMaxInstance_v1_t;
typedef hgmlVgpuTypeMaxInstance_v1_t hgmlVgpuTypeMaxInstance_t;
#define hgmlVgpuTypeMaxInstance_v1 HGML_STRUCT_VERSION(VgpuTypeMaxInstance, 1)

typedef struct
{
    unsigned int       version;
    unsigned int       vgpuCount;
    hgmlVgpuInstance_t *vgpuInstances;
} hgmlActiveVgpuInstanceInfo_v1_t;
typedef hgmlActiveVgpuInstanceInfo_v1_t hgmlActiveVgpuInstanceInfo_t;
#define hgmlActiveVgpuInstanceInfo_v1 HGML_STRUCT_VERSION(ActiveVgpuInstanceInfo, 1)

typedef struct
{
    unsigned int                    version;
    unsigned int                    engineId;
    unsigned int                    schedulerPolicy;
    unsigned int                    enableARRMode;
    hgmlVgpuSchedulerSetParams_t    schedulerParams;
} hgmlVgpuSchedulerState_v1_t;
typedef hgmlVgpuSchedulerState_v1_t hgmlVgpuSchedulerState_t;
#define hgmlVgpuSchedulerState_v1 HGML_STRUCT_VERSION(VgpuSchedulerState, 1)

typedef struct
{
    unsigned int                version;
    unsigned int                engineId;
    unsigned int                schedulerPolicy;
    unsigned int                arrMode;
    hgmlVgpuSchedulerParams_t   schedulerParams;
} hgmlVgpuSchedulerStateInfo_v1_t;
typedef hgmlVgpuSchedulerStateInfo_v1_t hgmlVgpuSchedulerStateInfo_t;
#define hgmlVgpuSchedulerStateInfo_v1 HGML_STRUCT_VERSION(VgpuSchedulerStateInfo, 1)

typedef struct
{
    unsigned int                version;
    unsigned int                engineId;
    unsigned int                schedulerPolicy;
    unsigned int                arrMode;
    hgmlVgpuSchedulerParams_t   schedulerParams;
    unsigned int                entriesCount;
    hgmlVgpuSchedulerLogEntry_t logEntries[HGML_SCHEDULER_SW_MAX_LOG_ENTRIES];
} hgmlVgpuSchedulerLogInfo_v1_t;
typedef hgmlVgpuSchedulerLogInfo_v1_t hgmlVgpuSchedulerLogInfo_t;
#define hgmlVgpuSchedulerLogInfo_v1 HGML_STRUCT_VERSION(VgpuSchedulerLogInfo, 1)

typedef struct
{
    unsigned int     version;
    hgmlVgpuTypeId_t vgpuTypeId;
    unsigned int     count;
    unsigned int     *placementIds;
    unsigned int     placementSize;
} hgmlVgpuCreatablePlacementInfo_v1_t;
typedef hgmlVgpuCreatablePlacementInfo_v1_t hgmlVgpuCreatablePlacementInfo_t;
#define hgmlVgpuCreatablePlacementInfo_v1 HGML_STRUCT_VERSION(VgpuCreatablePlacementInfo, 1)

#define HGML_FI_DEV_ECC_CURRENT           1
#define HGML_FI_DEV_ECC_PENDING           2
#define HGML_FI_DEV_ECC_SBE_VOL_TOTAL     3
#define HGML_FI_DEV_ECC_DBE_VOL_TOTAL     4
#define HGML_FI_DEV_ECC_SBE_AGG_TOTAL     5
#define HGML_FI_DEV_ECC_DBE_AGG_TOTAL     6
#define HGML_FI_DEV_ECC_SBE_VOL_L1        7
#define HGML_FI_DEV_ECC_DBE_VOL_L1        8
#define HGML_FI_DEV_ECC_SBE_VOL_L2        9
#define HGML_FI_DEV_ECC_DBE_VOL_L2        10
#define HGML_FI_DEV_ECC_SBE_VOL_DEV       11
#define HGML_FI_DEV_ECC_DBE_VOL_DEV       12
#define HGML_FI_DEV_ECC_SBE_VOL_REG       13
#define HGML_FI_DEV_ECC_DBE_VOL_REG       14
#define HGML_FI_DEV_ECC_SBE_VOL_TEX       15
#define HGML_FI_DEV_ECC_DBE_VOL_TEX       16
#define HGML_FI_DEV_ECC_DBE_VOL_CBU       17
#define HGML_FI_DEV_ECC_SBE_AGG_L1        18
#define HGML_FI_DEV_ECC_DBE_AGG_L1        19
#define HGML_FI_DEV_ECC_SBE_AGG_L2        20
#define HGML_FI_DEV_ECC_DBE_AGG_L2        21
#define HGML_FI_DEV_ECC_SBE_AGG_DEV       22
#define HGML_FI_DEV_ECC_DBE_AGG_DEV       23
#define HGML_FI_DEV_ECC_SBE_AGG_REG       24
#define HGML_FI_DEV_ECC_DBE_AGG_REG       25
#define HGML_FI_DEV_ECC_SBE_AGG_TEX       26
#define HGML_FI_DEV_ECC_DBE_AGG_TEX       27
#define HGML_FI_DEV_ECC_DBE_AGG_CBU       28

#define HGML_FI_DEV_RETIRED_SBE           29
#define HGML_FI_DEV_RETIRED_DBE           30
#define HGML_FI_DEV_RETIRED_PENDING       31

#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L0    32
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L1    33
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L2    34
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L3    35
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L4    36
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L5    37
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_TOTAL 38

#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L0    39
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L1    40
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L2    41
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L3    42
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L4    43
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L5    44
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_TOTAL 45

#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L0      46
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L1      47
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L2      48
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L3      49
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L4      50
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L5      51
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_TOTAL   52

#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L0    53
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L1    54
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L2    55
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L3    56
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L4    57
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L5    58
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_TOTAL 59

#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L0     60
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L1     61
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L2     62
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L3     63
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L4     64
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L5     65
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_TOTAL  66

#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L0     67
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L1     68
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L2     69
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L3     70
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L4     71
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L5     72
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_TOTAL  73

#define HGML_FI_DEV_PERF_POLICY_POWER              74
#define HGML_FI_DEV_PERF_POLICY_THERMAL            75
#define HGML_FI_DEV_PERF_POLICY_SYNC_BOOST         76
#define HGML_FI_DEV_PERF_POLICY_BOARD_LIMIT        77
#define HGML_FI_DEV_PERF_POLICY_LOW_UTILIZATION    78
#define HGML_FI_DEV_PERF_POLICY_RELIABILITY        79
#define HGML_FI_DEV_PERF_POLICY_TOTAL_APP_CLOCKS   80
#define HGML_FI_DEV_PERF_POLICY_TOTAL_BASE_CLOCKS  81

#define HGML_FI_DEV_MEMORY_TEMP  82

#define HGML_FI_DEV_TOTAL_ENERGY_CONSUMPTION 83

#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L0     84
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L1     85
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L2     86
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L3     87
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L4     88
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L5     89
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_COMMON 90

#define HGML_FI_DEV_ICNLINK_LINK_COUNT        91

#define HGML_FI_DEV_RETIRED_PENDING_SBE      92
#define HGML_FI_DEV_RETIRED_PENDING_DBE      93

#define HGML_FI_DEV_PCIE_REPLAY_COUNTER             94
#define HGML_FI_DEV_PCIE_REPLAY_ROLLOVER_COUNTER    95

#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L6     96
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L7     97
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L8     98
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L9     99
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L10   100
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L11   101

#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L6    102
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L7    103
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L8    104
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L9    105
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L10   106
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L11   107

#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L6      108
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L7      109
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L8      110
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L9      111
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L10     112
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L11     113

#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L6    114
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L7    115
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L8    116
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L9    117
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L10   118
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L11   119

#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L6     120
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L7     121
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L8     122
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L9     123
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L10    124
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L11    125

#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L6     126
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L7     127
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L8     128
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L9     129
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L10    130
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L11    131

#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L6     132
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L7     133
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L8     134
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L9     135
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L10    136
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L11    137

#define HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_TX      138
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_RX      139
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_TX       140
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_RX       141

#define HGML_FI_DEV_REMAPPED_COR        142
#define HGML_FI_DEV_REMAPPED_UNC        143
#define HGML_FI_DEV_REMAPPED_PENDING    144
#define HGML_FI_DEV_REMAPPED_FAILURE    145

#define HGML_FI_DEV_ICNLINK_REMOTE_ICNLINK_ID     146

#define HGML_FI_DEV_ICNSWITCH_CONNECTED_LINK_COUNT   147

#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L0    148
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L1    149
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L2    150
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L3    151
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L4    152
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L5    153
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L6    154
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L7    155
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L8    156
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L9    157
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L10   158
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L11   159
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_TOTAL 160

#define HGML_FI_DEV_ICNLINK_ERROR_DL_REPLAY            161

#define HGML_FI_DEV_ICNLINK_ERROR_DL_RECOVERY          162

#define HGML_FI_DEV_ICNLINK_ERROR_DL_CRC               163

#define HGML_FI_DEV_ICNLINK_GET_SPEED                  164
#define HGML_FI_DEV_ICNLINK_GET_STATE                  165
#define HGML_FI_DEV_ICNLINK_GET_VERSION                166

#define HGML_FI_DEV_ICNLINK_GET_POWER_STATE            167
#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD        168

#define HGML_FI_DEV_PCIE_L0_TO_RECOVERY_COUNTER       169

#define HGML_FI_DEV_C2C_LINK_COUNT                    170
#define HGML_FI_DEV_C2C_LINK_GET_STATUS               171
#define HGML_FI_DEV_C2C_LINK_GET_MAX_BW               172

#define HGML_FI_DEV_PCIE_COUNT_CORRECTABLE_ERRORS     173
#define HGML_FI_DEV_PCIE_COUNT_NAKS_RECEIVED          174
#define HGML_FI_DEV_PCIE_COUNT_RECEIVER_ERROR         175
#define HGML_FI_DEV_PCIE_COUNT_BAD_TLP                176
#define HGML_FI_DEV_PCIE_COUNT_NAKS_SENT              177
#define HGML_FI_DEV_PCIE_COUNT_BAD_DLLP               178
#define HGML_FI_DEV_PCIE_COUNT_NON_FATAL_ERROR        179
#define HGML_FI_DEV_PCIE_COUNT_FATAL_ERROR            180
#define HGML_FI_DEV_PCIE_COUNT_UNSUPPORTED_REQ        181
#define HGML_FI_DEV_PCIE_COUNT_LCRC_ERROR             182
#define HGML_FI_DEV_PCIE_COUNT_LANE_ERROR             183

#define HGML_FI_DEV_IS_RESETLESS_MIG_SUPPORTED        184

#define HGML_FI_DEV_POWER_AVERAGE                     185
#define HGML_FI_DEV_POWER_INSTANT                     186
#define HGML_FI_DEV_POWER_MIN_LIMIT                   187
#define HGML_FI_DEV_POWER_MAX_LIMIT                   188
#define HGML_FI_DEV_POWER_DEFAULT_LIMIT               189
#define HGML_FI_DEV_POWER_CURRENT_LIMIT               190
#define HGML_FI_DEV_ENERGY                            191
#define HGML_FI_DEV_POWER_REQUESTED_LIMIT             192

#define HGML_FI_DEV_TEMPERATURE_SHUTDOWN_TLIMIT       193
#define HGML_FI_DEV_TEMPERATURE_SLOWDOWN_TLIMIT       194
#define HGML_FI_DEV_TEMPERATURE_MEM_MAX_TLIMIT        195
#define HGML_FI_DEV_TEMPERATURE_GPU_MAX_TLIMIT        196

#define HGML_FI_DEV_PCIE_COUNT_TX_BYTES               197
#define HGML_FI_DEV_PCIE_COUNT_RX_BYTES               198

#define HGML_FI_DEV_IS_MIG_MODE_INDEPENDENT_MIG_QUERY_CAPABLE   199

#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD_MAX              200

#define HGML_FI_DEV_ICNLINK_COUNT_XMIT_PACKETS                    201
#define HGML_FI_DEV_ICNLINK_COUNT_XMIT_BYTES                      202
#define HGML_FI_DEV_ICNLINK_COUNT_RCV_PACKETS                     203
#define HGML_FI_DEV_ICNLINK_COUNT_RCV_BYTES                       204
#define HGML_FI_DEV_ICNLINK_COUNT_VL15_DROPPED                    205
#define HGML_FI_DEV_ICNLINK_COUNT_MALFORMED_PACKET_ERRORS         206
#define HGML_FI_DEV_ICNLINK_COUNT_BUFFER_OVERRUN_ERRORS           207
#define HGML_FI_DEV_ICNLINK_COUNT_RCV_ERRORS                      208
#define HGML_FI_DEV_ICNLINK_COUNT_RCV_REMOTE_ERRORS               209
#define HGML_FI_DEV_ICNLINK_COUNT_RCV_GENERAL_ERRORS              210
#define HGML_FI_DEV_ICNLINK_COUNT_LOCAL_LINK_INTEGRITY_ERRORS     211
#define HGML_FI_DEV_ICNLINK_COUNT_XMIT_DISCARDS                   212

#define HGML_FI_DEV_ICNLINK_COUNT_LINK_RECOVERY_SUCCESSFUL_EVENTS 213
#define HGML_FI_DEV_ICNLINK_COUNT_LINK_RECOVERY_FAILED_EVENTS     214
#define HGML_FI_DEV_ICNLINK_COUNT_LINK_RECOVERY_EVENTS            215

#define HGML_FI_DEV_ICNLINK_COUNT_RAW_BER_LANE0                   216
#define HGML_FI_DEV_ICNLINK_COUNT_RAW_BER_LANE1                   217
#define HGML_FI_DEV_ICNLINK_COUNT_RAW_BER                         218
#define HGML_FI_DEV_ICNLINK_COUNT_EFFECTIVE_ERRORS                219

#define HGML_FI_DEV_ICNLINK_COUNT_EFFECTIVE_BER                   220
#define HGML_FI_DEV_ICNLINK_COUNT_SYMBOL_ERRORS                   221

#define HGML_FI_DEV_ICNLINK_COUNT_SYMBOL_BER                      222

#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD_MIN               223
#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD_UNITS             224
#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD_SUPPORTED         225

#define HGML_FI_DEV_RESET_STATUS                                 226
#define HGML_FI_DEV_DRAIN_AND_RESET_STATUS                       227
#define HGML_FI_DEV_PCIE_OUTBOUND_ATOMICS_MASK                   228
#define HGML_FI_DEV_PCIE_INBOUND_ATOMICS_MASK                    229
#define HGML_FI_DEV_GET_GPU_RECOVERY_ACTION                      230
#define HGML_FI_DEV_C2C_LINK_ERROR_INTR                          231
#define HGML_FI_DEV_C2C_LINK_ERROR_REPLAY                        232
#define HGML_FI_DEV_C2C_LINK_ERROR_REPLAY_B2B                    233
#define HGML_FI_DEV_C2C_LINK_POWER_STATE                         234

#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_0                   235
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_1                   236
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_2                   237
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_3                   238
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_4                   239
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_5                   240
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_6                   241
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_7                   242
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_8                   243
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_9                   244
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_10                  245
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_11                  246
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_12                  247
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_13                  248
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_14                  249
#define HGML_FI_DEV_ICNLINK_COUNT_FEC_HISTORY_15                  250
#define HGML_FI_PWR_SMOOTHING_ENABLED                                   251
#define HGML_FI_PWR_SMOOTHING_PRIV_LVL                                  252
#define HGML_FI_PWR_SMOOTHING_IMM_RAMP_DOWN_ENABLED                     253
#define HGML_FI_PWR_SMOOTHING_APPLIED_TMP_CEIL                          254
#define HGML_FI_PWR_SMOOTHING_APPLIED_TMP_FLOOR                         255
#define HGML_FI_PWR_SMOOTHING_MAX_PERCENT_TMP_FLOOR_SETTING             256
#define HGML_FI_PWR_SMOOTHING_MIN_PERCENT_TMP_FLOOR_SETTING             257
#define HGML_FI_PWR_SMOOTHING_HW_CIRCUITRY_PERCENT_LIFETIME_REMAINING   258
#define HGML_FI_PWR_SMOOTHING_MAX_NUM_PRESET_PROFILES                   259
#define HGML_FI_PWR_SMOOTHING_PROFILE_PERCENT_TMP_FLOOR                 260
#define HGML_FI_PWR_SMOOTHING_PROFILE_RAMP_UP_RATE                      261
#define HGML_FI_PWR_SMOOTHING_PROFILE_RAMP_DOWN_RATE                    262
#define HGML_FI_PWR_SMOOTHING_PROFILE_RAMP_DOWN_HYST_VAL                263
#define HGML_FI_PWR_SMOOTHING_ACTIVE_PRESET_PROFILE                     264
#define HGML_FI_PWR_SMOOTHING_ADMIN_OVERRIDE_PERCENT_TMP_FLOOR          265
#define HGML_FI_PWR_SMOOTHING_ADMIN_OVERRIDE_RAMP_UP_RATE               266
#define HGML_FI_PWR_SMOOTHING_ADMIN_OVERRIDE_RAMP_DOWN_RATE             267
#define HGML_FI_PWR_SMOOTHING_ADMIN_OVERRIDE_RAMP_DOWN_HYST_VAL         268

#define HGML_FI_DEV_CLOCKS_EVENT_REASON_SW_POWER_CAP             HGML_FI_DEV_PERF_POLICY_POWER
#define HGML_FI_DEV_CLOCKS_EVENT_REASON_SYNC_BOOST               HGML_FI_DEV_PERF_POLICY_SYNC_BOOST
#define HGML_FI_DEV_CLOCKS_EVENT_REASON_SW_THERM_SLOWDOWN        269
#define HGML_FI_DEV_CLOCKS_EVENT_REASON_HW_THERM_SLOWDOWN        270
#define HGML_FI_DEV_CLOCKS_EVENT_REASON_HW_POWER_BRAKE_SLOWDOWN  271
#define HGML_FI_DEV_POWER_SYNC_BALANCING_FREQ                    272
#define HGML_FI_DEV_POWER_SYNC_BALANCING_AF                      273

#define HGML_FI_DEV_ICNLINK_CABLE_STATUS              500
#define HGML_FI_DEV_ICNLINK_LANE_WIDTH                501
#define HGML_FI_DEV_ICNLINK_LINKUP_COUNT              502
#define HGML_FI_DEV_ICNLINK_LINKDOWN_COUNT            503
#define HGML_FI_DEV_ICNLINK_FECC_ERROR_COUNT          504
#define HGML_FI_DEV_ICNLINK_FECU_ERROR_COUNT          505
#define HGML_FI_DEV_ICNLINK_PACKET_ERROR_TX           506
#define HGML_FI_DEV_ICNLINK_PACKET_ERROR_RX           507
#define HGML_FI_DEV_ICNLINK_PACKET_TOTAL_TX           508
#define HGML_FI_DEV_ICNLINK_PACKET_TOTAL_RX           509

#define HGML_FI_DEV_CORE_UTILIZATION                  510
#define HGML_FI_DEV_AUTO_RESET_STATUS                 511
#define HGML_FI_DEV_ICNLINK_PHYSICAL_PORT             512
#define HGML_FI_DEV_OVERCLOCKING_MODE                 513
#define HGML_FI_DEV_HBM_VENDOR                        514
#define HGML_FI_DEV_BASE_CLOCK                        515
#define HGML_FI_DEV_REAR_ID                           516

#define HGML_FI_DEV_XID_PPU_RESET                     517
#define HGML_FI_DEV_XID_OS_REBOOT                     518
#define HGML_FI_DEV_XID_COLD_REBOOT                   519

#define HGML_FI_DEV_PCM_ENABLED                       520
#define HGML_FI_DEV_GPM_ENABLED                       521
#define HGML_FI_DEV_TIDE_MODE_STATUS                  522
#define HGML_FI_DEV_MPS_MODE_STATUS                   523
#define HGML_FI_DEV_ICNLINK_RX_BANDWIDTH_TOTAL        524

#define HGML_FI_MAX                                   525

#define HGML_ICNLINK_LOW_POWER_THRESHOLD_UNIT_100US 0x0
#define HGML_ICNLINK_LOW_POWER_THRESHOLD_UNIT_50US  0x1

#define HGML_ICNLINK_POWER_STATE_HIGH_SPEED    0x0
#define HGML_ICNLINK_POWER_STATE_LOW           0x1

#define HGML_ICNLINK_LOW_POWER_THRESHOLD_MIN   0x1

#define HGML_ICNLINK_LOW_POWER_THRESHOLD_MAX   0x1FFF
#define HGML_ICNLINK_LOW_POWER_THRESHOLD_RESET 0xFFFFFFFF
#define HGML_ICNLINK_LOW_POWER_THRESHOLD_DEFAULT HGML_ICNLINK_LOW_POWER_THRESHOLD_RESET

typedef struct
{
    unsigned int lowPwrThreshold;

} hgmlIcnLinkPowerThres_t;

#define HGML_C2C_POWER_STATE_FULL_POWER 0
#define HGML_C2C_POWER_STATE_LOW_POWER 1

typedef struct
{
    unsigned int fieldId;
    unsigned int scopeId;
    long long timestamp;
    long long latencyUsec;
    hgmlValueType_t valueType;
    hgmlReturn_t hgmlReturn;
    hgmlValue_t value;
} hgmlFieldValue_t;

typedef struct
{
    struct hgmlUnit_st* handle;
} hgmlUnit_t;

typedef struct
{
    unsigned int hwbcId;
    char firmwareVersion[32];
} hgmlHwbcEntry_t;

typedef enum
{
    HGML_FAN_NORMAL       = 0,
    HGML_FAN_FAILED       = 1
} hgmlFanState_t;

typedef enum
{
    HGML_LED_COLOR_GREEN       = 0,
    HGML_LED_COLOR_AMBER       = 1
} hgmlLedColor_t;

typedef struct
{
    char cause[256];
    hgmlLedColor_t color;
} hgmlLedState_t;

typedef struct
{
    char name[96];
    char id[96];
    char serial[96];
    char firmwareVersion[96];
} hgmlUnitInfo_t;

typedef struct
{
    char state[256];
    unsigned int current;
    unsigned int voltage;
    unsigned int power;
} hgmlPSUInfo_t;

typedef struct
{
    unsigned int speed;
    hgmlFanState_t state;
} hgmlUnitFanInfo_t;

typedef struct
{
    hgmlUnitFanInfo_t fans[24];
    unsigned int count;
} hgmlUnitFanSpeeds_t;

typedef struct
{
    struct hgmlEventSet_st* handle;
} hgmlEventSet_t;

#define hgmlEventTypeNone                       0x0000000000000000LL
#define hgmlEventTypeSingleBitEccError          0x0000000000000001LL
#define hgmlEventTypeDoubleBitEccError          0x0000000000000002LL
#define hgmlEventTypePState                     0x0000000000000004LL
#define hgmlEventTypeXidCriticalError           0x0000000000000008LL
#define hgmlEventTypeClock                      0x0000000000000010LL
#define hgmlEventTypePowerSourceChange          0x0000000000000080LL
#define hgmlEventMigConfigChange                0x0000000000000100LL
#define hgmlEventTypeSingleBitEccErrorStorm     0x0000000000000200LL
#define hgmlEventTypeDramRetirementEvent        0x0000000000000400LL
#define hgmlEventTypeDramRetirementFailure      0x0000000000000800LL
#define hgmlEventTypeNonFatalPoisonError        0x0000000000001000LL
#define hgmlEventTypeFatalPoisonError           0x0000000000002000LL
#define hgmlEventTypeGpuUnavailableError        0x0000000000004000LL
#define hgmlEventTypeGpuRecoveryAction          0x0000000000008000LL
#define hgmlEventTypeAll (hgmlEventTypeNone    \
        | hgmlEventTypeSingleBitEccError       \
        | hgmlEventTypeDoubleBitEccError       \
        | hgmlEventTypePState                  \
        | hgmlEventTypeClock                   \
        | hgmlEventTypeXidCriticalError        \
        | hgmlEventTypePowerSourceChange       \
        | hgmlEventMigConfigChange             \
        | hgmlEventTypeSingleBitEccErrorStorm  \
        | hgmlEventTypeDramRetirementEvent     \
        | hgmlEventTypeDramRetirementFailure   \
        | hgmlEventTypeNonFatalPoisonError     \
        | hgmlEventTypeFatalPoisonError        \
        | hgmlEventTypeGpuUnavailableError     \
        | hgmlEventTypeGpuRecoveryAction)

typedef struct
{
    hgmlDevice_t        device;
    unsigned long long  eventType;
    unsigned long long  eventData;
    unsigned int        gpuInstanceId;
    unsigned int        computeInstanceId;
} hgmlEventData_t;

typedef struct
{
    struct hgmlSystemEventSet_st* handle;
} hgmlSystemEventSet_t;

#define hgmlSystemEventTypeGpuDriverUnbind  0x0000000000000001LL
#define hgmlSystemEventTypeGpuDriverBind    0x0000000000000002LL

#define hgmlSystemEventTypeCount 2

typedef struct
{
    unsigned int version;
    hgmlSystemEventSet_t set;
} hgmlSystemEventSetCreateRequest_v1_t;
typedef hgmlSystemEventSetCreateRequest_v1_t hgmlSystemEventSetCreateRequest_t;
#define hgmlSystemEventSetCreateRequest_v1 HGML_STRUCT_VERSION(SystemEventSetCreateRequest, 1)

typedef struct
{
    unsigned int version;
    hgmlSystemEventSet_t set;
} hgmlSystemEventSetFreeRequest_v1_t;
typedef hgmlSystemEventSetFreeRequest_v1_t hgmlSystemEventSetFreeRequest_t;
#define hgmlSystemEventSetFreeRequest_v1 HGML_STRUCT_VERSION(SystemEventSetFreeRequest, 1)

typedef struct
{
    unsigned int version;
    unsigned long long eventTypes;
    hgmlSystemEventSet_t set;
} hgmlSystemRegisterEventRequest_v1_t;
typedef hgmlSystemRegisterEventRequest_v1_t hgmlSystemRegisterEventRequest_t;
#define hgmlSystemRegisterEventRequest_v1 HGML_STRUCT_VERSION(SystemRegisterEventRequest, 1)

typedef struct
{
    unsigned long long  eventType;
    unsigned int gpuId;
} hgmlSystemEventData_v1_t;

typedef struct
{
    unsigned int version;
    unsigned int timeoutms;
    hgmlSystemEventSet_t set;
    hgmlSystemEventData_v1_t *data;
    unsigned int dataSize;
    unsigned int numEvent;
} hgmlSystemEventSetWaitRequest_v1_t;
typedef hgmlSystemEventSetWaitRequest_v1_t hgmlSystemEventSetWaitRequest_t;
#define hgmlSystemEventSetWaitRequest_v1 HGML_STRUCT_VERSION(SystemEventSetWaitRequest, 1)

#define hgmlClocksEventReasonGpuIdle                   0x0000000000000001LL
#define hgmlClocksEventReasonApplicationsClocksSetting 0x0000000000000002LL
#define hgmlClocksThrottleReasonUserDefinedClocks      hgmlClocksEventReasonApplicationsClocksSetting
#define hgmlClocksEventReasonSwPowerCap                0x0000000000000004LL
#define hgmlClocksThrottleReasonHwSlowdown             0x0000000000000008LL
#define hgmlClocksEventReasonSyncBoost                 0x0000000000000010LL
#define hgmlClocksEventReasonSwThermalSlowdown         0x0000000000000020LL
#define hgmlClocksThrottleReasonHwThermalSlowdown      0x0000000000000040LL
#define hgmlClocksThrottleReasonHwPowerBrakeSlowdown   0x0000000000000080LL
#define hgmlClocksEventReasonDisplayClockSetting       0x0000000000000100LL
#define hgmlClocksEventReasonNone                      0x0000000000000000LL
#define hgmlClocksEventReasonAll (hgmlClocksThrottleReasonNone \
      | hgmlClocksEventReasonGpuIdle                           \
      | hgmlClocksEventReasonApplicationsClocksSetting         \
      | hgmlClocksEventReasonSwPowerCap                        \
      | hgmlClocksThrottleReasonHwSlowdown                     \
      | hgmlClocksEventReasonSyncBoost                         \
      | hgmlClocksEventReasonSwThermalSlowdown                 \
      | hgmlClocksThrottleReasonHwThermalSlowdown              \
      | hgmlClocksThrottleReasonHwPowerBrakeSlowdown           \
      | hgmlClocksEventReasonDisplayClockSetting               \
)

#define hgmlClocksThrottleReasonGpuIdle                      hgmlClocksEventReasonGpuIdle
#define hgmlClocksThrottleReasonApplicationsClocksSetting    hgmlClocksEventReasonApplicationsClocksSetting
#define hgmlClocksThrottleReasonSyncBoost                    hgmlClocksEventReasonSyncBoost
#define hgmlClocksThrottleReasonSwPowerCap                   hgmlClocksEventReasonSwPowerCap
#define hgmlClocksThrottleReasonSwThermalSlowdown            hgmlClocksEventReasonSwThermalSlowdown
#define hgmlClocksThrottleReasonDisplayClockSetting          hgmlClocksEventReasonDisplayClockSetting
#define hgmlClocksThrottleReasonNone                         hgmlClocksEventReasonNone
#define hgmlClocksThrottleReasonAll                          hgmlClocksEventReasonAll

typedef struct
{
    unsigned int gpuUtilization;
    unsigned int memoryUtilization;
    unsigned long long maxMemoryUsage;
    unsigned long long time;
    unsigned long long startTime;
    unsigned int isRunning;
    unsigned int reserved[5];
} hgmlAccountingStats_t;

typedef enum
{
    HGML_ENCODER_QUERY_H264     = 0x00,
    HGML_ENCODER_QUERY_HEVC     = 0x01,
    HGML_ENCODER_QUERY_AV1      = 0x02,
    HGML_ENCODER_QUERY_UNKNOWN  = 0xFF
}hgmlEncoderType_t;

typedef struct
{
    unsigned int       sessionId;
    unsigned int       pid;
    hgmlVgpuInstance_t vgpuInstance;
    hgmlEncoderType_t  codecType;
    unsigned int       hResolution;
    unsigned int       vResolution;
    unsigned int       averageFps;
    unsigned int       averageLatency;
}hgmlEncoderSessionInfo_t;

typedef enum
{
    HGML_FBC_SESSION_TYPE_UNKNOWN = 0,
    HGML_FBC_SESSION_TYPE_TOSYS,
    HGML_FBC_SESSION_TYPE_HGGC,
    HGML_FBC_SESSION_TYPE_VID,
    HGML_FBC_SESSION_TYPE_HWENC
} hgmlFBCSessionType_t;

typedef struct
{
    unsigned int      sessionsCount;
    unsigned int      averageFPS;
    unsigned int      averageLatency;
} hgmlFBCStats_t;

#define HGML_HGFBC_SESSION_FLAG_DIFFMAP_ENABLED                0x00000001
#define HGML_HGFBC_SESSION_FLAG_CLASSIFICATIONMAP_ENABLED      0x00000002
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_NO_WAIT      0x00000004
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_INFINITE     0x00000008
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_TIMEOUT      0x00000010

typedef struct
{
    unsigned int          sessionId;
    unsigned int          pid;
    hgmlVgpuInstance_t    vgpuInstance;
    unsigned int          displayOrdinal;
    hgmlFBCSessionType_t  sessionType;
    unsigned int          sessionFlags;
    unsigned int          hMaxResolution;
    unsigned int          vMaxResolution;
    unsigned int          hResolution;
    unsigned int          vResolution;
    unsigned int          averageFPS;
    unsigned int          averageLatency;
} hgmlFBCSessionInfo_t;

typedef enum
{
    HGML_DETACH_GPU_KEEP         = 0,
    HGML_DETACH_GPU_REMOVE
} hgmlDetachGpuState_t;

typedef enum
{
    HGML_PCIE_LINK_KEEP         = 0,
    HGML_PCIE_LINK_SHUT_DOWN
} hgmlPcieLinkState_t;

#define HGML_CC_SYSTEM_CPU_CAPS_NONE         0
#define HGML_CC_SYSTEM_CPU_CAPS_AMD_SEV      1
#define HGML_CC_SYSTEM_CPU_CAPS_INTEL_TDX    2
#define HGML_CC_SYSTEM_CPU_CAPS_AMD_SEV_SNP  3
#define HGML_CC_SYSTEM_CPU_CAPS_AMD_SNP_VTOM 4

#define HGML_CC_SYSTEM_GPUS_CC_NOT_CAPABLE 0
#define HGML_CC_SYSTEM_GPUS_CC_CAPABLE     1

typedef struct
{
    unsigned int cpuCaps;
    unsigned int gpusCaps;
} hgmlConfComputeSystemCaps_t;

#define HGML_CC_SYSTEM_DEVTOOLS_MODE_OFF 0
#define HGML_CC_SYSTEM_DEVTOOLS_MODE_ON  1

#define HGML_CC_SYSTEM_ENVIRONMENT_UNAVAILABLE 0
#define HGML_CC_SYSTEM_ENVIRONMENT_SIM         1
#define HGML_CC_SYSTEM_ENVIRONMENT_PROD        2

#define HGML_CC_SYSTEM_FEATURE_DISABLED 0
#define HGML_CC_SYSTEM_FEATURE_ENABLED  1

typedef struct
{
    unsigned int environment;
    unsigned int ccFeature;
    unsigned int devToolsMode;
} hgmlConfComputeSystemState_t;

#define HGML_CC_SYSTEM_MULTIGPU_NONE           0
#define HGML_CC_SYSTEM_MULTIGPU_PROTECTED_PCIE 1
#define HGML_CC_SYSTEM_MULTIGPU_HGLE           2

typedef struct
{
    unsigned int version;
    unsigned int environment;
    unsigned int ccFeature;
    unsigned int devToolsMode;
    unsigned int multiGpuMode;
} hgmlSystemConfComputeSettings_v1_t;

typedef hgmlSystemConfComputeSettings_v1_t hgmlSystemConfComputeSettings_t;
#define hgmlSystemConfComputeSettings_v1 HGML_STRUCT_VERSION(SystemConfComputeSettings, 1)

typedef struct
hgmlConfComputeMemSizeInfo_st
{
    unsigned long long protectedMemSizeKib;
    unsigned long long unprotectedMemSizeKib;
} hgmlConfComputeMemSizeInfo_t;

#define HGML_CC_ACCEPTING_CLIENT_REQUESTS_FALSE 0
#define HGML_CC_ACCEPTING_CLIENT_REQUESTS_TRUE  1

#define HGML_GPU_CERT_CHAIN_SIZE 0x1000
#define HGML_GPU_ATTESTATION_CERT_CHAIN_SIZE 0x1400

typedef struct
{
    unsigned int certChainSize;
    unsigned int attestationCertChainSize;
    unsigned char certChain[HGML_GPU_CERT_CHAIN_SIZE];
    unsigned char attestationCertChain[HGML_GPU_ATTESTATION_CERT_CHAIN_SIZE];
} hgmlConfComputeGpuCertificate_t;

#define HGML_CC_GPU_CEC_NONCE_SIZE 0x20
#define HGML_CC_GPU_ATTESTATION_REPORT_SIZE 0x2000
#define HGML_CC_GPU_CEC_ATTESTATION_REPORT_SIZE 0x1000
#define HGML_CC_CEC_ATTESTATION_REPORT_NOT_PRESENT 0
#define HGML_CC_CEC_ATTESTATION_REPORT_PRESENT 1
#define HGML_CC_KEY_ROTATION_THRESHOLD_ATTACKER_ADVANTAGE_MIN 50
#define HGML_CC_KEY_ROTATION_THRESHOLD_ATTACKER_ADVANTAGE_MAX 65

typedef struct
{
    unsigned int isCecAttestationReportPresent;
    unsigned int attestationReportSize;
    unsigned int cecAttestationReportSize;
    unsigned char nonce[HGML_CC_GPU_CEC_NONCE_SIZE];
    unsigned char attestationReport[HGML_CC_GPU_ATTESTATION_REPORT_SIZE];
    unsigned char cecAttestationReport[HGML_CC_GPU_CEC_ATTESTATION_REPORT_SIZE];
} hgmlConfComputeGpuAttestationReport_t;

typedef struct
{
    unsigned int version;
    unsigned long long maxAttackerAdvantage;
} hgmlConfComputeSetKeyRotationThresholdInfo_v1_t;

typedef hgmlConfComputeSetKeyRotationThresholdInfo_v1_t hgmlConfComputeSetKeyRotationThresholdInfo_t;
#define hgmlConfComputeSetKeyRotationThresholdInfo_v1 \
        HGML_STRUCT_VERSION(ConfComputeSetKeyRotationThresholdInfo, 1)

typedef struct
{
    unsigned int version;
    unsigned long long attackerAdvantage;
} hgmlConfComputeGetKeyRotationThresholdInfo_v1_t;

typedef hgmlConfComputeGetKeyRotationThresholdInfo_v1_t hgmlConfComputeGetKeyRotationThresholdInfo_t;
#define hgmlConfComputeGetKeyRotationThresholdInfo_v1 \
        HGML_STRUCT_VERSION(ConfComputeGetKeyRotationThresholdInfo, 1)

#define HGML_GPU_FABRIC_UUID_LEN 16

#define HGML_GPU_FABRIC_STATE_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_STATE_NOT_STARTED   1
#define HGML_GPU_FABRIC_STATE_IN_PROGRESS   2
#define HGML_GPU_FABRIC_STATE_COMPLETED     3

typedef unsigned char hgmlGpuFabricState_t;

typedef struct
{
    unsigned char        clusterUuid[HGML_GPU_FABRIC_UUID_LEN];
    hgmlReturn_t         status;
    unsigned int         cliqueId;
    hgmlGpuFabricState_t state;
} hgmlGpuFabricInfo_t;

#define HGML_GPU_FABRIC_HEALTH_MASK_DEGRADED_BW_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_HEALTH_MASK_DEGRADED_BW_TRUE          1
#define HGML_GPU_FABRIC_HEALTH_MASK_DEGRADED_BW_FALSE         2

#define HGML_GPU_FABRIC_HEALTH_MASK_SHIFT_DEGRADED_BW 0
#define HGML_GPU_FABRIC_HEALTH_MASK_WIDTH_DEGRADED_BW 0x3

#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_RECOVERY_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_RECOVERY_TRUE          1
#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_RECOVERY_FALSE         2

#define HGML_GPU_FABRIC_HEALTH_MASK_SHIFT_ROUTE_RECOVERY 2
#define HGML_GPU_FABRIC_HEALTH_MASK_WIDTH_ROUTE_RECOVERY 0x3

#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_UNHEALTHY_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_UNHEALTHY_TRUE          1
#define HGML_GPU_FABRIC_HEALTH_MASK_ROUTE_UNHEALTHY_FALSE         2

#define HGML_GPU_FABRIC_HEALTH_MASK_SHIFT_ROUTE_UNHEALTHY 4
#define HGML_GPU_FABRIC_HEALTH_MASK_WIDTH_ROUTE_UNHEALTHY 0x3

#define HGML_GPU_FABRIC_HEALTH_MASK_ACCESS_TIMEOUT_RECOVERY_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_HEALTH_MASK_ACCESS_TIMEOUT_RECOVERY_TRUE          1
#define HGML_GPU_FABRIC_HEALTH_MASK_ACCESS_TIMEOUT_RECOVERY_FALSE         2

#define HGML_GPU_FABRIC_HEALTH_MASK_SHIFT_ACCESS_TIMEOUT_RECOVERY 6
#define HGML_GPU_FABRIC_HEALTH_MASK_WIDTH_ACCESS_TIMEOUT_RECOVERY 0x3

#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_NOT_SUPPORTED        0
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_NONE                 1
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_INCORRECT_SYSGUID    2
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_INCORRECT_CHASSIS_SN 3
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_NO_PARTITION         4
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_INSUFFICIENT_ICNLINKS 5
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_INCOMPATIBLE_GPU_FW  6
#define HGML_GPU_FABRIC_HEALTH_MASK_INCORRECT_CONFIGURATION_INVALID_LOCATION     7

#define HGML_GPU_FABRIC_HEALTH_MASK_SHIFT_INCORRECT_CONFIGURATION 8
#define HGML_GPU_FABRIC_HEALTH_MASK_WIDTH_INCORRECT_CONFIGURATION 0xf

#define HGML_GPU_FABRIC_HEALTH_SUMMARY_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_HEALTH_SUMMARY_HEALTHY 1
#define HGML_GPU_FABRIC_HEALTH_SUMMARY_UNHEALTHY 2
#define HGML_GPU_FABRIC_HEALTH_SUMMARY_LIMITED_CAPACITY 3

#define HGML_GPU_FABRIC_HEALTH_GET(var, type)             \
    (((var) >> HGML_GPU_FABRIC_HEALTH_MASK_SHIFT##type) & \
     (HGML_GPU_FABRIC_HEALTH_MASK_WIDTH##type))

#define HGML_GPU_FABRIC_HEALTH_TEST(var, type, val) \
    (HGML_GPU_FABRIC_HEALTH_GET(var, type) ==       \
     HGML_GPU_FABRIC_HEALTH_MASK##type##val)

typedef struct
{
    unsigned int         version;
    unsigned char        clusterUuid[HGML_GPU_FABRIC_UUID_LEN];
    hgmlReturn_t         status;
    unsigned int         cliqueId;
    hgmlGpuFabricState_t state;
    unsigned int         healthMask;
} hgmlGpuFabricInfo_v2_t;

#define hgmlGpuFabricInfo_v2 HGML_STRUCT_VERSION(GpuFabricInfo, 2)

typedef struct
{
    unsigned int         version;
    unsigned char        clusterUuid[HGML_GPU_FABRIC_UUID_LEN];
    hgmlReturn_t         status;
    unsigned int         cliqueId;
    hgmlGpuFabricState_t state;
    unsigned int         healthMask;
    unsigned char        healthSummary;
} hgmlGpuFabricInfo_v3_t;

typedef hgmlGpuFabricInfo_v3_t hgmlGpuFabricInfoV_t;

#define hgmlGpuFabricInfo_v3 HGML_STRUCT_VERSION(GpuFabricInfo, 3)

#define HGML_INIT_FLAG_NO_GPUS      1
#define HGML_INIT_FLAG_NO_ATTACH    2

#define HGML_DEVICE_INFOROM_VERSION_BUFFER_SIZE       16

#define HGML_DEVICE_UUID_BUFFER_SIZE                  80

#define HGML_DEVICE_UUID_V2_BUFFER_SIZE               96

#define HGML_DEVICE_PART_NUMBER_BUFFER_SIZE           80

#define HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE        80

#define HGML_SYSTEM_HGML_VERSION_BUFFER_SIZE          80

#define HGML_DEVICE_NAME_BUFFER_SIZE                  64

#define HGML_DEVICE_NAME_V2_BUFFER_SIZE               96

#define HGML_DEVICE_SERIAL_BUFFER_SIZE                30

#define HGML_DEVICE_VBIOS_VERSION_BUFFER_SIZE         32

typedef struct
{
    unsigned int version;
    char         branch[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];
} hgmlSystemDriverBranchInfo_v1_t;
typedef hgmlSystemDriverBranchInfo_v1_t hgmlSystemDriverBranchInfo_t;
#define hgmlSystemDriverBranchInfo_v1 HGML_STRUCT_VERSION(SystemDriverBranchInfo, 1)

#define HGML_AFFINITY_SCOPE_NODE     0
#define HGML_AFFINITY_SCOPE_SOCKET   1

typedef unsigned int hgmlAffinityScope_t;

typedef struct
{
    unsigned int version;
    hgmlTemperatureSensors_t sensorType;
    int temperature;
} hgmlTemperature_v1_t;

typedef hgmlTemperature_v1_t hgmlTemperature_t;

#define hgmlTemperature_v1 HGML_STRUCT_VERSION(Temperature, 1)

typedef enum
{
    HGML_CLOCK_LIMIT_ID_RANGE_START = 0xffffff00,
    HGML_CLOCK_LIMIT_ID_TDP,
    HGML_CLOCK_LIMIT_ID_UNLIMITED
} hgmlClockLimitId_t;

#define HGML_ICNLINK_BER_MANTISSA_SHIFT 8
#define HGML_ICNLINK_BER_MANTISSA_WIDTH 0xf

#define HGML_ICNLINK_BER_EXP_SHIFT 0
#define HGML_ICNLINK_BER_EXP_WIDTH 0xff

#define HGML_ICNLINK_ERROR_COUNTER_BER_GET(var, type) \
    (((var) >> HGML_ICNLINK_##type##_SHIFT) &         \
    (HGML_ICNLINK_##type##_WIDTH))                    \

#define HGML_ICNLINK_STATE_INACTIVE 0x0
#define HGML_ICNLINK_STATE_ACTIVE   0x1
#define HGML_ICNLINK_STATE_SLEEP    0x2

#define HGML_ICNLINK_TOTAL_SUPPORTED_BW_MODES 23

typedef struct
{
    unsigned int version;
    unsigned char bwModes[HGML_ICNLINK_TOTAL_SUPPORTED_BW_MODES];
    unsigned char totalBwModes;
} hgmlIcnLinkSupportedBwModes_v1_t;
typedef hgmlIcnLinkSupportedBwModes_v1_t hgmlIcnLinkSupportedBwModes_t;
#define hgmlIcnLinkSupportedBwModes_v1 HGML_STRUCT_VERSION(IcnLinkSupportedBwModes, 1)

typedef struct
{
    unsigned int version;
    unsigned int bIsBest;
    unsigned char bwMode;
} hgmlIcnLinkGetBwMode_v1_t;
typedef hgmlIcnLinkGetBwMode_v1_t hgmlIcnLinkGetBwMode_t;
#define hgmlIcnLinkGetBwMode_v1 HGML_STRUCT_VERSION(IcnLinkGetBwMode, 1)

typedef struct
{
    unsigned int version;
    unsigned int bSetBest;
    unsigned char bwMode;
} hgmlIcnLinkSetBwMode_v1_t;
typedef hgmlIcnLinkSetBwMode_v1_t hgmlIcnLinkSetBwMode_t;
#define hgmlIcnLinkSetBwMode_v1 HGML_STRUCT_VERSION(IcnLinkSetBwMode, 1)

typedef struct
{
    unsigned int version;
    unsigned int isIcnleEnabled;
} hgmlIcnLinkInfo_v1_t;
#define hgmlIcnLinkInfo_v1 HGML_STRUCT_VERSION(IcnLinkInfo, 1)

#define HGML_ICNLINK_FIRMWARE_UCODE_TYPE_MSE        0x1
#define HGML_ICNLINK_FIRMWARE_UCODE_TYPE_NETIR      0x2
#define HGML_ICNLINK_FIRMWARE_UCODE_TYPE_NETIR_UPHY 0x3
#define HGML_ICNLINK_FIRMWARE_UCODE_TYPE_NETIR_CLN  0x4
#define HGML_ICNLINK_FIRMWARE_UCODE_TYPE_NETIR_DLN  0x5
#define HGML_ICNLINK_FIRMWARE_VERSION_LENGTH        100

typedef struct
{
    unsigned char ucodeType;
    unsigned int major;
    unsigned int minor;
    unsigned int subMinor;
} hgmlIcnLinkFirmwareVersion_t;

typedef struct
{
    hgmlIcnLinkFirmwareVersion_t firmwareVersion[HGML_ICNLINK_FIRMWARE_VERSION_LENGTH];
    unsigned int numValidEntries;
} hgmlIcnLinkFirmwareInfo_t;

typedef struct
{
    unsigned int version;
    unsigned int isIcnleEnabled;
    hgmlIcnLinkFirmwareInfo_t firmwareInfo;
} hgmlIcnLinkInfo_v2_t;
typedef hgmlIcnLinkInfo_v2_t hgmlIcnLinkInfo_t;
#define hgmlIcnLinkInfo_v2 HGML_STRUCT_VERSION(IcnLinkInfo, 2)

typedef unsigned int hgmlVgpuProfileId_t;

typedef struct
{
    unsigned int minVersion;
    unsigned int maxVersion;
} hgmlVgpuVersion_t;

typedef struct
{
    unsigned int             version;
    unsigned int             revision;
    hgmlVgpuGuestInfoState_t guestInfoState;
    char                     guestDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];
    char                     hostDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];
    unsigned int             reserved[6];
    unsigned int             vgpuVirtualizationCaps;
    unsigned int             guestVgpuVersion;
    unsigned int             opaqueDataSize;
    char                     opaqueData[4];
} hgmlVgpuMetadata_t;

typedef struct
{
    unsigned int            version;
    unsigned int            revision;
    char                    hostDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];
    unsigned int            pgpuVirtualizationCaps;
    unsigned int            reserved[5];
    hgmlVgpuVersion_t       hostSupportedVgpuRange;
    unsigned int            opaqueDataSize;
    char                    opaqueData[4];
} hgmlVgpuPgpuMetadata_t;

typedef enum
{
    HGML_VGPU_VM_COMPATIBILITY_NONE         = 0x0,
    HGML_VGPU_VM_COMPATIBILITY_COLD         = 0x1,
    HGML_VGPU_VM_COMPATIBILITY_HIBERNATE    = 0x2,
    HGML_VGPU_VM_COMPATIBILITY_SLEEP        = 0x4,
    HGML_VGPU_VM_COMPATIBILITY_LIVE         = 0x8
} hgmlVgpuVmCompatibility_t;

typedef enum
{
    HGML_VGPU_COMPATIBILITY_LIMIT_NONE          = 0x0,
    HGML_VGPU_COMPATIBILITY_LIMIT_HOST_DRIVER   = 0x1,
    HGML_VGPU_COMPATIBILITY_LIMIT_GUEST_DRIVER  = 0x2,
    HGML_VGPU_COMPATIBILITY_LIMIT_GPU           = 0x4,
    HGML_VGPU_COMPATIBILITY_LIMIT_OTHER         = 0x80000000
} hgmlVgpuPgpuCompatibilityLimitCode_t;

typedef struct
{
    hgmlVgpuVmCompatibility_t               vgpuVmCompatibility;
    hgmlVgpuPgpuCompatibilityLimitCode_t    compatibilityLimitCode;
} hgmlVgpuPgpuCompatibility_t;

typedef struct
{
    hgmlPciInfo_t pciInfo;
    char uuid[HGML_DEVICE_UUID_BUFFER_SIZE];
} hgmlExcludedDeviceInfo_t;

#define HGML_PRM_DATA_MAX_SIZE 496

typedef struct
{
    unsigned dataSize;
    unsigned status;
    union {
        unsigned char inData[HGML_PRM_DATA_MAX_SIZE];
        unsigned char outData[HGML_PRM_DATA_MAX_SIZE];
    };
} hgmlPRMTLV_v1_t;

#define HGML_DEVICE_MIG_DISABLE 0x0

#define HGML_DEVICE_MIG_ENABLE 0x1

#define HGML_GPU_INSTANCE_PROFILE_1_SLICE      0x0
#define HGML_GPU_INSTANCE_PROFILE_2_SLICE      0x1
#define HGML_GPU_INSTANCE_PROFILE_3_SLICE      0x2
#define HGML_GPU_INSTANCE_PROFILE_4_SLICE      0x3
#define HGML_GPU_INSTANCE_PROFILE_7_SLICE      0x4
#define HGML_GPU_INSTANCE_PROFILE_8_SLICE      0x5
#define HGML_GPU_INSTANCE_PROFILE_6_SLICE      0x6

#define HGML_GPU_INSTANCE_PROFILE_1_SLICE_REV1 0x7
#define HGML_GPU_INSTANCE_PROFILE_2_SLICE_REV1 0x8
#define HGML_GPU_INSTANCE_PROFILE_1_SLICE_REV2 0x9
#define HGML_GPU_INSTANCE_PROFILE_1_SLICE_GFX      0x0A
#define HGML_GPU_INSTANCE_PROFILE_2_SLICE_GFX      0x0B
#define HGML_GPU_INSTANCE_PROFILE_4_SLICE_GFX      0x0C
#define HGML_GPU_INSTANCE_PROFILE_1_SLICE_NO_ME    0x0D
#define HGML_GPU_INSTANCE_PROFILE_2_SLICE_NO_ME    0x0E
#define HGML_GPU_INSTANCE_PROFILE_1_SLICE_ALL_ME   0x0F
#define HGML_GPU_INSTANCE_PROFILE_2_SLICE_ALL_ME   0x10

#define HGML_GPU_INSTANCE_PROFILE_2_CE         0x50
#define HGML_GPU_INSTANCE_PROFILE_4_CE         0x51
#define HGML_GPU_INSTANCE_PROFILE_8_CE         0x52
#define HGML_GPU_INSTANCE_PROFILE_16_CE        0x53
#define HGML_GPU_INSTANCE_PROFILE_14_CE        0x54

#define HGML_GPU_INSTANCE_PROFILE_COUNT        0x55

#define HGML_GPU_INSTANCE_PROFILE_CAPS_P2P     0x1
#define HGML_GPU_INTSTANCE_PROFILE_CAPS_P2P    0x1
#define HGML_GPU_INSTANCE_PROFILE_CAPS_GFX     0x2

#define HGML_COMPUTE_INSTANCE_PROFILE_CAPS_GFX 0x1

typedef struct
{
    unsigned int start;
    unsigned int size;
} hgmlGpuInstancePlacement_t;

typedef struct
{
    unsigned int id;
    unsigned int isP2pSupported;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int copyEngineCount;
    unsigned int decoderCount;
    unsigned int encoderCount;
    unsigned int jpegCount;
    unsigned int ofaCount;
    unsigned long long memorySizeMB;
} hgmlGpuInstanceProfileInfo_t;

typedef struct
{
    unsigned int version;
    unsigned int id;
    unsigned int isP2pSupported;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int copyEngineCount;
    unsigned int decoderCount;
    unsigned int encoderCount;
    unsigned int jpegCount;
    unsigned int ofaCount;
    unsigned long long memorySizeMB;
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE];
} hgmlGpuInstanceProfileInfo_v2_t;

#define hgmlGpuInstanceProfileInfo_v2 HGML_STRUCT_VERSION(GpuInstanceProfileInfo, 2)

typedef struct
{
    unsigned int version;
    unsigned int id;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int copyEngineCount;
    unsigned int decoderCount;
    unsigned int encoderCount;
    unsigned int jpegCount;
    unsigned int ofaCount;
    unsigned long long memorySizeMB;
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE];
    unsigned int capabilities;
} hgmlGpuInstanceProfileInfo_v3_t;

#define hgmlGpuInstanceProfileInfo_v3 HGML_STRUCT_VERSION(GpuInstanceProfileInfo, 3)

typedef struct
{
    hgmlDevice_t device;
    unsigned int id;
    unsigned int profileId;
    hgmlGpuInstancePlacement_t placement;
} hgmlGpuInstanceInfo_t;

#define HGML_COMPUTE_INSTANCE_PROFILE_1_SLICE       0x0
#define HGML_COMPUTE_INSTANCE_PROFILE_2_SLICE       0x1
#define HGML_COMPUTE_INSTANCE_PROFILE_3_SLICE       0x2
#define HGML_COMPUTE_INSTANCE_PROFILE_4_SLICE       0x3
#define HGML_COMPUTE_INSTANCE_PROFILE_7_SLICE       0x4
#define HGML_COMPUTE_INSTANCE_PROFILE_8_SLICE       0x5
#define HGML_COMPUTE_INSTANCE_PROFILE_6_SLICE       0x6
#define HGML_COMPUTE_INSTANCE_PROFILE_1_SLICE_REV1  0x7

#define HGML_COMPUTE_INSTANCE_PROFILE_1_CU          0x8
#define HGML_COMPUTE_INSTANCE_PROFILE_2_CU          0x9
#define HGML_COMPUTE_INSTANCE_PROFILE_3_CU          0xa

#define HGML_COMPUTE_INSTANCE_PROFILE_1_CE          0xb
#define HGML_COMPUTE_INSTANCE_PROFILE_2_CE          0xc
#define HGML_COMPUTE_INSTANCE_PROFILE_3_CE          0xd
#define HGML_COMPUTE_INSTANCE_PROFILE_4_CE          0xe
#define HGML_COMPUTE_INSTANCE_PROFILE_5_CE          0xf
#define HGML_COMPUTE_INSTANCE_PROFILE_6_CE          0x10
#define HGML_COMPUTE_INSTANCE_PROFILE_7_CE          0x11
#define HGML_COMPUTE_INSTANCE_PROFILE_8_CE          0x12
#define HGML_COMPUTE_INSTANCE_PROFILE_9_CE          0x13
#define HGML_COMPUTE_INSTANCE_PROFILE_10_CE         0x14
#define HGML_COMPUTE_INSTANCE_PROFILE_11_CE         0x15
#define HGML_COMPUTE_INSTANCE_PROFILE_12_CE         0x16
#define HGML_COMPUTE_INSTANCE_PROFILE_13_CE         0x17
#define HGML_COMPUTE_INSTANCE_PROFILE_14_CE         0x18
#define HGML_COMPUTE_INSTANCE_PROFILE_15_CE         0x19
#define HGML_COMPUTE_INSTANCE_PROFILE_16_CE         0x1a

#define HGML_COMPUTE_INSTANCE_PROFILE_COUNT         0x1b

#define HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED 0x0
#define HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT  0x1

typedef struct
{
    unsigned int start;
    unsigned int size;
} hgmlComputeInstancePlacement_t;

typedef struct
{
    unsigned int id;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int sharedCopyEngineCount;
    unsigned int sharedDecoderCount;
    unsigned int sharedEncoderCount;
    unsigned int sharedJpegCount;
    unsigned int sharedOfaCount;
} hgmlComputeInstanceProfileInfo_t;

typedef struct
{
    unsigned int version;
    unsigned int id;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int sharedCopyEngineCount;
    unsigned int sharedDecoderCount;
    unsigned int sharedEncoderCount;
    unsigned int sharedJpegCount;
    unsigned int sharedOfaCount;
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE];
} hgmlComputeInstanceProfileInfo_v2_t;

#define hgmlComputeInstanceProfileInfo_v2 HGML_STRUCT_VERSION(ComputeInstanceProfileInfo, 2)

typedef struct
{
    unsigned int version;
    unsigned int id;
    unsigned int sliceCount;
    unsigned int instanceCount;
    unsigned int multiprocessorCount;
    unsigned int sharedCopyEngineCount;
    unsigned int sharedDecoderCount;
    unsigned int sharedEncoderCount;
    unsigned int sharedJpegCount;
    unsigned int sharedOfaCount;
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE];
    unsigned int capabilities;
} hgmlComputeInstanceProfileInfo_v3_t;

#define hgmlComputeInstanceProfileInfo_v3 HGML_STRUCT_VERSION(ComputeInstanceProfileInfo, 3)

typedef struct
{
    hgmlDevice_t device;
    hgmlGpuInstance_t gpuInstance;
    unsigned int id;
    unsigned int profileId;
    hgmlComputeInstancePlacement_t placement;
} hgmlComputeInstanceInfo_t;

typedef struct
{
    struct hgmlComputeInstance_st* handle;
} hgmlComputeInstance_t;

typedef enum
{
    HGML_GPM_METRIC_GRAPHICS_UTIL               = 1,
    HGML_GPM_METRIC_SM_UTIL                     = 2,
    HGML_GPM_METRIC_SM_OCCUPANCY                = 3,
    HGML_GPM_METRIC_INTEGER_UTIL                = 4,
    HGML_GPM_METRIC_ANY_TENSOR_UTIL             = 5,
    HGML_GPM_METRIC_DFMA_TENSOR_UTIL            = 6,
    HGML_GPM_METRIC_HMMA_TENSOR_UTIL            = 7,
    HGML_GPM_METRIC_IMMA_TENSOR_UTIL            = 9,
    HGML_GPM_METRIC_DRAM_BW_UTIL                = 10,
    HGML_GPM_METRIC_FP64_UTIL                   = 11,
    HGML_GPM_METRIC_FP32_UTIL                   = 12,
    HGML_GPM_METRIC_FP16_UTIL                   = 13,
    HGML_GPM_METRIC_PCIE_TX_PER_SEC             = 20,
    HGML_GPM_METRIC_PCIE_RX_PER_SEC             = 21,
    HGML_GPM_METRIC_HGDEC_0_UTIL                = 30,
    HGML_GPM_METRIC_HGDEC_1_UTIL                = 31,
    HGML_GPM_METRIC_HGDEC_2_UTIL                = 32,
    HGML_GPM_METRIC_HGDEC_3_UTIL                = 33,
    HGML_GPM_METRIC_HGDEC_4_UTIL                = 34,
    HGML_GPM_METRIC_HGDEC_5_UTIL                = 35,
    HGML_GPM_METRIC_HGDEC_6_UTIL                = 36,
    HGML_GPM_METRIC_HGDEC_7_UTIL                = 37,
    HGML_GPM_METRIC_HGJPG_0_UTIL                = 40,
    HGML_GPM_METRIC_HGJPG_1_UTIL                = 41,
    HGML_GPM_METRIC_HGJPG_2_UTIL                = 42,
    HGML_GPM_METRIC_HGJPG_3_UTIL                = 43,
    HGML_GPM_METRIC_HGJPG_4_UTIL                = 44,
    HGML_GPM_METRIC_HGJPG_5_UTIL                = 45,
    HGML_GPM_METRIC_HGJPG_6_UTIL                = 46,
    HGML_GPM_METRIC_HGJPG_7_UTIL                = 47,
    HGML_GPM_METRIC_HGOFA_0_UTIL                = 50,
    HGML_GPM_METRIC_ICNLINK_TOTAL_RX_PER_SEC    = 60,
    HGML_GPM_METRIC_ICNLINK_TOTAL_TX_PER_SEC    = 61,
    HGML_GPM_METRIC_ICNLINK_L0_RX_PER_SEC       = 62,
    HGML_GPM_METRIC_ICNLINK_L0_TX_PER_SEC       = 63,
    HGML_GPM_METRIC_ICNLINK_L1_RX_PER_SEC       = 64,
    HGML_GPM_METRIC_ICNLINK_L1_TX_PER_SEC       = 65,
    HGML_GPM_METRIC_ICNLINK_L2_RX_PER_SEC       = 66,
    HGML_GPM_METRIC_ICNLINK_L2_TX_PER_SEC       = 67,
    HGML_GPM_METRIC_ICNLINK_L3_RX_PER_SEC       = 68,
    HGML_GPM_METRIC_ICNLINK_L3_TX_PER_SEC       = 69,
    HGML_GPM_METRIC_ICNLINK_L4_RX_PER_SEC       = 70,
    HGML_GPM_METRIC_ICNLINK_L4_TX_PER_SEC       = 71,
    HGML_GPM_METRIC_ICNLINK_L5_RX_PER_SEC       = 72,
    HGML_GPM_METRIC_ICNLINK_L5_TX_PER_SEC       = 73,
    HGML_GPM_METRIC_ICNLINK_L6_RX_PER_SEC       = 74,
    HGML_GPM_METRIC_ICNLINK_L6_TX_PER_SEC       = 75,
    HGML_GPM_METRIC_ICNLINK_L7_RX_PER_SEC       = 76,
    HGML_GPM_METRIC_ICNLINK_L7_TX_PER_SEC       = 77,
    HGML_GPM_METRIC_ICNLINK_L8_RX_PER_SEC       = 78,
    HGML_GPM_METRIC_ICNLINK_L8_TX_PER_SEC       = 79,
    HGML_GPM_METRIC_ICNLINK_L9_RX_PER_SEC       = 80,
    HGML_GPM_METRIC_ICNLINK_L9_TX_PER_SEC       = 81,
    HGML_GPM_METRIC_ICNLINK_L10_RX_PER_SEC      = 82,
    HGML_GPM_METRIC_ICNLINK_L10_TX_PER_SEC      = 83,
    HGML_GPM_METRIC_ICNLINK_L11_RX_PER_SEC      = 84,
    HGML_GPM_METRIC_ICNLINK_L11_TX_PER_SEC      = 85,
    HGML_GPM_METRIC_ICNLINK_L12_RX_PER_SEC      = 86,
    HGML_GPM_METRIC_ICNLINK_L12_TX_PER_SEC      = 87,
    HGML_GPM_METRIC_ICNLINK_L13_RX_PER_SEC      = 88,
    HGML_GPM_METRIC_ICNLINK_L13_TX_PER_SEC      = 89,
    HGML_GPM_METRIC_ICNLINK_L14_RX_PER_SEC      = 90,
    HGML_GPM_METRIC_ICNLINK_L14_TX_PER_SEC      = 91,
    HGML_GPM_METRIC_ICNLINK_L15_RX_PER_SEC      = 92,
    HGML_GPM_METRIC_ICNLINK_L15_TX_PER_SEC      = 93,
    HGML_GPM_METRIC_ICNLINK_L16_RX_PER_SEC      = 94,
    HGML_GPM_METRIC_ICNLINK_L16_TX_PER_SEC      = 95,
    HGML_GPM_METRIC_ICNLINK_L17_RX_PER_SEC      = 96,
    HGML_GPM_METRIC_ICNLINK_L17_TX_PER_SEC      = 97,
    HGML_GPM_METRIC_C2C_TOTAL_TX_PER_SEC        = 100,
    HGML_GPM_METRIC_C2C_TOTAL_RX_PER_SEC        = 101,
    HGML_GPM_METRIC_C2C_DATA_TX_PER_SEC         = 102,
    HGML_GPM_METRIC_C2C_DATA_RX_PER_SEC         = 103,
    HGML_GPM_METRIC_C2C_LINK0_TOTAL_TX_PER_SEC  = 104,
    HGML_GPM_METRIC_C2C_LINK0_TOTAL_RX_PER_SEC  = 105,
    HGML_GPM_METRIC_C2C_LINK0_DATA_TX_PER_SEC   = 106,
    HGML_GPM_METRIC_C2C_LINK0_DATA_RX_PER_SEC   = 107,
    HGML_GPM_METRIC_C2C_LINK1_TOTAL_TX_PER_SEC  = 108,
    HGML_GPM_METRIC_C2C_LINK1_TOTAL_RX_PER_SEC  = 109,
    HGML_GPM_METRIC_C2C_LINK1_DATA_TX_PER_SEC   = 110,
    HGML_GPM_METRIC_C2C_LINK1_DATA_RX_PER_SEC   = 111,
    HGML_GPM_METRIC_C2C_LINK2_TOTAL_TX_PER_SEC  = 112,
    HGML_GPM_METRIC_C2C_LINK2_TOTAL_RX_PER_SEC  = 113,
    HGML_GPM_METRIC_C2C_LINK2_DATA_TX_PER_SEC   = 114,
    HGML_GPM_METRIC_C2C_LINK2_DATA_RX_PER_SEC   = 115,
    HGML_GPM_METRIC_C2C_LINK3_TOTAL_TX_PER_SEC  = 116,
    HGML_GPM_METRIC_C2C_LINK3_TOTAL_RX_PER_SEC  = 117,
    HGML_GPM_METRIC_C2C_LINK3_DATA_TX_PER_SEC   = 118,
    HGML_GPM_METRIC_C2C_LINK3_DATA_RX_PER_SEC   = 119,
    HGML_GPM_METRIC_C2C_LINK4_TOTAL_TX_PER_SEC  = 120,
    HGML_GPM_METRIC_C2C_LINK4_TOTAL_RX_PER_SEC  = 121,
    HGML_GPM_METRIC_C2C_LINK4_DATA_TX_PER_SEC   = 122,
    HGML_GPM_METRIC_C2C_LINK4_DATA_RX_PER_SEC   = 123,
    HGML_GPM_METRIC_C2C_LINK5_TOTAL_TX_PER_SEC  = 124,
    HGML_GPM_METRIC_C2C_LINK5_TOTAL_RX_PER_SEC  = 125,
    HGML_GPM_METRIC_C2C_LINK5_DATA_TX_PER_SEC   = 126,
    HGML_GPM_METRIC_C2C_LINK5_DATA_RX_PER_SEC   = 127,
    HGML_GPM_METRIC_C2C_LINK6_TOTAL_TX_PER_SEC  = 128,
    HGML_GPM_METRIC_C2C_LINK6_TOTAL_RX_PER_SEC  = 129,
    HGML_GPM_METRIC_C2C_LINK6_DATA_TX_PER_SEC   = 130,
    HGML_GPM_METRIC_C2C_LINK6_DATA_RX_PER_SEC   = 131,
    HGML_GPM_METRIC_C2C_LINK7_TOTAL_TX_PER_SEC  = 132,
    HGML_GPM_METRIC_C2C_LINK7_TOTAL_RX_PER_SEC  = 133,
    HGML_GPM_METRIC_C2C_LINK7_DATA_TX_PER_SEC   = 134,
    HGML_GPM_METRIC_C2C_LINK7_DATA_RX_PER_SEC   = 135,
    HGML_GPM_METRIC_C2C_LINK8_TOTAL_TX_PER_SEC  = 136,
    HGML_GPM_METRIC_C2C_LINK8_TOTAL_RX_PER_SEC  = 137,
    HGML_GPM_METRIC_C2C_LINK8_DATA_TX_PER_SEC   = 138,
    HGML_GPM_METRIC_C2C_LINK8_DATA_RX_PER_SEC   = 139,
    HGML_GPM_METRIC_C2C_LINK9_TOTAL_TX_PER_SEC  = 140,
    HGML_GPM_METRIC_C2C_LINK9_TOTAL_RX_PER_SEC  = 141,
    HGML_GPM_METRIC_C2C_LINK9_DATA_TX_PER_SEC   = 142,
    HGML_GPM_METRIC_C2C_LINK9_DATA_RX_PER_SEC   = 143,
    HGML_GPM_METRIC_C2C_LINK10_TOTAL_TX_PER_SEC = 144,
    HGML_GPM_METRIC_C2C_LINK10_TOTAL_RX_PER_SEC = 145,
    HGML_GPM_METRIC_C2C_LINK10_DATA_TX_PER_SEC  = 146,
    HGML_GPM_METRIC_C2C_LINK10_DATA_RX_PER_SEC  = 147,
    HGML_GPM_METRIC_C2C_LINK11_TOTAL_TX_PER_SEC = 148,
    HGML_GPM_METRIC_C2C_LINK11_TOTAL_RX_PER_SEC = 149,
    HGML_GPM_METRIC_C2C_LINK11_DATA_TX_PER_SEC  = 150,
    HGML_GPM_METRIC_C2C_LINK11_DATA_RX_PER_SEC  = 151,
    HGML_GPM_METRIC_C2C_LINK12_TOTAL_TX_PER_SEC = 152,
    HGML_GPM_METRIC_C2C_LINK12_TOTAL_RX_PER_SEC = 153,
    HGML_GPM_METRIC_C2C_LINK12_DATA_TX_PER_SEC  = 154,
    HGML_GPM_METRIC_C2C_LINK12_DATA_RX_PER_SEC  = 155,
    HGML_GPM_METRIC_C2C_LINK13_TOTAL_TX_PER_SEC = 156,
    HGML_GPM_METRIC_C2C_LINK13_TOTAL_RX_PER_SEC = 157,
    HGML_GPM_METRIC_C2C_LINK13_DATA_TX_PER_SEC  = 158,
    HGML_GPM_METRIC_C2C_LINK13_DATA_RX_PER_SEC  = 159,
    HGML_GPM_METRIC_HOSTMEM_CACHE_HIT           = 160,
    HGML_GPM_METRIC_HOSTMEM_CACHE_MISS          = 161,
    HGML_GPM_METRIC_PEERMEM_CACHE_HIT           = 162,
    HGML_GPM_METRIC_PEERMEM_CACHE_MISS          = 163,
    HGML_GPM_METRIC_DRAM_CACHE_HIT              = 164,
    HGML_GPM_METRIC_DRAM_CACHE_MISS             = 165,
    HGML_GPM_METRIC_HGENC_0_UTIL                = 166,
    HGML_GPM_METRIC_HGENC_1_UTIL                = 167,
    HGML_GPM_METRIC_HGENC_2_UTIL                = 168,
    HGML_GPM_METRIC_HGENC_3_UTIL                = 169,
    HGML_GPM_METRIC_GR0_CTXSW_CYCLES_ELAPSED    = 170,
    HGML_GPM_METRIC_GR0_CTXSW_CYCLES_ACTIVE     = 171,
    HGML_GPM_METRIC_GR0_CTXSW_REQUESTS          = 172,
    HGML_GPM_METRIC_GR0_CTXSW_CYCLES_PER_REQ    = 173,
    HGML_GPM_METRIC_GR0_CTXSW_ACTIVE_PCT        = 174,
    HGML_GPM_METRIC_GR1_CTXSW_CYCLES_ELAPSED    = 175,
    HGML_GPM_METRIC_GR1_CTXSW_CYCLES_ACTIVE     = 176,
    HGML_GPM_METRIC_GR1_CTXSW_REQUESTS          = 177,
    HGML_GPM_METRIC_GR1_CTXSW_CYCLES_PER_REQ    = 178,
    HGML_GPM_METRIC_GR1_CTXSW_ACTIVE_PCT        = 179,
    HGML_GPM_METRIC_GR2_CTXSW_CYCLES_ELAPSED    = 180,
    HGML_GPM_METRIC_GR2_CTXSW_CYCLES_ACTIVE     = 181,
    HGML_GPM_METRIC_GR2_CTXSW_REQUESTS          = 182,
    HGML_GPM_METRIC_GR2_CTXSW_CYCLES_PER_REQ    = 183,
    HGML_GPM_METRIC_GR2_CTXSW_ACTIVE_PCT        = 184,
    HGML_GPM_METRIC_GR3_CTXSW_CYCLES_ELAPSED    = 185,
    HGML_GPM_METRIC_GR3_CTXSW_CYCLES_ACTIVE     = 186,
    HGML_GPM_METRIC_GR3_CTXSW_REQUESTS          = 187,
    HGML_GPM_METRIC_GR3_CTXSW_CYCLES_PER_REQ    = 188,
    HGML_GPM_METRIC_GR3_CTXSW_ACTIVE_PCT        = 189,
    HGML_GPM_METRIC_GR4_CTXSW_CYCLES_ELAPSED    = 190,
    HGML_GPM_METRIC_GR4_CTXSW_CYCLES_ACTIVE     = 191,
    HGML_GPM_METRIC_GR4_CTXSW_REQUESTS          = 192,
    HGML_GPM_METRIC_GR4_CTXSW_CYCLES_PER_REQ    = 193,
    HGML_GPM_METRIC_GR4_CTXSW_ACTIVE_PCT        = 194,
    HGML_GPM_METRIC_GR5_CTXSW_CYCLES_ELAPSED    = 195,
    HGML_GPM_METRIC_GR5_CTXSW_CYCLES_ACTIVE     = 196,
    HGML_GPM_METRIC_GR5_CTXSW_REQUESTS          = 197,
    HGML_GPM_METRIC_GR5_CTXSW_CYCLES_PER_REQ    = 198,
    HGML_GPM_METRIC_GR5_CTXSW_ACTIVE_PCT        = 199,
    HGML_GPM_METRIC_GR6_CTXSW_CYCLES_ELAPSED    = 200,
    HGML_GPM_METRIC_GR6_CTXSW_CYCLES_ACTIVE     = 201,
    HGML_GPM_METRIC_GR6_CTXSW_REQUESTS          = 202,
    HGML_GPM_METRIC_GR6_CTXSW_CYCLES_PER_REQ    = 203,
    HGML_GPM_METRIC_GR6_CTXSW_ACTIVE_PCT        = 204,
    HGML_GPM_METRIC_GR7_CTXSW_CYCLES_ELAPSED    = 205,
    HGML_GPM_METRIC_GR7_CTXSW_CYCLES_ACTIVE     = 206,
    HGML_GPM_METRIC_GR7_CTXSW_REQUESTS          = 207,
    HGML_GPM_METRIC_GR7_CTXSW_CYCLES_PER_REQ    = 208,
    HGML_GPM_METRIC_GR7_CTXSW_ACTIVE_PCT        = 209,

    HGML_GPM_METRIC_KSD_HIT_RATE                = 2048,
    HGML_GPM_METRIC_KVD_HIT_RATE                = 2049,
    HGML_GPM_METRIC_L2_HIT_RATE                 = 2050,
    HGML_GPM_METRIC_LLC_HIT_RATE                = 2051,
    HGML_GPM_METRIC_MAX                         = 2052,
} hgmlGpmMetricId_t;

typedef struct
{
    struct hgmlGpmSample_st* handle;
} hgmlGpmSample_t;

typedef struct
{
    unsigned int metricId;
    hgmlReturn_t hgmlReturn;
    double value;
    struct
    {
        char *shortName;
        char *longName;
        char *unit;
    } metricInfo;
} hgmlGpmMetric_t;

typedef struct
{
    unsigned int version;
    unsigned int numMetrics;
    hgmlGpmSample_t sample1;
    hgmlGpmSample_t sample2;
    hgmlGpmMetric_t metrics[HGML_GPM_METRIC_MAX];
} hgmlGpmMetricsGet_t;

#define HGML_GPM_METRICS_GET_VERSION 1

typedef struct
{
    unsigned int version;
    unsigned int isSupportedDevice;
} hgmlGpmSupport_t;

#define HGML_GPM_SUPPORT_VERSION 2

#define HGML_DEV_CAP_EGM (1 << 0)

typedef struct
{
    unsigned int version;
    unsigned int capMask;
} hgmlDeviceCapabilities_v1_t;
typedef hgmlDeviceCapabilities_v1_t hgmlDeviceCapabilities_t;
#define hgmlDeviceCapabilities_v1 HGML_STRUCT_VERSION(DeviceCapabilities, 1)

#define HGML_255_MASK_BITS_PER_ELEM     32
#define HGML_255_MASK_NUM_ELEMS         8
#define HGML_255_MASK_BIT_SET(index, hgmlMask)                          \
    hgmlMask.mask[index / HGML_255_MASK_BITS_PER_ELEM] |= (1 << (index % HGML_255_MASK_BITS_PER_ELEM))

#define HGML_255_MASK_BIT_GET(index, hgmlMask)                          \
    hgmlMask.mask[index / HGML_255_MASK_BITS_PER_ELEM] & (1 << (index % HGML_255_MASK_BITS_PER_ELEM))

#define HGML_255_MASK_BIT_SET_PTR(index, hgmlMask)                          \
    hgmlMask->mask[index / HGML_255_MASK_BITS_PER_ELEM] |= (1 << (index % HGML_255_MASK_BITS_PER_ELEM))

#define HGML_255_MASK_BIT_GET_PTR(index, hgmlMask)                          \
    hgmlMask->mask[index / HGML_255_MASK_BITS_PER_ELEM] & (1 << (index % HGML_255_MASK_BITS_PER_ELEM))

typedef struct
{
     unsigned int mask[HGML_255_MASK_NUM_ELEMS];
} hgmlMask255_t;

#define HGML_WORKLOAD_POWER_MAX_PROFILES        (255)
typedef enum
{
    HGML_POWER_PROFILE_MAX_P            = 0,
    HGML_POWER_PROFILE_MAX_Q            = 1,
    HGML_POWER_PROFILE_COMPUTE          = 2,
    HGML_POWER_PROFILE_MEMORY_BOUND     = 3,
    HGML_POWER_PROFILE_NETWORK          = 4,
    HGML_POWER_PROFILE_BALANCED         = 5,
    HGML_POWER_PROFILE_LLM_INFERENCE    = 6,
    HGML_POWER_PROFILE_LLM_TRAINING     = 7,
    HGML_POWER_PROFILE_RBM              = 8,
    HGML_POWER_PROFILE_DCPCIE           = 9,
    HGML_POWER_PROFILE_HMMA_SPARSE      = 10,
    HGML_POWER_PROFILE_HMMA_DENSE       = 11,
    HGML_POWER_PROFILE_SYNC_BALANCED    = 12,
    HGML_POWER_PROFILE_HPC              = 13,
    HGML_POWER_PROFILE_MIG              = 14,

    HGML_POWER_PROFILE_MAX              = 15,
} hgmlPowerProfileType_t;

typedef struct
{
    unsigned int    version;
    unsigned int    profileId;
    unsigned int    priority;
    hgmlMask255_t   conflictingMask;
} hgmlWorkloadPowerProfileInfo_v1_t;
typedef hgmlWorkloadPowerProfileInfo_v1_t hgmlWorkloadPowerProfileInfo_t;
#define hgmlWorkloadPowerProfileInfo_v1 HGML_STRUCT_VERSION(WorkloadPowerProfileInfo, 1)

typedef struct
{
    unsigned int              version;
    hgmlMask255_t             perfProfilesMask;
    hgmlWorkloadPowerProfileInfo_t perfProfile[HGML_WORKLOAD_POWER_MAX_PROFILES];
} hgmlWorkloadPowerProfileProfilesInfo_v1_t;
typedef hgmlWorkloadPowerProfileProfilesInfo_v1_t hgmlWorkloadPowerProfileProfilesInfo_t;
#define hgmlWorkloadPowerProfileProfilesInfo_v1 HGML_STRUCT_VERSION(WorkloadPowerProfileProfilesInfo, 1)

typedef struct
{
    unsigned int            version;
    hgmlMask255_t           perfProfilesMask;
    hgmlMask255_t           requestedProfilesMask;
    hgmlMask255_t           enforcedProfilesMask;
} hgmlWorkloadPowerProfileCurrentProfiles_v1_t;
typedef hgmlWorkloadPowerProfileCurrentProfiles_v1_t hgmlWorkloadPowerProfileCurrentProfiles_t;
#define hgmlWorkloadPowerProfileCurrentProfiles_v1 HGML_STRUCT_VERSION(WorkloadPowerProfileCurrentProfiles, 1)

typedef struct
{
    unsigned int version;
    hgmlMask255_t requestedProfilesMask;
} hgmlWorkloadPowerProfileRequestedProfiles_v1_t;
typedef hgmlWorkloadPowerProfileRequestedProfiles_v1_t hgmlWorkloadPowerProfileRequestedProfiles_t;
#define hgmlWorkloadPowerProfileRequestedProfiles_v1 HGML_STRUCT_VERSION(WorkloadPowerProfileRequestedProfiles, 1)

#define HGML_POWER_SMOOTHING_IDX_FROM_FIELD_VAL(field_val) (field_val - HGML_FI_PWR_SMOOTHING_ENABLED)

#define HGML_POWER_SMOOTHING_MAX_NUM_PROFILES                   5
#define HGML_POWER_SMOOTHING_NUM_PROFILE_PARAMS                 4
#define HGML_POWER_SMOOTHING_ADMIN_OVERRIDE_NOT_SET             0xFFFFFFFFU
#define HGML_POWER_SMOOTHING_PROFILE_PARAM_PERCENT_TMP_FLOOR    0
#define HGML_POWER_SMOOTHING_PROFILE_PARAM_RAMP_UP_RATE         1
#define HGML_POWER_SMOOTHING_PROFILE_PARAM_RAMP_DOWN_RATE       2
#define HGML_POWER_SMOOTHING_PROFILE_PARAM_RAMP_DOWN_HYSTERESIS 3

typedef struct
{
    unsigned int version;
    unsigned int profileId;
    unsigned int paramId;
    double value;
} hgmlPowerSmoothingProfile_v1_t;
typedef hgmlPowerSmoothingProfile_v1_t  hgmlPowerSmoothingProfile_t;
#define hgmlPowerSmoothingProfile_v1 HGML_STRUCT_VERSION(PowerSmoothingProfile, 1)

typedef struct
{
    unsigned int version;
    hgmlEnableState_t state;
} hgmlPowerSmoothingState_v1_t;
typedef hgmlPowerSmoothingState_v1_t  hgmlPowerSmoothingState_t;
#define hgmlPowerSmoothingState_v1 HGML_STRUCT_VERSION(PowerSmoothingState, 1)

#define hgmlBlacklistDeviceInfo_t hgmlExcludedDeviceInfo_t

hgmlReturn_t hgmlComputeInstanceDestroy(hgmlComputeInstance_t computeInstance);
hgmlReturn_t hgmlComputeInstanceGetInfo(hgmlComputeInstance_t computeInstance, hgmlComputeInstanceInfo_t *info);
hgmlReturn_t hgmlComputeInstanceGetInfo_v2(hgmlComputeInstance_t computeInstance, hgmlComputeInstanceInfo_t *info);
hgmlReturn_t hgmlDeviceClearAccountingPids(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceClearCpuAffinity(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceClearEccErrorCounts(hgmlDevice_t device, hgmlEccCounterType_t counterType);
hgmlReturn_t hgmlDeviceClearFieldValues(hgmlDevice_t device, int valuesCount, hgmlFieldValue_t *values);
hgmlReturn_t hgmlDeviceCreateGpuInstance(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstance_t *gpuInstance);
hgmlReturn_t hgmlDeviceCreateGpuInstanceWithPlacement(hgmlDevice_t device, unsigned int profileId, const hgmlGpuInstancePlacement_t *placement, hgmlGpuInstance_t *gpuInstance);
hgmlReturn_t hgmlDeviceCreateVgpuInstance(hgmlDevice_t device, hgmlVgpuProfileId_t vgpuProfileId, hgmlVgpuInstance_t* vgpuInstance);
hgmlReturn_t hgmlDeviceDestroyVgpuInstance(hgmlVgpuInstance_t vgpuInstance, bool force);
hgmlReturn_t hgmlDeviceDiscoverGpus(hgmlPciInfo_t *pciInfo);
hgmlReturn_t hgmlDeviceGetAPIRestriction(hgmlDevice_t device, hgmlRestrictedAPI_t apiType, hgmlEnableState_t *isRestricted);
hgmlReturn_t hgmlDeviceGetAccountingBufferSize(hgmlDevice_t device, unsigned int *bufferSize);
hgmlReturn_t hgmlDeviceGetAccountingMode(hgmlDevice_t device, hgmlEnableState_t *mode);
hgmlReturn_t hgmlDeviceGetAccountingPids(hgmlDevice_t device, unsigned int *count, unsigned int *pids);
hgmlReturn_t hgmlDeviceGetAccountingStats(hgmlDevice_t device, unsigned int pid, hgmlAccountingStats_t *stats);
hgmlReturn_t hgmlDeviceGetActiveVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuInstance_t *vgpuInstances);
hgmlReturn_t hgmlDeviceGetAdaptiveClockInfoStatus(hgmlDevice_t device, unsigned int *adaptiveClockStatus);
hgmlReturn_t hgmlDeviceGetAddressingMode(hgmlDevice_t device, hgmlDeviceAddressingMode_t *mode);
hgmlReturn_t hgmlDeviceGetAliveVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuInstance_t *vgpuInstances);
hgmlReturn_t hgmlDeviceGetApplicationsClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);
hgmlReturn_t hgmlDeviceGetArchitecture(hgmlDevice_t device, hgmlDeviceArchitecture_t *arch);
hgmlReturn_t hgmlDeviceGetAttributes(hgmlDevice_t device, hgmlDeviceAttributes_t *attributes);
hgmlReturn_t hgmlDeviceGetAttributes_v2(hgmlDevice_t device, hgmlDeviceAttributes_t *attributes);
hgmlReturn_t hgmlDeviceGetAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t *isEnabled, hgmlEnableState_t *defaultIsEnabled);
hgmlReturn_t hgmlDeviceGetBAR1MemoryInfo(hgmlDevice_t device, hgmlBAR1Memory_t *bar1Memory);
hgmlReturn_t hgmlDeviceGetBoardId(hgmlDevice_t device, unsigned int *boardId);
hgmlReturn_t hgmlDeviceGetBoardPartNumber(hgmlDevice_t device, char* partNumber, unsigned int length);
hgmlReturn_t hgmlDeviceGetBrand(hgmlDevice_t device, hgmlBrandType_t *type);
hgmlReturn_t hgmlDeviceGetBridgeChipInfo(hgmlDevice_t device, hgmlBridgeChipHierarchy_t *bridgeHierarchy);
hgmlReturn_t hgmlDeviceGetBusType(hgmlDevice_t device, hgmlBusType_t *type);
hgmlReturn_t hgmlDeviceGetC2cModeInfoV(hgmlDevice_t device, hgmlC2cModeInfo_v1_t *c2cModeInfo);
hgmlReturn_t hgmlDeviceGetCapabilities(hgmlDevice_t device, hgmlDeviceCapabilities_t *caps);
hgmlReturn_t hgmlDeviceGetClkMonStatus(hgmlDevice_t device, hgmlClkMonStatus_t *status);
hgmlReturn_t hgmlDeviceGetClock(hgmlDevice_t device, hgmlClockType_t clockType, hgmlClockId_t clockId, unsigned int *clockMHz);
hgmlReturn_t hgmlDeviceGetClockInfo(hgmlDevice_t device, hgmlClockType_t type, unsigned int *clock);
hgmlReturn_t hgmlDeviceGetClockOffsets(hgmlDevice_t device, hgmlClockOffset_t *info);
hgmlReturn_t hgmlDeviceGetComputeInstanceId(hgmlDevice_t device, unsigned int *id);
hgmlReturn_t hgmlDeviceGetComputeMode(hgmlDevice_t device, hgmlComputeMode_t *mode);
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);
hgmlReturn_t hgmlDeviceGetConfComputeGpuAttestationReport(hgmlDevice_t device, hgmlConfComputeGpuAttestationReport_t *gpuAtstReport);
hgmlReturn_t hgmlDeviceGetConfComputeGpuCertificate(hgmlDevice_t device, hgmlConfComputeGpuCertificate_t *gpuCert);
hgmlReturn_t hgmlDeviceGetConfComputeMemSizeInfo(hgmlDevice_t device, hgmlConfComputeMemSizeInfo_t *memInfo);
hgmlReturn_t hgmlDeviceGetConfComputeProtectedMemoryUsage(hgmlDevice_t device, hgmlMemory_t *memory);
hgmlReturn_t hgmlDeviceGetCoolerInfo(hgmlDevice_t device, hgmlCoolerInfo_t *coolerInfo);
hgmlReturn_t hgmlDeviceGetCount(unsigned int *deviceCount);
hgmlReturn_t hgmlDeviceGetCount_v2(unsigned int *deviceCount);
hgmlReturn_t hgmlDeviceGetCpuAffinity(hgmlDevice_t device, unsigned int cpuSetSize, unsigned long *cpuSet);
hgmlReturn_t hgmlDeviceGetCpuAffinityWithinScope(hgmlDevice_t device, unsigned int cpuSetSize, unsigned long *cpuSet, hgmlAffinityScope_t scope);
hgmlReturn_t hgmlDeviceGetCreatableVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuTypeId_t *vgpuTypeIds);
hgmlReturn_t hgmlDeviceGetCurrPcieLinkGeneration(hgmlDevice_t device, unsigned int *currLinkGen);
hgmlReturn_t hgmlDeviceGetCurrPcieLinkWidth(hgmlDevice_t device, unsigned int *currLinkWidth);
hgmlReturn_t hgmlDeviceGetCurrentClockFreqs(hgmlDevice_t device, hgmlDeviceCurrentClockFreqs_t *currentClockFreqs);
hgmlReturn_t hgmlDeviceGetCurrentClocksEventReasons(hgmlDevice_t device, unsigned long long *clocksEventReasons);
hgmlReturn_t hgmlDeviceGetDecoderUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);
hgmlReturn_t hgmlDeviceGetDefaultApplicationsClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);
hgmlReturn_t hgmlDeviceGetDefaultEccMode(hgmlDevice_t device, hgmlEnableState_t *defaultMode);
hgmlReturn_t hgmlDeviceGetDeviceHandleFromMigDeviceHandle(hgmlDevice_t migDevice, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetDisplayActive(hgmlDevice_t device, hgmlEnableState_t *isActive);
hgmlReturn_t hgmlDeviceGetDisplayMode(hgmlDevice_t device, hgmlEnableState_t *display);
hgmlReturn_t hgmlDeviceGetDramEncryptionMode(hgmlDevice_t device, hgmlDramEncryptionInfo_t *current, hgmlDramEncryptionInfo_t *pending);
hgmlReturn_t hgmlDeviceGetDriverModel(hgmlDevice_t device, hgmlDriverModel_t *current, hgmlDriverModel_t *pending);
hgmlReturn_t hgmlDeviceGetDriverModel_v2(hgmlDevice_t device, hgmlDriverModel_t *current, hgmlDriverModel_t *pending);
hgmlReturn_t hgmlDeviceGetDynamicPstatesInfo(hgmlDevice_t device, hgmlGpuDynamicPstatesInfo_t *pDynamicPstatesInfo);
hgmlReturn_t hgmlDeviceGetEccMode(hgmlDevice_t device, hgmlEnableState_t *current, hgmlEnableState_t *pending);
hgmlReturn_t hgmlDeviceGetEncoderCapacity(hgmlDevice_t device, hgmlEncoderType_t encoderQueryType, unsigned int *encoderCapacity);
hgmlReturn_t hgmlDeviceGetEncoderSessions(hgmlDevice_t device, unsigned int *sessionCount, hgmlEncoderSessionInfo_t *sessionInfos);
hgmlReturn_t hgmlDeviceGetEncoderStats(hgmlDevice_t device, unsigned int *sessionCount, unsigned int *averageFps, unsigned int *averageLatency);
hgmlReturn_t hgmlDeviceGetEncoderUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);
hgmlReturn_t hgmlDeviceGetEnforcedPowerLimit(hgmlDevice_t device, unsigned int *limit);
hgmlReturn_t hgmlDeviceGetFBCSessions(hgmlDevice_t device, unsigned int *sessionCount, hgmlFBCSessionInfo_t *sessionInfo);
hgmlReturn_t hgmlDeviceGetFBCStats(hgmlDevice_t device, hgmlFBCStats_t *fbcStats);
hgmlReturn_t hgmlDeviceGetFanControlPolicy_v2(hgmlDevice_t device, unsigned int fan, hgmlFanControlPolicy_t *policy);
hgmlReturn_t hgmlDeviceGetFanSpeed(hgmlDevice_t device, unsigned int *speed);
hgmlReturn_t hgmlDeviceGetFanSpeedRPM(hgmlDevice_t device, hgmlFanSpeedInfo_t *fanSpeed);
hgmlReturn_t hgmlDeviceGetFanSpeed_v2(hgmlDevice_t device, unsigned int fan, unsigned int * speed);
hgmlReturn_t hgmlDeviceGetFieldValues(hgmlDevice_t device, int valuesCount, hgmlFieldValue_t *values);
hgmlReturn_t hgmlDeviceGetGpcClkMinMaxVfOffset(hgmlDevice_t device, int *minOffset, int *maxOffset);
hgmlReturn_t hgmlDeviceGetGpcClkVfOffset(hgmlDevice_t device, int *offset);
hgmlReturn_t hgmlDeviceGetGpuFabricInfoV(hgmlDevice_t device, hgmlGpuFabricInfoV_t *gpuFabricInfo);
hgmlReturn_t hgmlDeviceGetGpuInstanceById(hgmlDevice_t device, unsigned int id, hgmlGpuInstance_t *gpuInstance);
hgmlReturn_t hgmlDeviceGetGpuInstanceId(hgmlDevice_t device, unsigned int *id);
hgmlReturn_t hgmlDeviceGetGpuInstancePossiblePlacements(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstancePlacement_t *placements, unsigned int *count);
hgmlReturn_t hgmlDeviceGetGpuInstancePossiblePlacements_v2(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstancePlacement_t *placements, unsigned int *count);
hgmlReturn_t hgmlDeviceGetGpuInstanceProfileInfo(hgmlDevice_t device, unsigned int profile, hgmlGpuInstanceProfileInfo_t *info);
hgmlReturn_t hgmlDeviceGetGpuInstanceProfileInfoByIdV(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstanceProfileInfo_v2_t *info);
hgmlReturn_t hgmlDeviceGetGpuInstanceProfileInfoV(hgmlDevice_t device, unsigned int profile, hgmlGpuInstanceProfileInfo_v2_t *info);
hgmlReturn_t hgmlDeviceGetGpuInstanceRemainingCapacity(hgmlDevice_t device, unsigned int profileId, unsigned int *count);
hgmlReturn_t hgmlDeviceGetGpuInstances(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstance_t *gpuInstances, unsigned int *count);
hgmlReturn_t hgmlDeviceGetGpuMaxPcieLinkGeneration(hgmlDevice_t device, unsigned int *maxLinkGenDevice);
hgmlReturn_t hgmlDeviceGetGpuOperationMode(hgmlDevice_t device, hgmlGpuOperationMode_t *current, hgmlGpuOperationMode_t *pending);
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);
hgmlReturn_t hgmlDeviceGetHandleByIndex(unsigned int index, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByIndex_v2(unsigned int index, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByPciBusId(const char *pciBusId, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByPciBusId_v2(const char *pciBusId, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByUUID(const char *uuid, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByUUIDV(const hgmlUUID_t *uuid, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHggcComputeCapability(hgmlDevice_t device, int *major, int *minor);
hgmlReturn_t hgmlDeviceGetHostVgpuMode(hgmlDevice_t device, hgmlHostVgpuMode_t *pHostVgpuMode);
hgmlReturn_t hgmlDeviceGetHostname_v1(hgmlDevice_t device, hgmlHostname_v1_t *hostname);
hgmlReturn_t hgmlDeviceGetIcnLinkBwMode(hgmlDevice_t device, hgmlIcnLinkGetBwMode_t *getBwMode);
hgmlReturn_t hgmlDeviceGetIcnLinkCapability(hgmlDevice_t device, unsigned int link, hgmlIcnLinkCapability_t capability, unsigned int *capResult);
hgmlReturn_t hgmlDeviceGetIcnLinkErrorCounter(hgmlDevice_t device, unsigned int link, hgmlIcnLinkErrorCounter_t counter, unsigned long long *counterValue);
hgmlReturn_t hgmlDeviceGetIcnLinkInfo(hgmlDevice_t device, hgmlIcnLinkInfo_t *info);
hgmlReturn_t hgmlDeviceGetIcnLinkRemoteDeviceType(hgmlDevice_t device, unsigned int link, hgmlIntIcnLinkDeviceType_t *pIcnLinkDeviceType);
hgmlReturn_t hgmlDeviceGetIcnLinkRemotePciInfo(hgmlDevice_t device, unsigned int link, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetIcnLinkRemotePciInfo_v2(hgmlDevice_t device, unsigned int link, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetIcnLinkState(hgmlDevice_t device, unsigned int link, hgmlEnableState_t *isActive);
hgmlReturn_t hgmlDeviceGetIcnLinkSupportedBwModes(hgmlDevice_t device, hgmlIcnLinkSupportedBwModes_t *supportedBwMode);
hgmlReturn_t hgmlDeviceGetIcnLinkVersion(hgmlDevice_t device, unsigned int link, unsigned int *version);
hgmlReturn_t hgmlDeviceGetIndex(hgmlDevice_t device, unsigned int *index);
hgmlReturn_t hgmlDeviceGetInforomConfigurationChecksum(hgmlDevice_t device, unsigned int *checksum);
hgmlReturn_t hgmlDeviceGetInforomImageVersion(hgmlDevice_t device, char *version, unsigned int length);
hgmlReturn_t hgmlDeviceGetInforomVersion(hgmlDevice_t device, hgmlInforomObject_t object, char *version, unsigned int length);
hgmlReturn_t hgmlDeviceGetIrqNum(hgmlDevice_t device, unsigned int *irqNum);
hgmlReturn_t hgmlDeviceGetJpgUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);
hgmlReturn_t hgmlDeviceGetLastBBXFlushTime(hgmlDevice_t device, unsigned long long *timestamp, unsigned long *durationUs);
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);
hgmlReturn_t hgmlDeviceGetMarginTemperature(hgmlDevice_t device, hgmlMarginTemperature_t *marginTempInfo);
hgmlReturn_t hgmlDeviceGetMaxClockInfo(hgmlDevice_t device, hgmlClockType_t type, unsigned int *clock);
hgmlReturn_t hgmlDeviceGetMaxCustomerBoostClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);
hgmlReturn_t hgmlDeviceGetMaxMigDeviceCount(hgmlDevice_t device, unsigned int *count);
hgmlReturn_t hgmlDeviceGetMaxPcieLinkGeneration(hgmlDevice_t device, unsigned int *maxLinkGen);
hgmlReturn_t hgmlDeviceGetMaxPcieLinkWidth(hgmlDevice_t device, unsigned int *maxLinkWidth);
hgmlReturn_t hgmlDeviceGetMemClkMinMaxVfOffset(hgmlDevice_t device, int *minOffset, int *maxOffset);
hgmlReturn_t hgmlDeviceGetMemClkVfOffset(hgmlDevice_t device, int *offset);
hgmlReturn_t hgmlDeviceGetMemoryAffinity(hgmlDevice_t device, unsigned int nodeSetSize, unsigned long *nodeSet, hgmlAffinityScope_t scope);
hgmlReturn_t hgmlDeviceGetMemoryBusWidth(hgmlDevice_t device, unsigned int *busWidth);
hgmlReturn_t hgmlDeviceGetMemoryErrorCounter(hgmlDevice_t device, hgmlMemoryErrorType_t errorType, hgmlEccCounterType_t counterType, hgmlMemoryLocation_t locationType, unsigned long long *count);
hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory);
hgmlReturn_t hgmlDeviceGetMemoryInfo_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory);
hgmlReturn_t hgmlDeviceGetMigDeviceHandleByIndex(hgmlDevice_t device, unsigned int index, hgmlDevice_t *migDevice);
hgmlReturn_t hgmlDeviceGetMigMode(hgmlDevice_t device, unsigned int *currentMode, unsigned int *pendingMode);
hgmlReturn_t hgmlDeviceGetMinMaxClockOfPState(hgmlDevice_t device, hgmlClockType_t type, hgmlPstates_t pstate, unsigned int * minClockMHz, unsigned int * maxClockMHz);
hgmlReturn_t hgmlDeviceGetMinMaxFanSpeed(hgmlDevice_t device, unsigned int * minSpeed, unsigned int * maxSpeed);
hgmlReturn_t hgmlDeviceGetMinorNumber(hgmlDevice_t device, unsigned int *minorNumber);
hgmlReturn_t hgmlDeviceGetModuleId(hgmlDevice_t device, unsigned int *moduleId);
hgmlReturn_t hgmlDeviceGetMultiGpuBoard(hgmlDevice_t device, unsigned int *multiGpuBool);
hgmlReturn_t hgmlDeviceGetName(hgmlDevice_t device, char *name, unsigned int length);
hgmlReturn_t hgmlDeviceGetNumFans(hgmlDevice_t device, unsigned int *numFans);
hgmlReturn_t hgmlDeviceGetNumGpuCores(hgmlDevice_t device, unsigned int *numCores);
hgmlReturn_t hgmlDeviceGetNumaNodeId(hgmlDevice_t device, unsigned int *node);
hgmlReturn_t hgmlDeviceGetOfaUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);
hgmlReturn_t hgmlDeviceGetP2PStatus(hgmlDevice_t device1, hgmlDevice_t device2, hgmlGpuP2PCapsIndex_t p2pIndex,hgmlGpuP2PStatus_t *p2pStatus);
hgmlReturn_t hgmlDeviceGetPciInfo(hgmlDevice_t device, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetPciInfoExt(hgmlDevice_t device, hgmlPciInfoExt_t *pci);
hgmlReturn_t hgmlDeviceGetPciInfo_v2(hgmlDevice_t device, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetPciInfo_v3(hgmlDevice_t device, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetPcieLinkMaxSpeed(hgmlDevice_t device, unsigned int *maxSpeed);
hgmlReturn_t hgmlDeviceGetPcieReplayCounter(hgmlDevice_t device, unsigned int *value);
hgmlReturn_t hgmlDeviceGetPcieSpeed(hgmlDevice_t device, unsigned int *pcieSpeed);
hgmlReturn_t hgmlDeviceGetPcieThroughput(hgmlDevice_t device, hgmlPcieUtilCounter_t counter, unsigned int *value);
hgmlReturn_t hgmlDeviceGetPdi(hgmlDevice_t device, hgmlPdi_t *pdi);
hgmlReturn_t hgmlDeviceGetPerformanceModes(hgmlDevice_t device, hgmlDevicePerfModes_t *perfModes);
hgmlReturn_t hgmlDeviceGetPerformanceState(hgmlDevice_t device, hgmlPstates_t *pState);
hgmlReturn_t hgmlDeviceGetPersistenceMode(hgmlDevice_t device, hgmlEnableState_t *mode);
hgmlReturn_t hgmlDeviceGetPgpuMetadataString(hgmlDevice_t device, char *pgpuMetadata, unsigned int *bufferSize);
hgmlReturn_t hgmlDeviceGetPlatformInfo(hgmlDevice_t device, hgmlPlatformInfo_t *platformInfo);
hgmlReturn_t hgmlDeviceGetPowerManagementDefaultLimit(hgmlDevice_t device, unsigned int *defaultLimit);
hgmlReturn_t hgmlDeviceGetPowerManagementLimit(hgmlDevice_t device, unsigned int *limit);
hgmlReturn_t hgmlDeviceGetPowerManagementLimitConstraints(hgmlDevice_t device, unsigned int *minLimit, unsigned int *maxLimit);
hgmlReturn_t hgmlDeviceGetPowerMizerMode_v1(hgmlDevice_t device, hgmlDevicePowerMizerModes_v1_t *powerMizerMode);
hgmlReturn_t hgmlDeviceGetPowerSource(hgmlDevice_t device, hgmlPowerSource_t *powerSource);
hgmlReturn_t hgmlDeviceGetPowerUsage(hgmlDevice_t device, unsigned int *power);
hgmlReturn_t hgmlDeviceGetProcessUtilization(hgmlDevice_t device, hgmlProcessUtilizationSample_t *utilization, unsigned int *processSamplesCount, unsigned long long lastSeenTimeStamp);
hgmlReturn_t hgmlDeviceGetProcessesUtilizationInfo(hgmlDevice_t device, hgmlProcessesUtilizationInfo_t *procesesUtilInfo);
hgmlReturn_t hgmlDeviceGetRemappedRows(hgmlDevice_t device, unsigned int *corrRows, unsigned int *uncRows, unsigned int *isPending, unsigned int *failureOccurred);
hgmlReturn_t hgmlDeviceGetRepairStatus(hgmlDevice_t device, hgmlRepairStatus_t *repairStatus);
hgmlReturn_t hgmlDeviceGetRetiredPages(hgmlDevice_t device, hgmlPageRetirementCause_t cause, unsigned int *pageCount, unsigned long long *addresses);
hgmlReturn_t hgmlDeviceGetRetiredPagesPendingStatus(hgmlDevice_t device, hgmlEnableState_t *isPending);
hgmlReturn_t hgmlDeviceGetRetiredPages_v2(hgmlDevice_t device, hgmlPageRetirementCause_t cause, unsigned int *pageCount, unsigned long long *addresses, unsigned long long *timestamps);
hgmlReturn_t hgmlDeviceGetRowRemapperHistogram(hgmlDevice_t device, hgmlRowRemapperHistogramValues_t *values);
hgmlReturn_t hgmlDeviceGetRunningProcessDetailList(hgmlDevice_t device, hgmlProcessDetailList_t *plist);
hgmlReturn_t hgmlDeviceGetSamples(hgmlDevice_t device, hgmlSamplingType_t type, unsigned long long lastSeenTimeStamp, hgmlValueType_t *sampleValType, unsigned int *sampleCount, hgmlSample_t *samples);
hgmlReturn_t hgmlDeviceGetSerial(hgmlDevice_t device, char *serial, unsigned int length);
hgmlReturn_t hgmlDeviceGetSramEccErrorStatus(hgmlDevice_t device, hgmlEccSramErrorStatus_t *status);
hgmlReturn_t hgmlDeviceGetSramUniqueUncorrectedEccErrorCounts(hgmlDevice_t device, hgmlEccSramUniqueUncorrectedErrorCounts_t *errorCounts);
hgmlReturn_t hgmlDeviceGetSupportedClocksEventReasons(hgmlDevice_t device, unsigned long long *supportedClocksEventReasons);
hgmlReturn_t hgmlDeviceGetSupportedEventTypes(hgmlDevice_t device, unsigned long long *eventTypes);
hgmlReturn_t hgmlDeviceGetSupportedGraphicsClocks(hgmlDevice_t device, unsigned int memoryClockMHz, unsigned int *count, unsigned int *clocksMHz);
hgmlReturn_t hgmlDeviceGetSupportedMemoryClocks(hgmlDevice_t device, unsigned int *count, unsigned int *clocksMHz);
hgmlReturn_t hgmlDeviceGetSupportedPerformanceStates(hgmlDevice_t device, hgmlPstates_t *pstates, unsigned int size);
hgmlReturn_t hgmlDeviceGetSupportedVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuTypeId_t *vgpuTypeIds);
hgmlReturn_t hgmlDeviceGetTargetFanSpeed(hgmlDevice_t device, unsigned int fan, unsigned int *targetSpeed);
hgmlReturn_t hgmlDeviceGetTemperatureThreshold(hgmlDevice_t device, hgmlTemperatureThresholds_t thresholdType, unsigned int *temp);
hgmlReturn_t hgmlDeviceGetTemperatureV(hgmlDevice_t device, hgmlTemperature_t *temperature);
hgmlReturn_t hgmlDeviceGetThermalSettings(hgmlDevice_t device, unsigned int sensorIndex, hgmlGpuThermalSettings_t *pThermalSettings);
hgmlReturn_t hgmlDeviceGetTopologyCommonAncestor(hgmlDevice_t device1, hgmlDevice_t device2, hgmlGpuTopologyLevel_t *pathInfo);
hgmlReturn_t hgmlDeviceGetTopologyNearestGpus(hgmlDevice_t device, hgmlGpuTopologyLevel_t level, unsigned int *count, hgmlDevice_t *deviceArray);
hgmlReturn_t hgmlDeviceGetTotalEccErrors(hgmlDevice_t device, hgmlMemoryErrorType_t errorType, hgmlEccCounterType_t counterType, unsigned long long *eccCounts);
hgmlReturn_t hgmlDeviceGetTotalEnergyConsumption(hgmlDevice_t device, unsigned long long *energy);
hgmlReturn_t hgmlDeviceGetUUID(hgmlDevice_t device, char *uuid, unsigned int length);
hgmlReturn_t hgmlDeviceGetUtilizationRates(hgmlDevice_t device, hgmlUtilization_t *utilization);
hgmlReturn_t hgmlDeviceGetVbiosVersion(hgmlDevice_t device, char *version, unsigned int length);
hgmlReturn_t hgmlDeviceGetVgpuCapabilities(hgmlDevice_t device, hgmlDeviceVgpuCapability_t capability, unsigned int *capResult);
hgmlReturn_t hgmlDeviceGetVgpuHeterogeneousMode(hgmlDevice_t device, hgmlVgpuHeterogeneousMode_t *pHeterogeneousMode);
hgmlReturn_t hgmlDeviceGetVgpuInstancesUtilizationInfo(hgmlDevice_t device, hgmlVgpuInstancesUtilizationInfo_t *vgpuUtilInfo);
hgmlReturn_t hgmlDeviceGetVgpuMetadata(hgmlDevice_t device, hgmlVgpuPgpuMetadata_t *pgpuMetadata, unsigned int *bufferSize);
hgmlReturn_t hgmlDeviceGetVgpuProcessUtilization(hgmlDevice_t device, unsigned long long lastSeenTimeStamp, unsigned int *vgpuProcessSamplesCount, hgmlVgpuProcessUtilizationSample_t *utilizationSamples);
hgmlReturn_t hgmlDeviceGetVgpuProcessesUtilizationInfo(hgmlDevice_t device, hgmlVgpuProcessesUtilizationInfo_t *vgpuProcUtilInfo);
hgmlReturn_t hgmlDeviceGetVgpuSchedulerCapabilities(hgmlDevice_t device, hgmlVgpuSchedulerCapabilities_t *pCapabilities);
hgmlReturn_t hgmlDeviceGetVgpuSchedulerLog(hgmlDevice_t device, hgmlVgpuSchedulerLog_t *pSchedulerLog);
hgmlReturn_t hgmlDeviceGetVgpuSchedulerState(hgmlDevice_t device, hgmlVgpuSchedulerGetState_t *pSchedulerState);
hgmlReturn_t hgmlDeviceGetVgpuTypeCreatablePlacements(hgmlDevice_t device, hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuPlacementList_t *pPlacementList);
hgmlReturn_t hgmlDeviceGetVgpuTypeSupportedPlacements(hgmlDevice_t device, hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuPlacementList_t *pPlacementList);
hgmlReturn_t hgmlDeviceGetVgpuUtilization(hgmlDevice_t device, unsigned long long lastSeenTimeStamp, hgmlValueType_t *sampleValType, unsigned int *vgpuInstanceSamplesCount, hgmlVgpuInstanceUtilizationSample_t *utilizationSamples);
hgmlReturn_t hgmlDeviceGetVirtualizationMode(hgmlDevice_t device, hgmlGpuVirtualizationMode_t *pVirtualMode);
hgmlReturn_t hgmlDeviceIsMigDeviceHandle(hgmlDevice_t device, unsigned int *isMigDevice);
hgmlReturn_t hgmlDeviceModifyDrainState(hgmlPciInfo_t *pciInfo, hgmlEnableState_t newState);
hgmlReturn_t hgmlDeviceOnSameBoard(hgmlDevice_t device1, hgmlDevice_t device2, int *onSameBoard);
hgmlReturn_t hgmlDevicePowerSmoothingActivatePresetProfile(hgmlDevice_t device, hgmlPowerSmoothingProfile_t *profile);
hgmlReturn_t hgmlDevicePowerSmoothingSetState(hgmlDevice_t device, hgmlPowerSmoothingState_t *state);
hgmlReturn_t hgmlDevicePowerSmoothingUpdatePresetProfileParam(hgmlDevice_t device, hgmlPowerSmoothingProfile_t *profile);
hgmlReturn_t hgmlDeviceQueryDrainState(hgmlPciInfo_t *pciInfo, hgmlEnableState_t *currentState);
hgmlReturn_t hgmlDeviceReadWritePRM_v1(hgmlDevice_t device, hgmlPRMTLV_v1_t *buffer);
hgmlReturn_t hgmlDeviceRegisterEvents(hgmlDevice_t device, unsigned long long eventTypes, hgmlEventSet_t set);
hgmlReturn_t hgmlDeviceRemoveGpu(hgmlPciInfo_t *pciInfo);
hgmlReturn_t hgmlDeviceRemoveGpu_v2(hgmlPciInfo_t *pciInfo, hgmlDetachGpuState_t gpuState, hgmlPcieLinkState_t linkState);
hgmlReturn_t hgmlDeviceResetGpuLockedClocks(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceResetIcnLinkErrorCounters(hgmlDevice_t device, unsigned int link);
hgmlReturn_t hgmlDeviceResetMemoryLockedClocks(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceSetAPIRestriction(hgmlDevice_t device, hgmlRestrictedAPI_t apiType, hgmlEnableState_t isRestricted);
hgmlReturn_t hgmlDeviceSetAccountingMode(hgmlDevice_t device, hgmlEnableState_t mode);
hgmlReturn_t hgmlDeviceSetAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t enabled);
hgmlReturn_t hgmlDeviceSetClockOffsets(hgmlDevice_t device, hgmlClockOffset_t *info);
hgmlReturn_t hgmlDeviceSetComputeMode(hgmlDevice_t device, hgmlComputeMode_t mode);
hgmlReturn_t hgmlDeviceSetConfComputeUnprotectedMemSize(hgmlDevice_t device, unsigned long long sizeKiB);
hgmlReturn_t hgmlDeviceSetCpuAffinity(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceSetDefaultAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t enabled, unsigned int flags);
hgmlReturn_t hgmlDeviceSetDefaultFanSpeed_v2(hgmlDevice_t device, unsigned int fan);
hgmlReturn_t hgmlDeviceSetDramEncryptionMode(hgmlDevice_t device, const hgmlDramEncryptionInfo_t *dramEncryption);
hgmlReturn_t hgmlDeviceSetDriverModel(hgmlDevice_t device, hgmlDriverModel_t driverModel, unsigned int flags);
hgmlReturn_t hgmlDeviceSetEccMode(hgmlDevice_t device, hgmlEnableState_t ecc);
hgmlReturn_t hgmlDeviceSetFanControlPolicy(hgmlDevice_t device, unsigned int fan, hgmlFanControlPolicy_t policy);
hgmlReturn_t hgmlDeviceSetFanSpeed_v2(hgmlDevice_t device, unsigned int fan, unsigned int speed);
hgmlReturn_t hgmlDeviceSetGpuLockedClocks(hgmlDevice_t device, unsigned int minGpuClockMHz, unsigned int maxGpuClockMHz);
hgmlReturn_t hgmlDeviceSetGpuOperationMode(hgmlDevice_t device, hgmlGpuOperationMode_t mode);
hgmlReturn_t hgmlDeviceSetHostname_v1(hgmlDevice_t device, hgmlHostname_v1_t *hostname);
hgmlReturn_t hgmlDeviceSetIcnLinkBwMode(hgmlDevice_t device, hgmlIcnLinkSetBwMode_t *setBwMode);
hgmlReturn_t hgmlDeviceSetIcnLinkDeviceLowPowerThreshold(hgmlDevice_t device, hgmlIcnLinkPowerThres_t *info);
hgmlReturn_t hgmlDeviceSetMemoryLockedClocks(hgmlDevice_t device, unsigned int minMemClockMHz, unsigned int maxMemClockMHz);
hgmlReturn_t hgmlDeviceSetMigMode(hgmlDevice_t device, unsigned int mode, hgmlReturn_t *activationStatus);
hgmlReturn_t hgmlDeviceSetPersistenceMode(hgmlDevice_t device, hgmlEnableState_t mode);
hgmlReturn_t hgmlDeviceSetPowerManagementLimit(hgmlDevice_t device, unsigned int limit);
hgmlReturn_t hgmlDeviceSetPowerManagementLimit_v2(hgmlDevice_t device, hgmlPowerValue_v2_t *powerValue);
hgmlReturn_t hgmlDeviceSetPowerMizerMode_v1(hgmlDevice_t device, hgmlDevicePowerMizerModes_v1_t *powerMizerMode);
hgmlReturn_t hgmlDeviceSetTemperatureThreshold(hgmlDevice_t device, hgmlTemperatureThresholds_t thresholdType, int *temp);
hgmlReturn_t hgmlDeviceSetVgpuCapabilities(hgmlDevice_t device, hgmlDeviceVgpuCapability_t capability, hgmlEnableState_t state);
hgmlReturn_t hgmlDeviceSetVgpuHeterogeneousMode(hgmlDevice_t device, const hgmlVgpuHeterogeneousMode_t *pHeterogeneousMode);
hgmlReturn_t hgmlDeviceSetVgpuSchedulerState(hgmlDevice_t device, hgmlVgpuSchedulerSetState_t *pSchedulerState);
hgmlReturn_t hgmlDeviceSetVirtualizationMode(hgmlDevice_t device, hgmlGpuVirtualizationMode_t virtualMode);
hgmlReturn_t hgmlDeviceValidateInforom(hgmlDevice_t device);
hgmlReturn_t hgmlDeviceWorkloadPowerProfileClearRequestedProfiles(hgmlDevice_t device, hgmlWorkloadPowerProfileRequestedProfiles_t *requestedProfiles);
hgmlReturn_t hgmlDeviceWorkloadPowerProfileGetCurrentProfiles(hgmlDevice_t device, hgmlWorkloadPowerProfileCurrentProfiles_t *currentProfiles);
hgmlReturn_t hgmlDeviceWorkloadPowerProfileGetProfilesInfo(hgmlDevice_t device, hgmlWorkloadPowerProfileProfilesInfo_t *profilesInfo);
hgmlReturn_t hgmlDeviceWorkloadPowerProfileSetRequestedProfiles(hgmlDevice_t device, hgmlWorkloadPowerProfileRequestedProfiles_t *requestedProfiles);
hgmlReturn_t hgmlEventSetCreate(hgmlEventSet_t *set);
hgmlReturn_t hgmlEventSetFree(hgmlEventSet_t set);
hgmlReturn_t hgmlEventSetWait(hgmlEventSet_t set, hgmlEventData_t * data, unsigned int timeoutms);
hgmlReturn_t hgmlEventSetWait_v2(hgmlEventSet_t set, hgmlEventData_t * data, unsigned int timeoutms);
hgmlReturn_t hgmlGetBlacklistDeviceCount(unsigned int *deviceCount);
hgmlReturn_t hgmlGetBlacklistDeviceInfoByIndex(unsigned int index, hgmlBlacklistDeviceInfo_t *info);
hgmlReturn_t hgmlGetExcludedDeviceCount(unsigned int *deviceCount);
hgmlReturn_t hgmlGetExcludedDeviceInfoByIndex(unsigned int index, hgmlExcludedDeviceInfo_t *info);
hgmlReturn_t hgmlGetVgpuCompatibility(hgmlVgpuMetadata_t *vgpuMetadata, hgmlVgpuPgpuMetadata_t *pgpuMetadata, hgmlVgpuPgpuCompatibility_t *compatibilityInfo);
hgmlReturn_t hgmlGetVgpuDriverCapabilities(hgmlVgpuDriverCapability_t capability, unsigned int *capResult);
hgmlReturn_t hgmlGetVgpuVersion(hgmlVgpuVersion_t *supported, hgmlVgpuVersion_t *current);
hgmlReturn_t hgmlGpmMetricsGet(hgmlGpmMetricsGet_t *metricsGet);
hgmlReturn_t hgmlGpmMigSampleGet(hgmlDevice_t device, unsigned int gpuInstanceId, hgmlGpmSample_t gpmSample);
hgmlReturn_t hgmlGpmQueryDeviceSupport(hgmlDevice_t device, hgmlGpmSupport_t *gpmSupport);
hgmlReturn_t hgmlGpmQueryIfStreamingEnabled(hgmlDevice_t device, unsigned int *state);
hgmlReturn_t hgmlGpmSampleAlloc(hgmlGpmSample_t *gpmSample);
hgmlReturn_t hgmlGpmSampleFree(hgmlGpmSample_t gpmSample);
hgmlReturn_t hgmlGpmSampleGet(hgmlDevice_t device, hgmlGpmSample_t gpmSample);
hgmlReturn_t hgmlGpmSetStreamingEnabled(hgmlDevice_t device, unsigned int state);
hgmlReturn_t hgmlGpuInstanceCreateComputeInstance(hgmlGpuInstance_t gpuInstance, unsigned int profileId, hgmlComputeInstance_t *computeInstance);
hgmlReturn_t hgmlGpuInstanceCreateComputeInstanceWithPlacement(hgmlGpuInstance_t gpuInstance, unsigned int profileId, const hgmlComputeInstancePlacement_t *placement, hgmlComputeInstance_t *computeInstance);
hgmlReturn_t hgmlGpuInstanceDestroy(hgmlGpuInstance_t gpuInstance);
hgmlReturn_t hgmlGpuInstanceGetActiveVgpus(hgmlGpuInstance_t gpuInstance, hgmlActiveVgpuInstanceInfo_t *pVgpuInstanceInfo);
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceById(hgmlGpuInstance_t gpuInstance, unsigned int id, hgmlComputeInstance_t *computeInstance);
hgmlReturn_t hgmlGpuInstanceGetComputeInstancePossiblePlacements(hgmlGpuInstance_t gpuInstance, unsigned int profileId, hgmlComputeInstancePlacement_t *placements, unsigned int *count);
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceProfileInfo(hgmlGpuInstance_t gpuInstance, unsigned int profile, unsigned int engProfile, hgmlComputeInstanceProfileInfo_t *info);
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceProfileInfoV(hgmlGpuInstance_t gpuInstance, unsigned int profile, unsigned int engProfile, hgmlComputeInstanceProfileInfo_v2_t *info);
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceRemainingCapacity(hgmlGpuInstance_t gpuInstance, unsigned int profileId, unsigned int *count);
hgmlReturn_t hgmlGpuInstanceGetComputeInstances(hgmlGpuInstance_t gpuInstance, unsigned int profileId, hgmlComputeInstance_t *computeInstances, unsigned int *count);
hgmlReturn_t hgmlGpuInstanceGetCreatableVgpus(hgmlGpuInstance_t gpuInstance, hgmlVgpuTypeIdInfo_t *pVgpus);
hgmlReturn_t hgmlGpuInstanceGetInfo(hgmlGpuInstance_t gpuInstance, hgmlGpuInstanceInfo_t *info);
hgmlReturn_t hgmlGpuInstanceGetVgpuHeterogeneousMode(hgmlGpuInstance_t gpuInstance, hgmlVgpuHeterogeneousMode_t *pHeterogeneousMode);
hgmlReturn_t hgmlGpuInstanceGetVgpuSchedulerLog(hgmlGpuInstance_t gpuInstance, hgmlVgpuSchedulerLogInfo_t *pSchedulerLogInfo);
hgmlReturn_t hgmlGpuInstanceGetVgpuSchedulerState(hgmlGpuInstance_t gpuInstance, hgmlVgpuSchedulerStateInfo_t *pSchedulerStateInfo);
hgmlReturn_t hgmlGpuInstanceGetVgpuTypeCreatablePlacements(hgmlGpuInstance_t gpuInstance, hgmlVgpuCreatablePlacementInfo_t *pCreatablePlacementInfo);
hgmlReturn_t hgmlGpuInstanceSetVgpuHeterogeneousMode(hgmlGpuInstance_t gpuInstance, const hgmlVgpuHeterogeneousMode_t *pHeterogeneousMode);
hgmlReturn_t hgmlGpuInstanceSetVgpuSchedulerState(hgmlGpuInstance_t gpuInstance, hgmlVgpuSchedulerState_t *pScheduler);
hgmlReturn_t hgmlInit(void);
hgmlReturn_t hgmlInitWithFlags(unsigned int flags);
hgmlReturn_t hgmlInit_v2(void);
hgmlReturn_t hgmlSetVgpuVersion(hgmlVgpuVersion_t *vgpuVersion);
hgmlReturn_t hgmlShutdown(void);
hgmlReturn_t hgmlSystemEventSetCreate(hgmlSystemEventSetCreateRequest_t *request);
hgmlReturn_t hgmlSystemEventSetFree(hgmlSystemEventSetFreeRequest_t *request);
hgmlReturn_t hgmlSystemEventSetWait(hgmlSystemEventSetWaitRequest_t *request);
hgmlReturn_t hgmlSystemGetConfComputeCapabilities(hgmlConfComputeSystemCaps_t *capabilities);
hgmlReturn_t hgmlSystemGetConfComputeGpusReadyState(unsigned int *isAcceptingWork);
hgmlReturn_t hgmlSystemGetConfComputeKeyRotationThresholdInfo(hgmlConfComputeGetKeyRotationThresholdInfo_t *pKeyRotationThrInfo);
hgmlReturn_t hgmlSystemGetConfComputeSettings(hgmlSystemConfComputeSettings_t *settings);
hgmlReturn_t hgmlSystemGetConfComputeState(hgmlConfComputeSystemState_t *state);
hgmlReturn_t hgmlSystemGetDriverBranch(hgmlSystemDriverBranchInfo_t *branchInfo, unsigned int length);
hgmlReturn_t hgmlSystemGetDriverVersion(char *version, unsigned int length);
hgmlReturn_t hgmlSystemGetHGMLVersion(char *version, unsigned int length);
hgmlReturn_t hgmlSystemGetHggcDriverVersion(int *hggcDriverVersion);
hgmlReturn_t hgmlSystemGetHggcDriverVersion_v2(int *hggcDriverVersion);
hgmlReturn_t hgmlSystemGetHicVersion(unsigned int *hwbcCount, hgmlHwbcEntry_t *hwbcEntries);
hgmlReturn_t hgmlSystemGetIcnLinkBwMode(unsigned int *icnlinkBwMode);
hgmlReturn_t hgmlSystemGetProcessName(unsigned int pid, char *name, unsigned int length);
hgmlReturn_t hgmlSystemGetTopologyGpuSet(unsigned int cpuNumber, unsigned int *count, hgmlDevice_t *deviceArray);
hgmlReturn_t hgmlSystemRegisterEvents(hgmlSystemRegisterEventRequest_t *request);
hgmlReturn_t hgmlSystemSetConfComputeGpusReadyState(unsigned int isAcceptingWork);
hgmlReturn_t hgmlSystemSetConfComputeKeyRotationThresholdInfo(hgmlConfComputeSetKeyRotationThresholdInfo_t *pKeyRotationThrInfo);
hgmlReturn_t hgmlSystemSetIcnLinkBwMode(unsigned int icnlinkBwMode);
hgmlReturn_t hgmlUnitGetCount(unsigned int *unitCount);
hgmlReturn_t hgmlUnitGetDevices(hgmlUnit_t unit, unsigned int *deviceCount, hgmlDevice_t *devices);
hgmlReturn_t hgmlUnitGetFanSpeedInfo(hgmlUnit_t unit, hgmlUnitFanSpeeds_t *fanSpeeds);
hgmlReturn_t hgmlUnitGetHandleByIndex(unsigned int index, hgmlUnit_t *unit);
hgmlReturn_t hgmlUnitGetLedState(hgmlUnit_t unit, hgmlLedState_t *state);
hgmlReturn_t hgmlUnitGetPsuInfo(hgmlUnit_t unit, hgmlPSUInfo_t *psu);
hgmlReturn_t hgmlUnitGetTemperature(hgmlUnit_t unit, unsigned int type, unsigned int *temp);
hgmlReturn_t hgmlUnitGetUnitInfo(hgmlUnit_t unit, hgmlUnitInfo_t *info);
hgmlReturn_t hgmlUnitSetLedState(hgmlUnit_t unit, hgmlLedColor_t color);
hgmlReturn_t hgmlVgpuInstanceClearAccountingPids(hgmlVgpuInstance_t vgpuInstance);
hgmlReturn_t hgmlVgpuInstanceGetAccountingMode(hgmlVgpuInstance_t vgpuInstance, hgmlEnableState_t *mode);
hgmlReturn_t hgmlVgpuInstanceGetAccountingPids(hgmlVgpuInstance_t vgpuInstance, unsigned int *count, unsigned int *pids);
hgmlReturn_t hgmlVgpuInstanceGetAccountingStats(hgmlVgpuInstance_t vgpuInstance, unsigned int pid, hgmlAccountingStats_t *stats);
hgmlReturn_t hgmlVgpuInstanceGetEccMode(hgmlVgpuInstance_t vgpuInstance, hgmlEnableState_t *eccMode);
hgmlReturn_t hgmlVgpuInstanceGetEncoderCapacity(hgmlVgpuInstance_t vgpuInstance, unsigned int *encoderCapacity);
hgmlReturn_t hgmlVgpuInstanceGetEncoderSessions(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount, hgmlEncoderSessionInfo_t *sessionInfo);
hgmlReturn_t hgmlVgpuInstanceGetEncoderStats(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount, unsigned int *averageFps, unsigned int *averageLatency);
hgmlReturn_t hgmlVgpuInstanceGetFBCSessions(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount, hgmlFBCSessionInfo_t *sessionInfo);
hgmlReturn_t hgmlVgpuInstanceGetFBCStats(hgmlVgpuInstance_t vgpuInstance, hgmlFBCStats_t *fbcStats);
hgmlReturn_t hgmlVgpuInstanceGetFbUsage(hgmlVgpuInstance_t vgpuInstance, unsigned long long *fbUsage);
hgmlReturn_t hgmlVgpuInstanceGetFrameRateLimit(hgmlVgpuInstance_t vgpuInstance, unsigned int *frameRateLimit);
hgmlReturn_t hgmlVgpuInstanceGetGpuInstanceId(hgmlVgpuInstance_t vgpuInstance, unsigned int *gpuInstanceId);
hgmlReturn_t hgmlVgpuInstanceGetGpuPciId(hgmlVgpuInstance_t vgpuInstance, char *vgpuPciId, unsigned int *length);
hgmlReturn_t hgmlVgpuInstanceGetLicenseInfo(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuLicenseInfo_t *licenseInfo);
hgmlReturn_t hgmlVgpuInstanceGetLicenseInfo_v2(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuLicenseInfo_t *licenseInfo);
hgmlReturn_t hgmlVgpuInstanceGetMdevUUID(hgmlVgpuInstance_t vgpuInstance, char *mdevUuid, unsigned int size);
hgmlReturn_t hgmlVgpuInstanceGetMetadata(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuMetadata_t *vgpuMetadata, unsigned int *bufferSize);
hgmlReturn_t hgmlVgpuInstanceGetPciInfo(hgmlVgpuInstance_t vgpuInstance, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlVgpuInstanceGetPlacementId(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuPlacementId_t *pPlacement);
hgmlReturn_t hgmlVgpuInstanceGetRuntimeStateSize(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuRuntimeState_t *pState);
hgmlReturn_t hgmlVgpuInstanceGetType(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuTypeId_t *vgpuTypeId);
hgmlReturn_t hgmlVgpuInstanceGetUUID(hgmlVgpuInstance_t vgpuInstance, char *uuid, unsigned int size);
hgmlReturn_t hgmlVgpuInstanceGetVmDriverVersion(hgmlVgpuInstance_t vgpuInstance, char* version, unsigned int length);
hgmlReturn_t hgmlVgpuInstanceGetVmID(hgmlVgpuInstance_t vgpuInstance, char *vmId, unsigned int size, hgmlVgpuVmIdType_t *vmIdType);
hgmlReturn_t hgmlVgpuInstanceSetEncoderCapacity(hgmlVgpuInstance_t vgpuInstance, unsigned int  encoderCapacity);
hgmlReturn_t hgmlVgpuTypeGetBAR1Info(hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuTypeBar1Info_t *bar1Info);
hgmlReturn_t hgmlVgpuTypeGetCapabilities(hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuCapability_t capability, unsigned int *capResult);
hgmlReturn_t hgmlVgpuTypeGetClass(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeClass, unsigned int *size);
hgmlReturn_t hgmlVgpuTypeGetDeviceID(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *deviceID, unsigned long long *subsystemID);
hgmlReturn_t hgmlVgpuTypeGetFbReservation(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *fbReservation);
hgmlReturn_t hgmlVgpuTypeGetFrameRateLimit(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *frameRateLimit);
hgmlReturn_t hgmlVgpuTypeGetFramebufferSize(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *fbSize);
hgmlReturn_t hgmlVgpuTypeGetGpuInstanceProfileId(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *gpuInstanceProfileId);
hgmlReturn_t hgmlVgpuTypeGetGspHeapSize(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *gspHeapSize);
hgmlReturn_t hgmlVgpuTypeGetLicense(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeLicenseString, unsigned int size);
hgmlReturn_t hgmlVgpuTypeGetMaxInstances(hgmlDevice_t device, hgmlVgpuTypeId_t vgpuTypeId, unsigned int *vgpuInstanceCount);
hgmlReturn_t hgmlVgpuTypeGetMaxInstancesPerGpuInstance(hgmlVgpuTypeMaxInstance_t *pMaxInstance);
hgmlReturn_t hgmlVgpuTypeGetMaxInstancesPerVm(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *vgpuInstanceCountPerVm);
hgmlReturn_t hgmlVgpuTypeGetName(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeName, unsigned int *size);
hgmlReturn_t hgmlVgpuTypeGetNumDisplayHeads(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *numDisplayHeads);
hgmlReturn_t hgmlVgpuTypeGetRemainingCapacity(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *count);
hgmlReturn_t hgmlVgpuTypeGetResolution(hgmlVgpuTypeId_t vgpuTypeId, unsigned int displayIndex, unsigned int *xdim, unsigned int *ydim);
hgmlReturn_t hgmlVgpuTypeGetVgpuProfileId(hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuProfileId_t* vgpuProfileId);
const char* hgmlErrorString(hgmlReturn_t result);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceFreezeIcnLinkUtilizationCounter(hgmlDevice_t device, unsigned int link, unsigned int counter, hgmlEnableState_t freeze);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetCurrentClocksThrottleReasons(hgmlDevice_t device, unsigned long long *clocksThrottleReasons);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetDetailedEccErrors(hgmlDevice_t device, hgmlMemoryErrorType_t errorType, hgmlEccCounterType_t counterType, hgmlEccErrorCounts_t *eccCounts);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetGpuFabricInfo(hgmlDevice_t device, hgmlGpuFabricInfo_t *gpuFabricInfo);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetHandleBySerial(const char *serial, hgmlDevice_t *device);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetIcnLinkUtilizationControl(hgmlDevice_t device, unsigned int link, unsigned int counter, hgmlIcnLinkUtilizationControl_t *control);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetIcnLinkUtilizationCounter(hgmlDevice_t device, unsigned int link, unsigned int counter, unsigned long long *rxcounter, unsigned long long *txcounter);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetPowerManagementMode(hgmlDevice_t device, hgmlEnableState_t *mode);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetPowerState(hgmlDevice_t device, hgmlPstates_t *pState);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetSupportedClocksThrottleReasons(hgmlDevice_t device, unsigned long long *supportedClocksThrottleReasons);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetTemperature(hgmlDevice_t device, hgmlTemperatureSensors_t sensorType, unsigned int *temp);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceGetViolationStatus(hgmlDevice_t device, hgmlPerfPolicyType_t perfPolicyType, hgmlViolationTime_t *violTime);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceResetApplicationsClocks(hgmlDevice_t device);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceResetIcnLinkUtilizationCounter(hgmlDevice_t device, unsigned int link, unsigned int counter);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceSetApplicationsClocks(hgmlDevice_t device, unsigned int memClockMHz, unsigned int graphicsClockMHz);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceSetGpcClkVfOffset(hgmlDevice_t device, int offset);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceSetIcnLinkUtilizationControl(hgmlDevice_t device, unsigned int link, unsigned int counter, hgmlIcnLinkUtilizationControl_t *control, unsigned int reset);
DEPRECATED(13.0) hgmlReturn_t hgmlDeviceSetMemClkVfOffset(hgmlDevice_t device, int offset);
DEPRECATED(13.0) hgmlReturn_t hgmlVgpuInstanceGetLicenseStatus(hgmlVgpuInstance_t vgpuInstance, unsigned int *licensed);

#ifdef __cplusplus
}
#endif

#endif
