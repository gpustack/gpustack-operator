package detector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

// TestAcceleratableDevicesSelectorLabels pins that the Devices selector labels are derived from the
// feature labels being published this pass, NOT read back off the node. The node here carries only
// the stable os/arch (NFD has not merged the accelerator feature labels yet), yet the feature key
// must still appear in the result — this guards the real-cluster regression where a freshly
// onboarded node's Devices stayed unstamped, so the three-view and AdmissionCheck could not find it.
// It also pins what the detector STRIPS from the flavor's NodeLabels: gpustack.ai/managed and the
// general(CPU) key (both worker-owned — see worker.TestNodeDevicesControlLabels for the mirror) plus
// the .count sizing pin, leaving only the accelerator selector keys + os/arch.
func TestAcceleratableDevicesSelectorLabels(t *testing.T) {
	const featKey = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"

	// A node NFD has not yet labeled with the accelerator feature (only the stable os/arch). It is
	// managed, but that mark lives on the flavor's NodeLabels (ExtractNodeFlavors stamps
	// gpustack.ai/managed=true) and must be stripped here — the worker's NodeDevicesReconciler owns it.
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: "true",
	}}}

	cases := []struct {
		name      string
		published map[string]string
		want      map[string]string
	}{
		{
			name: "feature published this pass yields the selector labels",
			published: map[string]string{
				featKey:              "true",
				featKey + ".count":   "4",
				featKey + ".product": "Tesla-T4",
				featKey + ".memory":  "16Gi",
			},
			want: map[string]string{
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
				featKey:              "true",
			},
		},
		{
			// Even when the pass resolves a REAL CPU key (vendor + family/id present), the paired
			// general(CPU) key must be filtered out — the worker owns the CPU key on the Devices.
			name: "a resolved real CPU key is still filtered out",
			published: map[string]string{
				featKey:            "true",
				featKey + ".count": "2",
				"feature.node.kubernetes.io/cpu-model.vendor_id": "AuthenticAMD",
				"feature.node.kubernetes.io/cpu-model.family":    "25",
				"feature.node.kubernetes.io/cpu-model.id":        "1",
			},
			want: map[string]string{
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
				featKey:              "true",
			},
		},
		{
			name:      "nothing published yet yields no selector labels",
			published: map[string]string{},
			want:      nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := acceleratableDevicesSelectorLabels(node, c.published)
			assert.Equal(t, c.want, got)
			assert.NotContains(t, got, systemname.ManagedLabelKey,
				"gpustack.ai/managed (stamped on the flavor's NodeLabels) is stripped — the worker owns it")
			for k := range got {
				assert.False(t, strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix),
					"no general(CPU) key survives — the worker owns it on the Devices")
			}
		})
	}
}

// TestAlignDeviceGroups pins that an existing group's re-detected content, including its
// accelerators' slicing capability, is persisted in the aligned output rather than discarded. This
// guards the regression where the alignment indexed the freshly detected group into the existing
// group's slot, correctly marked it changed, but then rebuilt the returned list from the original
// (stale) slice — so only added/removed groups ever took effect, and a capability change on an
// existing group required deleting the node's Devices object to pick up.
func TestAlignDeviceGroups(t *testing.T) {
	const (
		manufacturer = "nvidia"
		groupID      = "group-0"
	)
	allowed := sets.New(manufacturer)

	baseGroup := func(status device.AcceleratorStatus) device.DevicesGroup {
		return device.DevicesGroup{
			ID:           groupID,
			Manufacturer: manufacturer,
			Name:         "Tesla-T4",
			Accelerators: []device.Accelerator{
				{ID: "gpu-0", Status: status},
			},
		}
	}

	cases := []struct {
		name    string
		aGroups device.DevicesGroupList
		eGroups device.DevicesGroupList
		want    device.DevicesGroupList
	}{
		{
			name: "existing group's physical slicing profile change is persisted",
			aGroups: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{}),
			},
			eGroups: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{
					PhysicalSliced: device.AcceleratorPhysicalSliced{
						Profiles: []device.AcceleratorPhysicalSlicedProfile{
							{Name: "1g.5gb", Count: 7},
						},
						Count: 7,
					},
				}),
			},
			want: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{
					PhysicalSliced: device.AcceleratorPhysicalSliced{
						Profiles: []device.AcceleratorPhysicalSlicedProfile{
							{Name: "1g.5gb", Count: 7},
						},
						Count: 7,
					},
				}),
			},
		},
		{
			name: "existing group's logical slicing count change is persisted",
			aGroups: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{
					LogicalSliced: device.AcceleratorLogicalSliced{Count: 4},
				}),
			},
			eGroups: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{
					LogicalSliced: device.AcceleratorLogicalSliced{Count: 8},
				}),
			},
			want: device.DevicesGroupList{
				baseGroup(device.AcceleratorStatus{
					LogicalSliced: device.AcceleratorLogicalSliced{Count: 8},
				}),
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, changed := alignDeviceGroups(c.aGroups, c.eGroups, allowed)
			assert.True(t, changed, "a capability change on an existing group must be reported as changed")
			assert.Equal(t, c.want, got)
		})
	}
}

