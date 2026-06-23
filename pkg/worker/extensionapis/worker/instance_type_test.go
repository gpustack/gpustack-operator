package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// qty parses a quantity for terse fixture construction.
func qty(s string) resource.Quantity { return resource.MustParse(s) }

// qtyEqual compares quantities by value (Cmp) so the assertion does not depend
// on the SI format (BinarySI vs DecimalSI) of the operands.
func qtyEqual(t *testing.T, want, got resource.Quantity, msg string) {
	t.Helper()
	assert.Zerof(t, want.Cmp(got), "%s: want %s, got %s", msg, want.String(), got.String())
}

// mkClusterQueue assembles a kueue.ClusterQueue noted as an InstanceType with
// one flavor sharing the queue's name and the supplied capacity / reservation.
// Reservation may be nil to exercise the no-reservation branch.
func mkClusterQueue(
	name string,
	notes map[string]string,
	capacity core.ResourceList,
	reservation core.ResourceList,
) *kueue.ClusterQueue {
	flv := kueue.FlavorQuotas{
		Name:      kueue.ResourceFlavorReference(name),
		Resources: make([]kueue.ResourceQuota, 0, len(capacity)),
	}
	covered := make([]core.ResourceName, 0, len(capacity))
	for n, q := range capacity {
		flv.Resources = append(flv.Resources, kueue.ResourceQuota{
			Name:         n,
			NominalQuota: q,
		})
		covered = append(covered, n)
	}

	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: kueue.ClusterQueueSpec{
			ResourceGroups: []kueue.ResourceGroup{
				{
					CoveredResources: covered,
					Flavors:          []kueue.FlavorQuotas{flv},
				},
			},
		},
	}
	systemmeta.NoteResource(cq, "instancetypes", notes)

	if reservation != nil {
		usage := kueue.FlavorUsage{
			Name:      kueue.ResourceFlavorReference(name),
			Resources: make([]kueue.ResourceUsage, 0, len(reservation)),
		}
		for n, q := range reservation {
			usage.Resources = append(usage.Resources, kueue.ResourceUsage{
				Name:  n,
				Total: q,
			})
		}
		cq.Status.FlavorsReservation = []kueue.FlavorUsage{usage}
	}

	return cq
}

// mkSlicedClusterQueue builds the sliced InstanceType's ClusterQueue: the queue
// name is the per-unit "-1d-8s" profile while its single flavor is the per-node
// "-4d-8s" profile carrying 0 credits (the borrow topology), with the partition
// count and unit resources noted as the reconciler would.
func mkSlicedClusterQueue(reservation core.ResourceList) *kueue.ClusterQueue {
	const (
		queueName  = "gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d-8s"
		flavorName = "gpustack--amd-epyc-7r13-processor-ln-x64-48c-192g-88g--nvidia-a10g-4d-8s"
	)
	credits := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: queueName},
		Spec: kueue.ClusterQueueSpec{
			ResourceGroups: []kueue.ResourceGroup{{
				CoveredResources: []core.ResourceName{
					core.ResourceCPU, core.ResourceMemory, core.ResourceEphemeralStorage, credits,
				},
				Flavors: []kueue.FlavorQuotas{{
					Name: kueue.ResourceFlavorReference(flavorName),
					Resources: []kueue.ResourceQuota{
						{Name: core.ResourceCPU, NominalQuota: qty("48")},
						{Name: core.ResourceMemory, NominalQuota: qty("192Gi")},
						{Name: core.ResourceEphemeralStorage, NominalQuota: qty("88Gi")},
						{Name: credits, NominalQuota: qty("0")}, // borrows from the exclusive queue
					},
				}},
			}},
		},
	}
	systemmeta.NoteResource(cq, "instancetypes", map[string]string{
		"acceleratable":     "true",
		"manufacturer":      nodefeature.ManufacturerNVIDIA,
		"slicedAccelerator": "8",
		"unitCPU":           "12",
		"unitRAM":           "48",
		"detail":            "{}",
	})

	if reservation != nil {
		usage := kueue.FlavorUsage{
			Name:      kueue.ResourceFlavorReference(flavorName),
			Resources: make([]kueue.ResourceUsage, 0, len(reservation)),
		}
		for n, q := range reservation {
			usage.Resources = append(usage.Resources, kueue.ResourceUsage{Name: n, Total: q})
		}
		cq.Status.FlavorsReservation = []kueue.FlavorUsage{usage}
	}
	return cq
}

