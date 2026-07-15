package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

func TestNodeDevicesReconciler_SyncsManagedLabel(t *testing.T) {
	const node = "node-a"
	cases := []struct {
		name       string
		nodeLabels map[string]string
		devLabels  map[string]string
		want       string // expected managed value on the Devices; "" → label absent
	}{
		{
			name:       "stamps managed when the node is managed",
			nodeLabels: map[string]string{systemname.ManagedLabelKey: "true"},
			devLabels:  map[string]string{"feature.gpustack.ai/x": "true"},
			want:       "true",
		},
		{
			name:       "removes managed when the node carries none",
			nodeLabels: map[string]string{},
			devLabels:  map[string]string{systemname.ManagedLabelKey: "true"},
			want:       "",
		},
		{
			name:       "mirrors the node's explicit value",
			nodeLabels: map[string]string{systemname.ManagedLabelKey: "false"},
			devLabels:  map[string]string{systemname.ManagedLabelKey: "true"},
			want:       "false",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: node, Labels: c.nodeLabels}}
			devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: node, Labels: c.devLabels}}
			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(nd, devs).
				Build()
			r := &NodeDevicesReconciler{Client: cli}

			_, err := r.Reconcile(context.Background(),
				ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node}})
			require.NoError(t, err)

			got := new(workercore.Devices)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, got))
			assert.Equal(t, c.want, got.Labels[systemname.ManagedLabelKey])
		})
	}
}

// TestNodeDevicesReconciler_SyncsGeneralKey pins that the reconciler stamps the node's REAL
// general(CPU) key onto the Devices and drops a stale one (the "generic" sentinel the DeviceManager
// can no longer compute), so the node-devices AdmissionCheck — which locates a pool's Devices by the
// accelerated ResourceFlavor's real-CPU nodeLabels — actually finds them. The DeviceManager's own
// accelerator selector label is left untouched.
func TestNodeDevicesReconciler_SyncsGeneralKey(t *testing.T) {
	const node = "node-a"
	// A node whose CPU identity resolves to a real key (vendor + family/id), not the generic sentinel.
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: node, Labels: map[string]string{
		systemname.ManagedLabelKey:                       "true",
		"feature.node.kubernetes.io/cpu-model.vendor_id": "AuthenticAMD",
		"feature.node.kubernetes.io/cpu-model.family":    "25",
		"feature.node.kubernetes.io/cpu-model.id":        "1",
	}}}
	realKey := nodefeature.GeneralFeatureLabelPrefix + nodefeature.ExtractGeneralNodeKey(nd)
	staleKey := nodefeature.GeneralFeatureLabelPrefix + nodefeature.GeneralGroupGeneric
	accelKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	require.NotEqual(t, staleKey, realKey, "fixture must resolve to a real, non-generic CPU key")

	devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: node, Labels: map[string]string{
		staleKey: "true", // a stale generic key to be dropped
		accelKey: "true", // the DeviceManager's accelerator selector label, to be preserved
	}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nd, devs).Build()
	r := &NodeDevicesReconciler{Client: cli}

	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node}})
	require.NoError(t, err)

	got := new(workercore.Devices)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, got))
	assert.Equal(t, "true", got.Labels[realKey], "the real CPU key is stamped")
	assert.NotContains(t, got.Labels, staleKey, "the stale generic key is dropped")
	assert.Equal(t, "true", got.Labels[systemname.ManagedLabelKey], "managed still synced")
	assert.Equal(t, "true", got.Labels[accelKey], "the DeviceManager's accelerator label is left untouched")
}

// TestNodeDevicesReconciler_MissingDevicesIsNoop pins that a Node with no Devices
// object (e.g. not yet reported by the DeviceManager) reconciles without error.
func TestNodeDevicesReconciler_MissingDevicesIsNoop(t *testing.T) {
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{systemname.ManagedLabelKey: "true"},
	}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nd).Build()
	r := &NodeDevicesReconciler{Client: cli}

	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "node-a"}})
	assert.NoError(t, err)
}

