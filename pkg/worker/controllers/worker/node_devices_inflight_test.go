package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueconstants "sigs.k8s.io/kueue/pkg/constants"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	kueuetas "sigs.k8s.io/kueue/pkg/util/tas"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// admissionClientBuilder is the fake client every Reconcile of the node-devices check needs: the
// check lists the Pods bound to a node by field, which the fake client serves only through an index.
func admissionClientBuilder() *ctrlfake.ClientBuilder {
	return ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithIndex(&core.Pod{}, _podNodeNameField, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		})
}

// allocatedPod builds a Pod bound to node whose allocation record charges each card the units given
// for it, in mode, to its container "c". The check rebuilds a node's ledger from such records, so a
// fixture's occupancy has to be stated as Pods rather than as a published Status.
func allocatedPod(name, node string, devs *workercore.Devices, mode workercore.DeviceAllocationMode,
	units map[string]int32,
) *core.Pod {
	var status workercore.DevicesStatus
	for gi := range devs.Spec.Groups {
		g := &devs.Spec.Groups[gi]
		grp := workercore.DevicesAllocationGroup{ID: g.ID, Manufacturer: g.Manufacturer}
		for ai := range g.Accelerators {
			if u, ok := units[g.Accelerators[ai].ID]; ok {
				grp.Accelerators = append(grp.Accelerators, workercore.AcceleratorAllocation{
					ID: g.Accelerators[ai].ID, Index: g.Accelerators[ai].Index, Mode: mode, Allocated: u,
				})
			}
		}
		if len(grp.Accelerators) > 0 {
			status.Groups = append(status.Groups, grp)
		}
	}
	record, err := json.Marshal(deviceplugin.PodAllocations{"c": {Devices: status}})
	if err != nil {
		panic(err)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: name,
			Annotations: map[string]string{deviceplugin.AllocatedAcceleratorAnnoKey: string(record)},
		},
		Spec: core.PodSpec{NodeName: node, Containers: []core.Container{{Name: "c"}}},
	}
}

// chargingPods states a fixture's published Status as the Pods that produce it: one Pod per card
// that is not wholly free, charging it the units it is missing in the mode it reports. The Status
// the check rebuilds from them is the fixture's own.
func chargingPods(devs *workercore.Devices) []ctrlcli.Object {
	var pods []ctrlcli.Object
	for gi := range devs.Status.Groups {
		for _, acc := range devs.Status.Groups[gi].Accelerators {
			if acc.Remaining >= nodefeature.ResourceMaxUnits {
				continue
			}
			pods = append(pods, allocatedPod(devs.Name+"-holds-"+acc.ID, devs.Name, devs, acc.Mode,
				map[string]int32{acc.ID: nodefeature.ResourceMaxUnits - acc.Remaining}))
		}
	}
	return pods
}

// inflightFixture holds the resource keys and the TAS-assigned Workloads the inflight cases share.
type inflightFixture struct {
	base, slicedCard, slicedUnits, sharedCard, partitionCard core.ResourceName
	poolLabels                                               map[string]string
}

func newInflightFixture() inflightFixture {
	base := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	return inflightFixture{
		base:          base,
		slicedCard:    base + nodefeature.SlicedResourceNameSuffix,
		slicedUnits:   base + nodefeature.SlicedUnitsResourceNameSuffix,
		sharedCard:    base + nodefeature.SharedResourceNameSuffix,
		partitionCard: base + nodefeature.PartitionedResourceNameSuffix,
		poolLabels: map[string]string{
			"feature.gpustack.ai/nvidia":                  "true",
			"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
		},
	}
}

func (f inflightFixture) slice(units int32) core.ResourceList {
	return core.ResourceList{f.slicedCard: resource.MustParse("1"), f.slicedUnits: *resource.NewQuantity(int64(units), resource.DecimalSI)}
}

