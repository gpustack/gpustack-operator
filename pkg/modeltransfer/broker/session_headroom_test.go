package broker

// Tests for the refusal journal reserve: new refusal records stop being
// created once the shared journal holds refusalJournalCeiling records, so
// the remaining reserve stays available for admitted sessions. Journaled
// refusal receipts stay immutable; a refusal that cannot be journaled
// returns false and carries no durable receipt. The reserve prevents
// refusal-only exhaustion; it does not recover an already-full journal and
// does not guarantee endless capacity once legitimate session history
// grows.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// newHeadroomBroker opens a broker on an explicit ledger root with the
// test-only qualification seam open, so a test controls restarts.
func newHeadroomBroker(t *testing.T, root string, st *store.Store) *Broker {
	t.Helper()
	b, err := Open(root, testScope, st, defaultLimits(),
		func() time.Time { return testNow })
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.EnableOperationJournal())
	qualifyTestSource(t, b)
	return b
}

// headroomRefusal returns a distinct pre-admission refusal identity shaped
// like the private session handler's records.
func headroomRefusal(i int) SessionOwner {
	return SessionOwner{
		PodUID:           "pod-headroom",
		Namespace:        "tenant-headroom",
		RequestID:        fmt.Sprintf("%032x", 0x20000+i),
		TargetInstanceID: fmt.Sprintf("%032x", 0xB),
		OpenFingerprint:  "fingerprint-headroom",
	}
}

// fillRefusals journals distinct refusals until one is declined or the
// attempt budget is spent. It returns how many were journaled, the attempt
// number of the first declined refusal (0 if none), and the total session
// count afterwards.
func fillRefusals(b *Broker, maxAttempts int) (journaled, firstDeclined, total int) {
	for i := 1; i <= maxAttempts; i++ {
		if b.RecordUnadmitted(headroomRefusal(i)) {
			journaled++
			continue
		}
		return journaled, i, len(b.sessions)
	}
	return journaled, 0, len(b.sessions)
}

func TestRecordUnadmittedStopsAtRefusalReserve(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	b := newHeadroomBroker(t, filepath.Join(root, "broker"), st)

	journaled, firstDeclined, total := fillRefusals(b, sessionJournalLimit)
	require.Equal(t, refusalJournalCeiling, journaled,
		"distinct refusals journaled must stop at the reserve ceiling")
	require.Equal(t, refusalJournalCeiling+1, firstDeclined,
		"the first refusal beyond the reserve ceiling must be declined")
	require.Equal(t, refusalJournalCeiling, total,
		"total records must equal the journaled refusal count on a fresh ledger")

	// Journaled receipts stay immutable: the exact first refusal replays
	// idempotently at the ceiling, and a changed fingerprint must not
	// impersonate a journaled refusal.
	require.True(t, b.RecordUnadmitted(headroomRefusal(1)))
	impersonated := headroomRefusal(1)
	impersonated.OpenFingerprint = "fingerprint-impersonated"
	require.False(t, b.RecordUnadmitted(impersonated))
	require.Equal(t, refusalJournalCeiling, len(b.sessions))

	// The record set persists across a restart: the same ceiling, the same
	// idempotent replay, refusals still declined, and the ledger file bytes
	// untouched by replay-only traffic.
	ledgerPath := filepath.Join(b.root, "sessions.json")
	before, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, b.Close())

	reopened := newHeadroomBroker(t, filepath.Join(root, "broker"), st)
	require.Len(t, reopened.sessions, refusalJournalCeiling)
	require.True(t, reopened.RecordUnadmitted(headroomRefusal(1)))
	require.False(t, reopened.RecordUnadmitted(impersonated))
	require.False(t, reopened.RecordUnadmitted(headroomRefusal(999_999)))
	require.Equal(t, refusalJournalCeiling, len(reopened.sessions))
	after, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after),
		"replay-only traffic must not rewrite the ledger file")
	require.NoError(t, reopened.Close())
}

