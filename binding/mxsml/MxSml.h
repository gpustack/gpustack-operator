/**
 * Copyright © 2022 MetaX Integrated Circuits (Shanghai) Co., Ltd. All Rights Reserved.
 *
 * This software and associated documentation files (hereinafter collectively referred to as
 * "Software") is a proprietary commercial software developed by MetaX Integrated Circuits
 * (Shanghai) Co., Ltd. and/or its affiliates (hereinafter collectively referred to as “MetaX”).
 * The information presented in the Software belongs to MetaX. Without prior written permission
 * from MetaX, no entity or individual has the right to obtain a copy of the Software to deal in
 * the Software, including but not limited to use, copy, modify, merge, disclose, publish,
 * distribute, sublicense, and/or sell copies of the Software or substantial portions of the Software.
 *
 * The Software is provided for reference only, without warranty of any kind, either express or
 * implied, including but not limited to the warranty of merchantability, fitness for any purpose
 * and/or noninfringement. In no case shall MetaX be liable for any claim, damage or other liability
 * arising from, out of or in connection with the Software.
 *
 * If the Software need to be used in conjunction with any third-party software or open source
 * software, the rights to the third-party software or open source software still belong to the
 * copyright owners. For details, please refer to the respective notices or licenses. Please comply
 * with the provisions of the relevant notices or licenses. If the open source software licenses
 * additionally require the disposal of rights related to this Software, please contact MetaX
 * immediately and obtain MetaX 's written consent.
 *
 * MetaX reserves the right, at its sole discretion, to change, modify, add or remove portions of the
 * Software, at any time. MetaX reserves all the right for the final explanation.
 *
 */

#ifndef __MX_SML_H__
#define  __MX_SML_H__

#ifdef __cplusplus
extern "C" {
#endif

/**
 * @file MxSml.h
 *
 * @brief Main header file for mxsml.
 * All required function, structure, enum, etc. definitions should be defined in this file.
 *
 */

/*
 * For Windows DLL export
 */
#ifdef DECLDIR
    #undef DECLDIR
#endif

#if defined _WIN32
    #if !defined MXSML_STATIC_IMPORT
        #if defined MXSML_LIB_EXPORT
            #define DECLDIR __declspec(dllexport)
        #else
            #define DECLDIR __declspec(dllimport)
        #endif
    #else
        #define DECLDIR
    #endif
#else
    #define DECLDIR
#endif

/**
 * @brief Error codes returned by mxsml functions
 */
typedef enum MxSmlReturn
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Success = 0,                      //!< Operation was successful
    MXSML_Failure,                          //!< Any unexpected failure
    MXSML_NoDevice,                         //!< The device was not found
    MXSML_OperationNotSupport,              //!< The operation is not support in the specified device
    MXSML_SysfsError,                       //!< Sysfs file operation failure, please get detailed error from errno
    MXSML_SysfsWriteError,                  //!< Sysfs file write failure, please get detailed error from errno
    MXSML_InvalidDeviceId,                  //!< The provided device id is out of range
    MXSML_InvalidDieId,                     //!< The provided die id is out of range
    MXSML_PermissionDenied,                 //!< The operations require root access to run
    MXSML_InvalidInput,                     //!< A input argument is invalid
    MXSML_InsufficientSize,                 //!< An input argument is not large enough
    MXSML_Reserved3,                        //!< Reserved3
    MXSML_IOControlFailure,                 //!< Ioctl failure, please get detailed error from errno
    MXSML_MmapFailure,                      //!< Mmap failure, please get detailed error from errno
    MXSML_UnMmapFailure,                    //!< Unmmap failure, please get detailed error from errno
    MXSML_InvalidInputForMmap,              //!< Invalid input for mmap
    MXSML_Reserved1,                        //!< Reserved1
    MXSML_Reserved2,                        //!< Reserved2
    MXSML_TargetVfNotFound,                 //!< Target Vf not found
    MXSML_InvalidFrequency,                 //!< Specified frequency is not valid
    MXSML_FlrNotReady,                      //!< FLR is processing
    MXSML_OpenDeviceFileFailure,            //!< Open device file failed
    MXSML_CloseDeviceFileFailure,           //!< Close device file failed
    MXSML_BusyDevice,                       //!< Device is busy
    MXSML_MmioNotEnough,                    //!< Mmio address space is not enough for vf-vbios
    MXSML_GetPciBridgeFailure,              //!< Get pci bridge failed
    MXSML_LoadDllFailure,                   //!< Load dynamic library failed
} mxSmlReturn_t;

/**
 * @brief Available DPM IP types
 */
typedef enum MxSmlDpmIp
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Dpm_Dla = 0,                      //!< Valid for N-class device
    MXSML_Dpm_Xcore,                        //!< Valid for C-class device
    MXSML_Dpm_Mc,
    MXSML_Dpm_Soc,
    MXSML_Dpm_Dnoc,
    MXSML_Dpm_Vpue,
    MXSML_Dpm_Vpud,
    MXSML_Dpm_Hbm,
    MXSML_Dpm_G2d,                          //!< Valid for N-class device
    MXSML_Dpm_HbmPower,
    MXSML_Dpm_Ccx,
    MXSML_Dpm_Ip_Group,                     //!< Valid for N-class device, group of soc, dnoc, vpue, vpud, ccx, g2d
    MXSML_Dpm_Dma,
    MXSML_Dpm_Csc,
    MXSML_Dpm_Eth,
    MXSML_Dpm_Didt,
    MXSML_Dpm_Reserved
} mxSmlDpmIp_t;

/**
 * @brief Available pci power state
 */
typedef enum MxSmlPciPowerState
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Power_Pci_State_D0 = 0,
    MXSML_Power_Pci_State_D3hot,
    MXSML_Power_Pci_State_D3cold
} mxSmlPciPowerState_t;

/**
 * @brief Available RAS IP types
 */
typedef enum MxSmlRasIp
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Ras_mc = 0,
    MXSML_Ras_pcie,
    MXSML_Ras_fuse,
    MXSML_Ras_g2d,
    MXSML_Ras_int,
    MXSML_Ras_hag,
    MXSML_Ras_metalk,
    MXSML_Ras_smp0,
    MXSML_Ras_smp1,
    MXSML_Ras_ccx0,
    MXSML_Ras_ccx1,
    MXSML_Ras_ccx2,
    MXSML_Ras_ccx3,
    MXSML_Ras_dla0,
    MXSML_Ras_dla1,
    MXSML_Ras_vpue0,
    MXSML_Ras_vpue1,
    MXSML_Ras_vpud0,
    MXSML_Ras_vpud1,
    MXSML_Ras_vpud2,
    MXSML_Ras_vpud3,
    MXSML_Ras_vpud4,
    MXSML_Ras_vpud5,
    MXSML_Ras_vpud6,
    MXSML_Ras_vpud7,
    MXSML_Ras_dma0,
    MXSML_Ras_dma1,
    MXSML_Ras_dma2,
    MXSML_Ras_dma3,
    MXSML_Ras_dma4,
    MXSML_Ras_mcctl0,
    MXSML_Ras_mcctl1,
    MXSML_Ras_mcctl2,
    MXSML_Ras_mcctl3,
    MXSML_Ras_dhub1,
    MXSML_Ras_dhub2,
    MXSML_Ras_dhub3,
    MXSML_Ras_dhub4,
    MXSML_Ras_dhub5,
    MXSML_Ras_dhub6,
    MXSML_Ras_dhub7,
    MXSML_Ras_ath,
    MXSML_Ras_atul20,
    MXSML_Ras_atul21,
    MXSML_Ras_xsc,
    MXSML_Ras_ce,
    MXSML_Ras_eth,
    MXSML_Ras_ethsc
} mxSmlRasIp_t;

/**
 * @brief Available RAS error types
 */
typedef enum MxSmlRasErrorType
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Ras_Error_ue,                     //!< Uncorrected error
    MXSML_Ras_Error_ce,                     //!< Corrected error
    MXSML_Ras_Error_fatal,                  //!< Currently not support
    MXSML_Ras_Error_re                      //!< Currently not support
} mxSmlRasErrorType_t;

/**
 * @brief Available device brands
 */
typedef enum MxSmlDeviceBrand
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Brand_Unknown = 0,
    MXSML_Brand_N,
    MXSML_Brand_C,
    MXSML_Brand_G
} mxSmlDeviceBrand_t;

/**
 * @brief Available device virtualization mode
 */
typedef enum MxSmlDeviceVirtualizationMode
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Virtualization_Mode_None = 0,    //!< Represents bare metal
    MXSML_Virtualization_Mode_Pf,          //!< Physical function after virtualization
    MXSML_Virtualization_Mode_Vf           //!< Virtualized device
} mxSmlDeviceVirtualizationMode_t;

/**
* @brief Available firmware ip name
*/
typedef enum MxSmlFwIpName
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Fw_IpName_SMP0,
    MXSML_Fw_IpName_SMP1,
    MXSML_Fw_IpName_CCX0,
    MXSML_Fw_IpName_CCX1,
    MXSML_Fw_IpName_CCX2,
    MXSML_Fw_IpName_CCX3,
    MXSML_Fw_IpName_XCORE,
    MXSML_Fw_IpName_ETH,
    MXSML_Fw_IpName_DISP,
    MXSML_Fw_IpName_ALL
} mxSmlFwIpName_t;

/**
* @brief Available log level
*/
typedef enum MxSmlLoglevel
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Loglevel_NONE,
    MXSML_Loglevel_FATAL,
    MXSML_Loglevel_ERROR,
    MXSML_Loglevel_WARN,
    MXSML_Loglevel_INFO,
    MXSML_Loglevel_DEBUG,
    MXSML_Loglevel_VERBOSE,
    MXSML_Loglevel_UNKNOWN
} mxSmlLoglevel_t;

/**
 * @brief Available PCI generations
 *
 */
typedef enum MxSmlPciGen
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_PciGen_1 = 1,
    MXSML_PciGen_2 = 2,
    MXSML_PciGen_3 = 3,
    MXSML_PciGen_4 = 4,
    MXSML_PciGen_5 = 5,
} mxSmlPciGen_t;

/**
 * @brief Available topology types
 *
 */
typedef enum MxSmlGpuTopologyLevel
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_TOPOLOGY_INTERNAL,
    MXSML_TOPOLOGY_SINGLE,           //!< all devices that only need traverse a single PCIe switch
    MXSML_TOPOLOGY_MULTIPLE,         //!< all devices that need not traverse a host bridge
    MXSML_TOPOLOGY_HOSTBRIDGE,       //!< all devices that are connected to the same host bridge
    MXSML_TOPOLOGY_NODE,             //!< all devices that are connected to the same NUMA node but possibly multiple host bridges
    MXSML_TOPOLOGY_METAXLINK,        //!< all devices that are connected to the same MetaXLink
    MXSML_TOPOLOGY_SYSTEM,           //!< all devices in the system
    MXSML_TOPOLOGY_ETH,              //!< all devices that are connected to the same Eth
    MXSML_TOPOLOGY_UNDEFINED
} mxSmlGpuTopologyLevel_t;

