package broker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// seedAuthority wires a second live Binding authority, the way newBroker
// wires binding-uid-1, so one test can fail one authority while another
// keeps working. It returns the underlying ConfigMap API so the test can
// take the authority document away.
func seedAuthority(t *testing.T, b *Broker, bindingUID string) coreclient.ConfigMapInterface {
	t.Helper()
	maps := kubefake.NewSimpleClientset().CoreV1().ConfigMaps("model-transfer-system")
	seeded, seedErr := maps.Create(context.Background(), &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      AuthorityName(types.UID(bindingUID)),
			Namespace: "model-transfer-system",
			UID:       types.UID("authority-uid-" + bindingUID),
		},
		Data: map[string]string{authorityDocumentKey: fmt.Sprintf(
			`{"apiVersion":1,"bindingUID":%q,"epoch":"seed-epoch","entries":[]}`, bindingUID,
		)},
	}, meta.CreateOptions{})
	require.NoError(t, seedErr)
	grant := func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{
			BindingUID: types.UID(bindingUID),
			Bytes:      1 << 40, Registrations: 1024, Sessions: 1024,
		}, nil
	}
	b.SetReservationAuthority(types.UID(bindingUID),
		NewReservationAuthority(maps, "model-transfer-system", types.UID(bindingUID),
			authorityPin{UID: string(seeded.UID), Epoch: "seed-epoch"}, grant),
		grant,
	)

	return maps
}

// admitBindingIntent fills one Binding-bound intent that never starts
// natively, so reconstruction later proves its native release.
func admitBindingIntent(t *testing.T, b *Broker, digest, key, bindingUID, reservation string) string {
	t.Helper()
	assignment, perr := b.AdmitIntentFill(t.Context(), Intent{
		Key: key, Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
		IntentUID: key + "-uid", IntentGeneration: 1, BindingUID: bindingUID,
		ReservationID: reservation, Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	})
	require.Nil(t, perr)

	return assignment.Generation
}

// seedReleasedHistory places released records beyond the retained bound
// directly in the ledger's memory, so the pass has terminal work to do.
func seedReleasedHistory(t *testing.T, b *Broker) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assignmentSizes = map[string]int{}
	for i := 1; i <= 70; i++ {
		b.records[releasedToken(i)] = &GenerationRecord{Generation: releasedToken(i), State: StateReleased}
		b.assignmentSizes[releasedToken(i)] = 1
	}
}

