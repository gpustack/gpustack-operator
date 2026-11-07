package broker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

func TestAuthorityInventoryRequiresCompleteEnumerationAndSurvivesRestart(t *testing.T) {
	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.RequireAuthorityInventory("node-1"))
	b.SetAssignmentsFrozen(true)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	maps := kubefake.NewSimpleClientset().CoreV1().ConfigMaps("operator")
	assert.False(t, b.ReadyForDrain())
	require.NoError(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))
	assert.True(t, b.ReadyForDrain())
	_, err := maps.Create(context.Background(), &core.ConfigMap{ObjectMeta: meta.ObjectMeta{
		Name: "model-transfer-resv-corrupt", Namespace: "operator", UID: "authority",
	}, Data: map[string]string{"reservations.json": "bad"}}, meta.CreateOptions{})
	require.NoError(t, err)
	require.Error(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))
	assert.False(t, b.ReadyForDrain())
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, b.scope, st, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	require.NoError(t, reopened.RequireAuthorityInventory("node-1"))
	require.NoError(t, reopened.Close())
	require.NoError(t, os.Remove(filepath.Join(b.root, authorityInventoryFile)))
	reopened, err = Open(b.root, b.scope, st, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.ErrorIs(t, reopened.RequireAuthorityInventory("node-1"), ErrReservationUnavailable)
}

