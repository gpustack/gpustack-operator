package detector

import (
	"strings"
	"testing"

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