// workload builds a Workload holding a reservation whose single podset of pods Pods TAS placed on
// hostname, with this controller's check in state.
func (f inflightFixture) workload(name, hostname string, pods int32, requests core.ResourceList, state kueue.CheckState) *kueue.Workload {
	ta := &kueuetas.TopologyAssignment{
		Levels:  []string{core.LabelHostname},
		Domains: []kueuetas.TopologyDomainAssignment{{Values: []string{hostname}, Count: pods}},
	}
	return &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: name},
		Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
			Name: "main", Count: pods,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name: "c", Resources: core.ResourceRequirements{Requests: requests},
			}}}},
		}}},
		Status: kueue.WorkloadStatus{
			Conditions: []meta.Condition{{
				Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
				Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
			}},
			Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
				Name:               "main",
				Flavors:            map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
				Count:              ptr.To(pods),
				TopologyAssignment: kueuetas.V1Beta2From(ta),
			}}},
			AdmissionChecks: []kueue.AdmissionCheckState{{Name: _NodeDevicesAdmissionCheckName, State: state}},
		},
	}
}

// node returns the Devices ledger and the Node of one pool node named "node-<suffix>" whose hostname
// is "host-<suffix>".
func (f inflightFixture) node(suffix string, devs workercore.Devices) (*workercore.Devices, *core.Node) {
	name := "node-" + suffix
	for gi := range devs.Spec.Groups {
		for ai := range devs.Spec.Groups[gi].Accelerators {
			id := name + "-" + devs.Spec.Groups[gi].Accelerators[ai].ID
			devs.Spec.Groups[gi].Accelerators[ai].ID = id
			devs.Status.Groups[gi].Accelerators[ai].ID = id
		}
	}
	devs.ObjectMeta = meta.ObjectMeta{Name: name, Labels: f.poolLabels}
	return &devs, &core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{core.LabelHostname: "host-" + suffix}}}
}

func (f inflightFixture) client(objs ...ctrlcli.Object) ctrlcli.Client {
	objs = append(objs,
		&kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"}, Spec: kueue.ResourceFlavorSpec{NodeLabels: f.poolLabels}},
		&kueue.AdmissionCheck{ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName}, Spec: kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName}},
	)
	return admissionClientBuilder().WithObjects(objs...).WithStatusSubresource(&kueue.Workload{}).Build()
}

// judge reconciles the named Workload once and returns the state and message of this controller's
// check on it.
func judge(t *testing.T, r *NodeDevicesAdmissionReconciler, cli ctrlcli.Client, name string) (kueue.CheckState, string) {
	t.Helper()
	key := ctrlcli.ObjectKey{Namespace: "default", Name: name}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	got := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), key, got))
	acs := kueueadmissioncheck.FindAdmissionCheck(got.Status.AdmissionChecks, _NodeDevicesAdmissionCheckName)
	require.NotNil(t, acs)
	return acs.State, acs.Message
}

// partitionDevices builds one free partitionable card offering the profile at a single placement, so
// it can host exactly one instance.
func partitionDevices(profile string) workercore.Devices {
	prof := workercore.AcceleratorPhysicalSlicedProfile{
		Name: profile, Count: 1, Placements: []workercore.AcceleratorPlacement{{Start: 0, Length: 1}},
	}
	return workercore.Devices{
		Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
			ID: "g0", Manufacturer: "nvidia",
			Accelerators: []workercore.Accelerator{{ID: "gpu-0", Status: workercore.AcceleratorStatus{
				PhysicalSliced: workercore.AcceleratorPhysicalSliced{Profiles: []workercore.AcceleratorPhysicalSlicedProfile{prof}, Count: 1},
			}}},
		}}},
		Status: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID: "g0", Manufacturer: "nvidia",
			Accelerators: []workercore.AcceleratorAllocation{{ID: "gpu-0", Remaining: nodefeature.ResourceMaxUnits}},
		}}},
	}
}