/**
 * @brief Available versions
 *
 */
typedef enum MxSmlVersionUnit
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Version_Bios,
    MXSML_Version_Driver,
    MXSML_Version_Smp0,
    MXSML_Version_Smp1,
    MXSML_Version_Ccx0,
    MXSML_Version_Ccx1,
    MXSML_Version_Ccx2,
    MXSML_Version_Ccx3                //!< only valid for N-class device
} mxSmlVersionUnit_t;

/**
 * @brief Available IPs to get usages
 *
 */
typedef enum MxSmlUsageIp
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Usage_Dla,                  //!< only valid for N-class device
    MXSML_Usage_Vpue,
    MXSML_Usage_Vpud,
    MXSML_Usage_G2d,                  //!< only valid for N-class device
    MXSML_Usage_Xcore                 //!< only valid for C-class device
} mxSmlUsageIp_t;

/**
 * @deprecated Keeping definition for backward compatibility
 */
typedef enum MxSmlDeviceType
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Device_Unknown = 0
} mxSmlDeviceType_t;

#define DEVICE_BDF_ID_SIZE 32               //!< Guaranteed maximum possible size for BDF ID
#define DEVICE_UUID_SIZE 96                 //!< Guaranteed maximum possible size for UUID
#define DEVICE_NAME_SIZE 32                 //!< Guaranteed maximum possible size for Device name

/**
 * @brief This structure holds attributes for a device
 */
typedef struct MxSmlDeviceInfo {
    unsigned int deviceId;
    mxSmlDeviceType_t type;          //!< Deprecated. Do not use.
    char bdfId[DEVICE_BDF_ID_SIZE];
    unsigned int gpuId;
    unsigned int nodeId;
    char uuid[DEVICE_UUID_SIZE];
    mxSmlDeviceBrand_t brand;
    mxSmlDeviceVirtualizationMode_t mode;
    char deviceName[DEVICE_NAME_SIZE];
} mxSmlDeviceInfo_t;

/**
 * @brief This structure holds a ras register information
 */
typedef struct MxSmlRasRegister {
    mxSmlRasIp_t rasIp;
    unsigned int registerIndex;
    int registerData;
} mxSmlRasRegister_t;

#define RAS_STATUS_REG_NUM 128              //!< Number of RAS status register

/**
 * @brief This structure holds ras status register data
 */
typedef struct MxSmlRasStatusData {
    mxSmlRasRegister_t rasStatusRegister[RAS_STATUS_REG_NUM];
    int showRasStatusSize;
} mxSmlRasStatusData_t;

/**
 * @brief This structure holds ras error counter for specific register
 */
typedef struct MxSmlRasErrorRegister {
    mxSmlRasIp_t rasIp;
    unsigned int registerIndex;
    int rasErrorUe;
    int rasErrorCe;
} mxSmlRasErrorRegister_t;

#define RAS_ERROR_REG_NUM 128               //!< Number of RAS error counter register

/**
 * @brief This structure holds ras error counter register data
 */
typedef struct MxSmlRasErrorData {
    mxSmlRasErrorRegister_t rasErrorRegister[RAS_ERROR_REG_NUM];
    int showRasErrorSize;
} mxSmlRasErrorData_t;

#define EEPROM_INFO_SIZE 1024               //!< Guaranteed maximum possible size for Eeprom info

/**
 * @brief This structure holds the eeprom information
 *
 * content format: <tag1>:<value1>;...;<tagN>:<valueN>;
 *
 */
typedef struct MxSmlEepromInfo {
    char content[EEPROM_INFO_SIZE];
} mxSmlEepromInfo_t;

/**
 * @brief This structure holds the device  memory information in different domains, unit: KB
 */
typedef struct MxSmlMemoryInfo {
    long visVramTotal;
    long visVramUse;
    long vramTotal;
    long vramUse;
    long xttTotal;
    long xttUse;
} mxSmlMemoryInfo_t;

/**
 * @brief This structure holds Pcie speed and width information, unit: GT/s, lanes number
 */
typedef struct MxSmlPcieInfo {
    float speed;
    unsigned int width;
} mxSmlPcieInfo_t;

/**
 * @brief This structure holds hbm bandwidth(MBytes/s) information
 */
typedef struct MxSmlHbmBandwidth {
    int hbmBandwidthReqTotal;
    int hbmBandwidthRespTotal;
} mxSmlHbmBandWidth_t;

/**
 * @brief This structure holds dma engine bandwidth(MBytes/s) information
 */
typedef struct MxSmlDmaEngineBandwidth {
    int readReqBandwidth;
    int readRespBandwidth;
    int writeReqBandwidth;
    int writeRespBandwidth;
} mxSmlDmaEngineBandwidth_t;

#define VIRTUAL_DEVICE_SIZE 128             //!< Guaranteed maximum possible size for virtual device ids

/**
 * @brief This structure holds virtual device ids
 *
 * number : actual number of virtual device ids in the array
 */
typedef struct MxSmlVirtualDeviceIds {
    int number;
    int deviceId[VIRTUAL_DEVICE_SIZE];
} mxSmlVirtualDeviceIds_t;

/**
 * @brief This structure holds limited device ids
 */
typedef struct MxSmlVirtualDeviceIds mxSmlLimitedDeviceIds_t;

/**
 * @brief This structure holds the GPU information currently being used by a process (v1)
 *
 * bdfId : <domainId>:<busId>:<deviceId>.<functionId>, e.g. 0000:00:01.0
 * gpuId : GPU's sequence label
 * gpuMemoryUsage : Allocated device memory (in bytes)
 */
typedef struct MxSmlProcessGpuInfo {
    char bdfId[DEVICE_BDF_ID_SIZE];
    unsigned int gpuId;
    unsigned long gpuMemoryUsage;
} mxSmlProcessGpuInfo_t;

#define PROCESS_NAME_SIZE 64                //!< Maximum length showed process name
#define MAX_GPU_NUM_USED_BY_PROCESS 64      //!< Maximum stored GPU information used by a process

/**
 * @brief This structure holds the process information currently using GPU (v1)
 *
 * gpuNumber : the number of GPU currently used by the process
 */
typedef struct MxSmlProcessInfo {
    unsigned int processId;
    char processName[PROCESS_NAME_SIZE];
    unsigned int gpuNumber;
    mxSmlProcessGpuInfo_t processGpuInfo[MAX_GPU_NUM_USED_BY_PROCESS];
} mxSmlProcessInfo_t;

/**
 * @brief This structure holds the GPU information currently being used by a process (v2)
 *
 * bdfId : <domainId>:<busId>:<deviceId>.<functionId>, e.g. 0000:00:01.0
 * gpuId : GPU's sequence label
 * gpuMemoryUsage : Allocated device memory (in bytes)
 * dieId : Mcm GPU Die's sequence label
 */
typedef struct MxSmlProcessGpuInfo_v2 {
    char bdfId[DEVICE_BDF_ID_SIZE];
    unsigned int gpuId;
    unsigned long gpuMemoryUsage;
    unsigned int dieId;                      //!< Valid for MCM device
} mxSmlProcessGpuInfo_v2_t;

/**
 * @brief This structure holds the process information currently using GPU (v2)
 *
 * Version 2 adds dieId in processGpuInfo for MCM device
 *
 * gpuNumber : the number of GPU currently used by the process
 */
typedef struct MxSmlProcessInfo_v2 {
    unsigned int processId;
    char processName[PROCESS_NAME_SIZE];
    unsigned int gpuNumber;
    mxSmlProcessGpuInfo_v2_t processGpuInfo[MAX_GPU_NUM_USED_BY_PROCESS];
} mxSmlProcessInfo_v2_t;

/**
 * @brief This structure holds the GPU information currently being used by a process (v3)
 *
 * bdfId : <domainId>:<busId>:<deviceId>.<functionId>, e.g. 0000:00:01.0
 * gpuId : GPU's sequence label
 * gpuMemoryUsage : Allocated device memory (in bytes)
 * dieId : Mcm GPU Die's sequence label
 * sgpuId : GPU's slice gpu sequence label. It is -1 when device sgpu mode is disabled
 */
typedef struct MxSmlProcessGpuInfo_v3 {
    char bdfId[DEVICE_BDF_ID_SIZE];
    unsigned int gpuId;
    unsigned long gpuMemoryUsage;
    unsigned int dieId;                      //!< Valid for MCM device
    int sgpuId;                              //!< Valid when device sgpu mode is enabled
} mxSmlProcessGpuInfo_v3_t;

/**
 * @brief This structure holds the process information currently using GPU (v3)
 *
 * Version 3 adds sgpuId in processGpuInfo for sgpu
 *
 * gpuNumber : the number of GPU currently used by the process
 */
typedef struct MxSmlProcessInfo_v3 {
    unsigned int processId;
    char processName[PROCESS_NAME_SIZE];
    unsigned int gpuNumber;
    mxSmlProcessGpuInfo_v3_t processGpuInfo[MAX_GPU_NUM_USED_BY_PROCESS];
} mxSmlProcessInfo_v3_t;

/**
 * @brief Initialize mxsml
 *
 * @details When called, the devices are discovered and initialized.
 * This function shall be called once before invoking any other methods in this library.
 * Repeated initialization will not refresh resources.
 *
 * @retval MXSML_Success           Initialization was successful
 * @retval MXSML_NoDevice          No devices were discovered
 */
mxSmlReturn_t DECLDIR mxSmlInit();

#define MXSML_INIT_FLAG_NORMAL 0
#define MXSML_INIT_FLAG_REINIT 1
/**
 * @brief Initialize mxsml with flag
 *
 * @details When called, the devices are discovered and initialized.
 * If flags = MXSML_INIT_FLAG_NORMAL, the behavior is the same as mxsmlInit.
 * If flags = MXSML_INIT_FLAG_REINIT, repeated initialization will reinit resources.
 *
 * @retval MXSML_Success           Initialization was successful
 * @retval MXSML_NoDevice          No devices were discovered
 */
mxSmlReturn_t DECLDIR mxSmlInitWithFlags(unsigned int flags);

/**
 * @brief Get number of available devices in the system
 *
 * @details The available devices including physical devices and VF devices after virtualization.
 *
 * @return the number of available devices
 */
unsigned int mxSmlGetDeviceCount();

/**
 * @brief Get number of PF devices in the system
 *
 * @details After device is virtualized, the device is divided into PF and VF.
 *          User process can only run on the VF devices.
 *          PF devices are responsible for hardware management.
 *
 * @return the number of PF devices
 */