// TestConvertInstanceTypeFromClusterQueue_ExclusiveWithLentSliced pins the
// exclusive InstanceType in the borrow topology: its queue (the un-suffixed
// "-1d") now carries the lent sliced flavor ("-4d-8s", credits=4) plus a drained
// "-4d" tombstone (credits=0). The non-sliced path is unchanged, so capacity is
// the credits sum (4 whole cards) and unit resources are not folded.
func TestConvertInstanceTypeFromClusterQueue_ExclusiveWithLentSliced(t *testing.T) {
	const (
		queueName  = "gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d"
		lentFlavor = "gpustack--amd-epyc-7r13-processor-ln-x64-48c-192g-88g--nvidia-a10g-4d-8s"
		tombstone  = "gpustack--amd-epyc-7r13-processor-ln-x64-48c-192g-88g--nvidia-a10g-4d"
	)
	credits := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)
	covered := []core.ResourceName{
		core.ResourceCPU, core.ResourceMemory, core.ResourceEphemeralStorage, credits,
	}
	mkFlavor := func(name, cpu, ram, stg, cred string) kueue.FlavorQuotas {
		return kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(name),
			Resources: []kueue.ResourceQuota{
				{Name: core.ResourceCPU, NominalQuota: qty(cpu)},
				{Name: core.ResourceMemory, NominalQuota: qty(ram)},
				{Name: core.ResourceEphemeralStorage, NominalQuota: qty(stg)},
				{Name: credits, NominalQuota: qty(cred)},
			},
		}
	}
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: queueName},
		Spec: kueue.ClusterQueueSpec{
			ResourceGroups: []kueue.ResourceGroup{{
				CoveredResources: covered,
				Flavors: []kueue.FlavorQuotas{
					mkFlavor(lentFlavor, "48", "192Gi", "88Gi", "4"),
					mkFlavor(tombstone, "0", "0", "0", "0"),
				},
			}},
		},
	}
	systemmeta.NoteResource(cq, "instancetypes", map[string]string{
		"acceleratable": "true",
		"manufacturer":  nodefeature.ManufacturerNVIDIA,
		"unitCPU":       "12",
		"unitRAM":       "48",
		"detail":        "{}",
	})

	it := convertInstanceTypeFromClusterQueue(cq, false)
	require.NotNil(t, it)
	assert.True(t, it.Spec.Acceleratable, "Acceleratable")
	assert.Zero(t, it.Spec.Sliced, "Sliced (exclusive)")
	qtyEqual(t, qty("4"), it.Status.Accelerator.Capacity, "Capacity.Accelerator (whole cards)")
	qtyEqual(t, qty("4"), it.Status.Accelerator.Remaining, "Remaining.Accelerator")
	qtyEqual(t, qty("4"), it.Status.Accelerator.OnceMaxRequest, "OnceMaxRequest.Accelerator")
	assert.Equal(t, "12", it.Spec.UnitResources.CPU, "UnitResources.CPU (not folded)")
	assert.Equal(t, "48Gi", it.Spec.UnitResources.RAM, "UnitResources.RAM (not folded)")
}