// TestNodeDevicesAdmission_CountsInflightWorkloads reconciles two Workloads TAS assigned to one
// node, one after the other, before any Pod of the first reaches the ledger. The node has room for
// exactly one of them, so the second is held. Without counting the first as inflight, both are
// answered Ready and the device plugin puts the second on a card that cannot hold it.
func TestNodeDevicesAdmission_CountsInflightWorkloads(t *testing.T) {
	f := newInflightFixture()
	whole := int32(nodefeature.ResourceMaxUnits)
	share := whole / nodefeature.SharedResourceMaxSize
	fraction := whole * 2 / 5
	profileKey := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")
	oneInstance := core.ResourceList{f.partitionCard: resource.MustParse("1"), profileKey: resource.MustParse("1")}

	cases := []struct {
		name          string
		devs          workercore.Devices
		first, second core.ResourceList
	}{
		{
			name:   "a 60% then a 50% slice onto the one free card of four",
			devs:   withMode(devicesWithRemaining(fraction, fraction, fraction, whole), workercore.DeviceAllocationModeSliced),
			first:  f.slice(whole * 3 / 5),
			second: f.slice(whole / 2),
		},
		{
			name:   "two whole cards onto the one free card of four",
			devs:   withMode(devicesWithRemaining(0, 0, 0, whole), workercore.DeviceAllocationModeExclusive),
			first:  core.ResourceList{f.base: resource.MustParse("1")},
			second: core.ResourceList{f.base: resource.MustParse("1")},
		},
		{
			name:   "two shares onto the last free share",
			devs:   withMode(devicesWithRemaining(share), workercore.DeviceAllocationModeShared),
			first:  core.ResourceList{f.sharedCard: resource.MustParse("1")},
			second: core.ResourceList{f.sharedCard: resource.MustParse("1")},
		},
		{
			name:   "two partitions onto the last free placement",
			devs:   partitionDevices("1g.10gb"),
			first:  oneInstance,
			second: oneInstance,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// withMode marks every card, including the free one; a free card reads Mode None.
			for gi := range c.devs.Status.Groups {
				for ai := range c.devs.Status.Groups[gi].Accelerators {
					if acc := &c.devs.Status.Groups[gi].Accelerators[ai]; acc.Remaining >= whole {
						acc.Mode = workercore.DeviceAllocationModeNone
					}
				}
			}
			devs, node := f.node("b", c.devs)
			objs := append(chargingPods(devs), devs, node,
				f.workload("first", "host-b", 1, c.first, kueue.CheckStatePending),
				f.workload("second", "host-b", 1, c.second, kueue.CheckStatePending))
			cli := f.client(objs...)
			r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

			state, msg := judge(t, r, cli, "first")
			require.Equal(t, kueue.CheckStateReady, state, msg)
			state, msg = judge(t, r, cli, "second")
			assert.Equal(t, kueue.CheckStateRetry, state, msg)
			assert.Contains(t, msg, "counted as taken")
		})
	}
}

// otherWorkload is a Workload judged before the one under test, with Pods of it the ledger may
// already hold.
type otherWorkload struct {
	name     string
	hostname string
	pods     int32
	requests core.ResourceList
	check    kueue.CheckState
	admitted bool
	// charged lists, per Pod of it bound to node-b whose allocation is recorded, the units that Pod
	// holds on card 0 of node-b in sliced mode.
	charged []int32
}

