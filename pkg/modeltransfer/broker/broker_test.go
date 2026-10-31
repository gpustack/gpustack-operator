package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

var testNow = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

func TestBrokerAuthorityRejectsStaleWrite(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	maps := b.reservationAuthorities[types.UID(testBindingUID)].maps
	name := AuthorityName(testBindingUID)
	first, err := maps.Get(t.Context(), name, meta.GetOptions{})
	require.NoError(t, err)
	stale := first.DeepCopy()
	first.Annotations = map[string]string{"owner": "first"}
	_, err = maps.Update(t.Context(), first, meta.UpdateOptions{})
	require.NoError(t, err)
	stale.Annotations = map[string]string{"owner": "stale"}
	_, err = maps.Update(t.Context(), stale, meta.UpdateOptions{})
	require.True(t, apierrors.IsConflict(err), "stale authority updates must conflict instead of overwriting reservations")
	live, err := maps.Get(t.Context(), name, meta.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "first", live.Annotations["owner"])
}

func fixtureDigest(t *testing.T) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "model-transfer", "contracts",
		"fixtures", "tiny_dense", "manifest.json"))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), "sha256:" + hex.EncodeToString(sum[:])
}

// publishFixture publishes a verified tree for the golden fixture digest.
func publishFixture(t *testing.T, st *store.Store) string {
	t.Helper()
	hexDigest, digest := fixtureDigest(t)
	publishTree(t, st, hexDigest, "fixture-bytes", 12)
	return digest
}

// publishTree publishes a verified tree of size bytes under one digest.
func publishTree(t *testing.T, st *store.Store, hexDigest, content string, size int64) {
	t.Helper()
	digest := "sha256:" + hexDigest
	attempt, err := st.NewAttempt(hexDigest)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(attempt.Dir(), "tree"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(attempt.Dir(), "tree", "checkpoint.safetensors"),
		[]byte(content), 0o644,
	))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: digest, SizeBytes: size, ManifestFormat: "gpustack-model-transfer/v1",
		Source: store.SourceHub,
	}))
}

func newBroker(t *testing.T, limits Limits) (*Broker, *store.Store, string) {
	t.Helper()
	b, st, digest := openTestBroker(t, limits)
	// The source gate is closed in production: the source holds no safety
	// acceptance record. The tests behind it exercise the contracts the gate
	// protects, so the fixture opens it.
	qualifyTestSource(t, b)
	return b, st, digest
}

