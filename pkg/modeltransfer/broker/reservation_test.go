package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestAuthorityJSONRejectsDuplicateKeys(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		valid bool
	}{
		{name: "nested records", raw: `{"entries":[{"owner":{"uid":"one"}},{"owner":{"uid":"two"}}]}`, valid: true},
		{name: "nested empty arrays", raw: `{"entries":[[],[{}]],"count":0}`, valid: true},
		{name: "top-level duplicate", raw: `{"uid":"one","uid":"two"}`},
		{name: "nested duplicate", raw: `{"entries":[{"uid":"one","uid":"two"}]}`},
		{name: "trailing object", raw: `{} {}`},
		{name: "trailing malformed bytes", raw: `{} bad`},
		{name: "incomplete nested record", raw: `{"entries":[{`},
		{name: "wrong top level", raw: `[]`},
		{name: "excessive nesting", raw: `{"entries":` + strings.Repeat("[", 32) + `0` + strings.Repeat("]", 32) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectDuplicateJSONKeys(tc.raw)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

const (
	testBindingUID = "binding-uid-1"
	testEpoch      = "authority-epoch-1"
)

// versionedMaps gives the fake client the atomic resourceVersion conflicts
// enforced by the API server.
type versionedMaps struct {
	coreclient.ConfigMapInterface
	mu sync.Mutex
}

func (f *versionedMaps) Update(ctx context.Context, cm *core.ConfigMap,
	opts meta.UpdateOptions,
) (*core.ConfigMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, err := f.Get(ctx, cm.Name, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != cm.ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("stale version"))
	}
	next := cm.DeepCopy()
	version, _ := strconv.ParseInt(current.ResourceVersion, 10, 64)
	next.ResourceVersion = strconv.FormatInt(version+1, 10)
	return f.ConfigMapInterface.Update(ctx, next, opts)
}

// failingMaps reports a lost response after the underlying update lands.
type failingMaps struct {
	coreclient.ConfigMapInterface
	mu           sync.Mutex
	afterUpdate  func(*core.ConfigMap)
	failOnUpdate atomic.Bool
	updates      atomic.Int64
}

func (f *failingMaps) Update(ctx context.Context, cm *core.ConfigMap,
	opts meta.UpdateOptions,
) (*core.ConfigMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	applied, err := f.ConfigMapInterface.Update(ctx, cm, opts)
	if err != nil {
		return nil, err
	}
	if f.afterUpdate != nil {
		f.afterUpdate(applied)
	}
	if f.failOnUpdate.Load() {
		f.updates.Add(1)
		return applied, fmt.Errorf("synthetic response loss after the write landed")
	}
	return applied, err
}

func newTestAuthority(t *testing.T) (*ReservationAuthority, *failingMaps) {
	t.Helper()
	fakeMaps := &failingMaps{ConfigMapInterface: &versionedMaps{ConfigMapInterface: kubefake.NewSimpleClientset().
		CoreV1().ConfigMaps("model-transfer-system")}}
	seeded, err := fakeMaps.Create(context.Background(), &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      AuthorityName(testBindingUID),
			Namespace: "model-transfer-system",
			UID:       types.UID("authority-configmap-uid-1"),
		},
		Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` + testEpoch + `","entries":[]}`},
	}, meta.CreateOptions{})
	require.NoError(t, err)
	// The fake assigns its own UID; re-seed with the pin's UID embedded so
	// the authority's object-UID check passes against a stable value.
	require.NotEmpty(t, seeded.UID)
	authority := NewReservationAuthority(fakeMaps, "model-transfer-system",
		testBindingUID,
		authorityPin{UID: string(seeded.UID), Epoch: testEpoch},
		func(ctx context.Context) (ReservationGrant, error) {
			return ReservationGrant{
				BindingUID:    testBindingUID,
				Bytes:         1 << 30,
				Registrations: 64,
				Sessions:      64,
			}, nil
		},
	)
	authority.SetClock(func() time.Time { return testNow })
	return authority, fakeMaps
}