// TestNodeDevicesAdmission_InflightAccounting pins which Workloads count as inflight and how much
// of each, on node-b carrying one free card, against a Workload asking a 40% slice there. Every
// case is built so that the other answer is what the opposite accounting would give: two 40% Pods
// counted leave 20%, and so does a 40% Pod counted twice. node-b's published Status reads
// the card free throughout, as it does before the device manager has rebuilt it.
func TestNodeDevicesAdmission_InflightAccounting(t *testing.T) {
	f := newInflightFixture()
	whole := int32(nodefeature.ResourceMaxUnits)
	slice30, slice40, slice80 := f.slice(whole*3/10), f.slice(whole*2/5), f.slice(whole*4/5)

	cases := []struct {
		name  string
		other otherWorkload
		want  kueue.CheckState
	}{
		{
			name:  "a Ready Workload whose Pod the ledger holds is not counted again",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 1, requests: slice40, check: kueue.CheckStateReady, admitted: true, charged: []int32{whole * 2 / 5}},
			want:  kueue.CheckStateReady,
		},
		{
			name:  "a Pod the published Status does not show yet still holds its room",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 1, requests: slice80, check: kueue.CheckStateReady, admitted: true, charged: []int32{whole * 4 / 5}},
			want:  kueue.CheckStateRetry,
		},
		{
			name:  "only the Pods of a Workload the ledger does not hold yet are counted",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 2, requests: slice30, check: kueue.CheckStateReady, admitted: true, charged: []int32{whole * 3 / 10}},
			want:  kueue.CheckStateReady,
		},
		{
			name:  "a Ready Workload none of whose Pods the ledger holds is counted whole",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 2, requests: slice40, check: kueue.CheckStateReady},
			want:  kueue.CheckStateRetry,
		},
		{
			name:  "an admitted Workload that went through no check of this controller is counted",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 2, requests: slice40, admitted: true},
			want:  kueue.CheckStateRetry,
		},
		{
			name:  "a Workload held in Retry is not counted",
			other: otherWorkload{name: "o", hostname: "host-b", pods: 2, requests: slice40, check: kueue.CheckStateRetry},
			want:  kueue.CheckStateReady,
		},
		{
			name:  "a Ready Workload assigned to another node is not counted",
			other: otherWorkload{name: "o", hostname: "host-a", pods: 2, requests: slice40, check: kueue.CheckStateReady},
			want:  kueue.CheckStateReady,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devsA, nodeA := f.node("a", devicesWithRemaining(whole))
			devsB, nodeB := f.node("b", devicesWithRemaining(whole))
			other := f.workload(c.other.name, c.other.hostname, c.other.pods, c.other.requests, c.other.check)
			if c.other.check == "" {
				other.Status.AdmissionChecks = nil
			}
			if c.other.admitted {
				other.Status.Conditions = append(other.Status.Conditions, meta.Condition{
					Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue,
					Reason: "Admitted", Message: "admitted", LastTransitionTime: meta.Now(),
				})
			}
			objs := []ctrlcli.Object{
				devsA, nodeA, devsB, nodeB, other,
				f.workload("self", "host-b", 1, slice40, kueue.CheckStatePending),
			}
			for i, units := range c.other.charged {
				pod := allocatedPod(fmt.Sprintf("%s-%d", c.other.name, i), devsB.Name, devsB, workercore.DeviceAllocationModeSliced,
					map[string]int32{devsB.Spec.Groups[0].Accelerators[0].ID: units})
				pod.Annotations[kueue.WorkloadAnnotation] = c.other.name
				pod.Labels = map[string]string{kueueconstants.PodSetLabel: "main"}
				pod.Spec.Containers[0].Resources.Requests = c.other.requests
				objs = append(objs, pod)
			}
			cli := f.client(objs...)
			r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

			state, msg := judge(t, r, cli, "self")
			assert.Equal(t, c.want, state, msg)
		})
	}
}

// TestNodeDevicesAdmission_CountsReadyTheCacheHasNotShown pins the process-local record of Ready
// verdicts. The next judgment reads other Workloads from the cache, which can still predate the
// Ready just written; the second Workload must be held all the same.
func TestNodeDevicesAdmission_CountsReadyTheCacheHasNotShown(t *testing.T) {
	f := newInflightFixture()
	whole := int32(nodefeature.ResourceMaxUnits)
	devs, node := f.node("b", devicesWithRemaining(whole))
	cli := f.client(devs, node,
		f.workload("first", "host-b", 1, f.slice(whole*3/5), kueue.CheckStatePending),
		f.workload("second", "host-b", 1, f.slice(whole/2), kueue.CheckStatePending))
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

	state, msg := judge(t, r, cli, "first")
	require.Equal(t, kueue.CheckStateReady, state, msg)

	// Put the first Workload back as the cache would still show it: its check not yet Ready.
	stale := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "first"}, stale))
	stale.Status.AdmissionChecks[0].State = kueue.CheckStatePending
	require.NoError(t, cli.Status().Update(context.Background(), stale))

	state, msg = judge(t, r, cli, "second")
	assert.Equal(t, kueue.CheckStateRetry, state, msg)
}

