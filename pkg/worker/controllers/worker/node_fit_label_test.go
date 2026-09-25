package worker

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

// fitCard is one card of a fit-label fixture: its remaining units, its allocation mode, and its
// capability. logical is a card offering logical slicing; partitioned one in a hardware
// partitioning mode.
type fitCard struct {
	remaining   int32
	mode        workercore.DeviceAllocationMode
	logical     bool
	partitioned bool
	// slices is how many logical slices the card already hosts, of the ten a logical card offers.
	slices int32
}

// freeCard is a whole free card that can serve every whole-card family and a logical slice.
func freeCard(remaining int32) fitCard {
	return fitCard{remaining: remaining, logical: true}
}

// fitGroup is one accelerator model of a fit-label fixture.
type fitGroup struct {
	model string
	cards []fitCard
}

// fitDevices builds one node's Devices ledger with a group per model, the spec side carrying each
// card's capability and the status side its mode and remaining units, joined by accelerator ID.
func fitDevices(node string, groups ...fitGroup) *workercore.Devices {
	d := &workercore.Devices{ObjectMeta: meta.ObjectMeta{
		Name:   node,
		Labels: map[string]string{systemname.ManagedLabelKey: "true"},
	}}
	for _, g := range groups {
		var spec []workercore.Accelerator
		var status []workercore.AcceleratorAllocation
		for i, c := range g.cards {
			id := fmt.Sprintf("%s-%s-%d", node, g.model, i)
			var capability workercore.AcceleratorStatus
			if c.logical {
				capability.LogicalSliced.Count = 10
			}
			if c.partitioned {
				capability.PhysicalSliced.Count = 1
			}
			spec = append(spec, workercore.Accelerator{ID: id, Index: uint32(i), Status: capability})
			status = append(status, workercore.AcceleratorAllocation{
				ID: id, Index: uint32(i), Mode: c.mode, Remaining: c.remaining, AllocatedSlices: c.slices,
			})
		}
		d.Spec.Groups = append(d.Spec.Groups, workercore.DevicesGroup{ID: g.model, Manufacturer: "nvidia", Accelerators: spec})
		d.Status.Groups = append(d.Status.Groups, workercore.DevicesAllocationGroup{ID: g.model, Manufacturer: "nvidia", Accelerators: status})
	}
	return d
}

func fitNode(name string, managed bool, labels map[string]string) *core.Node {
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if managed {
		nd.Labels[systemname.ManagedLabelKey] = "true"
	}
	maps.Copy(nd.Labels, labels)
	return nd
}

var (
	_t4Sliced = nodefeature.FitSlicedMaxFreeUnitsLabelKey("nvidia-t4")
	_t4Shared = nodefeature.FitSharedFreeCardsLabelKey("nvidia-t4")
	_l4Sliced = nodefeature.FitSlicedMaxFreeUnitsLabelKey("nvidia-l4")
	_l4Shared = nodefeature.FitSharedFreeCardsLabelKey("nvidia-l4")
)

