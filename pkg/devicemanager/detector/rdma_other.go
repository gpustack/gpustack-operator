//go:build !linux

// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package detector

import (
	"errors"

	"gpustack.ai/gpustack/pkg/device"
)

// DetectRDMAInterfaces reports a failure off Linux, where sysfs does not exist. The error flows
// through the same retention path as any other failed read, so a development host behaves like a
// node whose discovery has not succeeded rather than like one with no devices.
func DetectRDMAInterfaces() ([]device.RDMADevice, error) {
	return nil, errors.New("the RDMA device discovery is only available on linux")
}
