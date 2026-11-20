package runtimecontrol

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

type trackingReader struct {
	ctrlcli.Reader
	getCalls int
}

func (r *trackingReader) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	r.getCalls++
	return r.Reader.Get(ctx, key, obj, opts...)
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, workercore.AddToScheme(s))
	return s
}

func TestLedgerConfigMapName_DNS1123AndLength(t *testing.T) {
	t.Parallel()

	uid := types.UID("2c5f6a84-9b1f-4f9b-8f8a-1f9a8d3c2e10")
	nameA := LedgerConfigMapName(uid, "prefill")
	nameB := LedgerConfigMapName(uid, "decode")
	nameLong := LedgerConfigMapName(uid, strings.Repeat("very-long-role-name-", 10))
	nameHugeUID := LedgerConfigMapName(types.UID(strings.Repeat("abcdef1234", 8)), "server")

	require.LessOrEqual(t, len(nameA), 63)
	require.LessOrEqual(t, len(nameB), 63)
	require.LessOrEqual(t, len(nameLong), 63)
	require.LessOrEqual(t, len(nameHugeUID), 63)

	require.True(t, strings.HasPrefix(nameA, "runtime-control-"+string(uid)+"-"))
	require.NotEqual(t, nameA, nameB)
	require.Equal(t, nameA, LedgerConfigMapName(uid, "prefill"))

	for _, n := range []string{nameA, nameB, nameLong, nameHugeUID} {
		require.NotEmpty(t, n)
		require.Equal(t, strings.ToLower(n), n)
		require.False(t, strings.HasPrefix(n, "-"))
		require.False(t, strings.HasSuffix(n, "-"))
	}
}

func TestLedgerStore_ReadWriteCompactionAndSeparation(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	reader := &trackingReader{Reader: cli}
	fixedNow := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)
	store := NewLedgerStore(reader, cli, WithLedgerClock(func() time.Time { return fixedNow }))

	owner := LedgerOwner{
		Namespace:  "default",
		Name:       "qwen-md",
		UID:        types.UID("2c5f6a84-9b1f-4f9b-8f8a-1f9a8d3c2e10"),
		Generation: 3,
	}
	ctx := context.Background()

	// Initial read returns empty ledger using uncached reader.
	ledger, err := store.Read(ctx, owner, "server")
	require.NoError(t, err)
	require.Equal(t, 1, reader.getCalls)
	require.Empty(t, ledger.ResourceVersion)
	require.Empty(t, ledger.Entries)

	// Record pending op before confirmed apply.
	ledger.RecordPendingOperation("pod-0", "uid-0", "boot-0", 0, "op-1", "pause=true", fixedNow)
	entry, ok := ledger.GetEntry("pod-0")
	require.True(t, ok)
	require.Equal(t, "op-1", entry.PendingOperationID)
	require.Equal(t, "pause=true", entry.PendingTargetState)
	require.Empty(t, entry.ConfirmedAppliedDigest)

	// Confirm apply persists digest, applied keys, and paused flag, clearing pending op.
	ledger.RecordConfirmedApply(
		"pod-0", "uid-0", "boot-0", 0, "sha256:aaa", 3,
		[]string{ParamSchedulingMaxBatchedTokens, ParamSchedulingMaxActiveRequests, ParamSchedulingMaxActiveRequests},
		true,
		fixedNow,
	)
	entry, ok = ledger.GetEntry("pod-0")
	require.True(t, ok)
	require.Equal(t, "sha256:aaa", entry.ConfirmedAppliedDigest)
	require.Equal(t, int64(3), entry.ObservedGeneration)
	require.Empty(t, entry.PendingOperationID)
	require.Equal(t, []string{ParamSchedulingMaxActiveRequests, ParamSchedulingMaxBatchedTokens}, entry.AppliedKeys)
	require.True(t, entry.Paused)

	// Subsequent pending op must NOT clobber ConfirmedAppliedDigest or AppliedKeys.
	ledger.RecordPendingOperation("pod-0", "uid-0", "boot-0", 0, "op-2", "pause=false", fixedNow)
	entry, _ = ledger.GetEntry("pod-0")
	require.Equal(t, "sha256:aaa", entry.ConfirmedAppliedDigest)
	require.Equal(t, "op-2", entry.PendingOperationID)
	require.Equal(t, []string{ParamSchedulingMaxActiveRequests, ParamSchedulingMaxBatchedTokens}, entry.AppliedKeys)
	require.True(t, entry.Paused)

	// Add second pod to verify compaction later.
	ledger.RecordConfirmedApply("pod-dead", "uid-dead", "boot-dead", 2, "sha256:bbb", 3, []string{ParamSchedulingCacheAwareWindow}, false, fixedNow)
	ledger.RecordConfirmedApply("pod-recreated", "uid-old", "boot-old", 1, "sha256:ccc", 3, nil, false, fixedNow)

	// Save creates owned ConfigMap.
	require.NoError(t, store.Save(ctx, owner, ledger))
	require.NotEmpty(t, ledger.ResourceVersion)

	// Re-read verifies persisted data and owner reference.
	reloaded, err := store.Read(ctx, owner, "server")
	require.NoError(t, err)
	require.Equal(t, ledger.ResourceVersion, reloaded.ResourceVersion)
	gotPod0, ok := reloaded.GetEntry("pod-0")
	require.True(t, ok)
	require.Equal(t, "sha256:aaa", gotPod0.ConfirmedAppliedDigest)
	require.Equal(t, "op-2", gotPod0.PendingOperationID)
	require.Equal(t, []string{ParamSchedulingMaxActiveRequests, ParamSchedulingMaxBatchedTokens}, gotPod0.AppliedKeys)
	require.True(t, gotPod0.Paused)

	// Compact removes dead pod and pod whose UID changed.
	removed := reloaded.Compact(map[string]types.UID{
		"pod-0":         "uid-0",
		"pod-recreated": "uid-new",
	})
	require.Equal(t, 2, removed)
	require.Len(t, reloaded.Entries, 1)
	require.NoError(t, store.Save(ctx, owner, reloaded))
}

