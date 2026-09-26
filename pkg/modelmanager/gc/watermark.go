package gc

import (
	"fmt"
	"math"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// kubelet's defaults, the ones the cap assumes for a threshold kubelet does not set.
const (
	defaultNodefsAvailablePercent  = 10
	defaultImagefsAvailablePercent = 15
	defaultImageGCHighPercent      = 85
	// watermarkMargin is the room the cap keeps below each kubelet threshold.
	watermarkMargin = 5
)

// Cap is the highest high watermark a cache sharing kubelet's filesystem may use, and where its
// thresholds came from.
type Cap struct {
	Percent int32
	Source  string
	// Ignored says which thresholds were not used and why, empty when all were: a reader needs it
	// whether or not the cap lowers the watermark, since kubelet enforces them all the same.
	Ignored string
}

// KubeletCap returns the high watermark the cache may use on kubelet's filesystem from the node's
// effective kubelet thresholds, the ones the worker read from kubelet's configz into the node's
// spec: the lowest of 100 - nodefs.available - 5, 100 - imagefs.available - 5 and
// imageGCHighThresholdPercent - 5. The image thresholds count because the plugin cannot see whether
// the image store shares the filesystem, so it assumes it does. A threshold kubelet does not set
// takes kubelet's default, and so does every threshold when k is nil: the worker could not read the
// node's effective configuration, and the source says so.
func KubeletCap(k *workercore.NodeModelStoreKubelet, totalBytes uint64) Cap {
	nodefs, imagefs, gcHigh := int64(defaultNodefsAvailablePercent), int64(defaultImagefsAvailablePercent),
		int64(defaultImageGCHighPercent)

	source := "kubelet's default thresholds, since the node's effective kubelet configuration could not be read"
	var ignored string
	if k != nil {
		source = "the node's effective kubelet configuration"
		var notes []string
		for _, t := range []struct {
			signal, value string
			dst           *int64
		}{{"nodefs.available", k.NodefsAvailable, &nodefs}, {"imagefs.available", k.ImagefsAvailable, &imagefs}} {
			v, ok, note := availablePercent(t.signal, t.value, totalBytes)
			if ok {
				*t.dst = v
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
		if k.ImageGCHighThresholdPercent != nil {
			gcHigh = int64(*k.ImageGCHighThresholdPercent)
		}
		if len(notes) > 0 {
			ignored = strings.Join(notes, "; ")
			source += " (" + ignored + ")"
		}
	}

	c := min(100-nodefs-watermarkMargin, 100-imagefs-watermarkMargin, gcHigh-watermarkMargin)

	return Cap{Percent: int32(max(c, 2)), Source: source, Ignored: ignored} // nolint: gosec // within 2..95.
}

// availablePercent reads an eviction threshold, a percentage or a quantity of bytes, as a
// percentage of the filesystem, rounded up. A quantity at or above the filesystem's size cannot be
// a share of it: it is not used, so kubelet's default applies instead of a cap driven to its floor,
// and the note says so.
func availablePercent(signal, v string, totalBytes uint64) (int64, bool, string) {
	if v == "" {
		return 0, false, ""
	}
	t, err := modelstore.ParseThreshold(v)
	switch {
	case err != nil:
		return 0, false, fmt.Sprintf("%s %s cannot be read, so kubelet's default applies", signal, v)
	case t.IsPercent:
		return int64(math.Ceil(t.Percent)), true, ""
	case totalBytes == 0:
		return 0, false, ""
	case uint64(max(t.Bytes, 0)) >= totalBytes: // nolint: gosec // bounded below by 0.
		return 0, false, fmt.Sprintf("%s %s is not below the filesystem's %d bytes, so kubelet's default applies",
			signal, v, totalBytes)
	}

	return int64(math.Ceil(float64(t.Bytes) / float64(totalBytes) * 100)), true, ""
}

// Effective applies the cap to a spec's watermarks: the high one no higher than the cap, and the low
// one below the high one.
func Effective(high, low int32, c *Cap) (int32, int32, string) {
	if c == nil || high <= c.Percent {
		return high, low, ""
	}
	h := c.Percent

	return h, min(low, h-1), fmt.Sprintf("the high watermark %d%% is capped at %d%% on the filesystem the cache "+
		"shares with kubelet, from %s", high, h, c.Source)
}
