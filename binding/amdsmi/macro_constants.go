// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package amdsmi

/*
#include "amdsmi.h"
*/
import "C"

// LIB_VERSION_STRING is the SDK version expanded by the C preprocessor.
const LIB_VERSION_STRING = C.AMDSMI_LIB_VERSION_STRING