unsigned int mxSmlGetPfDeviceCount();

/**
 * @brief Get the virtual devices of one phyical device
 *
 * @param[in]  phyDeviceId : the physical device index
 * @param[out] deviceIds : return the virtual device ids
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 */
mxSmlReturn_t DECLDIR mxSmlGetVirtualDevicesByPhysicalId(unsigned int phyDeviceId, mxSmlVirtualDeviceIds_t* deviceIds);

/**
 * @brief Get the device attributes
 *
 * @param[in]  deviceId : the device index
 * @param[out] deviceInfo : return the device attributes
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       deviceInfo is null
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceInfo(unsigned int deviceId, mxSmlDeviceInfo_t* deviceInfo);

/**
 * @brief Get all the limited devices
 *
 * @param[out] deviceIds : return the limited device ids
 *
 * SMI is not available for limited devices.
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidInput       deviceIds is null
 */
mxSmlReturn_t DECLDIR mxSmlGetAllLimitedDevices(mxSmlLimitedDeviceIds_t* deviceIds);

/**
 * @brief Get the limited device attributes
 *
 * @param[in]  deviceId : the device index
 * @param[out] deviceInfo : return the device attributes
 *
 * The attributes deviceId, type and bdfId are available for the limited devices.
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       deviceInfo is null
 */
mxSmlReturn_t DECLDIR mxSmlGetLimitedDeviceInfo(unsigned int deviceId, mxSmlDeviceInfo_t* deviceInfo);

/**
 * @brief Available pmbus unit
 */
typedef enum MxSmlPmbusUnit
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Pmbus_Soc = 0,
    MXSML_Pmbus_Core,
    MXSML_Pmbus_Hbm,
    MXSML_Pmbus_Pcie,
    MXSML_Pmbus_Hbm2,
    MXSML_Pmbus_Pcie2,
} mxSmlPmbusUnit_t;

/**
 * @brief This structure describes power info for specified pmbus unit
 *
 * voltage unit: mV, current unit: mA, power unit: mW
 */
typedef struct MxSmlPmbusInfo {
    unsigned int voltage;
    unsigned int current;
    unsigned int power;
} mxSmlPmbusInfo_t;

/**
 * @brief Get pmbus info for specified pmbus unit
 *
 * @param[in]  deviceId : the device index
 * @param[in]  pmbusUnit : the pmbus unit
 * @param[out] pmbusInfo : return the pmbus info
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           pmbusInfo is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPmbusInfo(unsigned int deviceId, mxSmlPmbusUnit_t pmbusUnit, mxSmlPmbusInfo_t* pmbusInfo);

/**
 * @brief Available temperature sensors
 */
typedef enum MxSmlTemperatureSensors
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Temperature_Hotspot = 0,                 //!< chip max temperature
    MXSML_Temperature_HotLimit,                    //!< chip temperature limit
    MXSML_Temperature_Soc,                         //!< Power DrMOS soc
    MXSML_Temperature_Core,                        //!< Power DrMOS core
    MXSML_Temperature_Ccx_Dnoc,                    //!< Deprecated. Do not use
    MXSML_Temperature_Csc_Fuse,                    //!< Deprecated. Do not use
    MXSML_Temperature_Ccx_Dla_Vpue1_Ath,           //!< Deprecated. Do not use
    MXSML_Temperature_Vpue1,                       //!< Deprecated. Do not use
    MXSML_Temperature_Vpue0,                       //!< Deprecated. Do not use
    MXSML_Temperature_Atul2,                       //!< Deprecated. Do not use
    MXSML_Temperature_Dla1,                        //!< Deprecated. Do not use
    MXSML_Temperature_Dla0,                        //!< Deprecated. Do not use
    MXSML_Temperature_Emc0,                        //!< air inlet, Valid for C-class device
    MXSML_Temperature_Emc1,                        //!< tdiode, Valid for C-class device
    MXSML_Temperature_Sgm,                         //!< air outlet, Valid for C-class device
    MXSML_Temperature_Hbm                          //!< chip, Valid for specific device
} mxSmlTemperatureSensors_t;

/**
 * @brief Get the current temperature readings for the device, in degress C
 *
 * @details The value is enlarged by 100 times.
 *
 * @param[in]  deviceId : the device index
 * @param[in]  temperatureType : flag that indicates which temperature reading to get
 * @param[out] temperature : reference in which to return the temperature reading
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           temperature is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetTemperatureInfo(unsigned int deviceId, mxSmlTemperatureSensors_t temperatureType, int* temperature);

/**
 * @brief This structure holds one way of board electricInfo
 *
 * voltage unit: mV, current unit: mA, power unit: mW
 */
typedef struct MxSmlBoardWayElectricInfo {
    unsigned int voltage;
    unsigned int current;
    unsigned int power;
} mxSmlBoardWayElectricInfo_t;

/**
 * @brief Get board electric information
 *
 * @param[in]  deviceId : the device index
 * @param[in,out]  infoSize : the size of the boardInfo array that is safe to access
 * @param[out] boardInfo : return the board voltage, current, power information
 *
 * @details N-class devices supply 2 way power, way 0 is 12V and way 1 is 3.3V;
 *          C-class devices supply 3 way 12V power.
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       boardInfo or infoSize is null
 * @retval MXSML_InsufficientSize   infoSize is not large enough but infoSize return the minimal boardInfo array size
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetBoardPowerInfo(unsigned int deviceId, unsigned int* infoSize, mxSmlBoardWayElectricInfo_t* boardInfo);

/**
 * @deprecated This structure holds DPM performance level for C-class device
 */
typedef struct MxSmlMxcDpmPerfLevel {
    unsigned int xcore;
    unsigned int mc;
    unsigned int soc;
    unsigned int dnoc;
    unsigned int vpue;
    unsigned int vpud;
    unsigned int ccx;
} mxSmlMxcDpmPerfLevel_t;

/**
 * @brief Get all dpm IP's max performance level
 *
 * @deprecated Since mxSmlMxcDpmPerfLevel_t is incomplate for DPM performance level
 * this function is deprecated in favor of mxSmlGetDpmIpMaxPerfLevel
 *
 * @param[in] deviceId : the device index
 * @param[out] dpmMaxLevel : return the max dpm performance level
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           dpmMaxLevel is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDpmMaxPerfLevel(unsigned int deviceId, mxSmlMxcDpmPerfLevel_t* dpmMaxLevel);

/**
 * @brief Get dpm IP's max performance level
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : the target IP
 * @param[out] dpmMaxLevel : return the max dpm performance level
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           dpmIp is invalid or dpmMaxLevel is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDpmIpMaxPerfLevel(unsigned int deviceId, mxSmlDpmIp_t dpmIp, unsigned int* dpmMaxLevel);

/**
 * @brief Set max performance level for IP of a device
 *
 * @details This function will set the IP's max performance level to the provided value
 *          Please refer to mxSmlDpmIp_t for the supported IPs
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : indicate which DPM IP to be set
 * @param[in] maxPerfLevel : the max performance level value
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       dpmIp or maxPerfLevel is invalid
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 * @retval MXSML_Failure            unknown device type
 */
mxSmlReturn_t DECLDIR mxSmlSetDpmIpMaxPerfLevel(unsigned int deviceId, mxSmlDpmIp_t dpmIp, unsigned int maxPerfLevel);

/**
 * @brief Get device memory usage information
 *
 * @param[in]  deviceId : the device index
 * @param[out] memoryInfo : return memory information (KB)
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetMemoryInfo(unsigned int deviceId, mxSmlMemoryInfo_t* memoryInfo);

/**
 * @brief Get Eeprom information
 *
 * @param[in]  deviceId : the device index
 * @param[out] eepromInfo : return Eeprom information
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetEepromInfo(unsigned int deviceId, mxSmlEepromInfo_t* eepromInfo);

#define BOARD_SERIAL_SIZE 32               //!< Guaranteed maximum possible size for board serial
/**
 * @brief Get board serial
 *
 * @param[in]  deviceId : the device index
 * @param[out] boardSerial : board serial
 * @param[in,out] size : length of board serial
 *
 * @details the guaranteed maximum possible size is 32
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       boardSerial or size is null
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal required size
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetBoardSerial(unsigned int deviceId, char* boardSerial, unsigned int* size);

/**
 * @brief Get pptable version
 *
 * @param[in]  deviceId : the device index
 * @param[in]  size : the size of version that is safe to access
 * @param[out] version : return device version
 *
 * @details the recommended size is 5, the fomat of version is like 0001
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       version is null or size is 0
 * @retval MXSML_InsufficientSize   size is not large enough
 * @retval MXSML_SysfsError         read sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPptableVersion(unsigned int deviceId, unsigned int size, char* version);

/**
 * @brief Get data from RAS error counter
 *
 * @param[in]   deviceId : the device index
 * @param[out]  rasErrorData : RAS error register data
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open/read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetRasErrorData(unsigned int deviceId, mxSmlRasErrorData_t* rasErrorData);

/**
 * @brief Get data from RAS status register
 *
 * @param[in]   deviceId : the device index
 * @param[out]  rasStatusData : RAS status register data
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open/read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetRasStatusData(unsigned int deviceId, mxSmlRasStatusData_t* rasStatusData);

/**
 * @brief Get Pcie speed and width
 *
 * @param[in]  deviceId : the device index
 * @param[out] pcieInfo : return Pcie information
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPcieInfo(unsigned int deviceId, mxSmlPcieInfo_t* pcieInfo);

/**
 * @brief Get Pcie max link speed and width
 *
 * @param[in]  deviceId : the device index
 * @param[out] pcieMaxLinkInfo : return Pcie max link information
 *
 * @details speed&width return 0 in a virtual environment
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPcieMaxLinkInfo(unsigned int deviceId, mxSmlPcieInfo_t* pcieMaxLinkInfo);

/**
 * @brief Get power state for target IP
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : the target IP
 * @param[out] powerState : power state for the target IP
 * @param[in,out] size : size of power state array that is safe to access
 *
 * @details the power state array size set as 8 is large enough
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 * @retval MXSML_InvalidInput       powerState or size is null or dpmIp is invalid in target device
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal power state array size
 */
mxSmlReturn_t DECLDIR mxSmlGetPowerStateInfo(unsigned int deviceId, mxSmlDpmIp_t dpmIp, int* powerState, unsigned int* size);

/**
 * @brief Get pci power state
 *
 * @param[in]  deviceId : the device index
 * @param[out]  pciPowerState : pci power state
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_Failure            the pci power state is invalid
 */
mxSmlReturn_t DECLDIR mxSmlGetPciPowerState(unsigned int deviceId, mxSmlPciPowerState_t* pciPowerState);

/**
 * @deprecated This structure holds VPUE and VPUD codec status
 */