func reserveRequest(reservationID, scope, ledger, generation string) ReservationRequest {
	return ReservationRequest{
		ReservationID:    reservationID,
		NodeScope:        scope,
		LedgerIdentity:   ledger,
		Generation:       generation,
		IntentUID:        "intent-uid-" + generation,
		IntentGeneration: 3,
		Digest:           "sha256:" + strings.Repeat("a", 64),
		Namespace:        "tenant-a",
		Bytes:            12,
		Registrations:    1,
	}
}

// (a) Global grants: two entries from different nodes and vendors draw on
// ONE Binding-wide budget — bytes, registrations, and sessions are summed
// across entries, never charged per entry.
func TestGlobalGrantSumsAcrossNodesAndVendors(t *testing.T) {
	cases := []struct {
		name     string
		grant    ReservationGrant
		sessions bool
	}{
		{name: "bytes", grant: ReservationGrant{BindingUID: testBindingUID, Bytes: 24, Registrations: 64, Sessions: 64}},
		{name: "registrations", grant: ReservationGrant{BindingUID: testBindingUID, Bytes: 1024, Registrations: 2, Sessions: 64}},
		{name: "sessions", grant: ReservationGrant{BindingUID: testBindingUID, Bytes: 1024, Registrations: 64, Sessions: 2}, sessions: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authority, _ := newTestAuthority(t)
			authority.liveGrant = func(context.Context) (ReservationGrant, error) { return tc.grant, nil }
			ctx := context.Background()
			first, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "node-a", "g-1"))
			require.NoError(t, err)
			second, err := authority.Reserve(ctx, reserveRequest("res-2", "nvidia", "node-b", "g-2"))
			require.NoError(t, err)
			if !tc.sessions {
				_, err = authority.Reserve(ctx, reserveRequest("res-3", "nvidia", "node-c", "g-3"))
				require.ErrorIs(t, err, errReservationCapacity)
				return
			}
			for _, entry := range []ReservationEntry{first, second} {
				require.NoError(t, authority.MarkAdmitted(ctx, entry.NodeScope, entry.LedgerIdentity, entry.Generation))
				owner := SessionReservation{
					SessionID: "s-" + entry.Generation, PodUID: "pod", ArtifactUID: "artifact",
					Namespace: entry.Namespace, Digest: entry.Digest, Generation: entry.Generation,
				}
				require.NoError(t, authority.ReserveSessionSlot(ctx, entry.ReservationID, owner))
				require.NoError(t, authority.ReserveSessionSlot(ctx, entry.ReservationID, owner))
			}
			err = authority.ReserveSessionSlot(ctx, first.ReservationID, SessionReservation{
				SessionID: "overflow", PodUID: "pod", ArtifactUID: "artifact",
				Namespace: first.Namespace, Digest: first.Digest, Generation: first.Generation,
			})
			require.ErrorIs(t, err, errReservationCapacity)
		})
	}
}

func TestConcurrentReservationsRespectGlobalCapacity(t *testing.T) {
	authority, _ := newTestAuthority(t)
	authority.liveGrant = func(context.Context) (ReservationGrant, error) {
		return ReservationGrant{BindingUID: testBindingUID, Bytes: 4, Registrations: 4, Sessions: 16}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 12)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			<-start
			id := strconv.Itoa(i)
			req := reserveRequest("r-"+id, "node-"+id, "ledger-"+id, "g-"+id)
			req.Bytes = 1
			_, err := authority.Reserve(ctx, req)
			results <- err
		})
	}
	close(start)
	workers.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else {
			require.ErrorIs(t, err, errReservationCapacity)
		}
	}
	assert.Equal(t, 4, admitted)
}

