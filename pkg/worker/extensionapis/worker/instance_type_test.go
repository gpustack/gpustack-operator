package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestConvertInstanceTypeFromClusterQueue(t *testing.T) {
	const nonAccName = "gpustack-general-16c-32g-100g"
	const accName = "gpustack-nvidia-t4-4c-16g-100g-1d"

	nonAccNotes := map[string]string{"acceleratable": "false"}
	accNotes := map[string]string{
		"acceleratable": "true",
		"manufacturer":  nodefeature.ManufacturerNVIDIA,
	}
	credits := nodefeature.GetCreditsResourceName(nodefeature.ManufacturerNVIDIA)

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
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cq := mkClusterQueue(c.cqName, c.notes, c.capacity, c.reservation)
			it := convertInstanceTypeFromClusterQueue(cq, c.overcommit)
			if !assert.NotNil(t, it, "expected InstanceType") {
				return
			}

			// Spec.
			assert.Equal(t, c.wantAcceleratable, it.Spec.Acceleratable, "Spec.Acceleratable")

			// Capacity comes straight from the ClusterQueue input.
			qtyEqual(t, c.capacity[core.ResourceCPU], it.Status.CPU.Capacity, "Capacity.CPU")
			qtyEqual(t, c.capacity[core.ResourceMemory], it.Status.RAM.Capacity, "Capacity.RAM")
			qtyEqual(t, c.capacity[core.ResourceEphemeralStorage], it.Status.LocalStorage.Capacity, "Capacity.Storage")
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