typedef struct MxSmlVpuStatus {
    int vpue0;
    int vpue1;
    int vpud0;
} mxSmlVpuStatus_t;

/**
 * @brief Get current VPUE and VPUD codec status
 *
 * @deprecated Since mxSmlVpuStatus_t is not an accurate class of codec status
 * this function is deprecated in favor of MxSmlGetCodecStatus
 *
 * @param[in]  deviceId : the device index
 * @param[out] vpuStatus : return VPU status
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetVpuStatus(unsigned int deviceId, mxSmlVpuStatus_t* vpuStatus);

/**
 * @brief This structure holds encoder and decoder codec status (number of streams)
 */
typedef struct MxSmlCodecStatus {
    int encoder;
    int decoder;
} mxSmlCodecStatus_t;

/**
 * @brief Get current encoder and decoder codec status
 *
 * @param[in]  deviceId : the device index
 * @param[out] codecStatus : return codec status
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetCodecStatus(unsigned int deviceId, mxSmlCodecStatus_t* codecStatus);

/**
 * @brief Available clock IP types
 */
typedef enum MxSmlClockIp
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Clock_Csc = 0,
    MXSML_Clock_Dla,                        //!< Valid for N-class device
    MXSML_Clock_Mc,                         //!< Valid for N-class device
    MXSML_Clock_Mc0,                        //!< Valid for C-class device
    MXSML_Clock_Mc1,                        //!< Valid for C-class device
    MXSML_Clock_Vpue,
    MXSML_Clock_Vpud,
    MXSML_Clock_Soc,
    MXSML_Clock_Dnoc,
    MXSML_Clock_G2D,                        //!< Valid for N-class device
    MXSML_Clock_Ccx,
    MXSML_Clock_Xcore,                      //!< Valid for C-class device
} mxSmlClockIp_t;

/**
 * @brief Get clock frequencies specified by the clock type
 *
 * @param[in]  deviceId : the device index
 * @param[in]  clockIp : identify which clock domain to query
 * @param[in,out]  clocksSize : the size of the clocksMhz array that is safe to access
 * @param[out] clocksMhz : return the clock frequency information in MHz
 *
 * @details the maximum clock size is 9
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InvalidInput           clocksSize or clocksMhz is nullptr or clockIp is not valid for target device
 * @retval MXSML_InsufficientSize       clocksSize is not large enough but infoSize return the minimal array size
 */
mxSmlReturn_t DECLDIR mxSmlGetClocks(
        unsigned int deviceId, mxSmlClockIp_t clockIp, unsigned int* clocksSize, unsigned int* clocksMhz);

/**
 * @brief Get hbm bandwidth
 *
 * @param[in] deviceId : the device index
 * @param[out] hbmBandWidth : return Hbm bandwidth(MBytes/s)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InvalidInput           hbmBandWidth is null
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetHbmBandWidth(unsigned int deviceId, mxSmlHbmBandWidth_t* hbmBandWidth);

/**
 * @brief This structure holds pcie Rx/Tx throughput(MBytes/s)
 */
typedef struct MxSmlPcieThroughput {
    int rx;
    int tx;
} mxSmlPcieThroughput_t;

/**
 * @brief Get pcie Rx/Tx Throughput(MBytes/s)
 *
 * @param[in] deviceId : the device index
 * @param[out] pcieThroughput : the pcie Throughput(MBytes/s)
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       pcieThroughput is null
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPcieThroughput(unsigned int deviceId, mxSmlPcieThroughput_t* pcieThroughput);

/**
 * @brief Get dma bandwidth(MBytes/s)
 *
 * @param[in] deviceId : the device index
 * @param[out] dmaBandwidth : the dma bandwidth(MBytes/s)
 * @param[in,out] size : the num of dma bandwidth
 *
 * @details the num of dma bandwidth is 4 for N-class device, and is 5 for C-class device
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       dmaBandwidth is null or size is 0
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal array size
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDmaBandwidth(unsigned int deviceId, mxSmlDmaEngineBandwidth_t* dmaBandwidth, unsigned int* size);

#define METAX_LINK_NUM 7                                //!< Number of C-class device MetaXLink
/**
 * @deprecated This structure holds MetaXLink information
*/
typedef struct MxSmlMetaXLinkInfo {
    float speed[METAX_LINK_NUM];                        //!< unit: GT/s
    unsigned int width[METAX_LINK_NUM];                 //!< lanes number
} mxSmlMetaXLinkInfo_t;

/**
 * @deprecated Get MetaXLink info
 *
 * @param[in] deviceId : the device index
 * @param[out] metaxLinkInfo : the MetaXLink Info
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       metaxLinkInfo is null
 * @retval MXSML_SysfsError         open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkInfo(unsigned int deviceId, mxSmlMetaXLinkInfo_t* metaxLinkInfo);

/**
 * @brief This structure holds single metaxlink info
*/
typedef struct MxSmlSingleMxlkInfo {
    float speed;                        //!< unit: GT/s
    unsigned int width;                 //!< lanes number
} mxSmlSingleMxlkInfo_t;

/**
 * @brief Get all MetaXLink infos
 *
 * @param[in] deviceId : the device index
 * @param[in,out] linkSize : the size of the metaxLinkInfo array that is safe to access
 * @param[out] mxlkInfos : the MetaXLink Infos
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       metaxLinkInfo or linkSize is null
 * @retval MXSML_InsufficientSize   linkSize is not large enough but linkSize return the minimal required array size
 * @retval MXSML_SysfsError         open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkInfo_v2(unsigned int deviceId, unsigned int* linkSize, mxSmlSingleMxlkInfo_t* mxlkInfos);

/**
 * @brief Available MetaXLink type
 */
typedef enum MxSmlMetaXLinkType
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_MetaXLink_Input = 0,
    MXSML_MetaXLink_Target,
} mxSmlMetaXLinkType_t;

/**
 * @brief This structure holds MetaXLink bandwidth(MBytes/s) information
 */
typedef struct MxSmlMetaXLinkBandwidth {
    int requestBandwidth;
    int responseBandwidth;
} mxSmlMetaXLinkBandwidth_t;

/**
 * @brief Get MetaXLink bandwidth(MBytes/s)
 *
 * @param[in] deviceId : the device index
 * @param[in] type : the MetaXLink type
 * @param[in,out] linkSize : the size of the metaxLinkBandwidth array that is safe to access
 * @param[out] metaxLinkBandwidth : the MetaXLink bandwidth(MBytes/s)
 *
 * @details the maximum number of C-class device MetaXLink is 7
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           metaxLinkBandwidth or linkSize is null
 * @retval MXSML_InsufficientSize       linkSize is not large enough but linkSize return the minimal required array size
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkBandwidth(unsigned int deviceId, mxSmlMetaXLinkType_t type, unsigned int* linkSize,
    mxSmlMetaXLinkBandwidth_t* metaxLinkBandwidth);

/**
 * @brief This structure holds MetaXLink dnoc perf count(Bytes) information
 */
typedef struct MxSmlMetaXLinkTrafficStat {
    long requestTrafficStat;
    long responseTrafficStat;
} mxSmlMetaXLinkTrafficStat_t;

/**
 * @brief Get MetaXLink traffic stat(Bytes)
 *
 * @param[in] deviceId : the device index
 * @param[in] type : the MetaXLink type
 * @param[in,out] linkSize : the size of the metaxLinkTrafficStat array that is safe to access
 * @param[out] metaxLinkTrafficStat : the MetaXLink traffic stat(Bytes)
 *
 * @details the maximum number of C-class device MetaXLink is 7
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           metaxLinkTrafficStat or linkSize is null
 * @retval MXSML_InsufficientSize       linkSize is not large enough but linkSize return the minimal required array size
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkTrafficStat(unsigned int deviceId, mxSmlMetaXLinkType_t type, unsigned int* linkSize,
    mxSmlMetaXLinkTrafficStat_t* metaxLinkTrafficStat);

/**
 * @brief This structure holds MetaXLink aer (ce/ue) count information
 */
typedef struct MxSmlMetaXLinkAer {
    int ceAer;
    int ueAer;
} mxSmlMetaXLinkAer_t;

/**
 * @brief Get MetaXLink aer count
 *
 * @param[in] deviceId : the device index
 * @param[in,out] linkSize : the size of the metaxLinkAer array that is safe to access
 * @param[out] metaxLinkAer : the MetaXLink aer count
 *
 * @details the maximum number of C-class device MetaXLink is 7
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           metaxLinkAer or linkSize is null
 * @retval MXSML_InsufficientSize       linkSize is not large enough but linkSize return the minimal required array size
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkAer(unsigned int deviceId, unsigned int* linkSize, mxSmlMetaXLinkAer_t* metaxLinkAer);

/**
 * @brief This structure holds the info of a remote device connected to MetaXLink
*/
typedef struct MxSmlMetaXLinkRemoteInfo {
    int deviceId;
    char bdfId[DEVICE_BDF_ID_SIZE];
} mxSmlMetaXLinkRemoteInfo_t;

/**
 * @brief Get MetaXLink remote device info
 *
 * @param[in] deviceId : the device index
 * @param[in] linkId : the link index
 * @param[out] remote : return the info of a remote device connected to target MetaXLink
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_NoDevice               No device was discovered for target link id
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InvalidInput           remote is null
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_Failure                any unexpected failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkRemoteInfo(unsigned int deviceId, unsigned int linkId, mxSmlMetaXLinkRemoteInfo_t* remote);

/**
 * @brief the status code of MetaXLink
*/
typedef enum MxSmlMetaXLinkState
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_MetaXLink_State_Enabled,
    MXSML_MetaXLink_State_Smi_Disabled,
    MXSML_MetaXLink_State_Turn_On_SRIOV_Disabled,
    MXSML_MetaXLink_State_Invalid_GpuNum_For_Topo_Disabled,
    MXSML_MetaXLink_State_Training_Failed_Disabled
} mxSmlMetaXLinkState_t;

/**
 * @brief Get MetaXLink state
 *
 * @param[in] deviceId : the device index
 * @param[out] mxlkStateCode : the status code of mxlk state
 * @param[out] mxlkState : the MetaXLink state
 * @param[in,out] size : the size of the mxlkState array that is safe to access
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           mxlkStateCode, mxlkState or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkState(unsigned int deviceId, mxSmlMetaXLinkState_t* mxlkStateCode, char* mxlkState, unsigned int* size);

/**
 * @brief the status code of MetaXLink port
*/
typedef enum MxSmlMxlkPortState
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Mxlk_Port_State_NoTraining,                 //!< initial state, no training
    MXSML_Mxlk_Port_State_Up,
    MXSML_Mxlk_Port_State_Down_Optical_InPlace,
    MXSML_Mxlk_Port_State_Down_Optical_OutPlace,
    MXSML_Mxlk_Port_State_Down_Optical_NoUse,
    MXSML_Mxlk_Port_State_NoUse                       //!< port is not used
} mxSmlMxlkPortState_t;

