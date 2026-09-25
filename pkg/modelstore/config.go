// Package modelstore holds a node model cache's effective configuration: the check of a
// NodeModelStoreSpec and the layered merge that produces one.
//
// The worker merges and checks before it writes a node's spec, and the model-manager plugin checks
// again before it uses one, because a hand edit of the object bypasses the worker. Both import this
// package, so the two checks cannot drift apart.
package modelstore

import (
	"errors"
	"fmt"
	"net/url"

	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelartifact"
)

// DriverName is the CSI driver the model-manager plugin registers, and the name of its CSIDriver
// object. A consumer renders node delivery only while that object exists.
const DriverName = "model.csi.gpustack.ai"

// The volume attributes a node-delivered volume names: the consumer's controller renders them and
// the node plugin reads them, so both take them from here.
const (
	VolumeAttrArtifact       = "artifact"
	VolumeAttrArtifactUID    = "artifactUID"
	VolumeAttrManifestDigest = "manifestDigest"
)

// The bounds a spec's numbers must keep.
const (
	MaxHighWatermarkPercent = 95
	MinLowWatermarkPercent  = 1
	MinDownloadConcurrency  = 1
	MaxDownloadConcurrency  = 64
)

// Layer is one source of configuration. A nil field leaves in place what the layers below it set,
// so a narrower layer (a node pool's) overrides only the fields it names.
type Layer struct {
	HighWatermarkPercent   *int32
	LowWatermarkPercent    *int32
	DownloadConcurrency    *int32
	DownloadBytesPerSecond *int64
	HuggingFaceEndpoint    *string
	HTTPSProxy             *string
	NoProxy                *string
	CABundleConfigMap      *string
}

// Merge overlays the layers in order, each over the ones before it, field by field.
func Merge(layers ...Layer) workercore.NodeModelStoreSpec {
	var spec workercore.NodeModelStoreSpec
	for _, l := range layers {
		overlay(&spec.Watermarks.HighPercent, l.HighWatermarkPercent)
		overlay(&spec.Watermarks.LowPercent, l.LowWatermarkPercent)
		overlay(&spec.Download.Concurrency, l.DownloadConcurrency)
		overlay(&spec.Download.BytesPerSecond, l.DownloadBytesPerSecond)
		overlay(&spec.Hub.HuggingFaceEndpoint, l.HuggingFaceEndpoint)
		overlay(&spec.Hub.HTTPSProxy, l.HTTPSProxy)
		overlay(&spec.Hub.NoProxy, l.NoProxy)
		overlay(&spec.Hub.CABundleConfigMap, l.CABundleConfigMap)
	}

	return spec
}

func overlay[T any](dst, src *T) {
	if src != nil {
		*dst = *src
	}
}

// Validate reports every rule a spec breaks, joined, or nil.
//
// A value that fails here is never used: the worker does not write it and the plugin does not
// download with it, rather than fall back to a value nobody chose.
func Validate(spec workercore.NodeModelStoreSpec) error {
	var errs []error

	w := spec.Watermarks
	if w.LowPercent < MinLowWatermarkPercent || w.HighPercent > MaxHighWatermarkPercent || w.LowPercent >= w.HighPercent {
		errs = append(errs, fmt.Errorf("watermarks: want %d <= low < high <= %d, got low %d and high %d",
			MinLowWatermarkPercent, MaxHighWatermarkPercent, w.LowPercent, w.HighPercent))
	}

	d := spec.Download
	if d.Concurrency < MinDownloadConcurrency || d.Concurrency > MaxDownloadConcurrency {
		errs = append(errs, fmt.Errorf("download concurrency: want %d to %d, got %d",
			MinDownloadConcurrency, MaxDownloadConcurrency, d.Concurrency))
	}
	if d.BytesPerSecond < 0 {
		errs = append(errs, fmt.Errorf("download bandwidth: want 0 or more bytes per second, got %d", d.BytesPerSecond))
	}

	h := spec.Hub
	if err := ValidateEndpoint(h.HuggingFaceEndpoint); err != nil {
		errs = append(errs, fmt.Errorf("hugging face endpoint: %w", err))
	}
	if h.HTTPSProxy != "" {
		if err := modelartifact.ValidateProxy(h.HTTPSProxy); err != nil {
			errs = append(errs, fmt.Errorf("https proxy: %w", err))
		}
	}

	return errors.Join(errs...)
}

// ValidateEndpoint accepts an absolute http or https URL with a host. The endpoint Setting admits
// through it, so a value a node would refuse never reaches the Settings store by an API write.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http or https URL with a host", raw)
	}

	return nil
}

// ParseBandwidth reads a download bandwidth, a non-negative quantity of bytes per second such as
// "200Mi"; "0" is unlimited.
func ParseBandwidth(raw string) (int64, error) {
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("parse bandwidth %q: %w", raw, err)
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("bandwidth %q is negative", raw)
	}
	v, ok := q.AsInt64()
	if !ok {
		return 0, fmt.Errorf("bandwidth %q is not a whole number of bytes", raw)
	}

	return v, nil
}
