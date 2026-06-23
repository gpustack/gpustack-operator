package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// TestNodeFeatureReconciler_PreservesSlicedPartitions verifies the reconciler merges
// only the capacity-derived labels it owns: admin-authored ".sliced.partitions"
// labels on the worker NodeFeature survive (a full Spec overwrite would wipe them),
// capacity-derived labels still converge, and removing the admin label drops it.
func TestNodeFeatureReconciler_PreservesSlicedPartitions(t *testing.T) {
	const nodeName = "node-5"
	partitionsKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g" + nodefeature.SlicedPartitionsLabelSuffix

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{systemname.ManagedLabelKey: "true"},
		},
		Status: core.NodeStatus{
			Capacity: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("8"),
				core.ResourceMemory: resource.MustParse("32Gi"),
			},
		},
	}
	// The worker NodeFeature the admin edits: it already carries the slicing label.
	workerNf := &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{
			Name:      nodeName + "-gpustack-worker",
			Namespace: kuberess.SystemNamespaceName,
		},
		Spec: func() nfd.NodeFeatureSpec {
			s := nfd.NewNodeFeatureSpec()
			s.Labels = map[string]string{partitionsKey: "8"}
			return *s
		}(),
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node, workerNf).
		Build()
	r := &NodeFeatureReconciler{Client: cli}
	reconcile := func() error {
		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
		return err
	}
	getNf := func() *nfd.NodeFeature {
		nf := new(nfd.NodeFeature)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: workerNf.Name, Namespace: workerNf.Namespace}, nf))
		return nf
	}

	t.Run("admin slicing label survives and capacity labels converge", func(t *testing.T) {
		require.NoError(t, reconcile())
		nf := getNf()
		assert.Equal(t, "8", nf.Spec.Labels[partitionsKey], "sliced.partitions preserved")
		assert.Equal(t, "true", nf.Spec.Labels[systemname.ManagedLabelKey], "capacity labels converged")
		hasGeneral := false
		for k := range nf.Spec.Labels {
			if strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) {
				hasGeneral = true
				break
			}
		}
		assert.True(t, hasGeneral, "capacity-derived general labels merged in, not just the preserved key")
	})

	t.Run("re-reconcile is idempotent", func(t *testing.T) {
		require.NoError(t, reconcile())
		assert.Equal(t, "8", getNf().Spec.Labels[partitionsKey], "still preserved after no-op reconcile")
	})

	t.Run("removing the admin label drops it", func(t *testing.T) {
		nf := getNf()
		delete(nf.Spec.Labels, partitionsKey)
		require.NoError(t, cli.Update(context.Background(), nf))

		require.NoError(t, reconcile())
		_, present := getNf().Spec.Labels[partitionsKey]
		assert.False(t, present, "sliced.partitions no longer preserved once the admin removes it")
	})
}

// TestNodeFeatureReconciler_DropsStaleOwnedLabels verifies the reconciler converges
// spec.labels to the capacity-derived set it owns: a stale owned label that the node
// no longer derives (e.g. after an accelerator model swap) is removed, so dead
// flavors/queues stop being advertised, while the admin-authored ".sliced.partitions"
// opt-in still survives.
func TestNodeFeatureReconciler_DropsStaleOwnedLabels(t *testing.T) {
	const nodeName = "node-6"
	partitionsKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g" + nodefeature.SlicedPartitionsLabelSuffix
	// An owned acceleratable label for a model the node no longer reports: eNf never
	// regenerates it (the node has no accelerator capacity), and it is not a
	// ".sliced.partitions" opt-in, so the reconciler must drop it.
	staleKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g.z-flavor"

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{systemname.ManagedLabelKey: "true"},
		},
		Status: core.NodeStatus{
			Capacity: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("8"),
				core.ResourceMemory: resource.MustParse("32Gi"),
			},
		},
	}
	workerNf := &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{
			Name:      nodeName + "-gpustack-worker",
			Namespace: kuberess.SystemNamespaceName,
		},
		Spec: func() nfd.NodeFeatureSpec {
			s := nfd.NewNodeFeatureSpec()
			s.Labels = map[string]string{partitionsKey: "8", staleKey: "8c-32g-100g-1d"}
			return *s
		}(),
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node, workerNf).
		Build()
	r := &NodeFeatureReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
	require.NoError(t, err)

	nf := new(nfd.NodeFeature)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Name: workerNf.Name, Namespace: workerNf.Namespace}, nf))

	_, present := nf.Spec.Labels[staleKey]
	assert.False(t, present, "stale owned label converged away")
	assert.Equal(t, "8", nf.Spec.Labels[partitionsKey], "admin sliced.partitions still preserved")
}