/**
 * @brief Get MetaXLink port state
 *
 * @param[in] deviceId : the device index
 * @param[out] mxlkPortState : the status code of mxlk port
 * @param[in,out] size : the size of the mxlkPortState array that is safe to access
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           mxlkPortState or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal mxlkPortState array size
 * @retval MXSML_Failure                the port number is invalid
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkPortState(unsigned int deviceId, mxSmlMxlkPortState_t* mxlkPortState, unsigned int* size);

/**
 * @brief Set the state of MetaXLink
 *
 * @param[in] deviceId : the device index
 * @param[in] mxlkState : the MetaXLink state, 0:disabled 1:enabled
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           mxlkState is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlSetMetaXLinkState(unsigned int deviceId, unsigned int mxlkState);

/**
 * @brief This structure holds device MetaXLink topology info
*/
typedef struct MxSmlMetaXLinkTopo {
    unsigned int topologyId;
    unsigned int socketId;
    unsigned int dieId;
} mxSmlMetaXLinkTopo_t;

/**
 * @brief Get device MetaXLink topology info
 *
 * @param[in] deviceId : the device index
 * @param[out] metaxLinkTopo : the MetaXLink topology
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       metaxLinkTopo is null
 * @retval MXSML_SysfsError         open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetMetaXLinkTopo(unsigned int deviceId, mxSmlMetaXLinkTopo_t* metaxLinkTopo);


#define VBIOS_BIN_PATH_SIZE 150                 //!< Guaranteed maximum possible size for file path

/**
 * @brief This structure holds the input and output parameters of vbios upgrade
 *
 * @details timeLimit[in] : the limited operation time [60s, 36000s]
 *          vbiosBinPath[in] : the vbios binary file path
 *          forceUpgrade[in] : 0-upgrade, 1-forceUpgrade
 *          ret[out] : if function return value is MXSML_IOControlFailure or MXSML_InvalidInput, please get details
 *          error code from ret:
 *          {
 *              UPDATE_VBIOS_RET_SUCCESS = 0,
 *              UPDATE_VBIOS_RET_ERROR_VBIOS_HBM_ADDR = 1,
 *              UPDATE_VBIOS_RET_CHECK_VBIOS_CHIPINFO_FAIL = 2,
 *              UPDATE_VBIOS_RET_CHECK_VBIOS_BOARD_TYPE_FAIL = 3,
 *              UPDATE_VBIOS_RET_VERIFY_VBIOS_SIGNATURE_FAIL = 4,
 *              UPDATE_VBIOS_RET_WRITE_FLASH_VBIOS_FAIL = 5,
 *              UPDATE_VBIOS_RET_CHECK_FLASH_VBIOS_FAIL = 6,
 *              UPDATE_VBIOS_RET_FLASH_INEXISTENCE_FAIL = 7,
 *              UPDATE_VBIOS_RET_MISMATCH_BAR0SIZE = 100
 *          }
 *          pfBar0Size[out] : when ret = 100, can get device pfBar0Size
 *          vfBar0Size[out] : when ret = 100, can get device vfBar0Size
 *
*/
typedef struct MxSmlVbiosUpgradeArg
{
    unsigned int timeLimit;
    char vbiosBinPath[VBIOS_BIN_PATH_SIZE + 1];
    int forceUpgrade;
    unsigned int ret;
    int pfBar0Size;
    int vfBar0Size;
} mxSmlVbiosUpgradeArg_t;

/**
 * @brief Device vbios upgrade
 *
 * @param[in] deviceId : the device index
 * @param[in,out] vbiosUpgradeArg : vbios upgrade arg
 *
 * @retval MXSML_Success               call was successful
 * @retval MXSML_InvalidInput          vbiosUpgradeArg is null or binPath in vbiosUpgradeArg is invalid
 * @retval MXSML_InvalidDeviceId       deviceId is out of range
 * @retval MXSML_MmioNotEnough         Mmio address space is not enough for vf-vbios
 * @retval MXSML_OpenDeviceFileFailure open device file failed
 * @retval MXSML_IOControlFailure      operation failed
 */
mxSmlReturn_t DECLDIR mxSmlVbiosUpgrade(unsigned int deviceId, mxSmlVbiosUpgradeArg_t* vbiosUpgradeArg);

/**
 * @brief Dump vbios data
 *
 * @param[in] deviceId : the device index
 * @param[in] timeLimit : the limited operation time [60s, 36000s]
 * @param[in] binPath : the vbios data file will be saved in binPath
 * @param[out] ret : firmware error code
 *
 * @details if function return value is MXSML_IOControlFailure, please get detailed error code from ret
 *
 * @retval MXSML_Success               call was successful
 * @retval MXSML_InvalidInput          binPath is too long
 * @retval MXSML_InvalidDeviceId       deviceId is out of range
 * @retval MXSML_OpenDeviceFileFailure open device file failed
 * @retval MXSML_IOControlFailure      operation failed
 */
mxSmlReturn_t DECLDIR mxSmlDumpVbios(unsigned int deviceId, unsigned int timeLimit, const char* binPath, unsigned int* ret);

/**
 * @brief Function level reset
 *
 * @param[in] deviceId : the device index
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlFunctionLevelReset(unsigned int deviceId);

/**
 * @brief Perform VF function level reset from PF
 *
 * @param[in] deviceId : the vf device index
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_TargetVfNotFound       target vf not found
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlFunctionLevelResetVfFromPf(unsigned int deviceId);

/**
 * @brief Set unlock key
 *
 * @param[in] deviceId : the device index
 * @param[in] unlockKey : the unlock key
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidInput       unlockKey is invalid
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetUnlockKey(unsigned int deviceId, const char* unlockKey);

/**
 * @brief Get process information currently running on GPU
 *
 * See \ref mxSmlProcessInfo_v2_t for details on process die info
 *          mxSmlProcessInfo_v3_t for details on process sgpu info
 *
 * @param[in] processNumber :  the amount of memory in MxSmlProcessInfo provided by @p processInfo
 * @param[out] processInfo : store process information
 *
 * since processes are dynamic, preconfigured processInfo may lost some information,
 * it's better to reserve space for processInfo by setting a larger processNumber
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidInput       processInfo is null
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetProcessInfo(unsigned int processNumber, mxSmlProcessInfo_t* processInfo);

/**
 * @brief mxSmlGetProcessInfo_v2 records process's die id
 */
mxSmlReturn_t DECLDIR mxSmlGetProcessInfo_v2(unsigned int processNumber, mxSmlProcessInfo_v2_t* processInfo);

/**
 * @brief mxSmlGetProcessInfo_v3 records process's sgpu id
 */
mxSmlReturn_t DECLDIR mxSmlGetProcessInfo_v3(unsigned int processNumber, mxSmlProcessInfo_v3_t* processInfo);

/**
 * @brief Get the number of processes currently using GPU
 *
 * @param[out] processNumber : the number of processes currently using GPU
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidInput       processNumber is null
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetNumberOfProcess(unsigned int* processNumber);

/**
 * @brief Get process information currently running on target GPU
 *
 * See \ref mxSmlProcessInfo_v2_t for details on process die info
 *          mxSmlProcessInfo_v3_t for details on process sgpu info
 *
 * @param[in] deviceId : the device index
 * @param[in,out] processNumber : reference in which to provide the infos array size,
 *                                and to return the number of returned elements
 * @param[out] processInfo : store process information
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidInput       processNumber or processInfo is null
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSingleGpuProcess(unsigned int deviceId, unsigned int* processNumber, mxSmlProcessInfo_t* processInfo);

/**
 * @@deprecated mxSmlGetSingleGpuProcess_v2 records process's die id
 */
mxSmlReturn_t DECLDIR mxSmlGetSingleGpuProcess_v2(unsigned int deviceId, unsigned int* processNumber, mxSmlProcessInfo_v2_t* processInfo);

/**
 * @brief mxSmlGetSingleGpuProcess_v3 records process's sgpu id
 */
mxSmlReturn_t DECLDIR mxSmlGetSingleGpuProcess_v3(unsigned int deviceId, unsigned int* processNumber, mxSmlProcessInfo_v3_t* processInfo);

/**
 * @brief Get topology info for two devices
 *
 * @param[in] deviceId1 : the first device index
 * @param[in] deviceId2 : the second device index
 * @param[out] topoInfo : return the link type
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId1 or deviceId2 is out of range
 * @retval MXSML_InvalidInput       topoInfo is null
 *
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceTopology(unsigned int deviceId1, unsigned int deviceId2, mxSmlGpuTopologyLevel_t* topoInfo);

/**
 * @brief Get distance for two devices
 *
 * @param[in] deviceId1 : the first device index
 * @param[in] deviceId2 : the second device index
 * @param[out] distance : return the distance value
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId1 or deviceId2 is out of range
 * @retval MXSML_InvalidInput       distance is null
 * @retval MXSML_Failure            the distance data is not available
 *
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceDistance(unsigned int deviceId1, unsigned int deviceId2, unsigned int* distance);

/**
 * @brief Retrieves an array of unsigned ints (sized to cpuSetSize) of bitmasks with
 * the ideal CPU affinity for the device
 *
 * @param[in] deviceId : the target device index
 * @param[in] cpuSetSize : the size of the cpuSet array that is safe to access
 * @param[out] cpuSet : array reference in which to return a bitmask of CPUs
 *
 * @details if processors 0,1,31, and 32 are ideal for the device and cpuSetSize is 2, 
 *          the retrieved cpuSet is cpuSet[0] = 0x80000003, cpuSet[1] = 0x1
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       cpuSet is null or cpuSetSize is 0
 *
 */
mxSmlReturn_t DECLDIR mxSmlGetCpuAffinity(unsigned int deviceId, unsigned int cpuSetSize, unsigned int* cpuSet);

/**
 * @brief Sets the ideal affinity for the calling thread and device
 *
 * @param[in] deviceId : the target device index
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 *
 */
mxSmlReturn_t DECLDIR mxSmlSetCpuAffinity(unsigned int deviceId);

/**
 * @brief Clear all affinity bindings for the calling thread
 *
 * @param[in] deviceId : the target device index
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 *
 */
mxSmlReturn_t DECLDIR mxSmlClearCpuAffinity(unsigned int deviceId);

/**
 * @brief Retrieves an array of unsigned ints (sized to nodeSetSize) of bitmasks with
 * the ideal NUMA node affinity for the device
 *
 * @param[in] deviceId : the target device index
 * @param[in] nodeSetSize : the size of the nodeSet array that is safe to access
 * @param[out] nodeSet : array reference in which to return a bitmask of nodes
 *
 * @details if node 1 is ideal for the device and nodeSetSize is 1, 
 *          the retrieved nodeSet is nodeSet[0] = 0x2
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       nodeSet is null or nodeSetSize is 0
 *
 */