// TestAlignDeviceGroupsOrder pins the order the aligned list comes back in. The alignment appends
// newly detected groups ahead of the ones the ledger already carried and then preserves whatever
// order it stored, so the stored order used to record which detection pass first saw each group —
// a node that grew a second accelerator model ended up with that model's group first, and no later
// pass ever moved it. The list is now ordered by the hardware: accelerators by their enumeration
// index, groups by manufacturer and then by the first accelerator each holds.
func TestAlignDeviceGroupsOrder(t *testing.T) {
	group := func(manufacturer, id string, indexes ...uint32) device.DevicesGroup {
		accels := make([]device.Accelerator, 0, len(indexes))
		for _, idx := range indexes {
			accels = append(accels, device.Accelerator{ID: fmt.Sprintf("%s-%d", id, idx), Index: idx})
		}
		return device.DevicesGroup{ID: id, Manufacturer: manufacturer, Accelerators: accels}
	}
	// ids flattens the aligned list into the walk order a consumer sees, so a case states one
	// expectation instead of a whole object tree. A group holding no accelerator contributes its
	// own id in parentheses: it would otherwise be invisible here, and an expectation could not
	// pin where such a group sorted.
	ids := func(groups device.DevicesGroupList) []string {
		out := make([]string, 0, len(groups))
		for i := range groups {
			if len(groups[i].Accelerators) == 0 {
				out = append(out, "("+groups[i].ID+")")
				continue
			}
			for j := range groups[i].Accelerators {
				out = append(out, groups[i].Accelerators[j].ID)
			}
		}
		return out
	}

	cases := []struct {
		name        string
		allowed     sets.Set[string]
		aGroups     device.DevicesGroupList
		eGroups     device.DevicesGroupList
		want        []string
		wantChanged bool
	}{
		{
			name:    "a newly detected group is not stored ahead of the ones already recorded",
			allowed: sets.New("nvidia"),
			aGroups: device.DevicesGroupList{group("nvidia", "l40s", 0, 1)},
			eGroups: device.DevicesGroupList{group("nvidia", "l40s", 0, 1), group("nvidia", "a10", 2)},
			want:    []string{"l40s-0", "l40s-1", "a10-2"},
			// The added group is a content change on its own.
			wantChanged: true,
		},
		{
			name:    "a stored order that is not canonical is rewritten",
			allowed: sets.New("nvidia"),
			aGroups: device.DevicesGroupList{group("nvidia", "a10", 2), group("nvidia", "l40s", 0, 1)},
			eGroups: device.DevicesGroupList{group("nvidia", "l40s", 0, 1), group("nvidia", "a10", 2)},
			want:    []string{"l40s-0", "l40s-1", "a10-2"},
			// Nothing about the hardware changed; the order alone is what has to be reported, or
			// the skewed ledger would never be rewritten.
			wantChanged: true,
		},
		{
			name:        "accelerators are ordered within their group",
			allowed:     sets.New("nvidia"),
			aGroups:     device.DevicesGroupList{group("nvidia", "l40s", 2, 0, 1)},
			eGroups:     device.DevicesGroupList{group("nvidia", "l40s", 2, 0, 1)},
			want:        []string{"l40s-0", "l40s-1", "l40s-2"},
			wantChanged: true,
		},
		{
			name:        "an already canonical list is reported unchanged",
			allowed:     sets.New("nvidia"),
			aGroups:     device.DevicesGroupList{group("nvidia", "l40s", 0, 1), group("nvidia", "a10", 2)},
			eGroups:     device.DevicesGroupList{group("nvidia", "l40s", 0, 1), group("nvidia", "a10", 2)},
			want:        []string{"l40s-0", "l40s-1", "a10-2"},
			wantChanged: false,
		},
		{
			name: "the manufacturer leads, not the index",
			// Only nvidia is detected this pass; the ascend group carries no fresh data and is
			// kept, which is what puts groups of two manufacturers through the same sort. Each
			// manufacturer numbers its own accelerators from 0, so ordering on the index alone
			// would interleave them — here the ascend group leads despite the higher index.
			allowed: sets.New("nvidia"),
			aGroups: device.DevicesGroupList{group("nvidia", "l40s", 0), group("ascend", "910b", 1)},
			eGroups: device.DevicesGroupList{group("nvidia", "l40s", 0)},
			want:    []string{"910b-1", "l40s-0"},
			// The stored order put nvidia first, so this pass rewrites it.
			wantChanged: true,
		},
		{
			name:        "a group holding no accelerator sorts last",
			allowed:     sets.New("nvidia"),
			aGroups:     device.DevicesGroupList{group("nvidia", "empty"), group("nvidia", "l40s", 0)},
			eGroups:     device.DevicesGroupList{group("nvidia", "empty"), group("nvidia", "l40s", 0)},
			want:        []string{"l40s-0", "(empty)"},
			wantChanged: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := alignDeviceGroups(c.aGroups, c.eGroups, c.allowed)
			assert.Equal(t, c.want, ids(got))
			assert.Equal(t, c.wantChanged, changed)

			// Whatever the input order, the result is a fixed point: re-aligning it reports no
			// further change, so a canonical ledger never rewrites itself.
			stable, stableChanged := alignDeviceGroups(got, c.eGroups, c.allowed)
			assert.Equal(t, ids(got), ids(stable))
			assert.False(t, stableChanged, "a second align must report nothing to change")
		})
	}
}

