//go:build linux

// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package detector

import "gpustack.ai/gpustack/pkg/device"

// DetectRDMAInterfaces enumerates this node's host verbs devices from the real sysfs root. The
// root is a constant here and a parameter everywhere else, which is what lets the pass run
// against a fixture off Linux — see enumerateRDMAInterfaces.
func DetectRDMAInterfaces() ([]device.RDMADevice, error) {
	return enumerateRDMAInterfaces(sysfsRoot)
}
