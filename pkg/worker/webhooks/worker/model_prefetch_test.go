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
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func newModelPrefetchWebhook(objs ...ctrlcli.Object) *ModelPrefetchWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()

	return &ModelPrefetchWebhook{Client: cli, APIReader: cli}
}

// the fixture: one store granting pool=h100, one node in it, a grant over that store, and a
// resolved artifact of 5Gi.
func newModelPrefetchFixture(allowPinned bool, quota string, sizeBytes int64) []ctrlcli.Object {
	tr := true
	store := &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "h100"},
		Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
		},
	}
	binding := &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "cache"},
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     workercore.ModelStoreBindingQuota{Bytes: resource.MustParse(quota)},
		},
	}
	if allowPinned {
		binding.Spec.AllowPinned = &tr
	}
	artifact := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "model", UID: types.UID("artifact-uid")},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{
				ManifestDigest: "sha256:" + strings.Repeat("7", 64),
				SizeBytes:      sizeBytes,
			},
		},
	}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "h100"}}}

	return []ctrlcli.Object{store, binding, artifact, node}
}

func newModelPrefetch() *workercore.ModelPrefetch {
	return &workercore.ModelPrefetch{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "warm"},
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
			Placement: &workercore.ModelPrefetchPlacement{
				NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
			},
		},
	}
}

func TestModelPrefetchAdmission(t *testing.T) {
	t.Run("a prefetch within the grant and the budget passes", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 5<<30)...)
		_, err := r.ValidateCreate(context.Background(), newModelPrefetch())
		require.NoError(t, err)
	})

	t.Run("a prefetch naming a missing artifact is refused", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 5<<30)...)
		pf := newModelPrefetch()
		pf.Spec.ArtifactRef.Name = "ghost"
		_, err := r.ValidateCreate(context.Background(), pf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Not found")
	})

	t.Run("a prefetch of an image artifact is refused", func(t *testing.T) {
		objs := newModelPrefetchFixture(true, "10Gi", 5<<30)
		objs[2].(*workercore.ModelArtifact).Spec.Source.Image = &workercore.ModelArtifactImageSource{Reference: "registry/qwen@sha256:" + strings.Repeat("a", 64)}
		r := newModelPrefetchWebhook(objs...)
		_, err := r.ValidateCreate(context.Background(), newModelPrefetch())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "never enters the node cache")
	})

	t.Run("a prefetch naming a missing grant is refused", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 5<<30)...)
		pf := newModelPrefetch()
		pf.Spec.BindingRef.Name = "ghost"
		_, err := r.ValidateCreate(context.Background(), pf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Not found")
	})

	t.Run("a placement naming both kinds is refused", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 5<<30)...)
		pf := newModelPrefetch()
		pf.Spec.Placement.InstanceTypes = []string{"h100-8"}
		_, err := r.ValidateCreate(context.Background(), pf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both instanceTypes and nodeSelector")
	})

	t.Run("a prefetch past the budget is refused", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 20<<30)...)
		_, err := r.ValidateCreate(context.Background(), newModelPrefetch())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "past the grant's")
	})

	t.Run("pinning without the grant's permission is refused", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(false, "10Gi", 5<<30)...)
		pf := newModelPrefetch()
		pf.Spec.Retention.Pinned = true
		_, err := r.ValidateCreate(context.Background(), pf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "allowPinned")
	})

	t.Run("a sibling with no counted targets yet adds nothing to the projection", func(t *testing.T) {
		objs := newModelPrefetchFixture(true, "10Gi", 5<<30)
		// A second prefetch of the same artifact, not yet reconciled: its demand is unknown, and
		// unknown is not this admission's node count.
		objs = append(objs, &workercore.ModelPrefetch{
			ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "warm2"},
			Spec: workercore.ModelPrefetchSpec{
				ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
				BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
			},
		})
		r := newModelPrefetchWebhook(objs...)
		_, err := r.ValidateCreate(context.Background(), newModelPrefetch())
		require.NoError(t, err, "an unreconciled sibling must not be projected at this prefetch's node count")
	})

	t.Run("a target set outside the grant is refused", func(t *testing.T) {
		objs := newModelPrefetchFixture(true, "10Gi", 5<<30)
		// The grant covers only stores named "h100"-like pools; the node sits in another pool.
		for i, o := range objs {
			if s, ok := o.(*workercore.ModelStore); ok {
				s.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"pool": "elsewhere"}}
				objs[i] = s
			}
			if n, ok := o.(*core.Node); ok {
				n.Labels = map[string]string{"pool": "h100"}
			}
		}
		r := newModelPrefetchWebhook(objs...)
		_, err := r.ValidateCreate(context.Background(), newModelPrefetch())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "leaves the grant")
	})

	t.Run("an update is re-asked in full", func(t *testing.T) {
		r := newModelPrefetchWebhook(newModelPrefetchFixture(true, "10Gi", 20<<30)...)
		_, err := r.ValidateUpdate(context.Background(), newModelPrefetch(), newModelPrefetch())
		require.Error(t, err, "a retarget that busts the budget is refused as readily as a create")
	})
}
