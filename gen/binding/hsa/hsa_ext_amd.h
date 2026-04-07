////////////////////////////////////////////////////////////////////////////////
//
// The University of Illinois/NCSA
// Open Source License (NCSA)
//
// Copyright (c) 2014-2025, Advanced Micro Devices, Inc. All rights reserved.
//
// Developed by:
//
//                 AMD Research and AMD HSA Software Development
//
//                 Advanced Micro Devices, Inc.
//
//                 www.amd.com
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to
// deal with the Software without restriction, including without limitation
// the rights to use, copy, modify, merge, publish, distribute, sublicense,
// and/or sell copies of the Software, and to permit persons to whom the
// Software is furnished to do so, subject to the following conditions:
//
//  - Redistributions of source code must retain the above copyright notice,
//    this list of conditions and the following disclaimers.
//  - Redistributions in binary form must reproduce the above copyright
//    notice, this list of conditions and the following disclaimers in
//    the documentation and/or other materials provided with the distribution.
//  - Neither the names of Advanced Micro Devices, Inc,
//    nor the names of its contributors may be used to endorse or promote
//    products derived from this Software without specific prior written
//    permission.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL
// THE CONTRIBUTORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR
// OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
// ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
// DEALINGS WITH THE SOFTWARE.
//
////////////////////////////////////////////////////////////////////////////////

// HSA AMD extension.

#ifndef HSA_RUNTIME_EXT_AMD_H_
#define HSA_RUNTIME_EXT_AMD_H_

#include "hsa.h"

/**
 * - 1.0 - initial version
 * - 1.1 - dmabuf export
 * - 1.2 - hsa_amd_memory_async_copy_on_engine
 * - 1.3 - HSA_AMD_MEMORY_POOL_GLOBAL_FLAG_EXTENDED_SCOPE_FINE_GRAINED pool
 * - 1.4 - Virtual Memory API
 * - 1.5 - hsa_amd_agent_info: HSA_AMD_AGENT_INFO_MEMORY_PROPERTIES
 * - 1.6 - Virtual Memory API: hsa_amd_vmem_address_reserve_align
 * - 1.7 - hsa_amd_signal_wait_all
 * - 1.8 - hsa_amd_memory_get_preferred_copy_engine
 * - 1.9 - hsa_amd_portable_export_dmabuf_v2
 * - 1.10 - hsa_amd_vmem_address_reserve: HSA_AMD_VMEM_ADDRESS_NO_REGISTER
 * - 1.11 - hsa_amd_agent_info_t: HSA_AMD_AGENT_INFO_CLOCK_COUNTERS
 * - 1.12 - hsa_amd_pointer_info: HSA_EXT_POINTER_TYPE_HSA_VMEM and HSA_EXT_POINTER_TYPE_RESERVED_ADDR
 * - 1.13 - hsa_amd_pointer_info: Added new registered field to hsa_amd_pointer_info_t
 * - 1.14 - hsa_amd_ais_file_write, hsa_amd_ais_file_read
 * - 1.15 - hsa_amd_register_system_event_handler: HSA_AMD_SYSTEM_SHUTDOWN
 * - 1.16 - hsa_amd_counted_queue APIs
 * - 1.17 - hsa_amd_memory_async_batch_copy
 * - 1.18 - hsa_amd_pointer_info: Added alloc_flags field to hsa_amd_pointer_info_t
 * - 1.19 - hsa_amd_agent_preload
 */
#define HSA_AMD_INTERFACE_VERSION_MAJOR 1
#define HSA_AMD_INTERFACE_VERSION_MINOR 19