// TestRetryGenerationReleasesIsolatesAuthorityFailure: one generation whose
// authority CAS cannot run must not stop another generation's release, and
// must not stop the terminal sweep — the released history compacts and the
// released cache reference goes in the same pass that reports the failure.
func TestRetryGenerationReleasesIsolatesAuthorityFailure(t *testing.T) {
	b, st, digest := newBroker(t, defaultLimits())
	seedAuthority(t, b, "binding-uid-2")
	gFailed := admitBindingIntent(t, b, digest, "intent-failed", testBindingUID, "res-failed")
	gReleased := admitBindingIntent(t, b, digest, "intent-released", "binding-uid-2", "res-released")
	require.NoError(t, b.Reconstruct(func() (map[string]bool, error) { return map[string]bool{}, nil }))
	require.True(t, b.records[gFailed].NativeReleased)
	require.True(t, b.records[gReleased].NativeReleased)
	seedReleasedHistory(t, b)

	// A generation holding unknown native outcomes is never a candidate:
	// unknown sessions are a stored charge only a qualified fence clears.
	b.mu.Lock()
	b.records["g-unknown"] = &GenerationRecord{
		Generation: "g-unknown", BindingUID: testBindingUID,
		State: StateDraining, NativeReleased: true, UnknownSessions: 2,
	}
	b.recountSessions(b.records["g-unknown"])
	require.Positive(t, b.records["g-unknown"].UnknownSessions,
		"the fixture holds an unknown charge")
	// A generation without durable native proof is never a candidate.
	b.records["g-native-held"] = &GenerationRecord{
		Generation: "g-native-held", BindingUID: testBindingUID, State: StateDraining,
	}
	b.mu.Unlock()

	// The failing generation's authority document is gone: its CAS cannot
	// run. Rebinding keeps the underlying API at hand for the deletion.
	failedMaps := seedAuthority(t, b, testBindingUID)
	require.NoError(t, failedMaps.Delete(t.Context(),
		AuthorityName(types.UID(testBindingUID)), meta.DeleteOptions{}))

	err := b.RetryGenerationReleases(context.Background())
	require.Error(t, err, "the pass reports the generation it could not release")
	assert.Equal(t, protocol.KindSourceUnavailable, protocol.KindOf(err))

	b.mu.Lock()
	defer b.mu.Unlock()
	assert.Equal(t, StateDraining, b.records[gFailed].State, "the failed generation stays draining")
	assert.True(t, b.records[gFailed].NativeReleased, "its durable native proof is kept")
	assert.False(t, b.records[gFailed].AuthorityDrained, "no drain mark is fabricated")
	assert.Equal(t, StateReleased, b.records[gReleased].State,
		"the unrelated generation releases despite the failure")
	assert.Positive(t, b.records["g-unknown"].UnknownSessions, "an unknown-held generation stays held")
	assert.Equal(t, StateDraining, b.records["g-unknown"].State)
	assert.False(t, b.records["g-native-held"].NativeReleased, "a native-held generation stays held")
	assert.Len(t, b.records, 67, "the terminal sweep still compacts released history: three held generations, and the newest 64 of 71 released")
	_, oldestGone := b.records[releasedToken(1)]
	assert.False(t, oldestGone, "the oldest released records went during the same pass")
	_, recentKept := b.records[releasedToken(70)]
	assert.True(t, recentKept, "the newest released history stays")
	_, heldRef, err := st.ReadRef(refVolumeID(b.scope, gFailed))
	require.NoError(t, err)
	assert.True(t, heldRef, "a held generation keeps its cache reference")
	_, releasedRef, err := st.ReadRef(refVolumeID(b.scope, gReleased))
	require.NoError(t, err)
	assert.False(t, releasedRef, "the released generation's cache reference went")
}

// TestRetryGenerationReleasesFailsClosedOnLocalProofFailure: a record whose
// durable write fails has uncertain proof state, so the pass returns that
// failure, fabricates no release proof, and runs no terminal sweep that
// would treat the record as released.
func TestRetryGenerationReleasesFailsClosedOnLocalProofFailure(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	generation := admitBindingIntent(t, b, digest, "intent-proof", testBindingUID, "res-proof")
	require.NoError(t, b.Reconstruct(func() (map[string]bool, error) { return map[string]bool{}, nil }))
	seedReleasedHistory(t, b)

	// The ledger cannot persist: its records directory refuses new files, and
	// a directory stands at the temp path so root cannot open it as a file either.
	recordsDir := filepath.Join(b.root, "generations")
	require.NoError(t, os.MkdirAll(filepath.Join(recordsDir, generation+".json.tmp"), 0o755))
	require.NoError(t, os.Chmod(recordsDir, 0o555))

	err := b.RetryGenerationReleases(context.Background())
	require.Error(t, err)
	var proof *localProofError
	require.ErrorAs(t, err, &proof, "the pass names the local proof failure")
	assert.Equal(t, protocol.KindSourceUnavailable, proof.perr.Kind)

	b.mu.Lock()
	defer b.mu.Unlock()
	assert.Equal(t, StateDraining, b.records[generation].State)
	assert.False(t, b.records[generation].AuthorityDrained,
		"a failed durable write fabricates no release progress")
	assert.Len(t, b.records, 71, "a failed local proof runs no terminal sweep: the record and all 70 released records stay")
}