func TestLedgerStore_OwnerMismatchAndCASAndGenerationGuards(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ownerA := LedgerOwner{
		Namespace:  "default",
		Name:       "md-a",
		UID:        types.UID("11111111-1111-1111-1111-111111111111"),
		Generation: 5,
	}
	cmName := LedgerConfigMapName(ownerA.UID, "server")

	// Pre-populate a ConfigMap owned by a DIFFERENT UID (simulating foreign object).
	foreignCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       "md-other",
				UID:        types.UID("22222222-2222-2222-2222-222222222222"),
				Controller: ptr.To(true),
			}},
		},
		Data: map[string]string{LedgerDataKey: `{}`},
	}

	cli := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(foreignCM).Build()
	store := NewLedgerStore(cli, cli)

	_, err := store.Read(ctx, ownerA, "server")
	require.ErrorIs(t, err, ErrLedgerOwnerMismatch)

	// Refuses to overwrite foreign ConfigMap on Save too.
	fakeLedger := &RoleLedger{
		Namespace:       "default",
		DeploymentName:  ownerA.Name,
		DeploymentUID:   ownerA.UID,
		RoleName:        "server",
		ResourceVersion: foreignCM.ResourceVersion,
		Entries:         map[string]PodLedgerEntry{},
	}
	err = store.Save(ctx, ownerA, fakeLedger)
	require.ErrorIs(t, err, ErrLedgerOwnerMismatch)

	// Fresh store for CAS & generation tests.
	cli2 := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	store2 := NewLedgerStore(cli2, cli2)

	l1, err := store2.Read(ctx, ownerA, "server")
	require.NoError(t, err)
	l1.RecordConfirmedApply("pod-0", "uid-0", "boot-0", 0, "sha256:111", 5, nil, false, time.Now())
	require.NoError(t, store2.Save(ctx, ownerA, l1))

	// Two concurrent readers load the same ResourceVersion.
	copyA, err := store2.Read(ctx, ownerA, "server")
	require.NoError(t, err)
	copyB, err := store2.Read(ctx, ownerA, "server")
	require.NoError(t, err)

	// First writer wins.
	copyA.RecordConfirmedApply("pod-0", "uid-0", "boot-0", 0, "sha256:222", 5, nil, false, time.Now())
	require.NoError(t, store2.Save(ctx, ownerA, copyA))

	// Second writer with stale ResourceVersion gets ErrLedgerConflict.
	copyB.RecordConfirmedApply("pod-0", "uid-0", "boot-0", 0, "sha256:333", 5, nil, false, time.Now())
	err = store2.Save(ctx, ownerA, copyB)
	require.ErrorIs(t, err, ErrLedgerConflict)
	require.True(t, IsLedgerConflict(err))

	// Stale generation writer is rejected even with fresh ResourceVersion.
	freshCopy, err := store2.Read(ctx, ownerA, "server")
	require.NoError(t, err)
	staleOwner := ownerA
	staleOwner.Generation = 4
	err = store2.Save(ctx, staleOwner, freshCopy)
	require.ErrorIs(t, err, ErrLedgerStaleGeneration)
}