mxSmlReturn_t DECLDIR mxSmlGetNodeAffinity(unsigned int deviceId, unsigned int nodeSetSize, unsigned int* nodeSet);

/**
 * @brief C-class device warm reset
 *
 * @param[in] deviceId : the device index
 *
 * @details For a group of devices connected by the MetaXLink,
 *          only one device needs to be reset.
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 * @retval MXSML_OperationNotSupport    warm reset is not support on target device
 * @retval MXSML_BusyDevice             device is busy
 * @retval MXSML_Failure                any unexpected failure
 */
mxSmlReturn_t DECLDIR mxSmlReset(unsigned int deviceId);

/**
 * @brief C-class PF device force warm reset
 *
 * @param[in] deviceId : the device index
 *
 * @details make sure no process is running on the
 *          target PF and related VF.
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 * @retval MXSML_OperationNotSupport    warm reset is not support on target device
 * @retval MXSML_Failure                any unexpected failure
 */
mxSmlReturn_t DECLDIR mxSmlForceReset(unsigned int deviceId);

/**
 * @brief Set PCI speed by changing PCI generation
 *
 * @param[in]  deviceId : the device index
 * @param[in]  pciGen   : the target PCI generation
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       pciGen is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetPciSpeed(unsigned int deviceId, mxSmlPciGen_t pciGen);

#define MIN_PCI_DELAY 1                     //!< Minimum pci delay, unit: ms
#define MAX_PCI_DELAY 10000                 //!< Maximum pci delay, unit: ms

/**
 * @brief Show pci delay time
 *
 * @param[in]  deviceId : the device index
 * @param[out]  pciDelay : pci delay time, range: [1,10000], unit: ms
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetPciDelay(unsigned int deviceId, unsigned int* pciDelay);

/**
 * @brief Set pci delay time before speed change
 *
 * @param[in]  deviceId : the device index
 * @param[in]  pciDelay : pci delay time, range: [1,10000], unit: ms
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         open sysfs file failure
 * @retval MXSML_SysfsWriteError    write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetPciDelay(unsigned int deviceId, unsigned int pciDelay);

/**
 * @brief Get DPM clock frequency(MHz)
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : the target IP
 * @param[out] clockInfo : dpm clock info for the target IP
 * @param[in,out] size : size of clock array that is safe to access
 *
 * @details the clock array size set as 12 is large enough
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 * @retval MXSML_InvalidInput       clockInfo is null
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal clock array size
 */
mxSmlReturn_t DECLDIR mxSmlGetDpmIpClockInfo(unsigned int deviceId, mxSmlDpmIp_t dpmIp, unsigned int* clockInfo, unsigned int* size);

/**
 * @brief Get DPM voltage (mV)
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : the target IP
 * @param[out] voltageInfo : dpm voltage info for the target IP
 * @param[in,out] size : size of voltage array that is safe to access
 *
 * @details the voltage array size set as 12 is large enough
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 * @retval MXSML_InvalidInput       voltageInfo is null
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal vdd array size
 */
mxSmlReturn_t DECLDIR mxSmlGetDpmIpVddInfo(unsigned int deviceId, mxSmlDpmIp_t dpmIp, unsigned int* voltageInfo, unsigned int* size);

/**
 * @brief Get current DPM ip perf level
 *
 * @param[in] deviceId : the device index
 * @param[in] dpmIp : the dpm ip
 * @param[out] dpmIpPerfLevel : dpm ip perf level
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 * @retval MXSML_InvalidInput       dpmIpPerfLevel is null
 */
mxSmlReturn_t DECLDIR mxSmlGetCurrentDpmIpPerfLevel(unsigned int deviceId, mxSmlDpmIp_t dpmIp, unsigned int* dpmIpPerfLevel);

/**
 * @brief Get maca version
 *
 * @param[in] size : length of version
 * @param[out] version : version of unit
 *
 * @details the guaranteed maximum possible size is 32
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidInput           version or size is null
 * @retval MXSML_LoadDllFailure         load dynamic library failed
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 */
mxSmlReturn_t DECLDIR mxSmlGetMacaVersion(char* version, unsigned int* size);

#define VERSION_INFO_SIZE 64                //!< Guaranteed maximum possible size for Version info

/**
 * @brief Get device version
 *
 * @param[in] deviceId : the device index
 * @param[in] versionUnit : unit
 * @param[out] version : version of unit
 * @param[in,out] size : length of version
 *
 * @details the guaranteed maximum possible size is 64
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           versionUnit in invalid, or version or size is null
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal version size
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceVersion(unsigned int deviceId, mxSmlVersionUnit_t versionUnit, char* version, unsigned int* size);

/**
 * @brief Get device IP usage
 *
 * @param[in] deviceId : the device index
 * @param[in] ip : device ip
 * @param[out] usage : return device usage
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceIpUsage(unsigned int deviceId, mxSmlUsageIp_t ip, int* usage);

/**
 * @brief Get device xcore ap usage for C-class device
 *
 * @param[in] deviceId : the device index
 * @param[out] apUsage : return each AP's usage
 * @param[in,out] size : size of apUsage array, return the actual num of AP usage
 * @param[in,out] dpcNum : size of dpc
 *
 * @retval MXSML_Success              call was successful
 * @retval MXSML_InvalidDeviceId      deviceId is out of range
 * @retval MXMSL_InvalidInput         apUsage or size or dpcNum is null
 * @retval MXSML_InsufficientSize     size or dpcNum is not large enough but size or dpcNum return the minimal size
 * @retval MXSML_OperationNotSupport  the operation is not support on target device
 * @retval MXSML_SysfsError           read sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetXcoreApUsage(unsigned int deviceId, unsigned int* apUsage, unsigned int* size, unsigned int* dpcNum);

/**
 * @brief Get firmware ip loglevel
 *
 * @param[in] deviceId : the device index
 * @param[in] fwIpName : the name of firmware ip
 * @param[out] fwIpLoglevel : the loglevel of firmware ip
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           fwIpName is invalid or fwIpLoglevel is null
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 */
mxSmlReturn_t DECLDIR mxSmlGetFwIpLoglevel(unsigned int deviceId, mxSmlFwIpName_t fwIpName, mxSmlLoglevel_t* fwIpLoglevel);

/**
 * @brief Set firmware loglevel
 *
 * @param[in]  deviceId : the device index
 * @param[in]  fwIpName : ip name of firmware
 * @param[in]  loglevel : loglevel used to set firmware ip
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           fwIpName is invalid or loglevel is out of range
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetFwLoglevel(unsigned int deviceId, mxSmlFwIpName_t fwIpName, mxSmlLoglevel_t loglevel);

#define CHIP_SERIAL_SIZE 32               //!< Guaranteed maximum possible size for chip serial
/**
 * @brief Get chip serial
 *
 * @param[in]  deviceId : the device index
 * @param[out] chipSerial : chip serial
 * @param[in,out] size : length of chip serial
 *
 * @details the guaranteed maximum possible size is 32
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InvalidDeviceId    deviceId is out of range
 * @retval MXSML_InvalidInput       chipSerial or size is null
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal required size
 * @retval MXSML_SysfsError         read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetChipSerial(unsigned int deviceId, char* chipSerial, unsigned int* size);

/**
 * @brief Get xcore ap usage toggle for MXC device
 *
 * @param[in] deviceId : the device index
 * @param[out] apUsageToggle : the xcore ap usage toggle: 0(disable) 1(enable)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           apUsageToggle is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetApUsageToggle(unsigned int deviceId, unsigned int* apUsageToggle);

/**
 * @brief Set xcore ap usage toggle for MXC device
 *
 * @param[in] deviceId : the device index
 * @param[in] toggle : the state to be set: 0(disable) 1(enable)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetApUsageToggle(unsigned int deviceId, int toggle);

/**
 * @brief Get ecc state
 *
 * @param[in] deviceId : the device index
 * @param[out] eccState : the ecc state: 0(disable) 1(enable)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           eccState is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetEccState(unsigned int deviceId, unsigned int* eccState);

/**
 * @brief Set ecc state
 *
 * @param[in] deviceId : the device index
 * @param[in] eccState : the ecc state to be set: 0(disable) 1(enable)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlSetEccState(unsigned int deviceId, unsigned int eccState);

#define MAX_SMP_NUM 2                    //!< the max num of smp
#define MAX_CCX_NUM 4                    //!< the max num of ccx
/**
 * @deprecated This structure holds loglevel of firmware's ips
 */
typedef struct MxSmlFwLoglevel {
    unsigned int smpCount;
    mxSmlLoglevel_t smp[MAX_SMP_NUM];
    unsigned int ccxCount;
    mxSmlLoglevel_t ccx[MAX_CCX_NUM];
} mxSmlFwLoglevel_t;

/**
 * @brief Get firmware loglevel
 *
 * @param[in] deviceId : the device index
 * @param[out] fwLoglevel : the loglevel of firmware
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           fwLoglevel is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetFwLoglevel(unsigned int deviceId, mxSmlFwLoglevel_t* fwLoglevel);

/**
 * @brief Get vbios sriov information
 *
 * @param[in] deviceId : the device index
 * @param[out] sriovState : determine if current device support SRIOV or not
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           sriovState is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
*/
mxSmlReturn_t DECLDIR mxSmlGetSriovState(unsigned int deviceId, unsigned int* sriovState);

/**
 * @brief Get device pcie slots
 *
 * @param[in] deviceId : the device index
 * @param[out] slotId : the slot id
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           slotId is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetDeviceSlot(unsigned int deviceId, unsigned int* slotId);

/**
 * @brief This structure holds optical module status
 *
 * voltage and temperature values are enlarged by 100 times
*/
typedef struct MxSmlOpticalModuleStatus {
    int temperature;                             //!< unit: degress 0.01C
    unsigned int voltage;                        //!< unit: mV
    unsigned int moduleState;
    unsigned int dataPathState;
    unsigned int rxState[2];                     //!< 6 bytes total
    unsigned int version[2];                     //!< version[0]: major, version[1]: minor
} mxSmlOpticalModuleStatus_t;

/**
 * @brief Get optical module status
 *
 * @param[in] deviceId : the device index
 * @param[out] status : optical module status
 * @param[in,out] size : the size of the status array that is safe to access
 *
 * @details the guaranteed maximum possible size is 3
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           status or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal status array size
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetOpticalModuleStatus(unsigned int deviceId, mxSmlOpticalModuleStatus_t* status, unsigned int* size);

#define OPTICAL_MODULE_INFO_SIZE 32     //!< Guaranteed maximum possible size for optical module info
/**
 * @brief This structure holds optical module firmware info
*/
typedef struct MxSmlOpticalModuleInfo {
    char name[OPTICAL_MODULE_INFO_SIZE];
    unsigned int oui[3];
    char pn[OPTICAL_MODULE_INFO_SIZE];
    char rev[OPTICAL_MODULE_INFO_SIZE];
    char sn[OPTICAL_MODULE_INFO_SIZE];
} mxSmlOpticalModuleInfo_t;