// TestControlOnNodeWithoutBlock pins the upgrade-across-ownership-change path: a Devices object
// created by v0.5.4 or earlier carries a NodeFeature controller reference, and the post-upgrade
// align pass must REPLACE it with the Node reference rather than append a second controller —
// the API server rejects two controller references, which is what froze every carried-over
// Devices at its pre-upgrade content (gpustack-operator#77).
func TestControlOnNodeWithoutBlock(t *testing.T) {
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-0", UID: "uid-node"}}

	t.Run("a NodeFeature controller reference from a pre-upgrade release is replaced", func(t *testing.T) {
		devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{
			Name: "node-0",
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion:         "nfd.k8s-sigs.io/v1alpha1",
					Kind:               "NodeFeature",
					Name:               "node-0",
					UID:                "uid-nodefeature",
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(false),
				},
			},
		}}

		controlOnNodeWithoutBlock(devs, nd)

		refs := devs.GetOwnerReferences()
		assert.Len(t, refs, 1, "exactly one controller reference survives")
		assert.Equal(t, "Node", refs[0].Kind)
		assert.Equal(t, nd.UID, refs[0].UID)
		assert.True(t, ptr.Deref(refs[0].Controller, false))
		assert.False(t, ptr.Deref(refs[0].BlockOwnerDeletion, true))
	})

	t.Run("an existing Node controller reference is refreshed in place", func(t *testing.T) {
		devs := &workercore.Devices{}
		kubemeta.ControlOnWithoutBlock(devs, nd, core.SchemeGroupVersion.WithKind("Node"))

		controlOnNodeWithoutBlock(devs, nd)

		refs := devs.GetOwnerReferences()
		assert.Len(t, refs, 1)
		assert.Equal(t, "Node", refs[0].Kind)
		assert.Equal(t, nd.UID, refs[0].UID)
	})

	t.Run("non-controller references of other kinds are left alone", func(t *testing.T) {
		devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{
			Name: "node-0",
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: "nfd.k8s-sigs.io/v1alpha1",
					Kind:       "NodeFeature",
					Name:       "node-0",
					UID:        "uid-nodefeature",
					Controller: ptr.To(true),
				},
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Name:       "device-manager",
					UID:        "uid-daemonset",
				},
			},
		}}

		controlOnNodeWithoutBlock(devs, nd)

		refs := devs.GetOwnerReferences()
		assert.Len(t, refs, 2, "the foreign controller is retired, the plain reference kept")
		assert.Equal(t, "DaemonSet", refs[0].Kind)
		assert.Equal(t, "Node", refs[1].Kind)
		assert.True(t, ptr.Deref(refs[1].Controller, false))
	})
}

