#ifndef __HGML_H__
#define __HGML_H__

#ifdef __cplusplus
extern "C" {
#endif

/**
 * HGML API versioning support
 */
#define HGML_API_VERSION            12
#define HGML_API_VERSION_STR       "12"

#define HGML_STRUCT_VERSION(data, ver) (unsigned int)(sizeof(hgml ## data ## _v ## ver ## _t) | \
                                                              (ver << 24U))

/***************************************************************************************************/
/** @defgroup hgmlDeviceStructs Device Structs
 *  @{
 */
/***************************************************************************************************/

/**
 * Special constant that some fields take when they are not available.
 * Used when only part of the struct is not available.
 *
 * Each structure explicitly states when to check for this value.
 */
#define HGML_VALUE_NOT_AVAILABLE (-1)

typedef struct hgmlDevice_st* hgmlDevice_t;

/**
 * Buffer size guaranteed to be large enough for pci bus id
 */
#define HGML_DEVICE_PCI_BUS_ID_BUFFER_SIZE      (32 - 4) //!< 4 byte reserved for globalPpuId

/**
 * Buffer size guaranteed to be large enough for pci bus id for ::busIdLegacy
 */
#define HGML_DEVICE_PCI_BUS_ID_BUFFER_V2_SIZE   16

/**
 * PCI information about a GPU device.
 */
typedef struct hgmlPciInfo_st
{
    char busIdLegacy[HGML_DEVICE_PCI_BUS_ID_BUFFER_V2_SIZE]; //!< The legacy tuple domain:bus:device.function PCI identifier (&amp; NULL terminator)
    unsigned int domain;             //!< The PCI domain on which the device's bus resides, 0 to 0xffffffff
    unsigned int bus;                //!< The bus on which the device resides, 0 to 0xff
    unsigned int device;             //!< The device's id on the bus, 0 to 31
    unsigned int pciDeviceId;        //!< The combined 16-bit device id and 16-bit vendor id

    // Added in HGML 2.285 API
    unsigned int pciSubSystemId;     //!< The 32-bit Sub System Device ID

    char busId[HGML_DEVICE_PCI_BUS_ID_BUFFER_SIZE]; //!< The tuple domain:bus:device.function PCI identifier (&amp; NULL terminator)

    unsigned int globalPpuId;
} hgmlPciInfo_t;

/**
 * PCI format string for ::busIdLegacy
 */
#define HGML_DEVICE_PCI_BUS_ID_LEGACY_FMT           "%04X:%02X:%02X.%01X"

/**
 * PCI format string for ::busId
 */
#define HGML_DEVICE_PCI_BUS_ID_FMT                  "%08X:%02X:%02X.%01X"

/**
 * Utility macro for filling the pci bus id format from a hgmlPciInfo_t
 */
#define HGML_DEVICE_PCI_BUS_ID_FMT_ARGS(pciInfo)    (pciInfo)->domain, \
                                                    (pciInfo)->bus,    \
                                                    (pciInfo)->device

/**
 * Detailed ECC error counts for a device.
 *
 * @deprecated  Different GPU families can have different memory error counters
 *              See \ref hgmlDeviceGetMemoryErrorCounter
 */
typedef struct hgmlEccErrorCounts_st
{
    unsigned long long l1Cache;      //!< L1 cache errors
    unsigned long long l2Cache;      //!< L2 cache errors
    unsigned long long deviceMemory; //!< Device memory errors
    unsigned long long registerFile; //!< Register file errors
} hgmlEccErrorCounts_t;

/**
 * Utilization information for a device.
 * Each sample period may be between 1 second and 1/6 second, depending on the product being queried.
 */
typedef struct hgmlUtilization_st
{
    unsigned int gpu;                //!< Percent of time over the past sample period during which one or more kernels was executing on the GPU
    unsigned int memory;             //!< Percent of time over the past sample period during which global (device) memory was being read or written
} hgmlUtilization_t;

/**
 * Memory allocation information for a device (v1).
 * The total amount is equal to the sum of the amounts of free and used memory.
 */
typedef struct hgmlMemory_st
{
    unsigned long long total;        //!< Total physical device memory (in bytes)
    unsigned long long free;         //!< Unallocated device memory (in bytes)
    unsigned long long used;         //!< Sum of Reserved and Allocated device memory (in bytes).
                                     //!< Note that the driver/GPU always sets aside a small amount of memory for bookkeeping
} hgmlMemory_t;

/**
 * Memory allocation information for a device (v2).
 *
 * Version 2 adds versioning for the struct and the amount of system-reserved memory as an output.
 */
typedef struct hgmlMemory_v2_st
{
    unsigned int version;            //!< Structure format version (must be hgmlMemory_v2)
    unsigned long long total;        //!< Total physical device memory (in bytes)
    unsigned long long reserved;     //!< Device memory (in bytes) reserved for system use (driver or firmware)
    unsigned long long free;         //!< Unallocated device memory (in bytes)
    unsigned long long used;         //!< Allocated device memory (in bytes).
} hgmlMemory_v2_t;

#define hgmlMemory_v2 HGML_STRUCT_VERSION(Memory, 2)

/**
 * BAR1 Memory allocation Information for a device
 */
typedef struct hgmlBAR1Memory_st
{
    unsigned long long bar1Total;    //!< Total BAR1 Memory (in bytes)
    unsigned long long bar1Free;     //!< Unallocated BAR1 Memory (in bytes)
    unsigned long long bar1Used;     //!< Allocated Used Memory (in bytes)
}hgmlBAR1Memory_t;

/**
 * Information about running compute processes on the GPU, legacy version
 * for older versions of the API.
 */
typedef struct hgmlProcessInfo_v1_st
{
    unsigned int        pid;                //!< Process ID
    unsigned long long  usedGpuMemory;      //!< Amount of used GPU memory in bytes.
                                            //! Under WDDM, \ref HGML_VALUE_NOT_AVAILABLE is always reported
                                            //! because Windows KMD manages all the memory and not the alixpu driver
} hgmlProcessInfo_v1_t;

/**
 * Information about running compute processes on the GPU
 */
typedef struct hgmlProcessInfo_v2_st
{
    unsigned int        pid;                //!< Process ID
    unsigned long long  usedGpuMemory;      //!< Amount of used GPU memory in bytes.
                                            //! Under WDDM, \ref HGML_VALUE_NOT_AVAILABLE is always reported
                                            //! because Windows KMD manages all the memory and not the alixpu driver
    unsigned int        gpuInstanceId;      //!< If MIG is enabled, stores a valid GPU instance ID. gpuInstanceId is set to
                                            //  0xFFFFFFFF otherwise.
    unsigned int        computeInstanceId;  //!< If MIG is enabled, stores a valid compute instance ID. computeInstanceId is set to
                                            //  0xFFFFFFFF otherwise.
} hgmlProcessInfo_v2_t, hgmlProcessInfo_t;

/**
 * Information about running process on the GPU with protected memory
 */
typedef struct
{
    unsigned int        pid;                      //!< Process ID
    unsigned long long  usedGpuMemory;            //!< Amount of used GPU memory in bytes.
                                                  //! Under WDDM, \ref HGML_VALUE_NOT_AVAILABLE is always reported
                                                  //! because Windows KMD manages all the memory and not the alixpu driver
    unsigned int        gpuInstanceId;            //!< If MIG is enabled, stores a valid GPU instance ID. gpuInstanceId is
                                                  //  set to 0xFFFFFFFF otherwise.
    unsigned int        computeInstanceId;        //!< If MIG is enabled, stores a valid compute instance ID. computeInstanceId
                                                  //  is set to 0xFFFFFFFF otherwise.
    unsigned long long  usedGpuCcProtectedMemory; //!< Amount of used GPU conf compute protected memory in bytes.
} hgmlProcessDetail_v1_t;

/**
 * Information about all running processes on the GPU for the given mode
 */
typedef struct
{
    unsigned int           version;             //!< Struct version, MUST be hgmlProcessDetailList_v1
    unsigned int           mode;                //!< Process mode(Compute/Graphics/MPSCompute)
    unsigned int           numProcArrayEntries; //!< Number of process entries in procArray
    hgmlProcessDetail_v1_t *procArray;          //!< Process array
} hgmlProcessDetailList_v1_t;

typedef hgmlProcessDetailList_v1_t hgmlProcessDetailList_t;

/**
 * hgmlProcessDetailList version
 */
#define hgmlProcessDetailList_v1 HGML_STRUCT_VERSION(ProcessDetailList, 1)

typedef struct hgmlDeviceAttributes_st
{
    unsigned int multiprocessorCount;       //!< Streaming Multiprocessor count
    unsigned int sharedCopyEngineCount;     //!< Shared Copy Engine count
    unsigned int sharedDecoderCount;        //!< Shared Decoder Engine count
    unsigned int sharedEncoderCount;        //!< Shared Encoder Engine count
    unsigned int sharedJpegCount;           //!< Shared JPEG Engine count
    unsigned int sharedOfaCount;            //!< Shared OFA Engine count
    unsigned int gpuInstanceSliceCount;     //!< GPU instance slice count
    unsigned int computeInstanceSliceCount; //!< Compute instance slice count
    unsigned long long memorySizeMB;        //!< Device memory size (in MiB)
} hgmlDeviceAttributes_t;

/**
 * C2C Mode information for a device
 */
typedef struct
{
    unsigned int isC2cEnabled;
} hgmlC2cModeInfo_v1_t;

#define hgmlC2cModeInfo_v1 HGML_STRUCT_VERSION(C2cModeInfo, 1)

/**
 * Possible values that classify the remap availability for each bank. The max
 * field will contain the number of banks that have maximum remap availability
 * (all reserved rows are available). None means that there are no reserved
 * rows available.
 */
typedef struct hgmlRowRemapperHistogramValues_st
{
    unsigned int max;
    unsigned int high;
    unsigned int partial;
    unsigned int low;
    unsigned int none;
} hgmlRowRemapperHistogramValues_t;

/**
 * Enum to represent type of bridge chip
 */
typedef enum hgmlBridgeChipType_enum
{
    HGML_BRIDGE_CHIP_PLX = 0,
    HGML_BRIDGE_CHIP_BRO4 = 1
}hgmlBridgeChipType_t;

/**
 * Maximum number of ICNLink links supported
 */
#define HGML_ICNLINK_MAX_LINKS 8

/**
 * Enum to represent the ICNLink utilization counter packet units
 */
typedef enum hgmlIcnLinkUtilizationCountUnits_enum
{
    HGML_ICNLINK_COUNTER_UNIT_CYCLES =  0,     // count by cycles
    HGML_ICNLINK_COUNTER_UNIT_PACKETS = 1,     // count by packets
    HGML_ICNLINK_COUNTER_UNIT_BYTES   = 2,     // count by bytes
    HGML_ICNLINK_COUNTER_UNIT_RESERVED = 3,    // count reserved for internal use
    // this must be last
    HGML_ICNLINK_COUNTER_UNIT_COUNT
} hgmlIcnLinkUtilizationCountUnits_t;

/**
 * Enum to represent the ICNLink utilization counter packet types to count
 *  ** this is ONLY applicable with the units as packets or bytes
 *  ** as specified in \a hgmlIcnLinkUtilizationCountUnits_t
 *  ** all packet filter descriptions are target GPU centric
 *  ** these can be "OR'd" together
 */
typedef enum hgmlIcnLinkUtilizationCountPktTypes_enum
{
    HGML_ICNLINK_COUNTER_PKTFILTER_NOP        = 0x1,     // no operation packets
    HGML_ICNLINK_COUNTER_PKTFILTER_READ       = 0x2,     // read packets
    HGML_ICNLINK_COUNTER_PKTFILTER_WRITE      = 0x4,     // write packets
    HGML_ICNLINK_COUNTER_PKTFILTER_RATOM      = 0x8,     // reduction atomic requests
    HGML_ICNLINK_COUNTER_PKTFILTER_NRATOM     = 0x10,    // non-reduction atomic requests
    HGML_ICNLINK_COUNTER_PKTFILTER_FLUSH      = 0x20,    // flush requests
    HGML_ICNLINK_COUNTER_PKTFILTER_RESPDATA   = 0x40,    // responses with data
    HGML_ICNLINK_COUNTER_PKTFILTER_RESPNODATA = 0x80,    // responses without data
    HGML_ICNLINK_COUNTER_PKTFILTER_ALL        = 0xFF     // all packets
} hgmlIcnLinkUtilizationCountPktTypes_t;

/**
 * Struct to define the ICNLINK counter controls
 */
typedef struct hgmlIcnLinkUtilizationControl_st
{
    hgmlIcnLinkUtilizationCountUnits_t units;
    hgmlIcnLinkUtilizationCountPktTypes_t pktfilter;
} hgmlIcnLinkUtilizationControl_t;

/**
 * Enum to represent ICNLink queryable capabilities
 */
typedef enum hgmlIcnLinkCapability_enum
{
    HGML_ICNLINK_CAP_P2P_SUPPORTED = 0,     // P2P over ICNLink is supported
    HGML_ICNLINK_CAP_SYSMEM_ACCESS = 1,     // Access to system memory is supported
    HGML_ICNLINK_CAP_P2P_ATOMICS   = 2,     // P2P atomics are supported
    HGML_ICNLINK_CAP_SYSMEM_ATOMICS= 3,     // System memory atomics are supported
    HGML_ICNLINK_CAP_SLI_BRIDGE    = 4,     // SLI is supported over this link
    HGML_ICNLINK_CAP_VALID         = 5,     // Link is supported on this device
    // should be last
    HGML_ICNLINK_CAP_COUNT
} hgmlIcnLinkCapability_t;

/**
 * Enum to represent ICNLink queryable error counters
 */
typedef enum hgmlIcnLinkErrorCounter_enum
{
    HGML_ICNLINK_ERROR_DL_REPLAY   = 0,     // Data link transmit replay error counter
    HGML_ICNLINK_ERROR_DL_RECOVERY = 1,     // Data link transmit recovery error counter
    HGML_ICNLINK_ERROR_DL_CRC_FLIT = 2,     // Data link receive flow control digit CRC error counter
    HGML_ICNLINK_ERROR_DL_CRC_DATA = 3,     // Data link receive data CRC error counter
    HGML_ICNLINK_ERROR_DL_ECC_DATA = 4,     // Data link receive data ECC error counter

    // this must be last
    HGML_ICNLINK_ERROR_COUNT
} hgmlIcnLinkErrorCounter_t;

/**
 * Enum to represent ICNLink's remote device type
 */
typedef enum hgmlIntIcnLinkDeviceType_enum
{
    HGML_ICNLINK_DEVICE_TYPE_PPU     = 0x00,
    HGML_ICNLINK_DEVICE_TYPE_SWITCH  = 0x02,
    HGML_ICNLINK_DEVICE_TYPE_UNKNOWN = 0xFF
} hgmlIntIcnLinkDeviceType_t;

/**
 * Represents level relationships within a system between two GPUs
 * The enums are spaced to allow for future relationships
 */
typedef enum hgmlGpuLevel_enum
{
    HGML_TOPOLOGY_INTERNAL           = 0,
    HGML_TOPOLOGY_SINGLE             = 10, // all devices that only need traverse a single PCIe switch
    HGML_TOPOLOGY_MULTIPLE           = 20, // all devices that need not traverse a host bridge
    HGML_TOPOLOGY_HOSTBRIDGE         = 30, // all devices that are connected to the same host bridge
    HGML_TOPOLOGY_NODE               = 40, // all devices that are connected to the same NUMA node but possibly multiple host bridges
    HGML_TOPOLOGY_SYSTEM             = 50  // all devices in the system

    // there is purposefully no COUNT here because of the need for spacing above
} hgmlGpuTopologyLevel_t;

/* Compatibility for CPU->NODE renaming */
#define HGML_TOPOLOGY_CPU HGML_TOPOLOGY_NODE


/* P2P Capability Index Status*/
typedef enum hgmlGpuP2PStatus_enum
{
    HGML_P2P_STATUS_OK     = 0,
    HGML_P2P_STATUS_CHIPSET_NOT_SUPPORTED,
    HGML_P2P_STATUS_GPU_NOT_SUPPORTED,
    HGML_P2P_STATUS_IOH_TOPOLOGY_NOT_SUPPORTED,
    HGML_P2P_STATUS_DISABLED_BY_REGKEY,
    HGML_P2P_STATUS_NOT_SUPPORTED,
    HGML_P2P_STATUS_UNKNOWN

} hgmlGpuP2PStatus_t;

/* P2P Capability Index*/
typedef enum hgmlGpuP2PCapsIndex_enum
{
    HGML_P2P_CAPS_INDEX_READ = 0,
    HGML_P2P_CAPS_INDEX_WRITE,
    HGML_P2P_CAPS_INDEX_ICNLINK,
    HGML_P2P_CAPS_INDEX_ATOMICS,
    HGML_P2P_CAPS_INDEX_PROP,
    HGML_P2P_CAPS_INDEX_UNKNOWN
}hgmlGpuP2PCapsIndex_t;

/**
 * Maximum limit on Physical Bridges per Board
 */
#define HGML_MAX_PHYSICAL_BRIDGE                         (128)

/**
 * Information about the Bridge Chip Firmware
 */
typedef struct hgmlBridgeChipInfo_st
{
    hgmlBridgeChipType_t type;                  //!< Type of Bridge Chip
    unsigned int fwVersion;                     //!< Firmware Version. 0=Version is unavailable
}hgmlBridgeChipInfo_t;

/**
 * This structure stores the complete Hierarchy of the Bridge Chip within the board. The immediate
 * bridge is stored at index 0 of bridgeInfoList, parent to immediate bridge is at index 1 and so forth.
 */
typedef struct hgmlBridgeChipHierarchy_st
{
    unsigned char  bridgeCount;                 //!< Number of Bridge Chips on the Board
    hgmlBridgeChipInfo_t bridgeChipInfo[HGML_MAX_PHYSICAL_BRIDGE]; //!< Hierarchy of Bridge Chips on the board
}hgmlBridgeChipHierarchy_t;

/**
 *  Represents Type of Sampling Event
 */
typedef enum hgmlSamplingType_enum
{
    HGML_TOTAL_POWER_SAMPLES        = 0, //!< To represent total power drawn by GPU
    HGML_GPU_UTILIZATION_SAMPLES    = 1, //!< To represent percent of time during which one or more kernels was executing on the GPU
    HGML_MEMORY_UTILIZATION_SAMPLES = 2, //!< To represent percent of time during which global (device) memory was being read or written
    HGML_ENC_UTILIZATION_SAMPLES    = 3, //!< To represent percent of time during which HGENC remains busy
    HGML_DEC_UTILIZATION_SAMPLES    = 4, //!< To represent percent of time during which HGDEC remains busy
    HGML_PROCESSOR_CLK_SAMPLES      = 5, //!< To represent processor clock samples
    HGML_MEMORY_CLK_SAMPLES         = 6, //!< To represent memory clock samples
    HGML_MODULE_POWER_SAMPLES       = 7, //!< To represent module power samples for total module

    // Keep this last
    HGML_SAMPLINGTYPE_COUNT
} hgmlSamplingType_t;

/**
 * Represents the queryable PCIe utilization counters
 */
typedef enum hgmlPcieUtilCounter_enum
{
    HGML_PCIE_UTIL_TX_BYTES             = 0, // 1KB granularity
    HGML_PCIE_UTIL_RX_BYTES             = 1, // 1KB granularity

    // Keep this last
    HGML_PCIE_UTIL_COUNT
} hgmlPcieUtilCounter_t;

/**
 * Represents the type for sample value returned
 */
typedef enum hgmlValueType_enum
{
    HGML_VALUE_TYPE_DOUBLE = 0,
    HGML_VALUE_TYPE_UNSIGNED_INT = 1,
    HGML_VALUE_TYPE_UNSIGNED_LONG = 2,
    HGML_VALUE_TYPE_UNSIGNED_LONG_LONG = 3,
    HGML_VALUE_TYPE_SIGNED_LONG_LONG = 4,
    HGML_VALUE_TYPE_SIGNED_INT = 5,

    // Keep this last
    HGML_VALUE_TYPE_COUNT
}hgmlValueType_t;


/**
 * Union to represent different types of Value
 */
typedef union hgmlValue_st
{
    double dVal;                    //!< If the value is double
    int siVal;                      //!< If the value is signed int
    unsigned int uiVal;             //!< If the value is unsigned int
    unsigned long ulVal;            //!< If the value is unsigned long
    unsigned long long ullVal;      //!< If the value is unsigned long long
    signed long long sllVal;        //!< If the value is signed long long
}hgmlValue_t;

/**
 * Information for Sample
 */
typedef struct hgmlSample_st
{
    unsigned long long timeStamp;       //!< CPU Timestamp in microseconds
    hgmlValue_t sampleValue;            //!< Sample Value
}hgmlSample_t;

/**
 * Represents type of perf policy for which violation times can be queried
 */
typedef enum hgmlPerfPolicyType_enum
{
    HGML_PERF_POLICY_POWER = 0,              //!< How long did power violations cause the GPU to be below application clocks
    HGML_PERF_POLICY_THERMAL = 1,            //!< How long did thermal violations cause the GPU to be below application clocks
    HGML_PERF_POLICY_SYNC_BOOST = 2,         //!< How long did sync boost cause the GPU to be below application clocks
    HGML_PERF_POLICY_BOARD_LIMIT = 3,        //!< How long did the board limit cause the GPU to be below application clocks
    HGML_PERF_POLICY_LOW_UTILIZATION = 4,    //!< How long did low utilization cause the GPU to be below application clocks
    HGML_PERF_POLICY_RELIABILITY = 5,        //!< How long did the board reliability limit cause the GPU to be below application clocks

    HGML_PERF_POLICY_TOTAL_APP_CLOCKS = 10,  //!< Total time the GPU was held below application clocks by any limiter (0 - 5 above)
    HGML_PERF_POLICY_TOTAL_BASE_CLOCKS = 11, //!< Total time the GPU was held below base clocks

    // Keep this last
    HGML_PERF_POLICY_COUNT
}hgmlPerfPolicyType_t;

/**
 * Struct to hold perf policy violation status data
 */
typedef struct hgmlViolationTime_st
{
    unsigned long long referenceTime;  //!< referenceTime represents CPU timestamp in microseconds
    unsigned long long violationTime;  //!< violationTime in Nanoseconds
} hgmlViolationTime_t;

#define HGML_MAX_THERMAL_SENSORS_PER_GPU  3

typedef enum
{
    HGML_THERMAL_TARGET_NONE          = 0,
    HGML_THERMAL_TARGET_UNKNOWN       = -1,
} hgmlThermalTarget_t;

typedef enum
{
    HGML_THERMAL_CONTROLLER_NONE = 0,
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

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlDeviceEnumvs Device Enums
 *  @{
 */
/***************************************************************************************************/

/**
 * Generic enable/disable enum.
 */
typedef enum hgmlEnableState_enum
{
    HGML_FEATURE_DISABLED    = 0,     //!< Feature disabled
    HGML_FEATURE_ENABLED     = 1      //!< Feature enabled
} hgmlEnableState_t;

//! Generic flag used to specify the default behavior of some functions. See description of particular functions for details.
#define hgmlFlagDefault     0x00
//! Generic flag used to force some behavior. See description of particular functions for details.
#define hgmlFlagForce       0x01

/**
 *  * The Brand of the GPU
 *   */
typedef enum hgmlBrandType_enum
{
    HGML_BRAND_UNKNOWN     = 0,
    HGML_BRAND_BEETHOVEN   = 14,
    // Keep this last
    HGML_BRAND_COUNT
} hgmlBrandType_t;

/**
 * Temperature thresholds.
 */
typedef enum hgmlTemperatureThresholds_enum
{
    HGML_TEMPERATURE_THRESHOLD_SHUTDOWN      = 0, // Temperature at which the GPU will
                                                  // shut down for HW protection
    HGML_TEMPERATURE_THRESHOLD_SLOWDOWN      = 1, // Temperature at which the GPU will
                                                  // begin HW slowdown
    HGML_TEMPERATURE_THRESHOLD_MEM_MAX       = 2, // Memory Temperature at which the GPU will
                                                  // begin SW slowdown
    HGML_TEMPERATURE_THRESHOLD_GPU_MAX       = 3, // GPU Temperature at which the GPU
                                                  // can be throttled below base clock
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_MIN  = 4, // Minimum GPU Temperature that can be
                                                  // set as acoustic threshold
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_CURR = 5, // Current temperature that is set as
                                                  // acoustic threshold.
    HGML_TEMPERATURE_THRESHOLD_ACOUSTIC_MAX  = 6, // Maximum GPU temperature that can be
                                                  // set as acoustic threshold.
    // Keep this last
    HGML_TEMPERATURE_THRESHOLD_COUNT
} hgmlTemperatureThresholds_t;

/**
 * Temperature sensors.
 */
typedef enum hgmlTemperatureSensors_enum
{
    HGML_TEMPERATURE_GPU      = 0,    //!< Temperature sensor for the GPU die

    // Keep this last
    HGML_TEMPERATURE_COUNT
} hgmlTemperatureSensors_t;

/**
 * Compute mode.
 *
 * HGML_COMPUTEMODE_EXCLUSIVE_PROCESS was added in HGGC 4.0.
 * Earlier HGGC versions supported a single exclusive mode,
 * which is equivalent to HGML_COMPUTEMODE_EXCLUSIVE_THREAD in HGGC 4.0 and beyond.
 */
typedef enum hgmlComputeMode_enum
{
    HGML_COMPUTEMODE_DEFAULT           = 0,  //!< Default compute mode -- multiple contexts per device
    HGML_COMPUTEMODE_EXCLUSIVE_THREAD  = 1,  //!< Support Removed
    HGML_COMPUTEMODE_PROHIBITED        = 2,  //!< Compute-prohibited mode -- no contexts per device
    HGML_COMPUTEMODE_EXCLUSIVE_PROCESS = 3,  //!< Compute-exclusive-process mode -- only one context per device, usable from multiple threads at a time

    // Keep this last
    HGML_COMPUTEMODE_COUNT
} hgmlComputeMode_t;

/**
 * Max Clock Monitors available
 */
#define MAX_CLK_DOMAINS                 32

/**
 * Clock Monitor error types
 */
typedef struct hgmlClkMonFaultInfo_struct {
    /**
     * The Domain which faulted
     */
    unsigned int   clkApiDomain;

    /**
     * Faults Information
     */
    unsigned int   clkDomainFaultMask;
} hgmlClkMonFaultInfo_t;

/**
 * Clock Monitor Status
 */
typedef struct hgmlClkMonStatus_status {
    /**
     * Fault status Indicator
     */
    unsigned int  bGlobalStatus;

    /**
     * Total faulted domain numbers
     */
    unsigned int   clkMonListSize;

    /**
     * The fault Information structure
     */
    hgmlClkMonFaultInfo_t clkMonList[MAX_CLK_DOMAINS];
} hgmlClkMonStatus_t;

/**
 * ECC bit types.
 *
 * @deprecated See \ref hgmlMemoryErrorType_t for a more flexible type
 */
#define hgmlEccBitType_t hgmlMemoryErrorType_t

/**
 * Single bit ECC errors
 *
 * @deprecated Mapped to \ref HGML_MEMORY_ERROR_TYPE_CORRECTED
 */
#define HGML_SINGLE_BIT_ECC HGML_MEMORY_ERROR_TYPE_CORRECTED

/**
 * Double bit ECC errors
 *
 * @deprecated Mapped to \ref HGML_MEMORY_ERROR_TYPE_UNCORRECTED
 */
#define HGML_DOUBLE_BIT_ECC HGML_MEMORY_ERROR_TYPE_UNCORRECTED

/**
 * Memory error types
 */
typedef enum hgmlMemoryErrorType_enum
{
    /**
     * A memory error that was corrected
     *
     * For ECC errors, these are single bit errors
     * For Texture memory, these are errors fixed by resend
     */
    HGML_MEMORY_ERROR_TYPE_CORRECTED = 0,
    /**
     * A memory error that was not corrected
     *
     * For ECC errors, these are double bit errors
     * For Texture memory, these are errors where the resend fails
     */
    HGML_MEMORY_ERROR_TYPE_UNCORRECTED = 1,


    // Keep this last
    HGML_MEMORY_ERROR_TYPE_COUNT //!< Count of memory error types

} hgmlMemoryErrorType_t;

/**
 * ECC counter types.
 *
 * Note: Volatile counts are reset each time the driver loads. On Windows this is once per boot. On Linux this can be more frequent.
 *       On Linux the driver unloads when no active clients exist. If persistence mode is enabled or there is always a driver
 *       client active (e.g. X11), then Linux also sees per-boot behavior. If not, volatile counts are reset each time a compute app
 *       is run.
 */
typedef enum hgmlEccCounterType_enum
{
    HGML_VOLATILE_ECC      = 0,      //!< Volatile counts are reset each time the driver loads.
    HGML_AGGREGATE_ECC     = 1,      //!< Aggregate counts persist across reboots (i.e. for the lifetime of the device)

    // Keep this last
    HGML_ECC_COUNTER_TYPE_COUNT      //!< Count of memory counter types
} hgmlEccCounterType_t;

/**
 * Clock types.
 *
 * All speeds are in Mhz.
 */
typedef enum hgmlClockType_enum
{
    HGML_CLOCK_GRAPHICS  = 0,        //!< Graphics clock domain
    HGML_CLOCK_SM        = 1,        //!< SM clock domain
    HGML_CLOCK_MEM       = 2,        //!< Memory clock domain
    HGML_CLOCK_VIDEO     = 3,        //!< Video encoder/decoder clock domain

    // Keep this last
    HGML_CLOCK_COUNT //!< Count of clock types
} hgmlClockType_t;

/**
 * Clock Ids.  These are used in combination with hgmlClockType_t
 * to specify a single clock value.
 */
typedef enum hgmlClockId_enum
{
    HGML_CLOCK_ID_CURRENT            = 0,   //!< Current actual clock value
    HGML_CLOCK_ID_APP_CLOCK_TARGET   = 1,   //!< Target application clock
    HGML_CLOCK_ID_APP_CLOCK_DEFAULT  = 2,   //!< Default application clock target
    HGML_CLOCK_ID_CUSTOMER_BOOST_MAX = 3,   //!< OEM-defined maximum clock rate

    //Keep this last
    HGML_CLOCK_ID_COUNT //!< Count of Clock Ids.
} hgmlClockId_t;

/**
 * Driver models.
 *
 * Windows only.
 */

typedef enum hgmlDriverModel_enum
{
    HGML_DRIVER_WDDM = 0,       //!< WDDM driver model -- GPU treated as a display device
    HGML_DRIVER_WDM  = 1        //!< WDM (TCC) model (recommended) -- GPU treated as a generic device
} hgmlDriverModel_t;

#define HGML_MAX_GPU_PERF_PSTATES 16

/**
 * Allowed PStates.
 */
typedef enum hgmlPStates_enum
{
    HGML_PSTATE_0               = 0,       //!< Performance state 0 -- Maximum Performance
    HGML_PSTATE_1               = 1,       //!< Performance state 1
    HGML_PSTATE_2               = 2,       //!< Performance state 2
    HGML_PSTATE_3               = 3,       //!< Performance state 3
    HGML_PSTATE_4               = 4,       //!< Performance state 4
    HGML_PSTATE_5               = 5,       //!< Performance state 5
    HGML_PSTATE_6               = 6,       //!< Performance state 6
    HGML_PSTATE_7               = 7,       //!< Performance state 7
    HGML_PSTATE_8               = 8,       //!< Performance state 8
    HGML_PSTATE_9               = 9,       //!< Performance state 9
    HGML_PSTATE_10              = 10,      //!< Performance state 10
    HGML_PSTATE_11              = 11,      //!< Performance state 11
    HGML_PSTATE_12              = 12,      //!< Performance state 12
    HGML_PSTATE_13              = 13,      //!< Performance state 13
    HGML_PSTATE_14              = 14,      //!< Performance state 14
    HGML_PSTATE_15              = 15,      //!< Performance state 15 -- Minimum Performance
    HGML_PSTATE_UNKNOWN         = 32       //!< Unknown performance state
} hgmlPstates_t;

/**
 * GPU Operation Mode
 *
 * GOM allows to reduce power usage and optimize GPU throughput by disabling GPU features.
 *
 * Each GOM is designed to meet specific user needs.
 */
typedef enum hgmlGom_enum
{
    HGML_GOM_ALL_ON                    = 0, //!< Everything is enabled and running at full speed

    HGML_GOM_COMPUTE                   = 1, //!< Designed for running only compute tasks. Graphics operations
                                            //!< are not allowed

    HGML_GOM_LOW_DP                    = 2  //!< Designed for running graphics applications that don't require
                                            //!< high bandwidth double precision
} hgmlGpuOperationMode_t;

/**
 * Available infoROM objects.
 */
typedef enum hgmlInforomObject_enum
{
    HGML_INFOROM_OEM            = 0,       //!< An object defined by OEM
    HGML_INFOROM_ECC            = 1,       //!< The ECC object determining the level of ECC support
    HGML_INFOROM_POWER          = 2,       //!< The power management object

    // Keep this last
    HGML_INFOROM_COUNT                     //!< This counts the number of infoROM objects the driver knows about
} hgmlInforomObject_t;

/**
 * Return values for HGML API calls.
 */
typedef enum hgmlReturn_enum
{
    // cppcheck-suppress *
    HGML_SUCCESS = 0,                          //!< The operation was successful
    HGML_ERROR_UNINITIALIZED = 1,              //!< HGML was not first initialized with hgmlInit()
    HGML_ERROR_INVALID_ARGUMENT = 2,           //!< A supplied argument is invalid
    HGML_ERROR_NOT_SUPPORTED = 3,              //!< The requested operation is not available on target device
    HGML_ERROR_NO_PERMISSION = 4,              //!< The current user does not have permission for operation
    HGML_ERROR_ALREADY_INITIALIZED = 5,        //!< Deprecated: Multiple initializations are now allowed through ref counting
    HGML_ERROR_NOT_FOUND = 6,                  //!< A query to find an object was unsuccessful
    HGML_ERROR_INSUFFICIENT_SIZE = 7,          //!< An input argument is not large enough
    HGML_ERROR_INSUFFICIENT_POWER = 8,         //!< A device's external power cables are not properly attached
    HGML_ERROR_DRIVER_NOT_LOADED = 9,          //!< Driver is not loaded
    HGML_ERROR_TIMEOUT = 10,                   //!< User provided timeout passed
    HGML_ERROR_IRQ_ISSUE = 11,                 //!< Kernel detected an interrupt issue with a GPU
    HGML_ERROR_LIBRARY_NOT_FOUND = 12,         //!< HGML Shared Library couldn't be found or loaded
    HGML_ERROR_FUNCTION_NOT_FOUND = 13,        //!< Local version of HGML doesn't implement this function
    HGML_ERROR_CORRUPTED_INFOROM = 14,         //!< infoROM is corrupted
    HGML_ERROR_GPU_IS_LOST = 15,               //!< The GPU has fallen off the bus or has otherwise become inaccessible
    HGML_ERROR_RESET_REQUIRED = 16,            //!< The GPU requires a reset before it can be used again
    HGML_ERROR_OPERATING_SYSTEM = 17,          //!< The GPU control device has been blocked by the operating system/cgroups
    HGML_ERROR_LIB_RM_VERSION_MISMATCH = 18,   //!< RM detects a driver/library version mismatch
    HGML_ERROR_IN_USE = 19,                    //!< An operation cannot be performed because the GPU is currently in use
    HGML_ERROR_MEMORY = 20,                    //!< Insufficient memory
    HGML_ERROR_NO_DATA = 21,                   //!< No data
    HGML_ERROR_VGPU_ECC_NOT_SUPPORTED = 22,    //!< The requested vgpu operation is not available on target device, becasue ECC is enabled
    HGML_ERROR_INSUFFICIENT_RESOURCES = 23,    //!< Ran out of critical resources, other than memory
    HGML_ERROR_FREQ_NOT_SUPPORTED = 24,        //!< Ran out of critical resources, other than memory
    HGML_ERROR_ARGUMENT_VERSION_MISMATCH = 25, //!< The provided version is invalid/unsupported
    HGML_ERROR_DEPRECATED  = 26,               //!< The requested functionality has been deprecated
    HGML_ERROR_NOT_READY = 27,                 //!< The system is not ready for the request
    HGML_ERROR_GPU_NOT_FOUND = 28,             //!< No GPUs were found
    HGML_ERROR_UNKNOWN = 999                   //!< An internal driver error occurred
} hgmlReturn_t;

/**
 * See \ref hgmlDeviceGetMemoryErrorCounter
 */
typedef enum hgmlMemoryLocation_enum
{
    HGML_MEMORY_LOCATION_L1_CACHE        = 0,    //!< GPU L1 Cache
    HGML_MEMORY_LOCATION_L2_CACHE        = 1,    //!< GPU L2 Cache
    HGML_MEMORY_LOCATION_DRAM            = 2,    //!< Turing+ DRAM
    HGML_MEMORY_LOCATION_DEVICE_MEMORY   = 2,    //!< GPU Device Memory
    HGML_MEMORY_LOCATION_REGISTER_FILE   = 3,    //!< GPU Register File
    HGML_MEMORY_LOCATION_TEXTURE_MEMORY  = 4,    //!< GPU Texture Memory
    HGML_MEMORY_LOCATION_TEXTURE_SHM     = 5,    //!< Shared memory
    HGML_MEMORY_LOCATION_CBU             = 6,    //!< CBU
    HGML_MEMORY_LOCATION_SRAM            = 7,    //!< Turing+ SRAM
    // Keep this last
    HGML_MEMORY_LOCATION_COUNT              //!< This counts the number of memory locations the driver knows about
} hgmlMemoryLocation_t;

/**
 * Causes for page retirement
 */
typedef enum hgmlPageRetirementCause_enum
{
    HGML_PAGE_RETIREMENT_CAUSE_MULTIPLE_SINGLE_BIT_ECC_ERRORS = 0, //!< Page was retired due to multiple single bit ECC error
    HGML_PAGE_RETIREMENT_CAUSE_DOUBLE_BIT_ECC_ERROR = 1,           //!< Page was retired due to double bit ECC error

    // Keep this last
    HGML_PAGE_RETIREMENT_CAUSE_COUNT
} hgmlPageRetirementCause_t;

/**
 * API types that allow changes to default permission restrictions
 */
typedef enum hgmlRestrictedAPI_enum
{
    HGML_RESTRICTED_API_SET_APPLICATION_CLOCKS = 0,   //!< APIs that change application clocks, see hgmlDeviceSetApplicationsClocks
                                                      //!< and see hgmlDeviceResetApplicationsClocks
    HGML_RESTRICTED_API_SET_AUTO_BOOSTED_CLOCKS = 1,  //!< APIs that enable/disable Auto Boosted clocks
                                                      //!< see hgmlDeviceSetAutoBoostedClocksEnabled
    // Keep this last
    HGML_RESTRICTED_API_COUNT
} hgmlRestrictedAPI_t;

/** @} */

/***************************************************************************************************/
/** @addtogroup virtualGPU
 *  @{
 */
/***************************************************************************************************/
/** @defgroup hgmlVirtualGpuEnums vGPU Enums
 *  @{
 */
/***************************************************************************************************/

/*!
 * GPU virtualization mode types.
 */
typedef enum hgmlGpuVirtualizationMode {
    HGML_GPU_VIRTUALIZATION_MODE_NONE = 0,  //!< Represents Bare Metal GPU
    HGML_GPU_VIRTUALIZATION_MODE_PASSTHROUGH = 1,  //!< Device is associated with GPU-Passthorugh
    HGML_GPU_VIRTUALIZATION_MODE_VGPU = 2,  //!< Device is associated with vGPU inside virtual machine.
    HGML_GPU_VIRTUALIZATION_MODE_HOST_VGPU = 3,  //!< Device is associated with VGX hypervisor in vGPU mode
    HGML_GPU_VIRTUALIZATION_MODE_HOST_VSGA = 4   //!< Device is associated with VGX hypervisor in vSGA mode
} hgmlGpuVirtualizationMode_t;

/**
 * Host vGPU modes
 */
typedef enum hgmlHostVgpuMode_enum
{
    HGML_HOST_VGPU_MODE_NON_SRIOV    = 0,     //!< Non SR-IOV mode
    HGML_HOST_VGPU_MODE_SRIOV        = 1      //!< SR-IOV mode
} hgmlHostVgpuMode_t;

/*!
 * Types of VM identifiers
 */
typedef enum hgmlVgpuVmIdType {
    HGML_VGPU_VM_ID_DOMAIN_ID = 0, //!< VM ID represents DOMAIN ID
    HGML_VGPU_VM_ID_UUID = 1       //!< VM ID represents UUID
} hgmlVgpuVmIdType_t;

/**
 * vGPU GUEST info state.
 */
typedef enum hgmlVgpuGuestInfoState_enum
{
    HGML_VGPU_INSTANCE_GUEST_INFO_STATE_UNINITIALIZED = 0,  //!< Guest-dependent fields uninitialized
    HGML_VGPU_INSTANCE_GUEST_INFO_STATE_INITIALIZED   = 1   //!< Guest-dependent fields initialized
} hgmlVgpuGuestInfoState_t;

/**
 * vGPU software licensable features
 */
typedef enum {
    HGML_GRID_LICENSE_FEATURE_CODE_UNKNOWN      = 0,  //!< Unknown
    HGML_GRID_LICENSE_FEATURE_CODE_VGPU         = 1,  //!< Virtual GPU
} hgmlGridLicenseFeatureCode_t;

/**
 * vGPU queryable capabilities
 */
typedef enum hgmlVgpuCapability_enum
{
    HGML_VGPU_CAP_ICNLINK_P2P                   = 0,  //!< P2P over ICNLink is supported
    HGML_VGPU_CAP_GPUDIRECT                     = 1,  //!< GPUDirect capability is supported
    HGML_VGPU_CAP_MULTI_VGPU_EXCLUSIVE          = 2,  //!< vGPU profile cannot be mixed with other vGPU profiles in same VM
    HGML_VGPU_CAP_EXCLUSIVE_TYPE                = 3,  //!< vGPU profile cannot run on a GPU alongside other profiles of different type
    HGML_VGPU_CAP_EXCLUSIVE_SIZE                = 4,  //!< vGPU profile cannot run on a GPU alongside other profiles of different size
    // Keep this last
    HGML_VGPU_CAP_COUNT
} hgmlVgpuCapability_t;

/**
* vGPU driver queryable capabilities
*/
typedef enum hgmlVgpuDriverCapability_enum
{
    HGML_VGPU_DRIVER_CAP_HETEROGENEOUS_MULTI_VGPU = 0,      //!< Supports mixing of different vGPU profiles within one guest VM
    // Keep this last
    HGML_VGPU_DRIVER_CAP_COUNT
} hgmlVgpuDriverCapability_t;

/**
* Device vGPU queryable capabilities
*/
typedef enum hgmlDeviceVgpuCapability_enum
{
    HGML_DEVICE_VGPU_CAP_FRACTIONAL_MULTI_VGPU            = 0,    //!< Fractional vGPU profiles on this GPU can be used in multi-vGPU configurations
    HGML_DEVICE_VGPU_CAP_HETEROGENEOUS_TIMESLICE_PROFILES = 1,    //!< Supports concurrent execution of timesliced vGPU profiles of differing types
    HGML_DEVICE_VGPU_CAP_HETEROGENEOUS_TIMESLICE_SIZES    = 2,    //!< Supports concurrent execution of timesliced vGPU profiles of differing framebuffer sizes
    HGML_DEVICE_VGPU_CAP_READ_DEVICE_BUFFER_BW            = 3,    //!< GPU device's read_device_buffer expected bandwidth capacity in megabytes per second
    HGML_DEVICE_VGPU_CAP_WRITE_DEVICE_BUFFER_BW           = 4,    //!< GPU device's write_device_buffer expected bandwidth capacity in megabytes per second
    // Keep this last
    HGML_DEVICE_VGPU_CAP_COUNT
} hgmlDeviceVgpuCapability_t;

/** @} */

/***************************************************************************************************/

/** @defgroup hgmlVgpuConstants vGPU Constants
 *  @{
 */
/***************************************************************************************************/

/**
 * Buffer size guaranteed to be large enough for \ref hgmlVgpuTypeGetLicense
 */
#define HGML_GRID_LICENSE_BUFFER_SIZE       128

#define HGML_VGPU_NAME_BUFFER_SIZE          64

#define HGML_GRID_LICENSE_FEATURE_MAX_COUNT 3

#define INVALID_GPU_INSTANCE_PROFILE_ID     0xFFFFFFFF

#define INVALID_GPU_INSTANCE_ID             0xFFFFFFFF

/*!
 * Macros for vGPU instance's virtualization capabilities bitfield.
 */
#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION         0:0
#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION_NO      0x0
#define HGML_VGPU_VIRTUALIZATION_CAP_MIGRATION_YES     0x1

/*!
 * Macros for pGPU's virtualization capabilities bitfield.
 */
#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION         0:0
#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION_NO      0x0
#define HGML_VGPU_PGPU_VIRTUALIZATION_CAP_MIGRATION_YES     0x1

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlVgpuStructs vGPU Structs
 *  @{
 */
/***************************************************************************************************/

typedef unsigned int hgmlVgpuTypeId_t;

typedef unsigned int hgmlVgpuInstance_t;

/**
 * Structure to store Utilization Value and vgpuInstance
 */
typedef struct hgmlVgpuInstanceUtilizationSample_st
{
    hgmlVgpuInstance_t  vgpuInstance;       //!< vGPU Instance
    unsigned long long  timeStamp;          //!< CPU Timestamp in microseconds
    hgmlValue_t         smUtil;             //!< SM (3D/Compute) Util Value
    hgmlValue_t         memUtil;            //!< Frame Buffer Memory Util Value
    hgmlValue_t         encUtil;            //!< Encoder Util Value
    hgmlValue_t         decUtil;            //!< Decoder Util Value
} hgmlVgpuInstanceUtilizationSample_t;

/**
 * Structure to store Utilization Value, vgpuInstance and subprocess information
 */
typedef struct hgmlVgpuProcessUtilizationSample_st
{
    hgmlVgpuInstance_t  vgpuInstance;                               //!< vGPU Instance
    unsigned int        pid;                                        //!< PID of process running within the vGPU VM
    char                processName[HGML_VGPU_NAME_BUFFER_SIZE];    //!< Name of process running within the vGPU VM
    unsigned long long  timeStamp;                                  //!< CPU Timestamp in microseconds
    unsigned int        smUtil;                                     //!< SM (3D/Compute) Util Value
    unsigned int        memUtil;                                    //!< Frame Buffer Memory Util Value
    unsigned int        encUtil;                                    //!< Encoder Util Value
    unsigned int        decUtil;                                    //!< Decoder Util Value
} hgmlVgpuProcessUtilizationSample_t;

/**
 * vGPU scheduler policies
 */
#define HGML_SUPPORTED_VGPU_SCHEDULER_POLICY_COUNT 3

#define HGML_SCHEDULER_SW_MAX_LOG_ENTRIES 200

/**
 * Union to represent the vGPU Scheduler Parameters
 */
typedef union
{
    struct
    {
        unsigned int    avgFactor;          //!< Average factor in compensating the timeslice for Adaptive Round Robin mode
        unsigned int    timeslice;          //!< The timeslice in ns for each software run list as configured, or the default value otherwise
    } vgpuSchedDataWithARR;

    struct
    {
        unsigned int    timeslice;          //!< The timeslice in ns for each software run list as configured, or the default value otherwise
    } vgpuSchedData;

} hgmlVgpuSchedulerParams_t;

/**
 * Structure to store the state and logs of a software runlist
 */
typedef struct hgmlVgpuSchedulerLogEntries_st
{
    unsigned long long          timestamp;                  //!< Timestamp in ns when this software runlist was preeempted
    unsigned long long          timeRunTotal;               //!< Total time in ns this software runlist has run
    unsigned long long          timeRun;                    //!< Time in ns this software runlist ran before preemption
    unsigned int                swRunlistId;                //!< Software runlist Id
    unsigned long long          targetTimeSlice;            //!< The actual timeslice after deduction
    unsigned long long          cumulativePreemptionTime;   //!< Preemption time in ns for this SW runlist
} hgmlVgpuSchedulerLogEntry_t;

/**
 * Structure to store a vGPU software scheduler log
 */
typedef struct hgmlVgpuSchedulerLog_st
{
    unsigned int                engineId;                                       //!< Engine whose software runlist log entries are fetched
    unsigned int                schedulerPolicy;                                //!< Scheduler policy
    unsigned int                arrMode;                                        //!< Adaptive Round Robin scheduler mode. One of the HGML_VGPU_SCHEDULER_ARR_*.
    hgmlVgpuSchedulerParams_t   schedulerParams;
    unsigned int                entriesCount;                                   //!< Count of log entries fetched
    hgmlVgpuSchedulerLogEntry_t logEntries[HGML_SCHEDULER_SW_MAX_LOG_ENTRIES];
} hgmlVgpuSchedulerLog_t;

/**
 * Structure to store the vGPU scheduler state
 */
typedef struct hgmlVgpuSchedulerGetState_st
{
    unsigned int                schedulerPolicy;    //!< Scheduler policy
    unsigned int                arrMode;            //!< Adaptive Round Robin scheduler mode. One of the HGML_VGPU_SCHEDULER_ARR_*.
    hgmlVgpuSchedulerParams_t   schedulerParams;
} hgmlVgpuSchedulerGetState_t;

/**
 * Union to represent the vGPU Scheduler set Parameters
 */
typedef union
{
    struct
    {
        unsigned int    avgFactor;          //!< Average factor in compensating the timeslice for Adaptive Round Robin mode
        unsigned int    frequency;          //!< Frequency for Adaptive Round Robin mode
    } vgpuSchedDataWithARR;

    struct
    {
        unsigned int    timeslice;          //!< The timeslice in ns(Nanoseconds) for each software run list as configured, or the default value otherwise
    } vgpuSchedData;

} hgmlVgpuSchedulerSetParams_t;

/**
 * Structure to set the vGPU scheduler state
 */
typedef struct hgmlVgpuSchedulerSetState_st
{
    unsigned int                    schedulerPolicy;    //!< Scheduler policy
    unsigned int                    enableARRMode;      //!< Adaptive Round Robin scheduler
    hgmlVgpuSchedulerSetParams_t    schedulerParams;
} hgmlVgpuSchedulerSetState_t;

/**
 * Structure to store the vGPU scheduler capabilities
 */
typedef struct hgmlVgpuSchedulerCapabilities_st
{
    unsigned int        supportedSchedulers[HGML_SUPPORTED_VGPU_SCHEDULER_POLICY_COUNT]; //!< List the supported vGPU schedulers on the device
    unsigned int        maxTimeslice;                                                    //!< Maximum timeslice value in ns
    unsigned int        minTimeslice;                                                    //!< Minimum timeslice value in ns
    unsigned int        isArrModeSupported;                                              //!< Flag to check Adaptive Round Robin mode enabled/disabled.
    unsigned int        maxFrequencyForARR;                                              //!< Maximum frequency for Adaptive Round Robin mode
    unsigned int        minFrequencyForARR;                                              //!< Minimum frequency for Adaptive Round Robin mode
    unsigned int        maxAvgFactorForARR;                                              //!< Maximum averaging factor for Adaptive Round Robin mode
    unsigned int        minAvgFactorForARR;                                              //!< Minimum averaging factor for Adaptive Round Robin mode
} hgmlVgpuSchedulerCapabilities_t;

/**
 * Structure to store the vGPU license expiry details
 */
typedef struct hgmlVgpuLicenseExpiry_st
{
    unsigned int    year;        //!< Year of license expiry
    unsigned short  month;       //!< Month of license expiry
    unsigned short  day;         //!< Day of license expiry
    unsigned short  hour;        //!< Hour of license expiry
    unsigned short  min;         //!< Minutes of license expiry
    unsigned short  sec;         //!< Seconds of license expiry
    unsigned char   status;      //!< License expiry status
} hgmlVgpuLicenseExpiry_t;

/**
 * vGPU license state
 */
typedef struct hgmlVgpuLicenseInfo_st
{
    unsigned char               isLicensed;     //!< License status
    hgmlVgpuLicenseExpiry_t     licenseExpiry;  //!< License expiry information
    unsigned int                currentState;   //!< Current license state
} hgmlVgpuLicenseInfo_t;

/**
 * Structure to store utilization value and process Id
 */
typedef struct hgmlProcessUtilizationSample_st
{
    unsigned int        pid;            //!< PID of process
    unsigned long long  timeStamp;      //!< CPU Timestamp in microseconds
    unsigned int        smUtil;         //!< SM (3D/Compute) Util Value
    unsigned int        memUtil;        //!< Frame Buffer Memory Util Value
    unsigned int        encUtil;        //!< Encoder Util Value
    unsigned int        decUtil;        //!< Decoder Util Value
} hgmlProcessUtilizationSample_t;

/**
 * Structure to store license expiry date and time values
 */
typedef struct hgmlGridLicenseExpiry_st
{
    unsigned int   year;        //!< Year value of license expiry
    unsigned short month;       //!< Month value of license expiry
    unsigned short day;         //!< Day value of license expiry
    unsigned short hour;        //!< Hour value of license expiry
    unsigned short min;         //!< Minutes value of license expiry
    unsigned short sec;         //!< Seconds value of license expiry
    unsigned char  status;      //!< License expiry status
} hgmlGridLicenseExpiry_t;

/**
 * Structure containing vGPU software licensable feature information
 */
typedef struct hgmlGridLicensableFeature_st
{
    hgmlGridLicenseFeatureCode_t    featureCode;                                 //!< Licensed feature code
    unsigned int                    featureState;                                //!< Non-zero if feature is currently licensed, otherwise zero
    char                            licenseInfo[HGML_GRID_LICENSE_BUFFER_SIZE];  //!< Deprecated.
    char                            productName[HGML_GRID_LICENSE_BUFFER_SIZE];  //!< Product name of feature
    unsigned int                    featureEnabled;                              //!< Non-zero if feature is enabled, otherwise zero
    hgmlGridLicenseExpiry_t         licenseExpiry;                               //!< License expiry structure containing date and time
} hgmlGridLicensableFeature_t;

/**
 * Structure to store vGPU software licensable features
 */
typedef struct hgmlGridLicensableFeatures_st
{
    int                         isGridLicenseSupported;                                       //!< Non-zero if vGPU Software Licensing is supported on the system, otherwise zero
    unsigned int                licensableFeaturesCount;                                      //!< Entries returned in \a gridLicensableFeatures array
    hgmlGridLicensableFeature_t gridLicensableFeatures[HGML_GRID_LICENSE_FEATURE_MAX_COUNT];  //!< Array of vGPU software licensable features.
} hgmlGridLicensableFeatures_t;

/**
 * Simplified chip architecture
 * @ref NVML_DEVICE_ARCH_AMPERE    7
 */
#define HGML_DEVICE_ARCH_BEETHOVEN   7 // Devices based on the T-HEAD Beethoven architecture

#define HGML_DEVICE_ARCH_UNKNOWN   0xffffffff // Anything else, presumably something newer

typedef unsigned int hgmlDeviceArchitecture_t;

/**
 * PCI bus types
 */
#define HGML_BUS_TYPE_UNKNOWN  0
#define HGML_BUS_TYPE_PCI      1
#define HGML_BUS_TYPE_PCIE     2
#define HGML_BUS_TYPE_FPCI     3
#define HGML_BUS_TYPE_AGP      4

typedef unsigned int hgmlBusType_t;

/**
 * Device Power Modes
 */

/**
 * Device Fan control policy
 */
#define HGML_FAN_POLICY_TEMPERATURE_CONTINOUS_SW 0
#define HGML_FAN_POLICY_MANUAL                   1

typedef unsigned int hgmlFanControlPolicy_t;

/**
 * Device Power Source
 */
#define HGML_POWER_SOURCE_AC         0x00000000
#define HGML_POWER_SOURCE_BATTERY    0x00000001
#define HGML_POWER_SOURCE_UNDERSIZED 0x00000002

typedef unsigned int hgmlPowerSource_t;

/*
 * Device PCIE link Max Speed
 */
#define HGML_PCIE_LINK_MAX_SPEED_INVALID   0x00000000
#define HGML_PCIE_LINK_MAX_SPEED_2500MBPS  0x00000001
#define HGML_PCIE_LINK_MAX_SPEED_5000MBPS  0x00000002
#define HGML_PCIE_LINK_MAX_SPEED_8000MBPS  0x00000003
#define HGML_PCIE_LINK_MAX_SPEED_16000MBPS 0x00000004
#define HGML_PCIE_LINK_MAX_SPEED_32000MBPS 0x00000005
#define HGML_PCIE_LINK_MAX_SPEED_64000MBPS 0x00000006

/*
 * Adaptive clocking status
 */
#define HGML_ADAPTIVE_CLOCKING_INFO_STATUS_DISABLED 0x00000000
#define HGML_ADAPTIVE_CLOCKING_INFO_STATUS_ENABLED  0x00000001

#define HGML_MAX_GPU_UTILIZATIONS 8
typedef enum hgmlGpuUtilizationDomainId_t
{
    HGML_GPU_UTILIZATION_DOMAIN_GPU    = 0, //!< Graphics engine domain
    HGML_GPU_UTILIZATION_DOMAIN_FB     = 1, //!< Frame buffer domain
    HGML_GPU_UTILIZATION_DOMAIN_VID    = 2, //!< Video engine domain
    HGML_GPU_UTILIZATION_DOMAIN_BUS    = 3, //!< Bus interface domain
} hgmlGpuUtilizationDomainId_t;

typedef struct hgmlGpuDynamicPstatesInfo_st
{
    unsigned int       flags;          //!< Reserved for future use
    struct
    {
        unsigned int   bIsPresent;     //!< Set if this utilization domain is present on this GPU
        unsigned int   percentage;     //!< Percentage of time where the domain is considered busy in the last 1-second interval
        unsigned int   incThreshold;   //!< Utilization threshold that can trigger a perf-increasing P-State change when crossed
        unsigned int   decThreshold;   //!< Utilization threshold that can trigger a perf-decreasing P-State change when crossed
    } utilization[HGML_MAX_GPU_UTILIZATIONS];
} hgmlGpuDynamicPstatesInfo_t;

/** @} */
/** @} */

/***************************************************************************************************/
/** @defgroup hgmlFieldValueEnums Field Value Enums
 *  @{
 */
/***************************************************************************************************/

/**
 * Field Identifiers.
 *
 * All Identifiers pertain to a device. Each ID is only used once and is guaranteed never to change.
 */
#define HGML_FI_DEV_ECC_CURRENT           1   //!< Current ECC mode. 1=Active. 0=Inactive
#define HGML_FI_DEV_ECC_PENDING           2   //!< Pending ECC mode. 1=Active. 0=Inactive
/* ECC Count Totals */
#define HGML_FI_DEV_ECC_SBE_VOL_TOTAL     3   //!< Total single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_TOTAL     4   //!< Total double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_TOTAL     5   //!< Total single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_TOTAL     6   //!< Total double bit aggregate (persistent) ECC errors
/* Individual ECC locations */
#define HGML_FI_DEV_ECC_SBE_VOL_L1        7   //!< L1 cache single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_L1        8   //!< L1 cache double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_VOL_L2        9   //!< L2 cache single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_L2        10  //!< L2 cache double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_VOL_DEV       11  //!< Device memory single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_DEV       12  //!< Device memory double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_VOL_REG       13  //!< Register file single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_REG       14  //!< Register file double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_VOL_TEX       15  //!< Texture memory single bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_TEX       16  //!< Texture memory double bit volatile ECC errors
#define HGML_FI_DEV_ECC_DBE_VOL_CBU       17  //!< CBU double bit volatile ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_L1        18  //!< L1 cache single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_L1        19  //!< L1 cache double bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_L2        20  //!< L2 cache single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_L2        21  //!< L2 cache double bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_DEV       22  //!< Device memory single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_DEV       23  //!< Device memory double bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_REG       24  //!< Register File single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_REG       25  //!< Register File double bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_SBE_AGG_TEX       26  //!< Texture memory single bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_TEX       27  //!< Texture memory double bit aggregate (persistent) ECC errors
#define HGML_FI_DEV_ECC_DBE_AGG_CBU       28  //!< CBU double bit aggregate ECC errors

/* Page Retirement */
#define HGML_FI_DEV_RETIRED_SBE           29  //!< Number of retired pages because of single bit errors
#define HGML_FI_DEV_RETIRED_DBE           30  //!< Number of retired pages because of double bit errors
#define HGML_FI_DEV_RETIRED_PENDING       31  //!< If any pages are pending retirement. 1=yes. 0=no.

/* ICNLink Flit Error Counters */
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L0    32 //!< ICNLink flow control CRC  Error Counter for Lane 0
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L1    33 //!< ICNLink flow control CRC  Error Counter for Lane 1
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L2    34 //!< ICNLink flow control CRC  Error Counter for Lane 2
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L3    35 //!< ICNLink flow control CRC  Error Counter for Lane 3
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L4    36 //!< ICNLink flow control CRC  Error Counter for Lane 4
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L5    37 //!< ICNLink flow control CRC  Error Counter for Lane 5
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_TOTAL 38 //!< ICNLink flow control CRC  Error Counter total for all Lanes

/* ICNLink CRC Data Error Counters */
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L0    39 //!< ICNLink data CRC Error Counter for Lane 0
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L1    40 //!< ICNLink data CRC Error Counter for Lane 1
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L2    41 //!< ICNLink data CRC Error Counter for Lane 2
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L3    42 //!< ICNLink data CRC Error Counter for Lane 3
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L4    43 //!< ICNLink data CRC Error Counter for Lane 4
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L5    44 //!< ICNLink data CRC Error Counter for Lane 5
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_TOTAL 45 //!< ICNLink data CRC Error Counter total for all Lanes

/* ICNLink Replay Error Counters */
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L0      46 //!< ICNLink Replay Error Counter for Lane 0
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L1      47 //!< ICNLink Replay Error Counter for Lane 1
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L2      48 //!< ICNLink Replay Error Counter for Lane 2
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L3      49 //!< ICNLink Replay Error Counter for Lane 3
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L4      50 //!< ICNLink Replay Error Counter for Lane 4
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L5      51 //!< ICNLink Replay Error Counter for Lane 5
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_TOTAL   52 //!< ICNLink Replay Error Counter total for all Lanes

/* ICNLink Recovery Error Counters */
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L0    53 //!< ICNLink Recovery Error Counter for Lane 0
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L1    54 //!< ICNLink Recovery Error Counter for Lane 1
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L2    55 //!< ICNLink Recovery Error Counter for Lane 2
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L3    56 //!< ICNLink Recovery Error Counter for Lane 3
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L4    57 //!< ICNLink Recovery Error Counter for Lane 4
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L5    58 //!< ICNLink Recovery Error Counter for Lane 5
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_TOTAL 59 //!< ICNLink Recovery Error Counter total for all Lanes

/* ICNLink Bandwidth Counters */
/*
 * HGML_FI_DEV_ICNLINK_BANDWIDTH_* field values are now deprecated.
 * Please use the following field values instead:
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_TX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_RX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_TX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_RX
 */

#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L0     60 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 0
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L1     61 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 1
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L2     62 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 2
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L3     63 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 3
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L4     64 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 4
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L5     65 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 5
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_TOTAL  66 //!< ICNLink Bandwidth Counter Total for Counter Set 0, All Lanes

/* ICNLink Bandwidth Counters */
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L0     67 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 0
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L1     68 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 1
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L2     69 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 2
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L3     70 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 3
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L4     71 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 4
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L5     72 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 5
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_TOTAL  73 //!< ICNLink Bandwidth Counter Total for Counter Set 1, All Lanes

/* HGML Perf Policy Counters */
#define HGML_FI_DEV_PERF_POLICY_POWER              74   //!< Perf Policy Counter for Power Policy
#define HGML_FI_DEV_PERF_POLICY_THERMAL            75   //!< Perf Policy Counter for Thermal Policy
#define HGML_FI_DEV_PERF_POLICY_SYNC_BOOST         76   //!< Perf Policy Counter for Sync boost Policy
#define HGML_FI_DEV_PERF_POLICY_BOARD_LIMIT        77   //!< Perf Policy Counter for Board Limit
#define HGML_FI_DEV_PERF_POLICY_LOW_UTILIZATION    78   //!< Perf Policy Counter for Low GPU Utilization Policy
#define HGML_FI_DEV_PERF_POLICY_RELIABILITY        79   //!< Perf Policy Counter for Reliability Policy
#define HGML_FI_DEV_PERF_POLICY_TOTAL_APP_CLOCKS   80   //!< Perf Policy Counter for Total App Clock Policy
#define HGML_FI_DEV_PERF_POLICY_TOTAL_BASE_CLOCKS  81   //!< Perf Policy Counter for Total Base Clocks Policy

/* Memory temperatures */
#define HGML_FI_DEV_MEMORY_TEMP  82 //!< Memory temperature for the device

/* Energy Counter */
#define HGML_FI_DEV_TOTAL_ENERGY_CONSUMPTION 83 //!< Total energy consumption for the GPU in mJ since the driver was last reloaded

/* ICNLink Speed */
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L0     84  //!< ICNLink Speed in MBps for Link 0
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L1     85  //!< ICNLink Speed in MBps for Link 1
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L2     86  //!< ICNLink Speed in MBps for Link 2
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L3     87  //!< ICNLink Speed in MBps for Link 3
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L4     88  //!< ICNLink Speed in MBps for Link 4
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L5     89  //!< ICNLink Speed in MBps for Link 5
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_COMMON 90  //!< Common ICNLink Speed in MBps for active links

#define HGML_FI_DEV_ICNLINK_LINK_COUNT        91  //!< Number of ICNLinks present on the device

#define HGML_FI_DEV_RETIRED_PENDING_SBE      92  //!< If any pages are pending retirement due to SBE. 1=yes. 0=no.
#define HGML_FI_DEV_RETIRED_PENDING_DBE      93  //!< If any pages are pending retirement due to DBE. 1=yes. 0=no.

#define HGML_FI_DEV_PCIE_REPLAY_COUNTER             94  //!< PCIe replay counter
#define HGML_FI_DEV_PCIE_REPLAY_ROLLOVER_COUNTER    95  //!< PCIe replay rollover counter

/* ICNLink Flit Error Counters */
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L6     96 //!< ICNLink flow control CRC  Error Counter for Lane 6
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L7     97 //!< ICNLink flow control CRC  Error Counter for Lane 7
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L8     98 //!< ICNLink flow control CRC  Error Counter for Lane 8
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L9     99 //!< ICNLink flow control CRC  Error Counter for Lane 9
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L10   100 //!< ICNLink flow control CRC  Error Counter for Lane 10
#define HGML_FI_DEV_ICNLINK_CRC_FLIT_ERROR_COUNT_L11   101 //!< ICNLink flow control CRC  Error Counter for Lane 11

/* ICNLink CRC Data Error Counters */
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L6    102 //!< ICNLink data CRC Error Counter for Lane 6
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L7    103 //!< ICNLink data CRC Error Counter for Lane 7
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L8    104 //!< ICNLink data CRC Error Counter for Lane 8
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L9    105 //!< ICNLink data CRC Error Counter for Lane 9
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L10   106 //!< ICNLink data CRC Error Counter for Lane 10
#define HGML_FI_DEV_ICNLINK_CRC_DATA_ERROR_COUNT_L11   107 //!< ICNLink data CRC Error Counter for Lane 11

/* ICNLink Replay Error Counters */
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L6      108 //!< ICNLink Replay Error Counter for Lane 6
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L7      109 //!< ICNLink Replay Error Counter for Lane 7
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L8      110 //!< ICNLink Replay Error Counter for Lane 8
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L9      111 //!< ICNLink Replay Error Counter for Lane 9
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L10     112 //!< ICNLink Replay Error Counter for Lane 10
#define HGML_FI_DEV_ICNLINK_REPLAY_ERROR_COUNT_L11     113 //!< ICNLink Replay Error Counter for Lane 11

/* ICNLink Recovery Error Counters */
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L6    114 //!< ICNLink Recovery Error Counter for Lane 6
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L7    115 //!< ICNLink Recovery Error Counter for Lane 7
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L8    116 //!< ICNLink Recovery Error Counter for Lane 8
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L9    117 //!< ICNLink Recovery Error Counter for Lane 9
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L10   118 //!< ICNLink Recovery Error Counter for Lane 10
#define HGML_FI_DEV_ICNLINK_RECOVERY_ERROR_COUNT_L11   119 //!< ICNLink Recovery Error Counter for Lane 11

/* ICNLink Bandwidth Counters */
/*
 * HGML_FI_DEV_ICNLINK_BANDWIDTH_* field values are now deprecated.
 * Please use the following field values instead:
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_TX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_RX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_TX
 * HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_RX
 */
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L6     120 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 6
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L7     121 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 7
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L8     122 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 8
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L9     123 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 9
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L10    124 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 10
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C0_L11    125 //!< ICNLink Bandwidth Counter for Counter Set 0, Lane 11

/* ICNLink Bandwidth Counters */
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L6     126 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 6
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L7     127 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 7
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L8     128 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 8
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L9     129 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 9
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L10    130 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 10
#define HGML_FI_DEV_ICNLINK_BANDWIDTH_C1_L11    131 //!< ICNLink Bandwidth Counter for Counter Set 1, Lane 11

/* ICNLink Speed */
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L6     132  //!< ICNLink Speed in MBps for Link 6
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L7     133  //!< ICNLink Speed in MBps for Link 7
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L8     134  //!< ICNLink Speed in MBps for Link 8
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L9     135  //!< ICNLink Speed in MBps for Link 9
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L10    136  //!< ICNLink Speed in MBps for Link 10
#define HGML_FI_DEV_ICNLINK_SPEED_MBPS_L11    137  //!< ICNLink Speed in MBps for Link 11

/**
 * ICNLink throughput counters field values
 *
 * Link ID needs to be specified in the scopeId field in hgmlFieldValue_t.
 * A scopeId of UINT_MAX returns aggregate value summed up across all links
 * for the specified counter type in fieldId.
 */
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_TX      138 //!< ICNLink TX Data throughput in KiB
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_DATA_RX      139 //!< ICNLink RX Data throughput in KiB
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_TX       140 //!< ICNLink TX Data + protocol overhead in KiB
#define HGML_FI_DEV_ICNLINK_THROUGHPUT_RAW_RX       141 //!< ICNLink RX Data + protocol overhead in KiB

/* Row Remapper */
#define HGML_FI_DEV_REMAPPED_COR        142 //!< Number of remapped rows due to correctable errors
#define HGML_FI_DEV_REMAPPED_UNC        143 //!< Number of remapped rows due to uncorrectable errors
#define HGML_FI_DEV_REMAPPED_PENDING    144 //!< If any rows are pending remapping. 1=yes 0=no
#define HGML_FI_DEV_REMAPPED_FAILURE    145 //!< If any rows failed to be remapped 1=yes 0=no

/**
 * Remote device ICNLink ID
 *
 * Link ID needs to be specified in the scopeId field in hgmlFieldValue_t.
 */
#define HGML_FI_DEV_ICNLINK_REMOTE_ICNLINK_ID     146 //!< Remote device ICNLink ID

/**
 * NVSwitch: connected ICNLink count
 */
#define HGML_FI_DEV_NVSWITCH_CONNECTED_LINK_COUNT   147  //!< Number of IcnLinks connected to NVSwitch

/* ICNLink ECC Data Error Counters
 *
 * Lane ID needs to be specified in the scopeId field in hgmlFieldValue_t.
 *
 */
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L0    148 //!< ICNLink data ECC Error Counter for Link 0
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L1    149 //!< ICNLink data ECC Error Counter for Link 1
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L2    150 //!< ICNLink data ECC Error Counter for Link 2
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L3    151 //!< ICNLink data ECC Error Counter for Link 3
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L4    152 //!< ICNLink data ECC Error Counter for Link 4
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L5    153 //!< ICNLink data ECC Error Counter for Link 5
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L6    154 //!< ICNLink data ECC Error Counter for Link 6
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L7    155 //!< ICNLink data ECC Error Counter for Link 7
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L8    156 //!< ICNLink data ECC Error Counter for Link 8
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L9    157 //!< ICNLink data ECC Error Counter for Link 9
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L10   158 //!< ICNLink data ECC Error Counter for Link 10
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_L11   159 //!< ICNLink data ECC Error Counter for Link 11
#define HGML_FI_DEV_ICNLINK_ECC_DATA_ERROR_COUNT_TOTAL 160 //!< ICNLink data ECC Error Counter total for all Links

#define HGML_FI_DEV_ICNLINK_ERROR_DL_REPLAY            161 //!< ICNLink Replay Error Counter
#define HGML_FI_DEV_ICNLINK_ERROR_DL_RECOVERY          162 //!< ICNLink Recovery Error Counter
#define HGML_FI_DEV_ICNLINK_ERROR_DL_CRC               163 //!< ICNLink CRC Error Counter
#define HGML_FI_DEV_ICNLINK_GET_SPEED                  164 //!< ICNLink Speed in MBps
#define HGML_FI_DEV_ICNLINK_GET_STATE                  165 //!< ICNLink State - Active,Inactive
#define HGML_FI_DEV_ICNLINK_GET_VERSION                166 //!< ICNLink Version

#define HGML_FI_DEV_ICNLINK_GET_POWER_STATE            167 //!< ICNLink Power state. 0=HIGH_SPEED 1=LOW_SPEED
#define HGML_FI_DEV_ICNLINK_GET_POWER_THRESHOLD        168 //!< ICNLink length of idle period (in units of 100us) before transitioning links to sleep state

#define HGML_FI_DEV_PCIE_L0_TO_RECOVERY_COUNTER       169 //!< Device PEX error recovery counter

#define HGML_FI_DEV_C2C_LINK_COUNT                    170 //!< Number of C2C Links present on the device
#define HGML_FI_DEV_C2C_LINK_GET_STATUS               171 //!< C2C Link Status 0=INACTIVE 1=ACTIVE
#define HGML_FI_DEV_C2C_LINK_GET_MAX_BW               172 //!< C2C Link Speed in MBps for active links

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

/**
 * Retrieves power usage for this GPU in milliwatts.
 * It is only available if power management mode is supported. See \ref hgmlDeviceGetPowerManagementMode and
 * \ref hgmlDeviceGetPowerUsage.
 *
 * scopeId needs to be specified. It signifies:
 * 0 - GPU Only Scope - Metrics for GPU are retrieved
 * 1 - Module scope - Metrics for the module (e.g. CPU + GPU) are retrieved.
 * Note: CPU here refers to alixpu CPU. x86 or non-alixpu ARM is not supported
 */
#define HGML_FI_DEV_POWER_AVERAGE                     185 //!< GPU power averaged over 1 sec interval, supported on or newer architectures.
#define HGML_FI_DEV_POWER_INSTANT                     186 //!< Current GPU power, supported on all architectures.
#define HGML_FI_DEV_POWER_MIN_LIMIT                   187 //!< Minimum power limit in milliwatts.
#define HGML_FI_DEV_POWER_MAX_LIMIT                   188 //!< Maximum power limit in milliwatts.
#define HGML_FI_DEV_POWER_DEFAULT_LIMIT               189 //!< Default power limit in milliwatts (limit which device boots with).
#define HGML_FI_DEV_POWER_CURRENT_LIMIT               190 //!< Limit currently enforced in milliwatts (This includes other limits set elsewhere. E.g. Out-of-band).
#define HGML_FI_DEV_ENERGY                            191 //!< Total energy consumption (in mJ) since the driver was last reloaded. Same as \ref HGML_FI_DEV_TOTAL_ENERGY_CONSUMPTION for the GPU.
#define HGML_FI_DEV_POWER_REQUESTED_LIMIT             192 //!< Power limit requested by HGML or any other userspace client.

/**
 * GPU T.Limit temperature thresholds in degree Celsius
 *
 * These fields are supported on Ada and later architectures and supersedes \ref hgmlDeviceGetTemperatureThreshold.
 */
#define HGML_FI_DEV_TEMPERATURE_SHUTDOWN_TLIMIT       193 //!< T.Limit temperature after which GPU may shut down for HW protection
#define HGML_FI_DEV_TEMPERATURE_SLOWDOWN_TLIMIT       194 //!< T.Limit temperature after which GPU may begin HW slowdown
#define HGML_FI_DEV_TEMPERATURE_MEM_MAX_TLIMIT        195 //!< T.Limit temperature after which GPU may begin SW slowdown due to memory temperature
#define HGML_FI_DEV_TEMPERATURE_GPU_MAX_TLIMIT        196 //!< T.Limit temperature after which GPU may be throttled below base clock

#define HGML_FI_DEV_ICNLINK_CABLE_STATUS              500 //!< ICNLink cable status
#define HGML_FI_DEV_ICNLINK_LANE_WIDTH                501 //!< ICNLink lane width
#define HGML_FI_DEV_ICNLINK_LINKUP_COUNT              502 //!< ICNLink linkup count
#define HGML_FI_DEV_ICNLINK_LINKDOWN_COUNT            503 //!< ICNLink linkdown count
#define HGML_FI_DEV_ICNLINK_FECC_ERROR_COUNT          504 //!< ICNLink fec correctable errors
#define HGML_FI_DEV_ICNLINK_FECU_ERROR_COUNT          505 //!< ICNLink fec uncorrectable errors
#define HGML_FI_DEV_ICNLINK_PACKET_ERROR_TX           506 //!< ICNLink packet error tx count
#define HGML_FI_DEV_ICNLINK_PACKET_ERROR_RX           507 //!< ICNLink packet error rx count
#define HGML_FI_DEV_ICNLINK_PACKET_TOTAL_TX           508 //!< ICNLink packet total tx count
#define HGML_FI_DEV_ICNLINK_PACKET_TOTAL_RX           509 //!< ICNLink packet total rx count


#define HGML_FI_DEV_CORE_UTILIZATION                  510 //!< Percent of time during which CE/DMA engine remains busy
#define HGML_FI_DEV_AUTO_RESET_STATUS                 511 //!< Device auto reset status
#define HGML_FI_DEV_ICNLINK_PHYSICAL_PORT             512 //!< ICNLink physical port
#define HGML_FI_DEV_OVERCLOCKING_MODE                 513 //!< Device overclocking mode
#define HGML_FI_DEV_HBM_VENDOR                        514 //!< Device hbm vendor
#define HGML_FI_DEV_BASE_CLOCK                        515 //!< Device base clock
#define HGML_FI_DEV_REAR_ID                           516 //!< Device rear id

#define HGML_FI_DEV_XID_PPU_RESET                     517 //!< Device Xid for PPU reset repair
#define HGML_FI_DEV_XID_OS_REBOOT                     518 //!< Device Xid for OS reboot repair
#define HGML_FI_DEV_XID_COLD_REBOOT                   519 //!< Device Xid for BMC cold reboot repair

#define HGML_FI_DEV_PCM_ENABLED                       520 //!< Whether PCM is enabled
#define HGML_FI_DEV_GPM_ENABLED                       521 //!< Whether GPM is enabled
#define HGML_FI_DEV_TIDE_MODE_STATUS                  522 //!< Device tide mode status
#define HGML_FI_DEV_MPS_MODE_STATUS                   523 //!< Device MPS mode status

#define HGML_FI_MAX                                   524 //!< One greater than the largest field ID defined above

/**
 * Information for a Field Value Sample
 */
typedef struct hgmlFieldValue_st
{
    unsigned int fieldId;       //!< ID of the HGML field to retrieve. This must be set before any call that uses this struct. See the constants starting with HGML_FI_ above.
    unsigned int scopeId;       //!< Scope ID can represent data used by HGML depending on fieldId's context. For example, for ICNLink throughput counter data, scopeId can represent linkId.
    long long timestamp;        //!< CPU Timestamp of this value in microseconds since 1970
    long long latencyUsec;      //!< How long this field value took to update (in usec) within HGML. This may be averaged across several fields that are serviced by the same driver call.
    hgmlValueType_t valueType;  //!< Type of the value stored in value
    hgmlReturn_t hgmlReturn;    //!< Return code for retrieving this value. This must be checked before looking at value, as value is undefined if hgmlReturn != HGML_SUCCESS
    hgmlValue_t value;          //!< Value for this field. This is only valid if hgmlReturn == HGML_SUCCESS
} hgmlFieldValue_t;


/** @} */

/***************************************************************************************************/
/** @defgroup hgmlUnitStructs Unit Structs
 *  @{
 */
/***************************************************************************************************/

typedef struct hgmlUnit_st* hgmlUnit_t;

/**
 * Description of HWBC entry
 */
typedef struct hgmlHwbcEntry_st
{
    unsigned int hwbcId;
    char firmwareVersion[32];
} hgmlHwbcEntry_t;

/**
 * Overclocking mode enum.
 */
typedef enum hgmlOverclockingMode_enum
{
    HGML_OVERCLOCKING_MODE_DEFAULT = 0,     //!< Default max freq mode
    HGML_OVERCLOCKING_MODE_ULTRA,           //!< Ultra max freq mode
} hgmlOverclockingMode_t;

/**
 * Fan state enum.
 */
typedef enum hgmlFanState_enum
{
    HGML_FAN_NORMAL       = 0,     //!< Fan is working properly
    HGML_FAN_FAILED       = 1      //!< Fan has failed
} hgmlFanState_t;

/**
 * Led color enum.
 */
typedef enum hgmlLedColor_enum
{
    HGML_LED_COLOR_GREEN       = 0,     //!< GREEN, indicates good health
    HGML_LED_COLOR_AMBER       = 1      //!< AMBER, indicates problem
} hgmlLedColor_t;

/**
 * LED states for an S-class unit.
 */
typedef struct hgmlLedState_st
{
    char cause[256];               //!< If amber, a text description of the cause
    hgmlLedColor_t color;          //!< GREEN or AMBER
} hgmlLedState_t;

/**
 * Static S-class unit info.
 */
typedef struct hgmlUnitInfo_st
{
    char name[96];                      //!< Product name
    char id[96];                        //!< Product identifier
    char serial[96];                    //!< Product serial number
    char firmwareVersion[96];           //!< Firmware version
} hgmlUnitInfo_t;

/**
 * Power usage information for an S-class unit.
 * The power supply state is a human readable string that equals "Normal" or contains
 * a combination of "Abnormal" plus one or more of the following:
 *
 *    - High voltage
 *    - Fan failure
 *    - Heatsink temperature
 *    - Current limit
 *    - Voltage below UV alarm threshold
 *    - Low-voltage
 *    - SI2C remote off command
 *    - MOD_DISABLE input
 *    - Short pin transition
*/
typedef struct hgmlPSUInfo_st
{
    char state[256];                 //!< The power supply state
    unsigned int current;            //!< PSU current (A)
    unsigned int voltage;            //!< PSU voltage (V)
    unsigned int power;              //!< PSU power draw (W)
} hgmlPSUInfo_t;

/**
 * Fan speed reading for a single fan in an S-class unit.
 */
typedef struct hgmlUnitFanInfo_st
{
    unsigned int speed;              //!< Fan speed (RPM)
    hgmlFanState_t state;            //!< Flag that indicates whether fan is working properly
} hgmlUnitFanInfo_t;

/**
 * Fan speed readings for an entire S-class unit.
 */
typedef struct hgmlUnitFanSpeeds_st
{
    hgmlUnitFanInfo_t fans[24];      //!< Fan speed data for each fan
    unsigned int count;              //!< Number of fans in unit
} hgmlUnitFanSpeeds_t;

/** @} */

/***************************************************************************************************/
/** @addtogroup hgmlEvents
 *  @{
 */
/***************************************************************************************************/

/**
 * Handle to an event set
 */
typedef struct hgmlEventSet_st* hgmlEventSet_t;

/** @defgroup hgmlEventType Event Types
 * @{
 * Event Types which user can be notified about.
 * See description of particular functions for details.
 *
 * See \ref hgmlDeviceRegisterEvents and \ref hgmlDeviceGetSupportedEventTypes to check which devices
 * support each event.
 *
 * Types can be combined with bitwise or operator '|' when passed to \ref hgmlDeviceRegisterEvents
 */
//! Event about single bit ECC errors
/**
 * \note A corrected texture memory error is not an ECC error, so it does not generate a single bit event
 */
#define hgmlEventTypeSingleBitEccError     0x0000000000000001LL

//! Event about double bit ECC errors
/**
 * \note An uncorrected texture memory error is not an ECC error, so it does not generate a double bit event
 */
#define hgmlEventTypeDoubleBitEccError     0x0000000000000002LL

//! Event about PState changes
/**
 *  \note On architecture PState changes are also an indicator that GPU is throttling down due to
 *  no work being executed on the GPU, power capping or thermal capping. In a typical situation,
 *  GPU should stay in P0 for the duration of the execution of the compute process.
 */
#define hgmlEventTypePState                0x0000000000000004LL

//! Event that Xid critical error occurred
#define hgmlEventTypeXidCriticalError      0x0000000000000008LL

//! Event about clock changes
#define hgmlEventTypeClock                 0x0000000000000010LL

//! Event about AC/Battery power source changes
#define hgmlEventTypePowerSourceChange     0x0000000000000080LL

//! Event about MIG configuration changes
#define hgmlEventMigConfigChange           0x0000000000000100LL

//! Mask with no events
#define hgmlEventTypeNone                  0x0000000000000000LL

//! Mask of all events
#define hgmlEventTypeAll (hgmlEventTypeNone    \
        | hgmlEventTypeSingleBitEccError       \
        | hgmlEventTypeDoubleBitEccError       \
        | hgmlEventTypePState                  \
        | hgmlEventTypeClock                   \
        | hgmlEventTypeXidCriticalError        \
        | hgmlEventTypePowerSourceChange       \
        | hgmlEventMigConfigChange             \
        )
/** @} */

/**
 * Information about occurred event
 */
typedef struct hgmlEventData_st
{
    hgmlDevice_t        device;             //!< Specific device where the event occurred
    unsigned long long  eventType;          //!< Information about what specific event occurred
    unsigned long long  eventData;          //!< Stores XID error for the device in the event of hgmlEventTypeXidCriticalError,
                                            //   eventData is 0 for any other event. eventData is set as 999 for unknown xid error.
    unsigned int        gpuInstanceId;      //!< If MIG is enabled and hgmlEventTypeXidCriticalError event is attributable to a GPU
                                            //   instance, stores a valid GPU instance ID. gpuInstanceId is set to 0xFFFFFFFF
                                            //   otherwise.
    unsigned int        computeInstanceId;  //!< If MIG is enabled and hgmlEventTypeXidCriticalError event is attributable to a
                                            //   compute instance, stores a valid compute instance ID. computeInstanceId is set to
                                            //   0xFFFFFFFF otherwise.
} hgmlEventData_t;

/** @} */

/***************************************************************************************************/
/** @addtogroup hgmlClocksEventReasons
 *  @{
 */
/***************************************************************************************************/

/** Nothing is running on the GPU and the clocks are dropping to Idle state
 * \note This limiter may be removed in a later release
 */
#define hgmlClocksEventReasonGpuIdle                   0x0000000000000001LL

/** GPU clocks are limited by current setting of applications clocks
 *
 * @see hgmlDeviceSetApplicationsClocks
 * @see hgmlDeviceGetApplicationsClock
 */
#define hgmlClocksEventReasonApplicationsClocksSetting 0x0000000000000002LL

/**
 * @deprecated Renamed to \ref hgmlClocksThrottleReasonApplicationsClocksSetting
 *             as the name describes the situation more accurately.
 */
#define hgmlClocksThrottleReasonUserDefinedClocks         hgmlClocksEventReasonApplicationsClocksSetting

/** The clocks have been optimized to ensure not to exceed currently set power limits
 *
 * @see hgmlDeviceGetPowerUsage
 * @see hgmlDeviceSetPowerManagementLimit
 * @see hgmlDeviceGetPowerManagementLimit
 */
#define hgmlClocksEventReasonSwPowerCap                0x0000000000000004LL

/** HW Slowdown (reducing the core clocks by a factor of 2 or more) is engaged
 *
 * This is an indicator of:
 *   - temperature being too high
 *   - External Power Brake Assertion is triggered (e.g. by the system power supply)
 *   - Power draw is too high and Fast Trigger protection is reducing the clocks
 *   - May be also reported during PState or clock change
 *      - This behavior may be removed in a later release.
 *
 * @see hgmlDeviceGetTemperature
 * @see hgmlDeviceGetTemperatureThreshold
 * @see hgmlDeviceGetPowerUsage
 */
#define hgmlClocksThrottleReasonHwSlowdown                0x0000000000000008LL

/** Sync Boost
 *
 * This GPU has been added to a Sync boost group with ppu-smi or DCGM in
 * order to maximize performance per watt. All GPUs in the sync boost group
 * will boost to the minimum possible clocks across the entire group. Look at
 * the throttle reasons for other GPUs in the system to see why those GPUs are
 * holding this one at lower clocks.
 *
 */
#define hgmlClocksEventReasonSyncBoost                 0x0000000000000010LL

/** SW Thermal Slowdown
 *
 * The current clocks have been optimized to ensure the the following is true:
 *  - Current GPU temperature does not exceed GPU Max Operating Temperature
 *  - Current memory temperature does not exceeed Memory Max Operating Temperature
 *
 */
#define hgmlClocksEventReasonSwThermalSlowdown         0x0000000000000020LL

/** HW Thermal Slowdown (reducing the core clocks by a factor of 2 or more) is engaged
 *
 * This is an indicator of:
 *   - temperature being too high
 *
 * @see hgmlDeviceGetTemperature
 * @see hgmlDeviceGetTemperatureThreshold
 * @see hgmlDeviceGetPowerUsage
 */
#define hgmlClocksThrottleReasonHwThermalSlowdown         0x0000000000000040LL

/** HW Power Brake Slowdown (reducing the core clocks by a factor of 2 or more) is engaged
 *
 * This is an indicator of:
 *   - External Power Brake Assertion being triggered (e.g. by the system power supply)
 *
 * @see hgmlDeviceGetTemperature
 * @see hgmlDeviceGetTemperatureThreshold
 * @see hgmlDeviceGetPowerUsage
 */
#define hgmlClocksThrottleReasonHwPowerBrakeSlowdown      0x0000000000000080LL

/** GPU clocks are limited by current setting of Display clocks
 *
 * @see bug 1997531
 */
#define hgmlClocksEventReasonDisplayClockSetting       0x0000000000000100LL

/** Bit mask representing no clocks throttling
 *
 * Clocks are as high as possible.
 * */
#define hgmlClocksEventReasonNone                      0x0000000000000000LL

/** Bit mask representing all supported clocks throttling reasons
 * New reasons might be added to this list in the future
 */
#define hgmlClocksEventReasonAll (hgmlClocksThrottleReasonNone \
      | hgmlClocksEventReasonGpuIdle                           \
      | hgmlClocksEventReasonApplicationsClocksSetting         \
      | hgmlClocksEventReasonSwPowerCap                        \
      | hgmlClocksThrottleReasonHwSlowdown                        \
      | hgmlClocksEventReasonSyncBoost                         \
      | hgmlClocksEventReasonSwThermalSlowdown                 \
      | hgmlClocksThrottleReasonHwThermalSlowdown                 \
      | hgmlClocksThrottleReasonHwPowerBrakeSlowdown              \
      | hgmlClocksEventReasonDisplayClockSetting               \
)

/**
 * @deprecated Use \ref hgmlClocksEventReasonGpuIdle instead
 */
#define hgmlClocksThrottleReasonGpuIdle                      hgmlClocksEventReasonGpuIdle
/**
 * @deprecated Use \ref hgmlClocksEventReasonApplicationsClocksSetting instead
 */
#define hgmlClocksThrottleReasonApplicationsClocksSetting    hgmlClocksEventReasonApplicationsClocksSetting
/**
 * @deprecated Use \ref hgmlClocksEventReasonSyncBoost instead
 */
#define hgmlClocksThrottleReasonSyncBoost                    hgmlClocksEventReasonSyncBoost
/**
 * @deprecated Use \ref hgmlClocksEventReasonSwPowerCap instead
 */
#define hgmlClocksThrottleReasonSwPowerCap                   hgmlClocksEventReasonSwPowerCap
/**
 * @deprecated Use \ref hgmlClocksEventReasonSwThermalSlowdown instead
 */
#define hgmlClocksThrottleReasonSwThermalSlowdown            hgmlClocksEventReasonSwThermalSlowdown
/**
 * @deprecated Use \ref hgmlClocksEventReasonDisplayClockSetting instead
 */
#define hgmlClocksThrottleReasonDisplayClockSetting          hgmlClocksEventReasonDisplayClockSetting
/**
 * @deprecated Use \ref hgmlClocksEventReasonNone instead
 */
#define hgmlClocksThrottleReasonNone                         hgmlClocksEventReasonNone
/**
 * @deprecated Use \ref hgmlClocksEventReasonAll instead
 */
#define hgmlClocksThrottleReasonAll                          hgmlClocksEventReasonAll
/** @} */

/***************************************************************************************************/
/** @defgroup hgmlAccountingStats Accounting Statistics
 *  @{
 *
 *  Set of APIs designed to provide per process information about usage of GPU.
 *
 *  @note All accounting statistics and accounting mode live in alixpu driver and reset
 *        to default (Disabled) when driver unloads.
 *        It is advised to run with persistence mode enabled.
 *
 *  @note Enabling accounting mode has no negative impact on the GPU performance.
 */
/***************************************************************************************************/

/**
 * Describes accounting statistics of a process.
 */
typedef struct hgmlAccountingStats_st {
    unsigned int gpuUtilization;                //!< Percent of time over the process's lifetime during which one or more kernels was executing on the GPU.
                                                //! Utilization stats just like returned by \ref hgmlDeviceGetUtilizationRates but for the life time of a
                                                //! process (not just the last sample period).
                                                //! Set to HGML_VALUE_NOT_AVAILABLE if hgmlDeviceGetUtilizationRates is not supported

    unsigned int memoryUtilization;             //!< Percent of time over the process's lifetime during which global (device) memory was being read or written.
                                                //! Set to HGML_VALUE_NOT_AVAILABLE if hgmlDeviceGetUtilizationRates is not supported

    unsigned long long maxMemoryUsage;          //!< Maximum total memory in bytes that was ever allocated by the process.
                                                //! Set to HGML_VALUE_NOT_AVAILABLE if hgmlProcessInfo_t->usedGpuMemory is not supported


    unsigned long long time;                    //!< Amount of time in ms during which the compute context was active. The time is reported as 0 if
                                                //!< the process is not terminated

    unsigned long long startTime;               //!< CPU Timestamp in usec representing start time for the process

    unsigned int isRunning;                     //!< Flag to represent if the process is running (1 for running, 0 for terminated)

    unsigned int reserved[5];                   //!< Reserved for future use
} hgmlAccountingStats_t;

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlEncoderStructs Encoder Structs
 *  @{
 */
/***************************************************************************************************/

/**
 * Represents type of encoder for capacity can be queried
 */
typedef enum hgmlEncoderQueryType_enum
{
    HGML_ENCODER_QUERY_H264 = 0,        //!< H264 encoder
    HGML_ENCODER_QUERY_HEVC = 1,        //!< HEVC encoder
    HGML_ENCODER_QUERY_AV1  = 2         //!< AV1 encoder
}hgmlEncoderType_t;

/**
 * Structure to hold encoder session data
 */
typedef struct hgmlEncoderSessionInfo_st
{
    unsigned int       sessionId;       //!< Unique session ID
    unsigned int       pid;             //!< Owning process ID
    hgmlVgpuInstance_t vgpuInstance;    //!< Owning vGPU instance ID (only valid on vGPU hosts, otherwise zero)
    hgmlEncoderType_t  codecType;       //!< Video encoder type
    unsigned int       hResolution;     //!< Current encode horizontal resolution
    unsigned int       vResolution;     //!< Current encode vertical resolution
    unsigned int       averageFps;      //!< Moving average encode frames per second
    unsigned int       averageLatency;  //!< Moving average encode latency in microseconds
}hgmlEncoderSessionInfo_t;

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlFBCStructs Frame Buffer Capture Structures
*  @{
*/
/***************************************************************************************************/

/**
 * Represents frame buffer capture session type
 */
typedef enum hgmlFBCSessionType_enum
{
    HGML_FBC_SESSION_TYPE_UNKNOWN = 0,     //!< Unknown
    HGML_FBC_SESSION_TYPE_TOSYS,           //!< ToSys
    HGML_FBC_SESSION_TYPE_HGGC,            //!< Hggc
    HGML_FBC_SESSION_TYPE_VID,             //!< Vid
    HGML_FBC_SESSION_TYPE_HWENC            //!< HEnc
} hgmlFBCSessionType_t;


/**
 * Structure to hold frame buffer capture sessions stats
 */
typedef struct hgmlFBCStats_st
{
    unsigned int      sessionsCount;    //!< Total no of sessions
    unsigned int      averageFPS;       //!< Moving average new frames captured per second
    unsigned int      averageLatency;   //!< Moving average new frame capture latency in microseconds
} hgmlFBCStats_t;

#define HGML_HGFBC_SESSION_FLAG_DIFFMAP_ENABLED                0x00000001    //!< Bit specifying differential map state.
#define HGML_HGFBC_SESSION_FLAG_CLASSIFICATIONMAP_ENABLED      0x00000002    //!< Bit specifying classification map state.
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_NO_WAIT      0x00000004    //!< Bit specifying if capture was requested as non-blocking call.
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_INFINITE     0x00000008    //!< Bit specifying if capture was requested as blocking call.
#define HGML_HGFBC_SESSION_FLAG_CAPTURE_WITH_WAIT_TIMEOUT      0x00000010    //!< Bit specifying if capture was requested as blocking call with timeout period.

/**
 * Structure to hold FBC session data
 */
typedef struct hgmlFBCSessionInfo_st
{
    unsigned int          sessionId;                           //!< Unique session ID
    unsigned int          pid;                                 //!< Owning process ID
    hgmlVgpuInstance_t    vgpuInstance;                        //!< Owning vGPU instance ID (only valid on vGPU hosts, otherwise zero)
    unsigned int          displayOrdinal;                      //!< Display identifier
    hgmlFBCSessionType_t  sessionType;                         //!< Type of frame buffer capture session
    unsigned int          sessionFlags;                        //!< Session flags (one or more of HGML_HGFBC_SESSION_FLAG_XXX).
    unsigned int          hMaxResolution;                      //!< Max horizontal resolution supported by the capture session
    unsigned int          vMaxResolution;                      //!< Max vertical resolution supported by the capture session
    unsigned int          hResolution;                         //!< Horizontal resolution requested by caller in capture call
    unsigned int          vResolution;                         //!< Vertical resolution requested by caller in capture call
    unsigned int          averageFPS;                          //!< Moving average new frames captured per second
    unsigned int          averageLatency;                      //!< Moving average new frame capture latency in microseconds
} hgmlFBCSessionInfo_t;

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlDrainDefs definitions related to the drain state
 *  @{
 */
/***************************************************************************************************/

/**
 *  Is the GPU device to be removed from the kernel by hgmlDeviceRemoveGpu()
 */
typedef enum hgmlDetachGpuState_enum
{
    HGML_DETACH_GPU_KEEP         = 0,
    HGML_DETACH_GPU_REMOVE
} hgmlDetachGpuState_t;

/**
 *  Parent bridge PCIe link state requested by hgmlDeviceRemoveGpu()
 */
typedef enum hgmlPcieLinkState_enum
{
    HGML_PCIE_LINK_KEEP         = 0,
    HGML_PCIE_LINK_SHUT_DOWN
} hgmlPcieLinkState_t;

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlSystem/hgmlDevice definitions related to Confidential Computing
 *  @{
 */
/***************************************************************************************************/
/**
 * Confidential Compute CPU Capabilities values
 */
#define HGML_CC_SYSTEM_CPU_CAPS_NONE      0
#define HGML_CC_SYSTEM_CPU_CAPS_AMD_SEV   1
#define HGML_CC_SYSTEM_CPU_CAPS_INTEL_TDX 2

/**
 * Confidenial Compute GPU Capabilities values
 */
#define HGML_CC_SYSTEM_GPUS_CC_NOT_CAPABLE 0
#define HGML_CC_SYSTEM_GPUS_CC_CAPABLE     1

typedef struct hgmlConfComputeSystemCaps_st {
    unsigned int cpuCaps;
    unsigned int gpusCaps;
} hgmlConfComputeSystemCaps_t;

/**
 * Confidential Compute DevTools Mode values
 */
#define HGML_CC_SYSTEM_DEVTOOLS_MODE_OFF 0
#define HGML_CC_SYSTEM_DEVTOOLS_MODE_ON  1

/**
 * Confidential Compute Environment values
 */
#define HGML_CC_SYSTEM_ENVIRONMENT_UNAVAILABLE 0
#define HGML_CC_SYSTEM_ENVIRONMENT_SIM         1
#define HGML_CC_SYSTEM_ENVIRONMENT_PROD        2

/**
 * Confidential Compute Feature Status values
 */
#define HGML_CC_SYSTEM_FEATURE_DISABLED 0
#define HGML_CC_SYSTEM_FEATURE_ENABLED  1

typedef struct hgmlConfComputeSystemState_st {
    unsigned int environment;
    unsigned int ccFeature;
    unsigned int devToolsMode;
} hgmlConfComputeSystemState_t;

/**
 * Protected memory size
 */
typedef struct
hgmlConfComputeMemSizeInfo_st
{
    unsigned long long protectedMemSizeKib;
    unsigned long long unprotectedMemSizeKib;
} hgmlConfComputeMemSizeInfo_t;

/**
 * Confidential Compute GPUs/System Ready State values
 */
#define HGML_CC_ACCEPTING_CLIENT_REQUESTS_FALSE 0
#define HGML_CC_ACCEPTING_CLIENT_REQUESTS_TRUE  1

/**
 * GPU Certificate Details
 */
#define HGML_GPU_CERT_CHAIN_SIZE 0x1000
#define HGML_GPU_ATTESTATION_CERT_CHAIN_SIZE 0x1400

typedef struct hgmlConfComputeGpuCertificate_st {
    unsigned int certChainSize;
    unsigned int attestationCertChainSize;
    unsigned char certChain[HGML_GPU_CERT_CHAIN_SIZE];
    unsigned char attestationCertChain[HGML_GPU_ATTESTATION_CERT_CHAIN_SIZE];
} hgmlConfComputeGpuCertificate_t;

/**
 * GPU Attestation Report
 */
#define HGML_CC_GPU_CEC_NONCE_SIZE 0x20
#define HGML_CC_GPU_ATTESTATION_REPORT_SIZE 0x2000
#define HGML_CC_GPU_CEC_ATTESTATION_REPORT_SIZE 0x1000
#define HGML_CC_CEC_ATTESTATION_REPORT_NOT_PRESENT 0
#define HGML_CC_CEC_ATTESTATION_REPORT_PRESENT 1

typedef struct hgmlConfComputeGpuAttestationReport_st {
    unsigned int isCecAttestationReportPresent;
    unsigned int attestationReportSize;
    unsigned int cecAttestationReportSize;
    unsigned char nonce[HGML_CC_GPU_CEC_NONCE_SIZE];
    unsigned char attestationReport[HGML_CC_GPU_ATTESTATION_REPORT_SIZE];
    unsigned char cecAttestationReport[HGML_CC_GPU_CEC_ATTESTATION_REPORT_SIZE];
} hgmlConfComputeGpuAttestationReport_t;

/** @} */

#define HGML_GPU_FABRIC_UUID_LEN 16

#define HGML_GPU_FABRIC_STATE_NOT_SUPPORTED 0
#define HGML_GPU_FABRIC_STATE_NOT_STARTED   1
#define HGML_GPU_FABRIC_STATE_IN_PROGRESS   2
#define HGML_GPU_FABRIC_STATE_COMPLETED     3

typedef unsigned char hgmlGpuFabricState_t;

typedef struct {
    unsigned char        clusterUuid[HGML_GPU_FABRIC_UUID_LEN]; //!< Uuid of the cluster to which this GPU belongs
    hgmlReturn_t         status;                                //!< Error status, if any. Must be checked only if state returns "complete".
    unsigned int         cliqueId;                              //!< ID of the fabric clique to which this GPU belongs
    hgmlGpuFabricState_t state;                                 //!< Current state of GPU registration process
} hgmlGpuFabricInfo_t;

/**
 * Device Scope - This is useful to retrieve the telemetry at GPU and module (e.g. GPU + CPU) level
 */
#define HGML_POWER_SCOPE_GPU     0U    //!< Targets only GPU
#define HGML_POWER_SCOPE_MODULE  1U    //!< Targets the whole module
#define HGML_POWER_SCOPE_MEMORY  2U    //!< Targets the GPU Memory

typedef unsigned char hgmlPowerScopeType_t;

typedef struct
{
    unsigned int         version;       //!< Structure format version (must be hgmlPowerValue_v2)
    hgmlPowerScopeType_t powerScope;    //!< [in]  Device type: GPU or Total Module
    unsigned int         powerValueMw;  //!< [out] Power value to retrieve or set in milliwatts
} hgmlPowerValue_v2_t;

#define hgmlPowerValue_v2 HGML_STRUCT_VERSION(PowerValue, 2)

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlInitializationAndCleanup Initialization and Cleanup
 * This chapter describes the methods that handle HGML initialization and cleanup.
 * It is the user's responsibility to call \ref hgmlInit_v2() before calling any other methods, and
 * hgmlShutdown() once HGML is no longer being used.
 *  @{
 */
/***************************************************************************************************/

#define HGML_INIT_FLAG_NO_GPUS      1   //!< Don't fail hgmlInit() when no GPUs are found
#define HGML_INIT_FLAG_NO_ATTACH    2   //!< Don't attach GPUs

/**
 * Initialize HGML, but don't initialize any GPUs yet.
 *
 * \note hgmlInit_v3 introduces a "flags" argument, that allows passing boolean values
 *       modifying the behaviour of hgmlInit().
 * \note In HGML 5.319 new hgmlInit_v2 has replaced hgmlInit"_v1" (default in HGML 4.304 and older) that
 *       did initialize all GPU devices in the system.
 *
 * This allows HGML to communicate with a GPU
 * when other GPUs in the system are unstable or in a bad state.  When using this API, GPUs are
 * discovered and initialized in hgmlDeviceGetHandleBy* functions instead.
 *
 * \note To contrast hgmlInit_v2 with hgmlInit"_v1", HGML 4.304 hgmlInit"_v1" will fail when any detected GPU is in
 *       a bad or unstable state.
 *
 * For all products.
 *
 * This method, should be called once before invoking any other methods in the library.
 * A reference count of the number of initializations is maintained.  Shutdown only occurs
 * when the reference count reaches zero.
 *
 * @return
 *         - \ref HGML_SUCCESS                   if HGML has been properly initialized
 *         - \ref HGML_ERROR_DRIVER_NOT_LOADED   if driver is not running
 *         - \ref HGML_ERROR_NO_PERMISSION       if HGML does not have permission to talk to the driver
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlInit_v2(void);

/**
 * hgmlInitWithFlags is a variant of hgmlInit(), that allows passing a set of boolean values
 *       modifying the behaviour of hgmlInit().
 *       Other than the "flags" parameter it is completely similar to \ref hgmlInit_v2.
 *
 * For all products.
 *
 * @param flags                                 behaviour modifier flags
 *
 * @return
 *         - \ref HGML_SUCCESS                   if HGML has been properly initialized
 *         - \ref HGML_ERROR_DRIVER_NOT_LOADED   if driver is not running
 *         - \ref HGML_ERROR_NO_PERMISSION       if HGML does not have permission to talk to the driver
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlInitWithFlags(unsigned int flags);

/**
 * Shut down HGML by releasing all GPU resources previously allocated with \ref hgmlInit_v2().
 *
 * For all products.
 *
 * This method should be called after HGML work is done, once for each call to \ref hgmlInit_v2()
 * A reference count of the number of initializations is maintained.  Shutdown only occurs
 * when the reference count reaches zero.  For backwards compatibility, no error is reported if
 * hgmlShutdown() is called more times than hgmlInit().
 *
 * @return
 *         - \ref HGML_SUCCESS                 if HGML has been properly shut down
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlShutdown(void);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlErrorReporting Error reporting
 * This chapter describes helper functions for error reporting routines.
 *  @{
 */
/***************************************************************************************************/

/**
 * Helper method for converting HGML error codes into readable strings.
 *
 * For all products.
 *
 * @param result                               HGML error code to convert
 *
 * @return String representation of the error.
 *
 */
const char* hgmlErrorString(hgmlReturn_t result);
/** @} */


/***************************************************************************************************/
/** @defgroup hgmlConstants Constants
 *  @{
 */
/***************************************************************************************************/

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetInforomVersion and \ref hgmlDeviceGetInforomImageVersion
 */
#define HGML_DEVICE_INFOROM_VERSION_BUFFER_SIZE       16

/**
 * Buffer size guaranteed to be large enough for storing GPU identifiers.
 */
#define HGML_DEVICE_UUID_BUFFER_SIZE                  80

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetUUID
 */
#define HGML_DEVICE_UUID_V2_BUFFER_SIZE               96

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetBoardPartNumber
 */
#define HGML_DEVICE_PART_NUMBER_BUFFER_SIZE           80

/**
 * Buffer size guaranteed to be large enough for \ref hgmlSystemGetDriverVersion
 */
#define HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE        80

/**
 * Buffer size guaranteed to be large enough for \ref hgmlSystemGetHGMLVersion
 */
#define HGML_SYSTEM_HGML_VERSION_BUFFER_SIZE          80

/**
 * Buffer size guaranteed to be large enough for storing GPU device names.
 */
#define HGML_DEVICE_NAME_BUFFER_SIZE                  64

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetName
 */
#define HGML_DEVICE_NAME_V2_BUFFER_SIZE               96

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetSerial
 */
#define HGML_DEVICE_SERIAL_BUFFER_SIZE                30

/**
 * Buffer size guaranteed to be large enough for \ref hgmlDeviceGetVbiosVersion
 */
#define HGML_DEVICE_VBIOS_VERSION_BUFFER_SIZE         32

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlSystemQueries System Queries
 * This chapter describes the queries that HGML can perform against the local system. These queries
 * are not device-specific.
 *  @{
 */
/***************************************************************************************************/

/**
 * Retrieves the version of the system's graphics driver.
 *
 * For all products.
 *
 * The version identifier is an alphanumeric string.  It will not exceed 80 characters in length
 * (including the NULL terminator).  See \ref hgmlConstants::HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE.
 *
 * @param version                              Reference in which to return the version identifier
 * @param length                               The maximum allowed length of the string returned in \a version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a version is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 */
hgmlReturn_t hgmlSystemGetDriverVersion(char *version, unsigned int length);

/**
 * Retrieves the version of the HGML library.
 *
 * For all products.
 *
 * The version identifier is an alphanumeric string.  It will not exceed 80 characters in length
 * (including the NULL terminator).  See \ref hgmlConstants::HGML_SYSTEM_HGML_VERSION_BUFFER_SIZE.
 *
 * @param version                              Reference in which to return the version identifier
 * @param length                               The maximum allowed length of the string returned in \a version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a version is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 */
hgmlReturn_t hgmlSystemGetHGMLVersion(char *version, unsigned int length);

/**
 * Retrieves the version of the HGGC driver.
 *
 * For all products.
 *
 * The HGGC driver version returned will be retreived from the currently installed version of HGGC.
 * If the hggc library is not found, this function will return a known supported version number.
 *
 * @param hggcDriverVersion                    Reference in which to return the version identifier
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a hggcDriverVersion has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a hggcDriverVersion is NULL
 */
hgmlReturn_t hgmlSystemGetHggcDriverVersion(int *hggcDriverVersion);

/**
 * Retrieves the version of the HGGC driver from the shared library.
 *
 * For all products.
 *
 * The returned HGGC driver version by calling cuDriverGetVersion()
 *
 * @param hggcDriverVersion                    Reference in which to return the version identifier
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a hggcDriverVersion has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a hggcDriverVersion is NULL
 *         - \ref HGML_ERROR_LIBRARY_NOT_FOUND  if \a libhggc.so.1 or libhggc.dll is not found
 *         - \ref HGML_ERROR_FUNCTION_NOT_FOUND if \a cuDriverGetVersion() is not found in the shared library
 */
hgmlReturn_t hgmlSystemGetHggcDriverVersion_v2(int *hggcDriverVersion);

/**
 * Macros for converting the HGGC driver version number to Major and Minor version numbers.
 */
#define HGML_HGGC_DRIVER_VERSION_MAJOR(v) ((v)/1000)
#define HGML_HGGC_DRIVER_VERSION_MINOR(v) (((v)%1000)/10)

/**
 * Gets name of the process with provided process id
 *
 * For all products.
 *
 * Returned process name is cropped to provided length.
 * name string is encoded in ANSI.
 *
 * @param pid                                  The identifier of the process
 * @param name                                 Reference in which to return the process name
 * @param length                               The maximum allowed length of the string returned in \a name
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a name has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a name is NULL or \a length is 0.
 *         - \ref HGML_ERROR_NOT_FOUND         if process doesn't exists
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlSystemGetProcessName(unsigned int pid, char *name, unsigned int length);

/**
 * Retrieves the IDs and firmware versions for any Host Interface Cards (HICs) in the system.
 *
 * For S-class products.
 *
 * The \a hwbcCount argument is expected to be set to the size of the input \a hwbcEntries array.
 * The HIC must be connected to an S-class system for it to be reported by this function.
 *
 * @param hwbcCount                            Size of hwbcEntries array
 * @param hwbcEntries                          Array holding information about hwbc
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a hwbcCount and \a hwbcEntries have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if either \a hwbcCount or \a hwbcEntries is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a hwbcCount indicates that the \a hwbcEntries array is too small
 */
hgmlReturn_t hgmlSystemGetHicVersion(unsigned int *hwbcCount, hgmlHwbcEntry_t *hwbcEntries);

/**
 * Retrieve the set of GPUs that have a CPU affinity with the given CPU number
 * For all products.
 * Supported on Linux only.
 *
 * @param cpuNumber                            The CPU number
 * @param count                                When zero, is set to the number of matching GPUs such that \a deviceArray
 *                                             can be malloc'd.  When non-zero, \a deviceArray will be filled with \a count
 *                                             number of device handles.
 * @param deviceArray                          An array of device handles for GPUs found with affinity to \a cpuNumber
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a deviceArray or \a count (if initially zero) has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a cpuNumber, or \a count is invalid, or \a deviceArray is NULL with a non-zero \a count
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device or OS does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           an error has occurred in underlying topology discovery
 */
hgmlReturn_t hgmlSystemGetTopologyGpuSet(unsigned int cpuNumber, unsigned int *count, hgmlDevice_t *deviceArray);


/** @} */

/***************************************************************************************************/
/** @defgroup hgmlUnitQueries Unit Queries
 * This chapter describes that queries that HGML can perform against each unit. For S-class systems only.
 * In each case the device is identified with an hgmlUnit_t handle. This handle is obtained by
 * calling \ref hgmlUnitGetHandleByIndex().
 *  @{
 */
/***************************************************************************************************/

 /**
 * Retrieves the number of units in the system.
 *
 * For S-class products.
 *
 * @param unitCount                            Reference in which to return the number of units
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a unitCount has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unitCount is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetCount(unsigned int *unitCount);

/**
 * Acquire the handle for a particular unit, based on its index.
 *
 * For S-class products.
 *
 * Valid indices are derived from the \a unitCount returned by \ref hgmlUnitGetCount().
 *   For example, if \a unitCount is 2 the valid indices are 0 and 1, corresponding to UNIT 0 and UNIT 1.
 *
 * The order in which HGML enumerates units has no guarantees of consistency between reboots.
 *
 * @param index                                The index of the target unit, >= 0 and < \a unitCount
 * @param unit                                 Reference in which to return the unit handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a unit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a index is invalid or \a unit is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetHandleByIndex(unsigned int index, hgmlUnit_t *unit);

/**
 * Retrieves the static information associated with a unit.
 *
 * For S-class products.
 *
 * See \ref hgmlUnitInfo_t for details on available unit info.
 *
 * @param unit                                 The identifier of the target unit
 * @param info                                 Reference in which to return the unit information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a info has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit is invalid or \a info is NULL
 */
hgmlReturn_t hgmlUnitGetUnitInfo(hgmlUnit_t unit, hgmlUnitInfo_t *info);

/**
 * Retrieves the LED state associated with this unit.
 *
 * For S-class products.
 *
 * See \ref hgmlLedState_t for details on allowed states.
 *
 * @param unit                                 The identifier of the target unit
 * @param state                                Reference in which to return the current LED state
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a state has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit is invalid or \a state is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this is not an S-class product
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlUnitSetLedState()
 */
hgmlReturn_t hgmlUnitGetLedState(hgmlUnit_t unit, hgmlLedState_t *state);

/**
 * Retrieves the PSU stats for the unit.
 *
 * For S-class products.
 *
 * See \ref hgmlPSUInfo_t for details on available PSU info.
 *
 * @param unit                                 The identifier of the target unit
 * @param psu                                  Reference in which to return the PSU information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a psu has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit is invalid or \a psu is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this is not an S-class product
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetPsuInfo(hgmlUnit_t unit, hgmlPSUInfo_t *psu);

/**
 * Retrieves the temperature readings for the unit, in degrees C.
 *
 * For S-class products.
 *
 * Depending on the product, readings may be available for intake (type=0),
 * exhaust (type=1) and board (type=2).
 *
 * @param unit                                 The identifier of the target unit
 * @param type                                 The type of reading to take
 * @param temp                                 Reference in which to return the intake temperature
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a temp has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit or \a type is invalid or \a temp is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this is not an S-class product
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetTemperature(hgmlUnit_t unit, unsigned int type, unsigned int *temp);

/**
 * Retrieves the fan speed readings for the unit.
 *
 * For S-class products.
 *
 * See \ref hgmlUnitFanSpeeds_t for details on available fan speed info.
 *
 * @param unit                                 The identifier of the target unit
 * @param fanSpeeds                            Reference in which to return the fan speed information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a fanSpeeds has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit is invalid or \a fanSpeeds is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this is not an S-class product
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetFanSpeedInfo(hgmlUnit_t unit, hgmlUnitFanSpeeds_t *fanSpeeds);

/**
 * Retrieves the set of GPU devices that are attached to the specified unit.
 *
 * For S-class products.
 *
 * The \a deviceCount argument is expected to be set to the size of the input \a devices array.
 *
 * @param unit                                 The identifier of the target unit
 * @param deviceCount                          Reference in which to provide the \a devices array size, and
 *                                             to return the number of attached GPU devices
 * @param devices                              Reference in which to return the references to the attached GPU devices
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a deviceCount and \a devices have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a deviceCount indicates that the \a devices array is too small
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit is invalid, either of \a deviceCount or \a devices is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlUnitGetDevices(hgmlUnit_t unit, unsigned int *deviceCount, hgmlDevice_t *devices);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlDeviceQueries Device Queries
 * This chapter describes that queries that HGML can perform against each device.
 * In each case the device is identified with an hgmlDevice_t handle. This handle is obtained by
 * calling one of \ref hgmlDeviceGetHandleByIndex_v2(), \ref hgmlDeviceGetHandleBySerial(),
 * \ref hgmlDeviceGetHandleByPciBusId_v2(). or \ref hgmlDeviceGetHandleByUUID().
 *  @{
 */
/***************************************************************************************************/

 /**
 * Retrieves the number of compute devices in the system. A compute device is a single GPU.
 *
 * For all products.
 *
 * Note: New hgmlDeviceGetCount_v2 (default in HGML 5.319) returns count of all devices in the system
 *       even if hgmlDeviceGetHandleByIndex_v2 returns HGML_ERROR_NO_PERMISSION for such device.
 *       Update your code to handle this error, or use HGML 4.304 or older hgml header file.
 *       For backward binary compatibility reasons _v1 version of the API is still present in the shared
 *       library.
 *       Old _v1 version of hgmlDeviceGetCount doesn't count devices that HGML has no permission to talk to.
 *
 * @param deviceCount                          Reference in which to return the number of accessible devices
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a deviceCount has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a deviceCount is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetCount_v2(unsigned int *deviceCount);

/**
 * Get attributes (engine counts etc.) for the given HGML device handle.
 *
 * @note This API currently only supports MIG device handles.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               HGML device handle
 * @param attributes                           Device attributes
 *
 * @return
 *        - \ref HGML_SUCCESS                  if \a device attributes were successfully retrieved
 *        - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device handle is invalid
 *        - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *        - \ref HGML_ERROR_NOT_SUPPORTED      if this query is not supported by the device
 *        - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetAttributes_v2(hgmlDevice_t device, hgmlDeviceAttributes_t *attributes);

/**
 * Acquire the handle for a particular device, based on its index.
 *
 * For all products.
 *
 * Valid indices are derived from the \a accessibleDevices count returned by
 *   \ref hgmlDeviceGetCount_v2(). For example, if \a accessibleDevices is 2 the valid indices
 *   are 0 and 1, corresponding to GPU 0 and GPU 1.
 *
 * The order in which HGML enumerates devices has no guarantees of consistency between reboots. For that reason it
 *   is recommended that devices be looked up by their PCI ids or UUID. See
 *   \ref hgmlDeviceGetHandleByUUID() and \ref hgmlDeviceGetHandleByPciBusId_v2().
 *
 * Note: The HGML index may not correlate with other APIs, such as the HGGC device index.
 *
 * Starting from HGML 5, this API causes HGML to initialize the target GPU
 * HGML may initialize additional GPUs if:
 *  - The target GPU is an SLI slave
 *
 * Note: New hgmlDeviceGetCount_v2 (default in HGML 5.319) returns count of all devices in the system
 *       even if hgmlDeviceGetHandleByIndex_v2 returns HGML_ERROR_NO_PERMISSION for such device.
 *       Update your code to handle this error, or use HGML 4.304 or older hgml header file.
 *       For backward binary compatibility reasons _v1 version of the API is still present in the shared
 *       library.
 *       Old _v1 version of hgmlDeviceGetCount doesn't count devices that HGML has no permission to talk to.
 *
 *       This means that hgmlDeviceGetHandleByIndex_v2 and _v1 can return different devices for the same index.
 *       If you don't touch macros that map old (_v1) versions to _v2 versions at the top of the file you don't
 *       need to worry about that.
 *
 * @param index                                The index of the target GPU, >= 0 and < \a accessibleDevices
 * @param device                               Reference in which to return the device handle
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a device has been set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a index is invalid or \a device is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_POWER if any attached devices have improperly attached external power cables
 *         - \ref HGML_ERROR_NO_PERMISSION      if the user doesn't have permission to talk to this device
 *         - \ref HGML_ERROR_IRQ_ISSUE          if kernel detected an interrupt issue with the attached GPUs
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 *
 * @see hgmlDeviceGetIndex
 * @see hgmlDeviceGetCount
 */
hgmlReturn_t hgmlDeviceGetHandleByIndex_v2(unsigned int index, hgmlDevice_t *device);

/**
 * Acquire the handle for a particular device, based on its board serial number.
 *
 * For &tm; or newer fully supported devices.
 *
 * This number corresponds to the value printed directly on the board, and to the value returned by
 *   \ref hgmlDeviceGetSerial().
 *
 * @deprecated Since more than one GPU can exist on a single board this function is deprecated in favor
 *             of \ref hgmlDeviceGetHandleByUUID.
 *             For dual GPU boards this function will return HGML_ERROR_INVALID_ARGUMENT.
 *
 * Starting from HGML 5, this API causes HGML to initialize the target GPU
 * HGML may initialize additional GPUs as it searches for the target GPU
 *
 * @param serial                               The board serial number of the target GPU
 * @param device                               Reference in which to return the device handle
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a device has been set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a serial is invalid, \a device is NULL or more than one
 *                                              device has the same serial (dual GPU boards)
 *         - \ref HGML_ERROR_NOT_FOUND          if \a serial does not match a valid device on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_POWER if any attached devices have improperly attached external power cables
 *         - \ref HGML_ERROR_IRQ_ISSUE          if kernel detected an interrupt issue with the attached GPUs
 *         - \ref HGML_ERROR_GPU_IS_LOST        if any GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 *
 * @see hgmlDeviceGetSerial
 * @see hgmlDeviceGetHandleByUUID
 */
hgmlReturn_t hgmlDeviceGetHandleBySerial(const char *serial, hgmlDevice_t *device);

/**
 * Acquire the handle for a particular device, based on its globally unique immutable UUID associated with each device.
 *
 * For all products.
 *
 * @param uuid                                 The UUID of the target GPU or MIG instance
 * @param device                               Reference in which to return the device handle or MIG device handle
 *
 * Starting from HGML 5, this API causes HGML to initialize the target GPU
 * HGML may initialize additional GPUs as it searches for the target GPU
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a device has been set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a uuid is invalid or \a device is null
 *         - \ref HGML_ERROR_NOT_FOUND          if \a uuid does not match a valid device on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_POWER if any attached devices have improperly attached external power cables
 *         - \ref HGML_ERROR_IRQ_ISSUE          if kernel detected an interrupt issue with the attached GPUs
 *         - \ref HGML_ERROR_GPU_IS_LOST        if any GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 *
 * @see hgmlDeviceGetUUID
 */
hgmlReturn_t hgmlDeviceGetHandleByUUID(const char *uuid, hgmlDevice_t *device);

/**
 * Acquire the handle for a particular device, based on its PCI bus id.
 *
 * For all products.
 *
 * This value corresponds to the hgmlPciInfo_t::busId returned by \ref hgmlDeviceGetPciInfo_v3().
 *
 * Starting from HGML 5, this API causes HGML to initialize the target GPU
 * HGML may initialize additional GPUs if:
 *  - The target GPU is an SLI slave
 *
 * \note HGML 4.304 and older version of hgmlDeviceGetHandleByPciBusId"_v1" returns HGML_ERROR_NOT_FOUND
 *       instead of HGML_ERROR_NO_PERMISSION.
 *
 * @param pciBusId                             The PCI bus id of the target GPU
 * @param device                               Reference in which to return the device handle
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a device has been set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a pciBusId is invalid or \a device is NULL
 *         - \ref HGML_ERROR_NOT_FOUND          if \a pciBusId does not match a valid device on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_POWER if the attached device has improperly attached external power cables
 *         - \ref HGML_ERROR_NO_PERMISSION      if the user doesn't have permission to talk to this device
 *         - \ref HGML_ERROR_IRQ_ISSUE          if kernel detected an interrupt issue with the attached GPUs
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetHandleByPciBusId_v2(const char *pciBusId, hgmlDevice_t *device);

/**
 * Retrieves the name of this device.
 *
 * For all products.
 *
 * The name is an alphanumeric string that denotes a particular product, e.g. &tm; It will not
 * exceed 96 characters in length (including the NULL terminator).  See \ref
 * hgmlConstants::HGML_DEVICE_NAME_V2_BUFFER_SIZE.
 *
 * When used with MIG device handles the API returns MIG device names which can be used to identify devices
 * based on their attributes.
 *
 * @param device                               The identifier of the target device
 * @param name                                 Reference in which to return the product name
 * @param length                               The maximum allowed length of the string returned in \a name
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a name has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a name is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetName(hgmlDevice_t device, char *name, unsigned int length);

/**
 * Retrieves the brand of this device.
 *
 * For all products.
 *
 * The type is a member of \ref hgmlBrandType_t defined above.
 *
 * @param device                               The identifier of the target device
 * @param type                                 Reference in which to return the product brand type
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a name has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a type is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetBrand(hgmlDevice_t device, hgmlBrandType_t *type);

/**
 * Retrieves the HGML index of this device.
 *
 * For all products.
 *
 * Valid indices are derived from the \a accessibleDevices count returned by
 *   \ref hgmlDeviceGetCount_v2(). For example, if \a accessibleDevices is 2 the valid indices
 *   are 0 and 1, corresponding to GPU 0 and GPU 1.
 *
 * The order in which HGML enumerates devices has no guarantees of consistency between reboots. For that reason it
 *   is recommended that devices be looked up by their PCI ids or GPU UUID. See
 *   \ref hgmlDeviceGetHandleByPciBusId_v2() and \ref hgmlDeviceGetHandleByUUID().
 *
 * When used with MIG device handles this API returns indices that can be
 * passed to \ref hgmlDeviceGetMigDeviceHandleByIndex to retrieve an identical handle.
 * MIG device indices are unique within a device.
 *
 * Note: The HGML index may not correlate with other APIs, such as the HGGC device index.
 *
 * @param device                               The identifier of the target device
 * @param index                                Reference in which to return the HGML index of the device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a index has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a index is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetHandleByIndex()
 * @see hgmlDeviceGetCount()
 */
hgmlReturn_t hgmlDeviceGetIndex(hgmlDevice_t device, unsigned int *index);

/**
 * Retrieves the globally unique board serial number associated with this device's board.
 *
 * For all products with an inforom.
 *
 * The serial number is an alphanumeric string that will not exceed 30 characters (including the NULL terminator).
 * This number matches the serial number tag that is physically attached to the board.  See \ref
 * hgmlConstants::HGML_DEVICE_SERIAL_BUFFER_SIZE.
 *
 * @param device                               The identifier of the target device
 * @param serial                               Reference in which to return the board/module serial number
 * @param length                               The maximum allowed length of the string returned in \a serial
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a serial has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a serial is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetSerial(hgmlDevice_t device, char *serial, unsigned int length);

/*
* Get a unique identifier for the device module on the baseboard
*
* This API retrieves a unique identifier for each GPU module that exists on a given baseboard.
* For non-baseboard products, this ID would always be 0.
*
* @param device                               The identifier of the target device
* @param moduleId                             Unique identifier for the GPU module
*
* @return
*         - \ref HGML_SUCCESS                 if \a moduleId has been successfully retrieved
*         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a moduleId is invalid
*         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
*/
hgmlReturn_t hgmlDeviceGetModuleId(hgmlDevice_t device, unsigned int *moduleId);

/**
 * Retrieves the Device's C2C Mode information
 *
 * @param device                               The identifier of the target device
 * @param c2cModeInfo                          Output struct containing the device's C2C Mode info
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a C2C Mode Infor query is successful
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a serial is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetC2cModeInfoV(hgmlDevice_t device, hgmlC2cModeInfo_v1_t *c2cModeInfo);

/***************************************************************************************************/

/** @defgroup hgmlAffinity CPU and Memory Affinity
 *  This chapter describes HGML operations that are associated with CPU and memory
 *  affinity.
 *  @{
 */
/***************************************************************************************************/

//! Scope of NUMA node for affinity queries
#define HGML_AFFINITY_SCOPE_NODE     0
//! Scope of processor socket for affinity queries
#define HGML_AFFINITY_SCOPE_SOCKET   1

typedef unsigned int hgmlAffinityScope_t;

/**
 * Retrieves an array of unsigned ints (sized to nodeSetSize) of bitmasks with
 * the ideal memory affinity within node or socket for the device.
 * For example, if NUMA node 0, 1 are ideal within the socket for the device and nodeSetSize ==  1,
 *     result[0] = 0x3
 *
 * \note If requested scope is not applicable to the target topology, the API
 *       will fall back to reporting the memory affinity for the immediate non-I/O
 *       ancestor of the device.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 * @param nodeSetSize                          The size of the nodeSet array that is safe to access
 * @param nodeSet                              Array reference in which to return a bitmask of NODEs, 64 NODEs per
 *                                             unsigned long on 64-bit machines, 32 on 32-bit machines
 * @param scope                                Scope that change the default behavior
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a NUMA node Affinity has been filled
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, nodeSetSize == 0, nodeSet is NULL or scope is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */

hgmlReturn_t hgmlDeviceGetMemoryAffinity(hgmlDevice_t device, unsigned int nodeSetSize, unsigned long *nodeSet, hgmlAffinityScope_t scope);

/**
 * Retrieves an array of unsigned ints (sized to cpuSetSize) of bitmasks with the
 * ideal CPU affinity within node or socket for the device.
 * For example, if processors 0, 1, 32, and 33 are ideal for the device and cpuSetSize == 2,
 *     result[0] = 0x3, result[1] = 0x3
 *
 * \note If requested scope is not applicable to the target topology, the API
 *       will fall back to reporting the CPU affinity for the immediate non-I/O
 *       ancestor of the device.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 * @param cpuSetSize                           The size of the cpuSet array that is safe to access
 * @param cpuSet                               Array reference in which to return a bitmask of CPUs, 64 CPUs per
 *                                                 unsigned long on 64-bit machines, 32 on 32-bit machines
 * @param scope                                Scope that change the default behavior
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a cpuAffinity has been filled
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, cpuSetSize == 0, cpuSet is NULL or sope is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */

hgmlReturn_t hgmlDeviceGetCpuAffinityWithinScope(hgmlDevice_t device, unsigned int cpuSetSize, unsigned long *cpuSet, hgmlAffinityScope_t scope);

/**
 * Retrieves an array of unsigned ints (sized to cpuSetSize) of bitmasks with the ideal CPU affinity for the device
 * For example, if processors 0, 1, 32, and 33 are ideal for the device and cpuSetSize == 2,
 *     result[0] = 0x3, result[1] = 0x3
 * This is equivalent to calling \ref hgmlDeviceGetCpuAffinityWithinScope with \ref HGML_AFFINITY_SCOPE_NODE.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 * @param cpuSetSize                           The size of the cpuSet array that is safe to access
 * @param cpuSet                               Array reference in which to return a bitmask of CPUs, 64 CPUs per
 *                                                 unsigned long on 64-bit machines, 32 on 32-bit machines
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a cpuAffinity has been filled
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, cpuSetSize == 0, or cpuSet is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetCpuAffinity(hgmlDevice_t device, unsigned int cpuSetSize, unsigned long *cpuSet);

/**
 * Sets the ideal affinity for the calling thread and device using the guidelines
 * given in hgmlDeviceGetCpuAffinity().  Note, this is a change as of version 8.0.
 * Older versions set the affinity for a calling process and all children.
 * Currently supports up to 1024 processors.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the calling process has been successfully bound
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetCpuAffinity(hgmlDevice_t device);

/**
 * Clear all affinity bindings for the calling thread.  Note, this is a change as of version
 * 8.0 as older versions cleared the affinity for a calling process and all children.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the calling process has been successfully unbound
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceClearCpuAffinity(hgmlDevice_t device);

/**
 * Retrieve the common ancestor for two devices
 * For all products.
 * Supported on Linux only.
 *
 * @param device1                              The identifier of the first device
 * @param device2                              The identifier of the second device
 * @param pathInfo                             A \ref hgmlGpuTopologyLevel_t that gives the path type
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pathInfo has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device1, or \a device2 is invalid, or \a pathInfo is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device or OS does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           an error has occurred in underlying topology discovery
 */

/** @} */
hgmlReturn_t hgmlDeviceGetTopologyCommonAncestor(hgmlDevice_t device1, hgmlDevice_t device2, hgmlGpuTopologyLevel_t *pathInfo);

/**
 * Retrieve the set of GPUs that are nearest to a given device at a specific interconnectivity level
 * For all products.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the first device
 * @param level                                The \ref hgmlGpuTopologyLevel_t level to search for other GPUs
 * @param count                                When zero, is set to the number of matching GPUs such that \a deviceArray
 *                                             can be malloc'd.  When non-zero, \a deviceArray will be filled with \a count
 *                                             number of device handles.
 * @param deviceArray                          An array of device handles for GPUs found at \a level
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a deviceArray or \a count (if initially zero) has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a level, or \a count is invalid, or \a deviceArray is NULL with a non-zero \a count
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device or OS does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           an error has occurred in underlying topology discovery
 */
hgmlReturn_t hgmlDeviceGetTopologyNearestGpus(hgmlDevice_t device, hgmlGpuTopologyLevel_t level, unsigned int *count, hgmlDevice_t *deviceArray);

/**
 * Retrieve the status for a given p2p capability index between a given pair of GPU
 *
 * @param device1                              The first device
 * @param device2                              The second device
 * @param p2pIndex                             p2p Capability Index being looked for between \a device1 and \a device2
 * @param p2pStatus                            Reference in which to return the status of the \a p2pIndex
 *                                             between \a device1 and \a device2
 * @return
 *         - \ref HGML_SUCCESS         if \a p2pStatus has been populated
 *         - \ref HGML_ERROR_INVALID_ARGUMENT     if \a device1 or \a device2 or \a p2pIndex is invalid or \a p2pStatus is NULL
 *         - \ref HGML_ERROR_UNKNOWN              on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetP2PStatus(hgmlDevice_t device1, hgmlDevice_t device2, hgmlGpuP2PCapsIndex_t p2pIndex,hgmlGpuP2PStatus_t *p2pStatus);

/**
 * Retrieves the globally unique immutable UUID associated with this device, as a 5 part hexadecimal string,
 * that augments the immutable, board serial identifier.
 *
 * For all products.
 *
 * The UUID is a globally unique identifier. It is the only available identifier for pre-architecture products.
 * It does NOT correspond to any identifier printed on the board.  It will not exceed 96 characters in length
 * (including the NULL terminator).  See \ref hgmlConstants::HGML_DEVICE_UUID_V2_BUFFER_SIZE.
 *
 * When used with MIG device handles the API returns globally unique UUIDs which can be used to identify MIG
 * devices across both GPU and MIG devices. UUIDs are immutable for the lifetime of a MIG device.
 *
 * @param device                               The identifier of the target device
 * @param uuid                                 Reference in which to return the GPU UUID
 * @param length                               The maximum allowed length of the string returned in \a uuid
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a uuid has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a uuid is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetUUID(hgmlDevice_t device, char *uuid, unsigned int length);

/**
 * Retrieves minor number for the device. The minor number for the device is such that the alixpu device node file for
 * each GPU will have the form /dev/alixpu[minor number].
 *
 * For all products.
 * Supported only for Linux
 *
 * @param device                                The identifier of the target device
 * @param minorNumber                           Reference in which to return the minor number for the device
 * @return
 *         - \ref HGML_SUCCESS                 if the minor number is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a minorNumber is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMinorNumber(hgmlDevice_t device, unsigned int *minorNumber);

/**
 * Retrieves the the device board part number which is programmed into the board's InfoROM
 *
 * For all products.
 *
 * @param device                                Identifier of the target device
 * @param partNumber                            Reference to the buffer to return
 * @param length                                Length of the buffer reference
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a partNumber has been set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if the needed VBIOS fields have not been filled
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device is invalid or \a serial is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetBoardPartNumber(hgmlDevice_t device, char* partNumber, unsigned int length);

/**
 * Retrieves the version information for the device's infoROM object.
 *
 * For all products with an inforom.
 *
 * and higher parts have non-volatile on-board memory for persisting device info, such as aggregate
 * ECC counts. The version of the data structures in this memory may change from time to time. It will not
 * exceed 16 characters in length (including the NULL terminator).
 * See \ref hgmlConstants::HGML_DEVICE_INFOROM_VERSION_BUFFER_SIZE.
 *
 * See \ref hgmlInforomObject_t for details on the available infoROM objects.
 *
 * @param device                               The identifier of the target device
 * @param object                               The target infoROM object
 * @param version                              Reference in which to return the infoROM version
 * @param length                               The maximum allowed length of the string returned in \a version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a version is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have an infoROM
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetInforomImageVersion
 */
hgmlReturn_t hgmlDeviceGetInforomVersion(hgmlDevice_t device, hgmlInforomObject_t object, char *version, unsigned int length);

/**
 * Retrieves the global infoROM image version
 *
 * For all products with an inforom.
 *
 * Image version just like VBIOS version uniquely describes the exact version of the infoROM flashed on the board
 * in contrast to infoROM object version which is only an indicator of supported features.
 * Version string will not exceed 16 characters in length (including the NULL terminator).
 * See \ref hgmlConstants::HGML_DEVICE_INFOROM_VERSION_BUFFER_SIZE.
 *
 * @param device                               The identifier of the target device
 * @param version                              Reference in which to return the infoROM image version
 * @param length                               The maximum allowed length of the string returned in \a version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a version is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have an infoROM
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetInforomVersion
 */
hgmlReturn_t hgmlDeviceGetInforomImageVersion(hgmlDevice_t device, char *version, unsigned int length);

/**
 * Retrieves the checksum of the configuration stored in the device's infoROM.
 *
 * For all products with an inforom.
 *
 * Can be used to make sure that two GPUs have the exact same configuration.
 * Current checksum takes into account configuration stored in PWR and ECC infoROM objects.
 * Checksum can change between driver releases or when user changes configuration (e.g. disable/enable ECC)
 *
 * @param device                               The identifier of the target device
 * @param checksum                             Reference in which to return the infoROM configuration checksum
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a checksum has been set
 *         - \ref HGML_ERROR_CORRUPTED_INFOROM if the device's checksum couldn't be retrieved due to infoROM corruption
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a checksum is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetInforomConfigurationChecksum(hgmlDevice_t device, unsigned int *checksum);

/**
 * Reads the infoROM from the flash and verifies the checksums.
 *
 * For all products with an inforom.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if infoROM is not corrupted
 *         - \ref HGML_ERROR_CORRUPTED_INFOROM if the device's infoROM is corrupted
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceValidateInforom(hgmlDevice_t device);

/**
 * Retrieves the timestamp and the duration of the last flush of the BBX (blackbox) infoROM object during the current run.
 *
 * For all products with an inforom.
 *
 * @param device                               The identifier of the target device
 * @param timestamp                            The start timestamp of the last BBX Flush
 * @param durationUs                           The duration (us) of the last BBX Flush
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a timestamp and \a durationUs are successfully retrieved
 *         - \ref HGML_ERROR_NOT_READY         if the BBX object has not been flushed yet
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have an infoROM
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetInforomVersion
 */
hgmlReturn_t hgmlDeviceGetLastBBXFlushTime(hgmlDevice_t device, unsigned long long *timestamp,
                                                   unsigned long *durationUs);

/**
 * Retrieves the display mode for the device.
 *
 * For all products.
 *
 * This method indicates whether a physical display (e.g. monitor) is currently connected to
 * any of the device's connectors.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param display                              Reference in which to return the display mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a display has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a display is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetDisplayMode(hgmlDevice_t device, hgmlEnableState_t *display);

/**
 * Retrieves the display active state for the device.
 *
 * For all products.
 *
 * This method indicates whether a display is initialized on the device.
 * For example whether X Server is attached to this device and has allocated memory for the screen.
 *
 * Display can be active even when no monitor is physically attached.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param isActive                             Reference in which to return the display active state
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a isActive has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a isActive is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetDisplayActive(hgmlDevice_t device, hgmlEnableState_t *isActive);

/**
 * Retrieves the persistence mode associated with this device.
 *
 * For all products.
 * For Linux only.
 *
 * When driver persistence mode is enabled the driver software state is not torn down when the last
 * client disconnects. By default this feature is disabled.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 Reference in which to return the current driver persistence mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a mode has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetPersistenceMode()
 */
hgmlReturn_t hgmlDeviceGetPersistenceMode(hgmlDevice_t device, hgmlEnableState_t *mode);

/**
 * Retrieves the PCI attributes of this device.
 *
 * For all products.
 *
 * See \ref hgmlPciInfo_t for details on the available PCI info.
 *
 * @param device                               The identifier of the target device
 * @param pci                                  Reference in which to return the PCI info
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pci has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pci is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPciInfo_v3(hgmlDevice_t device, hgmlPciInfo_t *pci);

/**
 * Retrieves the maximum PCIe link generation possible with this device and system
 *
 * I.E. for a generation 2 PCIe device attached to a generation 1 PCIe bus the max link generation this function will
 * report is generation 1.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param maxLinkGen                           Reference in which to return the max PCIe link generation
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a maxLinkGen has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a maxLinkGen is null
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if PCIe link information is not available
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMaxPcieLinkGeneration(hgmlDevice_t device, unsigned int *maxLinkGen);

/**
 * Retrieves the maximum PCIe link generation supported by this device
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param maxLinkGenDevice                     Reference in which to return the max PCIe link generation
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a maxLinkGenDevice has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a maxLinkGenDevice is null
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if PCIe link information is not available
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGpuMaxPcieLinkGeneration(hgmlDevice_t device, unsigned int *maxLinkGenDevice);

/**
 * Retrieves the maximum PCIe link width possible with this device and system
 *
 * I.E. for a device with a 16x PCIe bus width attached to a 8x PCIe system bus this function will report
 * a max link width of 8.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param maxLinkWidth                         Reference in which to return the max PCIe link generation
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a maxLinkWidth has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a maxLinkWidth is null
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if PCIe link information is not available
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMaxPcieLinkWidth(hgmlDevice_t device, unsigned int *maxLinkWidth);

/**
 * Retrieves the current PCIe link generation
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param currLinkGen                          Reference in which to return the current PCIe link generation
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a currLinkGen has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a currLinkGen is null
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if PCIe link information is not available
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetCurrPcieLinkGeneration(hgmlDevice_t device, unsigned int *currLinkGen);

/**
 * Retrieves the current PCIe link width
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param currLinkWidth                        Reference in which to return the current PCIe link generation
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a currLinkWidth has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a currLinkWidth is null
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if PCIe link information is not available
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetCurrPcieLinkWidth(hgmlDevice_t device, unsigned int *currLinkWidth);

/**
 * Retrieve PCIe utilization information.
 * This function is querying a byte counter over a 20ms interval and thus is the
 *   PCIe throughput over that interval.
 *
 * For &tm; or newer fully supported devices.
 *
 * This method is not supported in virtual machines running virtual GPU (vGPU).
 *
 * @param device                               The identifier of the target device
 * @param counter                              The specific counter that should be queried \ref hgmlPcieUtilCounter_t
 * @param value                                Reference in which to return throughput in KB/s
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a value has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a counter is invalid, or \a value is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPcieThroughput(hgmlDevice_t device, hgmlPcieUtilCounter_t counter, unsigned int *value);

/**
 * Retrieve the PCIe replay counter.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param value                                Reference in which to return the counter's value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a value has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a value is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPcieReplayCounter(hgmlDevice_t device, unsigned int *value);

/**
 * Retrieves the current clock speeds for the device.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlClockType_t for details on available clock information.
 *
 * @param device                               The identifier of the target device
 * @param type                                 Identify which clock domain to query
 * @param clock                                Reference in which to return the clock speed in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clock has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clock is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device cannot report the specified clock
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetClockInfo(hgmlDevice_t device, hgmlClockType_t type, unsigned int *clock);

/**
 * Retrieves the maximum clock speeds for the device.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlClockType_t for details on available clock information.
 *
 * \note On GPUs from family current P0 clocks (reported by \ref hgmlDeviceGetClockInfo) can differ from max clocks
 *       by few MHz.
 *
 * @param device                               The identifier of the target device
 * @param type                                 Identify which clock domain to query
 * @param clock                                Reference in which to return the clock speed in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clock has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clock is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device cannot report the specified clock
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMaxClockInfo(hgmlDevice_t device, hgmlClockType_t type, unsigned int *clock);

/**
 * Retrieve the GPCCLK VF offset value
 * @param[in]   device                         The identifier of the target device
 * @param[out]  offset                         The retrieved GPCCLK VF offset value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGpcClkVfOffset(hgmlDevice_t device, int *offset);

/**
 * Retrieves the current setting of a clock that applications will use unless an overspec situation occurs.
 * Can be changed using \ref hgmlDeviceSetApplicationsClocks.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param clockType                            Identify which clock domain to query
 * @param clockMHz                             Reference in which to return the clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clockMHz has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clockMHz is NULL or \a clockType is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetApplicationsClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);

/**
 * Retrieves the default applications clock that GPU boots with or
 * defaults to after \ref hgmlDeviceResetApplicationsClocks call.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param clockType                            Identify which clock domain to query
 * @param clockMHz                             Reference in which to return the default clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clockMHz has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clockMHz is NULL or \a clockType is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * \see hgmlDeviceGetApplicationsClock
 */
hgmlReturn_t hgmlDeviceGetDefaultApplicationsClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);

/**
 * Retrieves the clock speed for the clock specified by the clock type and clock ID.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param clockType                            Identify which clock domain to query
 * @param clockId                              Identify which clock in the domain to query
 * @param clockMHz                             Reference in which to return the clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clockMHz has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clockMHz is NULL or \a clockType is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetClock(hgmlDevice_t device, hgmlClockType_t clockType, hgmlClockId_t clockId, unsigned int *clockMHz);

/**
 * Retrieves the customer defined maximum boost clock speed specified by the given clock type.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param clockType                            Identify which clock domain to query
 * @param clockMHz                             Reference in which to return the clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clockMHz has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clockMHz is NULL or \a clockType is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device or the \a clockType on this device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMaxCustomerBoostClock(hgmlDevice_t device, hgmlClockType_t clockType, unsigned int *clockMHz);

/**
 * Retrieves the list of possible memory clocks that can be used as an argument for \ref hgmlDeviceSetApplicationsClocks.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param count                                Reference in which to provide the \a clocksMHz array size, and
 *                                             to return the number of elements
 * @param clocksMHz                            Reference in which to return the clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a count and \a clocksMHz have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a count is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a count is too small (\a count is set to the number of
 *                                                required elements)
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetApplicationsClocks
 * @see hgmlDeviceGetSupportedGraphicsClocks
 */
hgmlReturn_t hgmlDeviceGetSupportedMemoryClocks(hgmlDevice_t device, unsigned int *count, unsigned int *clocksMHz);

/**
 * Retrieves the list of possible graphics clocks that can be used as an argument for \ref hgmlDeviceSetApplicationsClocks.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param memoryClockMHz                       Memory clock for which to return possible graphics clocks
 * @param count                                Reference in which to provide the \a clocksMHz array size, and
 *                                             to return the number of elements
 * @param clocksMHz                            Reference in which to return the clocks in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a count and \a clocksMHz have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NOT_FOUND         if the specified \a memoryClockMHz is not a supported frequency
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clock is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a count is too small
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetApplicationsClocks
 * @see hgmlDeviceGetSupportedMemoryClocks
 */
hgmlReturn_t hgmlDeviceGetSupportedGraphicsClocks(hgmlDevice_t device, unsigned int memoryClockMHz, unsigned int *count, unsigned int *clocksMHz);

/**
 * Retrieve the current state of Auto Boosted clocks on a device and store it in \a isEnabled
 *
 * For &tm; or newer fully supported devices.
 *
 * Auto Boosted clocks are enabled by default on some hardware, allowing the GPU to run at higher clock rates
 * to maximize performance as thermal limits allow.
 *
 * On and newer hardware, Auto Aoosted clocks are controlled through application clocks.
 * Use \ref hgmlDeviceSetApplicationsClocks and \ref hgmlDeviceResetApplicationsClocks to control Auto Boost
 * behavior.
 *
 * @param device                               The identifier of the target device
 * @param isEnabled                            Where to store the current state of Auto Boosted clocks of the target device
 * @param defaultIsEnabled                     Where to store the default Auto Boosted clocks behavior of the target device that the device will
 *                                                 revert to when no applications are using the GPU
 *
 * @return
 *         - \ref HGML_SUCCESS                 If \a isEnabled has been been set with the Auto Boosted clocks state of \a device
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a isEnabled is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support Auto Boosted clocks
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceGetAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t *isEnabled, hgmlEnableState_t *defaultIsEnabled);

/**
 * Retrieves the intended operating speed of the device's fan.
 *
 * Note: The reported speed is the intended fan speed.  If the fan is physically blocked and unable to spin, the
 * output will not match the actual fan speed.
 *
 * For all discrete products with dedicated fans.
 *
 * The fan speed is expressed as a percentage of the product's maximum noise tolerance fan speed.
 * This value may exceed 100% in certain cases.
 *
 * @param device                               The identifier of the target device
 * @param speed                                Reference in which to return the fan speed percentage
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a speed has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a speed is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have a fan
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetFanSpeed(hgmlDevice_t device, unsigned int *speed);


/**
 * Retrieves the intended operating speed of the device's specified fan.
 *
 * Note: The reported speed is the intended fan speed. If the fan is physically blocked and unable to spin, the
 * output will not match the actual fan speed.
 *
 * For all discrete products with dedicated fans.
 *
 * The fan speed is expressed as a percentage of the product's maximum noise tolerance fan speed.
 * This value may exceed 100% in certain cases.
 *
 * @param device                                The identifier of the target device
 * @param fan                                   The index of the target fan, zero indexed.
 * @param speed                                 Reference in which to return the fan speed percentage
 *
 * @return
 *        - \ref HGML_SUCCESS                   if \a speed has been set
 *        - \ref HGML_ERROR_UNINITIALIZED       if the library has not been successfully initialized
 *        - \ref HGML_ERROR_INVALID_ARGUMENT    if \a device is invalid, \a fan is not an acceptable index, or \a speed is NULL
 *        - \ref HGML_ERROR_NOT_SUPPORTED       if the device does not have a fan or is newer than
 *        - \ref HGML_ERROR_GPU_IS_LOST         if the target GPU has fallen off the bus or is otherwise inaccessible
 *        - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetFanSpeed_v2(hgmlDevice_t device, unsigned int fan, unsigned int * speed);

/**
 * Retrieves the intended target speed of the device's specified fan.
 *
 * Normally, the driver dynamically adjusts the fan based on
 * the needs of the GPU.  But when user set fan speed using hgmlDeviceSetFanSpeed_v2,
 * the driver will attempt to make the fan achieve the setting in
 * hgmlDeviceSetFanSpeed_v2.  The actual current speed of the fan
 * is reported in hgmlDeviceGetFanSpeed_v2.
 *
 * For all discrete products with dedicated fans.
 *
 * The fan speed is expressed as a percentage of the product's maximum noise tolerance fan speed.
 * This value may exceed 100% in certain cases.
 *
 * @param device                                The identifier of the target device
 * @param fan                                   The index of the target fan, zero indexed.
 * @param targetSpeed                           Reference in which to return the fan speed percentage
 *
 * @return
 *        - \ref HGML_SUCCESS                   if \a speed has been set
 *        - \ref HGML_ERROR_UNINITIALIZED       if the library has not been successfully initialized
 *        - \ref HGML_ERROR_INVALID_ARGUMENT    if \a device is invalid, \a fan is not an acceptable index, or \a speed is NULL
 *        - \ref HGML_ERROR_NOT_SUPPORTED       if the device does not have a fan or is newer than
 *        - \ref HGML_ERROR_GPU_IS_LOST         if the target GPU has fallen off the bus or is otherwise inaccessible
 *        - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetTargetFanSpeed(hgmlDevice_t device, unsigned int fan, unsigned int *targetSpeed);

/**
 * Retrieves the min and max fan speed that user can set for the GPU fan.
 *
 * For all cuda-capable discrete products with fans
 *
 * @param device                        The identifier of the target device
 * @param minSpeed                      The minimum speed allowed to set
 * @param maxSpeed                      The maximum speed allowed to set
 *
 * return
 *         HGML_SUCCESS                 if speed has been adjusted
 *         HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         HGML_ERROR_INVALID_ARGUMENT  if device is invalid
 *         HGML_ERROR_NOT_SUPPORTED     if the device does not support this
 *                                      (doesn't have fans)
 *         HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMinMaxFanSpeed(hgmlDevice_t device, unsigned int * minSpeed,
                                                 unsigned int * maxSpeed);

/**
 * Gets current fan control policy.
 *
 * For &tm; or newer fully supported devices.
 *
 * For all cuda-capable discrete products with fans
 *
 * device                               The identifier of the target \a device
 * policy                               Reference in which to return the fan control \a policy
 *
 * return
 *         HGML_SUCCESS                 if \a policy has been populated
 *         HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a policy is null or the \a fan given doesn't reference
 *                                            a fan that exists.
 *         HGML_ERROR_NOT_SUPPORTED     if the \a device is older than
 *         HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetFanControlPolicy_v2(hgmlDevice_t device, unsigned int fan,
                                                      hgmlFanControlPolicy_t *policy);

/**
 * Retrieves the number of fans on the device.
 *
 * For all discrete products with dedicated fans.
 *
 * @param device                               The identifier of the target device
 * @param numFans                              The number of fans
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a fan number query was successful
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a numFans is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have a fan
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetNumFans(hgmlDevice_t device, unsigned int *numFans);

/**
 * Retrieves the current temperature readings for the device, in degrees C.
 *
 * For all products.
 *
 * See \ref hgmlTemperatureSensors_t for details on available temperature sensors.
 *
 * @param device                               The identifier of the target device
 * @param sensorType                           Flag that indicates which sensor reading to retrieve
 * @param temp                                 Reference in which to return the temperature reading
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a temp has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a sensorType is invalid or \a temp is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have the specified sensor
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetTemperature(hgmlDevice_t device, hgmlTemperatureSensors_t sensorType, unsigned int *temp);

/**
 * Retrieves the temperature threshold for the GPU with the specified threshold type in degrees C.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlTemperatureThresholds_t for details on available temperature thresholds.
 *
 * Note: This API is no longer the preferred interface for retrieving the following temperature thresholds
 * on Ada and later architectures: HGML_TEMPERATURE_THRESHOLD_SHUTDOWN, HGML_TEMPERATURE_THRESHOLD_SLOWDOWN,
 * HGML_TEMPERATURE_THRESHOLD_MEM_MAX and HGML_TEMPERATURE_THRESHOLD_GPU_MAX.
 *
 * Support for reading these temperature thresholds for Ada and later architectures would be removed from this
 * API in future releases. Please use \ref hgmlDeviceGetFieldValues with HGML_FI_DEV_TEMPERATURE_* fields to retrieve
 * temperature thresholds on these architectures.
 *
 * @param device                               The identifier of the target device
 * @param thresholdType                        The type of threshold value queried
 * @param temp                                 Reference in which to return the temperature reading
 * @return
 *         - \ref HGML_SUCCESS                 if \a temp has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a thresholdType is invalid or \a temp is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have a temperature sensor or is unsupported
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetTemperatureThreshold(hgmlDevice_t device, hgmlTemperatureThresholds_t thresholdType, unsigned int *temp);

/**
 * Used to execute a list of thermal system instructions.
 *
 * @param device                               The identifier of the target device
 * @param sensorIndex                          The index of the thermal sensor
 * @param pThermalSettings                     Reference in which to return the thermal sensor information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pThermalSettings has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pThermalSettings is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetThermalSettings(hgmlDevice_t device, unsigned int sensorIndex, hgmlGpuThermalSettings_t *pThermalSettings);

/**
 * Retrieves the current performance state for the device.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlPstates_t for details on allowed performance states.
 *
 * @param device                               The identifier of the target device
 * @param pState                               Reference in which to return the performance state reading
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pState has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pState is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPerformanceState(hgmlDevice_t device, hgmlPstates_t *pState);

/**
 * Retrieves current clocks event reasons.
 *
 * For all fully supported products.
 *
 * \note More than one bit can be enabled at the same time. Multiple reasons can be affecting clocks at once.
 *
 * @param device                                The identifier of the target device
 * @param clocksEventReasons                    Reference in which to return bitmask of active clocks event
 *                                                  reasons
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a clocksEventReasons has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a clocksEventReasons is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlClocksEventReasons
 * @see hgmlDeviceGetSupportedClocksEventReasons
 */
hgmlReturn_t hgmlDeviceGetCurrentClocksEventReasons(hgmlDevice_t device, unsigned long long *clocksEventReasons);

/**
 * @deprecated Use \ref hgmlDeviceGetCurrentClocksEventReasons instead
 */
hgmlReturn_t hgmlDeviceGetCurrentClocksThrottleReasons(hgmlDevice_t device, unsigned long long *clocksThrottleReasons);

/**
 * Retrieves bitmask of supported clocks event reasons that can be returned by
 * \ref hgmlDeviceGetCurrentClocksEventReasons
 *
 * For all fully supported products.
 *
 * This method is not supported in virtual machines running virtual GPU (vGPU).
 *
 * @param device                               The identifier of the target device
 * @param supportedClocksEventReasons       Reference in which to return bitmask of supported
 *                                              clocks event reasons
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a supportedClocksEventReasons has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a supportedClocksEventReasons is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlClocksEventReasons
 * @see hgmlDeviceGetCurrentClocksEventReasons
 */
hgmlReturn_t hgmlDeviceGetSupportedClocksEventReasons(hgmlDevice_t device, unsigned long long *supportedClocksEventReasons);

/**
 * @deprecated Use \ref hgmlDeviceGetSupportedClocksEventReasons instead
 */
hgmlReturn_t hgmlDeviceGetSupportedClocksThrottleReasons(hgmlDevice_t device, unsigned long long *supportedClocksThrottleReasons);

/**
 * Deprecated: Use \ref hgmlDeviceGetPerformanceState. This function exposes an incorrect generalization.
 *
 * Retrieve the current performance state for the device.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlPstates_t for details on allowed performance states.
 *
 * @param device                               The identifier of the target device
 * @param pState                               Reference in which to return the performance state reading
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pState has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pState is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPowerState(hgmlDevice_t device, hgmlPstates_t *pState);

/**
 * Retrieve performance monitor samples from the associated subdevice.
 *
 * @param device
 * @param pDynamicPstatesInfo
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pDynamicPstatesInfo has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pDynamicPstatesInfo is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetDynamicPstatesInfo(hgmlDevice_t device, hgmlGpuDynamicPstatesInfo_t *pDynamicPstatesInfo);

/**
 * Retrieve the MemClk (Memory Clock) VF offset value.
 * @param[in]   device                         The identifier of the target device
 * @param[out]  offset                         The retrieved MemClk VF offset value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMemClkVfOffset(hgmlDevice_t device, int *offset);

/**
 * Retrieve min and max clocks of some clock domain for a given PState
 *
 * @param device                               The identifier of the target device
 * @param type                                 Clock domain
 * @param pstate                               PState to query
 * @param minClockMHz                          Reference in which to return min clock frequency
 * @param maxClockMHz                          Reference in which to return max clock frequency
 *
 * @return
 *         - \ref HGML_SUCCESS                 if everything worked
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a type or \a pstate are invalid or both
 *                                                  \a minClockMHz and \a maxClockMHz are NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 */
hgmlReturn_t hgmlDeviceGetMinMaxClockOfPState(hgmlDevice_t device, hgmlClockType_t type, hgmlPstates_t pstate,
                                                      unsigned int *minClockMHz, unsigned int *maxClockMHz);

/**
 * Get all supported Performance States (P-States) for the device.
 *
 * The returned array would contain a contiguous list of valid P-States supported by
 * the device. If the number of supported P-States is fewer than the size of the array
 * supplied missing elements would contain \a HGML_PSTATE_UNKNOWN.
 *
 * The number of elements in the returned list will never exceed \a HGML_MAX_GPU_PERF_PSTATES.
 *
 * @param device                               The identifier of the target device
 * @param pstates                              Container to return the list of performance states
 *                                             supported by device
 * @param size                                 Size of the supplied \a pstates array in bytes
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pstates array has been retrieved
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if the the container supplied was not large enough to
 *                                             hold the resulting list
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a pstates is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support performance state readings
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetSupportedPerformanceStates(hgmlDevice_t device,
                                                             hgmlPstates_t *pstates, unsigned int size);

/**
 * Retrieve the GPCCLK min max VF offset value.
 * @param[in]   device                         The identifier of the target device
 * @param[out]  minOffset                      The retrieved GPCCLK VF min offset value
 * @param[out]  maxOffset                      The retrieved GPCCLK VF max offset value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGpcClkMinMaxVfOffset(hgmlDevice_t device,
                                                       int *minOffset, int *maxOffset);

/**
 * Retrieve the MemClk (Memory Clock) min max VF offset value.
 * @param[in]   device                         The identifier of the target device
 * @param[out]  minOffset                      The retrieved MemClk VF min offset value
 * @param[out]  maxOffset                      The retrieved MemClk VF max offset value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMemClkMinMaxVfOffset(hgmlDevice_t device,
                                                       int *minOffset, int *maxOffset);

/**
 * This API has been deprecated.
 *
 * Retrieves the power management mode associated with this device.
 *
 * For products from the family.
 *     - Requires \a HGML_INFOROM_POWER version 3.0 or higher.
 *
 * For from the or newer families.
 *     - Does not require \a HGML_INFOROM_POWER object.
 *
 * This flag indicates whether any power management algorithm is currently active on the device. An
 * enabled state does not necessarily mean the device is being actively throttled -- only that
 * that the driver will do so if the appropriate conditions are met.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 Reference in which to return the current power management mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a mode has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPowerManagementMode(hgmlDevice_t device, hgmlEnableState_t *mode);

/**
 * Retrieves the power management limit associated with this device.
 *
 * For &tm; or newer fully supported devices.
 *
 * The power limit defines the upper boundary for the card's power draw. If
 * the card's total power draw reaches this limit the power management algorithm kicks in.
 *
 * This reading is only available if power management mode is supported.
 * See \ref hgmlDeviceGetPowerManagementMode.
 *
 * @param device                               The identifier of the target device
 * @param limit                                Reference in which to return the power management limit in milliwatts
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a limit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a limit is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPowerManagementLimit(hgmlDevice_t device, unsigned int *limit);


/**
 * Retrieves information about possible values of power management limits on this device.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param minLimit                             Reference in which to return the minimum power management limit in milliwatts
 * @param maxLimit                             Reference in which to return the maximum power management limit in milliwatts
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a minLimit and \a maxLimit have been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a minLimit or \a maxLimit is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetPowerManagementLimit
 */
hgmlReturn_t hgmlDeviceGetPowerManagementLimitConstraints(hgmlDevice_t device, unsigned int *minLimit, unsigned int *maxLimit);

/**
 * Retrieves default power management limit on this device, in milliwatts.
 * Default power management limit is a power management limit that the device boots with.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param defaultLimit                         Reference in which to return the default power management limit in milliwatts
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a defaultLimit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a defaultLimit is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPowerManagementDefaultLimit(hgmlDevice_t device, unsigned int *defaultLimit);

/**
 * Retrieves power usage for this GPU in milliwatts and its associated circuitry (e.g. memory)
 *
 * For &tm; or newer fully supported devices.
 *
 * On and GPUs the reading is accurate to within +/- 5% of current power draw. On
 * (except ) or newer GPUs, the API returns power averaged over 1 sec interval. On and
 * older architectures, instantaneous power is returned.
 *
 * See \ref HGML_FI_DEV_POWER_AVERAGE and \ref HGML_FI_DEV_POWER_INSTANT to query specific power
 * values.
 *
 * It is only available if power management mode is supported. See \ref hgmlDeviceGetPowerManagementMode.
 *
 * @param device                               The identifier of the target device
 * @param power                                Reference in which to return the power usage information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a power has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a power is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support power readings
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPowerUsage(hgmlDevice_t device, unsigned int *power);

/**
 * Retrieves total energy consumption for this GPU in millijoules (mJ) since the driver was last reloaded
 *
 * For Volta &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param energy                               Reference in which to return the energy consumption information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a energy has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a energy is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support energy readings
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetTotalEnergyConsumption(hgmlDevice_t device, unsigned long long *energy);

/**
 * Get the effective power limit that the driver enforces after taking into account all limiters
 *
 * Note: This can be different from the \ref hgmlDeviceGetPowerManagementLimit if other limits are set elsewhere
 * This includes the out of band power limit interface
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                           The device to communicate with
 * @param limit                            Reference in which to return the power management limit in milliwatts
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a limit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a limit is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetEnforcedPowerLimit(hgmlDevice_t device, unsigned int *limit);

/**
 * Retrieves the current GOM and pending GOM (the one that GPU will switch to after reboot).
 *
 * For &tm; products from the family.
 * Modes \ref HGML_GOM_LOW_DP and \ref HGML_GOM_ALL_ON are supported on fully supported products.
 *
 * @param device                               The identifier of the target device
 * @param current                              Reference in which to return the current GOM
 * @param pending                              Reference in which to return the pending GOM
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a mode has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a current or \a pending is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlGpuOperationMode_t
 * @see hgmlDeviceSetGpuOperationMode
 */
hgmlReturn_t hgmlDeviceGetGpuOperationMode(hgmlDevice_t device, hgmlGpuOperationMode_t *current, hgmlGpuOperationMode_t *pending);

/**
 * Retrieves the amount of used, free, reserved and total memory available on the device, in bytes.
 * The reserved amount is supported on version 2 only.
 *
 * For all products.
 *
 * Enabling ECC reduces the amount of total available memory, due to the extra required parity bits.
 * Under WDDM most device memory is allocated and managed on startup by Windows.
 *
 * Under Linux and Windows TCC, the reported amount of used memory is equal to the sum of memory allocated
 * by all active channels on the device.
 *
 * See \ref hgmlMemory_v2_t for details on available memory info.
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate
 *       information, only if the caller has appropriate privileges. Per-instance
 *       information can be queried by using specific MIG device handles.
 *
 * @note hgmlDeviceGetMemoryInfo_v2 adds additional memory information.
 *
 * @note On systems where GPUs are NUMA nodes, the accuracy of FB memory utilization
 *       provided by this API depends on the memory accounting of the operating system.
 *       This is because FB memory is managed by the operating system instead of the alixpu GPU driver.
 *       Typically, pages allocated from FB memory are not released even after
 *       the process terminates to enhance performance. In scenarios where
 *       the operating system is under memory pressure, it may resort to utilizing FB memory.
 *       Such actions can result in discrepancies in the accuracy of memory reporting.
 *
 * @param device                               The identifier of the target device
 * @param memory                               Reference in which to return the memory information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a memory has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a memory is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory);
hgmlReturn_t hgmlDeviceGetMemoryInfo_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory);

/**
 * Retrieves the current compute mode for the device.
 *
 * For all products.
 *
 * See \ref hgmlComputeMode_t for details on allowed compute modes.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 Reference in which to return the current compute mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a mode has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetComputeMode()
 */
hgmlReturn_t hgmlDeviceGetComputeMode(hgmlDevice_t device, hgmlComputeMode_t *mode);

/**
 * Retrieves the HGGC compute capability of the device.
 *
 * For all products.
 *
 * Returns the major and minor compute capability version numbers of the
 * device.  The major and minor versions are equivalent to the
 * CU_DEVICE_ATTRIBUTE_COMPUTE_CAPABILITY_MINOR and
 * CU_DEVICE_ATTRIBUTE_COMPUTE_CAPABILITY_MAJOR attributes that would be
 * returned by HGGC's cuDeviceGetAttribute().
 *
 * @param device                               The identifier of the target device
 * @param major                                Reference in which to return the major HGGC compute capability
 * @param minor                                Reference in which to return the minor HGGC compute capability
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a major and \a minor have been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a major or \a minor are NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetHggcComputeCapability(hgmlDevice_t device, int *major, int *minor);

/**
 * Retrieves the current and pending ECC modes for the device.
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher.
 *
 * Changing ECC modes requires a reboot. The "pending" ECC mode refers to the target mode following
 * the next reboot.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param current                              Reference in which to return the current ECC mode
 * @param pending                              Reference in which to return the pending ECC mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a current and \a pending have been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or either \a current or \a pending is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetEccMode()
 */
hgmlReturn_t hgmlDeviceGetEccMode(hgmlDevice_t device, hgmlEnableState_t *current, hgmlEnableState_t *pending);

/**
 * Retrieves the default ECC modes for the device.
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher.
 *
 * See \ref hgmlEnableState_t for details on allowed modes.
 *
 * @param device                               The identifier of the target device
 * @param defaultMode                          Reference in which to return the default ECC mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a current and \a pending have been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a default is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetEccMode()
 */
hgmlReturn_t hgmlDeviceGetDefaultEccMode(hgmlDevice_t device, hgmlEnableState_t *defaultMode);

/**
 * Retrieves the device boardId from 0-N.
 * Devices with the same boardId indicate GPUs connected to the same PLX.  Use in conjunction with
 *  \ref hgmlDeviceGetMultiGpuBoard() to decide if they are on the same board as well.
 *  The boardId returned is a unique ID for the current configuration.  Uniqueness and ordering across
 *  reboots and system configurations is not guaranteed (i.e. if a returns 0x100 and
 *  the two GPUs on a in the same system returns 0x200 it is not guaranteed they will
 *  always return those values but they will always be different from each other).
 *
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param boardId                              Reference in which to return the device's board ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a boardId has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a boardId is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetBoardId(hgmlDevice_t device, unsigned int *boardId);

/**
 * Retrieves whether the device is on a Multi-GPU Board
 * Devices that are on multi-GPU boards will set \a multiGpuBool to a non-zero value.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param multiGpuBool                         Reference in which to return a zero or non-zero value
 *                                                 to indicate whether the device is on a multi GPU board
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a multiGpuBool has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a multiGpuBool is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMultiGpuBoard(hgmlDevice_t device, unsigned int *multiGpuBool);

/**
 * Retrieves the total ECC error counts for the device.
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher.
 * Requires ECC Mode to be enabled.
 *
 * The total error count is the sum of errors across each of the separate memory systems, i.e. the total set of
 * errors across the entire device.
 *
 * See \ref hgmlMemoryErrorType_t for a description of available error types.\n
 * See \ref hgmlEccCounterType_t for a description of available counter types.
 *
 * @param device                               The identifier of the target device
 * @param errorType                            Flag that specifies the type of the errors.
 * @param counterType                          Flag that specifies the counter-type of the errors.
 * @param eccCounts                            Reference in which to return the specified ECC errors
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a eccCounts has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a errorType or \a counterType is invalid, or \a eccCounts is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceClearEccErrorCounts()
 */
hgmlReturn_t hgmlDeviceGetTotalEccErrors(hgmlDevice_t device, hgmlMemoryErrorType_t errorType, hgmlEccCounterType_t counterType, unsigned long long *eccCounts);

/**
 * Retrieves the detailed ECC error counts for the device.
 *
 * @deprecated   This API supports only a fixed set of ECC error locations
 *               On different GPU architectures different locations are supported
 *               See \ref hgmlDeviceGetMemoryErrorCounter
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 2.0 or higher to report aggregate location-based ECC counts.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher to report all other ECC counts.
 * Requires ECC Mode to be enabled.
 *
 * Detailed errors provide separate ECC counts for specific parts of the memory system.
 *
 * Reports zero for unsupported ECC error counters when a subset of ECC error counters are supported.
 *
 * See \ref hgmlMemoryErrorType_t for a description of available bit types.\n
 * See \ref hgmlEccCounterType_t for a description of available counter types.\n
 * See \ref hgmlEccErrorCounts_t for a description of provided detailed ECC counts.
 *
 * @param device                               The identifier of the target device
 * @param errorType                            Flag that specifies the type of the errors.
 * @param counterType                          Flag that specifies the counter-type of the errors.
 * @param eccCounts                            Reference in which to return the specified ECC errors
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a eccCounts has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a errorType or \a counterType is invalid, or \a eccCounts is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceClearEccErrorCounts()
 */
hgmlReturn_t hgmlDeviceGetDetailedEccErrors(hgmlDevice_t device, hgmlMemoryErrorType_t errorType, hgmlEccCounterType_t counterType, hgmlEccErrorCounts_t *eccCounts);

/**
 * Retrieves the requested memory error counter for the device.
 *
 * For &tm; or newer fully supported devices.
 * Requires \a HGML_INFOROM_ECC version 2.0 or higher to report aggregate location-based memory error counts.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher to report all other memory error counts.
 *
 * Only applicable to devices with ECC.
 *
 * Requires ECC Mode to be enabled.
 *
 * @note On MIG-enabled GPUs, per instance information can be queried using specific
 *       MIG device handles. Per instance information is currently only supported for
 *       non-DRAM uncorrectable volatile errors. Querying volatile errors using device
 *       handles is currently not supported.
 *
 * See \ref hgmlMemoryErrorType_t for a description of available memory error types.\n
 * See \ref hgmlEccCounterType_t for a description of available counter types.\n
 * See \ref hgmlMemoryLocation_t for a description of available counter locations.\n
 *
 * @param device                               The identifier of the target device
 * @param errorType                            Flag that specifies the type of error.
 * @param counterType                          Flag that specifies the counter-type of the errors.
 * @param locationType                         Specifies the location of the counter.
 * @param count                                Reference in which to return the ECC counter
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a count has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a bitTyp,e \a counterType or \a locationType is
 *                                             invalid, or \a count is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support ECC error reporting in the specified memory
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMemoryErrorCounter(hgmlDevice_t device, hgmlMemoryErrorType_t errorType,
                                                   hgmlEccCounterType_t counterType,
                                                   hgmlMemoryLocation_t locationType, unsigned long long *count);

/**
 * Retrieves the current utilization rates for the device's major subsystems.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlUtilization_t for details on available utilization rates.
 *
 * \note During driver initialization when ECC is enabled one can see high GPU and Memory Utilization readings.
 *       This is caused by ECC Memory Scrubbing mechanism that is performed during driver initialization.
 *
 * @note On MIG-enabled GPUs, querying device utilization rates is not currently supported.
 *
 * @param device                               The identifier of the target device
 * @param utilization                          Reference in which to return the utilization information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a utilization is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetUtilizationRates(hgmlDevice_t device, hgmlUtilization_t *utilization);

/**
 * Retrieves the current utilization and sampling size in microseconds for the Encoder
 *
 * For &tm; or newer fully supported devices.
 *
 * @note On MIG-enabled GPUs, querying encoder utilization is not currently supported.
 *
 * @param device                               The identifier of the target device
 * @param utilization                          Reference to an unsigned int for encoder utilization info
 * @param samplingPeriodUs                     Reference to an unsigned int for the sampling period in US
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a utilization is NULL, or \a samplingPeriodUs is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetEncoderUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);

/**
 * Retrieves the current capacity of the device's encoder, as a percentage of maximum encoder capacity with valid values in the range 0-100.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param encoderQueryType                  Type of encoder to query
 * @param encoderCapacity                   Reference to an unsigned int for the encoder capacity
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a encoderCapacity is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a encoderCapacity is NULL, or \a device or \a encoderQueryType
 *                                              are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if device does not support the encoder specified in \a encodeQueryType
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetEncoderCapacity (hgmlDevice_t device, hgmlEncoderType_t encoderQueryType, unsigned int *encoderCapacity);

/**
 * Retrieves the current encoder statistics for a given device.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param sessionCount                      Reference to an unsigned int for count of active encoder sessions
 * @param averageFps                        Reference to an unsigned int for trailing average FPS of all active sessions
 * @param averageLatency                    Reference to an unsigned int for encode latency in microseconds
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a sessionCount, \a averageFps and \a averageLatency is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a sessionCount, or \a device or \a averageFps,
 *                                              or \a averageLatency is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetEncoderStats (hgmlDevice_t device, unsigned int *sessionCount,
                                                unsigned int *averageFps, unsigned int *averageLatency);

/**
 * Retrieves information about active encoder sessions on a target device.
 *
 * An array of active encoder sessions is returned in the caller-supplied buffer pointed at by \a sessionInfos. The
 * array element count is passed in \a sessionCount, and \a sessionCount is used to return the number of sessions
 * written to the buffer.
 *
 * If the supplied buffer is not large enough to accommodate the active session array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlEncoderSessionInfo_t array required in \a sessionCount.
 * To query the number of active encoder sessions, call this function with *sessionCount = 0.  The code will return
 * HGML_SUCCESS with number of active encoder sessions updated in *sessionCount.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param sessionCount                      Reference to caller supplied array size, and returns the number of sessions.
 * @param sessionInfos                      Reference in which to return the session information
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a sessionInfos is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a sessionCount is too small, array element count is returned in \a sessionCount
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a sessionCount is NULL.
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if this query is not supported by \a device
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetEncoderSessions(hgmlDevice_t device, unsigned int *sessionCount, hgmlEncoderSessionInfo_t *sessionInfos);

/**
 * Retrieves the current utilization and sampling size in microseconds for the Decoder
 *
 * For &tm; or newer fully supported devices.
 *
 * @note On MIG-enabled GPUs, querying decoder utilization is not currently supported.
 *
 * @param device                               The identifier of the target device
 * @param utilization                          Reference to an unsigned int for decoder utilization info
 * @param samplingPeriodUs                     Reference to an unsigned int for the sampling period in US
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a utilization is NULL, or \a samplingPeriodUs is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetDecoderUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);

/**
 * Retrieves the current utilization and sampling size in microseconds for the JPG
 *
 * %TURING_OR_NEWER%
 *
 * @note On MIG-enabled GPUs, querying decoder utilization is not currently supported.
 *
 * @param device                               The identifier of the target device
 * @param utilization                          Reference to an unsigned int for jpg utilization info
 * @param samplingPeriodUs                     Reference to an unsigned int for the sampling period in US
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a utilization is NULL, or \a samplingPeriodUs is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetJpgUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);

/**
 * Retrieves the current utilization and sampling size in microseconds for the OFA (Optical Flow Accelerator)
 *
 * %TURING_OR_NEWER%
 *
 * @note On MIG-enabled GPUs, querying decoder utilization is not currently supported.
 *
 * @param device                               The identifier of the target device
 * @param utilization                          Reference to an unsigned int for ofa utilization info
 * @param samplingPeriodUs                     Reference to an unsigned int for the sampling period in US
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a utilization is NULL, or \a samplingPeriodUs is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetOfaUtilization(hgmlDevice_t device, unsigned int *utilization, unsigned int *samplingPeriodUs);

/**
* Retrieves the active frame buffer capture sessions statistics for a given device.
*
* For &tm; or newer fully supported devices.
*
* @param device                            The identifier of the target device
* @param fbcStats                          Reference to hgmlFBCStats_t structure containing NvFBC stats
*
* @return
*         - \ref HGML_SUCCESS                  if \a fbcStats is fetched
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a fbcStats is NULL
*         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlDeviceGetFBCStats(hgmlDevice_t device, hgmlFBCStats_t *fbcStats);

/**
* Retrieves information about active frame buffer capture sessions on a target device.
*
* An array of active FBC sessions is returned in the caller-supplied buffer pointed at by \a sessionInfo. The
* array element count is passed in \a sessionCount, and \a sessionCount is used to return the number of sessions
* written to the buffer.
*
* If the supplied buffer is not large enough to accommodate the active session array, the function returns
* HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlFBCSessionInfo_t array required in \a sessionCount.
* To query the number of active FBC sessions, call this function with *sessionCount = 0.  The code will return
* HGML_SUCCESS with number of active FBC sessions updated in *sessionCount.
*
* For &tm; or newer fully supported devices.
*
* @note hResolution, vResolution, averageFPS and averageLatency data for a FBC session returned in \a sessionInfo may
*       be zero if there are no new frames captured since the session started.
*
* @param device                            The identifier of the target device
* @param sessionCount                      Reference to caller supplied array size, and returns the number of sessions.
* @param sessionInfo                       Reference in which to return the session information
*
* @return
*         - \ref HGML_SUCCESS                  if \a sessionInfo is fetched
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a sessionCount is too small, array element count is returned in \a sessionCount
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a sessionCount is NULL.
*         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlDeviceGetFBCSessions(hgmlDevice_t device, unsigned int *sessionCount, hgmlFBCSessionInfo_t *sessionInfo);

/**
 * Retrieves the current and pending driver model for the device.
 *
 * For &tm; or newer fully supported devices.
 * For windows only.
 *
 * On Windows platforms the device driver can run in either WDDM or WDM (TCC) mode. If a display is attached
 * to the device it must run in WDDM mode. TCC mode is preferred if a display is not attached.
 *
 * See \ref hgmlDriverModel_t for details on available driver models.
 *
 * @param device                               The identifier of the target device
 * @param current                              Reference in which to return the current driver model
 * @param pending                              Reference in which to return the pending driver model
 *
 * @return
 *         - \ref HGML_SUCCESS                 if either \a current and/or \a pending have been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or both \a current and \a pending are NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the platform is not windows
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceSetDriverModel()
 */
hgmlReturn_t hgmlDeviceGetDriverModel(hgmlDevice_t device, hgmlDriverModel_t *current, hgmlDriverModel_t *pending);

/**
 * Get VBIOS version of the device.
 *
 * For all products.
 *
 * The VBIOS version may change from time to time. It will not exceed 32 characters in length
 * (including the NULL terminator).  See \ref hgmlConstants::HGML_DEVICE_VBIOS_VERSION_BUFFER_SIZE.
 *
 * @param device                               The identifier of the target device
 * @param version                              Reference to which to return the VBIOS version
 * @param length                               The maximum allowed length of the string returned in \a version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a version is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVbiosVersion(hgmlDevice_t device, char *version, unsigned int length);

/**
 * Get Bridge Chip Information for all the bridge chips on the board.
 *
 * For all fully supported products.
 * Only applicable to multi-GPU products.
 *
 * @param device                                The identifier of the target device
 * @param bridgeHierarchy                       Reference to the returned bridge chip Hierarchy
 *
 * @return
 *         - \ref HGML_SUCCESS                 if bridge chip exists
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a bridgeInfo is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if bridge chip not supported on the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceGetBridgeChipInfo(hgmlDevice_t device, hgmlBridgeChipHierarchy_t *bridgeHierarchy);

/**
 * Get information about processes with a compute context on a device
 *
 * For &tm; or newer fully supported devices.
 *
 * This function returns information only about compute running processes (e.g. HGGC application which have
 * active context). Any graphics applications (e.g. using OpenGL, DirectX) won't be listed by this function.
 *
 * To query the current number of running compute processes, call this function with *infoCount = 0. The
 * return code will be HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if none are running. For this call
 * \a infos is allowed to be NULL.
 *
 * The usedGpuMemory field returned is all of the memory used by the application.
 *
 * Keep in mind that information returned by this call is dynamic and the number of elements might change in
 * time. Allocate more space for \a infos table in case new compute processes are spawned.
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate information, only if
 *       the caller has appropriate privileges. Per-instance information can be queried by using
 *       specific MIG device handles.
 *       Querying per-instance information using MIG device handles is not supported if the device is in vGPU Host virtualization mode.
 *
 * @param device                               The device handle or MIG device handle
 * @param infoCount                            Reference in which to provide the \a infos array size, and
 *                                             to return the number of returned elements
 * @param infos                                Reference in which to return the process information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a infoCount and \a infos have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a infoCount indicates that the \a infos array is too small
 *                                             \a infoCount will contain minimal amount of space necessary for
 *                                             the call to complete
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, either of \a infoCount or \a infos is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by \a device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see \ref hgmlSystemGetProcessName
 */
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);

/**
 * Get information about processes with a graphics context on a device
 *
 * For &tm; or newer fully supported devices.
 *
 * This function returns information only about graphics based processes
 * (eg. applications using OpenGL, DirectX)
 *
 * To query the current number of running graphics processes, call this function with *infoCount = 0. The
 * return code will be HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if none are running. For this call
 * \a infos is allowed to be NULL.
 *
 * The usedGpuMemory field returned is all of the memory used by the application.
 *
 * Keep in mind that information returned by this call is dynamic and the number of elements might change in
 * time. Allocate more space for \a infos table in case new graphics processes are spawned.
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate information, only if
 *       the caller has appropriate privileges. Per-instance information can be queried by using
 *       specific MIG device handles.
 *       Querying per-instance information using MIG device handles is not supported if the device is in vGPU Host virtualization mode.
 *
 * @param device                               The device handle or MIG device handle
 * @param infoCount                            Reference in which to provide the \a infos array size, and
 *                                             to return the number of returned elements
 * @param infos                                Reference in which to return the process information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a infoCount and \a infos have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a infoCount indicates that the \a infos array is too small
 *                                             \a infoCount will contain minimal amount of space necessary for
 *                                             the call to complete
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, either of \a infoCount or \a infos is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by \a device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see \ref hgmlSystemGetProcessName
 */
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);

/**
 * Get information about processes with a MPS compute context on a device
 *
 * For Volta &tm; or newer fully supported devices.
 *
 * This function returns information only about compute running processes (e.g. HGGC application which have
 * active context) utilizing MPS. Any graphics applications (e.g. using OpenGL, DirectX) won't be listed by
 * this function.
 *
 * To query the current number of running compute processes, call this function with *infoCount = 0. The
 * return code will be HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if none are running. For this call
 * \a infos is allowed to be NULL.
 *
 * The usedGpuMemory field returned is all of the memory used by the application.
 *
 * Keep in mind that information returned by this call is dynamic and the number of elements might change in
 * time. Allocate more space for \a infos table in case new compute processes are spawned.
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate information, only if
 *       the caller has appropriate privileges. Per-instance information can be queried by using
 *       specific MIG device handles.
 *       Querying per-instance information using MIG device handles is not supported if the device is in vGPU Host virtualization mode.
 *
 * @param device                               The device handle or MIG device handle
 * @param infoCount                            Reference in which to provide the \a infos array size, and
 *                                             to return the number of returned elements
 * @param infos                                Reference in which to return the process information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a infoCount and \a infos have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a infoCount indicates that the \a infos array is too small
 *                                             \a infoCount will contain minimal amount of space necessary for
 *                                             the call to complete
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, either of \a infoCount or \a infos is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by \a device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see \ref hgmlSystemGetProcessName
 */
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses_v3(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_t *infos);

/**
 * Get information about running processes on a device for input context
 *
 * This function returns information only about running processes (e.g. CUDA application which have
 * active context).
 *
 * To determine the size of the @ref plist->procArray array to allocate, call the function with
 * @ref plist->numProcArrayEntries set to zero and @ref plist->procArray set to NULL. The return
 * code will be either HGML_ERROR_INSUFFICIENT_SIZE (if there are valid processes of type
 * @ref plist->mode to report on, in which case the @ref plist->numProcArrayEntries field will
 * indicate the required number of entries in the array) or HGML_SUCCESS (if no processes of type
 * @ref plist->mode exist).
 *
 * The usedGpuMemory field returned is all of the memory used by the application.
 * The usedGpuCcProtectedMemory field returned is all of the protected memory used by the application.
 *
 * Keep in mind that information returned by this call is dynamic and the number of elements might change in
 * time. Allocate more space for \a plist->procArray table in case new processes are spawned.
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate information, only if
 *       the caller has appropriate privileges. Per-instance information can be queried by using
 *       specific MIG device handles.
 *       Querying per-instance information using MIG device handles is not supported if the device is in
 *       vGPU Host virtualization mode.
 *       Protected memory usage is currently not available in MIG mode and in windows.
 *
 * @param device                               The device handle or MIG device handle
 * @param plist                                Reference in which to process detail list
 * @param plist->version                       The api version
 * @param plist->mode                          The process mode
 * @param plist->procArray                     Reference in which to return the process information
 * @param plist->numProcArrayEntries           Proc array size of returned entries
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a plist->numprocArrayEntries and \a plist->procArray have been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a plist->numprocArrayEntries indicates that the \a plist->procArray is too small
 *                                             \a plist->numprocArrayEntries will contain minimal amount of space necessary for
 *                                             the call to complete
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a plist is NULL, \a plist->version is invalid,
 *                                             \a plist->mode is invalid,
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by \a device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceGetRunningProcessDetailList(hgmlDevice_t device, hgmlProcessDetailList_t *plist);

/**
 * Check if the GPU devices are on the same physical board.
 *
 * For all fully supported products.
 *
 * @param device1                               The first GPU device
 * @param device2                               The second GPU device
 * @param onSameBoard                           Reference in which to return the status.
 *                                              Non-zero indicates that the GPUs are on the same board.
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a onSameBoard has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a dev1 or \a dev2 are invalid or \a onSameBoard is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this check is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the either GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceOnSameBoard(hgmlDevice_t device1, hgmlDevice_t device2, int *onSameBoard);

/**
 * Retrieves the root/admin permissions on the target API. See \a hgmlRestrictedAPI_t for the list of supported APIs.
 * If an API is restricted only root users can call that API. See \a hgmlDeviceSetAPIRestriction to change current permissions.
 *
 * For all fully supported products.
 *
 * @param device                               The identifier of the target device
 * @param apiType                              Target API type for this operation
 * @param isRestricted                         Reference in which to return the current restriction
 *                                             HGML_FEATURE_ENABLED indicates that the API is root-only
 *                                             HGML_FEATURE_DISABLED indicates that the API is accessible to all users
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a isRestricted has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a apiType incorrect or \a isRestricted is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device or the device does not support
 *                                                 the feature that is being queried (E.G. Enabling/disabling Auto Boosted clocks is
 *                                                 not supported by the device)
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlRestrictedAPI_t
 */
hgmlReturn_t hgmlDeviceGetAPIRestriction(hgmlDevice_t device, hgmlRestrictedAPI_t apiType, hgmlEnableState_t *isRestricted);

/**
 * Gets recent samples for the GPU.
 *
 * For &tm; or newer fully supported devices.
 *
 * Based on type, this method can be used to fetch the power, utilization or clock samples maintained in the buffer by
 * the driver.
 *
 * Power, Utilization and Clock samples are returned as type "unsigned int" for the union hgmlValue_t.
 *
 * To get the size of samples that user needs to allocate, the method is invoked with samples set to NULL.
 * The returned samplesCount will provide the number of samples that can be queried. The user needs to
 * allocate the buffer with size as samplesCount * sizeof(hgmlSample_t).
 *
 * lastSeenTimeStamp represents CPU timestamp in microseconds. Set it to 0 to fetch all the samples maintained by the
 * underlying buffer. Set lastSeenTimeStamp to one of the timeStamps retrieved from the date of the previous query
 * to get more recent samples.
 *
 * This method fetches the number of entries which can be accommodated in the provided samples array, and the
 * reference samplesCount is updated to indicate how many samples were actually retrieved. The advantage of using this
 * method for samples in contrast to polling via existing methods is to get get higher frequency data at lower polling cost.
 *
 * @note On MIG-enabled GPUs, querying the following sample types, HGML_GPU_UTILIZATION_SAMPLES, HGML_MEMORY_UTILIZATION_SAMPLES
 *       HGML_ENC_UTILIZATION_SAMPLES and HGML_DEC_UTILIZATION_SAMPLES, is not currently supported.
 *
 * @param device                        The identifier for the target device
 * @param type                          Type of sampling event
 * @param lastSeenTimeStamp             Return only samples with timestamp greater than lastSeenTimeStamp.
 * @param sampleValType                 Output parameter to represent the type of sample value as described in hgmlSampleVal_t
 * @param sampleCount                   Reference to provide the number of elements which can be queried in samples array
 * @param samples                       Reference in which samples are returned

 * @return
 *         - \ref HGML_SUCCESS                 if samples are successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a samplesCount is NULL or
 *                                             reference to \a sampleCount is 0 for non null \a samples
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_FOUND         if sample entries are not found
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetSamples(hgmlDevice_t device, hgmlSamplingType_t type, unsigned long long lastSeenTimeStamp,
        hgmlValueType_t *sampleValType, unsigned int *sampleCount, hgmlSample_t *samples);

/**
 * Gets Total, Available and Used size of BAR1 memory.
 *
 * BAR1 is used to map the FB (device memory) so that it can be directly accessed by the CPU or by 3rd party
 * devices (peer-to-peer on the PCIE bus).
 *
 * @note In MIG mode, if device handle is provided, the API returns aggregate
 *       information, only if the caller has appropriate privileges. Per-instance
 *       information can be queried by using specific MIG device handles.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param bar1Memory                           Reference in which BAR1 memory
 *                                             information is returned.
 *
 * @return
 *         - \ref HGML_SUCCESS                 if BAR1 memory is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a bar1Memory is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceGetBAR1MemoryInfo(hgmlDevice_t device, hgmlBAR1Memory_t *bar1Memory);

/**
 * Gets the duration of time during which the device was throttled (lower than requested clocks) due to power
 * or thermal constraints.
 *
 * The method is important to users who are tying to understand if their GPUs throttle at any point during their applications. The
 * difference in violation times at two different reference times gives the indication of GPU throttling event.
 *
 * Violation for thermal capping is not supported at this time.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param perfPolicyType                       Represents Performance policy which can trigger GPU throttling
 * @param violTime                             Reference to which violation time related information is returned
 *
 *
 * @return
 *         - \ref HGML_SUCCESS                 if violation time is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a perfPolicyType is invalid, or \a violTime is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetViolationStatus(hgmlDevice_t device, hgmlPerfPolicyType_t perfPolicyType, hgmlViolationTime_t *violTime);

/**
 * Gets the device's interrupt number
 *
 * @param device                               The identifier of the target device
 * @param irqNum                               The interrupt number associated with the specified device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if irq number is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a irqNum is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetIrqNum(hgmlDevice_t device, unsigned int *irqNum);

/**
 * Gets the device's core count
 *
 * @param device                               The identifier of the target device
 * @param numCores                             The number of cores for the specified device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if Gpu core count is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a numCores is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetNumGpuCores(hgmlDevice_t device, unsigned int *numCores);

/**
 * Gets the devices power source
 *
 * @param device                               The identifier of the target device
 * @param powerSource                          The power source of the device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the current power source was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a powerSource is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetPowerSource(hgmlDevice_t device, hgmlPowerSource_t *powerSource);

/**
 * Gets the device's memory bus width
 *
 * @param device                               The identifier of the target device
 * @param busWidth                             The devices's memory bus width
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the memory bus width is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a busWidth is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetMemoryBusWidth(hgmlDevice_t device, unsigned int *busWidth);

/**
 * Gets the device's PCIE Max Link speed in MBPS
 *
 * @param device                               The identifier of the target device
 * @param maxSpeed                             The devices's PCIE Max Link speed in MBPS
 *
 * @return
 *         - \ref HGML_SUCCESS                 if Pcie Max Link Speed is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a maxSpeed is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetPcieLinkMaxSpeed(hgmlDevice_t device, unsigned int *maxSpeed);

/**
 * Gets the device's PCIe Link speed in Mbps
 *
 * @param device                               The identifier of the target device
 * @param pcieSpeed                            The devices's PCIe Max Link speed in Mbps
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pcieSpeed has been retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a pcieSpeed is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support PCIe speed getting
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPcieSpeed(hgmlDevice_t device, unsigned int *pcieSpeed);

/**
 * Gets the device's Adaptive Clock status
 *
 * @param device                               The identifier of the target device
 * @param adaptiveClockStatus                  The current adaptive clocking status, either
 *                                             @ref HGML_ADAPTIVE_CLOCKING_INFO_STATUS_DISABLED
 *                                             or @ref HGML_ADAPTIVE_CLOCKING_INFO_STATUS_ENABLED
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the current adaptive clocking status is successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, or \a adaptiveClockStatus is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *
 */
hgmlReturn_t hgmlDeviceGetAdaptiveClockInfoStatus(hgmlDevice_t device, unsigned int *adaptiveClockStatus);

/**
 * Get the type of the GPU Bus (PCIe, PCI, ...)
 *
 * @param device                               The identifier of the target device
 * @param type                                 The PCI Bus type
 *
 * return
 *         - \ref HGML_SUCCESS                 if the bus \a type is successfully retreived
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \device is invalid or \type is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetBusType(hgmlDevice_t device, hgmlBusType_t *type);

/**
 * Get fabric information associated with the device.
 *
 * On systems, GPU is registered with the alixpu Fabric Manager
 * Upon successful registration, the GPU is added to the ICNLink fabric to enable
 * peer-to-peer communication.
 * This API reports the current state of the GPU in the ICNLink fabric
 * along with other useful information.
 *
 * @param device                               The identifier of the target device
 * @param gpuFabricInfo                        Information about GPU fabric state
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't support gpu fabric
 */
hgmlReturn_t hgmlDeviceGetGpuFabricInfo(hgmlDevice_t device, hgmlGpuFabricInfo_t *gpuFabricInfo);

/**
 * Get Conf Computing System capabilities.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param capabilities                         System CC capabilities
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a capabilities were successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a capabilities is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlSystemGetConfComputeCapabilities(hgmlConfComputeSystemCaps_t *capabilities);

/**
 * Get Conf Computing System State.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param state                                System CC State
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a state were successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a state is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlSystemGetConfComputeState(hgmlConfComputeSystemState_t *state);

/**
 * Get Conf Computing Protected and Unprotected Memory Sizes.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param device                               Device handle
 * @param memInfo                              Protected/Unprotected Memory sizes
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a memInfo were successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a memInfo or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlDeviceGetConfComputeMemSizeInfo(hgmlDevice_t device, hgmlConfComputeMemSizeInfo_t *memInfo);

/**
 * Get Conf Computing GPUs ready state.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param isAcceptingWork                      Returns GPU current work accepting state,
 *                                             HGML_CC_ACCEPTING_CLIENT_REQUESTS_TRUE or
 *                                             HGML_CC_ACCEPTING_CLIENT_REQUESTS_FALSE
 *
 * return
 *         - \ref HGML_SUCCESS                 if \a current GPUs ready state were successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a isAcceptingWork is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlSystemGetConfComputeGpusReadyState(unsigned int *isAcceptingWork);

/**
 * Get Conf Computing protected memory usage.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param device                               The identifier of the target device
 * @param memory                               Reference in which to return the memory information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a memory has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a memory is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetConfComputeProtectedMemoryUsage(hgmlDevice_t device, hgmlMemory_t *memory);

/**
 * Get Conf Computing Gpu certificate details.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param device                               The identifier of the target device
 * @param gpuCert                              Reference in which to return the gpu certificate information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a gpu certificate info has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a memory is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetConfComputeGpuCertificate(hgmlDevice_t device,
                                                            hgmlConfComputeGpuCertificate_t *gpuCert);

/**
 * Get Conf Computing Gpu attestation report.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param device                               The identifier of the target device
 * @param gpuAtstReport                        Reference in which to return the gpu attestation report
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a gpu attestation report has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a memory is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetConfComputeGpuAttestationReport(hgmlDevice_t device,
                                                                  hgmlConfComputeGpuAttestationReport_t *gpuAtstReport);

/**
 * Retrieve GSP firmware version.
 *
 * The caller passes in buffer via \a version and corresponding GSP firmware numbered version
 * is returned with the same parameter in string format.
 *
 * @param device                               Device handle
 * @param version                              The retrieved GSP firmware version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if GSP firmware version is sucessfully retrieved
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or GSP \a version pointer is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if GSP firmware is not enabled for GPU
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGspFirmwareVersion(hgmlDevice_t device, char *version);

/**
 * Retrieve GSP firmware mode.
 *
 * The caller passes in integer pointers. GSP firmware enablement and default mode information is returned with
 * corresponding parameters. The return value in \a isEnabled and \a defaultMode should be treated as boolean.
 *
 * @param device                               Device handle
 * @param isEnabled                            Pointer to specify if GSP firmware is enabled
 * @param defaultMode                          Pointer to specify if GSP firmware is supported by default on \a device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if GSP firmware mode is sucessfully retrieved
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or any of \a isEnabled or \a defaultMode is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGspFirmwareMode(hgmlDevice_t device, unsigned int *isEnabled, unsigned int *defaultMode);

/**
 * @}
 */

/** @addtogroup hgmlAccountingStats
 *  @{
 */

/**
 * Queries the state of per process accounting mode.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlDeviceGetAccountingStats for more details.
 * See \ref hgmlDeviceSetAccountingMode
 *
 * @param device                               The identifier of the target device
 * @param mode                                 Reference in which to return the current accounting mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the mode has been successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode are NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetAccountingMode(hgmlDevice_t device, hgmlEnableState_t *mode);

/**
 * Queries process's accounting stats.
 *
 * For &tm; or newer fully supported devices.
 *
 * Accounting stats capture GPU utilization and other statistics across the lifetime of a process.
 * Accounting stats can be queried during life time of the process and after its termination.
 * The time field in \ref hgmlAccountingStats_t is reported as 0 during the lifetime of the process and
 * updated to actual running time after its termination.
 * Accounting stats are kept in a circular buffer, newly created processes overwrite information about old
 * processes.
 *
 * See \ref hgmlAccountingStats_t for description of each returned metric.
 * List of processes that can be queried can be retrieved from \ref hgmlDeviceGetAccountingPids.
 *
 * @note Accounting Mode needs to be on. See \ref hgmlDeviceGetAccountingMode.
 * @note Only compute and graphics applications stats can be queried. Monitoring applications stats can't be
 *         queried since they don't contribute to GPU utilization.
 * @note In case of pid collision stats of only the latest process (that terminated last) will be reported
 *
 * @warning On devices per process statistics are accurate only if there's one process running on a GPU.
 *
 * @param device                               The identifier of the target device
 * @param pid                                  Process Id of the target process to query stats for
 * @param stats                                Reference in which to return the process's accounting stats
 *
 * @return
 *         - \ref HGML_SUCCESS                 if stats have been successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a stats are NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if process stats were not found
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if \a device doesn't support this feature or accounting mode is disabled
 *                                              or on vGPU host.
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetAccountingBufferSize
 */
hgmlReturn_t hgmlDeviceGetAccountingStats(hgmlDevice_t device, unsigned int pid, hgmlAccountingStats_t *stats);

/**
 * Queries list of processes that can be queried for accounting stats. The list of processes returned
 * can be in running or terminated state.
 *
 * For &tm; or newer fully supported devices.
 *
 * To just query the number of processes ready to be queried, call this function with *count = 0 and
 * pids=NULL. The return code will be HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if list is empty.
 *
 * For more details see \ref hgmlDeviceGetAccountingStats.
 *
 * @note In case of PID collision some processes might not be accessible before the circular buffer is full.
 *
 * @param device                               The identifier of the target device
 * @param count                                Reference in which to provide the \a pids array size, and
 *                                               to return the number of elements ready to be queried
 * @param pids                                 Reference in which to return list of process ids
 *
 * @return
 *         - \ref HGML_SUCCESS                 if pids were successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a count is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if \a device doesn't support this feature or accounting mode is disabled
 *                                              or on vGPU host.
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a count is too small (\a count is set to
 *                                                 expected value)
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetAccountingBufferSize
 */
hgmlReturn_t hgmlDeviceGetAccountingPids(hgmlDevice_t device, unsigned int *count, unsigned int *pids);

/**
 * Returns the number of processes that the circular buffer with accounting pids can hold.
 *
 * For &tm; or newer fully supported devices.
 *
 * This is the maximum number of processes that accounting information will be stored for before information
 * about oldest processes will get overwritten by information about new processes.
 *
 * @param device                               The identifier of the target device
 * @param bufferSize                           Reference in which to provide the size (in number of elements)
 *                                               of the circular buffer for accounting stats.
 *
 * @return
 *         - \ref HGML_SUCCESS                 if buffer size was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a bufferSize is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature or accounting mode is disabled
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetAccountingStats
 * @see hgmlDeviceGetAccountingPids
 */
hgmlReturn_t hgmlDeviceGetAccountingBufferSize(hgmlDevice_t device, unsigned int *bufferSize);

/** @} */

/** @addtogroup hgmlDeviceQueries
 *  @{
 */

/**
 * Returns the list of retired pages by source, including pages that are pending retirement
 * The address information provided from this API is the hardware address of the page that was retired.  Note
 * that this does not match the virtual address used in HGGC, but will match the address information in XID 63
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param cause                             Filter page addresses by cause of retirement
 * @param pageCount                         Reference in which to provide the \a addresses buffer size, and
 *                                          to return the number of retired pages that match \a cause
 *                                          Set to 0 to query the size without allocating an \a addresses buffer
 * @param addresses                         Buffer to write the page addresses into
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pageCount was populated and \a addresses was filled
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a pageCount indicates the buffer is not large enough to store all the
 *                                             matching page addresses.  \a pageCount is set to the needed size.
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a pageCount is NULL, \a cause is invalid, or
 *                                             \a addresses is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetRetiredPages(hgmlDevice_t device, hgmlPageRetirementCause_t cause,
    unsigned int *pageCount, unsigned long long *addresses);


/**
 * Returns the list of retired pages by source, including pages that are pending retirement
 * The address information provided from this API is the hardware address of the page that was retired.  Note
 * that this does not match the virtual address used in HGGC, but will match the address information in XID 63
 *
 * \note hgmlDeviceGetRetiredPages_v2 adds an additional timestamps parameter to return the time of each page's
 *       retirement.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param cause                             Filter page addresses by cause of retirement
 * @param pageCount                         Reference in which to provide the \a addresses buffer size, and
 *                                          to return the number of retired pages that match \a cause
 *                                          Set to 0 to query the size without allocating an \a addresses buffer
 * @param addresses                         Buffer to write the page addresses into
 * @param timestamps                        Buffer to write the timestamps of page retirement, additional for _v2
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pageCount was populated and \a addresses was filled
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a pageCount indicates the buffer is not large enough to store all the
 *                                             matching page addresses.  \a pageCount is set to the needed size.
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a pageCount is NULL, \a cause is invalid, or
 *                                             \a addresses is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetRetiredPages_v2(hgmlDevice_t device, hgmlPageRetirementCause_t cause,
    unsigned int *pageCount, unsigned long long *addresses, unsigned long long *timestamps);

/**
 * Check if any pages are pending retirement and need a reboot to fully retire.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                            The identifier of the target device
 * @param isPending                         Reference in which to return the pending status
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a isPending was populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a isPending is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetRetiredPagesPendingStatus(hgmlDevice_t device, hgmlEnableState_t *isPending);

/**
 * Get number of remapped rows. The number of rows reported will be based on
 * the cause of the remapping. isPending indicates whether or not there are
 * pending remappings. A reset will be required to actually remap the row.
 * failureOccurred will be set if a row remapping ever failed in the past. A
 * pending remapping won't affect future work on the GPU since
 * error-containment and dynamic page blacklisting will take care of that.
 *
 * @note On MIG-enabled GPUs with active instances, querying the number of
 * remapped rows is not supported
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param corrRows                             Reference for number of rows remapped due to correctable errors
 * @param uncRows                              Reference for number of rows remapped due to uncorrectable errors
 * @param isPending                            Reference for whether or not remappings are pending
 * @param failureOccurred                      Reference that is set when a remapping has failed in the past
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a corrRows, \a uncRows, \a isPending or \a failureOccurred is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If MIG is enabled or if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           Unexpected error
 */
hgmlReturn_t hgmlDeviceGetRemappedRows(hgmlDevice_t device, unsigned int *corrRows, unsigned int *uncRows,
                                               unsigned int *isPending, unsigned int *failureOccurred);

/**
 * Get the row remapper histogram. Returns the remap availability for each bank
 * on the GPU.
 *
 * @param device                               Device handle
 * @param values                               Histogram values
 *
 * @return
 *        - \ref HGML_SUCCESS                  On success
 *        - \ref HGML_ERROR_UNKNOWN            On any unexpected error
 */
hgmlReturn_t hgmlDeviceGetRowRemapperHistogram(hgmlDevice_t device, hgmlRowRemapperHistogramValues_t *values);

/**
 * Get architecture for device
 *
 * @param device                               The identifier of the target device
 * @param arch                                 Reference where architecture is returned, if call successful.
 *                                             Set to HGML_DEVICE_ARCH_* upon success
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device or \a arch (output refererence) are invalid
 */
hgmlReturn_t hgmlDeviceGetArchitecture(hgmlDevice_t device, hgmlDeviceArchitecture_t *arch);

/**
 * Retrieves the frequency monitor fault status for the device.
 *
 * For &tm; or newer fully supported devices.
 * Requires root user.
 *
 * See \ref hgmlClkMonStatus_t for details on decoding the status output.
 *
 * @param device                               The identifier of the target device
 * @param status                               Reference in which to return the clkmon fault status
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a status has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a status is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetClkMonStatus()
 */
hgmlReturn_t hgmlDeviceGetClkMonStatus(hgmlDevice_t device, hgmlClkMonStatus_t *status);

/**
 * Retrieves the current utilization and process ID
 *
 * For &tm; or newer fully supported devices.
 *
 * Reads recent utilization of GPU SM (3D/Compute), framebuffer, video encoder, and video decoder for processes running.
 * Utilization values are returned as an array of utilization sample structures in the caller-supplied buffer pointed at
 * by \a utilization. One utilization sample structure is returned per process running, that had some non-zero utilization
 * during the last sample period. It includes the CPU timestamp at which  the samples were recorded. Individual utilization values
 * are returned as "unsigned int" values. If no valid sample entries are found since the lastSeenTimeStamp, HGML_ERROR_NOT_FOUND
 * is returned.
 *
 * To read utilization values, first determine the size of buffer required to hold the samples by invoking the function with
 * \a utilization set to NULL. The caller should allocate a buffer of size
 * processSamplesCount * sizeof(hgmlProcessUtilizationSample_t). Invoke the function again with the allocated buffer passed
 * in \a utilization, and \a processSamplesCount set to the number of entries the buffer is sized for.
 *
 * On successful return, the function updates \a processSamplesCount with the number of process utilization sample
 * structures that were actually written. This may differ from a previously read value as instances are created or
 * destroyed.
 *
 * lastSeenTimeStamp represents the CPU timestamp in microseconds at which utilization samples were last read. Set it to 0
 * to read utilization based on all the samples maintained by the driver's internal sample buffer. Set lastSeenTimeStamp
 * to a timeStamp retrieved from a previous query to read utilization since the previous query.
 *
 * @note On MIG-enabled GPUs, querying process utilization is not currently supported.
 *
 * @param device                    The identifier of the target device
 * @param utilization               Pointer to caller-supplied buffer in which guest process utilization samples are returned
 * @param processSamplesCount       Pointer to caller-supplied array size, and returns number of processes running
 * @param lastSeenTimeStamp         Return only samples with timestamp greater than lastSeenTimeStamp.

 * @return
 *         - \ref HGML_SUCCESS                 if \a utilization has been populated
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a utilization is NULL, or \a samplingPeriodUs is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_NOT_FOUND         if sample entries are not found
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetProcessUtilization(hgmlDevice_t device, hgmlProcessUtilizationSample_t *utilization,
                                              unsigned int *processSamplesCount, unsigned long long lastSeenTimeStamp);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlUnitCommands Unit Commands
 *  This chapter describes HGML operations that change the state of the unit. For S-class products.
 *  Each of these requires root/admin access. Non-admin users will see an HGML_ERROR_NO_PERMISSION
 *  error code when invoking any of these methods.
 *  @{
 */
/***************************************************************************************************/

/**
 * Set the LED state for the unit. The LED can be either green (0) or amber (1).
 *
 * For S-class products.
 * Requires root/admin permissions.
 *
 * This operation takes effect immediately.
 *
 *
 * <b>Current S-Class products don't provide unique LEDs for each unit. As such, both front
 * and back LEDs will be toggled in unison regardless of which unit is specified with this command.</b>
 *
 * See \ref hgmlLedColor_t for available colors.
 *
 * @param unit                                 The identifier of the target unit
 * @param color                                The target LED color
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the LED color has been set
 *         - \re


f HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a unit or \a color is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this is not an S-class product
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlUnitGetLedState()
 */
hgmlReturn_t hgmlUnitSetLedState(hgmlUnit_t unit, hgmlLedColor_t color);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlDeviceCommands Device Commands
 *  This chapter describes HGML operations that change the state of the device.
 *  Each of these requires root/admin access. Non-admin users will see an HGML_ERROR_NO_PERMISSION
 *  error code when invoking any of these methods.
 *  @{
 */
/***************************************************************************************************/

/**
 * Set the persistence mode for the device.
 *
 * For all products.
 * For Linux only.
 * Requires root/admin permissions.
 *
 * The persistence mode determines whether the GPU driver software is torn down after the last client
 * exits.
 *
 * This operation takes effect immediately. It is not persistent across reboots. After each reboot the
 * persistence mode is reset to "Disabled".
 *
 * See \ref hgmlEnableState_t for available modes.
 *
 * After calling this API with mode set to HGML_FEATURE_DISABLED on a device that has its own NUMA
 * memory, the given device handle will no longer be valid, and to continue to interact with this
 * device, a new handle should be obtained from one of the hgmlDeviceGetHandleBy*() APIs. This
 * limitation is currently only applicable to devices that have a coherent ICNLink connection to
 * system memory.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 The target persistence mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the persistence mode was set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetPersistenceMode()
 */
hgmlReturn_t hgmlDeviceSetPersistenceMode(hgmlDevice_t device, hgmlEnableState_t mode);

/**
 * Set the compute mode for the device.
 *
 * For all products.
 * Requires root/admin permissions.
 *
 * The compute mode determines whether a GPU can be used for compute operations and whether it can
 * be shared across contexts.
 *
 * This operation takes effect immediately. Under Linux it is not persistent across reboots and
 * always resets to "Default". Under windows it is persistent.
 *
 * Under windows compute mode may only be set to DEFAULT when running in WDDM
 *
 * @note On MIG-enabled GPUs, compute mode would be set to DEFAULT and changing it is not supported.
 *
 * See \ref hgmlComputeMode_t for details on available compute modes.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 The target compute mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the compute mode was set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetComputeMode()
 */
hgmlReturn_t hgmlDeviceSetComputeMode(hgmlDevice_t device, hgmlComputeMode_t mode);

/**
 * Set the ECC mode for the device.
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher.
 * Requires root/admin permissions.
 *
 * The ECC mode determines whether the GPU enables its ECC support.
 *
 * This operation takes effect after the next reboot.
 *
 * See \ref hgmlEnableState_t for details on available modes.
 *
 * @param device                               The identifier of the target device
 * @param ecc                                  The target ECC mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the ECC mode was set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a ecc is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetEccMode()
 */
hgmlReturn_t hgmlDeviceSetEccMode(hgmlDevice_t device, hgmlEnableState_t ecc);

/**
 * Clear the ECC error and other memory error counts for the device.
 *
 * For &tm; or newer fully supported devices.
 * Only applicable to devices with ECC.
 * Requires \a HGML_INFOROM_ECC version 2.0 or higher to clear aggregate location-based ECC counts.
 * Requires \a HGML_INFOROM_ECC version 1.0 or higher to clear all other ECC counts.
 * Requires root/admin permissions.
 * Requires ECC Mode to be enabled.
 *
 * Sets all of the specified ECC counters to 0, including both detailed and total counts.
 *
 * This operation takes effect immediately.
 *
 * See \ref hgmlMemoryErrorType_t for details on available counter types.
 *
 * @param device                               The identifier of the target device
 * @param counterType                          Flag that indicates which type of errors should be cleared.
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the error counts were cleared
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a counterType is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see
 *      - hgmlDeviceGetDetailedEccErrors()
 *      - hgmlDeviceGetTotalEccErrors()
 */
hgmlReturn_t hgmlDeviceClearEccErrorCounts(hgmlDevice_t device, hgmlEccCounterType_t counterType);

/**
 * Set the driver model for the device.
 *
 * For &tm; or newer fully supported devices.
 * For windows only.
 * Requires root/admin permissions.
 *
 * On Windows platforms the device driver can run in either WDDM or WDM (TCC) mode. If a display is attached
 * to the device it must run in WDDM mode.
 *
 * It is possible to force the change to WDM (TCC) while the display is still attached with a force flag (hgmlFlagForce).
 * This should only be done if the host is subsequently powered down and the display is detached from the device
 * before the next reboot.
 *
 * This operation takes effect after the next reboot.
 *
 * Windows driver model may only be set to WDDM when running in DEFAULT compute mode.
 *
 * Change driver model to WDDM is not supported when GPU doesn't support graphics acceleration or
 * will not support it after reboot. See \ref hgmlDeviceSetGpuOperationMode.
 *
 * See \ref hgmlDriverModel_t for details on available driver models.
 * See \ref hgmlFlagDefault and \ref hgmlFlagForce
 *
 * @param device                               The identifier of the target device
 * @param driverModel                          The target driver model
 * @param flags                                Flags that change the default behavior
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the driver model has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a driverModel is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the platform is not windows or the device does not support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetDriverModel()
 */
hgmlReturn_t hgmlDeviceSetDriverModel(hgmlDevice_t device, hgmlDriverModel_t driverModel, unsigned int flags);

typedef enum hgmlClockLimitId_enum {
    HGML_CLOCK_LIMIT_ID_RANGE_START = 0xffffff00,
    HGML_CLOCK_LIMIT_ID_TDP,
    HGML_CLOCK_LIMIT_ID_UNLIMITED
} hgmlClockLimitId_t;

/**
 * Set clocks that device will lock to.
 *
 * Sets the clocks that the device will be running at to the value in the range of minGpuClockMHz to maxGpuClockMHz.
 * Setting this will supersede application clock values and take effect regardless if a hggc app is running.
 * See /ref hgmlDeviceSetApplicationsClocks
 *
 * Can be used as a setting to request constant performance.
 *
 * This can be called with a pair of integer clock frequencies in MHz, or a pair of /ref hgmlClockLimitId_t values.
 * See the table below for valid combinations of these values.
 *
 * minGpuClock | maxGpuClock | Effect
 * ------------+-------------+--------------------------------------------------
 *     tdp     |     tdp     | Lock clock to TDP
 *  unlimited  |     tdp     | Upper bound is TDP but clock may drift below this
 *     tdp     |  unlimited  | Lower bound is TDP but clock may boost above this
 *  unlimited  |  unlimited  | Unlocked (== hgmlDeviceResetGpuLockedClocks)
 *
 * If one arg takes one of these values, the other must be one of these values as
 * well. Mixed numeric and symbolic calls return HGML_ERROR_INVALID_ARGUMENT.
 *
 * Requires root/admin permissions.
 *
 * After system reboot or driver reload applications clocks go back to their default value.
 * See \ref hgmlDeviceResetGpuLockedClocks.
 *
 * For Volta &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param minGpuClockMHz                       Requested minimum gpu clock in MHz
 * @param maxGpuClockMHz                       Requested maximum gpu clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a minGpuClockMHz and \a maxGpuClockMHz
 *                                                 is not a valid clock combination
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetGpuLockedClocks(hgmlDevice_t device, unsigned int minGpuClockMHz, unsigned int maxGpuClockMHz);

/**
 * Resets the gpu clock to the default value
 *
 * This is the gpu clock that will be used after system reboot or driver reload.
 * Default values are idle clocks, but the current values can be changed using \ref hgmlDeviceSetApplicationsClocks.
 *
 * @see hgmlDeviceSetGpuLockedClocks
 *
 * For Volta &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceResetGpuLockedClocks(hgmlDevice_t device);

/**
 * Set memory clocks that device will lock to.
 *
 * Sets the device's memory clocks to the value in the range of minMemClockMHz to maxMemClockMHz.
 * Setting this will supersede application clock values and take effect regardless of whether a cuda app is running.
 * See /ref hgmlDeviceSetApplicationsClocks
 *
 * Can be used as a setting to request constant performance.
 *
 * Requires root/admin permissions.
 *
 * After system reboot or driver reload applications clocks go back to their default value.
 * See \ref hgmlDeviceResetMemoryLockedClocks.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param minMemClockMHz                       Requested minimum memory clock in MHz
 * @param maxMemClockMHz                       Requested maximum memory clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a minGpuClockMHz and \a maxGpuClockMHz
 *                                                 is not a valid clock combination
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetMemoryLockedClocks(hgmlDevice_t device, unsigned int minMemClockMHz, unsigned int maxMemClockMHz);

/**
 * Resets the memory clock to the default value
 *
 * This is the memory clock that will be used after system reboot or driver reload.
 * Default values are idle clocks, but the current values can be changed using \ref hgmlDeviceSetApplicationsClocks.
 *
 * @see hgmlDeviceSetMemoryLockedClocks
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceResetMemoryLockedClocks(hgmlDevice_t device);

/**
 * Set clocks that applications will lock to.
 *
 * Sets the clocks that compute and graphics applications will be running at.
 * e.g. HGGC driver requests these clocks during context creation which means this property
 * defines clocks at which HGGC applications will be running unless some overspec event
 * occurs (e.g. over power, over thermal or external HW brake).
 *
 * Can be used as a setting to request constant performance.
 *
 * On and newer hardware, this will automatically disable automatic boosting of clocks.
 *
 * On K80 and newer and GPUs, users desiring fixed performance should also call
 * \ref hgmlDeviceSetAutoBoostedClocksEnabled to prevent clocks from automatically boosting
 * above the clock value being set.
 *
 * For &tm; or newer fully supported devices and or newer devices.
 * Requires root/admin permissions.
 *
 * See \ref hgmlDeviceGetSupportedMemoryClocks and \ref hgmlDeviceGetSupportedGraphicsClocks
 * for details on how to list available clocks combinations.
 *
 * After system reboot or driver reload applications clocks go back to their default value.
 * See \ref hgmlDeviceResetApplicationsClocks.
 *
 * @param device                               The identifier of the target device
 * @param memClockMHz                          Requested memory clock in MHz
 * @param graphicsClockMHz                     Requested graphics clock in MHz
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a memClockMHz and \a graphicsClockMHz
 *                                                 is not a valid clock combination
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetApplicationsClocks(hgmlDevice_t device, unsigned int memClockMHz, unsigned int graphicsClockMHz);

/**
 * Resets the application clock to the default value
 *
 * This is the applications clock that will be used after system reboot or driver reload.
 * Default value is constant, but the current value an be changed using \ref hgmlDeviceSetApplicationsClocks.
 *
 * On and newer hardware, if clocks were previously locked with \ref hgmlDeviceSetApplicationsClocks,
 * this call will unlock clocks. This returns clocks their default behavior ofautomatically boosting above
 * base clocks as thermal limits allow.
 *
 * @see hgmlDeviceGetApplicationsClock
 * @see hgmlDeviceSetApplicationsClocks
 *
 * For &tm; or newer fully supported devices and or newer devices.
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if new settings were successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceResetApplicationsClocks(hgmlDevice_t device);

/**
 * Try to set the current state of Auto Boosted clocks on a device.
 *
 * For &tm; or newer fully supported devices.
 *
 * Auto Boosted clocks are enabled by default on some hardware, allowing the GPU to run at higher clock rates
 * to maximize performance as thermal limits allow. Auto Boosted clocks should be disabled if fixed clock
 * rates are desired.
 *
 * Non-root users may use this API by default but can be restricted by root from using this API by calling
 * \ref hgmlDeviceSetAPIRestriction with apiType=HGML_RESTRICTED_API_SET_AUTO_BOOSTED_CLOCKS.
 * Note: Persistence Mode is required to modify current Auto Boost settings, therefore, it must be enabled.
 *
 * On and newer hardware, Auto Boosted clocks are controlled through application clocks.
 * Use \ref hgmlDeviceSetApplicationsClocks and \ref hgmlDeviceResetApplicationsClocks to control Auto Boost
 * behavior.
 *
 * @param device                               The identifier of the target device
 * @param enabled                              What state to try to set Auto Boosted clocks of the target device to
 *
 * @return
 *         - \ref HGML_SUCCESS                 If the Auto Boosted clocks were successfully set to the state specified by \a enabled
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support Auto Boosted clocks
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceSetAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t enabled);

/**
 * Try to set the default state of Auto Boosted clocks on a device. This is the default state that Auto Boosted clocks will
 * return to when no compute running processes (e.g. CUDA application which have an active context) are running
 *
 * For &tm; or newer fully supported devices and or newer devices.
 * Requires root/admin permissions.
 *
 * Auto Boosted clocks are enabled by default on some hardware, allowing the GPU to run at higher clock rates
 * to maximize performance as thermal limits allow. Auto Boosted clocks should be disabled if fixed clock
 * rates are desired.
 *
 * On and newer hardware, Auto Boosted clocks are controlled through application clocks.
 * Use \ref hgmlDeviceSetApplicationsClocks and \ref hgmlDeviceResetApplicationsClocks to control Auto Boost
 * behavior.
 *
 * @param device                               The identifier of the target device
 * @param enabled                              What state to try to set default Auto Boosted clocks of the target device to
 * @param flags                                Flags that change the default behavior. Currently Unused.
 *
 * @return
 *         - \ref HGML_SUCCESS                 If the Auto Boosted clock's default state was successfully set to the state specified by \a enabled
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NO_PERMISSION     If the calling user does not have permission to change Auto Boosted clock's default state.
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support Auto Boosted clocks
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 */
hgmlReturn_t hgmlDeviceSetDefaultAutoBoostedClocksEnabled(hgmlDevice_t device, hgmlEnableState_t enabled, unsigned int flags);

/**
 * Sets the speed of the fan control policy to default.
 *
 * For all cuda-capable discrete products with fans
 *
 * @param device                        The identifier of the target device
 * @param fan                           The index of the fan, starting at zero
 *
 * return
 *         HGML_SUCCESS                 if speed has been adjusted
 *         HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         HGML_ERROR_INVALID_ARGUMENT  if device is invalid
 *         HGML_ERROR_NOT_SUPPORTED     if the device does not support this
 *                                      (doesn't have fans)
 *         HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetDefaultFanSpeed_v2(hgmlDevice_t device, unsigned int fan);

/**
 * Sets current fan control policy.
 *
 * For &tm; or newer fully supported devices.
 *
 * Requires privileged user.
 *
 * For all cuda-capable discrete products with fans
 *
 * device                               The identifier of the target \a device
 * policy                               The fan control \a policy to set
 *
 * return
 *         HGML_SUCCESS                 if \a policy has been set
 *         HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a policy is null or the \a fan given doesn't reference
 *                                            a fan that exists.
 *         HGML_ERROR_NOT_SUPPORTED     if the \a device is older than
 *         HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetFanControlPolicy(hgmlDevice_t device, unsigned int fan,
                                                   hgmlFanControlPolicy_t policy);

/**
 * Sets the temperature threshold for the GPU with the specified threshold type in degrees C.
 *
 * For &tm; or newer fully supported devices.
 *
 * See \ref hgmlTemperatureThresholds_t for details on available temperature thresholds.
 *
 * @param device                               The identifier of the target device
 * @param thresholdType                        The type of threshold value to be set
 * @param temp                                 Reference which hold the value to be set
 * @return
 *         - \ref HGML_SUCCESS                 if \a temp has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a thresholdType is invalid or \a temp is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not have a temperature sensor or is unsupported
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetTemperatureThreshold(hgmlDevice_t device, hgmlTemperatureThresholds_t thresholdType, int *temp);

/**
 * Set new power limit of this device.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * See \ref hgmlDeviceGetPowerManagementLimitConstraints to check the allowed ranges of values.
 *
 * \note Limit is not persistent across reboots or driver unloads.
 * Enable persistent mode to prevent driver from unloading when no application is using the device.
 *
 * @param device                               The identifier of the target device
 * @param limit                                Power management limit in milliwatts to set
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a limit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a defaultLimit is out of range
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceGetPowerManagementLimitConstraints
 * @see hgmlDeviceGetPowerManagementDefaultLimit
 */
hgmlReturn_t hgmlDeviceSetPowerManagementLimit(hgmlDevice_t device, unsigned int limit);

/**
 * Sets new GOM. See \a hgmlGpuOperationMode_t for details.
 *
 * Modes \ref HGML_GOM_LOW_DP and \ref HGML_GOM_ALL_ON are supported on fully supported products.
 * Requires root/admin permissions.
 *
 * Changing GOMs requires a reboot.
 * The reboot requirement might be removed in the future.
 *
 * Compute only GOMs don't support graphics acceleration. Under windows switching to these GOMs when
 * pending driver model is WDDM is not supported. See \ref hgmlDeviceSetDriverModel.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 Target GOM
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a mode has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a mode incorrect
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support GOM or specific mode
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlGpuOperationMode_t
 * @see hgmlDeviceGetGpuOperationMode
 */
hgmlReturn_t hgmlDeviceSetGpuOperationMode(hgmlDevice_t device, hgmlGpuOperationMode_t mode);

/**
 * Changes the root/admin restructions on certain APIs. See \a hgmlRestrictedAPI_t for the list of supported APIs.
 * This method can be used by a root/admin user to give non-root/admin access to certain otherwise-restricted APIs.
 * The new setting lasts for the lifetime of the driver; it is not persistent. See \a hgmlDeviceGetAPIRestriction
 * to query the current restriction settings.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * @param device                               The identifier of the target device
 * @param apiType                              Target API type for this operation
 * @param isRestricted                         The target restriction
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a isRestricted has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a apiType incorrect
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support changing API restrictions or the device does not support
 *                                                 the feature that api restrictions are being set for (E.G. Enabling/disabling auto
 *                                                 boosted clocks is not supported by the device)
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlRestrictedAPI_t
 */
hgmlReturn_t hgmlDeviceSetAPIRestriction(hgmlDevice_t device, hgmlRestrictedAPI_t apiType, hgmlEnableState_t isRestricted);

/**
 * Sets the speed of a specified fan.
 *
 * WARNING: This function changes the fan control policy to manual. It means that YOU have to monitor
 *          the temperature and adjust the fan speed accordingly.
 *          If you set the fan speed too low you can burn your GPU!
 *          Use hgmlDeviceSetDefaultFanSpeed_v2 to restore default control policy.
 *
 * For all cuda-capable discrete products with fans that are or Newer.
 *
 * device                                The identifier of the target device
 * fan                                   The index of the fan, starting at zero
 * speed                                 The target speed of the fan [0-100] in % of max speed
 *
 * return
 *        HGML_SUCCESS                   if the fan speed has been set
 *        HGML_ERROR_UNINITIALIZED       if the library has not been successfully initialized
 *        HGML_ERROR_INVALID_ARGUMENT    if the device is not valid, or the speed is outside acceptable ranges,
 *                                              or if the fan index doesn't reference an actual fan.
 *        HGML_ERROR_NOT_SUPPORTED       if the device is older than.
 *        HGML_ERROR_UNKNOWN             if there was an unexpected error.
 */
hgmlReturn_t hgmlDeviceSetFanSpeed_v2(hgmlDevice_t device, unsigned int fan, unsigned int speed);

/**
 * Set the GPCCLK VF offset value
 * @param[in]   device                         The identifier of the target device
 * @param[in]   offset                         The GPCCLK VF offset value to set
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetGpcClkVfOffset(hgmlDevice_t device, int offset);

/**
 * Set the MemClk (Memory Clock) VF offset value. It requires elevated privileges.
 * @param[in]   device                         The identifier of the target device
 * @param[in]   offset                         The MemClk VF offset value to set
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a offset has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a offset is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetMemClkVfOffset(hgmlDevice_t device, int offset);

/**
 * Set Conf Computing Unprotected Memory Size.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param device                               Device Handle
 * @param sizeKiB                              Unprotected Memory size to be set in KiB
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a sizeKiB successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlDeviceSetConfComputeUnprotectedMemSize(hgmlDevice_t device, unsigned long long sizeKiB);

/**
 * Set Conf Computing GPUs ready state.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux, Windows TCC.
 *
 * @param isAcceptingWork                      GPU accepting new work, HGML_CC_ACCEPTING_CLIENT_REQUESTS_TRUE or
 *                                             HGML_CC_ACCEPTING_CLIENT_REQUESTS_FALSE
 *
 * return
 *         - \ref HGML_SUCCESS                 if \a current GPUs ready state is successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a isAcceptingWork is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlSystemSetConfComputeGpusReadyState(unsigned int isAcceptingWork);

/**
 * @}
 */

/** @addtogroup hgmlAccountingStats
 *  @{
 */

/**
 * Enables or disables per process accounting.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * @note This setting is not persistent and will default to disabled after driver unloads.
 *       Enable persistence mode to be sure the setting doesn't switch off to disabled.
 *
 * @note Enabling accounting mode has no negative impact on the GPU performance.
 *
 * @note Disabling accounting clears all accounting pids information.
 *
 * @note On MIG-enabled GPUs, accounting mode would be set to DISABLED and changing it is not supported.
 *
 * See \ref hgmlDeviceGetAccountingMode
 * See \ref hgmlDeviceGetAccountingStats
 * See \ref hgmlDeviceClearAccountingPids
 *
 * @param device                               The identifier of the target device
 * @param mode                                 The target accounting mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the new mode has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a mode are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetAccountingMode(hgmlDevice_t device, hgmlEnableState_t mode);

/**
 * Clears accounting information about all processes that have already terminated.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * See \ref hgmlDeviceGetAccountingMode
 * See \ref hgmlDeviceGetAccountingStats
 * See \ref hgmlDeviceSetAccountingMode
 *
 * @param device                               The identifier of the target device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if accounting information has been cleared
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceClearAccountingPids(hgmlDevice_t device);

/** @} */

/***************************************************************************************************/
/** @defgroup ICNLink ICNLink Methods
 * This chapter describes methods that HGML can perform on ICNLINK enabled devices.
 *  @{
 */
/***************************************************************************************************/

/**
 * Retrieves the state of the device's ICNLink for the link specified
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param isActive                             \a hgmlEnableState_t where HGML_FEATURE_ENABLED indicates that
 *                                             the link is active and HGML_FEATURE_DISABLED indicates it
 *                                             is inactive
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a isActive has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a link is invalid or \a isActive is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkState(hgmlDevice_t device, unsigned int link, hgmlEnableState_t *isActive);

/**
 * Retrieves the version of the device's ICNLink for the link specified
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param version                              Requested ICNLink version
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a link is invalid or \a version is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkVersion(hgmlDevice_t device, unsigned int link, unsigned int *version);

/**
 * Retrieves the requested capability from the device's ICNLink for the link specified
 * Please refer to the \a hgmlIcnLinkCapability_t structure for the specific caps that can be queried
 * The return value should be treated as a boolean.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param capability                           Specifies the \a hgmlIcnLinkCapability_t to be queried
 * @param capResult                            A boolean for the queried capability indicating that feature is available
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a capResult has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a link, or \a capability is invalid or \a capResult is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkCapability(hgmlDevice_t device, unsigned int link,
                                                   hgmlIcnLinkCapability_t capability, unsigned int *capResult);

/**
 * Retrieves the PCI information for the remote node on a ICNLink link
 * Note: pciSubSystemId is not filled in this function and is indeterminate
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param pci                                  \a hgmlPciInfo_t of the remote node for the specified link
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a pci has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a link is invalid or \a pci is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkRemotePciInfo_v2(hgmlDevice_t device, unsigned int link, hgmlPciInfo_t *pci);

/**
 * Retrieves the specified error counter value
 * Please refer to \a hgmlIcnLinkErrorCounter_t for error counters that are available
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param counter                              Specifies the ICNLink counter to be queried
 * @param counterValue                         Returned counter value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a counter has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a link, or \a counter is invalid or \a counterValue is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkErrorCounter(hgmlDevice_t device, unsigned int link,
                                                     hgmlIcnLinkErrorCounter_t counter, unsigned long long *counterValue);

/**
 * Resets all error counters to zero
 * Please refer to \a hgmlIcnLinkErrorCounter_t for the list of error counters that are reset
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the reset is successful
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a link is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceResetIcnLinkErrorCounters(hgmlDevice_t device, unsigned int link);

/**
 * Deprecated: Setting utilization counter control is no longer supported.
 *
 * Set the ICNLINK utilization counter control information for the specified counter, 0 or 1.
 * Please refer to \a hgmlIcnLinkUtilizationControl_t for the structure definition.  Performs a reset
 * of the counters if the reset parameter is non-zero.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param counter                              Specifies the counter that should be set (0 or 1).
 * @param link                                 Specifies the ICNLink link to be queried
 * @param control                              A reference to the \a hgmlIcnLinkUtilizationControl_t to set
 * @param reset                                Resets the counters on set if non-zero
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the control has been set successfully
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a counter, \a link, or \a control is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetIcnLinkUtilizationControl(hgmlDevice_t device, unsigned int link, unsigned int counter,
                                                           hgmlIcnLinkUtilizationControl_t *control, unsigned int reset);

/**
 * Deprecated: Getting utilization counter control is no longer supported.
 *
 * Get the ICNLINK utilization counter control information for the specified counter, 0 or 1.
 * Please refer to \a hgmlIcnLinkUtilizationControl_t for the structure definition
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param counter                              Specifies the counter that should be set (0 or 1).
 * @param link                                 Specifies the ICNLink link to be queried
 * @param control                              A reference to the \a hgmlIcnLinkUtilizationControl_t to place information
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the control has been set successfully
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a counter, \a link, or \a control is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkUtilizationControl(hgmlDevice_t device, unsigned int link, unsigned int counter,
                                                           hgmlIcnLinkUtilizationControl_t *control);


/**
 * Deprecated: Use \ref hgmlDeviceGetFieldValues with HGML_FI_DEV_ICNLINK_THROUGHPUT_* as field values instead.
 *
 * Retrieve the ICNLINK utilization counter based on the current control for a specified counter.
 * In general it is good practice to use \a hgmlDeviceSetIcnLinkUtilizationControl
 *  before reading the utilization counters as they have no default state
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param counter                              Specifies the counter that should be read (0 or 1).
 * @param rxcounter                            Receive counter return value
 * @param txcounter                            Transmit counter return value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a rxcounter and \a txcounter have been successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a counter, or \a link is invalid or \a rxcounter or \a txcounter are NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetIcnLinkUtilizationCounter(hgmlDevice_t device, unsigned int link, unsigned int counter,
                                                           unsigned long long *rxcounter, unsigned long long *txcounter);

/**
 * Deprecated: Freezing ICNLINK utilization counters is no longer supported.
 *
 * Freeze the ICNLINK utilization counters
 * Both the receive and transmit counters are operated on by this function
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be queried
 * @param counter                              Specifies the counter that should be frozen (0 or 1).
 * @param freeze                               HGML_FEATURE_ENABLED = freeze the receive and transmit counters
 *                                             HGML_FEATURE_DISABLED = unfreeze the receive and transmit counters
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully frozen or unfrozen
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a link, \a counter, or \a freeze is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceFreezeIcnLinkUtilizationCounter (hgmlDevice_t device, unsigned int link,
                                            unsigned int counter, hgmlEnableState_t freeze);

/**
 * Deprecated: Resetting ICNLINK utilization counters is no longer supported.
 *
 * Reset the ICNLINK utilization counters
 * Both the receive and transmit counters are operated on by this function
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                               The identifier of the target device
 * @param link                                 Specifies the ICNLink link to be reset
 * @param counter                              Specifies the counter that should be reset (0 or 1)
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully reset
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a link, or \a counter is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceResetIcnLinkUtilizationCounter (hgmlDevice_t device, unsigned int link, unsigned int counter);


/**
* Get the ICNLink device type of the remote device connected over the given link.
*
* @param device                                The device handle of the target GPU
* @param link                                  The ICNLink link index on the target GPU
* @param pIcnLinkDeviceType                     Pointer in which the output remote device type is returned
*
* @return
*         - \ref HGML_SUCCESS                  if \a pIcnLinkDeviceType has been set
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_NOT_SUPPORTED      if ICNLink is not supported
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device or \a link is invalid, or
*                                              \a pIcnLinkDeviceType is NULL
*         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is
*                                              otherwise inaccessible
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlDeviceGetIcnLinkRemoteDeviceType(hgmlDevice_t device, unsigned int link, hgmlIntIcnLinkDeviceType_t *pIcnLinkDeviceType);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlEvents Event Handling Methods
 * This chapter describes methods that HGML can perform against each device to register and wait for
 * some event to occur.
 *  @{
 */
/***************************************************************************************************/

/**
 * Create an empty set of events.
 * Event set should be freed by \ref hgmlEventSetFree
 *
 * For &tm; or newer fully supported devices.
 * @param set                                  Reference in which to return the event handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the event has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a set is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlEventSetFree
 */
hgmlReturn_t hgmlEventSetCreate(hgmlEventSet_t *set);

/**
 * Starts recording of events on a specified devices and add the events to specified \ref hgmlEventSet_t
 *
 * For &tm; or newer fully supported devices.
 * Ecc events are available only on ECC enabled devices (see \ref hgmlDeviceGetTotalEccErrors)
 * Power capping events are available only on Power Management enabled devices (see \ref hgmlDeviceGetPowerManagementMode)
 *
 * For Linux only.
 *
 * \b IMPORTANT: Operations on \a set are not thread safe
 *
 * This call starts recording of events on specific device.
 * All events that occurred before this call are not recorded.
 * Checking if some event occurred can be done with \ref hgmlEventSetWait_v2
 *
 * If function reports HGML_ERROR_UNKNOWN, event set is in undefined state and should be freed.
 * If function reports HGML_ERROR_NOT_SUPPORTED, event set can still be used. None of the requested eventTypes
 *     are registered in that case.
 *
 * @param device                               The identifier of the target device
 * @param eventTypes                           Bitmask of \ref hgmlEventType to record
 * @param set                                  Set to which add new event types
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the event has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a eventTypes is invalid or \a set is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the platform does not support this feature or some of requested event types
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlEventType
 * @see hgmlDeviceGetSupportedEventTypes
 * @see hgmlEventSetWait
 * @see hgmlEventSetFree
 */
hgmlReturn_t hgmlDeviceRegisterEvents(hgmlDevice_t device, unsigned long long eventTypes, hgmlEventSet_t set);

/**
 * Returns information about events supported on device
 *
 * For &tm; or newer fully supported devices.
 *
 * Events are not supported on Windows. So this function returns an empty mask in \a eventTypes on Windows.
 *
 * @param device                               The identifier of the target device
 * @param eventTypes                           Reference in which to return bitmask of supported events
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the eventTypes has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a eventType is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlEventType
 * @see hgmlDeviceRegisterEvents
 */
hgmlReturn_t hgmlDeviceGetSupportedEventTypes(hgmlDevice_t device, unsigned long long *eventTypes);

/**
 * Waits on events and delivers events
 *
 * For &tm; or newer fully supported devices.
 *
 * If some events are ready to be delivered at the time of the call, function returns immediately.
 * If there are no events ready to be delivered, function sleeps till event arrives
 * but not longer than specified timeout. This function in certain conditions can return before
 * specified timeout passes (e.g. when interrupt arrives)
 *
 * On Windows, in case of xid error, the function returns the most recent xid error type seen by the system.
 * If there are multiple xid errors generated before hgmlEventSetWait is invoked then the last seen xid error
 * type is returned for all xid error events.
 *
 * On Linux, every xid error event would return the associated event data and other information if applicable.
 *
 * In MIG mode, if device handle is provided, the API reports all the events for the available instances,
 * only if the caller has appropriate privileges. In absence of required privileges, only the events which
 * affect all the instances (i.e. whole device) are reported.
 *
 * This API does not currently support per-instance event reporting using MIG device handles.
 *
 * @param set                                  Reference to set of events to wait on
 * @param data                                 Reference in which to return event data
 * @param timeoutms                            Maximum amount of wait time in milliseconds for registered event
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the data has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a data is NULL
 *         - \ref HGML_ERROR_TIMEOUT           if no event arrived in specified timeout or interrupt arrived
 *         - \ref HGML_ERROR_GPU_IS_LOST       if a GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlEventType
 * @see hgmlDeviceRegisterEvents
 */
hgmlReturn_t hgmlEventSetWait_v2(hgmlEventSet_t set, hgmlEventData_t * data, unsigned int timeoutms);

/**
 * Releases events in the set
 *
 * For &tm; or newer fully supported devices.
 *
 * @param set                                  Reference to events to be released
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the event has been successfully released
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlDeviceRegisterEvents
 */
hgmlReturn_t hgmlEventSetFree(hgmlEventSet_t set);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlZPI Drain states
 * This chapter describes methods that HGML can perform against each device to control their drain state
 * and recognition by HGML and kernel driver. These methods can be used with out-of-band tools to
 * power on/off GPUs, enable robust reset scenarios, etc.
 *  @{
 */
/***************************************************************************************************/

/**
 * Modify the drain state of a GPU.  This method forces a GPU to no longer accept new incoming requests.
 * Any new HGML process will no longer see this GPU.  Persistence mode for this GPU must be turned off before
 * this call is made.
 * Must be called as administrator.
 * For Linux only.
 *
 * For &tm; or newer fully supported devices.
 * Some devices supported.
 *
 * @param pciInfo                              The PCI address of the GPU drain state to be modified
 * @param newState                             The drain state that should be entered, see \ref hgmlEnableState_t
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully reset
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a hgmlIndex or \a newState is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the calling process has insufficient permissions to perform operation
 *         - \ref HGML_ERROR_IN_USE            if the device has persistence mode turned on
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceModifyDrainState (hgmlPciInfo_t *pciInfo, hgmlEnableState_t newState);

/**
 * Query the drain state of a GPU.  This method is used to check if a GPU is in a currently draining
 * state.
 * For Linux only.
 *
 * For &tm; or newer fully supported devices.
 * Some devices supported.
 *
 * @param pciInfo                              The PCI address of the GPU drain state to be queried
 * @param currentState                         The current drain state for this GPU, see \ref hgmlEnableState_t
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully reset
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a hgmlIndex or \a currentState is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceQueryDrainState (hgmlPciInfo_t *pciInfo, hgmlEnableState_t *currentState);

/**
 * This method will remove the specified GPU from the view of both HGML and the kernel driver
 * as long as no other processes are attached. If other processes are attached, this call will return
 * HGML_ERROR_IN_USE and the GPU will be returned to its original "draining" state. Note: the
 * only situation where a process can still be attached after hgmlDeviceModifyDrainState() is called
 * to initiate the draining state is if that process was using, and is still using, a GPU before the
 * call was made. Also note, persistence mode counts as an attachment to the GPU thus it must be disabled
 * prior to this call.
 *
 * For long-running HGML processes please note that this will change the enumeration of current GPUs.
 * For example, if there are four GPUs present and GPU1 is removed, the new enumeration will be 0-2.
 * Also, device handles after the removed GPU will not be valid and must be re-established.
 * Must be run as administrator.
 * For Linux only.
 *
 * For &tm; or newer fully supported devices.
 * Some devices supported.
 *
 * @param pciInfo                              The PCI address of the GPU to be removed
 * @param gpuState                             Whether the GPU is to be removed, from the OS
 *                                             see \ref hgmlDetachGpuState_t
 * @param linkState                            Requested upstream PCIe link state, see \ref hgmlPcieLinkState_t
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully reset
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a hgmlIndex is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device doesn't support this feature
 *         - \ref HGML_ERROR_IN_USE            if the device is still in use and cannot be removed
 */
hgmlReturn_t hgmlDeviceRemoveGpu_v2(hgmlPciInfo_t *pciInfo, hgmlDetachGpuState_t gpuState, hgmlPcieLinkState_t linkState);

/**
 * Request the OS and the kernel driver to rediscover a portion of the PCI subsystem looking for GPUs that
 * were previously removed. The portion of the PCI tree can be narrowed by specifying a domain, bus, and device.
 * If all are zeroes then the entire PCI tree will be searched.  Please note that for long-running HGML processes
 * the enumeration will change based on how many GPUs are discovered and where they are inserted in bus order.
 *
 * In addition, all newly discovered GPUs will be initialized and their ECC scrubbed which may take several seconds
 * per GPU. Also, all device handles are no longer guaranteed to be valid post discovery.
 *
 * Must be run as administrator.
 * For Linux only.
 *
 * For &tm; or newer fully supported devices.
 * Some devices supported.
 *
 * @param pciInfo                              The PCI tree to be searched.  Only the domain, bus, and device
 *                                             fields are used in this call.
 *
 * @return
 *         - \ref HGML_SUCCESS                 if counters were successfully reset
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a pciInfo is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the operating system does not support this feature
 *         - \ref HGML_ERROR_OPERATING_SYSTEM  if the operating system is denying this feature
 *         - \ref HGML_ERROR_NO_PERMISSION     if the calling process has insufficient permissions to perform operation
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceDiscoverGpus (hgmlPciInfo_t *pciInfo);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlFieldValueQueries Field Value Queries
 *  This chapter describes HGML operations that are associated with retrieving Field Values from HGML
 *  @{
 */
/***************************************************************************************************/

/**
 * Request values for a list of fields for a device. This API allows multiple fields to be queried at once.
 * If any of the underlying fieldIds are populated by the same driver call, the results for those field IDs
 * will be populated from a single call rather than making a driver call for each fieldId.
 *
 * @param device                               The device handle of the GPU to request field values for
 * @param valuesCount                          Number of entries in values that should be retrieved
 * @param values                               Array of \a valuesCount structures to hold field values.
 *                                             Each value's fieldId must be populated prior to this call
 *
 * @return
 *         - \ref HGML_SUCCESS                 if any values in \a values were populated. Note that you must
 *                                             check the hgmlReturn field of each value for each individual
 *                                             status
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a values is NULL
 */
hgmlReturn_t hgmlDeviceGetFieldValues(hgmlDevice_t device, int valuesCount, hgmlFieldValue_t *values);

/**
 * Clear values for a list of fields for a device. This API allows multiple fields to be cleared at once.
 *
 * @param device                               The device handle of the GPU to request field values for
 * @param valuesCount                          Number of entries in values that should be cleared
 * @param values                               Array of \a valuesCount structures to hold field values.
 *                                             Each value's fieldId must be populated prior to this call
 *
 * @return
 *         - \ref HGML_SUCCESS                 if any values in \a values were cleared. Note that you must
 *                                             check the hgmlReturn field of each value for each individual
 *                                             status
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a values is NULL
 */
hgmlReturn_t hgmlDeviceClearFieldValues(hgmlDevice_t device, int valuesCount, hgmlFieldValue_t *values);

/** @} */

/***************************************************************************************************/
/** @defgroup vGPU Enums, Constants and Structs
 *  @{
 */
/** @} */
/***************************************************************************************************/

/***************************************************************************************************/
/** @defgroup hgmlVirtualGpuQueries vGPU APIs
 * This chapter describes operations that are associated with vGPU Software products.
 *  @{
 */
/***************************************************************************************************/

/**
 * This method is used to get the virtualization mode corresponding to the GPU.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                    Identifier of the target device
 * @param pVirtualMode              Reference to virtualization mode. One of HGML_GPU_VIRTUALIZATION_?
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a pVirtualMode is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device is invalid or \a pVirtualMode is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVirtualizationMode(hgmlDevice_t device, hgmlGpuVirtualizationMode_t *pVirtualMode);

/**
 * Queries if SR-IOV host operation is supported on a vGPU supported device.
 *
 * Checks whether SR-IOV host capability is supported by the device and the
 * driver, and indicates device is in SR-IOV mode if both of these conditions
 * are true.
 *
 * @param device                                The identifier of the target device
 * @param pHostVgpuMode                         Reference in which to return the current vGPU mode
 *
 * @return
 *         - \ref HGML_SUCCESS                  if device's vGPU mode has been successfully retrieved
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device handle is 0 or \a pVgpuMode is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if \a device doesn't support this feature.
 *         - \ref HGML_ERROR_UNKNOWN            if any unexpected error occurred
 */
hgmlReturn_t hgmlDeviceGetHostVgpuMode(hgmlDevice_t device, hgmlHostVgpuMode_t *pHostVgpuMode);

/**
 * This method is used to set the virtualization mode corresponding to the GPU.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                    Identifier of the target device
 * @param virtualMode               virtualization mode. One of HGML_GPU_VIRTUALIZATION_?
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a pVirtualMode is set
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device is invalid or \a pVirtualMode is NULL
 *         - \ref HGML_ERROR_GPU_IS_LOST        if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if setting of virtualization mode is not supported.
 *         - \ref HGML_ERROR_NO_PERMISSION      if setting of virtualization mode is not allowed for this client.
 */
hgmlReturn_t hgmlDeviceSetVirtualizationMode(hgmlDevice_t device, hgmlGpuVirtualizationMode_t virtualMode);

/**
 * Retrieve the vGPU Software licensable features.
 *
 * Identifies whether the system supports vGPU Software Licensing. If it does, return the list of licensable feature(s)
 * and their current license status.
 *
 * @param device                    Identifier of the target device
 * @param pGridLicensableFeatures   Pointer to structure in which vGPU software licensable features are returned
 *
 * @return
 *         - \ref HGML_SUCCESS                 if licensable features are successfully retrieved
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a pGridLicensableFeatures is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGridLicensableFeatures_v4(hgmlDevice_t device, hgmlGridLicensableFeatures_t *pGridLicensableFeatures);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlVgpu vGPU Management
 * @{
 *
 * This chapter describes APIs supporting alixpu vGPU.
 */
/***************************************************************************************************/

typedef unsigned int hgmlVgpuProfileId_t;

/**
 * Retrieve the requested vGPU driver capability.
 *
 * Refer to the \a hgmlVgpuDriverCapability_t structure for the specific capabilities that can be queried.
 * The return value in \a capResult should be treated as a boolean, with a non-zero value indicating that the capability
 * is supported.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param capability      Specifies the \a hgmlVgpuDriverCapability_t to be queried
 * @param capResult       A boolean for the queried capability indicating that feature is supported
 *
 * @return
 *      - \ref HGML_SUCCESS                      successful completion
 *      - \ref HGML_ERROR_UNINITIALIZED          if the library has not been successfully initialized
 *      - \ref HGML_ERROR_INVALID_ARGUMENT       if \a capability is invalid, or \a capResult is NULL
 *      - \ref HGML_ERROR_NOT_SUPPORTED          the API is not supported in current state or \a devices not in vGPU mode
 *      - \ref HGML_ERROR_UNKNOWN                on any unexpected error
*/
hgmlReturn_t hgmlGetVgpuDriverCapabilities(hgmlVgpuDriverCapability_t capability, unsigned int *capResult);

/**
 * Retrieve the requested vGPU capability for GPU.
 *
 * Refer to the \a hgmlDeviceVgpuCapability_t structure for the specific capabilities that can be queried.
 * The return value in \a capResult reports a non-zero value indicating that the capability
 * is supported, and also reports the capability's data based on the queried capability.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device     The identifier of the target device
 * @param capability Specifies the \a hgmlDeviceVgpuCapability_t to be queried
 * @param capResult  Specifies that the queried capability is supported, and also returns capability's data
 *
 * @return
 *      - \ref HGML_SUCCESS                      successful completion
 *      - \ref HGML_ERROR_UNINITIALIZED          if the library has not been successfully initialized
 *      - \ref HGML_ERROR_INVALID_ARGUMENT       if \a device is invalid, or \a capability is invalid, or \a capResult is NULL
 *      - \ref HGML_ERROR_NOT_SUPPORTED          the API is not supported in current state or \a device not in vGPU mode
 *      - \ref HGML_ERROR_UNKNOWN                on any unexpected error
*/
hgmlReturn_t hgmlDeviceGetVgpuCapabilities(hgmlDevice_t device, hgmlDeviceVgpuCapability_t capability, unsigned int *capResult);

/**
 * Retrieve the supported vGPU types on a physical GPU (device).
 *
 * An array of supported vGPU types for the physical GPU indicated by \a device is returned in the caller-supplied buffer
 * pointed at by \a vgpuTypeIds. The element count of hgmlVgpuTypeId_t array is passed in \a vgpuCount, and \a vgpuCount
 * is used to return the number of vGPU types written to the buffer.
 *
 * If the supplied buffer is not large enough to accommodate the vGPU type array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlVgpuTypeId_t array required in \a vgpuCount.
 * To query the number of vGPU types supported for the GPU, call this function with *vgpuCount = 0.
 * The code will return HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if no vGPU types are supported.
 *
 * @param device                   The identifier of the target device
 * @param vgpuCount                Pointer to caller-supplied array size, and returns number of vGPU types
 * @param vgpuTypeIds              Pointer to caller-supplied array in which to return list of vGPU types
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE      \a vgpuTypeIds buffer is too small, array element count is returned in \a vgpuCount
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuCount is NULL or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED          if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN                on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetSupportedVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuTypeId_t *vgpuTypeIds);

/**
 * Retrieve the currently creatable vGPU types on a physical GPU (device).
 *
 * An array of creatable vGPU types for the physical GPU indicated by \a device is returned in the caller-supplied buffer
 * pointed at by \a vgpuTypeIds. The element count of hgmlVgpuTypeId_t array is passed in \a vgpuCount, and \a vgpuCount
 * is used to return the number of vGPU types written to the buffer.
 *
 * The creatable vGPU types for a device may differ over time, as there may be restrictions on what type of vGPU types
 * can concurrently run on a device.  For example, if only one vGPU type is allowed at a time on a device, then the creatable
 * list will be restricted to whatever vGPU type is already running on the device.
 *
 * If the supplied buffer is not large enough to accommodate the vGPU type array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlVgpuTypeId_t array required in \a vgpuCount.
 * To query the number of vGPU types that can be created for the GPU, call this function with *vgpuCount = 0.
 * The code will return HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if no vGPU types are creatable.
 *
 * @param device                   The identifier of the target device
 * @param vgpuCount                Pointer to caller-supplied array size, and returns number of vGPU types
 * @param vgpuTypeIds              Pointer to caller-supplied array in which to return list of vGPU types
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE      \a vgpuTypeIds buffer is too small, array element count is returned in \a vgpuCount
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuCount is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED          if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN                on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetCreatableVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuTypeId_t *vgpuTypeIds);

/**
 * Create a vGPU instance on a physical GPU (device).
 *
 * The physical GPU indicated by \a device, and vGPU profile is indicated by \a vgpuProfileId.
 *
 * The function return \a vgpuInstance as a handle.
 *
 * @param device                   The identifier of the target device
 * @param vgpuProfileId            The identifier of the profile of vGPU to create
 * @param vgpuInstance             Pointer to buffer to return vgpuInstance
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuTypeId less than 0
 *         - \ref HGML_ERROR_NOT_SUPPORTED          if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN                on any unexpected error
 */
hgmlReturn_t hgmlDeviceCreateVgpuInstance(hgmlDevice_t device, hgmlVgpuProfileId_t vgpuProfileId, hgmlVgpuInstance_t* vgpuInstance);

/**
 * Destroy a vGPU instance on a physical GPU (device).
 *
 * To destroy a vGPU on the specified device by \a vgpuInstance.
 *
 * If you need to forcibly destroy a running vGPU，please use \a force option, and set it to true.
 *
 * @param vgpuInstance             The identifier of the vgpu instance
 * @param force                    The option to force deletion
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuInstance is less than or equal to 0
 *         - \ref HGML_ERROR_NOT_SUPPORTED          if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN                on any unexpected error
 */
hgmlReturn_t hgmlDeviceDestroyVgpuInstance(hgmlVgpuInstance_t vgpuInstance, unsigned int force);

/**
 * Query the profile id of a given \a vgpuTypeId.
 *
 * @param vgpuTypeId             The identifier of the vgpu type
 * @param vgpuProfileId          Pointer to buffer to return vgpu profile id
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuTypeId is invalid or \a vgpuProfileId is NULL
 */
hgmlReturn_t hgmlVgpuTypeGetVgpuProfileId(hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuProfileId_t* vgpuProfileId);

/**
 * Get the remaining capacity of a given \a vgpuTypeId.
 *
 * @param vgpuTypeId             The identifier of the vgpu type
 * @param count                  Pointer to buffer to count of remaining capacity
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuTypeId is invalid or \a count is NULL
 */
hgmlReturn_t hgmlVgpuTypeGetRemainingCapacity(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *count);

/**
 * Query the pci bus info of a given \a vgpuInstance.
 *
 * @param vgpuInstance             The identifier of the vgpu instance
 * @param pci                      Pointer to struct of vgpu instance pci bus info
 *
 * @return
 *         - \ref HGML_SUCCESS                      successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT       if \a vgpuTypeId is invalid or \a pci is NULL
 */
hgmlReturn_t hgmlVgpuInstanceGetPciInfo(hgmlVgpuInstance_t vgpuInstance, hgmlPciInfo_t *pci);

/**
 * Retrieve the class of a vGPU type. It will not exceed 64 characters in length (including the NUL terminator).
 * See \ref hgmlConstants::HGML_DEVICE_NAME_BUFFER_SIZE.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param vgpuTypeClass            Pointer to string array to return class in
 * @param size                     Size of string
 *
 * @return
 *         - \ref HGML_SUCCESS                   successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a vgpuTypeId is invalid, or \a vgpuTypeClass is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE   if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetClass(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeClass, unsigned int *size);

/**
 * Retrieve the vGPU type name.
 *
 * The name is an alphanumeric string that denotes a particular vGPU, e.g. GRID M60-2Q. It will not
 * exceed 64 characters in length (including the NUL terminator).  See \ref
 * hgmlConstants::HGML_DEVICE_NAME_BUFFER_SIZE.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param vgpuTypeName             Pointer to buffer to return name
 * @param size                     Size of buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a name is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetName(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeName, unsigned int *size);

/**
 * Retrieve the GPU Instance Profile ID for the given vGPU type ID.
 * The API will return a valid GPU Instance Profile ID for the MIG capable vGPU types, else INVALID_GPU_INSTANCE_PROFILE_ID is
 * returned.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param gpuInstanceProfileId     GPU Instance Profile ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if \a device is not in vGPU Host virtualization mode
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a gpuInstanceProfileId is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetGpuInstanceProfileId(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *gpuInstanceProfileId);

/**
 * Retrieve the device ID of a vGPU type.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param deviceID                 Device ID and vendor ID of the device contained in single 32 bit value
 * @param subsystemID              Subsystem ID and subsystem vendor ID of the device contained in single 32 bit value
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a deviceId or \a subsystemID are NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetDeviceID(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *deviceID, unsigned long long *subsystemID);

/**
 * Retrieve the vGPU framebuffer size in bytes.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param fbSize                   Pointer to framebuffer size in bytes
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a fbSize is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetFramebufferSize(hgmlVgpuTypeId_t vgpuTypeId, unsigned long long *fbSize);

/**
 * Retrieve count of vGPU's supported display heads.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param numDisplayHeads          Pointer to number of display heads
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a numDisplayHeads is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetNumDisplayHeads(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *numDisplayHeads);

/**
 * Retrieve vGPU display head's maximum supported resolution.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param displayIndex             Zero-based index of display head
 * @param xdim                     Pointer to maximum number of pixels in X dimension
 * @param ydim                     Pointer to maximum number of pixels in Y dimension
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a xdim or \a ydim are NULL, or \a displayIndex
 *                                             is out of range.
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetResolution(hgmlVgpuTypeId_t vgpuTypeId, unsigned int displayIndex, unsigned int *xdim, unsigned int *ydim);

/**
 * Retrieve license requirements for a vGPU type
 *
 * The license type and version required to run the specified vGPU type is returned as an alphanumeric string, in the form
 * "<license name>,<version>", for example "GRID-Virtual-PC,2.0". If a vGPU is runnable with* more than one type of license,
 * the licenses are delimited by a semicolon, for example "GRID-Virtual-PC,2.0;GRID-Virtual-WS,2.0;GRID-Virtual-WS-Ext,2.0".
 *
 * The total length of the returned string will not exceed 128 characters, including the NUL terminator.
 * See \ref hgmlVgpuConstants::HGML_GRID_LICENSE_BUFFER_SIZE.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param vgpuTypeLicenseString    Pointer to buffer to return license info
 * @param size                     Size of \a vgpuTypeLicenseString buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a vgpuTypeLicenseString is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetLicense(hgmlVgpuTypeId_t vgpuTypeId, char *vgpuTypeLicenseString, unsigned int size);

/**
 * Retrieve the static frame rate limit value of the vGPU type
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param frameRateLimit           Reference to return the frame rate limit value
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if frame rate limiter is turned off for the vGPU type
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a frameRateLimit is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetFrameRateLimit(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *frameRateLimit);

/**
 * Retrieve the maximum number of vGPU instances creatable on a device for given vGPU type
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                   The identifier of the target device
 * @param vgpuTypeId               Handle to vGPU type
 * @param vgpuInstanceCount        Pointer to get the max number of vGPU instances
 *                                 that can be created on a deicve for given vgpuTypeId
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid or is not supported on target device,
 *                                             or \a vgpuInstanceCount is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetMaxInstances(hgmlDevice_t device, hgmlVgpuTypeId_t vgpuTypeId, unsigned int *vgpuInstanceCount);

/**
 * Retrieve the maximum number of vGPU instances supported per VM for given vGPU type
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuTypeId               Handle to vGPU type
 * @param vgpuInstanceCountPerVm   Pointer to get the max number of vGPU instances supported per VM for given \a vgpuTypeId
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a vgpuInstanceCountPerVm is NULL
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuTypeGetMaxInstancesPerVm(hgmlVgpuTypeId_t vgpuTypeId, unsigned int *vgpuInstanceCountPerVm);

/**
 * Retrieve the alive vGPU instances on a device.
 *
 * An array of active vGPU instances is returned in the caller-supplied buffer pointed at by \a vgpuInstances. The

 * array elememt count is passed in \a vgpuCount, and \a vgpuCount is used to return the number of vGPU instances
 * written to the buffer.
 *
 * If the supplied buffer is not large enough to accomodate the vGPU instance array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlVgpuInstance_t array required in \a vgpuCount.
 * To query the number of active vGPU instances, call this function with *vgpuCount = 0.  The code will return
 * HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if no vGPU Types are supported.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                   The identifier of the target device
 * @param vgpuCount                Pointer which passes in the array size as well as get
 *                                 back the number of types
 * @param vgpuInstances            Pointer to array in which to return list of vGPU instances
 *
 * @return
 *         - \ref HGML_SUCCESS                  successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device is invalid, or \a vgpuCount is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a size is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetAliveVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuInstance_t *vgpuInstances);

/**
 * Retrieve the active vGPU instances on a device.
 *
 * An array of active vGPU instances is returned in the caller-supplied buffer pointed at by \a vgpuInstances. The
 * array element count is passed in \a vgpuCount, and \a vgpuCount is used to return the number of vGPU instances
 * written to the buffer.
 *
 * If the supplied buffer is not large enough to accommodate the vGPU instance array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlVgpuInstance_t array required in \a vgpuCount.
 * To query the number of active vGPU instances, call this function with *vgpuCount = 0.  The code will return
 * HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if no vGPU Types are supported.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                   The identifier of the target device
 * @param vgpuCount                Pointer which passes in the array size as well as get
 *                                 back the number of types
 * @param vgpuInstances            Pointer to array in which to return list of vGPU instances
 *
 * @return
 *         - \ref HGML_SUCCESS                  successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a device is invalid, or \a vgpuCount is NULL
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a size is too small
 *         - \ref HGML_ERROR_NOT_SUPPORTED      if vGPU is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetActiveVgpus(hgmlDevice_t device, unsigned int *vgpuCount, hgmlVgpuInstance_t *vgpuInstances);

/**
 * Retrieve the VM ID associated with a vGPU instance.
 *
 * The VM ID is returned as a string, not exceeding 80 characters in length (including the NUL terminator).
 * See \ref hgmlConstants::HGML_DEVICE_UUID_BUFFER_SIZE.
 *
 * The format of the VM ID varies by platform, and is indicated by the type identifier returned in \a vmIdType.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param vmId                     Pointer to caller-supplied buffer to hold VM ID
 * @param size                     Size of buffer in bytes
 * @param vmIdType                 Pointer to hold VM ID type
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vmId or \a vmIdType is NULL, or \a vgpuInstance is 0
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetVmID(hgmlVgpuInstance_t vgpuInstance, char *vmId, unsigned int size, hgmlVgpuVmIdType_t *vmIdType);

/**
 * Retrieve the UUID of a vGPU instance.
 *
 * The UUID is a globally unique identifier associated with the vGPU, and is returned as a 5-part hexadecimal string,
 * not exceeding 80 characters in length (including the NULL terminator).
 * See \ref hgmlConstants::HGML_DEVICE_UUID_BUFFER_SIZE.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param uuid                     Pointer to caller-supplied buffer to hold vGPU UUID
 * @param size                     Size of buffer in bytes
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a uuid is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetUUID(hgmlVgpuInstance_t vgpuInstance, char *uuid, unsigned int size);

/**
 * Retrieve the driver version installed in the VM associated with a vGPU.
 *
 * The version is returned as an alphanumeric string in the caller-supplied buffer \a version. The length of the version
 * string will not exceed 80 characters in length (including the NUL terminator).
 * See \ref hgmlConstants::HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE.
 *
 * hgmlVgpuInstanceGetVmDriverVersion() may be called at any time for a vGPU instance. The guest VM driver version is
 * returned as "Not Available" if no driver is installed in the VM, or the VM has not yet booted to the point where the
 * driver is loaded and initialized.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param version                  Caller-supplied buffer to return driver version string
 * @param length                   Size of \a version buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a version has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetVmDriverVersion(hgmlVgpuInstance_t vgpuInstance, char* version, unsigned int length);

/**
 * Retrieve the framebuffer usage in bytes.
 *
 * Framebuffer usage is the amont of vGPU framebuffer memory that is currently in use by the VM.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             The identifier of the target instance
 * @param fbUsage                  Pointer to framebuffer usage in bytes
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a fbUsage is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetFbUsage(hgmlVgpuInstance_t vgpuInstance, unsigned long long *fbUsage);

/**
 * @deprecated Use \ref hgmlVgpuInstanceGetLicenseInfo_v2.
 *
 * Retrieve the current licensing state of the vGPU instance.
 *
 * If the vGPU is currently licensed, \a licensed is set to 1, otherwise it is set to 0.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param licensed                 Reference to return the licensing status
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a licensed has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a licensed is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetLicenseStatus(hgmlVgpuInstance_t vgpuInstance, unsigned int *licensed);

/**
 * Retrieve the vGPU type of a vGPU instance.
 *
 * Returns the vGPU type ID of vgpu assigned to the vGPU instance.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param vgpuTypeId               Reference to return the vgpuTypeId
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a vgpuTypeId has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a vgpuTypeId is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetType(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuTypeId_t *vgpuTypeId);

/**
 * Retrieve the frame rate limit set for the vGPU instance.
 *
 * Returns the value of the frame rate limit set for the vGPU instance
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param frameRateLimit           Reference to return the frame rate limit
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a frameRateLimit has been set
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if frame rate limiter is turned off for the vGPU type
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a frameRateLimit is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetFrameRateLimit(hgmlVgpuInstance_t vgpuInstance, unsigned int *frameRateLimit);

/**
 * Retrieve the current ECC mode of vGPU instance.
 *
 * @param vgpuInstance            The identifier of the target vGPU instance
 * @param eccMode                 Reference in which to return the current ECC mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the vgpuInstance's ECC mode has been successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a mode is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the vGPU doesn't support this feature
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetEccMode(hgmlVgpuInstance_t vgpuInstance, hgmlEnableState_t *eccMode);

/**
 * Retrieve the encoder capacity of a vGPU instance, as a percentage of maximum encoder capacity with valid values in the range 0-100.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param encoderCapacity          Reference to an unsigned int for the encoder capacity
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a encoderCapacity has been retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a encoderQueryType is invalid
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetEncoderCapacity(hgmlVgpuInstance_t vgpuInstance, unsigned int *encoderCapacity);

/**
 * Set the encoder capacity of a vGPU instance, as a percentage of maximum encoder capacity with valid values in the range 0-100.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param encoderCapacity          Unsigned int for the encoder capacity value
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a encoderCapacity has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a encoderCapacity is out of range of 0-100.
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceSetEncoderCapacity(hgmlVgpuInstance_t vgpuInstance, unsigned int  encoderCapacity);

/**
 * Retrieves the current encoder statistics of a vGPU Instance
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance                      Identifier of the target vGPU instance
 * @param sessionCount                      Reference to an unsigned int for count of active encoder sessions
 * @param averageFps                        Reference to an unsigned int for trailing average FPS of all active sessions
 * @param averageLatency                    Reference to an unsigned int for encode latency in microseconds
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a sessionCount, \a averageFps and \a averageLatency is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a sessionCount , or \a averageFps or \a averageLatency is NULL
 *                                              or \a vgpuInstance is 0.
 *         - \ref HGML_ERROR_NOT_FOUND          if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetEncoderStats(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount,
                                                     unsigned int *averageFps, unsigned int *averageLatency);

/**
 * Retrieves information about all active encoder sessions on a vGPU Instance.
 *
 * An array of active encoder sessions is returned in the caller-supplied buffer pointed at by \a sessionInfo. The
 * array element count is passed in \a sessionCount, and \a sessionCount is used to return the number of sessions
 * written to the buffer.
 *
 * If the supplied buffer is not large enough to accommodate the active session array, the function returns
 * HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlEncoderSessionInfo_t array required in \a sessionCount.
 * To query the number of active encoder sessions, call this function with *sessionCount = 0. The code will return
 * HGML_SUCCESS with number of active encoder sessions updated in *sessionCount.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance                      Identifier of the target vGPU instance
 * @param sessionCount                      Reference to caller supplied array size, and returns
 *                                          the number of sessions.
 * @param sessionInfo                       Reference to caller supplied array in which the list
 *                                          of session information us returned.
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a sessionInfo is fetched
 *         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a sessionCount is too small, array element count is
                                                returned in \a sessionCount
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a sessionCount is NULL, or \a vgpuInstance is 0.
 *         - \ref HGML_ERROR_NOT_FOUND          if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetEncoderSessions(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount, hgmlEncoderSessionInfo_t *sessionInfo);

/**
* Retrieves the active frame buffer capture sessions statistics of a vGPU Instance
*
* For &tm; or newer fully supported devices.
*
* @param vgpuInstance                      Identifier of the target vGPU instance
* @param fbcStats                          Reference to hgmlFBCStats_t structure containing NvFBC stats
*
* @return
*         - \ref HGML_SUCCESS                  if \a fbcStats is fetched
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a vgpuInstance is 0, or \a fbcStats is NULL
*         - \ref HGML_ERROR_NOT_FOUND          if \a vgpuInstance does not match a valid active vGPU instance on the system
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlVgpuInstanceGetFBCStats(hgmlVgpuInstance_t vgpuInstance, hgmlFBCStats_t *fbcStats);

/**
* Retrieves information about active frame buffer capture sessions on a vGPU Instance.
*
* An array of active FBC sessions is returned in the caller-supplied buffer pointed at by \a sessionInfo. The
* array element count is passed in \a sessionCount, and \a sessionCount is used to return the number of sessions
* written to the buffer.
*
* If the supplied buffer is not large enough to accommodate the active session array, the function returns
* HGML_ERROR_INSUFFICIENT_SIZE, with the element count of hgmlFBCSessionInfo_t array required in \a sessionCount.
* To query the number of active FBC sessions, call this function with *sessionCount = 0.  The code will return
* HGML_SUCCESS with number of active FBC sessions updated in *sessionCount.
*
* For &tm; or newer fully supported devices.
*
* @note hResolution, vResolution, averageFPS and averageLatency data for a FBC session returned in \a sessionInfo may
*       be zero if there are no new frames captured since the session started.
*
* @param vgpuInstance                      Identifier of the target vGPU instance
* @param sessionCount                      Reference to caller supplied array size, and returns the number of sessions.
* @param sessionInfo                       Reference in which to return the session information
*
* @return
*         - \ref HGML_SUCCESS                  if \a sessionInfo is fetched
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a vgpuInstance is 0, or \a sessionCount is NULL.
*         - \ref HGML_ERROR_NOT_FOUND          if \a vgpuInstance does not match a valid active vGPU instance on the system
*         - \ref HGML_ERROR_INSUFFICIENT_SIZE  if \a sessionCount is too small, array element count is returned in \a sessionCount
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlVgpuInstanceGetFBCSessions(hgmlVgpuInstance_t vgpuInstance, unsigned int *sessionCount, hgmlFBCSessionInfo_t *sessionInfo);

/**
* Retrieve the GPU Instance ID for the given vGPU Instance.
* The API will return a valid GPU Instance ID for MIG backed vGPU Instance, else INVALID_GPU_INSTANCE_ID is returned.
*
* For &tm; or newer fully supported devices.
*
* @param vgpuInstance                      Identifier of the target vGPU instance
* @param gpuInstanceId                     GPU Instance ID
*
* @return
*         - \ref HGML_SUCCESS                  successful completion
*         - \ref HGML_ERROR_UNINITIALIZED      if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a vgpuInstance is 0, or \a gpuInstanceId is NULL.
*         - \ref HGML_ERROR_NOT_FOUND          if \a vgpuInstance does not match a valid active vGPU instance on the system
*         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
*/
hgmlReturn_t hgmlVgpuInstanceGetGpuInstanceId(hgmlVgpuInstance_t vgpuInstance, unsigned int *gpuInstanceId);

/**
* Retrieves the PCI Id of the given vGPU Instance i.e. the PCI Id of the GPU as seen inside the VM.
*
* The vGPU PCI id is returned as "00000000:00:00.0" if alixpu driver is not installed on the vGPU instance.
*
* @param vgpuInstance                         Identifier of the target vGPU instance
* @param vgpuPciId                            Caller-supplied buffer to return vGPU PCI Id string
* @param length                               Size of the vgpuPciId buffer
*
* @return
*         - \ref HGML_SUCCESS                 if vGPU PCI Id is sucessfully retrieved
*         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a vgpuPciId is NULL
*         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
*         - \ref HGML_ERROR_DRIVER_NOT_LOADED if alixpu driver is not running on the vGPU instance
*         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a length is too small, \a length is set to required length
*         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
*/
hgmlReturn_t hgmlVgpuInstanceGetGpuPciId(hgmlVgpuInstance_t vgpuInstance, char *vgpuPciId, unsigned int *length);

/**
* Retrieve the requested capability for a given vGPU type. Refer to the \a hgmlVgpuCapability_t structure
* for the specific capabilities that can be queried. The return value in \a capResult should be treated as
* a boolean, with a non-zero value indicating that the capability is supported.
*
* For &tm; or newer fully supported devices.
*
* @param vgpuTypeId                           Handle to vGPU type
* @param capability                           Specifies the \a hgmlVgpuCapability_t to be queried
* @param capResult                            A boolean for the queried capability indicating that feature is supported
*
* @return
*         - \ref HGML_SUCCESS                 successful completion
*         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
*         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuTypeId is invalid, or \a capability is invalid, or \a capResult is NULL
*         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
*/
hgmlReturn_t hgmlVgpuTypeGetCapabilities(hgmlVgpuTypeId_t vgpuTypeId, hgmlVgpuCapability_t capability, unsigned int *capResult);

/**
 * Retrieve the MDEV UUID of a vGPU instance.
 *
 * The MDEV UUID is a globally unique identifier of the mdev device assigned to the VM, and is returned as a 5-part hexadecimal string,
 * not exceeding 80 characters in length (including the NULL terminator).
 * MDEV UUID is displayed only on KVM platform.
 * See \ref hgmlConstants::HGML_DEVICE_UUID_BUFFER_SIZE.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance             Identifier of the target vGPU instance
 * @param mdevUuid                 Pointer to caller-supplied buffer to hold MDEV UUID
 * @param size                     Size of buffer in bytes
 *
 * @return
 *         - \ref HGML_SUCCESS                 successful completion
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_NOT_SUPPORTED     on any hypervisor other than KVM
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a mdevUuid is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a size is too small
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetMdevUUID(hgmlVgpuInstance_t vgpuInstance, char *mdevUuid, unsigned int size);

/** @} */

/***************************************************************************************************/
/** @defgroup hgml vGPU Migration
 * This chapter describes operations that are associated with vGPU Migration.
 *  @{
 */
/***************************************************************************************************/

/**
 * Structure representing range of vGPU versions.
 */
typedef struct hgmlVgpuVersion_st
{
    unsigned int minVersion; //!< Minimum vGPU version.
    unsigned int maxVersion; //!< Maximum vGPU version.
} hgmlVgpuVersion_t;

/**
 * vGPU metadata structure.
 */
typedef struct hgmlVgpuMetadata_st
{
    unsigned int             version;                                                    //!< Current version of the structure
    unsigned int             revision;                                                   //!< Current revision of the structure
    hgmlVgpuGuestInfoState_t guestInfoState;                                             //!< Current state of Guest-dependent fields
    char                     guestDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE]; //!< Version of driver installed in guest
    char                     hostDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];  //!< Version of driver installed in host
    unsigned int             reserved[6];                                                //!< Reserved for internal use
    unsigned int             vgpuVirtualizationCaps;                                     //!< vGPU virtualization capabilities bitfield
    unsigned int             guestVgpuVersion;                                           //!< vGPU version of guest driver
    unsigned int             opaqueDataSize;                                             //!< Size of opaque data field in bytes
    char                     opaqueData[4];                                              //!< Opaque data
} hgmlVgpuMetadata_t;

/**
 * Physical GPU metadata structure
 */
typedef struct hgmlVgpuPgpuMetadata_st
{
    unsigned int            version;                                                    //!< Current version of the structure
    unsigned int            revision;                                                   //!< Current revision of the structure
    char                    hostDriverVersion[HGML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE];  //!< Host driver version
    unsigned int            pgpuVirtualizationCaps;                                     //!< Pgpu virtualization capabilities bitfield
    unsigned int            reserved[5];                                                //!< Reserved for internal use
    hgmlVgpuVersion_t       hostSupportedVgpuRange;                                     //!< vGPU version range supported by host driver
    unsigned int            opaqueDataSize;                                             //!< Size of opaque data field in bytes
    char                    opaqueData[4];                                              //!< Opaque data
} hgmlVgpuPgpuMetadata_t;

/**
 * vGPU VM compatibility codes
 */
typedef enum hgmlVgpuVmCompatibility_enum
{
    HGML_VGPU_VM_COMPATIBILITY_NONE         = 0x0,    //!< vGPU is not runnable
    HGML_VGPU_VM_COMPATIBILITY_COLD         = 0x1,    //!< vGPU is runnable from a cold / powered-off state (ACPI S5)
    HGML_VGPU_VM_COMPATIBILITY_HIBERNATE    = 0x2,    //!< vGPU is runnable from a hibernated state (ACPI S4)
    HGML_VGPU_VM_COMPATIBILITY_SLEEP        = 0x4,    //!< vGPU is runnable from a sleeped state (ACPI S3)
    HGML_VGPU_VM_COMPATIBILITY_LIVE         = 0x8     //!< vGPU is runnable from a live/paused (ACPI S0)
} hgmlVgpuVmCompatibility_t;

/**
 *  vGPU-pGPU compatibility limit codes
 */
typedef enum hgmlVgpuPgpuCompatibilityLimitCode_enum
{
    HGML_VGPU_COMPATIBILITY_LIMIT_NONE          = 0x0,           //!< Compatibility is not limited.
    HGML_VGPU_COMPATIBILITY_LIMIT_HOST_DRIVER   = 0x1,           //!< ompatibility is limited by host driver version.
    HGML_VGPU_COMPATIBILITY_LIMIT_GUEST_DRIVER  = 0x2,           //!< Compatibility is limited by guest driver version.
    HGML_VGPU_COMPATIBILITY_LIMIT_GPU           = 0x4,           //!< Compatibility is limited by GPU hardware.
    HGML_VGPU_COMPATIBILITY_LIMIT_OTHER         = 0x80000000     //!< Compatibility is limited by an undefined factor.
} hgmlVgpuPgpuCompatibilityLimitCode_t;

/**
 * vGPU-pGPU compatibility structure
 */
typedef struct hgmlVgpuPgpuCompatibility_st
{
    hgmlVgpuVmCompatibility_t               vgpuVmCompatibility;    //!< Compatibility of vGPU VM. See \ref hgmlVgpuVmCompatibility_t
    hgmlVgpuPgpuCompatibilityLimitCode_t    compatibilityLimitCode; //!< Limiting factor for vGPU-pGPU compatibility. See \ref hgmlVgpuPgpuCompatibilityLimitCode_t
} hgmlVgpuPgpuCompatibility_t;

/**
 * Returns vGPU metadata structure for a running vGPU. The structure contains information about the vGPU and its associated VM
 * such as the currently installed guest driver version, together with host driver version and an opaque data section
 * containing internal state.
 *
 * hgmlVgpuInstanceGetMetadata() may be called at any time for a vGPU instance. Some fields in the returned structure are
 * dependent on information obtained from the guest VM, which may not yet have reached a state where that information
 * is available. The current state of these dependent fields is reflected in the info structure's \ref hgmlVgpuGuestInfoState_t field.
 *
 * The VMM may choose to read and save the vGPU's VM info as persistent metadata associated with the VM, and provide
 * it to Virtual GPU Manager when creating a vGPU for subsequent instances of the VM.
 *
 * The caller passes in a buffer via \a vgpuMetadata, with the size of the buffer in \a bufferSize. If the vGPU Metadata structure
 * is too large to fit in the supplied buffer, the function returns HGML_ERROR_INSUFFICIENT_SIZE with the size needed
 * in \a bufferSize.
 *
 * @param vgpuInstance             vGPU instance handle
 * @param vgpuMetadata             Pointer to caller-supplied buffer into which vGPU metadata is written
 * @param bufferSize               Size of vgpuMetadata buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                   vGPU metadata structure was successfully returned
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE   vgpuMetadata buffer is too small, required size is returned in \a bufferSize
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a bufferSize is NULL or \a vgpuInstance is 0; if \a vgpuMetadata is NULL and the value of \a bufferSize is not 0.
 *         - \ref HGML_ERROR_NOT_FOUND           if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetMetadata(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuMetadata_t *vgpuMetadata, unsigned int *bufferSize);

/**
 * Returns a vGPU metadata structure for the physical GPU indicated by \a device. The structure contains information about
 * the GPU and the currently installed host driver version that's controlling it, together with an opaque data section
 * containing internal state.
 *
 * The caller passes in a buffer via \a pgpuMetadata, with the size of the buffer in \a bufferSize. If the \a pgpuMetadata
 * structure is too large to fit in the supplied buffer, the function returns HGML_ERROR_INSUFFICIENT_SIZE with the size needed
 * in \a bufferSize.
 *
 * @param device                The identifier of the target device
 * @param pgpuMetadata          Pointer to caller-supplied buffer into which \a pgpuMetadata is written
 * @param bufferSize            Pointer to size of \a pgpuMetadata buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                   GPU metadata structure was successfully returned
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE   pgpuMetadata buffer is too small, required size is returned in \a bufferSize
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a bufferSize is NULL or \a device is invalid; if \a pgpuMetadata is NULL and the value of \a bufferSize is not 0.
 *         - \ref HGML_ERROR_NOT_SUPPORTED       vGPU is not supported by the system
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuMetadata(hgmlDevice_t device, hgmlVgpuPgpuMetadata_t *pgpuMetadata, unsigned int *bufferSize);

/**
 * Takes a vGPU instance metadata structure read from \ref hgmlVgpuInstanceGetMetadata(), and a vGPU metadata structure for a
 * physical GPU read from \ref hgmlDeviceGetVgpuMetadata(), and returns compatibility information of the vGPU instance and the
 * physical GPU.
 *
 * The caller passes in a buffer via \a compatibilityInfo, into which a compatibility information structure is written. The
 * structure defines the states in which the vGPU / VM may be booted on the physical GPU. If the vGPU / VM compatibility
 * with the physical GPU is limited, a limit code indicates the factor limiting compatability.
 * (see \ref hgmlVgpuPgpuCompatibilityLimitCode_t for details).
 *
 * Note: vGPU compatibility does not take into account dynamic capacity conditions that may limit a system's ability to
 *       boot a given vGPU or associated VM.
 *
 * @param vgpuMetadata          Pointer to caller-supplied vGPU metadata structure
 * @param pgpuMetadata          Pointer to caller-supplied GPU metadata structure
 * @param compatibilityInfo     Pointer to caller-supplied buffer to hold compatibility info
 *
 * @return
 *         - \ref HGML_SUCCESS                   vGPU metadata structure was successfully returned
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a vgpuMetadata or \a pgpuMetadata or \a bufferSize are NULL
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlGetVgpuCompatibility(hgmlVgpuMetadata_t *vgpuMetadata, hgmlVgpuPgpuMetadata_t *pgpuMetadata, hgmlVgpuPgpuCompatibility_t *compatibilityInfo);

/**
 * Returns the properties of the physical GPU indicated by the device in an ascii-encoded string format.
 *
 * The caller passes in a buffer via \a pgpuMetadata, with the size of the buffer in \a bufferSize. If the
 * string is too large to fit in the supplied buffer, the function returns HGML_ERROR_INSUFFICIENT_SIZE with the size needed
 * in \a bufferSize.
 *
 * @param device                The identifier of the target device
 * @param pgpuMetadata          Pointer to caller-supplied buffer into which \a pgpuMetadata is written
 * @param bufferSize            Pointer to size of \a pgpuMetadata buffer
 *
 * @return
 *         - \ref HGML_SUCCESS                   GPU metadata structure was successfully returned
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE   \a pgpuMetadata buffer is too small, required size is returned in \a bufferSize
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a bufferSize is NULL or \a device is invalid; if \a pgpuMetadata is NULL and the value of \a bufferSize is not 0.
 *         - \ref HGML_ERROR_NOT_SUPPORTED       if vGPU is not supported by the system
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetPgpuMetadataString(hgmlDevice_t device, char *pgpuMetadata, unsigned int *bufferSize);

/**
 * Returns the vGPU Software scheduler logs.
 * \a pSchedulerLog points to a caller-allocated structure to contain the logs. The number of elements returned will
 * never exceed \a HGML_SCHEDULER_SW_MAX_LOG_ENTRIES.
 *
 * To get the entire logs, call the function atleast 5 times a second.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                The identifier of the target \a device
 * @param pSchedulerLog         Reference in which \a pSchedulerLog is written
 *
 * @return
 *         - \ref HGML_SUCCESS                   vGPU scheduler logs were successfully obtained
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a pSchedulerLog is NULL or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED       The API is not supported in current state or \a device not in vGPU host mode
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuSchedulerLog(hgmlDevice_t device, hgmlVgpuSchedulerLog_t *pSchedulerLog);

/**
 * Returns the vGPU scheduler state.
 * The information returned in \a hgmlVgpuSchedulerGetState_t is not relevant if the BEST EFFORT policy is set.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                The identifier of the target \a device
 * @param pSchedulerState       Reference in which \a pSchedulerState is returned
 *
 * @return
 *         - \ref HGML_SUCCESS                   vGPU scheduler state is successfully obtained
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a pSchedulerState is NULL or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED       The API is not supported in current state or \a device not in vGPU host mode
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuSchedulerState(hgmlDevice_t device, hgmlVgpuSchedulerGetState_t *pSchedulerState);

/**
 * Returns the vGPU scheduler capabilities.
 * The list of supported vGPU schedulers returned in \a hgmlVgpuSchedulerCapabilities_t is from
 * the HGML_VGPU_SCHEDULER_POLICY_*. This list enumerates the supported scheduler policies
 * if the engine is Graphics type.
 * The other values in \a hgmlVgpuSchedulerCapabilities_t are also applicable if the engine is
 * Graphics type. For other engine types, it is BEST EFFORT policy.
 * If ARR is supported and enabled, scheduling frequency and averaging factor are applicable
 * else timeSlice is applicable.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                The identifier of the target \a device
 * @param pCapabilities         Reference in which \a pCapabilities is written
 *
 * @return
 *         - \ref HGML_SUCCESS                   vGPU scheduler capabilities were successfully obtained
 *         - \ref HGML_ERROR_INVALID_ARGUMENT    if \a pCapabilities is NULL or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED       The API is not supported in current state or \a device not in vGPU host mode
 *         - \ref HGML_ERROR_UNKNOWN             on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuSchedulerCapabilities(hgmlDevice_t device, hgmlVgpuSchedulerCapabilities_t *pCapabilities);

/**
 * Sets the vGPU scheduler state.
 *
 * For &tm; or newer fully supported devices.
 *
 * The scheduler state change won't persist across module load/unload.
 * Scheduler state and params will be allowed to set only when no VM is running.
 * In \a hgmlVgpuSchedulerSetState_t, IFF enableARRMode is enabled then
 * provide avgFactorForARR and frequency as input. If enableARRMode is disabled
 * then provide timeslice as input.
 *
 * @param device                The identifier of the target \a device
 * @param pSchedulerState       vGPU \a pSchedulerState to set
 *
 * @return
 *         - \ref HGML_SUCCESS                  vGPU scheduler state has been successfully set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a pSchedulerState is NULL or \a device is invalid
 *         - \ref HGML_ERROR_RESET_REQUIRED     if setting \a pSchedulerState failed with fatal error,
 *                                              reboot is required to overcome from this error.
 *         - \ref HGML_ERROR_NOT_SUPPORTED      The API is not supported in current state or \a device not in vGPU host mode
 *                                              or if any vGPU instance currently exists on the \a device
 *         - \ref HGML_ERROR_UNKNOWN            on any unexpected error
 */
hgmlReturn_t hgmlDeviceSetVgpuSchedulerState(hgmlDevice_t device, hgmlVgpuSchedulerSetState_t *pSchedulerState);

/*
 * Virtual GPU (vGPU) version
 *
 * The vGPU Manager and the guest drivers are tagged with a range of supported vGPU versions. This determines the range of guest driver versions that
 * are compatible for vGPU feature support with a given vGPU Manager. For vGPU feature support, the range of supported versions for the vGPU Manager
 * and the guest driver must overlap. Otherwise, the guest driver fails to load in the VM.
 *
 * When the guest driver loads, either when the VM is booted or when the driver is installed or upgraded, a negotiation occurs between the guest driver
 * and the vGPU Manager to select the highest mutually compatible vGPU version. The negotiated vGPU version stays the same across VM migration.
 */

/**
 * Query the ranges of supported vGPU versions.
 *
 * This function gets the linear range of supported vGPU versions that is preset for the vGPU Manager and the range set by an administrator.
 * If the preset range has not been overridden by \ref hgmlSetVgpuVersion, both ranges are the same.
 *
 * The caller passes pointers to the following \ref hgmlVgpuVersion_t structures, into which the vGPU Manager writes the ranges:
 * 1. \a supported structure that represents the preset range of vGPU versions supported by the vGPU Manager.
 * 2. \a current structure that represents the range of supported vGPU versions set by an administrator. By default, this range is the same as the preset range.
 *
 * @param supported  Pointer to the structure in which the preset range of vGPU versions supported by the vGPU Manager is written
 * @param current    Pointer to the structure in which the range of supported vGPU versions set by an administrator is written
 *
 * @return
 * - \ref HGML_SUCCESS                 The vGPU version range structures were successfully obtained.
 * - \ref HGML_ERROR_NOT_SUPPORTED     The API is not supported.
 * - \ref HGML_ERROR_INVALID_ARGUMENT  The \a supported parameter or the \a current parameter is NULL.
 * - \ref HGML_ERROR_UNKNOWN           An error occurred while the data was being fetched.
 */
hgmlReturn_t hgmlGetVgpuVersion(hgmlVgpuVersion_t *supported, hgmlVgpuVersion_t *current);

/**
 * Override the preset range of vGPU versions supported by the vGPU Manager with a range set by an administrator.
 *
 * This function configures the vGPU Manager with a range of supported vGPU versions set by an administrator. This range must be a subset of the
 * preset range that the vGPU Manager supports. The custom range set by an administrator takes precedence over the preset range and is advertised to
 * the guest VM for negotiating the vGPU version. See \ref hgmlGetVgpuVersion for details of how to query the preset range of versions supported.
 *
 * This function takes a pointer to vGPU version range structure \ref hgmlVgpuVersion_t as input to override the preset vGPU version range that the vGPU Manager supports.
 *
 * After host system reboot or driver reload, the range of supported versions reverts to the range that is preset for the vGPU Manager.
 *
 * @note 1. The range set by the administrator must be a subset of the preset range that the vGPU Manager supports. Otherwise, an error is returned.
 *       2. If the range of supported guest driver versions does not overlap the range set by the administrator, the guest driver fails to load.
 *       3. If the range of supported guest driver versions overlaps the range set by the administrator, the guest driver will load with a negotiated
 *          vGPU version that is the maximum value in the overlapping range.
 *       4. No VMs must be running on the host when this function is called. If a VM is running on the host, the call to this function fails.
 *
 * @param vgpuVersion   Pointer to a caller-supplied range of supported vGPU versions.
 *
 * @return
 * - \ref HGML_SUCCESS                 The preset range of supported vGPU versions was successfully overridden.
 * - \ref HGML_ERROR_NOT_SUPPORTED     The API is not supported.
 * - \ref HGML_ERROR_IN_USE            The range was not overridden because a VM is running on the host.
 * - \ref HGML_ERROR_INVALID_ARGUMENT  The \a vgpuVersion parameter specifies a range that is outside the range supported by the vGPU Manager or if \a vgpuVersion is NULL.
 */
hgmlReturn_t hgmlSetVgpuVersion(hgmlVgpuVersion_t *vgpuVersion);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlUtil vGPU Utilization and Accounting
 * This chapter describes operations that are associated with vGPU Utilization and Accounting.
 *  @{
 */
/***************************************************************************************************/

/**
 * Retrieves current utilization for vGPUs on a physical GPU (device).
 *
 * For &tm; or newer fully supported devices.
 *
 * Reads recent utilization of GPU SM (3D/Compute), framebuffer, video encoder, and video decoder for vGPU instances running
 * on a device. Utilization values are returned as an array of utilization sample structures in the caller-supplied buffer
 * pointed at by \a utilizationSamples. One utilization sample structure is returned per vGPU instance, and includes the
 * CPU timestamp at which the samples were recorded. Individual utilization values are returned as "unsigned int" values
 * in hgmlValue_t unions. The function sets the caller-supplied \a sampleValType to HGML_VALUE_TYPE_UNSIGNED_INT to
 * indicate the returned value type.
 *
 * To read utilization values, first determine the size of buffer required to hold the samples by invoking the function with
 * \a utilizationSamples set to NULL. The function will return HGML_ERROR_INSUFFICIENT_SIZE, with the current vGPU instance
 * count in \a vgpuInstanceSamplesCount, or HGML_SUCCESS if the current vGPU instance count is zero. The caller should allocate
 * a buffer of size vgpuInstanceSamplesCount * sizeof(hgmlVgpuInstanceUtilizationSample_t). Invoke the function again with
 * the allocated buffer passed in \a utilizationSamples, and \a vgpuInstanceSamplesCount set to the number of entries the
 * buffer is sized for.
 *
 * On successful return, the function updates \a vgpuInstanceSampleCount with the number of vGPU utilization sample
 * structures that were actually written. This may differ from a previously read value as vGPU instances are created or
 * destroyed.
 *
 * lastSeenTimeStamp represents the CPU timestamp in microseconds at which utilization samples were last read. Set it to 0
 * to read utilization based on all the samples maintained by the driver's internal sample buffer. Set lastSeenTimeStamp
 * to a timeStamp retrieved from a previous query to read utilization since the previous query.
 *
 * @param device                        The identifier for the target device
 * @param lastSeenTimeStamp             Return only samples with timestamp greater than lastSeenTimeStamp.
 * @param sampleValType                 Pointer to caller-supplied buffer to hold the type of returned sample values
 * @param vgpuInstanceSamplesCount      Pointer to caller-supplied array size, and returns number of vGPU instances
 * @param utilizationSamples            Pointer to caller-supplied buffer in which vGPU utilization samples are returned

 * @return
 *         - \ref HGML_SUCCESS                 if utilization samples are successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a vgpuInstanceSamplesCount or \a sampleValType is
 *                                             NULL, or a sample count of 0 is passed with a non-NULL \a utilizationSamples
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if supplied \a vgpuInstanceSamplesCount is too small to return samples for all
 *                                             vGPU instances currently executing on the device
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if vGPU is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_FOUND         if sample entries are not found
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuUtilization(hgmlDevice_t device, unsigned long long lastSeenTimeStamp,
                                                  hgmlValueType_t *sampleValType, unsigned int *vgpuInstanceSamplesCount,
                                                  hgmlVgpuInstanceUtilizationSample_t *utilizationSamples);

/**
 * Retrieves current utilization for processes running on vGPUs on a physical GPU (device).
 *
 * For &tm; or newer fully supported devices.
 *
 * Reads recent utilization of GPU SM (3D/Compute), framebuffer, video encoder, and video decoder for processes running on
 * vGPU instances active on a device. Utilization values are returned as an array of utilization sample structures in the
 * caller-supplied buffer pointed at by \a utilizationSamples. One utilization sample structure is returned per process running
 * on vGPU instances, that had some non-zero utilization during the last sample period. It includes the CPU timestamp at which
 * the samples were recorded. Individual utilization values are returned as "unsigned int" values.
 *
 * To read utilization values, first determine the size of buffer required to hold the samples by invoking the function with
 * \a utilizationSamples set to NULL. The function will return HGML_ERROR_INSUFFICIENT_SIZE, with the current vGPU instance
 * count in \a vgpuProcessSamplesCount. The caller should allocate a buffer of size
 * vgpuProcessSamplesCount * sizeof(hgmlVgpuProcessUtilizationSample_t). Invoke the function again with
 * the allocated buffer passed in \a utilizationSamples, and \a vgpuProcessSamplesCount set to the number of entries the
 * buffer is sized for.
 *
 * On successful return, the function updates \a vgpuSubProcessSampleCount with the number of vGPU sub process utilization sample
 * structures that were actually written. This may differ from a previously read value depending on the number of processes that are active
 * in any given sample period.
 *
 * lastSeenTimeStamp represents the CPU timestamp in microseconds at which utilization samples were last read. Set it to 0
 * to read utilization based on all the samples maintained by the driver's internal sample buffer. Set lastSeenTimeStamp
 * to a timeStamp retrieved from a previous query to read utilization since the previous query.
 *
 * @param device                        The identifier for the target device
 * @param lastSeenTimeStamp             Return only samples with timestamp greater than lastSeenTimeStamp.
 * @param vgpuProcessSamplesCount       Pointer to caller-supplied array size, and returns number of processes running on vGPU instances
 * @param utilizationSamples            Pointer to caller-supplied buffer in which vGPU sub process utilization samples are returned

 * @return
 *         - \ref HGML_SUCCESS                 if utilization samples are successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid, \a vgpuProcessSamplesCount or a sample count of 0 is
 *                                             passed with a non-NULL \a utilizationSamples
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if supplied \a vgpuProcessSamplesCount is too small to return samples for all
 *                                             vGPU instances currently executing on the device
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if vGPU is not supported by the device
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_NOT_FOUND         if sample entries are not found
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetVgpuProcessUtilization(hgmlDevice_t device, unsigned long long lastSeenTimeStamp,
                                                         unsigned int *vgpuProcessSamplesCount,
                                                         hgmlVgpuProcessUtilizationSample_t *utilizationSamples);
/**
 * Queries the state of per process accounting mode on vGPU.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance            The identifier of the target vGPU instance
 * @param mode                    Reference in which to return the current accounting mode
 *
 * @return
 *         - \ref HGML_SUCCESS                 if the mode has been successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a mode is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the vGPU doesn't support this feature
 *         - \ref HGML_ERROR_DRIVER_NOT_LOADED if driver is not running on the vGPU instance
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetAccountingMode(hgmlVgpuInstance_t vgpuInstance, hgmlEnableState_t *mode);

/**
 * Queries list of processes running on vGPU that can be queried for accounting stats. The list of processes
 * returned can be in running or terminated state.
 *
 * For &tm; or newer fully supported devices.
 *
 * To just query the maximum number of processes that can be queried, call this function with *count = 0 and
 * pids=NULL. The return code will be HGML_ERROR_INSUFFICIENT_SIZE, or HGML_SUCCESS if list is empty.
 *
 * For more details see \ref hgmlVgpuInstanceGetAccountingStats.
 *
 * @note In case of PID collision some processes might not be accessible before the circular buffer is full.
 *
 * @param vgpuInstance            The identifier of the target vGPU instance
 * @param count                   Reference in which to provide the \a pids array size, and
 *                                to return the number of elements ready to be queried
 * @param pids                    Reference in which to return list of process ids
 *
 * @return
 *         - \ref HGML_SUCCESS                 if pids were successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a count is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the vGPU doesn't support this feature or accounting mode is disabled
 *         - \ref HGML_ERROR_INSUFFICIENT_SIZE if \a count is too small (\a count is set to expected value)
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see hgmlVgpuInstanceGetAccountingPids
 */
hgmlReturn_t hgmlVgpuInstanceGetAccountingPids(hgmlVgpuInstance_t vgpuInstance, unsigned int *count, unsigned int *pids);

/**
 * Queries process's accounting stats.
 *
 * For &tm; or newer fully supported devices.
 *
 * Accounting stats capture GPU utilization and other statistics across the lifetime of a process, and
 * can be queried during life time of the process or after its termination.
 * The time field in \ref hgmlAccountingStats_t is reported as 0 during the lifetime of the process and
 * updated to actual running time after its termination.
 * Accounting stats are kept in a circular buffer, newly created processes overwrite information about old
 * processes.
 *
 * See \ref hgmlAccountingStats_t for description of each returned metric.
 * List of processes that can be queried can be retrieved from \ref hgmlVgpuInstanceGetAccountingPids.
 *
 * @note Accounting Mode needs to be on. See \ref hgmlVgpuInstanceGetAccountingMode.
 * @note Only compute and graphics applications stats can be queried. Monitoring applications stats can't be
 *         queried since they don't contribute to GPU utilization.
 * @note In case of pid collision stats of only the latest process (that terminated last) will be reported
 *
 * @param vgpuInstance            The identifier of the target vGPU instance
 * @param pid                     Process Id of the target process to query stats for
 * @param stats                   Reference in which to return the process's accounting stats
 *
 * @return
 *         - \ref HGML_SUCCESS                 if stats have been successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a stats is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *                                             or \a stats is not found
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the vGPU doesn't support this feature or accounting mode is disabled
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetAccountingStats(hgmlVgpuInstance_t vgpuInstance, unsigned int pid, hgmlAccountingStats_t *stats);

/**
 * Clears accounting information of the vGPU instance that have already terminated.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * @note Accounting Mode needs to be on. See \ref hgmlVgpuInstanceGetAccountingMode.
 * @note Only compute and graphics applications stats are reported and can be cleared since monitoring applications
 *         stats don't contribute to GPU utilization.
 *
 * @param vgpuInstance            The identifier of the target vGPU instance
 *
 * @return
 *         - \ref HGML_SUCCESS                 if accounting information has been cleared
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is invalid
 *         - \ref HGML_ERROR_NO_PERMISSION     if the user doesn't have permission to perform this operation
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the vGPU doesn't support this feature or accounting mode is disabled
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceClearAccountingPids(hgmlVgpuInstance_t vgpuInstance);

/**
 * Query the license information of the vGPU instance.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param vgpuInstance              Identifier of the target vGPU instance
 * @param licenseInfo               Pointer to vGPU license information structure
 *
 * @return
 *         - \ref HGML_SUCCESS                 if information is successfully retrieved
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a vgpuInstance is 0, or \a licenseInfo is NULL
 *         - \ref HGML_ERROR_NOT_FOUND         if \a vgpuInstance does not match a valid active vGPU instance on the system
 *         - \ref HGML_ERROR_DRIVER_NOT_LOADED if alixpu driver is not running on the vGPU instance
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlVgpuInstanceGetLicenseInfo_v2(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuLicenseInfo_t *licenseInfo);
/** @} */

/***************************************************************************************************/
/** @defgroup hgmlExcludedGpuQueries Excluded GPU Queries
 * This chapter describes HGML operations that are associated with excluded GPUs.
 *  @{
 */
/***************************************************************************************************/

/**
 * Excluded GPU device information
 **/
typedef struct hgmlBlacklistDeviceInfo_t
{
    hgmlPciInfo_t pciInfo;                   //!< The PCI information for the excluded GPU
    char uuid[HGML_DEVICE_UUID_BUFFER_SIZE]; //!< The ASCII string UUID for the excluded GPU
} hgmlBlacklistDeviceInfo_t;

typedef struct hgmlExcludedDeviceInfo_st
{
    hgmlPciInfo_t pciInfo;                   //!< The PCI information for the excluded GPU
    char uuid[HGML_DEVICE_UUID_BUFFER_SIZE]; //!< The ASCII string UUID for the excluded GPU
} hgmlExcludedDeviceInfo_t;

/**
 * Retrieves the number of excluded GPU devices in the system.
 *
 * For all products.
 *
 * @param deviceCount                          Reference in which to return the number of excluded devices
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a deviceCount has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a deviceCount is NULL
 */
hgmlReturn_t hgmlGetExcludedDeviceCount(unsigned int *deviceCount);

/**
 * Acquire the device information for an excluded GPU device, based on its index.
 *
 * For all products.
 *
 * Valid indices are derived from the \a deviceCount returned by
 *   \ref hgmlGetExcludedDeviceCount(). For example, if \a deviceCount is 2 the valid indices
 *   are 0 and 1, corresponding to GPU 0 and GPU 1.
 *
 * @param index                                The index of the target GPU, >= 0 and < \a deviceCount
 * @param info                                 Reference in which to return the device information
 *
 * @return
 *         - \ref HGML_SUCCESS                  if \a device has been set
 *         - \ref HGML_ERROR_INVALID_ARGUMENT   if \a index is invalid or \a info is NULL
 *
 * @see hgmlGetExcludedDeviceCount
 */
hgmlReturn_t hgmlGetExcludedDeviceInfoByIndex(unsigned int index, hgmlExcludedDeviceInfo_t *info);

/** @} */

/***************************************************************************************************/
/** @defgroup hgmlMultiInstanceGPU Multi Instance GPU Management
 * This chapter describes HGML operations that are associated with Multi Instance GPU management.
 *  @{
 */
/***************************************************************************************************/

/**
 * Disable Multi Instance GPU mode.
 */
#define HGML_DEVICE_MIG_DISABLE 0x0

/**
 * Enable Multi Instance GPU mode.
 */
#define HGML_DEVICE_MIG_ENABLE 0x1

/**
 * GPU instance profiles.
 *
 * These macros should be passed to \ref hgmlDeviceGetGpuInstanceProfileInfo to retrieve the
 * detailed information about a GPU instance such as profile ID, engine counts.
 */
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

#define HGML_GPU_INSTANCE_PROFILE_2_CE         0xa
#define HGML_GPU_INSTANCE_PROFILE_4_CE         0xb
#define HGML_GPU_INSTANCE_PROFILE_8_CE         0xc
#define HGML_GPU_INSTANCE_PROFILE_16_CE        0xd
#define HGML_GPU_INSTANCE_PROFILE_17_CE        0xe

#define HGML_GPU_INSTANCE_PROFILE_COUNT        0xf

/**
 * MIG GPU instance profile capability.
 *
 * Bit field values representing MIG profile capabilities
 * \ref hgmlGpuInstanceProfileInfo_v3_t.capabilities
 */
#define HGML_GPU_INTSTANCE_PROFILE_CAPS_P2P     0x1

/**
 * MIG compute instance profile capability.
 *
 * Bit field values representing MIG profile capabilities
 * \ref hgmlComputeInstanceProfileInfo_v3_t.capabilities
 */
/* No capabilities for compute profiles currently exposed */

typedef struct hgmlGpuInstancePlacement_st
{
    unsigned int start;               //!< Index of first occupied memory slice
    unsigned int size;                //!< Number of memory slices occupied
} hgmlGpuInstancePlacement_t;

/**
 * GPU instance profile information.
 */
typedef struct hgmlGpuInstanceProfileInfo_st
{
    unsigned int id;                  //!< Unique profile ID within the device
    unsigned int isP2pSupported;      //!< Peer-to-Peer support
    unsigned int sliceCount;          //!< GPU Slice count
    unsigned int instanceCount;       //!< GPU instance count
    unsigned int multiprocessorCount; //!< Streaming Multiprocessor count
    unsigned int copyEngineCount;     //!< Copy Engine count
    unsigned int decoderCount;        //!< Decoder Engine count
    unsigned int encoderCount;        //!< Encoder Engine count
    unsigned int jpegCount;           //!< JPEG Engine count
    unsigned int ofaCount;            //!< OFA Engine count
    unsigned long long memorySizeMB;  //!< Memory size in MBytes
} hgmlGpuInstanceProfileInfo_t;

/**
 * GPU instance profile information (v2).
 *
 * Version 2 adds the \ref hgmlGpuInstanceProfileInfo_v2_t.version field
 * to the start of the structure, and the \ref hgmlGpuInstanceProfileInfo_v2_t.name
 * field to the end. This structure is not backwards-compatible with
 * \ref hgmlGpuInstanceProfileInfo_t.
 */
typedef struct hgmlGpuInstanceProfileInfo_v2_st
{
    unsigned int version;                       //!< Structure version identifier (set to \ref hgmlGpuInstanceProfileInfo_v2)
    unsigned int id;                            //!< Unique profile ID within the device
    unsigned int isP2pSupported;                //!< Peer-to-Peer support
    unsigned int sliceCount;                    //!< GPU Slice count
    unsigned int instanceCount;                 //!< GPU instance count
    unsigned int multiprocessorCount;           //!< Streaming Multiprocessor count
    unsigned int copyEngineCount;               //!< Copy Engine count
    unsigned int decoderCount;                  //!< Decoder Engine count
    unsigned int encoderCount;                  //!< Encoder Engine count
    unsigned int jpegCount;                     //!< JPEG Engine count
    unsigned int ofaCount;                      //!< OFA Engine count
    unsigned long long memorySizeMB;            //!< Memory size in MBytes
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE]; //!< Profile name
} hgmlGpuInstanceProfileInfo_v2_t;

/**
 * Version identifier value for \ref hgmlGpuInstanceProfileInfo_v2_t.version.
 */
#define hgmlGpuInstanceProfileInfo_v2 HGML_STRUCT_VERSION(GpuInstanceProfileInfo, 2)

/**
 * GPU instance profile information (v3).
 *
 * Version 3 removes isP2pSupported field and adds the \ref hgmlGpuInstanceProfileInfo_v3_t.capabilities
 * field \ref hgmlGpuInstanceProfileInfo_t.
 */
typedef struct hgmlGpuInstanceProfileInfo_v3_st
{
    unsigned int version;                       //!< Structure version identifier (set to \ref hgmlGpuInstanceProfileInfo_v3)
    unsigned int id;                            //!< Unique profile ID within the device
    unsigned int sliceCount;                    //!< GPU Slice count
    unsigned int instanceCount;                 //!< GPU instance count
    unsigned int multiprocessorCount;           //!< Streaming Multiprocessor count
    unsigned int copyEngineCount;               //!< Copy Engine count
    unsigned int decoderCount;                  //!< Decoder Engine count
    unsigned int encoderCount;                  //!< Encoder Engine count
    unsigned int jpegCount;                     //!< JPEG Engine count
    unsigned int ofaCount;                      //!< OFA Engine count
    unsigned long long memorySizeMB;            //!< Memory size in MBytes
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE]; //!< Profile name
    unsigned int capabilities;                  //!< Additional capabilities
} hgmlGpuInstanceProfileInfo_v3_t;

/**
 * Version identifier value for \ref hgmlGpuInstanceProfileInfo_v3_t.version.
 */
#define hgmlGpuInstanceProfileInfo_v3 HGML_STRUCT_VERSION(GpuInstanceProfileInfo, 3)

typedef struct hgmlGpuInstanceInfo_st
{
    hgmlDevice_t device;                      //!< Parent device
    unsigned int id;                          //!< Unique instance ID within the device
    unsigned int profileId;                   //!< Unique profile ID within the device
    hgmlGpuInstancePlacement_t placement;     //!< Placement for this instance
} hgmlGpuInstanceInfo_t;

typedef struct hgmlGpuInstance_st* hgmlGpuInstance_t;

/**
 * Compute instance profiles.
 *
 * These macros should be passed to \ref hgmlGpuInstanceGetComputeInstanceProfileInfo to retrieve the
 * detailed information about a compute instance such as profile ID, engine counts
 */
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
#define HGML_COMPUTE_INSTANCE_PROFILE_17_CE         0x1b

#define HGML_COMPUTE_INSTANCE_PROFILE_COUNT         0x1c

#define HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED 0x0 //!< All the engines except multiprocessors would be shared
#define HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT  0x1

typedef struct hgmlComputeInstancePlacement_st
{
    unsigned int start;                 //!< Index of first occupied compute slice
    unsigned int size;                  //!< Number of compute slices occupied
} hgmlComputeInstancePlacement_t;

/**
 * Compute instance profile information.
 */
typedef struct hgmlComputeInstanceProfileInfo_st
{
    unsigned int id;                    //!< Unique profile ID within the GPU instance
    unsigned int sliceCount;            //!< GPU Slice count
    unsigned int instanceCount;         //!< Compute instance count
    unsigned int multiprocessorCount;   //!< Streaming Multiprocessor count
    unsigned int sharedCopyEngineCount; //!< Shared Copy Engine count
    unsigned int sharedDecoderCount;    //!< Shared Decoder Engine count
    unsigned int sharedEncoderCount;    //!< Shared Encoder Engine count
    unsigned int sharedJpegCount;       //!< Shared JPEG Engine count
    unsigned int sharedOfaCount;        //!< Shared OFA Engine count
} hgmlComputeInstanceProfileInfo_t;

/**
 * Compute instance profile information (v2).
 *
 * Version 2 adds the \ref hgmlComputeInstanceProfileInfo_v2_t.version field
 * to the start of the structure, and the \ref hgmlComputeInstanceProfileInfo_v2_t.name
 * field to the end. This structure is not backwards-compatible with
 * \ref hgmlComputeInstanceProfileInfo_t.
 */
typedef struct hgmlComputeInstanceProfileInfo_v2_st
{
    unsigned int version;                       //!< Structure version identifier (set to \ref hgmlComputeInstanceProfileInfo_v2)
    unsigned int id;                            //!< Unique profile ID within the GPU instance
    unsigned int sliceCount;                    //!< GPU Slice count
    unsigned int instanceCount;                 //!< Compute instance count
    unsigned int multiprocessorCount;           //!< Streaming Multiprocessor count
    unsigned int sharedCopyEngineCount;         //!< Shared Copy Engine count
    unsigned int sharedDecoderCount;            //!< Shared Decoder Engine count
    unsigned int sharedEncoderCount;            //!< Shared Encoder Engine count
    unsigned int sharedJpegCount;               //!< Shared JPEG Engine count
    unsigned int sharedOfaCount;                //!< Shared OFA Engine count
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE]; //!< Profile name
} hgmlComputeInstanceProfileInfo_v2_t;

/**
 * Version identifier value for \ref hgmlComputeInstanceProfileInfo_v2_t.version.
 */
#define hgmlComputeInstanceProfileInfo_v2 HGML_STRUCT_VERSION(ComputeInstanceProfileInfo, 2)

/**
 * Compute instance profile information (v3).
 *
 * Version 3 adds the \ref hgmlComputeInstanceProfileInfo_v3_t.capabilities field
 * \ref hgmlComputeInstanceProfileInfo_t.
 */
typedef struct hgmlComputeInstanceProfileInfo_v3_st
{
    unsigned int version;                       //!< Structure version identifier (set to \ref hgmlComputeInstanceProfileInfo_v3)
    unsigned int id;                            //!< Unique profile ID within the GPU instance
    unsigned int sliceCount;                    //!< GPU Slice count
    unsigned int instanceCount;                 //!< Compute instance count
    unsigned int multiprocessorCount;           //!< Streaming Multiprocessor count
    unsigned int sharedCopyEngineCount;         //!< Shared Copy Engine count
    unsigned int sharedDecoderCount;            //!< Shared Decoder Engine count
    unsigned int sharedEncoderCount;            //!< Shared Encoder Engine count
    unsigned int sharedJpegCount;               //!< Shared JPEG Engine count
    unsigned int sharedOfaCount;                //!< Shared OFA Engine count
    char name[HGML_DEVICE_NAME_V2_BUFFER_SIZE]; //!< Profile name
    unsigned int capabilities;                  //!< Additional capabilities
} hgmlComputeInstanceProfileInfo_v3_t;

/**
 * Version identifier value for \ref hgmlComputeInstanceProfileInfo_v3_t.version.
 */
#define hgmlComputeInstanceProfileInfo_v3 HGML_STRUCT_VERSION(ComputeInstanceProfileInfo, 3)

typedef struct hgmlComputeInstanceInfo_st
{
    hgmlDevice_t device;                      //!< Parent device
    hgmlGpuInstance_t gpuInstance;            //!< Parent GPU instance
    unsigned int id;                          //!< Unique instance ID within the GPU instance
    unsigned int profileId;                   //!< Unique profile ID within the GPU instance
    hgmlComputeInstancePlacement_t placement; //!< Placement for this instance within the GPU instance's compute slice range {0, sliceCount}
} hgmlComputeInstanceInfo_t;

typedef struct hgmlComputeInstance_st* hgmlComputeInstance_t;

/**
 * Set MIG mode for the device.
 *
 * For &tm; or newer fully supported devices.
 * Requires root user.
 *
 * This mode determines whether a GPU instance can be created.
 *
 * This API may unbind or reset the device to activate the requested mode. Thus, the attributes associated with the
 * device, such as minor number, might change. The caller of this API is expected to query such attributes again.
 *
 * On certain platforms like pass-through virtualization, where reset functionality may not be exposed directly, VM
 * reboot is required. \a activationStatus would return \ref HGML_ERROR_RESET_REQUIRED for such cases.
 *
 * \a activationStatus would return the appropriate error code upon unsuccessful activation. For example, if device
 * unbind fails because the device isn't idle, \ref HGML_ERROR_IN_USE would be returned. The caller of this API
 * is expected to idle the device and retry setting the \a mode.
 *
 * @note On Windows, only disabling MIG mode is supported. \a activationStatus would return \ref
 *       HGML_ERROR_NOT_SUPPORTED as GPU reset is not supported on Windows through this API.
 *
 * @param device                               The identifier of the target device
 * @param mode                                 The mode to be set, \ref HGML_DEVICE_MIG_DISABLE or
 *                                             \ref HGML_DEVICE_MIG_ENABLE
 * @param activationStatus                     The activationStatus status
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device,\a mode or \a activationStatus are invalid
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't support MIG mode
 */
hgmlReturn_t hgmlDeviceSetMigMode(hgmlDevice_t device, unsigned int mode, hgmlReturn_t *activationStatus);

/**
 * Get MIG mode for the device.
 *
 * For &tm; or newer fully supported devices.
 *
 * Changing MIG modes may require device unbind or reset. The "pending" MIG mode refers to the target mode following the
 * next activation trigger.
 *
 * @param device                               The identifier of the target device
 * @param currentMode                          Returns the current mode, \ref HGML_DEVICE_MIG_DISABLE or
 *                                             \ref HGML_DEVICE_MIG_ENABLE
 * @param pendingMode                          Returns the pending mode, \ref HGML_DEVICE_MIG_DISABLE or
 *                                             \ref HGML_DEVICE_MIG_ENABLE
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a currentMode or \a pendingMode are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't support MIG mode
 */
hgmlReturn_t hgmlDeviceGetMigMode(hgmlDevice_t device, unsigned int *currentMode, unsigned int *pendingMode);

/**
 * Get GPU instance profile information.
 *
 * Information provided by this API is immutable throughout the lifetime of a MIG mode.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 * @param profile                              One of the HGML_GPU_INSTANCE_PROFILE_*
 * @param info                                 Returns detailed profile information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profile or \a info are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profile isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstanceProfileInfo(hgmlDevice_t device, unsigned int profile,
                                                         hgmlGpuInstanceProfileInfo_t *info);

/**
 * Get GPU instance placements.
 *
 * A placement represents the location of a GPU instance within a device. This API only returns all the possible
 * placements for the given profile.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param placements                           Returns placements, the buffer must be large enough to accommodate
 *                                             the instances supported by the profile.
 *                                             See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param count                                The count of returned placements
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profileId, \a placements or \a count are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstancePossiblePlacements(hgmlDevice_t device, unsigned int profileId,
                                                                hgmlGpuInstancePlacement_t *placements,
                                                                unsigned int *count);


/**
 * Versioned wrapper around \ref hgmlDeviceGetGpuInstanceProfileInfo that accepts a versioned
 * \ref hgmlGpuInstanceProfileInfo_v2_t or later output structure.
 *
 * @note The caller must set the \ref hgmlGpuInstanceProfileInfo_v2_t.version field to the
 * appropriate version prior to calling this function. For example:
 * \code
 *     hgmlGpuInstanceProfileInfo_v2_t profileInfo =
 *         { .version = hgmlGpuInstanceProfileInfo_v2 };
 *     hgmlReturn_t result = hgmlDeviceGetGpuInstanceProfileInfoV(device,
 *                                                                profile,
 *                                                                &profileInfo);
 * \endcode
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               The identifier of the target device
 * @param profile                              One of the HGML_GPU_INSTANCE_PROFILE_*
 * @param info                                 Returns detailed profile information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profile, \a info, or \a info->version are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profile isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstanceProfileInfoV(hgmlDevice_t device, unsigned int profile,
                                                          hgmlGpuInstanceProfileInfo_v2_t *info);

/**
 * Get GPU instance placements.
 *
 * A placement represents the location of a GPU instance within a device. This API only returns all the possible
 * placements for the given profile.
 * A created GPU instance occupies memory slices described by its placement. Creation of new GPU instance will
 * fail if there is overlap with the already occupied memory slices.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param placements                           Returns placements allowed for the profile. Can be NULL to discover number
 *                                             of allowed placements for this profile. If non-NULL must be large enough
 *                                             to accommodate the placements supported by the profile.
 * @param count                                Returns number of allowed placemenets for the profile.
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profileId or \a count are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstancePossiblePlacements_v2(hgmlDevice_t device, unsigned int profileId,
                                                                   hgmlGpuInstancePlacement_t *placements,
                                                                   unsigned int *count);

/**
 * Get GPU instance profile capacity.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param count                                Returns remaining instance count for the profile ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profileId or \a count are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstanceRemainingCapacity(hgmlDevice_t device, unsigned int profileId,
                                                               unsigned int *count);

/**
 * Create GPU instance.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * If the parent device is unbound, reset or the GPU instance is destroyed explicitly, the GPU instance handle would
 * become invalid. The GPU instance must be recreated to acquire a valid handle.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param gpuInstance                          Returns the GPU instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                       Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED           If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT        If \a device, \a profile, \a profileId or \a gpuInstance are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED           If \a device doesn't have MIG mode enabled or in vGPU guest
 *         - \ref HGML_ERROR_NO_PERMISSION           If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_INSUFFICIENT_RESOURCES  If the requested GPU instance could not be created
 */
hgmlReturn_t hgmlDeviceCreateGpuInstance(hgmlDevice_t device, unsigned int profileId,
                                                 hgmlGpuInstance_t *gpuInstance);

/**
 * Create GPU instance with the specified placement.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * If the parent device is unbound, reset or the GPU instance is destroyed explicitly, the GPU instance handle would
 * become invalid. The GPU instance must be recreated to acquire a valid handle.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param placement                            The requested placement. See \ref hgmlDeviceGetGpuInstancePossiblePlacements_v2
 * @param gpuInstance                          Returns the GPU instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                       Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED           If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT        If \a device, \a profile, \a profileId, \a placement or \a gpuInstance
 *                                                   are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED           If \a device doesn't have MIG mode enabled or in vGPU guest
 *         - \ref HGML_ERROR_NO_PERMISSION           If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_INSUFFICIENT_RESOURCES  If the requested GPU instance could not be created
 */
hgmlReturn_t hgmlDeviceCreateGpuInstanceWithPlacement(hgmlDevice_t device, unsigned int profileId,
                                                              const hgmlGpuInstancePlacement_t *placement,
                                                              hgmlGpuInstance_t *gpuInstance);
/**
 * Destroy GPU instance.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param gpuInstance                          The GPU instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or in vGPU guest
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_IN_USE            If the GPU instance is in use. This error would be returned if processes
 *                                             (e.g. HGGC application) or compute instances are active on the
 *                                             GPU instance.
 */
hgmlReturn_t hgmlGpuInstanceDestroy(hgmlGpuInstance_t gpuInstance);

/**
 * Get GPU instances for given profile ID.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param device                               The identifier of the target device
 * @param profileId                            The GPU instance profile ID. See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param gpuInstances                         Returns pre-exiting GPU instances, the buffer must be large enough to
 *                                             accommodate the instances supported by the profile.
 *                                             See \ref hgmlDeviceGetGpuInstanceProfileInfo
 * @param count                                The count of returned GPU instances
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a profileId, \a gpuInstances or \a count are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlDeviceGetGpuInstances(hgmlDevice_t device, unsigned int profileId,
                                               hgmlGpuInstance_t *gpuInstances, unsigned int *count);

/**
 * Get GPU instances for given instance ID.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param device                               The identifier of the target device
 * @param id                                   The GPU instance ID
 * @param gpuInstance                          Returns GPU instance
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a id or \a gpuInstance are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_NOT_FOUND         If the GPU instance is not found.
 */
hgmlReturn_t hgmlDeviceGetGpuInstanceById(hgmlDevice_t device, unsigned int id, hgmlGpuInstance_t *gpuInstance);

/**
 * Get GPU instance information.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param gpuInstance                          The GPU instance handle
 * @param info                                 Return GPU instance information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance or \a info are invalid
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetInfo(hgmlGpuInstance_t gpuInstance, hgmlGpuInstanceInfo_t *info);

/**
 * Get compute instance profile information.
 *
 * Information provided by this API is immutable throughout the lifetime of a MIG mode.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profile                              One of the HGML_COMPUTE_INSTANCE_PROFILE_*
 * @param engProfile                           One of the HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_*
 * @param info                                 Returns detailed profile information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance, \a profile, \a engProfile or \a info are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a profile isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceProfileInfo(hgmlGpuInstance_t gpuInstance, unsigned int profile,
                                                                  unsigned int engProfile,
                                                                  hgmlComputeInstanceProfileInfo_t *info);

/**
 * Versioned wrapper around \ref hgmlGpuInstanceGetComputeInstanceProfileInfo that accepts a versioned
 * \ref hgmlComputeInstanceProfileInfo_v2_t or later output structure.
 *
 * @note The caller must set the \ref hgmlGpuInstanceProfileInfo_v2_t.version field to the
 * appropriate version prior to calling this function. For example:
 * \code
 *     hgmlComputeInstanceProfileInfo_v2_t profileInfo =
 *         { .version = hgmlComputeInstanceProfileInfo_v2 };
 *     hgmlReturn_t result = hgmlGpuInstanceGetComputeInstanceProfileInfoV(gpuInstance,
 *                                                                         profile,
 *                                                                         engProfile,
 *                                                                         &profileInfo);
 * \endcode
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profile                              One of the HGML_COMPUTE_INSTANCE_PROFILE_*
 * @param engProfile                           One of the HGML_COMPUTE_INSTANCE_ENGINE_PROFILE_*
 * @param info                                 Returns detailed profile information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance, \a profile, \a engProfile, \a info, or \a info->version are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a profile isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceProfileInfoV(hgmlGpuInstance_t gpuInstance, unsigned int profile,
                                                                   unsigned int engProfile,
                                                                   hgmlComputeInstanceProfileInfo_v2_t *info);

/**
 * Get compute instance profile capacity.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profileId                            The compute instance profile ID.
 *                                             See \ref hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param count                                Returns remaining instance count for the profile ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance, \a profileId or \a availableCount are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceRemainingCapacity(hgmlGpuInstance_t gpuInstance,
                                                                        unsigned int profileId, unsigned int *count);

/**
 * Get compute instance placements.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * A placement represents the location of a compute instance within a GPU instance. This API only returns all the possible
 * placements for the given profile.
 * A created compute instance occupies compute slices described by its placement. Creation of new compute instance will
 * fail if there is overlap with the already occupied compute slices.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profileId                            The compute instance profile ID. See \ref  hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param placements                           Returns placements allowed for the profile. Can be NULL to discover number
 *                                             of allowed placements for this profile. If non-NULL must be large enough
 *                                             to accommodate the placements supported by the profile.
 * @param count                                Returns number of allowed placemenets for the profile.
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance, \a profileId or \a count are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled or \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstancePossiblePlacements(hgmlGpuInstance_t gpuInstance,
                                                                         unsigned int profileId,
                                                                         hgmlComputeInstancePlacement_t *placements,
                                                                         unsigned int *count);

/**
 * Create compute instance.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * If the parent device is unbound, reset or the parent GPU instance is destroyed or the compute instance is destroyed
 * explicitly, the compute instance handle would become invalid. The compute instance must be recreated to acquire
 * a valid handle.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profileId                            The compute instance profile ID.
 *                                             See \ref hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param computeInstance                      Returns the compute instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                       Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED           If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT        If \a gpuInstance, \a profile, \a profileId or \a computeInstance
 *                                                   are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED           If \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION           If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_INSUFFICIENT_RESOURCES  If the requested compute instance could not be created
 */
hgmlReturn_t hgmlGpuInstanceCreateComputeInstance(hgmlGpuInstance_t gpuInstance, unsigned int profileId,
                                                          hgmlComputeInstance_t *computeInstance);

/**
 * Create compute instance with the specified placement.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * If the parent device is unbound, reset or the parent GPU instance is destroyed or the compute instance is destroyed
 * explicitly, the compute instance handle would become invalid. The compute instance must be recreated to acquire
 * a valid handle.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profileId                            The compute instance profile ID.
 *                                             See \ref hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param placement                            The requested placement. See \ref hgmlGpuInstanceGetComputeInstancePossiblePlacements
 * @param computeInstance                      Returns the compute instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                       Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED           If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT        If \a gpuInstance, \a profile, \a profileId or \a computeInstance
 *                                                   are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED           If \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION           If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_INSUFFICIENT_RESOURCES  If the requested compute instance could not be created
 */
hgmlReturn_t hgmlGpuInstanceCreateComputeInstanceWithPlacement(hgmlGpuInstance_t gpuInstance, unsigned int profileId,
                                                                       const hgmlComputeInstancePlacement_t *placement,
                                                                       hgmlComputeInstance_t *computeInstance);

/**
 * Destroy compute instance.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param computeInstance                      The compute instance handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a computeInstance is invalid
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_IN_USE            If the compute instance is in use. This error would be returned if
 *                                             processes (e.g. HGGC application) are active on the compute instance.
 */
hgmlReturn_t hgmlComputeInstanceDestroy(hgmlComputeInstance_t computeInstance);

/**
 * Get compute instances for given profile ID.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param profileId                            The compute instance profile ID.
 *                                             See \ref hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param computeInstances                     Returns pre-exiting compute instances, the buffer must be large enough to
 *                                             accommodate the instances supported by the profile.
 *                                             See \ref hgmlGpuInstanceGetComputeInstanceProfileInfo
 * @param count                                The count of returned compute instances
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a gpuInstance, \a profileId, \a computeInstances or \a count
 *                                             are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a profileId isn't supported
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstances(hgmlGpuInstance_t gpuInstance, unsigned int profileId,
                                                        hgmlComputeInstance_t *computeInstances, unsigned int *count);

/**
 * Get compute instance for given instance ID.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 * Requires privileged user.
 *
 * @param gpuInstance                          The identifier of the target GPU instance
 * @param id                                   The compute instance ID
 * @param computeInstance                      Returns compute instance
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a device, \a ID or \a computeInstance are invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     If \a device doesn't have MIG mode enabled
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 *         - \ref HGML_ERROR_NOT_FOUND         If the compute instance is not found.
 */
hgmlReturn_t hgmlGpuInstanceGetComputeInstanceById(hgmlGpuInstance_t gpuInstance, unsigned int id,
                                                           hgmlComputeInstance_t *computeInstance);

/**
 * Get compute instance information.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param computeInstance                      The compute instance handle
 * @param info                                 Return compute instance information
 *
 * @return
 *         - \ref HGML_SUCCESS                 Upon success
 *         - \ref HGML_ERROR_UNINITIALIZED     If library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  If \a computeInstance or \a info are invalid
 *         - \ref HGML_ERROR_NO_PERMISSION     If user doesn't have permission to perform the operation
 */
hgmlReturn_t hgmlComputeInstanceGetInfo_v2(hgmlComputeInstance_t computeInstance, hgmlComputeInstanceInfo_t *info);

/**
 * Test if the given handle refers to a MIG device.
 *
 * A MIG device handle is an HGML abstraction which maps to a MIG compute instance.
 * These overloaded references can be used (with some restrictions) interchangeably
 * with a GPU device handle to execute queries at a per-compute instance granularity.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               HGML handle to test
 * @param isMigDevice                          True when handle refers to a MIG device
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a device status was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device handle or \a isMigDevice reference is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this check is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceIsMigDeviceHandle(hgmlDevice_t device, unsigned int *isMigDevice);

/**
 * Get GPU instance ID for the given MIG device handle.
 *
 * GPU instance IDs are unique per device and remain valid until the GPU instance is destroyed.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               Target MIG device handle
 * @param id                                   GPU instance ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 if instance ID was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a id reference is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetGpuInstanceId(hgmlDevice_t device, unsigned int *id);

/**
 * Get compute instance ID for the given MIG device handle.
 *
 * Compute instance IDs are unique per GPU instance and remain valid until the compute instance
 * is destroyed.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               Target MIG device handle
 * @param id                                   Compute instance ID
 *
 * @return
 *         - \ref HGML_SUCCESS                 if instance ID was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a id reference is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetComputeInstanceId(hgmlDevice_t device, unsigned int *id);

/**
 * Get the maximum number of MIG devices that can exist under a given parent HGML device.
 *
 * Returns zero if MIG is not supported or enabled.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               Target device handle
 * @param count                                Count of MIG devices
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a count was successfully retrieved
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device or \a count reference is invalid
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMaxMigDeviceCount(hgmlDevice_t device, unsigned int *count);

/**
 * Get MIG device handle for the given index under its parent HGML device.
 *
 * If the compute instance is destroyed either explicitly or by destroying,
 * resetting or unbinding the parent GPU instance or the GPU device itself
 * the MIG device handle would remain invalid and must be requested again
 * using this API. Handles may be reused and their properties can change in
 * the process.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param device                               Reference to the parent GPU device handle
 * @param index                                Index of the MIG device
 * @param migDevice                            Reference to the MIG device handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a migDevice handle was successfully created
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device, \a index or \a migDevice reference is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_NOT_FOUND         if no valid MIG device was found at \a index
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetMigDeviceHandleByIndex(hgmlDevice_t device, unsigned int index,
                                                         hgmlDevice_t *migDevice);

/**
 * Get parent device handle from a MIG device handle.
 *
 * For &tm; or newer fully supported devices.
 * Supported on Linux only.
 *
 * @param migDevice                            MIG device handle
 * @param device                               Device handle
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a device handle was successfully created
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a migDevice or \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 */
hgmlReturn_t hgmlDeviceGetDeviceHandleFromMigDeviceHandle(hgmlDevice_t migDevice, hgmlDevice_t *device);

/** @} */ // @defgroup hgmlMultiInstanceGPU


/***************************************************************************************************/
/** @defgroup GPM HGML GPM
 *  @{
 */
/***************************************************************************************************/
/** @defgroup hgmlGpmEnums GPM Enums
 *  @{
 */
/***************************************************************************************************/

/**
 * GPM Metric Identifiers
 */
typedef enum
{
    HGML_GPM_METRIC_GRAPHICS_UTIL               = 1,    //!< Percentage of time any compute/graphics app was active on the GPU. 0.0 - 100.0
    HGML_GPM_METRIC_SM_UTIL                     = 2,    //!< Percentage of SMs that were busy. 0.0 - 100.0
    HGML_GPM_METRIC_SM_OCCUPANCY                = 3,    //!< Percentage of warps that were active vs theoretical maximum. 0.0 - 100.0
    HGML_GPM_METRIC_INTEGER_UTIL                = 4,    //!< Percentage of time the GPU's SMs were doing integer operations. 0.0 - 100.0
    HGML_GPM_METRIC_ANY_TENSOR_UTIL             = 5,    //!< Percentage of time the GPU's SMs were doing ANY tensor operations. 0.0 - 100.0
    HGML_GPM_METRIC_DFMA_TENSOR_UTIL            = 6,    //!< Percentage of time the GPU's SMs were doing DFMA tensor operations. 0.0 - 100.0
    HGML_GPM_METRIC_HMMA_TENSOR_UTIL            = 7,    //!< Percentage of time the GPU's SMs were doing HMMA tensor operations. 0.0 - 100.0
    HGML_GPM_METRIC_IMMA_TENSOR_UTIL            = 9,    //!< Percentage of time the GPU's SMs were doing IMMA tensor operations. 0.0 - 100.0
    HGML_GPM_METRIC_DRAM_BW_UTIL                = 10,   //!< Percentage of DRAM bw used vs theoretical maximum. 0.0 - 100.0 */
    HGML_GPM_METRIC_FP64_UTIL                   = 11,   //!< Percentage of time the GPU's SMs were doing non-tensor FP64 math. 0.0 - 100.0
    HGML_GPM_METRIC_FP32_UTIL                   = 12,   //!< Percentage of time the GPU's SMs were doing non-tensor FP32 math. 0.0 - 100.0
    HGML_GPM_METRIC_FP16_UTIL                   = 13,   //!< Percentage of time the GPU's SMs were doing non-tensor FP16 math. 0.0 - 100.0
    HGML_GPM_METRIC_PCIE_TX_PER_SEC             = 20,   //!< PCIe traffic from this GPU in MiB/sec
    HGML_GPM_METRIC_PCIE_RX_PER_SEC             = 21,   //!< PCIe traffic to this GPU in MiB/sec
    HGML_GPM_METRIC_HGDEC_0_UTIL                = 30,   //!< Percent utilization of HGDEC 0. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_1_UTIL                = 31,   //!< Percent utilization of HGDEC 1. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_2_UTIL                = 32,   //!< Percent utilization of HGDEC 2. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_3_UTIL                = 33,   //!< Percent utilization of HGDEC 3. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_4_UTIL                = 34,   //!< Percent utilization of HGDEC 4. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_5_UTIL                = 35,   //!< Percent utilization of HGDEC 5. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_6_UTIL                = 36,   //!< Percent utilization of HGDEC 6. 0.0 - 100.0
    HGML_GPM_METRIC_HGDEC_7_UTIL                = 37,   //!< Percent utilization of HGDEC 7. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_0_UTIL                = 40,   //!< Percent utilization of HGJPG 0. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_1_UTIL                = 41,   //!< Percent utilization of HGJPG 1. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_2_UTIL                = 42,   //!< Percent utilization of HGJPG 2. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_3_UTIL                = 43,   //!< Percent utilization of HGJPG 3. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_4_UTIL                = 44,   //!< Percent utilization of HGJPG 4. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_5_UTIL                = 45,   //!< Percent utilization of HGJPG 5. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_6_UTIL                = 46,   //!< Percent utilization of HGJPG 6. 0.0 - 100.0
    HGML_GPM_METRIC_HGJPG_7_UTIL                = 47,   //!< Percent utilization of HGJPG 7. 0.0 - 100.0
    HGML_GPM_METRIC_HGOFA_0_UTIL                = 50,   //!< Percent utilization of HGOFA 0. 0.0 - 100.0
    HGML_GPM_METRIC_ICNLINK_TOTAL_RX_PER_SEC    = 60,   //!< ICNLINK read bandwidth for all links in MiB/sec
    HGML_GPM_METRIC_ICNLINK_TOTAL_TX_PER_SEC    = 61,   //!< ICNLINK write bandwidth for all links in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L0_RX_PER_SEC       = 62,   //!< ICNLINK read bandwidth for link 0 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L0_TX_PER_SEC       = 63,   //!< ICNLINK write bandwidth for link 0 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L1_RX_PER_SEC       = 64,   //!< ICNLINK read bandwidth for link 1 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L1_TX_PER_SEC       = 65,   //!< ICNLINK write bandwidth for link 1 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L2_RX_PER_SEC       = 66,   //!< ICNLINK read bandwidth for link 2 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L2_TX_PER_SEC       = 67,   //!< ICNLINK write bandwidth for link 2 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L3_RX_PER_SEC       = 68,   //!< ICNLINK read bandwidth for link 3 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L3_TX_PER_SEC       = 69,   //!< ICNLINK write bandwidth for link 3 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L4_RX_PER_SEC       = 70,   //!< ICNLINK read bandwidth for link 4 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L4_TX_PER_SEC       = 71,   //!< ICNLINK write bandwidth for link 4 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L5_RX_PER_SEC       = 72,   //!< ICNLINK read bandwidth for link 5 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L5_TX_PER_SEC       = 73,   //!< ICNLINK write bandwidth for link 5 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L6_RX_PER_SEC       = 74,   //!< ICNLINK read bandwidth for link 6 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L6_TX_PER_SEC       = 75,   //!< ICNLINK write bandwidth for link 6 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L7_RX_PER_SEC       = 76,   //!< ICNLINK read bandwidth for link 7 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L7_TX_PER_SEC       = 77,   //!< ICNLINK write bandwidth for link 7 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L8_RX_PER_SEC       = 78,   //!< ICNLINK read bandwidth for link 8 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L8_TX_PER_SEC       = 79,   //!< ICNLINK write bandwidth for link 8 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L9_RX_PER_SEC       = 80,   //!< ICNLINK read bandwidth for link 9 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L9_TX_PER_SEC       = 81,   //!< ICNLINK write bandwidth for link 9 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L10_RX_PER_SEC      = 82,   //!< ICNLINK read bandwidth for link 10 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L10_TX_PER_SEC      = 83,   //!< ICNLINK write bandwidth for link 10 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L11_RX_PER_SEC      = 84,   //!< ICNLINK read bandwidth for link 11 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L11_TX_PER_SEC      = 85,   //!< ICNLINK write bandwidth for link 11 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L12_RX_PER_SEC      = 86,   //!< ICNLINK read bandwidth for link 12 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L12_TX_PER_SEC      = 87,   //!< ICNLINK write bandwidth for link 12 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L13_RX_PER_SEC      = 88,   //!< ICNLINK read bandwidth for link 13 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L13_TX_PER_SEC      = 89,   //!< ICNLINK write bandwidth for link 13 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L14_RX_PER_SEC      = 90,   //!< ICNLINK read bandwidth for link 14 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L14_TX_PER_SEC      = 91,   //!< ICNLINK write bandwidth for link 14 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L15_RX_PER_SEC      = 92,   //!< ICNLINK read bandwidth for link 15 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L15_TX_PER_SEC      = 93,   //!< ICNLINK write bandwidth for link 15 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L16_RX_PER_SEC      = 94,   //!< ICNLINK read bandwidth for link 16 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L16_TX_PER_SEC      = 95,   //!< ICNLINK write bandwidth for link 16 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L17_RX_PER_SEC      = 96,   //!< ICNLINK read bandwidth for link 17 in MiB/sec
    HGML_GPM_METRIC_ICNLINK_L17_TX_PER_SEC      = 97,   //!< ICNLINK write bandwidth for link 17 in MiB/sec

    // New metrics added to HGML, supported by PPU.
    HGML_GPM_METRIC_KSD_HIT_RATE                = 200,   //!< Percentage of hit rate in KSD
    HGML_GPM_METRIC_KVD_HIT_RATE                = 201,   //!< Percentage of hit rate in KVD
    HGML_GPM_METRIC_L2_HIT_RATE                 = 202,   //!< Percentage of hit rate in L2
    HGML_GPM_METRIC_LLC_HIT_RATE                = 203,   //!< Percentage of hit rate in LLC
    HGML_GPM_METRIC_MAX                         = 250,   //!< Maximum value above +1. Note that changing this should also change HGML_GPM_METRICS_GET_VERSION due to struct size change
} hgmlGpmMetricId_t;

/** @} */ // @defgroup hgmlGpmEnums


/***************************************************************************************************/
/** @defgroup hgmlGpmStructs GPM Structs
 *  @{
 */
/***************************************************************************************************/

/**
 * Handle to an allocated GPM sample allocated with hgmlGpmSampleAlloc(). Free this with hgmlGpmSampleFree().
 */
typedef struct hgmlGpmSample_st* hgmlGpmSample_t;

/**
 * GPM metric information.
 */
typedef struct {
    char *shortName;
    char *longName;
    char *unit;
} hgmlGpmMetricMetricInfo_t;

typedef struct
{
    unsigned int               metricId;   //!<  IN: HGML_GPM_METRIC_? #define of which metric to retrieve
    hgmlReturn_t               hgmlReturn; //!<  OUT: Status of this metric. If this is nonzero, then value is not valid
    double                     value;      //!<  OUT: Value of this metric. Is only valid if hgmlReturn is 0 (HGML_SUCCESS)
    hgmlGpmMetricMetricInfo_t  metricInfo; //!< OUT: Metric name and unit. Those can be NULL if not defined
} hgmlGpmMetric_t;

/**
 * GPM buffer information.
 */
typedef struct
{
    unsigned int    version;                           //!< IN: Set to HGML_GPM_METRICS_GET_VERSION
    unsigned int    numMetrics;                        //!< IN: How many metrics to retrieve in metrics[]
    hgmlGpmSample_t sample1;                           //!< IN: Sample buffer
    hgmlGpmSample_t sample2;                           //!< IN: Sample buffer
    hgmlGpmMetric_t metrics[HGML_GPM_METRIC_MAX];      //!< IN/OUT: Array of metrics. Set metricId on call. See hgmlReturn and value on return
} hgmlGpmMetricsGet_t;

#define HGML_GPM_METRICS_GET_VERSION 1

/**
 * GPM device information.
 */
typedef struct
{
    unsigned int version;           //!< IN: Set to HGML_GPM_SUPPORT_VERSION
    unsigned int isSupportedDevice; //!< OUT: Indicates device support
} hgmlGpmSupport_t;

#define HGML_GPM_SUPPORT_VERSION 2

/** @} */ // @defgroup hgmlGPMStructs

/***************************************************************************************************/
/** @defgroup hgmlGpmFunctions GPM Functions
 *  @{
 */
/***************************************************************************************************/

/**
 * Calculate GPM metrics from two samples.
 *
 * For &tm; or newer fully supported devices.
 *
 * @param metricsGet             IN/OUT: populated \a hgmlGpmMetricsGet_t struct
 *
 * @return
 *         - \ref HGML_SUCCESS on success
 *         - Nonzero HGML_ERROR_? enum on error
 */
hgmlReturn_t hgmlGpmMetricsGet(hgmlGpmMetricsGet_t *metricsGet);


/**
 * Free an allocated sample buffer that was allocated with \ref hgmlGpmSampleAlloc()
 *
 * For &tm; or newer fully supported devices.
 *
 * @param gpmSample              Sample to free
 *
 * @return
 *         - \ref HGML_SUCCESS                on success
 *         - \ref HGML_ERROR_INVALID_ARGUMENT if an invalid pointer is provided
 */
hgmlReturn_t hgmlGpmSampleFree(hgmlGpmSample_t gpmSample);


/**
 * Allocate a sample buffer to be used with HGML GPM . You will need to allocate
 * at least two of these buffers to use with the HGML GPM feature
 *
 * For &tm; or newer fully supported devices.
 *
 * @param gpmSample             Where  the allocated sample will be stored
 *
 * @return
 *         - \ref HGML_SUCCESS                on success
 *         - \ref HGML_ERROR_INVALID_ARGUMENT if an invalid pointer is provided
 *         - \ref HGML_ERROR_MEMORY           if system memory is insufficient
 */
hgmlReturn_t hgmlGpmSampleAlloc(hgmlGpmSample_t *gpmSample);

/**
 * Read a sample of GPM metrics into the provided \a gpmSample buffer. After
 * two samples are gathered, you can call hgmlGpmMetricGet on those samples to
 * retrive metrics
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                Device to get samples for
 * @param gpmSample             Buffer to read samples into
 *
 * @return
 *         - \ref HGML_SUCCESS on success
 *         - Nonzero HGML_ERROR_? enum on error
 */
hgmlReturn_t hgmlGpmSampleGet(hgmlDevice_t device, hgmlGpmSample_t gpmSample);

/**
 * Read a sample of GPM metrics into the provided \a gpmSample buffer for a MIG GPU Instance.
 *
 * After two samples are gathered, you can call hgmlGpmMetricGet on those
 * samples to retrive metrics
 *
 * For &tm; or newer fully supported devices.
 *
 * @param device                Device to get samples for
 * @param gpuInstanceId         MIG GPU Instance ID
 * @param gpmSample             Buffer to read samples into
 *
 * @return
 *         - \ref HGML_SUCCESS on success
 *         - Nonzero HGML_ERROR_? enum on error
 */
hgmlReturn_t hgmlGpmMigSampleGet(hgmlDevice_t device, unsigned int gpuInstanceId, hgmlGpmSample_t gpmSample);

/**
 * Indicate whether the supplied device supports GPM
 *
 * @param device                HGML device to query for
 * @param gpmSupport            Structure to indicate GPM support \a hgmlGpmSupport_t. Indicates
 *                              GPM support per system for the supplied device
 *
 * @return
 *         - HGML_SUCCESS on success
 *         - Nonzero HGML_ERROR_? enum if there is an error in processing the query
 */
hgmlReturn_t hgmlGpmQueryDeviceSupport(hgmlDevice_t device, hgmlGpmSupport_t *gpmSupport);

/* GPM Stream State */
/**
 * Get GPM stream state.
 *
 * Supported on Linux, Windows TCC.
 *
 * @param device                               The identifier of the target device
 * @param state                                Returns GPM stream state
 *                                             HGML_FEATURE_DISABLED or HGML_FEATURE_ENABLED
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a current GPM stream state were successfully queried
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a  device is invalid or \a state is NULL
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlGpmQueryIfStreamingEnabled(hgmlDevice_t device, unsigned int *state);

/**
 * Set GPM stream state.
 *
 * Supported on Linux, Windows TCC.
 *
 * @param device                               The identifier of the target device
 * @param state                                GPM stream state,
 *                                             HGML_FEATURE_DISABLED or HGML_FEATURE_ENABLED
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a current GPM stream state is successfully set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 */
hgmlReturn_t hgmlGpmSetStreamingEnabled(hgmlDevice_t device, unsigned int state);

/** @} */ // @defgroup hgmlGpmFunctions
/** @} */ // @defgroup GPM

#define HGML_ICNLINK_POWER_STATE_HIGH_SPEED    0x0
#define HGML_ICNLINK_POWER_STATE_LOW           0x1

#define HGML_ICNLINK_LOW_POWER_THRESHOLD_MIN   0x1
#define HGML_ICNLINK_LOW_POWER_THRESHOLD_MAX   0x1FFF
#define HGML_ICNLINK_LOW_POWER_THRESHOLD_RESET 0xFFFFFFFF

/* Structure containing Low Power parameters */
typedef struct hgmlIcnLinkPowerThres_st
{
    unsigned int lowPwrThreshold;           //!< Low power threshold (in units of 100us)
} hgmlIcnLinkPowerThres_t;

/**
 * Set ICNLink Low Power Threshold for device.
 *
 * @param device                               The identifier of the target device
 * @param info                                 Reference to \a hgmlIcnLinkPowerThres_t struct
 *                                             input parameters
 *
 * @return
 *        - \ref HGML_SUCCESS                 if the \a Threshold is successfully set
 *        - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *        - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a Threshold is not within range
 *        - \ref HGML_ERROR_NOT_SUPPORTED     if this query is not supported by the device
 *
 **/
hgmlReturn_t hgmlDeviceSetIcnLinkDeviceLowPowerThreshold(hgmlDevice_t device, hgmlIcnLinkPowerThres_t *info);

/**
 * Set the global ICNLINK bandwith mode
 *
 * @param nvlinkBwMode             nvlink bandwidth mode
 * @return
 *         - \ref HGML_SUCCESS                on success
 *         - \ref HGML_ERROR_INVALID_ARGUMENT if an invalid argument is provided
 *         - \ref HGML_ERROR_IN_USE           if P2P object exists
 *         - \ref HGML_ERROR_NOT_SUPPORTED    if GPU is not or newer architecture.
 *         - \ref HGML_ERROR_NO_PERMISSION    if not root user
 */
hgmlReturn_t hgmlSystemSetIcnLinkBwMode(unsigned int nvlinkBwMode);

/**
 * Get the global ICNLINK bandwith mode
 *
 * @param nvlinkBwMode             reference of nvlink bandwidth mode
 * @return
 *         - \ref HGML_SUCCESS                on success
 *         - \ref HGML_ERROR_INVALID_ARGUMENT if an invalid pointer is provided
 *         - \ref HGML_ERROR_NOT_SUPPORTED    if GPU is not or newer architecture.
 *         - \ref HGML_ERROR_NO_PERMISSION    if not root user
 */
hgmlReturn_t hgmlSystemGetIcnLinkBwMode(unsigned int *nvlinkBwMode);

/**
 * Set new power limit of this device.
 *
 * For &tm; or newer fully supported devices.
 * Requires root/admin permissions.
 *
 * See \ref hgmlDeviceGetPowerManagementLimitConstraints to check the allowed ranges of values.
 *
 * See \ref hgmlPowerValue_v2_t for more information on the struct.
 *
 * \note Limit is not persistent across reboots or driver unloads.
 * Enable persistent mode to prevent driver from unloading when no application is using the device.
 *
 * This API replaces hgmlDeviceSetPowerManagementLimit. It can be used as a drop-in replacement for the older version.
 *
 * @param device                               The identifier of the target device
 * @param powerValue                           Power management limit in milliwatts to set
 *
 * @return
 *         - \ref HGML_SUCCESS                 if \a limit has been set
 *         - \ref HGML_ERROR_UNINITIALIZED     if the library has not been successfully initialized
 *         - \ref HGML_ERROR_INVALID_ARGUMENT  if \a device is invalid or \a powerValue is NULL or contains invalid values
 *         - \ref HGML_ERROR_NOT_SUPPORTED     if the device does not support this feature
 *         - \ref HGML_ERROR_GPU_IS_LOST       if the target GPU has fallen off the bus or is otherwise inaccessible
 *         - \ref HGML_ERROR_UNKNOWN           on any unexpected error
 *
 * @see HGML_FI_DEV_POWER_AVERAGE
 * @see HGML_FI_DEV_POWER_INSTANT
 * @see HGML_FI_DEV_POWER_MIN_LIMIT
 * @see HGML_FI_DEV_POWER_MAX_LIMIT
 * @see HGML_FI_DEV_POWER_CURRENT_LIMIT
 */
hgmlReturn_t hgmlDeviceSetPowerManagementLimit_v2(hgmlDevice_t device, hgmlPowerValue_v2_t *powerValue);

/**
 * HGML API versioning support
 */

hgmlReturn_t hgmlInit(void);
hgmlReturn_t hgmlDeviceGetCount(unsigned int *deviceCount);
hgmlReturn_t hgmlDeviceGetHandleByIndex(unsigned int index, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetHandleByPciBusId(const char *pciBusId, hgmlDevice_t *device);
hgmlReturn_t hgmlDeviceGetPciInfo(hgmlDevice_t device, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetIcnLinkRemotePciInfo(hgmlDevice_t device, unsigned int link, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetGridLicensableFeatures(hgmlDevice_t device, hgmlGridLicensableFeatures_t *pGridLicensableFeatures);
hgmlReturn_t hgmlDeviceRemoveGpu(hgmlPciInfo_t *pciInfo);
hgmlReturn_t hgmlEventSetWait(hgmlEventSet_t set, hgmlEventData_t * data, unsigned int timeoutms);
hgmlReturn_t hgmlDeviceGetAttributes(hgmlDevice_t device, hgmlDeviceAttributes_t *attributes);
hgmlReturn_t hgmlComputeInstanceGetInfo(hgmlComputeInstance_t computeInstance, hgmlComputeInstanceInfo_t *info);
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v1_t *infos);
hgmlReturn_t hgmlDeviceGetGpuInstancePossiblePlacements(hgmlDevice_t device, unsigned int profileId, hgmlGpuInstancePlacement_t *placements, unsigned int *count);
hgmlReturn_t hgmlVgpuInstanceGetLicenseInfo(hgmlVgpuInstance_t vgpuInstance, hgmlVgpuLicenseInfo_t *licenseInfo);
hgmlReturn_t hgmlDeviceGetPciInfo_v2(hgmlDevice_t device, hgmlPciInfo_t *pci);
hgmlReturn_t hgmlDeviceGetGridLicensableFeatures_v2(hgmlDevice_t device, hgmlGridLicensableFeatures_t *pGridLicensableFeatures);
hgmlReturn_t hgmlDeviceGetGridLicensableFeatures_v3(hgmlDevice_t device, hgmlGridLicensableFeatures_t *pGridLicensableFeatures);
hgmlReturn_t hgmlDeviceGetComputeRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlDeviceGetGraphicsRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlDeviceGetMPSComputeRunningProcesses_v2(hgmlDevice_t device, unsigned int *infoCount, hgmlProcessInfo_v2_t *infos);
hgmlReturn_t hgmlGetBlacklistDeviceCount(unsigned int *deviceCount);
hgmlReturn_t hgmlGetBlacklistDeviceInfoByIndex(unsigned int index, hgmlBlacklistDeviceInfo_t *info);

#ifdef __cplusplus
}
#endif

#endif
