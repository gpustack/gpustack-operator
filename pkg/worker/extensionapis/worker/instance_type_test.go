package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// mkClusterQueue assembles a kueue.ClusterQueue noted as an InstanceType,
// with one flavor sharing the given name and the supplied capacity / reservation.
//
// Capacity and reservation entries are passed as ResourceName→Quantity maps;
// the accelerator resource (when present) uses the credits name derived from
// `notes["manufacturer"]`. Reservation is nil to test the no-reservation
// branch.
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

func TestConvertInstanceTypeFromClusterQueue_NonAcceleratable(t *testing.T) {
	// Profile: 16 CPU, 32 Gi RAM, 100 Gi storage; one node.
	const cqName = "gpustack-general-16c-32g-100g"
	notes := map[string]string{
		"acceleratable": "false",
	}
	capacity := core.ResourceList{
		core.ResourceCPU:              qty("16"),
		core.ResourceMemory:           qty("32Gi"),
		core.ResourceEphemeralStorage: qty("100Gi"),
	}

	cases := []struct {
		name        string
		overcommit  bool
		reservation core.ResourceList
		wantCapCPU  resource.Quantity
		wantRemCPU  resource.Quantity
		wantRemRAM  resource.Quantity
		wantRemStg  resource.Quantity
	}{
		{
			name:        "no reservation: Remaining == Capacity",
			overcommit:  false,
			reservation: nil,
			wantCapCPU:  qty("16"),
			wantRemCPU:  qty("16"),
			wantRemRAM:  qty("32Gi"),
			wantRemStg:  qty("100Gi"),
		},
		{
			name:       "no overcommit: reservation in limits-units, direct subtract",
			overcommit: false,
			reservation: core.ResourceList{
				// One pod with 4C/8Gi/15Gi limits → requests == limits.
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantCapCPU: qty("16"),
			wantRemCPU: qty("12"),   // 16 - 4
			wantRemRAM: qty("24Gi"), // 32Gi - 8Gi
			wantRemStg: qty("85Gi"), // 100Gi - 15Gi
		},
		{
			name:       "overcommit: reservation in requests-units, scaled back before subtract",
			overcommit: true,
			reservation: core.ResourceList{
				// Same pod (4C/8Gi/15Gi limits) but recorded as overcommit requests:
				// 800m × 4 = 3200m, 128Mi × 8 = 1Gi, 128Mi × 15 = 1920Mi.
				core.ResourceCPU:              qty("3200m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
			},
			wantCapCPU: qty("16"),
			wantRemCPU: qty("12"),   // 16 - reverse(3200m, non-acc) = 16 - 4
			wantRemRAM: qty("24Gi"), // 32Gi - reverse(1Gi, RAM) = 32Gi - 8Gi
			wantRemStg: qty("85Gi"), // 100Gi - reverse(1920Mi, storage) = 100Gi - 15Gi
		},
		{
			name:       "overcommit symmetric with non-overcommit yields identical Remaining",
			overcommit: true,
			reservation: core.ResourceList{
				// Two pods aggregated: 2C/4Gi/10Gi + 2C/4Gi/5Gi
				// → 800m×(2+2)=3200m, 128Mi×(4+4)=1Gi, 128Mi×(10+5)=1920Mi.
				core.ResourceCPU:              qty("3200m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
			},
			wantCapCPU: qty("16"),
			wantRemCPU: qty("12"),
			wantRemRAM: qty("24Gi"),
			wantRemStg: qty("85Gi"),
		},
	}

	for _, cs := range cases {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			cq := mkClusterQueue(cqName, notes, capacity, cs.reservation)
			it := convertInstanceTypeFromClusterQueue(cq, cs.overcommit)

			if !assert.NotNil(t, it, "expected InstanceType") {
				return
			}
			assert.False(t, it.Spec.Acceleratable, "non-accel type")
			assert.Truef(t, cs.wantCapCPU.Equal(it.Status.CPU.Capacity),
				"capacity CPU: want %s, got %s", cs.wantCapCPU.String(), it.Status.CPU.Capacity.String())
			assert.Truef(t, cs.wantRemCPU.Equal(it.Status.CPU.Remaining),
				"remaining CPU: want %s, got %s", cs.wantRemCPU.String(), it.Status.CPU.Remaining.String())
			assert.Truef(t, cs.wantRemRAM.Equal(it.Status.RAM.Remaining),
				"remaining RAM: want %s, got %s", cs.wantRemRAM.String(), it.Status.RAM.Remaining.String())
			assert.Truef(t, cs.wantRemStg.Equal(it.Status.LocalStorage.Remaining),
				"remaining Stg: want %s, got %s", cs.wantRemStg.String(), it.Status.LocalStorage.Remaining.String())
		})
	}
}

func TestConvertInstanceTypeFromClusterQueue_Acceleratable(t *testing.T) {
	// Profile: 4 CPU, 16 Gi RAM, 100 Gi storage, 1 accelerator; one node.
	const cqName = "gpustack-nvidia-t4-4c-16g-100g-1d"
	notes := map[string]string{
		"acceleratable": "true",
		"manufacturer":  devicefeature.ManufacturerNVIDIA,
	}
	credits := devicefeature.GetCreditsResourceName(devicefeature.ManufacturerNVIDIA)
	capacity := core.ResourceList{
		core.ResourceCPU:              qty("4"),
		core.ResourceMemory:           qty("16Gi"),
		core.ResourceEphemeralStorage: qty("100Gi"),
		credits:                       qty("1"),
	}

	cases := []struct {
		name        string
		overcommit  bool
		reservation core.ResourceList
		wantRemCPU  resource.Quantity
		wantRemRAM  resource.Quantity
		wantRemStg  resource.Quantity
		wantRemAcc  resource.Quantity
	}{
		{
			name:        "no reservation",
			overcommit:  true,
			reservation: nil,
			wantRemCPU:  qty("4"),
			wantRemRAM:  qty("16Gi"),
			wantRemStg:  qty("100Gi"),
			wantRemAcc:  qty("1"),
		},
		{
			name:       "no overcommit: direct subtract",
			overcommit: false,
			reservation: core.ResourceList{
				core.ResourceCPU:              qty("2"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
				credits:                       qty("1"),
			},
			wantRemCPU: qty("2"),
			wantRemRAM: qty("8Gi"),
			wantRemStg: qty("85Gi"),
			wantRemAcc: qty("0"),
		},
		{
			name:       "overcommit acc accel>0: CPU=100m×accel.Value(); credits untouched",
			overcommit: true,
			reservation: core.ResourceList{
				// One pod with limits 2C/8Gi/15Gi/1acc → requests:
				// CPU = 100m × 1 (accel.Value()) = 100m
				// RAM = 128Mi × 8 = 1Gi
				// Stg = 128Mi × 15 = 1920Mi
				// Acc = 1 (not overcommitted)
				core.ResourceCPU:              qty("100m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
				credits:                       qty("1"),
			},
			wantRemCPU: qty("3"),    // 4 - reverse(100m, acc) = 4 - 1 (info-loss: actual was 2)
			wantRemRAM: qty("8Gi"),  // 16Gi - 8Gi
			wantRemStg: qty("85Gi"), // 100Gi - 15Gi
			wantRemAcc: qty("0"),    // 1 - 1
		},
		{
			name:       "overcommit acc no-accel pod: CPU multiplier = CPU.Value()",
			overcommit: true,
			reservation: core.ResourceList{
				// One pod with limits 2C/8Gi/15Gi and accel=0 → requests:
				// CPU = 100m × 2 (CPU.Value()) = 200m
				// RAM = 128Mi × 8 = 1Gi
				// Stg = 128Mi × 15 = 1920Mi
				// No credits reservation (accel = 0).
				core.ResourceCPU:              qty("200m"),
				core.ResourceMemory:           qty("1Gi"),
				core.ResourceEphemeralStorage: qty("1920Mi"),
			},
			wantRemCPU: qty("2"),    // 4 - reverse(200m, acc) = 4 - 2 (exact)
			wantRemRAM: qty("8Gi"),  // 16Gi - 8Gi
			wantRemStg: qty("85Gi"), // 100Gi - 15Gi
			wantRemAcc: qty("1"),    // untouched
		},
	}

	for _, cs := range cases {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			cq := mkClusterQueue(cqName, notes, capacity, cs.reservation)
			it := convertInstanceTypeFromClusterQueue(cq, cs.overcommit)

			if !assert.NotNil(t, it, "expected InstanceType") {
				return
			}
			assert.True(t, it.Spec.Acceleratable, "accel type")
			assert.Truef(t, cs.wantRemCPU.Equal(it.Status.CPU.Remaining),
				"remaining CPU: want %s, got %s", cs.wantRemCPU.String(), it.Status.CPU.Remaining.String())
			assert.Truef(t, cs.wantRemRAM.Equal(it.Status.RAM.Remaining),
				"remaining RAM: want %s, got %s", cs.wantRemRAM.String(), it.Status.RAM.Remaining.String())
			assert.Truef(t, cs.wantRemStg.Equal(it.Status.LocalStorage.Remaining),
				"remaining Stg: want %s, got %s", cs.wantRemStg.String(), it.Status.LocalStorage.Remaining.String())
			assert.Truef(t, cs.wantRemAcc.Equal(it.Status.Accelerator.Remaining),
				"remaining Acc: want %s, got %s", cs.wantRemAcc.String(), it.Status.Accelerator.Remaining.String())
		})
	}
}