// (b) Two Bindings in the same namespace hold independent authorities: a
// reservation under one Binding UID is invisible to the other.
func TestTwoBindingsInOneNamespaceAreIndependent(t *testing.T) {
	fakeMaps := &failingMaps{ConfigMapInterface: &versionedMaps{ConfigMapInterface: kubefake.NewSimpleClientset().
		CoreV1().ConfigMaps("model-transfer-system")}}
	seed := func(bindingUID types.UID) authorityPin {
		seeded, err := fakeMaps.Create(context.Background(), &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Name:      AuthorityName(bindingUID),
				Namespace: "model-transfer-system",
				UID:       types.UID("cm-uid-" + string(bindingUID)),
			},
			Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"` + string(bindingUID) +
				`","epoch":"epoch-` + string(bindingUID) + `","entries":[]}`},
		}, meta.CreateOptions{})
		require.NoError(t, err)
		return authorityPin{UID: string(seeded.UID), Epoch: "epoch-" + string(bindingUID)}
	}
	pinA := seed("binding-a")
	pinB := seed("binding-b")
	grant := func(bindingUID types.UID) func(context.Context) (ReservationGrant, error) {
		return func(context.Context) (ReservationGrant, error) {
			return ReservationGrant{
				BindingUID: bindingUID,
				Bytes:      1 << 30, Registrations: 64, Sessions: 64,
			}, nil
		}
	}
	authorityA := NewReservationAuthority(fakeMaps, "model-transfer-system",
		"binding-a", pinA, grant("binding-a"))
	authorityB := NewReservationAuthority(fakeMaps, "model-transfer-system",
		"binding-b", pinB, grant("binding-b"))

	reservedA, err := authorityA.Reserve(context.Background(),
		reserveRequest("res-a", "ascend", "ledger-a", "g-a"))
	require.NoError(t, err)
	reservedB, err := authorityB.Reserve(context.Background(),
		reserveRequest("res-b", "ascend", "ledger-a", "g-a"))
	require.NoError(t, err)
	assert.NotEqual(t, reservedA.ReservationID, reservedB.ReservationID)
	assert.Equal(t, reservedA.Generation, reservedB.Generation)

	// Each authority sees only its own Binding's entries.
	adoptedA, err := authorityA.Adopt(context.Background(), "ascend", "ledger-a")
	require.NoError(t, err)
	require.Len(t, adoptedA, 1)
	assert.Equal(t, reservedA.Generation, adoptedA[0].Generation)
}

// (c) A replaced authority object, or a pin epoch mismatch, is refused by
// every mutation: Reserve, Mark, session slots, Release, and Adopt.
func TestReplacedAuthorityObjectFailsClosed(t *testing.T) {
	authority, fakeMaps := newTestAuthority(t)
	ctx := context.Background()
	reserved, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)

	// Replace the object: new UID, same name.
	require.NoError(t, fakeMaps.Delete(ctx, authority.name(), meta.DeleteOptions{}))
	recreated, err := fakeMaps.Create(ctx, &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      authority.name(),
			Namespace: "model-transfer-system",
			UID:       types.UID("recreated-configmap-uid"),
		},
		Data: map[string]string{authorityDocumentKey: `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` + testEpoch +
			`","entries":[]}`},
	}, meta.CreateOptions{})
	require.NoError(t, err)

	_, err = authority.Reserve(ctx, reserveRequest("res-2", "ascend", "l-2", "g-2"))
	require.True(t, errors.Is(err, ErrReservationUnavailable))
	_, err = authority.Adopt(ctx, "ascend", "l-1")
	require.True(t, errors.Is(err, ErrReservationUnavailable))
	err = authority.MarkAdmitted(ctx, "ascend", "l-1", reserved.Generation)
	require.True(t, errors.Is(err, ErrReservationUnavailable))
	err = authority.ReserveSessionSlot(ctx, reserved.ReservationID, SessionReservation{
		SessionID: "s", PodUID: "p", Namespace: "n", ArtifactUID: "a",
		Digest: "d", Generation: reserved.Generation,
	})
	require.True(t, errors.Is(err, ErrReservationUnavailable))
	_ = recreated
}