var errDetectorPass = errors.New("pass panicked")

// scriptedPass is one answer a manufacturer's library gives: the hardware it saw, or the failure it
// raised instead. A pass that saw nothing and a pass that failed are different answers, which is what
// the cases below are about.
type scriptedPass struct {
	groups device.DevicesGroupList
	err    error
}

// scriptedDetector answers each pass with the next entry of its script and repeats the last entry once
// the script runs out, so a case says what one manufacturer's library did round by round. An empty
// monitor script mirrors the detect one — hardware that answers both questions the same way.
//
// Each pass also announces itself on a channel, which is what lets a case about the loop wait for the
// round it is about instead of sleeping for as long as one should take. A sleep would decide the case on
// the runner's load: a negative assertion made before the loop has run at all passes for the wrong
// reason.
type scriptedDetector struct {
	name    string
	detect  []scriptedPass
	monitor []scriptedPass

	detectCalls  atomic.Int64
	monitorCalls atomic.Int64
	detected     chan struct{}
	monitored    chan struct{}
}

// newScriptedDetector builds one with its announcements buffered deeply enough that no pass ever waits
// for a reader and none is lost before one arrives.
func newScriptedDetector(name string, detect, monitor []scriptedPass) *scriptedDetector {
	return &scriptedDetector{
		name:      name,
		detect:    detect,
		monitor:   monitor,
		detected:  make(chan struct{}, 64),
		monitored: make(chan struct{}, 64),
	}
}

func (f *scriptedDetector) Name() string {
	return f.name
}

func (f *scriptedDetector) DetectAccelerator(bool) (device.DevicesGroupList, error) {
	p := scriptedAnswer(f.detect, &f.detectCalls)
	announce(f.detected)
	return p.groups, p.err
}

func (f *scriptedDetector) MonitorAccelerator(bool) (device.MetricsGroupList, error) {
	script := f.monitor
	if len(script) == 0 {
		script = f.detect
	}
	p := scriptedAnswer(script, &f.monitorCalls)
	announce(f.monitored)
	if p.err != nil {
		return nil, p.err
	}
	return metricsOf(p.groups), nil
}

// scriptedAnswer returns the entry this pass is due, counting the call.
func scriptedAnswer(script []scriptedPass, calls *atomic.Int64) scriptedPass {
	if len(script) == 0 {
		return scriptedPass{}
	}
	i := int(calls.Add(1)) - 1
	if i >= len(script) {
		i = len(script) - 1
	}
	return script[i]
}

