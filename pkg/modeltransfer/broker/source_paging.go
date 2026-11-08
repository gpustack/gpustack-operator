package broker

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

const (
	sourceResidentBudgetEnv     = "GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES"
	hixlSourceResidentBudgetEnv = "GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES"
	hixlMemoryAlignment         = int64(2 << 20)
)

// sourceAllocationBytes includes backing space for aligned HIXL addresses and extents.
func sourceAllocationBytes(size int64, backend string) int64 {
	if backend == "hixl" && size > 0 {
		return ((size+hixlMemoryAlignment-1)/hixlMemoryAlignment + 1) * hixlMemoryAlignment
	}
	return size
}

func sourceResidentLimits() (int64, int64, error) {
	var resident int64
	if raw := os.Getenv(sourceResidentBudgetEnv); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return 0, 0, fmt.Errorf("%s must be a non-negative integer", sourceResidentBudgetEnv)
		}
		resident = value
		if resident > 0 {
			resident = max(AlignDown2M(resident), 1<<30)
		}
	}
	// An empty or zero HIXL budget keeps the 16 GiB ceiling; a negative or
	// non-integer value fails fast like the primary resident budget.
	hixl := int64(16 << 30)
	if raw := os.Getenv(hixlSourceResidentBudgetEnv); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return 0, 0, fmt.Errorf("%s must be a non-negative integer", hixlSourceResidentBudgetEnv)
		}
		if value > 0 {
			hixl = max(AlignDown2M(value), 1<<30)
		}
	}
	return resident, hixl, nil
}

// sourceResidentBytes separates the immutable file extent from the pinned
// window. A clamped window needs a verified manifest proving every shard fits.
func (b *Broker) sourceResidentBytes(req FillRequest, manifest *protocol.Manifest, backend string) (int64, *protocol.Error) {
	if req.TotalBytes <= 0 {
		return 0, protocol.New(protocol.KindInvalidDescriptor, "fill", "the source file extent is outside the supported bound")
	}
	if req.TotalBytes > 4<<40 {
		return 0, protocol.New(protocol.KindCapacityExhausted, "fill", "the source file extent exceeds the supported capacity")
	}
	required := req.TotalBytes
	var largest int64
	if backend == "hixl" && manifest == nil {
		return 0, protocol.New(protocol.KindCapacityExhausted, "fill", "aligned source admission requires a verified file manifest")
	}
	if manifest != nil {
		var extent int64
		required = 0
		for _, file := range manifest.ReferencedFiles() {
			if !strings.HasSuffix(file.Path, ".safetensors") || file.Size <= 0 || file.Size > 1<<40 {
				return 0, protocol.New(protocol.KindInvalidDescriptor, "fill", "the source manifest has an invalid shard")
			}
			extent += file.Size
			allocation := sourceAllocationBytes(file.Size, backend)
			required += allocation
			largest = max(largest, allocation)
		}
		if extent != req.TotalBytes {
			return 0, protocol.New(protocol.KindInvalidDescriptor, "fill", "the source file extent does not match the manifest")
		}
	}
	// Invert the reservation charge without multiplying a caller's limit.
	ceiling := min(req.Grant.Bytes, b.limits.PhysicalBytes)
	resident := min(required, ceiling-ceiling/(reserveOverhead+1))
	if reserveBytes(resident) > ceiling {
		resident--
	}
	if b.residentBudget > 0 {
		resident = min(resident, b.residentBudget)
	}
	if backend == "hixl" && b.hixlResidentBudget > 0 {
		resident = min(resident, b.hixlResidentBudget)
	}
	if resident == required {
		return resident, nil
	}
	resident = AlignDown2M(resident)
	if manifest == nil {
		return 0, protocol.New(protocol.KindCapacityExhausted, "fill", "a clamped source window requires a verified file manifest")
	}
	if resident < max(1<<30, largest) {
		return 0, protocol.New(protocol.KindCapacityExhausted, "fill", "the source window cannot hold the largest shard and source floor")
	}
	return resident, nil
}

func residentCeiling(rec *GenerationRecord) int64 {
	if rec.ResidentBudgetBytes > 0 {
		return rec.ResidentBudgetBytes
	}
	return rec.TotalBytes
}
