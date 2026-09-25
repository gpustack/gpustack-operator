// Package metrics holds the node model cache's Prometheus metrics. Their labels never carry a
// namespace, an artifact, a repository or a Pod: those are tenant names, and a node's metrics are
// read across tenants.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The mount results.
const (
	MountHit          = "hit"
	MountMaterialized = "materialized"
	MountDenied       = "denied"
	MountPending      = "pending"
)

var (
	// DownloadBytes counts the bytes received, by source.
	DownloadBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_model_manager_download_bytes_total",
		Help: "Bytes of model files received, by source.",
	}, []string{"source"})

	// Mounts counts mount calls, by result.
	Mounts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_model_manager_mounts_total",
		Help: "Mount calls, by result: hit (published content), materialized (published by this call's " +
			"materialization), denied (authorization refused) or pending (materializing or backing off).",
	}, []string{"result"})

	// Materializations counts finished attempts, by result: published or a failure reason.
	Materializations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpustack_model_manager_materializations_total",
		Help: "Finished materialization attempts, by result: published or the failure's reason.",
	}, []string{"result"})

	// PublishDuration is how long an attempt took from its start to its publication.
	PublishDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "gpustack_model_manager_publish_duration_seconds",
		Help:    "Seconds from a materialization's start to its publication.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 16),
	})

	// GCRemovedBytes counts the bytes collection removed.
	GCRemovedBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gpustack_model_manager_gc_removed_bytes_total",
		Help: "Bytes of model content removed by collection.",
	})

	// StoredBytes and CapacityBytes are the cache's occupied bytes and its filesystem's size.
	StoredBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gpustack_model_manager_stored_bytes",
		Help: "Bytes the node's published trees and partial downloads occupy.",
	})
	CapacityBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gpustack_model_manager_capacity_bytes",
		Help: "Size of the filesystem holding the node's model cache.",
	})
)

// Collectors is every metric above, for registration.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		DownloadBytes, Mounts, Materializations, PublishDuration, GCRemovedBytes, StoredBytes, CapacityBytes,
	}
}