// announce records that a pass ran, and never blocks the pass to do it: a case that stopped reading has
// finished with the loop, and a full channel is not its failure.
func announce(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// awaitPasses waits for n passes of one kind to have started, so what a case asserts afterwards rests
// on the loop having got there rather than on how long the case waited.
func awaitPasses(t *testing.T, ch chan struct{}, n int, kind string) {
	t.Helper()

	for i := range n {
		select {
		case <-ch:
		case <-time.After(detectorTimeout):
			t.Fatalf("%d of %d %s passes ran", i, n, kind)
		}
	}
}

// groupsFixture is one manufacturer's detected hardware, named by its accelerators.
func groupsFixture(manufacturer string, deviceIDs ...string) device.DevicesGroupList {
	accelerators := make([]device.Accelerator, 0, len(deviceIDs))
	for i := range deviceIDs {
		accelerators = append(accelerators, device.Accelerator{ID: deviceIDs[i], Index: uint32(i)})
	}
	return device.DevicesGroupList{{
		Manufacturer: manufacturer,
		ID:           manufacturer + "-group-0",
		Accelerators: accelerators,
	}}
}

// metricsOf is the monitor pass's view of the same hardware, so a case declares its devices once and
// both passes agree on which of them exist — which is what the re-detect trigger compares.
func metricsOf(groups device.DevicesGroupList) device.MetricsGroupList {
	out := make(device.MetricsGroupList, 0, len(groups))
	for i := range groups {
		accelerators := make([]device.AcceleratorMetrics, 0, len(groups[i].Accelerators))
		for j := range groups[i].Accelerators {
			accelerators = append(accelerators, device.AcceleratorMetrics{
				ID: groups[i].Accelerators[j].ID,
			})
		}
		out = append(out, device.MetricsGroup{
			Manufacturer: groups[i].Manufacturer,
			Accelerators: accelerators,
		})
	}
	return out
}

// detectRound is what one detect pass must report: the merged groups, and the manufacturers whose
// pass could not measure theirs.
type detectRound struct {
	groups     device.DevicesGroupList
	unmeasured []string
}

// TestDetectAcceleratorFailedPass pins the difference between a manufacturer whose pass FAILED and one
// that HAS no devices. Only the second is evidence about the hardware: a detector answers an absent
// driver or an empty bus with an empty list and no error, so an error means the pass could not measure
// — and reporting nothing for it would tell the allocator the manufacturer was undetected, retire its
// device-plugin sockets and strip the node of that family's capacity keys.
func TestDetectAcceleratorFailedPass(t *testing.T) {
	testCases := []struct {
		name      string
		detectors []device.Detector
		// expected is what each successive pass must report, one entry per round.
		expected []detectRound
	}{
		{
			name: "a pass that fails after a successful one reports what was last detected",
			detectors: []device.Detector{&scriptedDetector{name: "alpha", detect: []scriptedPass{
				{groups: groupsFixture("alpha", "dev-0")},
				{err: errDetectorPass},
			}}},
			expected: []detectRound{
				{groups: groupsFixture("alpha", "dev-0")},
				{groups: groupsFixture("alpha", "dev-0"), unmeasured: []string{"alpha"}},
			},
		},
		{
			name: "a pass that has never succeeded reports nothing",
			detectors: []device.Detector{&scriptedDetector{name: "alpha", detect: []scriptedPass{
				{err: errDetectorPass},
			}}},
			expected: []detectRound{
				{unmeasured: []string{"alpha"}},
				{unmeasured: []string{"alpha"}},
			},
		},
		{
			// The one case where a manufacturer must disappear: it answered, and its answer was that
			// it holds nothing. Whatever was held for it is stale from that point on, and a later
			// failure must not resurrect a card that was pulled.
			name: "a pass that ran and found nothing drops what was last detected",
			detectors: []device.Detector{&scriptedDetector{name: "alpha", detect: []scriptedPass{
				{groups: groupsFixture("alpha", "dev-0")},
				{},
				{err: errDetectorPass},
			}}},
			expected: []detectRound{
				{groups: groupsFixture("alpha", "dev-0")},
				{},
				{unmeasured: []string{"alpha"}},
			},
		},
		{
			name: "a manufacturer that failed neither shadows nor is shadowed by one that answered",
			detectors: []device.Detector{
				&scriptedDetector{name: "alpha", detect: []scriptedPass{
					{groups: groupsFixture("alpha", "dev-0")},
					{err: errDetectorPass},
				}},
				&scriptedDetector{name: "beta", detect: []scriptedPass{
					{groups: groupsFixture("beta", "dev-0")},
				}},
			},
			expected: []detectRound{
				{groups: append(groupsFixture("alpha", "dev-0"), groupsFixture("beta", "dev-0")...)},
				{
					groups:     append(groupsFixture("alpha", "dev-0"), groupsFixture("beta", "dev-0")...),
					unmeasured: []string{"alpha"},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Detector{detectors: tc.detectors}

			for i := range tc.expected {
				grpList, unmeasured := d.DetectAccelerator(context.Background())
				assert.Equal(t, tc.expected[i].groups, grpList, "round %d groups", i+1)
				assert.Equal(t, sets.New(tc.expected[i].unmeasured...), unmeasured,
					"round %d unmeasured", i+1)
			}
		})
	}
}

// TestMonitorAcceleratorUnmeasured pins the monitor pass's half of the same distinction. A sample is
// only worth what its timestamp claims, so a manufacturer that could not be measured is absent from
// the result rather than carried over from the last tick — and named, which is what keeps its absence
// from reading as accelerators that went away.
func TestMonitorAcceleratorUnmeasured(t *testing.T) {
	testCases := []struct {
		name      string
		detectors []device.Detector
		// expectedMeasured is the manufacturers the sample must carry, in order.
		expectedMeasured []string
		// expectedUnmeasured is the manufacturers the pass must name as unmeasurable.
		expectedUnmeasured []string
	}{
		{
			name: "a pass that fails names its manufacturer and samples nothing for it",
			detectors: []device.Detector{
				&scriptedDetector{name: "alpha", monitor: []scriptedPass{{err: errDetectorPass}}},
				&scriptedDetector{name: "beta", monitor: []scriptedPass{
					{groups: groupsFixture("beta", "dev-0")},
				}},
			},
			expectedMeasured:   []string{"beta"},
			expectedUnmeasured: []string{"alpha"},
		},
		{
			// It ran, and what it measured is that nothing is there. Nothing to name.
			name: "a pass that measured nothing is not a manufacturer that could not be measured",
			detectors: []device.Detector{
				&scriptedDetector{name: "alpha", monitor: []scriptedPass{{}}},
			},
			expectedMeasured:   []string{},
			expectedUnmeasured: nil,
		},
		{
			name: "a pass that fails after measuring does not report the sample it took before",
			detectors: []device.Detector{
				&scriptedDetector{name: "alpha", monitor: []scriptedPass{
					{groups: groupsFixture("alpha", "dev-0")},
					{err: errDetectorPass},
				}},
			},
			expectedMeasured:   []string{},
			expectedUnmeasured: []string{"alpha"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Detector{detectors: tc.detectors}

			var (
				grpList    device.MetricsGroupList
				unmeasured sets.Set[string]
			)
			// The last case's script only reaches its failing entry on the second pass.
			for range 2 {
				grpList, unmeasured = d.MonitorAccelerator(context.Background())
			}

			measured := make([]string, 0, len(grpList))
			for i := range grpList {
				measured = append(measured, grpList[i].Manufacturer)
			}
			assert.Equal(t, tc.expectedMeasured, measured)
			assert.Equal(t, sets.New(tc.expectedUnmeasured...), unmeasured)
		})
	}
}

// TestMeasuredDeviceKeys pins which detected devices a monitor pass's result is compared against:
// those of the manufacturers it could measure, and no others. The keys of a manufacturer it could not
// measure are dropped from the comparison — they are missing from its result for a reason that says
// nothing about the hardware.
func TestMeasuredDeviceKeys(t *testing.T) {
	keys := sets.New(
		_DeviceKey{Manufacturer: "alpha", ID: "dev-0"},
		_DeviceKey{Manufacturer: "alpha", ID: "dev-1"},
		_DeviceKey{Manufacturer: "beta", ID: "dev-0"},
	)

	testCases := []struct {
		name       string
		unmeasured sets.Set[string]
		expected   sets.Set[_DeviceKey]
	}{
		{
			name:       "every manufacturer measured leaves the comparison whole",
			unmeasured: sets.New[string](),
			expected:   keys,
		},
		{
			name:       "a manufacturer that could not be measured takes its keys out",
			unmeasured: sets.New("alpha"),
			expected:   sets.New(_DeviceKey{Manufacturer: "beta", ID: "dev-0"}),
		},
		{
			name:       "a manufacturer nothing was detected for takes nothing out",
			unmeasured: sets.New("gamma"),
			expected:   keys,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, measuredDeviceKeys(keys, tc.unmeasured))
		})
	}
}