func TestAdmissionUsesRefusalReserveHeadroom(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	intent := Intent{
		Key: "headroom-admission", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
		IntentUID: "prefetch", IntentGeneration: 1, BindingUID: "binding-uid-1",
		ReservationID: "headroom-reservation", Registrations: 1,
		ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	lease, perr := b.RegisterWorker(assignment.Generation, intent.WorkerID,
		assignment.WorkerSecret, "instance")
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID,
		lease.ID, assignment.WorkerSecret, "instance"))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID,
		lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{
			Name: "file", Addr: 1, Length: 12,
			MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID,
		}},
		"instance"))

	journaled, _, total := fillRefusals(b, sessionJournalLimit)
	require.Equal(t, refusalJournalCeiling, journaled)
	require.Equal(t, refusalJournalCeiling, total)

	// A fresh valid admission uses the reserved headroom even though the
	// refusal ceiling is reached.
	fresh := SessionOwner{
		PodUID: "pod-fresh", Namespace: "tenant-a", ArtifactUID: intent.ArtifactUID,
		Digest: digest, RequestID: strings.Repeat("e", 32),
		TargetInstanceID: strings.Repeat("f", 32),
		OpenFingerprint:  "fingerprint-fresh",
	}
	session, perr := b.AdmitSession(context.Background(), fresh,
		"tenant-a", assignment.Generation)
	require.Nil(t, perr)
	require.NotEmpty(t, session.ID)
	require.Equal(t, refusalJournalCeiling+1, len(b.sessions))
	require.NoError(t, b.Close())
}

func TestLegacyFullLedgerLoadsAndIsNotAltered(t *testing.T) {
	// Build a ledger already at the old full bound of sessionJournalLimit
	// records: every record is a valid old refusal with the exact fields
	// RecordUnadmitted writes, keyed by its stable session id — the
	// observed refusal-only saturation.
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	seeded, err := Open(filepath.Join(root, "broker"), testScope, st,
		defaultLimits(), func() time.Time { return testNow })
	require.NoError(t, err)
	require.NoError(t, seeded.EnableOperationJournal())
	require.NoError(t, seeded.Close())
	sessionsPath := filepath.Join(root, "broker", "sessions.json")

	records := map[string]*SessionRecord{}
	for i := 0; i < sessionJournalLimit; i++ {
		owner := SessionOwner{
			PodUID:           fmt.Sprintf("legacy-pod-%d", i),
			Namespace:        "legacy",
			RequestID:        fmt.Sprintf("%032x", 0x30000+i),
			TargetInstanceID: fmt.Sprintf("%032x", 0xC),
			OpenFingerprint:  fmt.Sprintf("fingerprint-legacy-%d", i),
		}
		records[stableSessionID(owner)] = &SessionRecord{
			Phase: "refused", OwnerUID: owner.PodUID, Namespace: owner.Namespace,
			RequestID: owner.RequestID, TargetInstanceID: owner.TargetInstanceID,
			OpenFingerprint: owner.OpenFingerprint,
		}
	}
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sessionsPath, raw, 0o600))
	before, err := os.ReadFile(sessionsPath)
	require.NoError(t, err)

	// The old full bound still loads under the new code and is not altered
	// or falsely freed: the record set stays at the full count and the file
	// on disk keeps its exact bytes.
	reopened := newHeadroomBroker(t, filepath.Join(root, "broker"), st)
	require.Len(t, reopened.sessions, sessionJournalLimit)

	require.False(t, reopened.RecordUnadmitted(headroomRefusal(1)),
		"a refusal record cannot be created in an already-full ledger")
	require.Len(t, reopened.sessions, sessionJournalLimit)

	// A journaled legacy refusal keeps its immutable receipt after the
	// restart: replaying the exact identity stays idempotent.
	legacyRefused := SessionOwner{
		PodUID:           "legacy-pod-0",
		Namespace:        "legacy",
		RequestID:        fmt.Sprintf("%032x", 0x30000),
		TargetInstanceID: fmt.Sprintf("%032x", 0xC),
		OpenFingerprint:  "fingerprint-legacy-0",
	}
	require.True(t, reopened.RecordUnadmitted(legacyRefused))
	require.Len(t, reopened.sessions, sessionJournalLimit)

	after, err := os.ReadFile(sessionsPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after),
		"the full legacy ledger file must not be rewritten or altered")
}