func TestDesiredFitLabels(t *testing.T) {
	cases := []struct {
		name string
		node *core.Node
		devs *workercore.Devices
		want map[string]string
	}{
		{
			name: "an unmanaged node carries none",
			node: fitNode("n", false, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000)}}),
		},
		{
			name: "a node without a ledger carries none",
			node: fitNode("n", true, nil),
		},
		{
			name: "the largest free card wins, not the sum",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(640000), freeCard(640000), freeCard(1600000)}}),
			want: map[string]string{_t4Sliced: "1600000", _t4Shared: "3"},
		},
		{
			name: "a fragmented node reports its largest single card",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(640000), freeCard(640000), freeCard(640000), freeCard(640000)}}),
			want: map[string]string{_t4Sliced: "640000", _t4Shared: "4"},
		},
		{
			name: "a card another mode holds grants no share, and a sliced card still counts for a slice",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{
				{remaining: 0, mode: workercore.DeviceAllocationModeExclusive, logical: true},
				{remaining: 800000, mode: workercore.DeviceAllocationModeSliced, logical: true},
				{remaining: 480000, mode: workercore.DeviceAllocationModeShared, logical: true},
			}}),
			want: map[string]string{_t4Sliced: "800000", _t4Shared: "1"},
		},
		{
			name: "a card with less than one share left grants none",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(159999)}}),
			want: map[string]string{_t4Sliced: "159999", _t4Shared: "0"},
		},
		{
			name: "a card without logical slicing serves no slice but can grant a share",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{{remaining: 1600000}}}),
			want: map[string]string{_t4Shared: "1"},
		},
		{
			name: "a partitioned card is in neither population",
			node: fitNode("n", true, nil),
			devs: fitDevices("n", fitGroup{"t4", []fitCard{{remaining: 1600000, logical: true, partitioned: true}}}),
			want: map[string]string{},
		},
		{
			name: "a group ID that is not label grammar gets no label, and the others still do",
			node: fitNode("n", true, nil),
			devs: fitDevices("n",
				fitGroup{"Bad Group", []fitCard{freeCard(1600000)}},
				fitGroup{"t4", []fitCard{freeCard(640000)}},
			),
			want: map[string]string{_t4Sliced: "640000", _t4Shared: "1"},
		},
		{
			name: "each model gets its own pair from its own cards",
			node: fitNode("n", true, nil),
			devs: fitDevices("n",
				fitGroup{"t4", []fitCard{freeCard(640000)}},
				fitGroup{"l4", []fitCard{freeCard(1600000), freeCard(160000)}},
			),
			want: map[string]string{
				_t4Sliced: "640000", _t4Shared: "1",
				_l4Sliced: "1600000", _l4Shared: "2",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := desiredFitLabels(c.node, c.devs)
			if len(c.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestBuildFitLabelPatch(t *testing.T) {
	cases := []struct {
		name    string
		desired map[string]string
		current map[string]string
		want    map[string]any
	}{
		{
			name:    "equal writes nothing",
			desired: map[string]string{_t4Sliced: "640000"},
			current: map[string]string{_t4Sliced: "640000", "zone": "a"},
		},
		{
			name:    "a changed and a new value are set",
			desired: map[string]string{_t4Sliced: "1600000", _t4Shared: "4"},
			current: map[string]string{_t4Sliced: "640000", "zone": "a"},
			want:    map[string]any{_t4Sliced: "1600000", _t4Shared: "4"},
		},
		{
			name:    "a stale fit label is removed and a foreign one is left alone",
			desired: map[string]string{},
			current: map[string]string{
				_t4Sliced: "640000",
				"zone":    "a",
				nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-t4.count": "4",
			},
			want: map[string]any{_t4Sliced: nil},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, buildFitLabelPatch(c.desired, c.current))
		})
	}
}

// TestFitLabelsAgreeWithNodeDevicesFeasibility pins the property the labels exist for: for a single
// Pod asking for one card, the label admits a node exactly when the node-devices check, judging that
// node's cards, answers Ready.
func TestFitLabelsAgreeWithNodeDevicesFeasibility(t *testing.T) {
	ledgers := []struct {
		name  string
		cards []fitCard
	}{
		{"empty node", []fitCard{freeCard(1600000), freeCard(1600000), freeCard(1600000), freeCard(1600000)}},
		{"the sum fits and no card does", []fitCard{freeCard(640000), freeCard(640000), freeCard(640000), freeCard(640000)}},
		{"one card has room", []fitCard{freeCard(640000), freeCard(640000), freeCard(960000), freeCard(160000)}},
		{"an exclusive and a sliced card", []fitCard{{remaining: 0, mode: workercore.DeviceAllocationModeExclusive, logical: true}, {remaining: 800000, mode: workercore.DeviceAllocationModeSliced, logical: true}}},
		{"shares held on two cards", []fitCard{{remaining: 480000, mode: workercore.DeviceAllocationModeShared, logical: true}, {remaining: 160000, mode: workercore.DeviceAllocationModeShared, logical: true}, freeCard(100000)}},
		{"a partitioned card beside a free", []fitCard{{remaining: 1600000, logical: true, partitioned: true}, freeCard(320000)}},
		{"a card without logical slicing", []fitCard{{remaining: 1600000}}},
		{"less than a share on every card", []fitCard{freeCard(159999), freeCard(1)}},
		{"every card fully held exclusively", []fitCard{{mode: workercore.DeviceAllocationModeExclusive, logical: true}}},
		{"the freest card has no slot left", []fitCard{{remaining: 960000, mode: workercore.DeviceAllocationModeSliced, logical: true, slices: 10}, {remaining: 160000, mode: workercore.DeviceAllocationModeSliced, logical: true, slices: 2}}},
		{"the freest card has one slot left", []fitCard{{remaining: 960000, mode: workercore.DeviceAllocationModeSliced, logical: true, slices: 9}, {remaining: 160000, mode: workercore.DeviceAllocationModeSliced, logical: true, slices: 2}}},
	}
	node := fitNode("n", true, nil)
	for _, l := range ledgers {
		name := l.name
		devs := fitDevices("n", fitGroup{"t4", l.cards})
		labels := desiredFitLabels(node, devs)
		labelAdmits := func(t *testing.T, key string, threshold int64) bool {
			v, ok := labels[key]
			if !ok {
				return false
			}
			n, err := strconv.ParseInt(v, 10, 64)
			require.NoError(t, err)
			return n > threshold
		}
		for _, units := range []int32{1, 160000, 320000, 640000, 800000, 960000, 1600000} {
			t.Run(fmt.Sprintf("%s/slice of %d units", name, units), func(t *testing.T) {
				state, _ := feasibilityOfOnePool([]workercore.Devices{*devs}, []familyDemand{{
					family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: units,
				}})
				assert.Equal(t, state == kueue.CheckStateReady, labelAdmits(t, _t4Sliced, int64(units)-1))
			})
		}
		for _, n := range []int32{1, 2, 3, 4} {
			t.Run(fmt.Sprintf("%s/shared on %d cards", name, n), func(t *testing.T) {
				state, _ := feasibilityOfOnePool([]workercore.Devices{*devs}, []familyDemand{{
					family: nodefeature.ResourceFamilyShared, cards: n,
					sharedNeeds: []sharedNeed{{cards: n, containers: 1}},
				}})
				assert.Equal(t, state == kueue.CheckStateReady, labelAdmits(t, _t4Shared, int64(n)-1))
			})
		}
	}
}

func TestFitSignatureChanged(t *testing.T) {
	base := fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(640000)}})
	cases := []struct {
		name string
		next *workercore.Devices
		want bool
	}{
		{
			name: "a card that is not the largest shrinks and still grants a share",
			next: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(480000)}}),
			want: false,
		},
		{
			name: "the largest card shrinks",
			next: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(960000), freeCard(640000)}}),
			want: true,
		},
		{
			name: "a card runs out of shares",
			next: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(100000)}}),
			want: true,
		},
		{
			name: "a model appears",
			next: fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(640000)}}, fitGroup{"l4", []fitCard{freeCard(1600000)}}),
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, fitSignatureChanged(base, c.next))
		})
	}
}