#ifdef __cplusplus
extern "C" {
#endif

/**
 * @brief Agent attributes.
 */
typedef enum hsa_amd_agent_info_s {
  /**
   * Chip identifier. The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_CHIP_ID = 0xA000,
  /**
   * Size of a cacheline in bytes. The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_CACHELINE_SIZE = 0xA001,
  /**
   * The number of compute unit available in the agent. The type of this
   * attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_COMPUTE_UNIT_COUNT = 0xA002,
  /**
   * The maximum clock frequency of the agent in MHz. The type of this
   * attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_MAX_CLOCK_FREQUENCY = 0xA003,
  /**
   * Internal driver node identifier. The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_DRIVER_NODE_ID = 0xA004,
  /**
   * Max number of watch points on memory address ranges to generate exception
   * events when the watched addresses are accessed.  The type of this
   * attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_MAX_ADDRESS_WATCH_POINTS = 0xA005,
  /**
   * Agent BDF_ID, named LocationID in thunk. The type of this attribute is
   * uint32_t.
   */
  HSA_AMD_AGENT_INFO_BDFID = 0xA006,
  /**
   * Memory Interface width, the return value type is uint32_t.
   * This attribute is deprecated.
   */
  HSA_AMD_AGENT_INFO_MEMORY_WIDTH = 0xA007,
  /**
   * Max Memory Clock, the return value type is uint32_t.
   */
  HSA_AMD_AGENT_INFO_MEMORY_MAX_FREQUENCY = 0xA008,
  /**
   * Board name of Agent - populated from MarketingName of Kfd Node
   * The value is an Ascii string of 64 chars.
   */
  HSA_AMD_AGENT_INFO_PRODUCT_NAME = 0xA009,
  /**
   * Maximum number of waves possible in a Compute Unit.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_MAX_WAVES_PER_CU = 0xA00A,
  /**
   * Number of SIMD's per compute unit CU
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_SIMDS_PER_CU = 0xA00B,
  /**
   * Number of Shader Engines (SE) in Gpu
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_SHADER_ENGINES = 0xA00C,
  /**
   * Number of Shader Arrays Per Shader Engines in Gpu
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_SHADER_ARRAYS_PER_SE = 0xA00D,
  /**
   * Address of the HDP flush registers.  Use of these registers does not conform to the HSA memory
   * model and should be treated with caution.
   * The type of this attribute is hsa_amd_hdp_flush_t.
   */
  HSA_AMD_AGENT_INFO_HDP_FLUSH = 0xA00E,
  /**
   * PCIe domain for the agent.  Pairs with HSA_AMD_AGENT_INFO_BDFID
   * to give the full physical location of the Agent.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_DOMAIN = 0xA00F,
  /**
   * Queries for support of cooperative queues.  See ::HSA_QUEUE_TYPE_COOPERATIVE.
   * The type of this attribute is bool.
   */
  HSA_AMD_AGENT_INFO_COOPERATIVE_QUEUES = 0xA010,
  /**
   * Queries UUID of an agent. The value is an Ascii string with a maximum
   * of 21 chars including NUL. The string value consists of two parts: header
   * and body. The header identifies device type (GPU, CPU, DSP) while body
   * encodes UUID as a 16 digit hex string
   *
   * Agents that do not support UUID will return the string "GPU-XX" or
   * "CPU-XX" or "DSP-XX" depending upon their device type ::hsa_device_type_t
   */
  HSA_AMD_AGENT_INFO_UUID = 0xA011,
  /**
   * Queries for the ASIC revision of an agent. The value is an integer that
   * increments for each revision. This can be used by user-level software to
   * change how it operates, depending on the hardware version. This allows
   * selective workarounds for hardware errata.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_ASIC_REVISION = 0xA012,
  /**
   * Queries whether or not the host can directly access SVM memory that is
   * physically resident in the agent's local memory.
   * The type of this attribute is bool.
   */
  HSA_AMD_AGENT_INFO_SVM_DIRECT_HOST_ACCESS = 0xA013,
  /**
   * Some processors support more CUs than can reliably be used in a cooperative
   * dispatch.  This queries the count of CUs which are fully enabled for
   * cooperative dispatch.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_COOPERATIVE_COMPUTE_UNIT_COUNT = 0xA014,
  /**
   * Queries the amount of memory available in bytes accross all global pools
   * owned by the agent.
   * The type of this attribute is uint64_t.
   */
  HSA_AMD_AGENT_INFO_MEMORY_AVAIL = 0xA015,
  /**
   * Timestamp value increase rate, in Hz. The timestamp (clock) frequency is
   * in the range 1-400MHz.
   * The type of this attribute is uint64_t.
   */
  HSA_AMD_AGENT_INFO_TIMESTAMP_FREQUENCY = 0xA016,
  /**
   * Queries for the ASIC family ID of an agent.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_ASIC_FAMILY_ID = 0xA107,
  /**
   * Queries for the Packet Processor(CP Firmware) ucode version of an agent.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_UCODE_VERSION = 0xA108,
  /**
   * Queries for the SDMA engine ucode of an agent.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_SDMA_UCODE_VERSION = 0xA109,
  /**
   * Queries the number of SDMA engines.
   * If HSA_AMD_AGENT_INFO_NUM_SDMA_XGMI_ENG query returns non-zero,
   * this query returns the the number of SDMA engines optimized for
   * host to device bidirectional traffic.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_SDMA_ENG = 0xA10A,
  /**
   * Queries the number of additional SDMA engines optimized for D2D xGMI copies.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_SDMA_XGMI_ENG = 0xA10B,
  /**
   * Queries for version of IOMMU supported by agent.
   * The type of this attribute is hsa_amd_iommu_version_t.
   */
  HSA_AMD_AGENT_INFO_IOMMU_SUPPORT = 0xA110,
  /**
   * Queries for number of XCCs within the agent.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_NUM_XCC = 0xA111,
  /**
   * Queries for driver unique identifier.
   * The type of this attribute is uint32_t.
   */
  HSA_AMD_AGENT_INFO_DRIVER_UID = 0xA112,
  /**
   * Returns the hsa_agent_t of the nearest CPU agent
   * The type of this attribute is hsa_agent_t.
   */
  HSA_AMD_AGENT_INFO_NEAREST_CPU = 0xA113,
  /**
   * Bit-mask indicating memory properties of this agent. A memory property is set if the flag bit
   * is set at that position. User may use the hsa_flag_isset64 macro to verify whether a flag
   * is set. The type of this attribute is uint8_t[8].
   */
  HSA_AMD_AGENT_INFO_MEMORY_PROPERTIES = 0xA114,
  /**
   * Bit-mask indicating AQL Extensions supported by this agent. An AQL extension is set if the flag
   * bit is set at that position. User may use the hsa_flag_isset64 macro to verify whether a flag
   * is set. The type of this attribute is uint8_t[8].
   */
  HSA_AMD_AGENT_INFO_AQL_EXTENSIONS = 0xA115, /* Not implemented yet */
  /**
   * Maximum allowed value in bytes for scratch limit for this agent. This amount
   * is shared accross all queues created on this agent.
   * The type of this attribute is uint64_t.
   */
  HSA_AMD_AGENT_INFO_SCRATCH_LIMIT_MAX = 0xA116,
  /**
   * Current scratch limit threshold in bytes for this agent. This limit can be
   * modified using the hsa_amd_agent_set_async_scratch_limit call.
   * - AQL dispatches that require scratch-memory above this threshold will trigger a
   *   scratch use-once.
   * - AQL dispatches using less scratch-memory than this threshold, ROCr will
   *   permanently assign the allocated scratch memory to the queue handling the dispatch.
   *   This memory can be reclaimed by calling hsa_amd_agent_set_async_scratch_limit
   *   with a lower threshold by current value.
   *
   * The type of this attribute is uint64_t.
   */
  HSA_AMD_AGENT_INFO_SCRATCH_LIMIT_CURRENT = 0xA117,
  /**
   * Queries the driver for clock counters of the agent.
   * The type of this attribute is hsa_amd_clock_counters_t.
   */
  HSA_AMD_AGENT_INFO_CLOCK_COUNTERS = 0xA118,
  /**
   * The agent uses PM4 emulation mode.
   */
  HSA_AMD_AGENT_INFO_PM4_EMULATION = 0xA119,
  /**
   * Queries for the LUID that identifies a hardware node. The LUID is only
   * valid on Windows. The type of this attribute is LUID.
   */
  HSA_AMD_AGENT_INFO_LUID = 0xA11A,
  /**
   * The agent supports expert scheduling mode. The type of this attribute is bool.
   */
  HSA_AMD_AGENT_INFO_HAS_EXPERT_SCHED_MODE = 0xA11B,
  /**
   * Queries the secondary CUID (128-bit UUID (16 bytes) in UUIDv8 format)
   * of a CPU/GPU agent. The type of this attribute is uint8_t[16].
   */
  HSA_AMD_AGENT_INFO_CUID = 0xA11C,
} hsa_amd_agent_info_t;


/** @} */

#ifdef __cplusplus
}  // end extern "C" block
#endif

#endif  // header guard
