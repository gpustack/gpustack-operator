package hgml

import (
	"sync"
	"time"
)

// GpmQueryDeviceSupportV retrieves the support information for GPU performance metrics (GPM) on the device,
// returning a handler that can be used to access different versions of the GPM support information.
func (l Device) GpmQueryDeviceSupportV() GpmSupportHandler {
	return GpmSupportHandler(l)
}

type GpmSupportHandler Device

func (l GpmSupportHandler) V1() (GpmSupport, Return) {
	if l.so.Lookup("hgmlGpmQueryDeviceSupport") != nil {
		return GpmSupport{}, ERROR_FUNCTION_NOT_FOUND
	}

	var gpmSupport GpmSupport
	gpmSupport.Version = GPM_SUPPORT_VERSION
	ret := hgmlGpmQueryDeviceSupport(l.handle, &gpmSupport)
	return gpmSupport, ret
}

// GpmMetricsGetV retrieves the performance metrics of the device for GPU performance monitoring (GPM),
// returning a handler that can be used to access different versions of the GPM metrics information.
func (l Device) GpmMetricsGetV() GpmMetricsGetHandler {
	return GpmMetricsGetHandler{l, false}
}

// GpmMetricsGetV retrieves the performance metrics of the device for GPU performance monitoring (GPM),
// returning a handler that can be used to access different versions of the GPM metrics information.
func (l _MigDevice) GpmMetricsGetV() GpmMetricsGetHandler {
	return GpmMetricsGetHandler{l.Device, true}
}

type GpmMetricsGetHandler struct {
	Device
	isMig bool
}

var gpmLock sync.Mutex

func (l GpmMetricsGetHandler) V1(duration time.Duration, metricIds ...GpmMetricId) ([]GpmMetric, Return) {
	if l.so.Lookup("hgmlGpmMetricsGet") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	var gpmSample1 hgmlGpmSample
	ret := hgmlGpmSampleAlloc(&gpmSample1)
	if !ret.IsSuccess() {
		return nil, ret
	}
	defer func() {
		_ = hgmlGpmSampleFree(gpmSample1)
	}()

	var gpmSample2 hgmlGpmSample
	ret = hgmlGpmSampleAlloc(&gpmSample2)
	if !ret.IsSuccess() {
		return nil, ret
	}
	defer func() {
		_ = hgmlGpmSampleFree(gpmSample2)
	}()

	ret = func() Return {
		gpmLock.Lock()
		defer gpmLock.Unlock()

		if l.isMig {
			var gpuInstanceId uint32
			ret = hgmlDeviceGetGpuInstanceId(l.handle, &gpuInstanceId)
			if !ret.IsSuccess() {
				return ret
			}

			ret = hgmlGpmMigSampleGet(l.handle, gpuInstanceId, gpmSample1)
			if ret.IsSuccess() {
				time.Sleep(duration)
				ret = hgmlGpmMigSampleGet(l.handle, gpuInstanceId, gpmSample2)
			}
			return ret
		}

		ret = hgmlGpmSampleGet(l.handle, gpmSample1)
		if ret.IsSuccess() {
			time.Sleep(duration)
			ret = hgmlGpmSampleGet(l.handle, gpmSample2)
		}
		return ret
	}()
	if !ret.IsSuccess() {
		return nil, ret
	}

	var gpmMetrics hgmlGpmMetricsGetType
	gpmMetrics.Version = STRUCT_VERSION(gpmMetrics, 1)
	gpmMetrics.NumMetrics = uint32(len(metricIds))
	for i := range metricIds {
		gpmMetrics.Metrics[i].MetricId = uint32(metricIds[i])
	}
	gpmMetrics.Sample1 = gpmSample1
	gpmMetrics.Sample2 = gpmSample2

	ret = hgmlGpmMetricsGet(&gpmMetrics)
	return gpmMetrics.Metrics[:], ret
}