/**
 * @brief Get optical module firmware info
 *
 * @param[in] deviceId : the device index
 * @param[out] info : optical module firmware info
 * @param[in,out] size : the size of the info array that is safe to access
 *
 * @details the guaranteed maximum possible size is 3
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           info or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal info array size
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetOpticalModuleInfo(unsigned int deviceId, mxSmlOpticalModuleInfo_t* info, unsigned int* size);

#define OPTICAL_POWER_RX_NUM 8     //!< Guaranteed maximum possible number for rx optical power
/**
 * @brief This structure holds optical module power, unit: 0.1uW
*/
typedef struct MxSmlOpticalModulePower {
    unsigned int value[OPTICAL_POWER_RX_NUM];              //!< Internally measured Rx input optical power
} mxSmlOpticalModulePower_t;

/**
 * @brief Get optical module power
 *
 * @param[in] deviceId : the device index
 * @param[out] power : optical module power
 * @param[in,out] size : the size of the power array that is safe to access
 *
 * @details the guaranteed maximum possible size is 3
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           power or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal power array size
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetOpticalModulePower(unsigned int deviceId, mxSmlOpticalModulePower_t* power, unsigned int* size);

/**
 * @brief Get device real path
 *
 * @param[in] deviceId : the device index
 * @param[out] deviceRealPath : the device real path
 * @param[in,out] size : the size of the deviceRealPath array that is safe to access
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           deviceRealPath or size is null
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal deviceRealPath array size
 * @retval MXSML_SysfsError             get realpath failed
*/
mxSmlReturn_t DECLDIR mxSmlGetDeviceRealPath(unsigned int deviceId, char* deviceRealPath, unsigned int* size);

/**
 * @brief Get device state
 *
 * @param[in] deviceId : the device index
 * @param[out] deviceState : the device state: 0(not available) 1(available)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           deviceState is null
 * @retval MXSML_SysfsError             get realpath failed
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceState(unsigned int deviceId, int* deviceState);

/**
 * @brief convert mxsml error code into readable strings
 *
 * @param[in] result : mxsml error code to convert
 *
 * @retval String representation of the error
 */
const char* mxSmlGetErrorString(mxSmlReturn_t result);

/**
 * @brief Get system operation mode
 *
 * @param[out] opMode : system operation mode: 0(Normal mode) 1(Maintenance mode)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidInput           opMode is null
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetOpMode(unsigned int* opMode);

/**
 * @brief Set system operation mode
 *
 * @param[in] opMode : system operation mode: 0(Normal mode) 1(Maintenance mode)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 * @retval MXSML_PermissionDenied   the operations require root access to run
 */
mxSmlReturn_t DECLDIR mxSmlSetOpMode(unsigned int opMode);

/**
 * @brief Get uuids of local and remote server connected by metaxlink
 *
 * @param[out] local : local server's uuid
 * @param[out] remote: remote server's uuid
 * @param[in,out] size: the size of local and remote that is safe to access
 *
 * @details the guaranteed uuid maximum possible size is 64
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal required size
 * @retval MXSML_InvalidInput       any pointer is null
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetLocalAndRemoteUuid(char* local, char* remote, unsigned int* size);

/**
 * @brief Get uuids of local and multiple remote servers connected by metaxlink
 *
 * @param[out] local : local server's uuid
 * @param[out] remotes: remote servers' uuid
 * @param[in,out] remotesSize: the number of remote servers
 * @param[in,out] uuidSize: the size of local and remote that is safe to access
 *
 * @details the guaranteed uuid maximum possible size is 64
 *
 * @retval MXSML_Success            call was successful
 * @retval MXSML_InsufficientSize   size is not large enough but size return the minimal required size
 * @retval MXSML_InvalidInput       any pointer is null
 * @retval MXSML_SysfsError         open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetLocalAndMultipleRemoteUuid(char* local, char** remotes, unsigned int* remotesSize, unsigned int* uuidSize);

/** @addtogroup mxsmlClocksThrottleReasons
 *  @{
 */

/** Nothing is running on the GPU and the clocks are dropping to Idle state
 *
 */
#define mxsmlClocksThrottleReasonIdle 0x0000000000000001LL

/** GPU clocks are limited by current setting of applications clocks
 *
 * @see mxSmlSetDpmIpMaxPerfLevel
 */
#define mxsmlClocksThrottleReasonApplicationsLimit 0x0000000000000002LL

/** The clocks are optimized to ensure not to exceed power limits
 *
 */
#define mxsmlClocksThrottleReasonOverPower 0x0000000000000004LL

/** The clocks are optimized to ensure not to exceed chip temperature limits
 *
 */
#define mxsmlClocksThrottleReasonChipOverTemperature 0x0000000000000008LL

/** The clocks are optimized to ensure not to exceed VR temperature limits
 *
 */
#define mxsmlClocksThrottleReasonVrOverTemperature 0x0000000000000010LL

/** The clocks are optimized to ensure not to exceed hbm temperature limits
 *
 */
#define mxsmlClocksThrottleReasonHbmOverTemperature 0x0000000000000020LL

/** The clocks are optimized to ensure not to exceed temperature limits
 *
 */
#define mxsmlClocksThrottleReasonThermalOverTemperature 0x0000000000000040LL

/** The clocks are optimized by peak current protection
 *
 */
#define mxsmlClocksThrottleReasonPcc 0x0000000000000080LL

/** HW Power Brake Slowdown is engaged
 *
 */
#define mxsmlClocksThrottleReasonPowerBrake 0x0000000000000100LL

/** The clocks are optimized by di/dt dynamic modulation
 *
 */
#define mxsmlClocksThrottleReasonDidt 0x0000000000000200LL

/** The clocks are optimized by utilization dynamic modulation
 *
 */
#define mxsmlClocksThrottleReasonDynamic 0x0000000000000400LL

/** The clocks are limited by other reason
 *
 */
#define mxsmlClocksThrottleReasonOther 0x0000000080000000LL

/** Bit mask representing no clocks throttling
 *
 * Clocks are as high as possible.
 */
#define mxsmlClocksThrottleReasonNone 0x0000000000000000LL

/** Bit mask representing all supported clocks throttling reasons
 * New reasons might be added to this list in the future
 */
#define mxsmlClocksThrottleReasonAll (mxsmlClocksThrottleReasonIdle \
    | mxsmlClocksThrottleReasonApplicationsLimit                    \
    | mxsmlClocksThrottleReasonOverPower                            \
    | mxsmlClocksThrottleReasonChipOverTemperature                  \
    | mxsmlClocksThrottleReasonVrOverTemperature                    \
    | mxsmlClocksThrottleReasonHbmOverTemperature                   \
    | mxsmlClocksThrottleReasonThermalOverTemperature               \
    | mxsmlClocksThrottleReasonPcc                                  \
    | mxsmlClocksThrottleReasonPowerBrake                           \
    | mxsmlClocksThrottleReasonDidt                                 \
    | mxsmlClocksThrottleReasonDynamic                              \
)
/** @} */

/**
 * @brief Get current clocks throttling reasons
 *
 * @note More than one bit can be enabled at the same time. Multiple reasons can be affecting clocks at once.
 *
 * @param[in] deviceId : the device index
 * @param[out] clocksThrottleReasons: Reference in which to return bitmask of active clocks throttle reasons
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           clocksThrottleReasons is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 *
 * @see mxsmlClocksThrottleReasons
 */
mxSmlReturn_t DECLDIR mxSmlGetCurrentClocksThrottleReason(unsigned int deviceId, unsigned long long* clocksThrottleReasons);

/**
 * @brief Get the board power limit associated with the device
 *
 * @param[in] deviceId : the device index
 * @param[out] limit : reference in which to return the board power limit in milliwatts
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           limit is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetBoardPowerLimit(unsigned int deviceId, unsigned int* limit);

/**
 * @brief Show device slice gpu toggle
 *
 * @param[in] deviceId : the device index
 * @param[out] toggle : return the sgpu toggle: 0(disable) 1(enable)
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetSgpuToggle(unsigned int deviceId, unsigned int *toggle);

/**
 * @brief Set device slice gpu toggle
 *
 * @param[in] deviceId : the device index
 * @param[in] toggle : the state to be set: 0(disable) 1(enable)
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlSetSgpuToggle(unsigned int deviceId, unsigned int toggle);

/**
 * @brief Create a slice gpu
 *
 * @param[in] deviceId : the device index
 * @param[in] memory : sgpu memory quota, range: (0, available gpu vram), step: 128MB, unit: MB
 * @param[in] compute : sgpu compute quota, range: 0 - 100%
 * @param[out] sgpuId : return the created sgpu index
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           sgpuId is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlCreateSgpu(unsigned int deviceId, unsigned int memory, unsigned int compute, unsigned int *sgpuId);

/**
 * @brief Remove a slice gpu
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlRemoveSgpu(unsigned int deviceId, unsigned int sgpuId);

#define MAX_SGPU_COUNT 16               //!< Guaranteed maximum sgpu count on one device

/**
 * @brief Get number of available sgpus in the device
 *
 * @details if deviceId is out of range or any error occured, return 0
 *
 * @return the number of available sgpus on the device
 */
unsigned int mxSmlGetSgpuCount(unsigned int deviceId);

/**
 * @brief This structure holds attributes for a sgpu
 */
typedef struct MxSmlSgpuInfo {
    unsigned int parentDeviceId;
    unsigned int sgpuId;
    unsigned int vramQuota;
    unsigned int swQueuePriority;                     //!< 0: low, 1: normal, 2: high
    unsigned int computeQuota;
    unsigned int minor;
    unsigned int deviceQueuePriority;                 //!< 0: low, 1: high
    char uuid[DEVICE_UUID_SIZE];
} mxSmlSgpuInfo_t;

/**
 * @brief Get sgpu attributes
 *
 * @param[in]  deviceId : the device index
 * @param[in]  sgpuId : the sgpu index
 * @param[out] sgpuInfo : return sgpu attributes
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           sgpuInfo is null
 * @retval MXSML_OperationNotSupport    target sgpu is not exist
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuInfo(unsigned int deviceId, unsigned int sgpuId, mxSmlSgpuInfo_t* sgpuInfo);

/**
 * @brief Set sgpu device queue priority
 *
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[in] deviceQueuePriority : sgpu device queue priority, 0: low, 1: high
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    target sgpu is not exist
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetSgpuDeviceQueuePriority(unsigned int deviceId,
                    unsigned int sgpuId, unsigned int deviceQueuePriority);

/**
 * @brief the pci event type
*/
typedef enum MxSmlPciEventType
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_Pci_Event_AER_UE,      //!< PCIe AER uncorrectable error
    MXSML_Pci_Event_AER_CE,      //!< PCIe AER correctable error
    MXSML_Pci_Event_SYNFLD,      //!< PCIe syncflood error
    MXSML_Pci_Event_DBE,         //!< PCIe device base error
    MXSML_Pci_Event_MMIO         //!< PCIe mmio disconnect
} mxSmlPciEventType_t;