func TestLedgerStore_SizeCapGuard(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	store := NewLedgerStore(cli, cli, WithMaxLedgerBytes(256))
	owner := LedgerOwner{
		Namespace:  "default",
		Name:       "md-big",
		UID:        types.UID("33333333-3333-3333-3333-333333333333"),
		Generation: 1,
	}

	ledger, err := store.Read(context.Background(), owner, "server")
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		name := "pod-" + strings.Repeat("x", 20) + string(rune('a'+i))
		ledger.RecordConfirmedApply(name, types.UID("uid-"+name), "boot-1", 0, "sha256:abc", 1, nil, false, time.Now())
	}

	err = store.Save(context.Background(), owner, ledger)
	require.ErrorIs(t, err, ErrLedgerSizeExceeded)
}

func TestLedgerStore_HelpersDeleteAndCorruptHandling(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	md := &workercore.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "md-del",
			UID:        types.UID("44444444-4444-4444-4444-444444444444"),
			Generation: 2,
		},
	}
	owner := LedgerOwnerFromModelDeployment(md)
	require.Equal(t, md.UID, owner.UID)
	require.Equal(t, LedgerOwner{}, LedgerOwnerFromModelDeployment(nil))

	// Empty UID fallback in naming is still valid DNS-1123.
	require.NotEmpty(t, LedgerConfigMapName("", "server"))

	cli := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	store := NewLedgerStore(cli, cli)

	// Validation errors on nil reader/writer or missing owner fields.
	nilStore := NewLedgerStore(nil, nil)
	_, err := nilStore.Read(ctx, owner, "server")
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorIs(t, nilStore.Save(ctx, owner, &RoleLedger{RoleName: "server"}), ErrInvalidArgument)
	require.ErrorIs(t, nilStore.Delete(ctx, owner, "server"), ErrInvalidArgument)

	// Exercise SetEntry, DeleteEntry, RecordAmbiguousOperation, and PodUID reset.
	fixedNow := time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
	ledger, err := store.Read(ctx, owner, "server")
	require.NoError(t, err)
	ledger.SetEntry("pod-1", PodLedgerEntry{
		PodUID:                 "uid-1",
		ConfirmedAppliedDigest: "sha256:aaa",
		AppliedKeys:            []string{ParamSchedulingMaxBatchedTokens, ParamSchedulingMaxActiveRequests, ""},
		Paused:                 true,
	})
	ledger.RecordAmbiguousOperation("pod-1", "uid-1", "boot-1", 1, "op-amb", "timeout on pause", fixedNow)
	e1, ok := ledger.GetEntry("pod-1")
	require.True(t, ok)
	require.Equal(t, "sha256:aaa", e1.ConfirmedAppliedDigest)
	require.Equal(t, "op-amb", e1.AmbiguousOperationID)
	require.Equal(t, []string{ParamSchedulingMaxActiveRequests, ParamSchedulingMaxBatchedTokens}, e1.AppliedKeys)
	require.True(t, e1.Paused)

	// New PodUID under same podName clears stale digest from prior Pod.
	ledger.RecordAmbiguousOperation("pod-1", "uid-2", "boot-2", 0, "op-amb-2", "timeout", fixedNow)
	e2, _ := ledger.GetEntry("pod-1")
	require.Empty(t, e2.ConfirmedAppliedDigest)
	require.Equal(t, types.UID("uid-2"), e2.PodUID)

	ledger.DeleteEntry("pod-1")
	_, ok = ledger.GetEntry("pod-1")
	require.False(t, ok)

	// Save then Delete succeeds and is idempotent on repeat Delete.
	require.NoError(t, store.Save(ctx, owner, ledger))
	require.NoError(t, store.Delete(ctx, owner, "server"))
	require.NoError(t, store.Delete(ctx, owner, "server"))

	// Corrupt JSON ConfigMap owned by this deployment returns ErrLedgerCorrupt.
	cmName := LedgerConfigMapName(owner.UID, "decode")
	corruptCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: owner.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       owner.Name,
				UID:        owner.UID,
				Controller: ptr.To(true),
			}},
		},
		Data: map[string]string{LedgerDataKey: `{not-json`},
	}
	require.NoError(t, cli.Create(ctx, corruptCM))
	_, err = store.Read(ctx, owner, "decode")
	require.ErrorIs(t, err, ErrLedgerCorrupt)
}
