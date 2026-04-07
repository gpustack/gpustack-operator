/*
 * Copyright 2014 Advanced Micro Devices, Inc.
 *
 * Permission is hereby granted, free of charge, to any person obtaining a
 * copy of this software and associated documentation files (the "Software"),
 * to deal in the Software without restriction, including without limitation
 * the rights to use, copy, modify, merge, publish, distribute, sublicense,
 * and/or sell copies of the Software, and to permit persons to whom the
 * Software is furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL
 * THE COPYRIGHT HOLDER(S) OR AUTHOR(S) BE LIABLE FOR ANY CLAIM, DAMAGES OR
 * OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
 * ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
 * OTHER DEALINGS IN THE SOFTWARE.
 *
 */

/**
 * \file amdgpu.h
 *
 * Declare public libdrm_amdgpu API
 *
 * This file define API exposed by libdrm_amdgpu library.
 * User wanted to use libdrm_amdgpu functionality must include
 * this file.
 *
 */
#ifndef _AMDGPU_H_
#define _AMDGPU_H_

#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#include <cstdint>
#else  // __cplusplus
#include <stdint.h>
#endif  // __cplusplus

/**
 * Define opaque pointer to context associated with fd.
 * This context will be returned as the result of
 * "initialize" function and should be pass as the first
 * parameter to any API call
 */
//typedef struct amdgpu_device *amdgpu_device_handle;
typedef struct
{
    struct amdgpu_device* handle;
} amdgpu_device_handle;

struct amdgpu_gpu_info {
	/** Asic id */
	uint32_t asic_id;
	/** Chip revision */
	uint32_t chip_rev;
	/** Chip external revision */
	uint32_t chip_external_rev;
	/** Family ID */
	uint32_t family_id;
	/** Special flags */
	uint64_t ids_flags;
	/** max engine clock*/
	uint64_t max_engine_clk;
	/** max memory clock */
	uint64_t max_memory_clk;
	/** number of shader engines */
	uint32_t num_shader_engines;
	/** number of shader arrays per engine */
	uint32_t num_shader_arrays_per_engine;
	/**  Number of available good shader pipes */
	uint32_t avail_quad_shader_pipes;
	/**  Max. number of shader pipes.(including good and bad pipes  */
	uint32_t max_quad_shader_pipes;
	/** Number of parameter cache entries per shader quad pipe */
	uint32_t cache_entries_per_quad_pipe;
	/**  Number of available graphics context */
	uint32_t num_hw_gfx_contexts;
	/** Number of render backend pipes */
	uint32_t rb_pipes;
	/**  Enabled render backend pipe mask */
	uint32_t enabled_rb_pipes_mask;
	/** Frequency of GPU Counter */
	uint32_t gpu_counter_freq;
	/** CC_RB_BACKEND_DISABLE.BACKEND_DISABLE per SE */
	uint32_t backend_disable[4];
	/** Value of MC_ARB_RAMCFG register*/
	uint32_t mc_arb_ramcfg;
	/** Value of GB_ADDR_CONFIG */
	uint32_t gb_addr_cfg;
	/** Values of the GB_TILE_MODE0..31 registers */
	uint32_t gb_tile_mode[32];
	/** Values of GB_MACROTILE_MODE0..15 registers */
	uint32_t gb_macro_tile_mode[16];
	/** Value of PA_SC_RASTER_CONFIG register per SE */
	uint32_t pa_sc_raster_cfg[4];
	/** Value of PA_SC_RASTER_CONFIG_1 register per SE */
	uint32_t pa_sc_raster_cfg1[4];
	/* CU info */
	uint32_t cu_active_number;
	uint32_t cu_ao_mask;
	uint32_t cu_bitmap[4][4];
	/* video memory type info*/
	uint32_t vram_type;
	/* video memory bit width*/
	uint32_t vram_bit_width;
	/** constant engine ram size*/
	uint32_t ce_ram_size;
	/* vce harvesting instance */
	uint32_t vce_harvest_config;
	/* PCI revision ID */
	uint32_t pci_rev_id;
};

/**
 *
 * \param   fd            - \c [in]  File descriptor for AMD GPU device
 *                                   received previously as the result of
 *                                   e.g. drmOpen() call.
 *                                   For legacy fd type, the DRI2/DRI3
 *                                   authentication should be done before
 *                                   calling this function.
 * \param   major_version - \c [out] Major version of library. It is assumed
 *                                   that adding new functionality will cause
 *                                   increase in major version
 * \param   minor_version - \c [out] Minor version of library
 * \param   device_handle - \c [out] Pointer to opaque context which should
 *                                   be passed as the first parameter on each
 *                                   API call
 *
 *
 * \return   0 on success\n
 *          <0 - Negative POSIX Error code
 *
 *
 * \sa amdgpu_device_deinitialize()
*/
int amdgpu_device_initialize(int fd,
			     uint32_t *major_version,
			     uint32_t *minor_version,
			     amdgpu_device_handle *device_handle);

/**
 *
 * When access to such library does not needed any more the special
 * function must be call giving opportunity to clean up any
 * resources if needed.
 *
 * \param   device_handle - \c [in]  Context associated with file
 *                                   descriptor for AMD GPU device
 *                                   received previously as the
 *                                   result e.g. of drmOpen() call.
 *
 * \return  0 on success\n
 *         <0 - Negative POSIX Error code
 *
 * \sa amdgpu_device_initialize()
 *
*/
int amdgpu_device_deinitialize(amdgpu_device_handle device_handle);

/**
 * Query GPU H/w Info
 *
 * Query hardware specific information
 *
 * \param   dev  - \c [in] Device handle. See #amdgpu_device_initialize()
 * \param   heap - \c [in] Heap type
 * \param   info - \c [in] Pointer to structure to get needed information
 *
 * \return   0 on success\n
 *          <0 - Negative POSIX Error code
 *
*/
int amdgpu_query_gpu_info(amdgpu_device_handle dev,
			   struct amdgpu_gpu_info *info);

/**
 *  Get the ASIC marketing name
 *
 * \param   dev         - \c [in] Device handle. See #amdgpu_device_initialize()
 *
 * \return  the constant string of the marketing name
 *          "NULL" means the ASIC is not found
*/
const char *amdgpu_get_marketing_name(amdgpu_device_handle dev);

#ifdef __cplusplus
}
#endif  // __cplusplus

#endif  // _AMDGPU_H_