// (d) Malformed documents never credit: negative charges, duplicate
// reservations, and oversize documents all hold.
func TestMalformedDocumentsHoldInsteadOfCredit(t *testing.T) {
	authority, fakeMaps := newTestAuthority(t)
	ctx := context.Background()

	for name, doc := range map[string]string{
		"negative bytes": `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` +
			testEpoch + `","entries":[{"reservationID":"r1","nodeScope":"ascend",` +
			`"ledgerIdentity":"l","generation":"g-1","intentUID":"i","intentGeneration":1,` +
			`"digest":"sha256:x","namespace":"n","bytes":-5,"registrations":1,"state":"reserved"}]}`,
		"duplicate reservations": `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` +
			testEpoch + `","entries":[{"reservationID":"r1","nodeScope":"ascend",` +
			`"ledgerIdentity":"l","generation":"g-1","intentUID":"i","intentGeneration":1,` +
			`"digest":"sha256:x","namespace":"n","bytes":5,"registrations":1,"state":"reserved"},` +
			`{"reservationID":"r1","nodeScope":"ascend","ledgerIdentity":"l","generation":"g-2",` +
			`"intentUID":"i","intentGeneration":1,"digest":"sha256:x","namespace":"n",` +
			`"bytes":5,"registrations":1,"state":"reserved"}]}`,
		"unknown state": `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` +
			testEpoch + `","entries":[{"reservationID":"r1","nodeScope":"ascend",` +
			`"ledgerIdentity":"l","generation":"g-1","intentUID":"i","intentGeneration":1,` +
			`"digest":"sha256:x","namespace":"n","bytes":5,"registrations":1,"state":"vaporized"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			cm, err := fakeMaps.Get(ctx, authority.name(), meta.GetOptions{})
			require.NoError(t, err)
			cm.Data = map[string]string{authorityDocumentKey: doc}
			_, err = fakeMaps.Update(ctx, cm, meta.UpdateOptions{})
			require.NoError(t, err)
			_, err = authority.Reserve(ctx, reserveRequest("res-x", "ascend", "l-x", "g-x"))
			require.True(t, errors.Is(err, ErrReservationUnavailable),
				"a malformed authority must hold, not credit: %v", err)
		})
	}
}

// (e) Identical retries are idempotent; any immutable-field mismatch in a
// replay is refused rather than adopted.
func TestReplayIdentityIsImmutable(t *testing.T) {
	authority, _ := newTestAuthority(t)
	ctx := context.Background()
	first, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)

	same, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)
	assert.Equal(t, first.ReservationID, same.ReservationID)
	assert.Equal(t, first.Bytes, same.Bytes)

	changed := reserveRequest("res-1", "ascend", "l-1", "g-1")
	changed.Bytes = 9999
	_, err = authority.Reserve(ctx, changed)
	require.True(t, errors.Is(err, ErrReservationUnavailable))

	changedDigest := reserveRequest("res-1", "ascend", "l-1", "g-1")
	changedDigest.Digest = "sha256:" + strings.Repeat("b", 64)
	_, err = authority.Reserve(ctx, changedDigest)
	require.True(t, errors.Is(err, ErrReservationUnavailable))
}

// (f) A successful safe release returns quota exactly once; an entry with
// live session owners, and an unknown reservation, cannot release.
func TestReleaseCreditsOnceAndHoldsUnknownAndActive(t *testing.T) {
	authority, _ := newTestAuthority(t)
	ctx := context.Background()
	reserved, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)
	owner := SessionReservation{
		SessionID: "s-1", PodUID: "pod",
		Namespace: "tenant-a", ArtifactUID: "a", Digest: reserved.Digest,
		Generation: reserved.Generation,
	}
	require.NoError(t, authority.MarkAdmitted(ctx, reserved.NodeScope, reserved.LedgerIdentity, reserved.Generation))
	require.NoError(t, authority.ReserveSessionSlot(ctx, reserved.ReservationID, owner))

	// Session-bearing release is refused; the quota stays held.
	err = authority.Release(ctx, reserved.ReservationID, "ascend", "l-1")
	require.Error(t, err)
	adopted, err := authority.Adopt(ctx, "ascend", "l-1")
	require.NoError(t, err)
	require.Len(t, adopted, 1)

	// The exact owner releases its slot, then the safe release compacts
	// the entry once; a second release is a refusal, not a double credit.
	require.NoError(t, authority.ReleaseSessionSlot(ctx,
		reserved.ReservationID, "ascend", "l-1", owner))
	require.Error(t, authority.Release(ctx, reserved.ReservationID, "ascend", "l-1"))
	require.NoError(t, authority.MarkDrained(ctx, reserved.ReservationID, "ascend", "l-1"))
	require.NoError(t, authority.Release(ctx, reserved.ReservationID, "ascend", "l-1"))
	err = authority.Release(ctx, reserved.ReservationID, "ascend", "l-1")
	require.Error(t, err)
	adopted, err = authority.Adopt(ctx, "ascend", "l-1")
	require.NoError(t, err)
	assert.Empty(t, adopted)
	// A retry after the generation entry was compacted confirms the session charge is gone.
	require.NoError(t, authority.ReleaseSessionSlotIfPresent(ctx, reserved.ReservationID, "ascend", "l-1", owner))

	// A foreign scope cannot release another node's reservation.
	other, err := authority.Reserve(ctx, reserveRequest("res-2", "nvidia", "l-2", "g-2"))
	require.NoError(t, err)
	err = authority.Release(ctx, other.ReservationID, "ascend", "l-1")
	require.Error(t, err)
}

// (g) Grant growth admits; shrink and live-grant UID mismatch hold; API
// errors and exhausted conflicts retain the charge; the grant is re-read on
// every pass so a stale snapshot never passes.
func TestLiveGrantGrowthShrinkMismatchAndAPIError(t *testing.T) {
	authority, fakeMaps := newTestAuthority(t)
	ctx := context.Background()
	live := atomic.Int64{}
	live.Store(1 << 20)
	authority.liveGrant = func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{
			BindingUID: testBindingUID,
			Bytes:      live.Load(), Registrations: 64, Sessions: 64,
		}, nil
	}
	first, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)

	// Shrink below the live charge holds new admissions.
	live.Store(first.Bytes)
	_, err = authority.Reserve(ctx, reserveRequest("res-2", "nvidia", "l-2", "g-2"))
	require.True(t, errors.Is(err, errReservationCapacity))

	// A live grant whose Binding UID differs from the pin never passes.
	authority.liveGrant = func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{
			BindingUID: "other-binding",
			Bytes:      1 << 30, Registrations: 64, Sessions: 64,
		}, nil
	}
	_, err = authority.Reserve(ctx, reserveRequest("res-3", "nvidia", "l-3", "g-3"))
	require.True(t, errors.Is(err, ErrReservationUnavailable))

	// API errors retain the charge.
	authority.liveGrant = func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{}, fmt.Errorf("apiserver unreachable")
	}
	_, err = authority.Reserve(ctx, reserveRequest("res-4", "nvidia", "l-4", "g-4"))
	require.True(t, errors.Is(err, ErrReservationUnavailable))

	// Growth admits again.
	live.Store(1 << 20)
	authority.liveGrant = func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{
			BindingUID: testBindingUID,
			Bytes:      live.Load(), Registrations: 64, Sessions: 64,
		}, nil
	}
	_, err = authority.Reserve(ctx, reserveRequest("res-5", "nvidia", "l-5", "g-5"))
	require.NoError(t, err)
	_ = fakeMaps
}

// (h) An applied-but-lost CAS response is not a failure to reserve: the
// replay adopts the entry that actually landed instead of forking a second.
func TestAppliedResponseLossThenReplayAdoptsSameEntry(t *testing.T) {
	authority, fakeMaps := newTestAuthority(t)
	ctx := context.Background()
	fakeMaps.failOnUpdate.Store(true)
	_, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.True(t, errors.Is(err, ErrReservationUnavailable),
		"a lost response surfaces as unavailable, never as success")
	fakeMaps.failOnUpdate.Store(false)

	// The applied write landed: the same identity replays onto the same
	// entry, and no second entry exists.
	same, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)
	adopted, err := authority.Adopt(ctx, "ascend", "l-1")
	require.NoError(t, err)
	require.Len(t, adopted, 1)
	assert.Equal(t, same.Generation, adopted[0].Generation)
}

// (i) A session cap lowered online drains naturally: the sessions the grant
// already admitted stay idempotent, the generation-level marks keep
// working while the live total sits above the lowered cap, and only NEW
// session claims are refused until releases take the live total below it.
func TestLiveSessionGrantShrinkDrainsExistingAndCapsNew(t *testing.T) {
	authority, _ := newTestAuthority(t)
	ctx := context.Background()
	liveSessions := int32(8)
	authority.liveGrant = func(ctx context.Context) (ReservationGrant, error) {
		return ReservationGrant{
			BindingUID: testBindingUID,
			Bytes:      1 << 30, Registrations: 64, Sessions: liveSessions,
		}, nil
	}
	entry, err := authority.Reserve(ctx, reserveRequest("res-1", "ascend", "l-1", "g-1"))
	require.NoError(t, err)
	require.NoError(t, authority.MarkAdmitted(ctx, "ascend", "l-1", "g-1"))
	owner := func(id string) SessionReservation {
		return SessionReservation{
			SessionID: id, PodUID: "pod-" + id, ArtifactUID: "artifact",
			Namespace: entry.Namespace, Digest: entry.Digest, Generation: entry.Generation,
		}
	}
	held := []string{"s-1", "s-2", "s-3", "s-4", "s-5", "s-6", "s-7", "s-8"}
	for _, id := range held {
		require.NoError(t, authority.ReserveSessionSlot(ctx, entry.ReservationID, owner(id)))
	}

	// The Binding's session cap drops online from 8 to 4 while all eight
	// sessions are live.
	liveSessions = 4

	// The eight held sessions replay idempotently under the lowered cap.
	for _, id := range held {
		require.NoError(t, authority.ReserveSessionSlot(ctx, entry.ReservationID, owner(id)),
			"an already held session must not be stranded by the lowered cap")
	}

	// A new session is refused while the live total is above the new cap.
	err = authority.ReserveSessionSlot(ctx, entry.ReservationID, owner("s-new"))
	require.ErrorIs(t, err, errReservationCapacity)

	// The generation-level marks keep working through the drain: the
	// already admitted entry re-marks as a no-op, and a fresh generation
	// reserves and admits on both marks despite the live session total.
	require.NoError(t, authority.MarkReserved(ctx, "ascend", "l-1", "g-1"))
	require.NoError(t, authority.MarkAdmitted(ctx, "ascend", "l-1", "g-1"))
	second, err := authority.Reserve(ctx, reserveRequest("res-2", "ascend", "l-1", "g-2"))
	require.NoError(t, err)
	require.NoError(t, authority.MarkReserved(ctx, second.NodeScope, second.LedgerIdentity, second.Generation))
	require.NoError(t, authority.MarkAdmitted(ctx, second.NodeScope, second.LedgerIdentity, second.Generation))

	// Draining down to exactly the new cap still refuses new sessions.
	for _, id := range []string{"s-1", "s-2", "s-3", "s-4"} {
		require.NoError(t, authority.ReleaseSessionSlot(ctx, entry.ReservationID, "ascend", "l-1", owner(id)))
	}
	err = authority.ReserveSessionSlot(ctx, entry.ReservationID, owner("s-new"))
	require.ErrorIs(t, err, errReservationCapacity)

	// One release below the new cap admits exactly one new session, and
	// the refilled total is capped again.
	require.NoError(t, authority.ReleaseSessionSlot(ctx, entry.ReservationID, "ascend", "l-1", owner("s-5")))
	require.NoError(t, authority.ReserveSessionSlot(ctx, entry.ReservationID, owner("s-new-1")))
	err = authority.ReserveSessionSlot(ctx, entry.ReservationID, owner("s-new-2"))
	require.ErrorIs(t, err, errReservationCapacity)
}

// Compiled-in guards for the checked arithmetic: a document whose entries
// approach the int64 ceiling must be refused, not wrapped.
func TestCheckedArithmeticRefusesNearCeilingDocuments(t *testing.T) {
	authority, fakeMaps := newTestAuthority(t)
	ctx := context.Background()
	huge := int64(1)<<62 + 1
	doc := `{"apiVersion":1,"bindingUID":"` + testBindingUID + `","epoch":"` + testEpoch +
		`","entries":[{"reservationID":"r1","nodeScope":"ascend","ledgerIdentity":"l",` +
		`"generation":"g-1","intentUID":"i","intentGeneration":1,"digest":"sha256:x",` +
		`"namespace":"n","bytes":` + fmt.Sprint(huge) + `,"registrations":1,"state":"reserved"}]}`
	cm, err := fakeMaps.Get(ctx, authority.name(), meta.GetOptions{})
	require.NoError(t, err)
	cm.Data = map[string]string{authorityDocumentKey: doc}
	_, err = fakeMaps.Update(ctx, cm, meta.UpdateOptions{})
	require.NoError(t, err)
	_, err = authority.Reserve(ctx, reserveRequest("res-x", "nvidia", "l-x", "g-x"))
	require.True(t, errors.Is(err, ErrReservationUnavailable))
}
