package modelstore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelTransferReport builds one store's model-transfer report with the given generation entries.
func modelTransferReport(entries ...workercore.NodeModelStoreModelTransferGeneration) *workercore.NodeModelStoreModelTransferStatus {
	if len(entries) == 0 {
		return nil
	}

	return &workercore.NodeModelStoreModelTransferStatus{Generations: entries}
}

// TestAggregateModelTransferGenerations pins the aggregation the prefetch status is built from: the
// figures are per generation, the drain-and-refill window reads as two entries rather than one
// average, a digest this window is not about is excluded, and the order is stable so a rebuilt
// status never differs from itself by chance.
func TestAggregateModelTransferGenerations(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7", 64)
	otherDigest := "sha256:" + strings.Repeat("a", 64)

	stores := []workercore.NodeModelStore{
		{
			Name: "node-a",
			Status: workercore.NodeModelStoreStatus{
				// Two generations for one digest on one store: the drain-and-refill window.
				ModelTransfer: modelTransferReport(
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-1", Digest: digest,
						State:         workercore.NodeModelStoreModelTransferGenerationStateDraining,
						ResidentBytes: 5 << 30, Registrations: 2, ActiveSessions: 0, RejectedAdmissions: 1,
					},
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-2", Digest: digest,
						State:         workercore.NodeModelStoreModelTransferGenerationStateServing,
						ResidentBytes: 5 << 30, Registrations: 1, ActiveSessions: 3,
					},
					// A generation of another digest is this aggregation's business only through
					// its own digest query.
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-9", Digest: otherDigest,
						State: workercore.NodeModelStoreModelTransferGenerationStateServing,
					},
				),
			},
		},
		{
			Name: "node-b",
			Status: workercore.NodeModelStoreStatus{
				ModelTransfer: modelTransferReport(
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-1", Digest: digest,
						State:         workercore.NodeModelStoreModelTransferGenerationStateQuarantined,
						ResidentBytes: 5 << 30, Registrations: 1, RejectedAdmissions: 2,
					},
				),
			},
		},
		{
			Name: "node-c",
			Status: workercore.NodeModelStoreStatus{
				ModelTransfer: modelTransferReport(
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-1", Digest: digest,
						State:         workercore.NodeModelStoreModelTransferGenerationStateServing,
						ResidentBytes: 5 << 30, Registrations: 1, ActiveSessions: 1,
					},
				),
			},
		},
		{
			Name:   "node-silent",
			Status: workercore.NodeModelStoreStatus{},
		},
	}

	got := AggregateModelTransferGenerations(stores, digest)
	require.Len(t, got, 2)

	draining := got[0]
	assert.Equal(t, "gen-1", draining.Generation)
	assert.Equal(t, int32(1), draining.ReadyNodes, "only the serving store counts as ready")
	assert.Equal(t, int32(2), draining.DrainingNodes,
		"a draining and a quarantined store are both past admission")
	assert.Equal(t, int64(15<<30), draining.ResidentBytes,
		"a quarantined generation's memory is still held, so it counts")
	assert.Equal(t, int32(4), draining.Registrations)
	assert.Equal(t, int32(1), draining.ActiveSessions,
		"a store that admits no sessions contributes none, not its neighbor's")
	assert.Equal(t, int64(3), draining.RejectedAdmissions)

	fresh := got[1]
	assert.Equal(t, "gen-2", fresh.Generation)
	assert.Equal(t, int32(1), fresh.ReadyNodes)
	assert.Equal(t, int32(0), fresh.DrainingNodes)
	assert.Equal(t, int64(0), fresh.RejectedAdmissions,
		"a generation's zero is its own, never the sibling generation's")

	assert.Equal(t, got, AggregateModelTransferGenerations(stores, digest),
		"the aggregation is deterministic: a rebuilt status never differs from itself by chance")

	assert.Empty(t, AggregateModelTransferGenerations(stores, otherDigest+"-missing"),
		"a digest no store serves aggregates to nothing, not to an empty entry")
}

// TestAggregateModelTransferGenerationsOrdersNumerically pins the ordering: generation identifiers that
// are numbers order as numbers, so a node's tenth generation never sorts before its second.
func TestAggregateModelTransferGenerationsOrdersNumerically(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7", 64)
	stores := []workercore.NodeModelStore{
		{
			ObjectMeta: meta.ObjectMeta{Name: "node-a"},
			Status: workercore.NodeModelStoreStatus{
				ModelTransfer: modelTransferReport(
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "10", Digest: digest,
						State: workercore.NodeModelStoreModelTransferGenerationStateServing,
					},
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "2", Digest: digest,
						State: workercore.NodeModelStoreModelTransferGenerationStateDraining,
					},
				),
			},
		},
	}

	got := AggregateModelTransferGenerations(stores, digest)
	require.Len(t, got, 2)
	assert.Equal(t, "2", got[0].Generation)
	assert.Equal(t, "10", got[1].Generation,
		"numeric generation identifiers order as numbers, not as strings")

	// Mixed shapes fall back to the deterministic name order.
	mixed := []workercore.NodeModelStore{
		{
			ObjectMeta: meta.ObjectMeta{Name: "node-a"},
			Status: workercore.NodeModelStoreStatus{
				ModelTransfer: modelTransferReport(
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "gen-2", Digest: digest,
						State: workercore.NodeModelStoreModelTransferGenerationStateServing,
					},
					workercore.NodeModelStoreModelTransferGeneration{
						Generation: "10", Digest: digest,
						State: workercore.NodeModelStoreModelTransferGenerationStateServing,
					},
				),
			},
		},
	}
	got = AggregateModelTransferGenerations(mixed, digest)
	require.Len(t, got, 2)
	assert.Equal(t, "10", got[0].Generation,
		"a non-numeric identifier falls back to the name order beside a numeric one")
}
