package gc

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// kubelet's defaults, the ones the cap assumes for a threshold its configuration file does not set.
const (
	defaultNodefsAvailablePercent  = 10
	defaultImagefsAvailablePercent = 15
	defaultImageGCHighPercent      = 85
	// watermarkMargin is the room the cap keeps below each kubelet threshold.
	watermarkMargin = 5
)

// kubeletConfig is the part of kubelet's configuration file the cap reads.
type kubeletConfig struct {
	EvictionHard                map[string]string `json:"evictionHard"`
	ImageGCHighThresholdPercent *int32            `json:"imageGCHighThresholdPercent"`
}

// Cap is the highest high watermark a cache sharing kubelet's filesystem may use, and where its
// thresholds came from.
type Cap struct {
	Percent int32
	Source  string
}

// KubeletCap reads kubelet's eviction and image collection thresholds from its configuration file
// at path and returns the high watermark the cache may use on kubelet's filesystem: the lowest of
// 100 - nodefs.available - 5, 100 - imagefs.available - 5 and imageGCHighThresholdPercent - 5. The
// image thresholds count because the plugin cannot see whether the image store shares the
// filesystem, so it assumes it does. A threshold the file does not set, a missing file and one that
// cannot be read take kubelet's default. Flags that override the file are not seen.
func KubeletCap(path string, totalBytes uint64) Cap {
	nodefs, imagefs, gcHigh := int64(defaultNodefsAvailablePercent), int64(defaultImagefsAvailablePercent),
		int64(defaultImageGCHighPercent)
	source := "kubelet's default thresholds"

	b, err := os.ReadFile(path)
	var cfg kubeletConfig
	switch {
	case errors.Is(err, fs.ErrNotExist):
		source += " (no kubelet configuration file at " + path + ")"
	case err != nil, yaml.Unmarshal(b, &cfg) != nil:
		source += " (the kubelet configuration file at " + path + " cannot be read)"
	default:
		source = "the kubelet configuration file at " + path
		if v, ok := availablePercent(cfg.EvictionHard["nodefs.available"], totalBytes); ok {
			nodefs = v
		}
		if v, ok := availablePercent(cfg.EvictionHard["imagefs.available"], totalBytes); ok {
			imagefs = v
		}
		if cfg.ImageGCHighThresholdPercent != nil {
			gcHigh = int64(*cfg.ImageGCHighThresholdPercent)
		}
	}

	c := min(100-nodefs-watermarkMargin, 100-imagefs-watermarkMargin, gcHigh-watermarkMargin)

	return Cap{Percent: int32(max(c, 2)), Source: source} // nolint: gosec // within 2..95.
}

// availablePercent reads an eviction threshold, a percentage or a quantity of bytes, as a
// percentage of the filesystem, rounded up.
func availablePercent(v string, totalBytes uint64) (int64, bool) {
	if v == "" {
		return 0, false
	}
	if p, ok := strings.CutSuffix(v, "%"); ok {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || f > 100 {
			return 0, false
		}
		return int64(math.Ceil(f)), true
	}
	q, err := resource.ParseQuantity(v)
	if err != nil || totalBytes == 0 {
		return 0, false
	}

	return int64(math.Ceil(q.AsApproximateFloat64() / float64(totalBytes) * 100)), true
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
