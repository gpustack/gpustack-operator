package amdgpu

import "C"
import (
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

func (l *AMDGPU) Open(cardID uint32) (Device, Return) {
	if l.so.Lookup("amdgpu_device_initialize") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	devPath := filepath.Join("/dev/dri", "card"+strconvx.Itoa(cardID))
	devFile, err := os.Open(devPath)
	if err != nil {
		return Device{}, ERROR_CARD_NOTFOUND
	}

	var (
		majorVersion uint32
		minorVersion uint32
		handle       amdgpuDevice
	)

	ret := Return(amdgpuDeviceInitialize(int32(devFile.Fd()), &majorVersion, &minorVersion, &handle))
	if !ret.IsSuccess() {
		_ = devFile.Close()
		return Device{}, ret
	}

	return Device{devFile: devFile, handle: handle, so: l.so}, SUCCESS
}

type Device struct {
	devFile *os.File
	handle  amdgpuDevice
	so      binding.Library
}

// Free releases the device handle and any resources it holds.
func (l Device) Free() Return {
	if l.so.Lookup("amdgpu_device_deinitialize") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	defer func() {
		if l.devFile != nil {
			_ = l.devFile.Close()
		}
	}()

	ret := Return(amdgpuDeviceDeinitialize(l.handle))
	return ret
}

// QueryGPUInfo retrieves information about the GPU associated with the device handle.
func (l Device) QueryGPUInfo() (GpuInfo, Return) {
	if l.so.Lookup("amdgpu_query_gpu_info") != nil {
		return GpuInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInfo
	ret := Return(amdgpuQueryGpuInfo(l.handle, &info))
	return info, ret
}

// GetMarketingName retrieves the marketing name of the GPU associated with the device handle.
func (l Device) GetMarketingName() string {
	if l.so.Lookup("amdgpu_get_marketing_name") != nil {
		return ""
	}

	return amdgpuGetMarketingName(l.handle)
}