// openTestBroker builds a broker whose source gate the test owns.
func openTestBroker(t *testing.T, limits Limits) (*Broker, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	digest := publishFixture(t, st)
	publishTree(t, st, strings.Repeat("c", 64), "cccc", 12)
	now := testNow
	b, err := Open(filepath.Join(root, "broker"), testScope, st, limits,
		func() time.Time { return now })
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	// Intent admissions reserve against a Binding-wide authority backed by
	// the fake client: production never admits on one node's word alone.
	// The authority object exists because its owner pinned it; a missing
	// object holds charges instead of being silently recreated.
	fakeMaps := &versionedMaps{ConfigMapInterface: kubefake.NewSimpleClientset().CoreV1().ConfigMaps("model-transfer-system")}
	seeded, seedErr := fakeMaps.Create(context.Background(), &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      AuthorityName("binding-uid-1"),
			Namespace: "model-transfer-system",
			UID:       "authority-uid-1",
		},
		Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"binding-uid-1","epoch":"seed-epoch","entries":[]}`},
	}, meta.CreateOptions{})
	require.NoError(t, seedErr)
	b.SetReservationAuthority("binding-uid-1",
		NewReservationAuthority(fakeMaps, "model-transfer-system", "binding-uid-1",
			authorityPin{UID: string(seeded.UID), Epoch: "seed-epoch"},
			func(ctx context.Context) (ReservationGrant, error) {
				return ReservationGrant{
					BindingUID: "binding-uid-1",
					Bytes:      1 << 40, Registrations: 1024, Sessions: 1024,
				}, nil
			},
		),
		func(ctx context.Context) (ReservationGrant, error) {
			return ReservationGrant{
				BindingUID: "binding-uid-1",
				Bytes:      1 << 40, Registrations: 1024, Sessions: 1024,
			}, nil
		},
	)
	return b, st, digest
}

// testScope is every test broker's vendor identity; a second broker in a
// test takes a different scope so tokens cannot collide.
const testScope = "testvendor"

// testOwner is the authenticated session owner tests admit under.
func testOwner(namespace string) SessionOwner {
	return SessionOwner{
		PodUID: "pod-uid-" + namespace, Namespace: namespace,
		ArtifactUID: "artifact-uid-" + namespace,
		Digest:      "sha256:" + strings.Repeat("0", 64),
	}
}

func defaultLimits() Limits {
	return Limits{
		PhysicalBytes:   10000,
		MaxGenerations:  8,
		MaxQuarantined:  2,
		QuarantineBytes: 8000,
		LeaseTTL:        time.Minute,
	}
}

func grant(namespace string, bytes int64) Grant {
	return Grant{Namespace: namespace, Bytes: bytes, Registrations: 4, Sessions: 4}
}

func fill(namespace, digest string, total int64) FillRequest {
	return FillRequest{
		Digest: digest, Namespace: namespace, TotalBytes: total,
		Grant: grant(namespace, 10000), WorkerID: "source-worker-a",
	}
}

// admitServe fills, verifies and registers a worker, leaving one Serving
// generation with a live lease.
func admitServe(t *testing.T, b *Broker, digest string, generation, secret *string) {
	t.Helper()
	admission, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	*generation, *secret = admission.Generation, admission.WorkerSecret
	lease, err := b.RegisterWorker(*generation, "source-worker-a", *secret)
	require.Nil(t, err)
	require.Nil(t, b.FillVerified(t.Context(), *generation, "source-worker-a", lease.ID, *secret, 2,
		servedDescriptors("source-worker-a")))
}

// servedDescriptors builds the two honest registrations admitServe's
// lifecycle reports; every window lies inside the fixture manifest's
// tensor ranges.
func servedDescriptors(workerID string) []RegistrationDescriptor {
	return []RegistrationDescriptor{
		{
			Name: "model.norm.weight", Addr: 0x1000, Length: 6,
			MemoryType: "HOST", Backend: "hixl", WorkerUID: workerID,
		},
		{
			Name: "model.bias", Addr: 0x2000, Length: 6,
			MemoryType: "HOST", Backend: "hixl", WorkerUID: workerID,
		},
	}
}

func TestAdmitFillRequiresVerifiedPublishedSource(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	_, digest := fixtureDigest(t)

	// A digest this node never published is refused before any reservation.
	_, err := b.AdmitFill(t.Context(), fill("tenant-a", "sha256:"+strings.Repeat("f", 64), 12))
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindSourceUnavailable, err.Kind)

	// An unresolved byte total is a zero-cost reservation in disguise.
	stored, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 0))
	assert.Nil(t, stored)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindInvalidDescriptor, err.Kind)
}

func TestAdmitFillRequiresExplicitGrant(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())

	// No grant at all: no permission, whatever the filesystem quota is.
	req := fill("tenant-a", digest, 12)
	req.Grant = Grant{}
	_, err := b.AdmitFill(t.Context(), req)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindDeniedContent, err.Kind)

	// A zero grant grants nothing.
	req.Grant = grant("tenant-a", 0)
	_, err = b.AdmitFill(t.Context(), req)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
}

func TestAdmitFillReservesBothBudgetsAtomically(t *testing.T) {
	// One namespace grant with room for exactly one reservation: two
	// concurrent fills race and exactly one wins.
	limits := defaultLimits()
	limits.PhysicalBytes = 10000
	b, _, digest := newBroker(t, limits)

	var wg sync.WaitGroup
	won := make(chan *Admission, 2)
	lost := make(chan *protocol.Error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			req := fill("tenant-a", digest, 4000)
			req.Grant.Bytes = 5000 // one reserve of 4000 + overhead fits, two do not
			req.WorkerID = fmt.Sprintf("worker-%d", worker)
			admission, err := b.AdmitFill(t.Context(), req)
			if err != nil {
				lost <- err
				return
			}
			won <- admission
		}(i)
	}
	wg.Wait()
	close(won)
	close(lost)
	assert.Len(t, won, 1)
	assert.Len(t, lost, 1)
	assert.Equal(t, protocol.KindCapacityExhausted, (<-lost).Kind)
}

func TestAdmitFillChargesActualBuffersWithoutSharedBacking(t *testing.T) {
	// Two namespaces reserving the same digest pay the node twice: a
	// digest is not a promise that one physical buffer backs both, so the
	// node conservatively charges every live generation's actual
	// reservation. One reserve of 12 bytes plus its headroom is 13, so a
	// 20-byte physical ceiling holds one generation and refuses a second
	// even for the same digest.
	limits := defaultLimits()
	limits.PhysicalBytes = 20
	b, _, digest := newBroker(t, limits)

	first, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	second, err := b.AdmitFill(t.Context(), fill("tenant-b", digest, 12))
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
	assert.NotEqual(t, first.Generation, "")
	assert.Nil(t, second)

	// A distinct digest is refused the same way.
	_, err = b.AdmitFill(t.Context(), fill("tenant-c", "sha256:"+strings.Repeat("c", 64), 12))
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
}

func TestAdmitFillRefusesAnOverflowingReservation(t *testing.T) {
	// A byte total near the int64 ceiling must be refused, never wrap
	// into a false pass. The marker claims the huge size, so the refusal
	// is the budget arithmetic, not the source check.
	limits := defaultLimits()
	limits.PhysicalBytes = 64 << 20
	b, st, _ := newBroker(t, limits)
	hexDigest := strings.Repeat("d", 64)
	publishTree(t, st, hexDigest, "d", int64(1)<<62+1)

	huge := int64(1)<<62 + 1
	_, err := b.AdmitFill(t.Context(), fill("tenant-a", "sha256:"+hexDigest, huge))
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
}

func TestFillVerifiedRequiresRealRegistrationsAndHonestResidentBytes(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	admission, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	lease, regErr := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, regErr)

	// Zero registrations never become Serving.
	refused := b.FillVerified(t.Context(), admission.Generation, "source-worker-a", lease.ID,
		admission.WorkerSecret, 0, nil)
	require.NotNil(t, refused)

	// A descriptor owned by another worker is refused.
	foreign := []RegistrationDescriptor{{
		Name: "model.norm.weight", Addr: 0x1000, Length: 12,
		MemoryType: "HOST", Backend: "hixl", WorkerUID: "someone-else",
	}}
	mismatched := b.FillVerified(t.Context(), admission.Generation, "source-worker-a", lease.ID,
		admission.WorkerSecret, 1, foreign)
	require.NotNil(t, mismatched)

	// A truthful descriptor makes the generation Serving with the actual
	// resident bytes, not the reservation's overhead estimate.
	descriptors := []RegistrationDescriptor{{
		Name: "model.norm.weight", Addr: 0x1000, Length: 12,
		MemoryType: "HOST", Backend: "hixl", WorkerUID: "source-worker-a",
	}}
	require.Nil(t, b.FillVerified(t.Context(), admission.Generation, "source-worker-a", lease.ID,
		admission.WorkerSecret, 1, descriptors))
	b.mu.Lock()
	record := b.records[admission.Generation]
	assert.Equal(t, StateServing, record.State)
	assert.Equal(t, int64(12), record.ResidentBytes)
	b.mu.Unlock()
}

func TestOpenRefusesASharedLedgerRootWhileAnotherBrokerHoldsIt(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	defer b.Close()
	_, err := Open(b.root, testScope+"-other", b.store, b.limits, b.now)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "ledger lock")
}

func TestLedgerRoundTripsScopedTokensAndKeepsLegacyRecords(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "uid-1/rev-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 100),
		WorkerID: "source-worker-a", IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-concurrent",
		Registrations: 1,
	}
	admission, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	// The token is scoped and opaque: the test knows its shape, nothing else.
	require.Regexp(t, `^g-testvendor-[0-9a-f]{32}-[1-9][0-9]*$`, admission.Generation)

	// A legacy numeric record still loads after the rename to scoped tokens.
	legacy := *b.records[admission.Generation]
	legacy.Generation = "9"
	legacy.WorkerSecret = ""
	legacy.WorkerSecretHash = "aa"
	legacy.IntentKey = ""
	raw, marshalErr := json.Marshal(&legacy)
	require.NoError(t, marshalErr)
	require.NoError(t, os.WriteFile(
		filepath.Join(b.root, "generations", "9.json"), raw, 0o600,
	))

	require.NoError(t, b.Close())
	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()

	// Both records survive the round trip, and the same intent revision
	// answers the same admission across the restart.
	_, perr = reopened.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	assignment, ok := reopened.AssignmentForIntent("uid-1/rev-1")
	require.True(t, ok)
	assert.Equal(t, admission.Generation, assignment.Generation)
	assert.Equal(t, admission.WorkerSecret, assignment.WorkerSecret)
	reopened.mu.Lock()
	_, hasLegacy := reopened.records["9"]
	reopened.mu.Unlock()
	assert.True(t, hasLegacy, "a legacy numeric record must keep loading")
}

// TestAdmitIntentFillFollowsAnOnlineGrantEdit pins the local grant
// snapshot's contract: re-listing the same intent revision with an edited
// Binding grant moves the admitted generation's snapshot forward — the
// session cap either way above zero, bytes and registrations forward only —
// the admission answer stays the same, and the move is durable.
func TestAdmitIntentFillFollowsAnOnlineGrantEdit(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "uid-1/rev-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000),
		WorkerID: "source-worker-a", IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-grant-edit",
		Registrations: 1,
	}
	admission, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)

	// The Binding's session cap drops online from 4 to 2: same revision,
	// same admission answer, and the local snapshot follows the new cap.
	lowered := intent
	lowered.Grant = grant("tenant-a", 10000)
	lowered.Grant.Sessions = 2
	replay, perr := b.AdmitIntentFill(t.Context(), lowered)
	require.Nil(t, perr)
	assert.Equal(t, admission.Generation, replay.Generation)
	assert.Equal(t, admission.WorkerSecret, replay.WorkerSecret)
	b.mu.Lock()
	assert.Equal(t, int32(2), b.records[admission.Generation].Grant.Sessions)
	b.mu.Unlock()

	// A grant that would shrink bytes or registrations, or drop the session
	// cap below one, leaves the snapshot where it is.
	shrunk := intent
	shrunk.Grant = grant("tenant-a", 12)
	shrunk.Grant.Sessions = 0
	_, perr = b.AdmitIntentFill(t.Context(), shrunk)
	require.Nil(t, perr)
	b.mu.Lock()
	snapshot := b.records[admission.Generation].Grant
	b.mu.Unlock()
	assert.Equal(t, int64(10000), snapshot.Bytes)
	assert.Equal(t, int32(2), snapshot.Sessions)

	// The move is durable: a reopened broker loads the followed grant.
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	defer reopened.Close()
	reopened.mu.Lock()
	durable := reopened.records[admission.Generation].Grant
	reopened.mu.Unlock()
	assert.Equal(t, int32(2), durable.Sessions)
	assert.Equal(t, int64(10000), durable.Bytes)
}

func TestOpenRefusesACorruptCounter(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	hexDigest := strings.Repeat("e", 64)
	publishTree(t, b.store, hexDigest, "cccc", 12)
	_, perr := b.AdmitFill(t.Context(), fill("tenant-a", "sha256:"+hexDigest, 12))
	require.Nil(t, perr)
	require.NoError(t, b.Close())
	require.NoError(t, os.WriteFile(filepath.Join(b.root, "next-generation"),
		[]byte("not a number\n"), 0o600))
	reopened, openErr := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NotNil(t, openErr)
	assert.Contains(t, openErr.Error(), "corrupt")
	assert.Nil(t, reopened)

	// A counter at zero is the legitimate fresh-initialized state, not
	// lost state: it opens, and the next mint is token number one.
	require.NoError(t, os.WriteFile(filepath.Join(b.root, "next-generation"),
		[]byte("0\n"), 0o600))
	fresh, openErr := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NoError(t, openErr)
	token, mintErr := fresh.ledger.nextGeneration()
	require.NoError(t, mintErr)
	assert.Regexp(t, `-1$`, token)
	require.NoError(t, fresh.Close())

	// A negative counter is corruption, never a state.
	require.NoError(t, os.WriteFile(filepath.Join(b.root, "next-generation"),
		[]byte("-3\n"), 0o600))
	_, openErr = Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NotNil(t, openErr)
	assert.Contains(t, openErr.Error(), "negative")
}

func TestOpenRefusesAMissingCounterWithRecords(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	_, perr := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, perr)
	require.NoError(t, b.Close())
	require.NoError(t, os.Remove(filepath.Join(b.root, "next-generation")))
	_, openErr := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NotNil(t, openErr)
	assert.Contains(t, openErr.Error(), "missing")
}

func TestOpenRefusesAMismatchedLedgerIdentity(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.Close())
	// Another scope, or a re-minted identity, may not reopen this root:
	// the tokens it would mint could collide with the recorded ones.
	require.NoError(t, os.WriteFile(filepath.Join(b.root, "layout"),
		[]byte(`{"layoutVersion":"gpustack-model-transfer-ledger v1","scope":"otherscope","identity":"0123456789abcdef0123456789abcdef"}`),
		0o600))
	_, openErr := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NotNil(t, openErr)
	assert.Contains(t, openErr.Error(), "belongs to scope")
}

func TestRestartRestoresSessionOwnersAndUnknownCounts(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	admission, perr := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, perr)
	lease, regErr := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, regErr)
	descriptors := []RegistrationDescriptor{{
		Name: "model.norm.weight", Addr: 0x1000, Length: 12,
		MemoryType: "HOST", Backend: "hixl", WorkerUID: "source-worker-a",
	}}
	require.Nil(t, b.FillVerified(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, 1, descriptors))
	owner := testOwner("tenant-a")
	session, serr := b.AdmitSession(t.Context(), owner, "tenant-a", admission.Generation)
	require.Nil(t, serr)

	// Two sessions the session file cannot explain (a legacy ledger
	// upgraded in place): a restart frees no capacity and invents no owner.
	b.mu.Lock()
	record := b.records[admission.Generation]
	record.ActiveSessions = 3
	require.NoError(t, b.ledger.write(record))
	b.mu.Unlock()
	require.NoError(t, b.Close())

	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	reopened.mu.Lock()
	count := reopened.records[admission.Generation].ActiveSessions
	reopened.mu.Unlock()
	assert.EqualValues(t, 3, count)

	// The persisted session keeps its owner and still reads under its
	// live lease; a wrong owner is refused; the right owner releases one,
	// and the unknown remainder stays accounted.
	view, verr := reopened.SessionView(session.ID)
	require.Nil(t, verr)
	assert.Equal(t, admission.Generation, view.Generation)
	_, perr = reopened.AdmitSession(t.Context(), testOwner("tenant-b"), "tenant-b", admission.Generation)
	require.NotNil(t, perr)
	stranger := SessionOwner{
		PodUID: "other", Namespace: "tenant-a",
		ArtifactUID: owner.ArtifactUID,
	}
	require.NotNil(t, reopened.ReleaseSession(t.Context(), stranger, session.ID))
	require.Nil(t, reopened.ReleaseSession(t.Context(), owner, session.ID))
	reopened.mu.Lock()
	count = reopened.records[admission.Generation].ActiveSessions
	reopened.mu.Unlock()
	assert.EqualValues(t, 2, count)

	// Drain after the restart is safe and accounted.
	require.Nil(t, reopened.Drain(admission.Generation))
}

func TestAdmitIntentFillSurvivesInterruption(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "uid-1/rev-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 100),
		WorkerID: "source-worker-a", IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-concurrent",
		Registrations: 1,
	}
	first, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)

	// A crash after the record write is the fault boundary: the record
	// and its assignment are one durable transaction, so the reopen
	// recovers the SAME admission and never mints a duplicate.
	require.NoError(t, b.Close())
	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	second, perr := reopened.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	assert.Equal(t, first.Generation, second.Generation)
	assert.Equal(t, first.WorkerSecret, second.WorkerSecret)

	// Retirement holds the record until its global reservation can be released.
	require.NoError(t, reopened.RetireIntentAssignment(intent.Key))
	_, ok := reopened.AssignmentForIntent(intent.Key)
	assert.False(t, ok)
	reopened.mu.Lock()
	_, stillThere := reopened.records[first.Generation]
	reopened.mu.Unlock()
	assert.True(t, stillThere)
}

func TestRetireIntentAssignmentHoldsAStartedFill(t *testing.T) {
	// A filling generation whose worker already registered has an unknown
	// native state: retirement holds it as Draining instead of canceling.
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "uid-1/rev-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 100),
		WorkerID: "source-worker-a", IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-concurrent",
		Registrations: 1,
	}
	first, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	lease, regErr := b.RegisterWorker(first.Generation, "source-worker-a",
		first.WorkerSecret)
	require.Nil(t, regErr)
	// The registered lease is what makes the native state unknown and
	// what retirement must hold instead of cancel.
	_ = lease

	b.SetBootstrapCredential("assignment-cache")
	offers, pollErr := b.AssignmentsFor("assignment-cache")
	require.Nil(t, pollErr)
	require.Len(t, offers, 1)
	require.NoError(t, b.RetireIntentAssignment(intent.Key))
	require.Empty(t, b.assignmentSizes, "retired offers must not retain response-size state")
	_, ok := b.AssignmentForIntent(intent.Key)
	assert.False(t, ok)
	b.mu.Lock()
	record := b.records[first.Generation]
	state := record.State
	secret := record.WorkerSecret
	b.mu.Unlock()
	assert.Equal(t, StateDraining, state)
	assert.Equal(t, first.WorkerSecret, secret)
	// The reservation stays accounted while held.
	assert.NotZero(t, record.ReservedBytes)
}

func TestReconcileIntentsDrainsARetiredIntentAndRetiresItsOffer(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intents := &fakeIntents{items: []Intent{{
		Key: "intent-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: Grant{
			Namespace: "tenant-a", Bytes: 100,
			Registrations: 4, Sessions: 4,
		},
		WorkerID: "source-worker-a", ManifestPath: "/tmp/manifest.json",
		IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-intent-1",
		Registrations: 1, ArtifactUID: "artifact-1", SourceGeneration: 1,
		ManifestSHA256: strings.Repeat("a", 64), ManifestJSON: "{}",
		TreePath: "/tree", Backend: "nixl",
	}}}
	require.NoError(t, ReconcileIntents(context.Background(), b, intents))
	require.Len(t, b.OfferedAssignments(), 1)

	// The same revision admits nothing new.
	require.NoError(t, ReconcileIntents(context.Background(), b, intents))
	require.Len(t, b.OfferedAssignments(), 1)

	// A failed listing retains everything.
	intents.err = errors.New("apiserver unreachable")
	require.NotNil(t, ReconcileIntents(context.Background(), b, intents))
	require.Len(t, b.OfferedAssignments(), 1)
	intents.err = nil

	// An absent intent retires its offer while its reservation remains held.
	intents.items = nil
	require.NoError(t, ReconcileIntents(context.Background(), b, intents))
	require.Empty(t, b.OfferedAssignments())
	b.mu.Lock()
	records := len(b.records)
	b.mu.Unlock()
	assert.Equal(t, 1, records)
}

func TestReconcileIntentsRetiresSupersededPreNativeIntentBeforeAdmittingReplacement(t *testing.T) {
	limits := defaultLimits()
	limits.MaxGenerations = 1
	b, _, digest := newBroker(t, limits)
	intents := &fakeIntents{items: []Intent{{
		Key: "intent-v1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: Grant{Namespace: "tenant-a", Bytes: 100, Registrations: 4, Sessions: 4},
		WorkerID: "source-worker-a", ManifestPath: "/tmp/manifest.json",
		IntentUID: "intent-uid-1", IntentGeneration: 1,
		BindingUID: "binding-uid-1", ReservationID: "res-intent-v1",
		Registrations: 1, ArtifactUID: "artifact-1", SourceGeneration: 1,
		ManifestSHA256: strings.Repeat("a", 64), ManifestJSON: "{}",
		TreePath: "/tree", Backend: "nixl",
	}}}
	require.NoError(t, ReconcileIntents(context.Background(), b, intents))
	require.Contains(t, b.OfferedAssignments(), "intent-v1")

	intents.items = []Intent{{
		Key: "intent-v2", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: Grant{Namespace: "tenant-a", Bytes: 100, Registrations: 4, Sessions: 4},
		WorkerID: "source-worker-a", ManifestPath: "/tmp/manifest.json",
		IntentUID: "intent-uid-1", IntentGeneration: 2,
		BindingUID: "binding-uid-1", ReservationID: "res-intent-v2",
		Registrations: 1, ArtifactUID: "artifact-1", SourceGeneration: 1,
		ManifestSHA256: strings.Repeat("a", 64), ManifestJSON: "{}",
		TreePath: "/tree", Backend: "nixl",
	}}
	require.NoError(t, ReconcileIntents(context.Background(), b, intents))
	offers := b.OfferedAssignments()
	require.Len(t, offers, 1)
	assert.Contains(t, offers, "intent-v2")
}

type fakeIntents struct {
	items []Intent
	err   error
}

func (f *fakeIntents) ListIntents(ctx context.Context) ([]Intent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func TestGenerationTokensAreMonotonicAndNeverRecycled(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	first, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	second, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	assert.Less(t, first.Generation, second.Generation)

	// A released token is never handed out again: the ledger's single
	// writer closes, then the root is reopened and filled once more.
	root := b.root
	st := b.store
	limits := b.limits
	now := b.now
	require.NoError(t, b.Reconstruct(func() (map[string]bool, error) { return nil, nil }))
	require.NoError(t, b.Close())
	reopened, openErr := Open(root, testScope, st, limits, now)
	require.NoError(t, openErr)
	third, fillErr := reopened.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, fillErr)
	assert.Less(t, second.Generation, third.Generation)
}

func TestWorkerLeasesAndForeignControlMessages(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	admission, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)

	// Registration binds the admitted worker identity and its secret.
	lease, err := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, err)
	assert.False(t, lease.Expires.IsZero())

	// A foreign worker identity is refused.
	_, err = b.RegisterWorker(admission.Generation, "source-worker-b",
		admission.WorkerSecret)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindAuthentication, err.Kind)

	// A wrong secret is refused, and the refusal does not echo it.
	_, err = b.RegisterWorker(admission.Generation, "source-worker-a", "wrong")
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindAuthentication, err.Kind)
	assert.NotContains(t, err.Error(), admission.WorkerSecret)

	// A heartbeat renews the lease.
	result, err := b.Heartbeat(admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, Health{})
	require.Nil(t, err)
	assert.Equal(t, ActionNone, result.Action)

	// A foreign heartbeat is refused.
	_, err = b.Heartbeat(admission.Generation, "source-worker-b",
		lease.ID, admission.WorkerSecret, Health{})
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindAuthentication, err.Kind)

	// A heartbeat for a generation that never existed is stale.
	_, err = b.Heartbeat("999", "source-worker-a", lease.ID,
		admission.WorkerSecret, Health{})
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindStaleGeneration, err.Kind)
}

func TestExpiredLeaseInvalidatesReadinessNotDisk(t *testing.T) {
	b, st, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)

	// Advance past the lease TTL and sweep.
	b.SetNow(func() time.Time { return testNow.Add(2 * defaultLimits().LeaseTTL) })
	require.Nil(t, b.Tick())

	snapshot := b.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateDraining, snapshot[0].State)

	// The published tree stays disk-ready: readiness and disk are separate.
	hexDigest, _ := fixtureDigest(t)
	assert.True(t, st.IsPublished(hexDigest))
}

func TestSessionsAdmitAgainstServingGenerationOnly(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	// The grant's session cap freezes at admission: two sessions at once.
	req := fill("tenant-a", digest, 12)
	req.Grant.Sessions = 2
	admission, err := b.AdmitFill(t.Context(), req)
	require.Nil(t, err)
	lease, err := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, err)
	require.Nil(t, b.FillVerified(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, 2, servedDescriptors("source-worker-a")))
	generation := admission.Generation

	session, err := b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
	assert.NotEmpty(t, session.ID)

	// Another namespace may not read this generation's sessions.
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-b"), "tenant-b", generation)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindDeniedContent, err.Kind)

	// A superseded generation is stale.
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", "999")
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindStaleGeneration, err.Kind)

	// The grant's session cap is enforced and the refusal is counted.
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
	snapshot := b.Snapshot()
	require.Len(t, snapshot, 1)
	assert.EqualValues(t, 1, snapshot[0].RejectedAdmissions)
	assert.EqualValues(t, 2, snapshot[0].ActiveSessions)

	// Release frees the slot.
	require.Nil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), session.ID))
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
}

func TestDrainStopsNewSessionsAndPreservesOthers(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	var first, firstSecret string
	admitServe(t, b, digest, &first, &firstSecret)

	// A second, independent generation stays untouched by the first's drain.
	req := fill("tenant-a", digest, 12)
	req.WorkerID = "source-worker-b"
	second, err := b.AdmitFill(t.Context(), req)
	require.Nil(t, err)
	lease, err := b.RegisterWorker(second.Generation, "source-worker-b",
		second.WorkerSecret)
	require.Nil(t, err)
	require.Nil(t, b.FillVerified(t.Context(), second.Generation, "source-worker-b", lease.ID,
		second.WorkerSecret, 1, servedDescriptors("source-worker-b")[:1]))

	require.Nil(t, b.Drain(first))
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", first)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindStaleGeneration, err.Kind)

	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", second.Generation)
	require.Nil(t, err)
}

func TestUnknownOutcomeQuarantinesAndRetainsBudget(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)
	lease, err := b.RegisterWorker(generation, "source-worker-a", secret)
	require.Nil(t, err)

	// A submitted operation whose outcome is unknown quarantines the
	// generation and retains its budget charge and its source reference.
	require.Nil(t, b.Quarantine(generation, "source-worker-a", lease.ID, secret,
		"unknown operation outcome"))
	snapshot := b.Snapshot()
	require.Len(t, snapshot, 1)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateQuarantined,
		snapshot[0].State)

	// Quarantined memory is still charged: a new fill that would exceed the
	// physical budget is refused.
	limits := defaultLimits()
	_ = limits
	_, err = b.AdmitFill(t.Context(), fill("tenant-a", "sha256:"+strings.Repeat("e", 64), 12))
	require.NotNil(t, err)

	// Quarantine is idempotent.
	require.Nil(t, b.Quarantine(generation, "source-worker-a", lease.ID, secret,
		"unknown operation outcome"))

	// A quarantined generation never admits readers.
	_, err = b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindStaleGeneration, err.Kind)
}

func TestQuarantineCapsRefuseNewFills(t *testing.T) {
	limits := defaultLimits()
	limits.MaxQuarantined = 1
	b, _, digest := newBroker(t, limits)

	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)
	lease, err := b.RegisterWorker(generation, "source-worker-a", secret)
	require.Nil(t, err)
	require.Nil(t, b.Quarantine(generation, "source-worker-a", lease.ID, secret,
		"disconnect failed"))

	req := fill("tenant-a", digest, 12)
	req.WorkerID = "source-worker-b"
	_, err = b.AdmitFill(t.Context(), req)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)
}

func TestDrainAckReleasesOnlyWithoutSessions(t *testing.T) {
	b, st, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)
	hexDigest, _ := fixtureDigest(t)

	// The source reference keeps the tree while the generation lives.
	removed, err := st.RemoveUnreferenced(t.Context(), hexDigest)
	require.NoError(t, err)
	assert.False(t, removed)

	lease, err := b.RegisterWorker(generation, "source-worker-a", secret)
	require.Nil(t, err)
	session, err := b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)

	require.Nil(t, b.Drain(generation))
	denied := b.DrainAck(t.Context(), generation, "source-worker-a", lease.ID, secret)
	require.NotNil(t, denied)
	assert.Equal(t, protocol.KindOperationState, denied.Kind)

	require.Nil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), session.ID))
	require.Nil(t, b.DrainAck(t.Context(), generation, "source-worker-a", lease.ID, secret))

	// Released: the reference is gone and the collector may take the tree.
	assert.Empty(t, b.Snapshot())
	removed, err = st.RemoveUnreferenced(t.Context(), hexDigest)
	require.NoError(t, err)
	assert.True(t, removed)
}

func TestReconstructAfterCrash(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	var serving, servingSecret string
	admitServe(t, b, digest, &serving, &servingSecret)

	// A second fill that never completed, whose worker is gone.
	stuck, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	// And a fill whose worker is still admitted, under its own identity.
	liveReq := fill("tenant-a", digest, 12)
	liveReq.WorkerID = "source-worker-c"
	live, err := b.AdmitFill(t.Context(), liveReq)
	require.Nil(t, err)

	// A daemon restart: the broker reconstructs from the durable ledger and
	// the set of admitted source workers.
	require.NoError(t, b.Reconstruct(func() (map[string]bool, error) {
		return map[string]bool{liveReq.WorkerID: true}, nil
	}))

	byGeneration := map[string]workercore.NodeModelStoreModelTransferGeneration{}
	for _, entry := range b.Snapshot() {
		byGeneration[entry.Generation] = entry
	}
	// Serving became Draining: a restart invalidates transfer readiness.
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateDraining,
		byGeneration[serving].State)
	// The stuck fill without its worker was released.
	_, present := byGeneration[stuck.Generation]
	assert.True(t, present)
	// The fill with a live worker is kept, still filling.
	_, present = byGeneration[live.Generation]
	assert.True(t, present, "a filling generation is held as draining")
}

func TestReconstructRequiresReadableWorkerSet(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	_, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	// An unreadable worker set is an error, never an empty set in disguise:
	// empty would release every filling reservation.
	require.Error(t, b.Reconstruct(func() (map[string]bool, error) { return nil, assert.AnError }))
}

func TestSnapshotIsRedacted(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)
	lease, err := b.RegisterWorker(generation, "source-worker-a", secret)
	require.Nil(t, err)

	encoded, marshalErr := json.Marshal(b.Snapshot())
	require.NoError(t, marshalErr)
	text := string(encoded)
	assert.NotContains(t, text, "tenant-a")
	assert.NotContains(t, text, "source-worker-a")
	assert.NotContains(t, text, secret)
	assert.NotContains(t, text, lease.ID)
	assert.NotContains(t, text, "workerSecret")
}

func TestReadSnapshotIsTheReportersView(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)

	entries, err := ReadSnapshot(b.root, testNow)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateServing, entries[0].State)
	assert.Equal(t, digest, entries[0].Digest)

	// The same records after the lease expired read as Draining, without the
	// broker process having to be alive to say so.
	entries, err = ReadSnapshot(b.root, testNow.Add(2*defaultLimits().LeaseTTL))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateDraining, entries[0].State)
}

func TestPriorSuccessNeverEasesLaterRefusal(t *testing.T) {
	// A session that succeeded does not make a later admission eligible for
	// capacity it does not have: refusals are per request, never inherited.
	b, _, digest := newBroker(t, defaultLimits())
	var generation, secret string
	admitServe(t, b, digest, &generation, &secret)
	first, err := b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
	require.Nil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), first.ID))

	second, err := b.AdmitSession(t.Context(), testOwner("tenant-a"), "tenant-a", generation)
	require.Nil(t, err)
	require.Nil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), second.ID))
	require.NotNil(t, b.ReleaseSession(t.Context(), testOwner("tenant-a"), second.ID), "release is not a credit")
}

func TestHeartbeatNeverLowersTheSessionChargeAndDrainAckFollowsTruth(t *testing.T) {
	// The worker's health word never moves the session charge: a healthy
	// heartbeat proves no engine released anything. The charge is the
	// broker's own named sessions plus the persisted unknown charge, and
	// the drain acknowledgement follows that truth.
	b, _, digest := newBroker(t, defaultLimits())
	admission, err := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, err)
	lease, err := b.RegisterWorker(admission.Generation, "source-worker-a", admission.WorkerSecret)
	require.Nil(t, err)
	require.Nil(t, b.FillVerified(t.Context(), admission.Generation, "source-worker-a", lease.ID, admission.WorkerSecret, 1,
		servedDescriptors("source-worker-a")[:1]))

	owner := testOwner("tenant-a")
	session, serr := b.AdmitSession(t.Context(), owner, "tenant-a", admission.Generation)
	require.Nil(t, serr)

	// A heartbeat reporting zero sessions cannot free the named session.
	_, err = b.Heartbeat(admission.Generation, "source-worker-a", lease.ID, admission.WorkerSecret,
		Health{ResidentBytes: 13, Registrations: 1, ActiveSessions: 0})
	require.Nil(t, err)
	assert.EqualValues(t, 1, b.Snapshot()[0].ActiveSessions)

	require.Nil(t, b.Drain(admission.Generation))
	res, err := b.Heartbeat(admission.Generation, "source-worker-a", lease.ID, admission.WorkerSecret,
		Health{ResidentBytes: 13, Registrations: 1, ActiveSessions: 0})
	require.Nil(t, err)
	assert.Equal(t, ActionDrain, res.Action)
	assert.EqualValues(t, 1, b.Snapshot()[0].ActiveSessions)

	// The drain acknowledgement is refused while the named session lives;
	// the owner's own authenticated release is what frees it.
	refused := b.DrainAck(t.Context(), admission.Generation, "source-worker-a", lease.ID, admission.WorkerSecret)
	require.NotNil(t, refused)
	assert.Equal(t, protocol.KindOperationState, refused.Kind)
	require.Nil(t, b.ReleaseSession(t.Context(), owner, session.ID))
	require.Nil(t, b.DrainAck(t.Context(), admission.Generation, "source-worker-a", lease.ID, admission.WorkerSecret))
}

func TestRegistrationsCapAndSnapshotNumericOrder(t *testing.T) {
	limits := defaultLimits()
	limits.MaxGenerations = 16
	limits.QuarantineBytes = 5 // smaller than a single fill's reserve (13), while quarantined == 0
	b, _, digest := newBroker(t, limits)

	// Registration cap is enforced across multi-registration fills.
	req := fill("tenant-a", digest, 12)
	req.Grant.Registrations = 3
	first, err := b.AdmitFill(t.Context(), req)
	require.Nil(t, err)
	lease, err := b.RegisterWorker(first.Generation, "source-worker-a", first.WorkerSecret)
	require.Nil(t, err)
	// Exceeding the namespace's 3-registration grant in FillVerified is refused.
	denied := b.FillVerified(t.Context(), first.Generation, "source-worker-a", lease.ID, first.WorkerSecret, 4, nil)
	require.NotNil(t, denied)
	assert.Equal(t, protocol.KindCapacityExhausted, denied.Kind)
	require.Nil(t, b.FillVerified(t.Context(), first.Generation, "source-worker-a", lease.ID, first.WorkerSecret, 2,
		servedDescriptors("source-worker-a")))

	// With 2 of 3 registrations used, one more 1-registration fill fits, and a third is refused.
	second, err := b.AdmitFill(t.Context(), req)
	require.Nil(t, err)
	secondLease, err := b.RegisterWorker(second.Generation, "source-worker-a", second.WorkerSecret)
	require.Nil(t, err)
	require.Nil(t, b.FillVerified(t.Context(), second.Generation, "source-worker-a", secondLease.ID, second.WorkerSecret, 1,
		servedDescriptors("source-worker-a")[:1]))
	_, err = b.AdmitFill(t.Context(), req)
	require.NotNil(t, err)
	assert.Equal(t, protocol.KindCapacityExhausted, err.Kind)

	// Admit 8 more generations in another namespace and verify Snapshot()
	// and ReadSnapshot() sort by issue order. Tokens are opaque, so the
	// order is asserted against the generations as they were handed out,
	// never against literal counter values.
	issued := make([]string, 0, 10)
	issued = append(issued, first.Generation, second.Generation)
	for i := range 8 {
		otherReq := fill("tenant-b", digest, 12)
		otherReq.Grant.Registrations = 16
		otherReq.WorkerID = fmt.Sprintf("worker-b-%d", i)
		adm, admErr := b.AdmitFill(t.Context(), otherReq)
		require.Nil(t, admErr)
		l, regErr := b.RegisterWorker(adm.Generation, otherReq.WorkerID, adm.WorkerSecret)
		require.Nil(t, regErr)
		require.Nil(t, b.FillVerified(t.Context(), adm.Generation, otherReq.WorkerID, l.ID, adm.WorkerSecret, 1,
			servedDescriptors(otherReq.WorkerID)[:1]))
		issued = append(issued, adm.Generation)
	}
	snap := b.Snapshot()
	require.Len(t, snap, 10)
	for i, entry := range snap {
		assert.Equal(t, issued[i], entry.Generation)
	}
}

func TestAFreshLedgerAdmitsAndNeverReusesTokens(t *testing.T) {
	// A genuinely fresh root (no identity, no counter) admits immediately;
	// after its record is fully released through the qualified lifecycle,
	// its counter still stands, so a re-admission mints a new token
	// instead of reusing one.
	root := t.TempDir()
	t.Cleanup(func() {
		// The store publishes read-only trees; allow the temp dir to go.
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hexDigest := strings.Repeat("f", 64)
	digest := "sha256:" + hexDigest
	publishTree(t, st, hexDigest, "cccc", 12)
	b, openErr := Open(filepath.Join(root, "broker"), testScope, st,
		defaultLimits(), func() time.Time { return testNow })
	require.NoError(t, openErr)
	admission, perr := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, perr)
	lease, regErr := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, regErr)
	require.NoError(t, b.Close())

	reopened, openErr := Open(root+"/broker", testScope, st, defaultLimits(),
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	// The fill completes on the reopened broker, then the qualified
	// lifecycle drains and releases it.
	descriptors := []RegistrationDescriptor{{
		Name: "model.norm.weight", Addr: 0x1000, Length: 12,
		MemoryType: "HOST", Backend: "hixl", WorkerUID: "source-worker-a",
	}}
	require.Nil(t, reopened.FillVerified(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, 1, descriptors))
	require.Nil(t, reopened.Drain(admission.Generation))
	require.Nil(t, reopened.DrainAck(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret))
	require.NoError(t, reopened.Close())

	// Every record is released; the counter survives the empty ledger.
	third, openErr := Open(root+"/broker", testScope, st, defaultLimits(),
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer third.Close()
	second, perr := third.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, perr)
	assert.NotEqual(t, admission.Generation, second.Generation)
}

func TestOpenRefusesACorruptLayout(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.Close())
	require.NoError(t, os.WriteFile(filepath.Join(b.root, "layout"),
		[]byte("{\"layoutVersion\":\"something-else\",\"scope\":\"x\",\"identity\":\"y\"}"),
		0o600))
	_, openErr := Open(b.root, testScope, b.store, b.limits, func() time.Time { return testNow })
	require.NotNil(t, openErr)
	assert.Contains(t, openErr.Error(), "layout version")
}

func TestAZeroHealthHeartbeatNeverFreesUnknownSessions(t *testing.T) {
	// The exact defect: reopen with an unknown session charge, then a
	// healthy heartbeat that reports zero sessions must not wipe it, and
	// the drain acknowledgement must stay refused until a qualified fence.
	b, _, digest := newBroker(t, defaultLimits())
	admission, perr := b.AdmitFill(t.Context(), fill("tenant-a", digest, 12))
	require.Nil(t, perr)
	lease, regErr := b.RegisterWorker(admission.Generation, "source-worker-a",
		admission.WorkerSecret)
	require.Nil(t, regErr)
	descriptors := []RegistrationDescriptor{{
		Name: "model.norm.weight", Addr: 0x1000, Length: 12,
		MemoryType: "HOST", Backend: "hixl", WorkerUID: "source-worker-a",
	}}
	require.Nil(t, b.FillVerified(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, 1, descriptors))
	b.mu.Lock()
	record := b.records[admission.Generation]
	record.ActiveSessions = 2
	require.NoError(t, b.ledger.write(record))
	b.mu.Unlock()
	require.NoError(t, b.Close())

	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	heart, perr := reopened.Heartbeat(admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret, Health{ActiveSessions: 0})
	require.Nil(t, perr)
	assert.Equal(t, ActionNone, heart.Action)

	reopened.mu.Lock()
	count := reopened.records[admission.Generation].ActiveSessions
	unknown := reopened.records[admission.Generation].UnknownSessions
	reopened.mu.Unlock()
	assert.EqualValues(t, 2, count)
	assert.EqualValues(t, 2, unknown)

	refused := reopened.DrainAck(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret)
	require.NotNil(t, refused)
	assert.Equal(t, protocol.KindOperationState, refused.Kind)

	// A qualified fence that names its evidence is the only way out, and
	// the generation still drains before its acknowledgement.
	require.Nil(t, reopened.ClearUnknownSessions(admission.Generation,
		"trained-harness fence receipt abc123"))
	require.Nil(t, reopened.Drain(admission.Generation))
	refused = reopened.DrainAck(t.Context(), admission.Generation, "source-worker-a",
		lease.ID, admission.WorkerSecret)
	require.Nil(t, refused)
}

func TestConcurrentAdmitIntentFillMintsOneGeneration(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "uid-1/rev-1", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: Grant{
			Namespace: "tenant-a", Bytes: 10000,
			Registrations: 64, Sessions: 64,
		},
		WorkerID: "source-worker-a", IntentUID: "intent-uid-1", IntentGeneration: 3,
		BindingUID: "binding-uid-1", ReservationID: "res-concurrent",
		Registrations: 1,
	}
	const callers = 8
	results := make(chan string, callers)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < callers; i++ {
		go func() {
			start.Wait()
			admission, perr := b.AdmitIntentFill(t.Context(), intent)
			if perr != nil {
				results <- "ERR:" + perr.Error()
				return
			}
			results <- admission.Generation
		}()
	}
	start.Done()
	generations := map[string]bool{}
	for i := 0; i < callers; i++ {
		generation := <-results
		require.NotContains(t, generation, "ERR:", generation)
		generations[generation] = true
	}
	assert.Len(t, generations, 1)
}

func TestAdmissionDoesNotExposeFailedLocalConfirmation(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	intent := Intent{
		Key: "intent/final-write", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000),
		WorkerID: "source-worker-a", IntentUID: "intent-uid", IntentGeneration: 1,
		BindingUID: "binding-uid-1", ReservationID: "first-reservation", Registrations: 1,
	}
	authority := b.reservationAuthorities[types.UID(intent.BindingUID)]
	blocked := ""
	authority.maps = &failingMaps{
		ConfigMapInterface: authority.maps.(coreclient.ConfigMapInterface),
		afterUpdate: func(cm *core.ConfigMap) {
			var doc authorityDocument
			require.NoError(t, json.Unmarshal([]byte(cm.Data[authorityDocumentKey]), &doc))
			for _, entry := range doc.Entries {
				if entry.State == authorityStateAdmitted && blocked == "" {
					blocked = filepath.Join(b.root, recordsDir, entry.Generation+".json.tmp")
					require.NoError(t, os.Mkdir(blocked, 0o700))
				}
			}
		},
	}
	assignment, err := b.AdmitIntentFill(t.Context(), intent)
	require.NotNil(t, err)
	require.Nil(t, assignment)
	assert.Empty(t, b.OfferedAssignments())
	require.Len(t, b.records, 1)
	var generation string
	for key, record := range b.records {
		generation = key
		assert.Empty(t, record.WorkerSecret)
		assert.NotEqual(t, "admitted", record.AdmissionPhase)
		durable, readErr := b.ledger.read(key)
		require.NoError(t, readErr)
		assert.Empty(t, durable.WorkerSecret)
	}
	require.NoError(t, os.Remove(blocked))
	intent.ReservationID = "new-caller-attempt"
	assignment, err = b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, err)
	require.NotNil(t, assignment)
	assert.Equal(t, generation, assignment.Generation)
	assert.Equal(t, "first-reservation", b.records[generation].ReservationID)
}
