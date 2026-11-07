package modelstore

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func testGrant() *workercore.ModelStoreBindingSourceMemory {
	return &workercore.ModelStoreBindingSourceMemory{
		Bytes:         resource.MustParse("16Gi"),
		Registrations: 4,
		Sessions:      8,
	}
}

func testDigestOf(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)
}

// TestPlanModelTransferSourceMemory pins the declared composition of one source worker's memory request:
// the admitted residency, a fill buffer one transfer window wide, the declared runtime overhead,
// and quarantine headroom for a generation whose completion could not be proven.
func TestPlanModelTransferSourceMemory(t *testing.T) {
	resolved := int64(10 << 30)
	grant := testGrant()

	plan, err := PlanModelTransferSourceMemory(resolved, grant.Bytes.Value())
	require.NoError(t, err)

	assert.Equal(t, resolved, plan.ResidentBytes)
	assert.Equal(t, resolved, plan.WindowBytes,
		"a checkpoint inside the grant's headroom reserves its full size")
	assert.False(t, plan.Clamped)
	assert.False(t, plan.BelowFloor)
	assert.Equal(t, int64(1<<30), plan.FillBytes,
		"the fill buffer is one transfer window")
	assert.Equal(t, int64(256<<20), plan.RuntimeBytes)
	assert.Equal(t, resolved/4, plan.QuarantineBytes,
		"quarantine headroom quarters the residency")
	assert.Equal(t, resolved+1<<30+256<<20+resolved/4, plan.TotalBytes)
	assert.Equal(t, plan.TotalBytes,
		plan.ResidentBytes+plan.FillBytes+plan.RuntimeBytes+plan.QuarantineBytes)
}

// TestPlanModelTransferSourceMemoryWindowsAboveGrant pins the windowed residency: a checkpoint
// larger than the grant clamps the residency to the largest window the grant's headroom backs
// (headroom minus the quarantine quarter) and streams the remainder instead of refusing.
func TestPlanModelTransferSourceMemoryWindowsAboveGrant(t *testing.T) {
	resolved := int64(30 << 30)
	grant := testGrant()

	plan, err := PlanModelTransferSourceMemory(resolved, grant.Bytes.Value())
	require.NoError(t, err)

	fill, runtime := int64(1<<30), int64(256<<20)
	available := grant.Bytes.Value() - fill - runtime
	wantWindow := available / 5 * 4

	assert.True(t, plan.Clamped, "an over-grant checkpoint must clamp the residency")
	assert.False(t, plan.BelowFloor)
	assert.Equal(t, wantWindow, plan.WindowBytes)
	assert.Equal(t, wantWindow, plan.ResidentBytes)
	assert.Equal(t, wantWindow/4, plan.QuarantineBytes,
		"quarantine headroom quarters the clamped residency")
	assert.Equal(t, wantWindow+fill+runtime+wantWindow/4, plan.TotalBytes,
		"the total stays within the grant")
	assert.LessOrEqual(t, plan.TotalBytes, grant.Bytes.Value())
}

// TestPlanModelTransferSourceMemoryBelowFloor pins the floor: a grant whose headroom cannot
// back even protocol.DefaultWindowBytes of residency reports BelowFloor for the caller's
// policy decision (Prefer falls back, Require waits) instead of admitting a tiny window.
func TestPlanModelTransferSourceMemoryBelowFloor(t *testing.T) {
	// A grant that backs a sliver of headroom, but under the 1 GiB floor after the
	// quarantine quarter.
	grant := int64(1<<30 + 256<<20)

	plan, err := PlanModelTransferSourceMemory(int64(10<<30), grant)
	require.ErrorIs(t, err, ErrSourceWindowBelowFloor)
	assert.True(t, plan.BelowFloor)
	assert.Less(t, plan.WindowBytes, int64(1<<30))
}

// TestPlanModelTransferSourceMemorySmallCheckpointUnderGrant pins that the floor
// bounds what a grant can back, not what a checkpoint reserves: a small checkpoint
// under a floor-capable grant succeeds with its own size as the window.
func TestPlanModelTransferSourceMemorySmallCheckpointUnderGrant(t *testing.T) {
	resolved := int64(512 << 20)
	grant := testGrant()

	plan, err := PlanModelTransferSourceMemory(resolved, grant.Bytes.Value())
	require.NoError(t, err)

	assert.False(t, plan.Clamped)
	assert.False(t, plan.BelowFloor,
		"a small checkpoint under a floor-capable grant is not below the floor")
	assert.Equal(t, resolved, plan.WindowBytes)
	assert.Equal(t, resolved, plan.ResidentBytes)
	assert.LessOrEqual(t, plan.TotalBytes, grant.Bytes.Value())
}

// TestPlanModelTransferSourceMemoryClampsToMaxResident pins the clamp to the
// residency the grant's headroom backs: a 14 GiB checkpoint under a 16 GiB grant
// clamps to maxResident and the total stays within the grant.
func TestPlanModelTransferSourceMemoryClampsToMaxResident(t *testing.T) {
	resolved := int64(14 << 30)
	grant := testGrant()

	plan, err := PlanModelTransferSourceMemory(resolved, grant.Bytes.Value())
	require.NoError(t, err)

	fill, runtime := int64(1<<30), int64(256<<20)
	maxResident := (grant.Bytes.Value() - fill - runtime) / 5 * 4

	assert.True(t, plan.Clamped, "a checkpoint over the backed residency clamps")
	assert.False(t, plan.BelowFloor)
	assert.Equal(t, maxResident, plan.WindowBytes)
	assert.Less(t, plan.WindowBytes, resolved)
	assert.LessOrEqual(t, plan.TotalBytes, grant.Bytes.Value())
}