// TestConvertInstanceTypeFromClusterQueue_Sliced pins Task 11: a sliced
// InstanceType reports Accelerator capacity = card-count × partitions, remaining
// at the slice rate, OnceMaxRequest = floorPow2(min(partitions/2, remaining))
// (round DOWN, shrinking with usage), and per-slice unit resources rounded down.
func TestConvertInstanceTypeFromClusterQueue_Sliced(t *testing.T) {
	credits := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

	cases := []struct {
		name        string
		reservation core.ResourceList
		wantCap     resource.Quantity
		wantRem     resource.Quantity
		wantOrm     resource.Quantity
	}{
		{
			// node-5: 4 cards × 8 partitions = 32 slices, none reserved.
			name:    "no reservation: 32 slices, ORM capped at partitions/2",
			wantCap: qty("32"),
			wantRem: qty("32"),
			wantOrm: qty("4"), // min(4, 32) = 4
		},
		{
			// One 1/8 slice reserved = 0.125 credits → remaining (4−0.125)×8 = 31.
			name:        "one slice reserved: remaining 31, ORM still 4",
			reservation: core.ResourceList{credits: qty("125m")},
			wantCap:     qty("32"),
			wantRem:     qty("31"),
			wantOrm:     qty("4"), // min(4, 31) = 4
		},
		{
			// 29 slices reserved = 3.625 credits → remaining (4−3.625)×8 = 3.
			// ORM rounds DOWN: floorPow2(min(4,3)) = 2 (not 4).
			name:        "remaining 3: ORM rounds down to 2",
			reservation: core.ResourceList{credits: qty("3625m")},
			wantCap:     qty("32"),
			wantRem:     qty("3"),
			wantOrm:     qty("2"),
		},
		{
			// 30 slices reserved = 3.75 credits → remaining 2 → ORM 2.
			name:        "remaining 2: ORM is 2",
			reservation: core.ResourceList{credits: qty("3750m")},
			wantCap:     qty("32"),
			wantRem:     qty("2"),
			wantOrm:     qty("2"),
		},
		{
			// 31 slices reserved = 3.875 credits → remaining 1 → ORM 1.
			name:        "remaining 1: ORM is 1",
			reservation: core.ResourceList{credits: qty("3875m")},
			wantCap:     qty("32"),
			wantRem:     qty("1"),
			wantOrm:     qty("1"),
		},
		{
			// Fully reserved (4 credits) → remaining 0 → ORM 0.
			name:        "remaining 0: ORM collapses to 0",
			reservation: core.ResourceList{credits: qty("4")},
			wantCap:     qty("32"),
			wantRem:     qty("0"),
			wantOrm:     qty("0"),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			it := convertInstanceTypeFromClusterQueue(mkSlicedClusterQueue(c.reservation), false)
			require.NotNil(t, it, "expected InstanceType")

			assert.True(t, it.Spec.Acceleratable, "Acceleratable")
			assert.Equal(t, int64(8), it.Spec.Sliced, "Sliced")
			qtyEqual(t, c.wantCap, it.Status.Accelerator.Capacity, "Capacity.Accelerator")
			qtyEqual(t, c.wantRem, it.Status.Accelerator.Remaining, "Remaining.Accelerator")
			qtyEqual(t, c.wantOrm, it.Status.Accelerator.OnceMaxRequest, "OnceMaxRequest.Accelerator")
			assert.Equal(t, "1", it.Spec.UnitResources.CPU, "UnitResources.CPU (12/8 round down)")
			assert.Equal(t, "6Gi", it.Spec.UnitResources.RAM, "UnitResources.RAM (48/8 round down)")
		})
	}
}