#define EVENT_TIME_LENGTH 20     //!< Guaranteed maximum possible length for pci event time
#define EVENT_NAME_LENGTH 64     //!< Guaranteed maximum possible length for pci event name

/**
 * @brief this structure holds the pci event info
*/
typedef struct MxSmlPciEventInfo
{
    int bitNumber;
    int count;
    char firstTime[EVENT_TIME_LENGTH];
    char name[EVENT_NAME_LENGTH];
} mxSmlPciEventInfo_t;

/**
 * @brief Get state of PCI mmio
 *
 * @param[in] deviceId : the device index
 * @param[out] state : the state of PCI mmio
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           state is null
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetPciMmioState(unsigned int deviceId, unsigned int* state);

#define MAX_EVENT_NUM 32        //!< max num of event
/**
 * @brief Get event info
 *
 * @param[in] deviceId : the device index
 * @param[in] eventType : the event type
 * @param[out] eventInfo : the event infos
 * @param[in,out] size : the size of the eventInfos array that is safe to access
 *
 * @retval MXSML_SUCCESS                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           eventType is invalid or eventInfos or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal eventInfos array size
 * @retval MXSML_SysfsError             open sysfs file failure
*/
mxSmlReturn_t DECLDIR mxSmlGetPciEventInfo(unsigned int deviceId, mxSmlPciEventType_t eventType, mxSmlPciEventInfo_t* eventInfos,
    unsigned int* size);

/**
 * @brief Get sgpu state
 *
 * @param[in]  deviceId : the device index
 * @param[in]  sgpuId : the sgpu index
 * @param[out] state : the sgpu state: 0(not available) 1(available)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           state is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuState(unsigned int deviceId, unsigned int sgpuId, int* state);

/**
 * @brief Set sgpu vram quota
 *
 * @details sgpu vram quota shoule be multiple of 128, if not,
 *          MXSML_SysfsWriteError will be returned with errno Invalid argument
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[in] vram : sgpu target vram quota, multiple of 128, unit: MB
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    target sgpu is not exist
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetSgpuVramQuota(unsigned int deviceId, unsigned int sgpuId, unsigned int vram);

/**
 * @brief Set sgpu compute quota
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[in] compute : sgpu target compute quota, range: 0 - 100%
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    target sgpu is not exist
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetSgpuComputeQuota(unsigned int deviceId, unsigned int sgpuId, unsigned int compute);

/**
 * @brief This structure holds device available quota for sgpu
 */
typedef struct MxSmlSgpuAvailableQuota {
    unsigned int totalVramQuota;        //!< device total vram quota for sgpu, unit: MB
    unsigned int vramQuota;             //!< device free vram quota, has not been assigned to sgpu, unit: MB
    unsigned int computeQuota;          //!< device free compute quota, unit: %
} mxSmlSgpuAvailableQuota_t;

/**
 * @brief Get device available quota
 *
 * @param[in]  deviceId : the device index
 * @param[out] availableInfo : return device available quota
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           availableInfo is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuAvailableQuota(unsigned int deviceId, mxSmlSgpuAvailableQuota_t* availableInfo);

/**
 * @brief Get sgpu usage in ten thousandth
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[out] usage : return sgpu usage
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           usage is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuUsage(unsigned int deviceId, unsigned int sgpuId, int* usage);

/**
 * @brief This structure holds the sgpu memory information, unit: bytes
 */
typedef struct MxSmlSgpuMemoryInfo {
    long total;
    long used;
    long free;
} mxSmlSgpuMemoryInfo_t;

/**
 * @brief Get sgpu memory information
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[out] memory : return sgpu memory information
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           memory is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuMemory(unsigned int deviceId, unsigned int sgpuId, mxSmlSgpuMemoryInfo_t* memory);

/**
 * @brief Get device timeslice for sgpu
 *
 * @param[in] deviceId : the device index
 * @param[out] timeslice : return sgpu timeslice, unit: ms
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           timeslice is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceTimeslice(unsigned int deviceId, unsigned int* timeslice);

/**
 * @brief Set device timeslice for sgpu
 *
 * @param[in] deviceId : the device index
 * @param[in] timeslice : target timeslice, unit: ms
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetDeviceTimeslice(unsigned int deviceId, unsigned int timeslice);

/**
 * @brief the device sched class for sgpu
*/
typedef enum MxSmlSchedClass
#ifdef __cplusplus
 : unsigned int
#endif
{
    MXSML_SCHED_CLASS_BEST_EFFORT,             //!< sgpu has no compute quota limit
    MXSML_SCHED_CLASS_FIXED_SHARE,             //!< sgpu cannot used spare resources other than fixed compute quota
    MXSML_SCHED_CLASS_BURST_SHARE,             //!< sgpu can used spare resources other than fixed compute quota
    MXSML_SCHED_CLASS_MAX,
} mxSmlSchedClass_t;

/**
 * @brief Get device sched class for sgpu
 *
 * @param[in] deviceId : the device index
 * @param[out] schedClass : return sgpu sched class
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           schedClass is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetDeviceSchedClass(unsigned int deviceId, mxSmlSchedClass_t* schedClass);

/**
 * @brief Set device sched class for sgpu
 *
 * @param[in] deviceId : the device index
 * @param[in] schedClass : target sched class
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetDeviceSchedClass(unsigned int deviceId, mxSmlSchedClass_t schedClass);

#define SGPU_ALIAS_SIZE 32               //!< Guaranteed maximum possible size for sgpu alias
/**
 * @brief Get sgpu alias
 *
 * @param[in]  deviceId : the device index
 * @param[in]  sgpuId : the sgpu index
 * @param[out] alias : return sgpu alias
 * @param[in,out]  size : the size of alias that is safe to access
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           alias or size is null
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal alias char size
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuAlias(unsigned int deviceId, unsigned int sgpuId, char* alias, unsigned int* size);

/**
 * @brief Set sgpu alias
 *
 * @param[in] deviceId : the device index
 * @param[in] sgpuId : the sgpu index
 * @param[in] alias : the sgpu alias
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           alias is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_SysfsWriteError        write sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlSetSgpuAlias(unsigned int deviceId, unsigned int sgpuId, char* alias);

/**
 * @brief Get isaVersion of device
 *
 * @param[in] deviceId : the device index
 * @param[in,out] isaVersion : the device isaVersion
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           isaVersion is null
*/
mxSmlReturn_t DECLDIR mxSmlGetDeviceIsaVersion(unsigned int deviceId, int* isaVersion);

#define DEVICE_UNAVAILABLE_REASON_SIZE 64           //!< Guaranteed maximum possible size for device unavailable reason

/**
 * @brief this structure holds the device unavailable reason
*/
typedef struct MxSmlDeviceUnavailableReasonInfo
{
    int unavailableCode;
    char unavailableReason[DEVICE_UNAVAILABLE_REASON_SIZE];
} mxSmlDeviceUnavailableReasonInfo_t;

/**
 * @brief Get device unavailable reason
 *
 * @param[in] deviceId : the device index
 * @param[in,out] reason : the device unavailable reason
 *
 * @details the guaranteed maximum possible size is 64
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           reason is null
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
*/
mxSmlReturn_t DECLDIR mxSmlGetDeviceUnavailableReason(unsigned int deviceId, mxSmlDeviceUnavailableReasonInfo_t* reason);

/**
 * @brief Get device fan speed info
 *
 * @param[in]  deviceId : the device index
 * @param[out] rpm : return the fan revolutions per minute
 * @param[out] pwm : return the fan pulse width modulation
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           rpm or pwm is null
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
*/
mxSmlReturn_t DECLDIR mxSmlGetFanSpeedInfo(unsigned int deviceId, unsigned int* rpm, unsigned int* pwm);

/**
 * @brief Get sgpu annotations id for k8s
 *
 * @param[in]  deviceId : the device index
 * @param[in]  sgpuId : the sgpu index
 * @param[out] annotationsId : return sgpu annotations id
 * @param[in,out] size : length of annotations id
 *
 * @details annotations id is empty when it is not exist,
 *          the guaranteed maximum possible size is 96,
 *          id e.g. GPU-31b30e43-7304-6142-1b0d-e29d4d2c6b50-12:2
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           annotationsId or size is null
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InsufficientSize       size is not large enough but size return the minimal required size
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetSgpuAnnotationsId(unsigned int deviceId, unsigned int sgpuId,
                                        char* annotationsId, unsigned int* size);

/**
 * @brief This structure holds ECC total error count
*/
typedef struct MxSmlEccErrorCount
{
    unsigned int sramCE;
    unsigned int sramUE;
    unsigned int dramCE;
    unsigned int dramUE;
    unsigned int retiredPage;
} mxSmlEccErrorCount_t;

/**
 * @brief Get total ECC error counts
 *
 * @param[in] deviceId : the device index
 * @param[out] eccCounts : the Ecc errors
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_InvalidInput           eccCounts is null
 * @retval MXSML_SysfsError             open sysfs file failure
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
*/
mxSmlReturn_t DECLDIR mxSmlGetTotalEccErrors(unsigned int deviceId, mxSmlEccErrorCount_t* eccCounts);

/**
 * @brief Get board id where device is located
 *
 * @param[in] deviceId : the device index
 *
 * @details if vf on docker or any error occured, return -1
 *
 * @return the board id where device is located
 */
int DECLDIR mxSmlGetDeviceBoardId(unsigned int deviceId);

/**
 * @brief This structure holds ETH Rx/Tx throughput(MBytes/s)
 */
typedef struct MxSmlEthThroughput {
    int rx;
    int tx;
} mxSmlEthThroughput_t;

/**
 * @brief Get ETH throughput
 *
 * @param[in] deviceId : the device index
 * @param[out] ethThroughput : return ETH throughput(MBytes/s)
 *
 * @retval MXSML_Success                call was successful
 * @retval MXSML_InvalidDeviceId        deviceId is out of range
 * @retval MXSML_OperationNotSupport    the operation is not support on target device
 * @retval MXSML_InvalidInput           ethThroughput is null
 * @retval MXSML_SysfsError             read sysfs file failure
 */
mxSmlReturn_t DECLDIR mxSmlGetEthThroughput(unsigned int deviceId, mxSmlEthThroughput_t* ethThroughput);

#ifdef __cplusplus
}
#endif

#endif // __MX_SML_H__
