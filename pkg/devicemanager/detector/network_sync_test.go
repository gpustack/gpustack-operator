package detector

import (
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

// alignmentFixture builds an existing Devices object already owned by its node and already
// carrying the selector labels, so the only thing left that can move `skip` is the inventory under
// test. Without that, an assertion about the interface branch would pass on an owner reference
// being stamped.
func alignmentFixture(t *testing.T, stored []workercore.DeviceInterface) (
	actual *workercore.Devices, node *core.Node,
) {
	t.Helper()

	node = &core.Node{
		ObjectMeta: meta.ObjectMeta{Name: "node-1", UID: types.UID("node-1-uid")},
	}
	actual = &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Spec:       workercore.DevicesSpec{Interfaces: stored},
	}
	kubemeta.ControlOnWithoutBlock(actual, node, core.SchemeGroupVersion.WithKind("Node"))
	return actual, node
}

func iface(name string, mtu int32) workercore.DeviceInterface {
	return workercore.DeviceInterface{Name: name, Bus: "pci", MTU: mtu}
}

// TestDevicesAlignmentInterfaces pins the comparison that decides whether the interface inventory
// is ever written.
//
// This is the single most likely way the whole feature ships broken while looking healthy: the
// object's groups are compared, and if the interfaces are not compared independently the field is
// computed on every pass and written never — with nothing about the object looking wrong.
func TestDevicesAlignmentInterfaces(t *testing.T) {
	testCases := []struct {
		name string
		// stored is what the API already holds; detected is what this pass produced.
		stored, detected []workercore.DeviceInterface
		// syncInterfaces is false when the pass could not enumerate at all.
		syncInterfaces bool
		wantSkip       bool
		wantStored     []workercore.DeviceInterface
	}{
		{
			// The criterion. Groups are equal on both sides, so ONLY the interface comparison can
			// report a change; if it does not, nothing is written.
			name:           "groups equal and interfaces differ, so the inventory is written",
			stored:         []workercore.DeviceInterface{iface("eth0", 1500)},
			detected:       []workercore.DeviceInterface{iface("eth0", 9000)},
			syncInterfaces: true,
			wantSkip:       false,
			wantStored:     []workercore.DeviceInterface{iface("eth0", 9000)},
		},
		{
			// The converse, and equally a criterion: an unchanged pass must write nothing. If this
			// fails, the detector issues an API write on every pass forever, and no functional
			// test would ever notice.
			name:           "nothing changed, so nothing is written",
			stored:         []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)},
			detected:       []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)},
			syncInterfaces: true,
			wantSkip:       true,
			wantStored:     []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)},
		},
		{
			// A pass that could not enumerate must leave the previous inventory alone. Replacing
			// it with an empty list would publish "this worker has no interfaces", which is a
			// claim a failed read cannot support.
			name:           "enumeration failed, so the previous inventory is kept",
			stored:         []workercore.DeviceInterface{iface("eth0", 9000)},
			detected:       nil,
			syncInterfaces: false,
			wantSkip:       true,
			wantStored:     []workercore.DeviceInterface{iface("eth0", 9000)},
		},
		{
			// A successful pass that finds nothing IS a real answer, unlike a failed one, so the
			// empty list must be written. Conflating the two is what the flag exists to prevent.
			name:           "enumeration succeeded and found nothing, so the empty list is written",
			stored:         []workercore.DeviceInterface{iface("eth0", 9000)},
			detected:       nil,
			syncInterfaces: true,
			wantSkip:       false,
			wantStored:     nil,
		},
		{
			name:           "an interface appears",
			stored:         []workercore.DeviceInterface{iface("eth0", 9000)},
			detected:       []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)},
			syncInterfaces: true,
			wantSkip:       false,
			wantStored:     []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, node := alignmentFixture(t, tc.stored)
			alignment := devicesAlignment{
				expected: &workercore.Devices{
					Spec: workercore.DevicesSpec{Interfaces: tc.detected},
				},
				node:           node,
				manufacturers:  sets.New[string](),
				syncInterfaces: tc.syncInterfaces,
			}

			got, skip, err := alignment.apply(actual)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if !kubemeta.DeepEqual(got.Spec.Interfaces, tc.wantStored) {
				t.Errorf("stored inventory = %v, want %v", got.Spec.Interfaces, tc.wantStored)
			}
		})
	}
}

// TestDevicesAlignmentInterfaceOrderIsAChange pins why the pass has to publish a sorted list.
//
// The two lists here hold the same interfaces in a different order. The equality used above treats
// that as a change — so if the detector ever emitted them unordered, every pass would report a
// change and write, forever, with correct data in the object the whole time. That failure has no
// symptom a functional test could see, which is why the ordering is asserted at the producer and
// its consequence is asserted here.
func TestDevicesAlignmentInterfaceOrderIsAChange(t *testing.T) {
	ordered := []workercore.DeviceInterface{iface("eth0", 9000), iface("eth1", 9000)}
	shuffled := []workercore.DeviceInterface{iface("eth1", 9000), iface("eth0", 9000)}

	actual, node := alignmentFixture(t, ordered)
	alignment := devicesAlignment{
		expected:       &workercore.Devices{Spec: workercore.DevicesSpec{Interfaces: shuffled}},
		node:           node,
		manufacturers:  sets.New[string](),
		syncInterfaces: true,
	}

	_, skip, err := alignment.apply(actual)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skip {
		t.Fatal("a reordering was treated as no change; if that were true, ordering would not " +
			"matter and the producer's sort would be pointless")
	}
}