func TestNodeFitLabelNodeUpdated(t *testing.T) {
	cases := []struct {
		name     string
		old, new *core.Node
		want     bool
	}{
		{
			name: "the managed mark flips",
			old:  fitNode("n", false, nil),
			new:  fitNode("n", true, nil),
			want: true,
		},
		{
			name: "a fit label is edited by someone else",
			old:  fitNode("n", true, map[string]string{_t4Sliced: "640000"}),
			new:  fitNode("n", true, map[string]string{_t4Sliced: "1"}),
			want: true,
		},
		{
			name: "an unrelated label changes",
			old:  fitNode("n", true, map[string]string{"zone": "a"}),
			new:  fitNode("n", true, map[string]string{"zone": "b"}),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, nodeFitLabelNodeUpdated(c.old, c.new))
		})
	}
}

func TestNodeFitLabelReconciler_Reconcile(t *testing.T) {
	node := fitNode("n", true, map[string]string{
		"zone":    "a",
		_l4Sliced: "1600000", // a model the node no longer has
	})
	devs := fitDevices("n", fitGroup{"t4", []fitCard{freeCard(640000), freeCard(1600000)}})
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, devs).Build()
	r := &NodeFitLabelReconciler{Client: cli}
	req := ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: "n"}}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	got := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, got))
	assert.Equal(t, map[string]string{
		systemname.ManagedLabelKey: "true",
		"zone":                     "a",
		_t4Sliced:                  "1600000",
		_t4Shared:                  "2",
	}, got.Labels)

	// A second pass computes the same values and writes nothing.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	again := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, again))
	assert.Equal(t, got.ResourceVersion, again.ResourceVersion)
}

func TestFitDevicesUpdated(t *testing.T) {
	unmanaged := func(d *workercore.Devices) *workercore.Devices {
		d.Labels = nil
		return d
	}
	cases := []struct {
		name     string
		old, new *workercore.Devices
		want     bool
	}{
		{
			name: "the managed mark is synced onto a ledger whose values did not move",
			old:  unmanaged(fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000)}})),
			new:  fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000)}}),
			want: true,
		},
		{
			name: "a managed ledger's fit value moves",
			old:  fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000)}}),
			new:  fitDevices("n", fitGroup{"t4", []fitCard{freeCard(640000)}}),
			want: true,
		},
		{
			name: "an unmanaged ledger's fit value moves",
			old:  unmanaged(fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000)}})),
			new:  unmanaged(fitDevices("n", fitGroup{"t4", []fitCard{freeCard(640000)}})),
			want: true,
		},
		{
			name: "a managed ledger changes without moving a fit value",
			old:  fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(640000)}}),
			new:  fitDevices("n", fitGroup{"t4", []fitCard{freeCard(1600000), freeCard(480000)}}),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, fitDevicesUpdated(c.old, c.new))
		})
	}
}