func TestSessionHandlerRefusalReceiptAtRefusalCeiling(t *testing.T) {
	// HTTP-level contract of the private session handler around the refusal
	// reserve: a journaled refusal's replay receives the authoritative
	// `admission: not-created` receipt; once the ceiling is reached, a NEW
	// request receives the plain typed refusal with NO such receipt, no
	// admission, and no record change.
	b, generation, _, _ := gateFixture(t)

	// Gate closed: the qualification refusal is journaled with its
	// authoritative receipt, and an identical retry replays that receipt.
	closeTestSource(t, b)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})
	body := gateSessionRequest(t, digest, strings.Repeat("c", 32), "weight")

	code, answer, raw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, code, raw)
	require.Equal(t, "not-created", answer["admission"])
	require.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])
	b.mu.Lock()
	records := len(b.sessions)
	b.mu.Unlock()
	require.Equal(t, 1, records)

	replayCode, replay, replayRaw := gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, replayCode, replayRaw)
	require.Equal(t, "not-created", replay["admission"])
	require.Equal(t, string(protocol.KindSourceUnavailable), replay["kind"])
	b.mu.Lock()
	records = len(b.sessions)
	b.mu.Unlock()
	require.Equal(t, 1, records)

	// Gate still closed: fill the journal to the refusal ceiling with
	// distinct identities, so every subsequent distinct request is refused
	// AND unjournaled.
	for i := 1; ; i++ {
		b.mu.Lock()
		ceilingReached := len(b.sessions) >= refusalJournalCeiling
		b.mu.Unlock()
		if ceilingReached {
			break
		}
		if i > 100000 {
			t.Fatal("fill loop did not converge")
		}
		if !b.RecordUnadmitted(SessionOwner{
			PodUID:           "pod-ceiling",
			Namespace:        "tenant",
			RequestID:        fmt.Sprintf("%032x", 0x40000+i),
			TargetInstanceID: strings.Repeat("b", 32),
			OpenFingerprint:  "fingerprint-ceiling",
		}) {
			t.Fatalf("refusal %d was not journaled before the ceiling", i)
		}
	}
	b.mu.Lock()
	records = len(b.sessions)
	b.mu.Unlock()
	require.Equal(t, refusalJournalCeiling, records)

	// The journaled request replayed at the ceiling still carries its
	// authoritative receipt: the exact same request bytes resolve to the
	// same refused record, and nothing is admitted or changed.
	code, answer, raw = gateSessionPost(t, handler, body)
	require.Equal(t, http.StatusConflict, code, raw)
	require.Equal(t, "not-created", answer["admission"])
	require.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])
	b.mu.Lock()
	records = len(b.sessions)
	b.mu.Unlock()
	require.Equal(t, refusalJournalCeiling, records)

	// A distinct fresh request at the ceiling: refused by the closed gate,
	// NOT journaled (the reserve declines it), and carrying NO
	// authoritative receipt. Nothing is admitted and no record is added.
	fresh := gateSessionRequest(t, digest, strings.Repeat("d", 32), "weight")
	code, answer, raw = gateSessionPost(t, handler, fresh)
	require.Equal(t, http.StatusConflict, code, raw)
	require.Equal(t, string(protocol.KindSourceUnavailable), answer["kind"])
	require.NotContains(t, raw, "not-created")
	b.mu.Lock()
	records = len(b.sessions)
	phases := map[string]int{}
	for _, record := range b.sessions {
		phases[record.Phase]++
	}
	b.mu.Unlock()
	require.Equal(t, refusalJournalCeiling, records)
	require.Equal(t, refusalJournalCeiling, phases["refused"])
	require.Zero(t, phases["prepared"])
	require.Zero(t, phases["admitted"])

	// Opening the TEST gate shows the same fresh identity can then admit
	// into the reserved headroom: a conservative unjournaled refusal does
	// not prevent a later admission once the original cause is resolved.
	qualifyTestSource(t, b)
	code, answer, raw = gateSessionPost(t, handler, fresh)
	require.Equal(t, http.StatusOK, code, raw)
	require.NotContains(t, raw, "not-created")
	require.NotEmpty(t, answer["sessionID"])
	b.mu.Lock()
	records = len(b.sessions)
	phases = map[string]int{}
	for _, record := range b.sessions {
		phases[record.Phase]++
	}
	b.mu.Unlock()
	require.Equal(t, refusalJournalCeiling+1, records)
	require.Equal(t, 1, phases["admitted"], "exactly the fresh request may be admitted")
	require.Zero(t, phases["prepared"])
	require.Equal(t, refusalJournalCeiling, phases["refused"])
}