// TestNodeDevicesControlInSync pins that the sync/predicate comparison looks ONLY at the worker-owned
// control labels — the managed mark and the general(CPU) key(s) — and ignores the DeviceManager-owned
// accelerator (acceleratable.) selector keys. It is the mirror of TestAcceleratableDevicesSelectorLabels:
// there the accelerator keys survive and the general key is dropped; here the managed mark + general
// key are what matter and the accelerator keys are what get ignored.
func TestNodeDevicesControlInSync(t *testing.T) {
	const (
		managed = systemname.ManagedLabelKey
		gKey    = nodefeature.GeneralFeatureLabelPrefix + "amd-epyc-7r32"
		gKey2   = nodefeature.GeneralFeatureLabelPrefix + "intel-xeon-8358"
		generic = nodefeature.GeneralFeatureLabelPrefix + "generic"
		aKey    = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g"
		aKey2   = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	)
	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"both empty", map[string]string{}, map[string]string{}, true},
		{
			name: "identical control labels",
			a:    map[string]string{managed: "true", gKey: "true"},
			b:    map[string]string{managed: "true", gKey: "true"},
			want: true,
		},
		{
			// The point: differing accelerator keys must NOT read as drift — the DeviceManager owns
			// them (the mirror of TestAcceleratableDevicesSelectorLabels, where they are what survive).
			name: "accelerator keys differ but are ignored",
			a:    map[string]string{managed: "true", gKey: "true", aKey: "true"},
			b:    map[string]string{managed: "true", gKey: "true", aKey2: "true"},
			want: true,
		},
		{
			name: "managed value differs",
			a:    map[string]string{managed: "true", gKey: "true"},
			b:    map[string]string{managed: "false", gKey: "true"},
			want: false,
		},
		{
			name: "managed present vs absent",
			a:    map[string]string{managed: "true", gKey: "true"},
			b:    map[string]string{gKey: "true"},
			want: false,
		},
		{
			name: "general key differs",
			a:    map[string]string{gKey: "true"},
			b:    map[string]string{gKey2: "true"},
			want: false,
		},
		{
			name: "a stale extra general key reads as drift",
			a:    map[string]string{gKey: "true", generic: "true"},
			b:    map[string]string{gKey: "true"},
			want: false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, nodeDevicesControlInSync(c.a, c.b))
			assert.Equal(t, c.want, nodeDevicesControlInSync(c.b, c.a), "comparison is symmetric")
		})
	}
}

// TestNodeDevicesControlLabels is the worker-side mirror of detector.TestAcceleratableDevicesSelectorLabels:
// the DeviceManager stamps the accelerator (acceleratable.) keys + os/arch and drops managed + the CPU
// key, while the worker stamps EXACTLY the managed mark + the real general(CPU) key and never copies
// the accelerator keys or os/arch. Together the two builders cover a Devices object's full label set.
func TestNodeDevicesControlLabels(t *testing.T) {
	// A node with real CPU identity labels; the closure lets a case add extra node labels.
	nd := func(extra map[string]string) *core.Node {
		l := map[string]string{
			"feature.node.kubernetes.io/cpu-model.vendor_id": "AuthenticAMD",
			"feature.node.kubernetes.io/cpu-model.family":    "25",
			"feature.node.kubernetes.io/cpu-model.id":        "1",
		}
		for k, v := range extra {
			l[k] = v
		}
		return &core.Node{ObjectMeta: meta.ObjectMeta{Labels: l}}
	}
	gKey := nodefeature.GeneralFeatureLabelPrefix + nodefeature.ExtractGeneralNodeKey(nd(nil))

	cases := []struct {
		name string
		node *core.Node
		want map[string]string
	}{
		{
			// Accelerator keys + os/arch are on the node too — the worker builder must ignore them.
			name: "managed node → only the managed mark + the real general key",
			node: nd(map[string]string{
				systemname.ManagedLabelKey:                                  "true",
				nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g": "true",
				core.LabelOSStable:                                          "linux",
				core.LabelArchStable:                                        "amd64",
			}),
			want: map[string]string{systemname.ManagedLabelKey: "true", gKey: "true"},
		},
		{
			name: "unmanaged node → only the general key (no managed mark)",
			node: nd(nil),
			want: map[string]string{gKey: "true"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, nodeDevicesControlLabels(c.node))
		})
	}
}
