/* Copyright(C) 2026. Huawei Technologies Co.,Ltd. All rights reserved.
   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/* The DCMI V2 entry points, as served by the A5/950 generation of the driver.
   Transcribed from the vendor header of the same name, keeping its declaration order so a diff
   against a newer vendor drop stays readable.

   Two curation notes:

   - This header is only ever included from dcmi_wrapper.h, immediately after
     dcmi_interface_api.h. That is deliberate and required: every struct and enum below is
     declared by the V1 header -- V2 introduces no type of its own -- and it is the V1 header that
     defines both DCMI_VERSION_2, which satisfies the guard here, and DCMIDLLEXPORT. The vendor
     copy redefines DCMIDLLEXPORT identically; that line is dropped rather than repeated.

   - dcmiv2_subscribe_fault_event is declared here because the vendor declares it, but it is
     deliberately absent from DCMI_V2_API_LIST in dcmi_wrapper_api.def: the vendor's own function
     pointer for it takes a third parameter this declaration does not have, so the two sources
     disagree about its binary ABI. See the note in that file. */

#ifndef __DCMI_INTERFACE_API_V2_H__
#define __DCMI_INTERFACE_API_V2_H__

#ifdef __cplusplus
#if __cplusplus
extern "C" {
#endif
#endif /* __cplusplus */

#if defined DCMI_VERSION_2

DCMIDLLEXPORT int dcmiv2_init(void);

DCMIDLLEXPORT int dcmiv2_get_device_list(int *device_list, int *device_num, int list_len);

DCMIDLLEXPORT int dcmiv2_get_all_device_count(int *all_device_count);

DCMIDLLEXPORT int dcmiv2_get_device_type(int dev_id, enum dcmi_unit_type *device_type);

DCMIDLLEXPORT int dcmiv2_get_device_pcie_info(int dev_id, struct dcmi_pcie_info_all *pcie_info);

DCMIDLLEXPORT int dcmiv2_get_device_chip_info(int dev_id, struct dcmi_chip_info_v2 *chip_info);

DCMIDLLEXPORT int dcmiv2_get_device_power_info(int dev_id, int *power);

DCMIDLLEXPORT int dcmiv2_get_device_health(int dev_id, unsigned int *health);

DCMIDLLEXPORT int dcmiv2_get_device_error_code_list(int dev_id, int *error_count, unsigned int *error_code_list,
    unsigned int list_len);

DCMIDLLEXPORT int dcmiv2_get_device_temperature(int dev_id, int *temperature);

DCMIDLLEXPORT int dcmiv2_get_device_voltage(int dev_id, unsigned int *voltage);

DCMIDLLEXPORT int dcmiv2_get_device_ecc_info(int dev_id, enum dcmi_device_type input_type,
    struct dcmi_ecc_info *device_ecc_info);

DCMIDLLEXPORT int dcmiv2_get_device_frequency(int dev_id, enum dcmi_freq_type input_type, unsigned int *frequency);

DCMIDLLEXPORT int dcmiv2_get_device_hbm_info(int dev_id, struct dcmi_hbm_info *hbm_info);

DCMIDLLEXPORT int dcmiv2_get_device_utilization_rate(int dev_id, int input_type, unsigned int *utilization_rate);

DCMIDLLEXPORT int dcmiv2_get_device_multi_utilization_rate(int dev_id, struct dcmi_multi_utilization_info *util_info);

DCMIDLLEXPORT int dcmiv2_get_device_multi_utilization_rate_period(int dev_id,
    struct dcmi_multi_utilization_info *util_info, int period);

DCMIDLLEXPORT int dcmiv2_get_device_info(int dev_id, enum dcmi_main_cmd main_cmd, unsigned int sub_cmd, void *buf,
    unsigned int *size);

DCMIDLLEXPORT int dcmiv2_get_device_network_health(int dev_id, enum dcmi_rdfx_detect_result *result);

DCMIDLLEXPORT int dcmiv2_get_chip_phy_id_by_dev_id(unsigned int dev_id, unsigned int *phyid);

DCMIDLLEXPORT int dcmiv2_get_dev_id_by_chip_phy_id(unsigned int phyid, unsigned int *dev_id);

DCMIDLLEXPORT int dcmiv2_reset_device(int dev_id, enum dcmi_reset_channel channel_type);

DCMIDLLEXPORT int dcmiv2_get_device_outband_channel_state(int dev_id, int *channel_state);

DCMIDLLEXPORT int dcmiv2_pre_reset_device(int dev_id);

DCMIDLLEXPORT int dcmiv2_rescan_device(int dev_id);

DCMIDLLEXPORT int dcmiv2_get_device_boot_status(int dev_id, enum dcmi_boot_status *boot_status);

DCMIDLLEXPORT int dcmiv2_subscribe_fault_event(int dev_id, struct dcmi_event_filter filter);

DCMIDLLEXPORT int dcmiv2_get_device_die_id(int dev_id, enum dcmi_die_type input_type, struct dcmi_die_id *die_id);

DCMIDLLEXPORT int dcmiv2_get_device_proc_mem_info(int dev_id, struct dcmi_proc_mem_info *proc_info, int *proc_num);

DCMIDLLEXPORT int dcmiv2_get_device_board_info(int dev_id, struct dcmi_board_info *board_info);

DCMIDLLEXPORT int dcmiv2_get_pcie_link_bandwidth_info(int dev_id,
    struct dcmi_pcie_link_bandwidth_info *pcie_link_bandwidth_info);

DCMIDLLEXPORT int dcmiv2_get_dcmi_version(char *dcmi_ver, int buf_size);

DCMIDLLEXPORT int dcmiv2_get_mainboard_id(int dev_id, unsigned int *mainboard_id);

DCMIDLLEXPORT int dcmiv2_get_affinity_cpu_info_by_dev_id(int dev_id, char *affinity_cpu, int *len);

DCMIDLLEXPORT int dcmiv2_start_ub_ping_mesh(int dev_id, int count, struct dcmi_ub_ping_mesh_operate *ubping_mesh);

DCMIDLLEXPORT int dcmiv2_stop_ub_ping_mesh(int dev_id, int task_id);

DCMIDLLEXPORT int dcmiv2_get_ub_ping_mesh_info(int dev_id, int task_id,
    struct dcmi_ub_ping_mesh_info *ub_ping_mesh_reply, int mesh_reply_size, int *count);

DCMIDLLEXPORT int dcmiv2_get_ub_ping_mesh_state(int dev_id, int task_id, unsigned int *state);

DCMIDLLEXPORT int dcmiv2_get_urma_device_cnt(int dev_id, unsigned int *dev_cnt);

DCMIDLLEXPORT int dcmiv2_get_eid_list_by_urma_dev_index(int dev_id, unsigned int dev_index,
    dcmi_urma_eid_info_t *eid_list, unsigned int *eid_cnt);

DCMIDLLEXPORT int dcmiv2_get_device_elabel_info(int dev_id, struct dcmi_elabel_info *elabel_info);

DCMIDLLEXPORT int dcmiv2_get_port_pkt_stats_info(int dev_id, struct dcmi_ub_port_info *ub_port_info,
    struct dcmi_port_pkt_stats_info *port_pkt_stats_info);
#endif

#ifdef __cplusplus
#if __cplusplus
}
#endif
#endif /* __cplusplus */

#endif /* __DCMI_INTERFACE_API_V2_H__ */
