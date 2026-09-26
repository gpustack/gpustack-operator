package modelstore

import (
	"sort"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The model-manager plugin's Pods, as the chart labels them, and the name of their secure port.
const (
	PluginComponent      = "model-manager"
	PluginSecurePortName = "https"
	// DefaultPluginSecurePort is the chart's default, for a Pod whose port is unnamed.
	DefaultPluginSecurePort = 32444
)

// Aggregate is where one content is across nodes: the counts by state, and the downloading nodes'
// progress. Every node holds, or downloads, a whole copy.
type Aggregate struct {
	Ready, Downloading, Failed int32
	// DownloadedBytes sums what the downloading nodes hold.
	DownloadedBytes int64
	// FailureReasons counts the failed nodes by reason.
	FailureReasons map[string]int32

	// fractions sums each downloading node's downloaded share of its size.
	fractions float64
}

// NodeEntries returns each node's entry for digest, by node name, from the stores that list it.
func NodeEntries(stores []workercore.NodeModelStore, digest string) map[string]workercore.NodeModelStoreModel {
	entries := map[string]workercore.NodeModelStoreModel{}
	for i := range stores {
		for _, m := range stores[i].Status.Models {
			if m.Digest == digest {
				entries[stores[i].Name] = m
			}
		}
	}

	return entries
}

// AggregateEntries counts the entries of one content across nodes.
func AggregateEntries(entries map[string]workercore.NodeModelStoreModel) Aggregate {
	a := Aggregate{FailureReasons: map[string]int32{}}
	for _, m := range entries {
		switch m.State {
		case workercore.NodeModelStoreModelStateReady:
			a.Ready++
		case workercore.NodeModelStoreModelStateFailed:
			a.Failed++
			a.FailureReasons[m.Reason]++
		case workercore.NodeModelStoreModelStateDownloading:
			a.Downloading++
			a.DownloadedBytes += m.DownloadedBytes
			if m.SizeBytes > 0 {
				a.fractions += min(float64(m.DownloadedBytes)/float64(m.SizeBytes), 1)
			}
		}
	}

	return a
}

// DownloadingPercent is the downloading nodes' mean progress in whole percent, rounded down, and
// false while no node downloads. Ready nodes are counted, not averaged in, so a node that starts
// downloading does not pull a mean of finished copies down.
func (a Aggregate) DownloadingPercent() (int32, bool) {
	if a.Downloading == 0 {
		return 0, false
	}

	return int32(a.fractions / float64(a.Downloading) * 100), true // nolint: gosec // within 0..100.
}

// SteppedPercent is DownloadingPercent rounded down to a multiple of 5, the step a stored status moves
// in, and nil while no node downloads.
func (a Aggregate) SteppedPercent() *int32 {
	p, ok := a.DownloadingPercent()
	if !ok {
		return nil
	}
	p -= p % 5

	return &p
}

// Reasons returns the failure reasons sorted by name, with their counts.
func (a Aggregate) Reasons() []string {
	reasons := make([]string, 0, len(a.FailureReasons))
	for r := range a.FailureReasons {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)

	return reasons
}
