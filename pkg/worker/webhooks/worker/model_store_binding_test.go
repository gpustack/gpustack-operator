package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func newModelStoreBindingWebhook(objs ...ctrlcli.Object) *ModelStoreBindingWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()

	return &ModelStoreBindingWebhook{Client: cli, APIReader: cli}
}

func testModelStore(name string) *workercore.ModelStore {
	return &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": name}},
		},
	}
}

// newModelStoreBinding builds a Binding that passes every rule against the fixture store.
func newModelStoreBinding() *workercore.ModelStoreBinding {
	return &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "cache"},
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     workercore.ModelStoreBindingQuota{Bytes: resource.MustParse("10Gi")},
		},
	}
}

func validateBindingCreate(t *testing.T, r *ModelStoreBindingWebhook, b *workercore.ModelStoreBinding) error {
	t.Helper()
	_, err := r.ValidateCreate(context.Background(), b)

	return err
}

func validateBindingUpdate(t *testing.T, r *ModelStoreBindingWebhook, old, next *workercore.ModelStoreBinding) error {
	t.Helper()
	_, err := r.ValidateUpdate(context.Background(), old, next)

	return err
}

func TestModelStoreBindingAdmission(t *testing.T) {
	r := newModelStoreBindingWebhook(testModelStore("h100"), testModelStore("mi300"))

	t.Run("a grant over an existing store passes", func(t *testing.T) {
		require.NoError(t, validateBindingCreate(t, r, newModelStoreBinding()))
	})

	t.Run("a grant naming a store that does not exist is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.StoreRefs = []workercore.ModelStoreBindingStoreReference{{Name: "ghost"}}
		err := validateBindingCreate(t, r, b)
		require.Error(t, err)
		assert.True(t, kerrors.IsInvalid(err))
	})

	t.Run("a zero budget is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.Quota.Bytes = resource.MustParse("0")
		require.Error(t, validateBindingCreate(t, r, b))
	})

	t.Run("a negative budget is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.Quota.Bytes = resource.MustParse("-1Gi")
		require.Error(t, validateBindingCreate(t, r, b))
	})
}

func TestModelStoreBindingUpdateImmutability(t *testing.T) {
	r := newModelStoreBindingWebhook(testModelStore("h100"), testModelStore("mi300"))
	old := newModelStoreBinding()

	t.Run("a quota increase passes", func(t *testing.T) {
		grown := old.DeepCopy()
		grown.Spec.Quota.Bytes = resource.MustParse("20Gi")
		require.NoError(t, validateBindingUpdate(t, r, old, grown))
	})

	t.Run("a quota decrease is refused", func(t *testing.T) {
		shrunk := old.DeepCopy()
		shrunk.Spec.Quota.Bytes = resource.MustParse("5Gi")
		require.Error(t, validateBindingUpdate(t, r, old, shrunk))
	})

	t.Run("allowPinned may go false to true", func(t *testing.T) {
		widened := old.DeepCopy()
		tr := true
		widened.Spec.AllowPinned = &tr
		require.NoError(t, validateBindingUpdate(t, r, old, widened))
	})

	t.Run("allowPinned may not go true to false", func(t *testing.T) {
		tr := true
		oldPinned := old.DeepCopy()
		oldPinned.Spec.AllowPinned = &tr
		narrowed := oldPinned.DeepCopy()
		fl := false
		narrowed.Spec.AllowPinned = &fl
		require.Error(t, validateBindingUpdate(t, r, oldPinned, narrowed))
	})

	t.Run("the store list is frozen", func(t *testing.T) {
		moved := old.DeepCopy()
		moved.Spec.StoreRefs = []workercore.ModelStoreBindingStoreReference{{Name: "mi300"}}
		require.Error(t, validateBindingUpdate(t, r, old, moved))
	})

	t.Run("an untouched update passes", func(t *testing.T) {
		require.NoError(t, validateBindingUpdate(t, r, old, old.DeepCopy()))
	})
}
