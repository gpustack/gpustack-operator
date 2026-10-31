// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package rsmi

/*
typedef struct metrics_table_header_t metrics_table_header_t;
#include "rocm_smi.h"
*/
import "C"

// MAX_NUM_POWER_PROFILES is the number of bits in the SDK power-profile mask.
const MAX_NUM_POWER_PROFILES = C.RSMI_MAX_NUM_POWER_PROFILES