func TestConvertInstanceTypeFromClusterQueue(t *testing.T) {
	const nonAccName = "gpustack--amd-25-1-16c-32g-100g"
	const accName = "gpustack--amd-25-1-4c-16g-100g--nvidia-t4-1d"

	nonAccNotes := map[string]string{"acceleratable": "false"}
	accNotes := map[string]string{
		"acceleratable": "true",
		"manufacturer":  nodefeature.ManufacturerNVIDIA,
	}
	credits := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

	nonAccCapacity := core.ResourceList{
		core.ResourceCPU:              qty("16"),
		core.ResourceMemory:           qty("32Gi"),
		core.ResourceEphemeralStorage: qty("100Gi"),
	}
	accCapacity := core.ResourceList{
		core.ResourceCPU:              qty("4"),
		core.ResourceMemory:           qty("16Gi"),
		core.ResourceEphemeralStorage: qty("100Gi"),
		credits:                       qty("1"),
	}

	cases := []struct {
		name string

		cqName      string
		notes       map[string]string
		capacity    core.ResourceList
		reservation core.ResourceList
		overcommit  bool
		draining    bool

		wantAcceleratable bool
		wantCapAcc        resource.Quantity
		wantRemCPU        resource.Quantity
		wantRemRAM        resource.Quantity
		wantRemStg        resource.Quantity
		wantRemAcc        resource.Quantity
		// OnceMaxRequest is the largest single-pod ask the InstanceType can
		// admit. Without reservation it equals the flavor's profile maxima.
		// With reservation it tracks the post-reservation remaining (clamped
		// to the flavor's own ORM ceiling). For acceleratable types, the
		// display ORM is gated by remaining accelerator > 0 — fully reserving
		// the accelerator collapses the whole display ORM to zero, because
		// no new acc-needing pod can be admitted.
		wantOrmCPU resource.Quantity
		wantOrmRAM resource.Quantity
		wantOrmStg resource.Quantity
		wantOrmAcc resource.Quantity
	}{
		// --- non-acceleratable ---
		{
			name:        "non-acc, no reservation: Remaining == Capacity",
			cqName:      nonAccName,
			notes:       nonAccNotes,
			capacity:    nonAccCapacity,
			reservation: nil,
			overcommit:  false,

			wantAcceleratable: false,
			wantCapAcc:        qty("0"),
			wantRemCPU:        qty("16"),
			wantRemRAM:        qty("32Gi"),
			wantRemStg:        qty("100Gi"),
			wantRemAcc:        qty("0"),
			// ORM == flavor profile (parsed from cq name).
			wantOrmCPU: qty("16"),
			wantOrmRAM: qty("32Gi"),
			wantOrmStg: qty("100Gi"),
			wantOrmAcc: qty("0"),
		},
		{
			name:     "non-acc, overcommit off: reservation in limits-units, direct subtract",
			cqName:   nonAccName,
			notes:    nonAccNotes,
			capacity: nonAccCapacity,
			reservation: core.ResourceList{
				// Single pod with limits 4C/8Gi/15Gi; overcommit off ⇒ requests == limits.
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			overcommit: false,

			wantAcceleratable: false,
			wantCapAcc:        qty("0"),
			wantRemCPU:        qty("12"),   // 16 - 4
			wantRemRAM:        qty("24Gi"), // 32Gi - 8Gi
			wantRemStg:        qty("85Gi"), // 100Gi - 15Gi
			wantRemAcc:        qty("0"),
			// With reservation, ORM tracks the post-subtract remaining
			// (clamp to flavor ORM ceiling is a no-op here: 12 <= 16).
			wantOrmCPU: qty("12"),
			wantOrmRAM: qty("24Gi"),
			wantOrmStg: qty("85Gi"),
			wantOrmAcc: qty("0"),
		},
		{
			name:     "non-acc, overcommit on: reservation in requests-units, scaled back before subtract",
			cqName:   nonAccName,
			notes:    nonAccNotes,
			capacity: nonAccCapacity,
			reservation: core.ResourceList{
				// Same pod (4C/8Gi/15Gi) recorded as overcommit requests:
				// CPU = 800m × 4 = 3200m, RAM = 128Mi × 8 = 1Gi, Stg = 128Mi × 15 = 1920Mi.
				core.ResourceCPU:              qty("3200m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
			},
			overcommit: true,

			wantAcceleratable: false,
			wantCapAcc:        qty("0"),
			wantRemCPU:        qty("12"),   // 16 - ScaleBack(3200m, non-acc) = 16 - 4
			wantRemRAM:        qty("24Gi"), // 32Gi - ScaleBack(1Gi, RAM) = 32Gi - 8Gi
			wantRemStg:        qty("85Gi"), // 100Gi - ScaleBack(1920Mi, Stg) = 100Gi - 15Gi
			wantRemAcc:        qty("0"),
			// ORM identical to the non-overcommit case — semantic
			// equivalence is the whole point of ScaleBack.
			wantOrmCPU: qty("12"),
			wantOrmRAM: qty("24Gi"),
			wantOrmStg: qty("85Gi"),
			wantOrmAcc: qty("0"),
		},

		// --- acceleratable ---
		{
			name:        "acc, no reservation: Remaining == Capacity",
			cqName:      accName,
			notes:       accNotes,
			capacity:    accCapacity,
			reservation: nil,
			overcommit:  true,

			wantAcceleratable: true,
			wantCapAcc:        qty("1"),
			wantRemCPU:        qty("4"),
			wantRemRAM:        qty("16Gi"),
			wantRemStg:        qty("100Gi"),
			wantRemAcc:        qty("1"),
			// ORM == flavor profile parsed from cq name.
			wantOrmCPU: qty("4"),
			wantOrmRAM: qty("16Gi"),
			wantOrmStg: qty("100Gi"),
			wantOrmAcc: qty("1"),
		},
		{
			name:     "acc, overcommit off: reservation in limits-units, accelerator counted as-is",
			cqName:   accName,
			notes:    accNotes,
			capacity: accCapacity,
			reservation: core.ResourceList{
				core.ResourceCPU:              qty("2"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
				credits:                       qty("1"),
			},
			overcommit: false,

			wantAcceleratable: true,
			wantCapAcc:        qty("1"),
			wantRemCPU:        qty("2"),    // 4 - 2
			wantRemRAM:        qty("8Gi"),  // 16Gi - 8Gi
			wantRemStg:        qty("85Gi"), // 100Gi - 15Gi
			wantRemAcc:        qty("0"),    // 1 - 1
			// Accelerator fully reserved (remAccRf=0). The acceleratable
			// branch only updates display ORM when remAccRf > 0, so the
			// whole ORM block collapses to zero — no new acc-needing pod
			// can fit.
			wantOrmCPU: qty("0"),
			wantOrmRAM: qty("0"),
			wantOrmStg: qty("0"),
			wantOrmAcc: qty("0"),
		},
		{
			name:     "acc, overcommit on: CPU uses 100m base, credits are NOT scaled back",
			cqName:   accName,
			notes:    accNotes,
			capacity: accCapacity,
			reservation: core.ResourceList{
				// Pod with limits 2C/8Gi/15Gi/1acc, recorded as overcommit requests:
				// CPU = 100m × CPU.Value() = 100m × 2 = 200m  (acceleratable base)
				// RAM = 128Mi × 8  = 1Gi
				// Stg = 128Mi × 15 = 1920Mi
				// credits = 1  (accelerator is not overcommitted; reservation stored as-is)
				core.ResourceCPU:              qty("200m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
				credits:                       qty("1"),
			},
			overcommit: true,

			wantAcceleratable: true,
			wantCapAcc:        qty("1"),
			wantRemCPU:        qty("2"),    // 4 - ScaleBack(200m, acc) = 4 - 2
			wantRemRAM:        qty("8Gi"),  // 16Gi - ScaleBack(1Gi, RAM) = 16Gi - 8Gi
			wantRemStg:        qty("85Gi"), // 100Gi - ScaleBack(1920Mi, Stg) = 100Gi - 15Gi
			// Key invariant: ScaleBack is a pass-through for the credits
			// resource name, so 1 reserved credit subtracts 1 directly.
			wantRemAcc: qty("0"),
			// Same "fully reserved → ORM zero" outcome as the off case.
			wantOrmCPU: qty("0"),
			wantOrmRAM: qty("0"),
			wantOrmStg: qty("0"),
			wantOrmAcc: qty("0"),
		},

		// --- draining ---
		{
			// A draining (HoldAndDrain) queue exposes zero capacity across all
			// four resources, even though its flavor still carries nominal
			// quota — no new workload should target a queue being drained.
			name:        "draining: HoldAndDrain zeroes all resources",
			cqName:      nonAccName,
			notes:       nonAccNotes,
			capacity:    nonAccCapacity,
			reservation: nil,
			overcommit:  false,
			draining:    true,

			wantAcceleratable: false,
			wantCapAcc:        qty("0"),
			wantRemCPU:        qty("0"),
			wantRemRAM:        qty("0"),
			wantRemStg:        qty("0"),
			wantRemAcc:        qty("0"),
			wantOrmCPU:        qty("0"),
			wantOrmRAM:        qty("0"),
			wantOrmStg:        qty("0"),
			wantOrmAcc:        qty("0"),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cq := mkClusterQueue(c.cqName, c.notes, c.capacity, c.reservation)
			if c.draining {
				cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
			}
			it := convertInstanceTypeFromClusterQueue(cq, c.overcommit)
			if !assert.NotNil(t, it, "expected InstanceType") {
				return
			}

			// Spec.
			assert.Equal(t, c.wantAcceleratable, it.Spec.Acceleratable, "Spec.Acceleratable")

			// Capacity comes straight from the ClusterQueue input, unless the
			// queue is draining (HoldAndDrain), where every resource is zeroed.
			wantCapCPU := c.capacity[core.ResourceCPU]
			wantCapRAM := c.capacity[core.ResourceMemory]
			wantCapStg := c.capacity[core.ResourceEphemeralStorage]
			if c.draining {
				wantCapCPU, wantCapRAM, wantCapStg = qty("0"), qty("0"), qty("0")
			}
			qtyEqual(t, wantCapCPU, it.Status.CPU.Capacity, "Capacity.CPU")
			qtyEqual(t, wantCapRAM, it.Status.RAM.Capacity, "Capacity.RAM")
			qtyEqual(t, wantCapStg, it.Status.LocalStorage.Capacity, "Capacity.Storage")
			qtyEqual(t, c.wantCapAcc, it.Status.Accelerator.Capacity, "Capacity.Accelerator")

			// Remaining.
			qtyEqual(t, c.wantRemCPU, it.Status.CPU.Remaining, "Remaining.CPU")
			qtyEqual(t, c.wantRemRAM, it.Status.RAM.Remaining, "Remaining.RAM")
			qtyEqual(t, c.wantRemStg, it.Status.LocalStorage.Remaining, "Remaining.Storage")
			qtyEqual(t, c.wantRemAcc, it.Status.Accelerator.Remaining, "Remaining.Accelerator")

			// OnceMaxRequest.
			qtyEqual(t, c.wantOrmCPU, it.Status.CPU.OnceMaxRequest, "OnceMaxRequest.CPU")
			qtyEqual(t, c.wantOrmRAM, it.Status.RAM.OnceMaxRequest, "OnceMaxRequest.RAM")
			qtyEqual(t, c.wantOrmStg, it.Status.LocalStorage.OnceMaxRequest, "OnceMaxRequest.Storage")
			qtyEqual(t, c.wantOrmAcc, it.Status.Accelerator.OnceMaxRequest, "OnceMaxRequest.Accelerator")
		})
	}
}