// detectorTimeout bounds every wait below, so a loop that never gets where a case needs it fails the
// case instead of stalling the suite.
const detectorTimeout = 10 * time.Second

// TestStartHoldsBackAFirstPassThatDetectedNothing pins what a device manager asked for exactly one
// manufacturer does before that manufacturer has ever answered. Its DaemonSet is scheduled by that
// manufacturer's PCI vendor label, so the hardware is there and the software is not yet: the round is
// held back and repeated, rather than publishing and reporting a node with no accelerators. Which is
// also what --no-fast-failed turns off — and what the code claimed to do instead was quit.
func TestStartHoldsBackAFirstPassThatDetectedNothing(t *testing.T) {
	const manufacturer = "alpha"

	testCases := []struct {
		name         string
		noFastFailed bool
		// publishes is whether the empty result reaches the allocator.
		publishes bool
		// keepsDetecting is whether the loop runs the detect round again rather than settling into
		// monitoring what it did not find.
		keepsDetecting bool
	}{
		{
			name:           "a first pass that detected nothing is held back and repeated",
			noFastFailed:   false,
			publishes:      false,
			keepsDetecting: true,
		},
		{
			name:           "--no-fast-failed publishes it like any other result",
			noFastFailed:   true,
			publishes:      true,
			keepsDetecting: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			detector := newScriptedDetector(manufacturer, []scriptedPass{{}}, nil)
			publishedCh := make(chan sets.Set[string], 8)
			d := &Detector{
				manufacturers:           sets.New(manufacturer),
				noFastFailed:            tc.noFastFailed,
				detectors:               []device.Detector{detector},
				monitorPeriod:           10 * time.Millisecond,
				detectedManufacturersCh: publishedCh,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				_ = d.Start(ctx)
			}()

			if tc.keepsDetecting {
				// A second detect pass proves the round was repeated, and that the first one is over:
				// the loop is sequential, so anything the first round was going to publish is in the
				// channel by the time the second one starts.
				awaitPasses(t, detector.detected, 2, "detect")
			} else {
				// Two monitor ticks prove the round finished and the loop settled into monitoring what
				// it did not find, rather than going back to detecting.
				awaitPasses(t, detector.detected, 1, "detect")
				awaitPasses(t, detector.monitored, 2, "monitor")
			}

			var published bool
			select {
			case set := <-publishedCh:
				published = true
				assert.Empty(t, set.UnsortedList(), "nothing was detected, so nothing can be published")
			default:
			}
			assert.Equal(t, tc.publishes, published)

			calls := detector.detectCalls.Load()
			if tc.keepsDetecting {
				assert.Greater(t, calls, int64(1),
					"the round must be repeated while the manufacturer has not answered")
			} else {
				assert.Equal(t, int64(1), calls,
					"a published result is monitored, not detected again")
			}
		})
	}
}

