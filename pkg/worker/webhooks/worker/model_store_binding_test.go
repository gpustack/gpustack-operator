package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
)

func newModelStoreBindingWebhook(objs ...ctrlcli.Object) *ModelStoreBindingWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()

	return &ModelStoreBindingWebhook{Client: cli, APIReader: cli}
}

func TestModelStoreBindingAuthorityPin(t *testing.T) {
	binding := newModelStoreBinding()
	binding.UID = "binding-uid"
	epoch := strings.Repeat("a", 64)
	pin, err := json.Marshal(broker.AuthorityPin{UID: "authority-uid", Epoch: epoch})
	require.NoError(t, err)
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: broker.AuthorityName(binding.UID), Namespace: setting.DelegatedSecretNamespace, UID: "authority-uid"}, Data: map[string]string{"reservations.json": `{"apiVersion":1,"bindingUID":"binding-uid","epoch":"` + epoch + `","entries":[]}`}}
	webhook := newModelStoreBindingWebhook(testModelStore("h100"), cm)
	pinned := binding.DeepCopy()
	pinned.Annotations = map[string]string{broker.AuthorityAnnotation: string(pin)}
	require.Error(t, validateBindingCreate(t, webhook, pinned))
	require.NoError(t, validateBindingUpdate(t, webhook, binding, pinned))
	replaced := pinned.DeepCopy()
	replaced.Annotations[broker.AuthorityAnnotation] = `{"UID":"other"}`
	require.Error(t, validateBindingUpdate(t, webhook, pinned, replaced))
	delete(replaced.Annotations, broker.AuthorityAnnotation)
	require.Error(t, validateBindingUpdate(t, webhook, pinned, replaced))
	missing := newModelStoreBindingWebhook(testModelStore("h100"))
	require.Error(t, validateBindingUpdate(t, missing, binding, pinned))
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

// TestModelStoreBindingSourceMemoryAdmission pins the grant's shape rules: all three limits are
// administrator-set, and a grant whose bytes, registrations or sessions say nothing cannot back a
// reservation.
func TestModelStoreBindingSourceMemoryAdmission(t *testing.T) {
	r := newModelStoreBindingWebhook(testModelStore("h100"))

	t.Run("a grant over an existing store passes", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.SourceMemory = &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("16Gi"),
			Registrations: 4,
			Sessions:      8,
		}
		require.NoError(t, validateBindingCreate(t, r, b))
	})

	t.Run("a grant without memory is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.SourceMemory = &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("0"),
			Registrations: 4,
			Sessions:      8,
		}
		err := validateBindingCreate(t, r, b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sourceMemory.bytes")
	})

	t.Run("a negative grant is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.SourceMemory = &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("-1Gi"),
			Registrations: 4,
			Sessions:      8,
		}
		require.Error(t, validateBindingCreate(t, r, b))
	})

	t.Run("a grant without a registration limit is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.SourceMemory = &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("16Gi"),
			Registrations: 0,
			Sessions:      8,
		}
		err := validateBindingCreate(t, r, b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "registrations")
	})

	t.Run("a grant without a session limit is refused", func(t *testing.T) {
		b := newModelStoreBinding()
		b.Spec.SourceMemory = &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("16Gi"),
			Registrations: 4,
			Sessions:      0,
		}
		err := validateBindingCreate(t, r, b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sessions")
	})
}

// TestModelStoreBindingSourceMemoryGrowthOnly pins the grant's edit rules: an admin may grant for
// the first time, grow bytes and registrations, move the session cap either way while it
// stays at one or more — sessions admitted above a lowered cap drain naturally under it — and
// remove the grant, which hands the namespace to the node's configured capacity. No edit may
// shrink bytes or registrations while the grant stands.
func TestModelStoreBindingSourceMemoryGrowthOnly(t *testing.T) {
	r := newModelStoreBindingWebhook(testModelStore("h100"))

	newGrant := func() *workercore.ModelStoreBindingSourceMemory {
		return &workercore.ModelStoreBindingSourceMemory{
			Bytes:         resource.MustParse("16Gi"),
			Registrations: 4,
			Sessions:      8,
		}
	}
	granted := newModelStoreBinding()
	granted.Spec.SourceMemory = newGrant()

	t.Run("granting for the first time passes", func(t *testing.T) {
		next := newModelStoreBinding()
		next.Spec.SourceMemory = newGrant()
		require.NoError(t, validateBindingUpdate(t, r, newModelStoreBinding(), next))
	})

	t.Run("growing every limit passes", func(t *testing.T) {
		grown := granted.DeepCopy()
		grown.Spec.SourceMemory.Bytes = resource.MustParse("32Gi")
		grown.Spec.SourceMemory.Registrations = 8
		grown.Spec.SourceMemory.Sessions = 16
		require.NoError(t, validateBindingUpdate(t, r, granted, grown))
	})

	t.Run("shrinking the memory is refused", func(t *testing.T) {
		shrunk := granted.DeepCopy()
		shrunk.Spec.SourceMemory.Bytes = resource.MustParse("8Gi")
		require.Error(t, validateBindingUpdate(t, r, granted, shrunk))
	})

	t.Run("shrinking the registrations is refused", func(t *testing.T) {
		shrunk := granted.DeepCopy()
		shrunk.Spec.SourceMemory.Registrations = 2
		require.Error(t, validateBindingUpdate(t, r, granted, shrunk))
	})

	t.Run("shrinking the sessions online passes", func(t *testing.T) {
		shrunk := granted.DeepCopy()
		shrunk.Spec.SourceMemory.Sessions = 2
		require.NoError(t, validateBindingUpdate(t, r, granted, shrunk))
	})

	t.Run("dropping the sessions below one is refused", func(t *testing.T) {
		drained := granted.DeepCopy()
		drained.Spec.SourceMemory.Sessions = 0
		err := validateBindingUpdate(t, r, granted, drained)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sessions")
	})

	t.Run("removing the grant passes", func(t *testing.T) {
		revoked := granted.DeepCopy()
		revoked.Spec.SourceMemory = nil
		require.NoError(t, validateBindingUpdate(t, r, granted, revoked),
			"removal makes the namespace follow the node's configured capacity")
	})

	t.Run("an untouched grant passes", func(t *testing.T) {
		require.NoError(t, validateBindingUpdate(t, r, granted, granted.DeepCopy()))
	})
}