func TestAuthorityInventoryAllowsForeignOrphanAndHoldsLocalOrphan(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.RequireAuthorityInventory("node-1"))
	b.SetAssignmentsFrozen(true)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	maps := kubefake.NewSimpleClientset().CoreV1().ConfigMaps("operator")
	epoch := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name      string
		nodeName  string
		wantError bool
	}{
		{
			name:      "orphan authority from another node does not block local inventory",
			nodeName:  "node-2",
			wantError: false,
		},
		{
			name:      "orphan authority naming this node without a local record holds inventory",
			nodeName:  "node-1",
			wantError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `{"apiVersion":1,"bindingUID":"deleted-binding","epoch":"` + epoch + `","entries":[{"reservationID":"res-1","nodeScope":"` +
				b.scope + `","ledgerIdentity":"foreign-ledger","generation":"g-1","intentUID":"intent-1","intentGeneration":1,"digest":"` +
				digest + `","namespace":"tenant-a","nodeName":"` + tc.nodeName + `","artifactUID":"artifact-1","sourceGeneration":1,"sourcePodUID":"worker-1","bytes":12,"registrations":1,"state":"admitted"}]}`
			cm := &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      AuthorityName("deleted-binding"),
					Namespace: "operator",
					UID:       "orphan-cm-uid",
				},
				Data: map[string]string{authorityDocumentKey: doc},
			}
			_ = maps.Delete(context.Background(), cm.Name, meta.DeleteOptions{})
			_, err := maps.Create(context.Background(), cm, meta.CreateOptions{})
			require.NoError(t, err)

			err = b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator")
			if tc.wantError {
				require.ErrorIs(t, err, ErrReservationUnavailable)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAuthorityInventoryPrunesDeletedReleasedBindingAndSweepsOrphanedGenerationRef(t *testing.T) {
	b, st, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.RequireAuthorityInventory("node-1"))
	b.SetAssignmentsFrozen(true)

	binding := &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{
			Name:      "binding-a",
			Namespace: "tenant-a",
			UID:       "binding-uid-1",
		},
	}
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(binding).Build()
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*core.ConfigMap)
		if object.UID == "" {
			object.UID = types.UID("authority-uid-1")
		}
		if object.ResourceVersion == "" {
			object.ResourceVersion = "1"
		}
		return false, nil, nil
	})
	maps := kube.CoreV1().ConfigMaps("operator")

	authority, grantFn, err := EnsureReservationAuthority(
		context.Background(), reader, maps, "operator",
		binding.Namespace, binding.Name, binding.UID, b.LimitsSnapshot(),
	)
	require.NoError(t, err)
	b.SetReservationAuthority(binding.UID, authority, grantFn)
	require.NoError(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))

	intent := Intent{
		Key:              "intent-key-1",
		Namespace:        "tenant-a",
		Digest:           digest,
		TotalBytes:       12,
		Grant:            Grant{Namespace: "tenant-a", Bytes: 1024, Registrations: 4, Sessions: 4},
		WorkerID:         "worker-1",
		NodeName:         "node-1",
		IntentUID:        "pf-1",
		IntentGeneration: 1,
		IntentName:       "pf",
		ArtifactName:     "model",
		BindingUID:       string(binding.UID),
		BindingNamespace: binding.Namespace,
		BindingName:      binding.Name,
		ReservationID:    "res-1",
		Registrations:    1,
		ArtifactUID:      "artifact-1",
		SourceGeneration: 1,
		ManifestSHA256:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ManifestJSON:     `{}`,
		TreePath:         "/cache/tree",
		Backend:          "nixl",
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	require.NotNil(t, assignment)

	savedCM, err := maps.Get(context.Background(), AuthorityName(binding.UID), meta.GetOptions{})
	require.NoError(t, err)

	// While the generation is still active, deleting the authority ConfigMap must fail closed.
	require.NoError(t, maps.Delete(context.Background(), savedCM.Name, meta.DeleteOptions{}))
	require.ErrorIs(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"), ErrReservationUnavailable)

	// Restore the ConfigMap, retire and release the generation, and delete the ModelStoreBinding.
	savedCM.ResourceVersion = ""
	_, err = maps.Create(context.Background(), savedCM, meta.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, b.RetireIntentAssignment(intent.Key))
	require.NoError(t, b.RetryGenerationReleases(context.Background()))
	require.NoError(t, reader.Delete(context.Background(), binding))

	// Simulate a crash that left the released generation's cache ref on disk:
	// RetryGenerationReleases sweeps it so GC is never permanently blocked.
	orphanRef := refVolumeID(b.scope, assignment.Generation)
	require.NoError(t, st.WriteRef(store.Ref{VolumeID: orphanRef, Hex: store.HexOf(digest), PodUID: "worker-1"}))
	require.NoError(t, b.RetryGenerationReleases(context.Background()))
	_, exists, err := st.ReadRef(orphanRef)
	require.NoError(t, err)
	assert.False(t, exists)

	// Once the ModelStoreBinding is deleted and all local generations are released,
	// VerifyAuthorityInventory prunes the Binding from authority-inventory.json and
	// garbage-collects the obsolete empty ConfigMap itself, so the inventory
	// keeps succeeding without the document ever being deleted by hand.
	require.NoError(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))
	assert.NotContains(t, b.inventory.Bindings, string(binding.UID))
	_, err = maps.Get(context.Background(), savedCM.Name, meta.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the empty orphan authority is collected")
}

func TestAuthorityInventoryCollectsEmptyOrphanAuthority(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.RequireAuthorityInventory("node-1"))
	b.SetAssignmentsFrozen(true)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	maps := kubefake.NewSimpleClientset().CoreV1().ConfigMaps("operator")
	epoch := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("an empty orphan whose Binding is deleted is collected", func(t *testing.T) {
		orphan := &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Name:      AuthorityName("deleted-binding"),
				Namespace: "operator",
				UID:       "orphan-cm-uid",
			},
			Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"deleted-binding","epoch":"` +
				epoch + `","entries":[]}`},
		}
		_, err := maps.Create(context.Background(), orphan, meta.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))
		_, err = maps.Get(context.Background(), orphan.Name, meta.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "the empty orphan authority is collected")
	})

	t.Run("a document with entries or an active local charge is never collected", func(t *testing.T) {
		entries := `{"apiVersion":1,"bindingUID":"retired-binding","epoch":"` + epoch + `","entries":[{"reservationID":"res-1","nodeScope":"` +
			b.scope + `","ledgerIdentity":"foreign-ledger","generation":"g-1","intentUID":"intent-1","intentGeneration":1,"digest":"` +
			digest + `","namespace":"tenant-a","nodeName":"node-2","artifactUID":"artifact-1","sourceGeneration":1,"sourcePodUID":"worker-1","bytes":12,"registrations":1,"state":"admitted"}]}`
		foreign := &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Name:      AuthorityName("retired-binding"),
				Namespace: "operator",
				UID:       "foreign-cm-uid",
			},
			Data: map[string]string{authorityDocumentKey: entries},
		}
		_, err := maps.Create(context.Background(), foreign, meta.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, b.VerifyAuthorityInventory(context.Background(), reader, maps, "operator"))
		_, err = maps.Get(context.Background(), foreign.Name, meta.GetOptions{})
		require.NoError(t, err, "a document another node still holds an entry in stays")
	})
}

func TestAuthorityInventoryOrphanDeleteFailureDoesNotInvalidateServing(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			b, _, _ := newBroker(t, defaultLimits())
			require.NoError(t, b.RequireAuthorityInventory("node-1"))
			b.SetAssignmentsFrozen(true)
			reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{
				Name: AuthorityName("deleted-binding"), Namespace: "operator", UID: "orphan", ResourceVersion: "7",
			}, Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"deleted-binding","epoch":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","entries":[]}`}}
			client := kubefake.NewSimpleClientset(cm)
			client.PrependReactor("delete", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				require.NotNil(t, options.Preconditions)
				require.Equal(t, cm.UID, *options.Preconditions.UID)
				require.Equal(t, cm.ResourceVersion, *options.Preconditions.ResourceVersion)
				if conflict {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("changed"))
				}
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("denied"))
			})
			err := b.VerifyAuthorityInventory(context.Background(), reader, client.CoreV1().ConfigMaps("operator"), "operator")
			require.NoError(t, err)
			assert.True(t, b.ReadyForDrain())
			_, err = client.CoreV1().ConfigMaps("operator").Get(context.Background(), cm.Name, meta.GetOptions{})
			require.NoError(t, err)
		})
	}
}