// TestPodCharged pins when a Pod's allocation record counts as holding the Pod's whole demand.
func TestPodCharged(t *testing.T) {
	f := newInflightFixture()
	devs := devicesWithRemaining(nodefeature.ResourceMaxUnits)
	card := core.ResourceList{f.base: resource.MustParse("1")}

	cases := []struct {
		name string
		pod  func() *core.Pod
		want bool
	}{
		{
			name: "every container asking a card is recorded",
			pod: func() *core.Pod {
				p := allocatedPod("p", "node", &devs, workercore.DeviceAllocationModeExclusive, map[string]int32{"gpu-0": nodefeature.ResourceMaxUnits})
				p.Spec.Containers[0].Resources.Limits = card
				p.Spec.Containers = append(p.Spec.Containers, core.Container{Name: "sidecar"})
				return p
			},
			want: true,
		},
		{
			name: "a container asking a card is not recorded",
			pod: func() *core.Pod {
				p := allocatedPod("p", "node", &devs, workercore.DeviceAllocationModeExclusive, map[string]int32{"gpu-0": nodefeature.ResourceMaxUnits})
				p.Spec.InitContainers = []core.Container{{Name: "init", Resources: core.ResourceRequirements{Limits: card}}}
				return p
			},
			want: false,
		},
		{
			name: "an unreadable record",
			pod: func() *core.Pod {
				p := allocatedPod("p", "node", &devs, workercore.DeviceAllocationModeExclusive, nil)
				p.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{"
				return p
			},
			want: false,
		},
		{
			name: "no record",
			pod: func() *core.Pod {
				return &core.Pod{Spec: core.PodSpec{Containers: []core.Container{{Name: "c", Resources: core.ResourceRequirements{Limits: card}}}}}
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, podCharged(c.pod()))
		})
	}
}

// TestNodeDevicesAdmission_HoldsBehindAnInflightFlavorItCannotResolve pins the hold for an inflight
// Workload whose flavor no longer names a card population: which cards it will take cannot be told,
// so the Workload judged after it is held rather than fitted as if they were free. The same inflight
// Workload on a flavor that resolves leaves room for it.
func TestNodeDevicesAdmission_HoldsBehindAnInflightFlavorItCannotResolve(t *testing.T) {
	f := newInflightFixture()
	whole := int32(nodefeature.ResourceMaxUnits)

	cases := []struct {
		name   string
		flavor kueue.ResourceFlavorReference
		want   kueue.CheckState
		wantIn string
	}{
		{name: "a flavor that resolves", flavor: "gpu-pool", want: kueue.CheckStateReady, wantIn: "has enough free cards"},
		{name: "a flavor that is gone", flavor: "gone", want: kueue.CheckStateRetry, wantIn: "resolves to no card population"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devs, node := f.node("b", devicesWithRemaining(whole))
			other := f.workload("other", "host-b", 1, f.slice(whole*3/10), kueue.CheckStateReady)
			other.Status.Admission.PodSetAssignments[0].Flavors[creditsResource] = c.flavor
			cli := f.client(devs, node, other, f.workload("self", "host-b", 1, f.slice(whole*2/5), kueue.CheckStatePending))
			r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

			state, msg := judge(t, r, cli, "self")
			assert.Equal(t, c.want, state, msg)
			assert.Contains(t, msg, c.wantIn)
		})
	}
}