// TestMinSourceWindowBytes pins the floor resolution: the floor is fixed at
// the default protocol window, and no environment variable moves it.
func TestMinSourceWindowBytes(t *testing.T) {
	assert.Equal(t, int64(1<<30), MinSourceWindowBytes(),
		"the floor mirrors protocol.DefaultWindowBytes: 1 GiB")
	assert.Equal(t, int64(1<<30), MinSourceWindowBytes(),
		"repeated reads agree")
}

// TestPlanModelTransferSourceMemoryFloorIsFixed pins that the below-floor
// threshold no longer moves with an environment figure: a grant backing a
// 3 GiB residency admits whatever GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES
// says, because the controller no longer reads it.
func TestPlanModelTransferSourceMemoryFloorIsFixed(t *testing.T) {
	grant := int64(5 << 30)
	resolved := int64(10 << 30)

	t.Setenv("GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES", "4294967296")
	plan, err := PlanModelTransferSourceMemory(resolved, grant)
	require.NoError(t, err)
	assert.False(t, plan.BelowFloor, "a 3 GiB backed residency clears the fixed floor")
}

func TestPlanModelTransferSourceMemoryRefusesUnsizedAndZeroGrant(t *testing.T) {
	grant := testGrant()

	_, err := PlanModelTransferSourceMemory(0, grant.Bytes.Value())
	require.Error(t, err, "an unsized artifact authorizes no allocation")
	assert.Contains(t, err.Error(), "size")

	_, err = PlanModelTransferSourceMemory(1, 0)
	require.Error(t, err, "a zero grant is no grant")
}

// TestPlanModelTransferSourceMemoryClampsExtremes pins the ceiling behavior at the extremes:
// the clamped residency plus its headroom stays under the int64 ceiling where the old
// full-residency plan would have overflowed.
func TestPlanModelTransferSourceMemoryClampsExtremes(t *testing.T) {
	for _, resolved := range []int64{math.MaxInt64, math.MaxInt64 - (320 << 20)} {
		t.Run(fmt.Sprint(resolved), func(t *testing.T) {
			plan, err := PlanModelTransferSourceMemory(resolved, math.MaxInt64)
			require.NoError(t, err)
			assert.True(t, plan.Clamped, "an extreme checkpoint against an unbounded grant still clamps")
			assert.Less(t, plan.ResidentBytes, resolved,
				"the clamp reduced the residency below the resolved size")
		})
	}
}

// TestModelTransferAdmittedDigests pins what counts as admitted: a generation in any of the three reported
// states holds its reservation; a digest no store reports admits nothing.
func TestModelTransferAdmittedDigests(t *testing.T) {
	d1, d2, d3 := testDigestOf("1"), testDigestOf("2"), testDigestOf("3")
	stores := []workercore.NodeModelStore{
		{ObjectMeta: meta.ObjectMeta{Name: "node-a"}, Status: workercore.NodeModelStoreStatus{
			ModelTransfer: modelTransferReport(
				workercore.NodeModelStoreModelTransferGeneration{
					Generation: "1", Digest: d1,
					State: workercore.NodeModelStoreModelTransferGenerationStateServing,
				},
			),
		}},
		{ObjectMeta: meta.ObjectMeta{Name: "node-b"}, Status: workercore.NodeModelStoreStatus{
			ModelTransfer: modelTransferReport(
				workercore.NodeModelStoreModelTransferGeneration{
					Generation: "2", Digest: d2,
					State: workercore.NodeModelStoreModelTransferGenerationStateQuarantined,
				},
				workercore.NodeModelStoreModelTransferGeneration{
					Generation: "3", Digest: d1,
					State: workercore.NodeModelStoreModelTransferGenerationStateDraining,
				},
			),
		}},
	}

	got := ModelTransferAdmittedDigests(stores, map[string]bool{d1: true, d2: true, d3: true})
	assert.Equal(t, map[string]bool{d1: true, d2: true}, got,
		"a draining or quarantined generation still holds its reservation; an unreported one does not")

	assert.Empty(t, ModelTransferAdmittedDigests(stores, map[string]bool{d3: true}))
	assert.Empty(t, ModelTransferAdmittedDigests(nil, map[string]bool{d1: true}))
}

// TestTransferLimitsEcho pins the applied-policy echo: the grant's limits, restated where
// they were admitted; no grant echoes none.
func TestTransferLimitsEcho(t *testing.T) {
	limits := TransferLimits(testGrant())
	require.NotNil(t, limits)
	assert.Equal(t, "16Gi", limits.Bytes.String())
	assert.Equal(t, int32(4), limits.Registrations)
	assert.Equal(t, int32(8), limits.Sessions)

	assert.Nil(t, TransferLimits(nil))
}
