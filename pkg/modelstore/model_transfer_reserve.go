package modelstore

import (
	"errors"
	"fmt"
	"math"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// The declared figures of one source worker's memory composition. They are
// declarations, not measurements: the residency is the admitted reservation,
// the fill buffer is one transfer window wide, the runtime overhead is the
// figure this controller declares, and the quarantine headroom quarters the
// residency so a generation whose completion could not be proven stays held
// instead of evicted to zero.
const (
	// ModelTransferSourceRuntimeBytes is the declared runtime overhead of the source
	// process itself: registrations, control state, and allocator slack.
	ModelTransferSourceRuntimeBytes int64 = 256 << 20

	// modelTransferQuarantineDivisor is how much of the residency is declared again
	// as quarantine headroom: residency divided by this figure.
	modelTransferQuarantineDivisor int64 = 4
)

// MinSourceWindowBytes returns the minimum source residency window floor:
// the default protocol window, fixed. The floor is a node-independent
// contract between the controller's plan and the broker's own 1 GiB buffer
// floor, not a per-deployment figure, so no environment variable moves it.
func MinSourceWindowBytes() int64 {
	return protocol.DefaultWindowBytes
}

// ErrSourceWindowBelowFloor reports that the grant's headroom cannot back even
// the minimum source window. The caller owns the policy decision: a Prefer
// deployment falls back to Node-to-Node Peer Sync or the local CSI disk load,
// and a Require deployment keeps waiting until the source node frees enough
// capacity to back the floor.
var ErrSourceWindowBelowFloor = errors.New("source residency window below the minimum floor")

// ModelTransferSourceMemoryPlan is one source worker's memory request, as it goes
// through existing Pod admission: residency, fill, runtime, and quarantine
// headroom, summed in TotalBytes.
type ModelTransferSourceMemoryPlan struct {
	ResidentBytes   int64
	FillBytes       int64
	RuntimeBytes    int64
	QuarantineBytes int64
	TotalBytes      int64

	// WindowBytes is the source residency window W_source actually reserved:
	// the full resolved size when it fits the grant's headroom, otherwise the
	// largest residency the headroom backs — a windowed source residency whose
	// paging streams the rest of the checkpoint.
	WindowBytes int64
	// Clamped says the resolved checkpoint exceeded the grant, so the residency
	// was windowed down to WindowBytes instead of refused.
	Clamped bool
	// BelowFloor says the grant's headroom cannot back even the minimum source
	// window floor (MinSourceWindowBytes): the caller decides between the Prefer
	// fallback to disk delivery and the Require wait for source capacity.
	BelowFloor bool
}

// PlanModelTransferSourceMemory composes the memory one source worker requests for an
// artifact of resolvedBytes under a grant of grantBytes.
//
// The residency is windowed rather than refused: when the checkpoint fits the
// grant's headroom the plan reserves it in full with zero paging, and when it
// exceeds the headroom the plan clamps the residency to the largest window the
// headroom backs and streams the remainder. A grant whose headroom backs no
// window under the MinSourceWindowBytes floor returns ErrSourceWindowBelowFloor
// for the caller's policy decision; a small checkpoint under a floor-capable
// grant reserves its own size. An unsized artifact still authorizes no
// allocation at all.
func PlanModelTransferSourceMemory(resolvedBytes, grantBytes int64) (ModelTransferSourceMemoryPlan, error) {
	switch {
	case resolvedBytes <= 0:
		return ModelTransferSourceMemoryPlan{}, fmt.Errorf(
			"resolved size %d cannot be admitted: an unsized artifact authorizes no allocation",
			resolvedBytes,
		)
	case grantBytes <= 0:
		return ModelTransferSourceMemoryPlan{}, fmt.Errorf(
			"grant bytes %d cannot back a source reservation", grantBytes,
		)
	}

	fill := protocol.DefaultWindowBytes
	runtime := ModelTransferSourceRuntimeBytes

	// The headroom the grant leaves for residency once the fill window and the
	// declared runtime overhead are paid. The residency itself quarters into
	// quarantine, so the headroom backs four fifths of its own size.
	available := grantBytes - fill - runtime
	maxResident := available / 5 * 4

	// The floor bounds what a grant can back, not what a checkpoint needs: a
	// small checkpoint under a floor-capable grant reserves only its own size.
	plan := ModelTransferSourceMemoryPlan{FillBytes: fill, RuntimeBytes: runtime}
	floor := MinSourceWindowBytes()
	if maxResident < floor {
		plan.WindowBytes = max(maxResident, 0)
		plan.BelowFloor = true
		return plan, fmt.Errorf("%w: the grant %d backs only a %d residency window, under the %d floor",
			ErrSourceWindowBelowFloor, grantBytes, plan.WindowBytes, floor)
	}

	plan.WindowBytes = resolvedBytes
	if resolvedBytes > maxResident {
		plan.WindowBytes = maxResident
		plan.Clamped = true
	}

	plan.ResidentBytes = plan.WindowBytes
	plan.QuarantineBytes = plan.ResidentBytes / modelTransferQuarantineDivisor
	overhead := plan.FillBytes + plan.RuntimeBytes + plan.QuarantineBytes
	if plan.ResidentBytes > math.MaxInt64-overhead {
		return ModelTransferSourceMemoryPlan{}, fmt.Errorf("resolved size %d overflows the source memory ceiling", resolvedBytes)
	}
	plan.TotalBytes = plan.ResidentBytes + overhead

	return plan, nil
}