// TestStartKeepsAFailedManufacturerPublished is the consequence that matters, through the loop that
// draws it: the set published on detectedManufacturersCh is what the allocator reads as detected, and
// a manufacturer missing from it has its allocator stopped and its sockets retired. A detect pass that
// failed must not cost that, so the manufacturer stays published while its passes keep failing.
func TestStartKeepsAFailedManufacturerPublished(t *testing.T) {
	const manufacturer = "alpha"

	publishedCh := make(chan sets.Set[string], 8)
	d := &Detector{
		manufacturers: sets.New(manufacturer),
		detectors: []device.Detector{newScriptedDetector(manufacturer,
			// Detected once, then failing for good.
			[]scriptedPass{
				{groups: groupsFixture(manufacturer, "dev-0")},
				{err: errDetectorPass},
			},
			// Monitoring keeps working, which is what takes the loop round again.
			[]scriptedPass{{groups: groupsFixture(manufacturer, "dev-0")}},
		)},
		monitorPeriod:           10 * time.Millisecond,
		detectedManufacturersCh: publishedCh,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// NB: reporting devices fails without KUBERNETES_NODE_NAME and a loopback client, which the
		// detector logs and tolerates — and which keeps this loop detecting every period.
		_ = d.Start(ctx)
	}()

	// The first round detects; every round after it fails.
	for round := 1; round <= 3; round++ {
		select {
		case published := <-publishedCh:
			assert.True(t, published.Has(manufacturer),
				"round %d must publish the manufacturer, got %v", round, published.UnsortedList())
		case <-time.After(detectorTimeout):
			t.Fatalf("round %d published nothing", round)
		}
	}
}

// TestStartDetectsAgainAfterAFailedPass pins that reporting a manufacturer as it was last detected is
// a substitution the loop comes back from, not one it settles on: while a pass keeps failing, the
// loop keeps detecting, so the failure keeps being reported instead of being logged once and then
// looking healthy.
//
// The monitor pass here measures the card once and nothing after, so it is what takes the loop from
// the round that detected to the round that fails, and then has nothing to say — which leaves the
// failed detect pass as the only reason left to detect again.
func TestStartDetectsAgainAfterAFailedPass(t *testing.T) {
	const manufacturer = "alpha"

	publishedCh := make(chan sets.Set[string], 8)
	d := &Detector{
		manufacturers: sets.New(manufacturer),
		detectors: []device.Detector{newScriptedDetector(manufacturer,
			[]scriptedPass{
				{groups: groupsFixture(manufacturer, "dev-0")},
				{err: errDetectorPass},
			},
			[]scriptedPass{
				{groups: groupsFixture(manufacturer, "dev-0")},
				{},
			},
		)},
		monitorPeriod:           10 * time.Millisecond,
		detectedManufacturersCh: publishedCh,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = d.Start(ctx)
	}()

	for round := 1; round <= 3; round++ {
		select {
		case <-publishedCh:
		case <-time.After(detectorTimeout):
			t.Fatalf("round %d never came: the loop settled on a substituted result", round)
		}
	}
}
