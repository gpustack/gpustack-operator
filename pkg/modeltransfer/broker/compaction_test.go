package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func testOperation(id, sessionID, generation, state, verdict string) *OperationRecord {
	return &OperationRecord{
		Request:       TransferRequest{RequestID: id, SessionID: sessionID, Generation: generation, PodUID: "owner"},
		SourceLeaseID: "lease",
		State:         state,
		Verdict:       verdict,
	}
}

func operationFileExists(t *testing.T, b *Broker, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(b.root, operationDir, id+".json"))
	if os.IsNotExist(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestOperationCompactionReclaimsFinishedSessionsAndGenerations(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.mu.Lock()
	b.records["g-keep"] = &GenerationRecord{Generation: "g-keep", Digest: digest, State: StateServing}
	b.records["g-released"] = &GenerationRecord{Generation: "g-released", Digest: digest, State: StateReleased}
	b.sessions["session-active"] = &SessionRecord{Generation: "g-keep", OwnerUID: "pod", Namespace: "tenant-a", Phase: "admitted"}
	b.sessions["session-done"] = &SessionRecord{Generation: "g-keep", OwnerUID: "pod", Namespace: "tenant-a", Phase: "released"}
	ops := []*OperationRecord{
		testOperation(strings.Repeat("a", 32), "session-active", "g-keep", "acknowledged", "succeeded"),
		testOperation(strings.Repeat("b", 32), "session-done", "g-keep", "acknowledged", "succeeded"),
		testOperation(strings.Repeat("c", 32), "session-gone", "g-released", "acknowledged", "failed"),
		testOperation(strings.Repeat("d", 32), "session-active", "g-vanished", "acknowledged", "succeeded"),
		testOperation(strings.Repeat("e", 32), "session-done", "g-keep", "submitted", ""),
		testOperation(strings.Repeat("f", 32), "session-active", "g-keep", "unknown", ""),
	}
	for _, op := range ops {
		require.NoError(t, b.persistOperationLocked(op))
	}
	b.mu.Unlock()

	b.mu.Lock()
	b.compactOperationsLocked()
	b.mu.Unlock()

	assert.Len(t, b.operations, 3, "only finished receipts leave the journal")
	assert.Contains(t, b.operations, strings.Repeat("a", 32), "the active session's acknowledged receipt stays")
	assert.Contains(t, b.operations, strings.Repeat("e", 32), "an unacknowledged receipt is never reclaimed")
	assert.Contains(t, b.operations, strings.Repeat("f", 32), "an unacknowledged receipt is never reclaimed")
	assert.NotContains(t, b.operations, strings.Repeat("b", 32), "a released session's acknowledged receipt goes")
	assert.NotContains(t, b.operations, strings.Repeat("c", 32), "a released generation's acknowledged receipt goes")
	assert.NotContains(t, b.operations, strings.Repeat("d", 32), "a vanished generation's acknowledged receipt goes")
	assert.False(t, operationFileExists(t, b, strings.Repeat("b", 32)))
	assert.False(t, operationFileExists(t, b, strings.Repeat("c", 32)))
	assert.True(t, operationFileExists(t, b, strings.Repeat("a", 32)))
	var persisted int64
	b.mu.Lock()
	for _, op := range b.operations {
		raw, err := json.Marshal(op)
		require.NoError(t, err)
		persisted += int64(len(raw))
	}
	assert.Equal(t, persisted, b.operationBytes, "the journal's byte total matches what remains")
	b.mu.Unlock()
}

func TestOperationCompactionTrimsActiveSessionHistoryUnderPressure(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.mu.Lock()
	b.records["g-keep"] = &GenerationRecord{Generation: "g-keep", Digest: digest, State: StateServing}
	b.sessions["session-long"] = &SessionRecord{Generation: "g-keep", OwnerUID: "pod", Namespace: "tenant-a", Phase: "admitted"}
	for _, id := range []string{"01", "02", "03"} {
		require.NoError(t, b.persistOperationLocked(
			testOperation(strings.Repeat(id, 16), "session-long", "g-keep", "acknowledged", "succeeded"),
		))
	}
	require.NoError(t, b.persistOperationLocked(
		testOperation(strings.Repeat("04", 16), "session-long", "g-keep", "submitted", ""),
	))
	// Push the journal past half its byte bound the way a long-lived
	// session's window history would; the receipts themselves stay small.
	b.operationBytes = operationJournalBytes/2 + 1
	b.compactOperationsLocked()
	b.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	assert.Len(t, b.operations, 2, "one acknowledged receipt and the submitted receipt remain")
	assert.Contains(t, b.operations, strings.Repeat("03", 16), "the surviving acknowledged receipt is deterministic")
	assert.Contains(t, b.operations, strings.Repeat("04", 16), "the submitted receipt is never trimmed")
}

func TestSessionCompactionReclaimsTerminalRecords(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.mu.Lock()
	b.records["g-released"] = &GenerationRecord{Generation: "g-released", Digest: digest, State: StateReleased}
	b.records["g-serving"] = &GenerationRecord{Generation: "g-serving", Digest: digest, State: StateServing}
	b.sessions["session-released-done"] = &SessionRecord{Generation: "g-released", OwnerUID: "pod", Namespace: "tenant-a", Phase: "released"}
	b.sessions["session-released-held"] = &SessionRecord{Generation: "g-serving", OwnerUID: "pod", Namespace: "tenant-a", Phase: "released"}
	b.sessions["session-admitted"] = &SessionRecord{Generation: "g-serving", OwnerUID: "pod", Namespace: "tenant-a", Phase: "admitted"}
	b.sessions["session-refused"] = &SessionRecord{OwnerUID: "pod", Namespace: "tenant-a", Phase: "refused"}
	// A receipt naming a session blocks its reclaim, whatever its state.
	require.NoError(t, b.persistOperationLocked(
		testOperation(strings.Repeat("a", 32), "session-released-held", "g-serving", "acknowledged", "succeeded"),
	))
	b.mu.Unlock()

	b.mu.Lock()
	require.NoError(t, b.compactSessionsLocked())
	b.mu.Unlock()

	assert.NotContains(t, b.sessions, "session-released-done", "a released session of a released generation goes")
	assert.Contains(t, b.sessions, "session-released-held", "a session a receipt still names stays")
	assert.Contains(t, b.sessions, "session-admitted")
	assert.Contains(t, b.sessions, "session-refused", "a refused record below the pressure bound stays")
	b.mu.Lock()
	persisted, err := b.loadSessions()
	b.mu.Unlock()
	require.NoError(t, err)
	assert.NotContains(t, persisted, "session-released-done", "the reclaim is persisted")
	assert.Contains(t, persisted, "session-released-held")
}

func TestSessionCompactionReclaimsRefusedRecordsUnderPressure(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.mu.Lock()
	b.records["g-serving"] = &GenerationRecord{Generation: "g-serving", Digest: digest, State: StateServing}
	for i := range 2048 {
		b.sessions[fmt.Sprintf("filler-%04d", i)] = &SessionRecord{Generation: "g-serving", OwnerUID: "pod", Namespace: "tenant-a", Phase: "admitted"}
	}
	b.sessions["session-refused"] = &SessionRecord{OwnerUID: "pod", Namespace: "tenant-a", Phase: "refused"}
	b.sessions["session-released"] = &SessionRecord{OwnerUID: "pod", Namespace: "tenant-a", Phase: "released"}
	b.mu.Unlock()

	b.mu.Lock()
	require.NoError(t, b.compactSessionsLocked())
	count := len(b.sessions)
	_, refusedKept := b.sessions["session-refused"]
	_, releasedKept := b.sessions["session-released"]
	b.mu.Unlock()

	assert.False(t, refusedKept, "a refused record is reclaimed under the pressure bound")
	assert.False(t, releasedKept)
	assert.Equal(t, 2048, count, "admitted sessions are never reclaimed")
}

// releasedToken builds a distinct ordered generation token for the
// compaction test: legacy numeric tokens order by their number.
func releasedToken(i int) string {
	return strconv.Itoa(i)
}

func TestReleasedGenerationCompactionKeepsRecentHistory(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	b.mu.Lock()
	b.assignmentSizes = map[string]int{}
	for i := 1; i <= 70; i++ {
		b.records[releasedToken(i)] = &GenerationRecord{Generation: releasedToken(i), State: StateReleased}
		b.assignmentSizes[releasedToken(i)] = 1
	}
	b.sessions["session-holder"] = &SessionRecord{Generation: releasedToken(2), OwnerUID: "pod", Namespace: "tenant-a", Phase: "admitted"}
	b.operations[strings.Repeat("a", 32)] = testOperation(strings.Repeat("a", 32), "session-holder", releasedToken(3), "acknowledged", "succeeded")
	b.mu.Unlock()

	b.mu.Lock()
	require.NoError(t, b.compactReleasedGenerationsLocked())
	count := len(b.records)
	cached := len(b.assignmentSizes)
	_, referencedSession := b.records[releasedToken(2)]
	_, referencedOperation := b.records[releasedToken(3)]
	_, oldest := b.records[releasedToken(1)]
	_, withinKeep := b.records[releasedToken(70)]
	b.mu.Unlock()

	assert.Zero(t, cached, "released history must not retain assignment response sizes")
	assert.Equal(t, 66, count, "the referenced records and the newest 64 released records remain")
	assert.True(t, referencedSession, "a record a session names is kept whatever its age")
	assert.True(t, referencedOperation, "a record a receipt names is kept whatever its age")
	assert.False(t, oldest, "the oldest unreferenced released records go first")
	assert.True(t, withinKeep, "the newest released records stay as history")
}

func TestRetrySessionTransactionsSharesLivenessAcrossSessionsAndToleratesLookupFailure(t *testing.T) {
	build := func(t *testing.T) (*Broker, Intent, string) {
		t.Helper()
		b, _, digest := newBroker(t, defaultLimits())
		require.NoError(t, b.EnableOperationJournal())
		intent := Intent{
			Key: "shared-liveness", Namespace: "tenant-a", Digest: digest,
			TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
			IntentUID: "prefetch-uid", IntentGeneration: 1, BindingUID: "binding-uid-1",
			ReservationID: "shared-liveness-reservation", Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
		}
		assignment, perr := b.AdmitIntentFill(t.Context(), intent)
		require.Nil(t, perr)
		lease, perr := b.RegisterWorker(assignment.Generation, intent.WorkerID, assignment.WorkerSecret, "instance-a")
		require.Nil(t, perr)
		require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
		require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, 1,
			[]RegistrationDescriptor{{Name: "file", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID}}, "instance-a"))
		return b, intent, assignment.Generation
	}

	t.Run("one owner's sessions share one liveness read", func(t *testing.T) {
		b, intent, generation := build(t)
		owner := SessionOwner{PodUID: "target-pod", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: intent.Digest}
		first, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, generation)
		require.Nil(t, perr)
		second, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, generation)
		require.Nil(t, perr)
		checks := 0
		b.SetSessionPodChecker(func(context.Context, string, string) (bool, error) {
			checks++
			return false, nil
		})
		require.NoError(t, b.RetrySessionTransactions(context.Background()))
		assert.Equal(t, 1, checks, "one owner's sessions share one liveness read per pass")
		assert.Equal(t, "released", b.sessions[first.ID].Phase)
		assert.Equal(t, "released", b.sessions[second.ID].Phase)
	})

	t.Run("a failed liveness read holds the session and the pass", func(t *testing.T) {
		b, intent, generation := build(t)
		owner := SessionOwner{PodUID: "target-pod", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: intent.Digest}
		session, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, generation)
		require.Nil(t, perr)
		b.SetSessionPodChecker(func(context.Context, string, string) (bool, error) {
			return false, errors.New("the API server is unreachable")
		})
		require.NoError(t, b.RetrySessionTransactions(context.Background()),
			"a transient lookup failure never fails the pass")
		assert.Equal(t, "admitted", b.sessions[session.ID].Phase, "a failed lookup never reads as Pod disappearance")
		assert.EqualValues(t, 1, b.records[generation].ActiveSessions)
	})
}

func TestMissingWorkerPodRetainsNativeOwnershipAcrossRestart(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	intent := Intent{
		Key: "fence-intent", Namespace: "tenant-a", Digest: digest,
		TotalBytes: 12, Grant: grant("tenant-a", 10000), WorkerID: "source-worker-a",
		IntentUID: "prefetch-uid", IntentGeneration: 1, BindingUID: "binding-uid-1",
		ReservationID: "fence-reservation", Registrations: 1, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	}
	assignment, perr := b.AdmitIntentFill(t.Context(), intent)
	require.Nil(t, perr)
	lease, perr := b.RegisterWorker(assignment.Generation, intent.WorkerID, assignment.WorkerSecret, "instance-a")
	require.Nil(t, perr)
	require.Nil(t, b.NativeStart(assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, "instance-a"))
	require.Nil(t, b.FillVerified(t.Context(), assignment.Generation, intent.WorkerID, lease.ID, assignment.WorkerSecret, 1,
		[]RegistrationDescriptor{{Name: "file", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: intent.WorkerID}}, "instance-a"))
	owner := SessionOwner{PodUID: "target-pod", Namespace: intent.Namespace, ArtifactUID: intent.ArtifactUID, Digest: digest}
	session, perr := b.AdmitSession(context.Background(), owner, intent.Namespace, assignment.Generation)
	require.Nil(t, perr)
	opID := strings.Repeat("d", 32)
	b.mu.Lock()
	require.NoError(t, b.persistOperationLocked(&OperationRecord{
		Request: TransferRequest{
			RequestID: opID, SessionID: session.ID, PodNamespace: intent.Namespace,
			PodUID: owner.PodUID, TargetInstanceID: owner.TargetInstanceID,
			Generation: assignment.Generation, SourcePodUID: intent.WorkerID,
		},
		SourceLeaseID: lease.ID, ArtifactUID: intent.ArtifactUID, Digest: digest,
		State: "unknown",
	}))
	// The API can lose the Pod while its native worker is still running.
	rec := b.records[assignment.Generation]
	rec.State = StateQuarantined
	rec.UnknownSessions = 2
	b.recountSessions(rec)
	require.NoError(t, b.ledger.write(rec))
	priorRecord, priorSession, priorOperation := *rec, *b.sessions[session.ID], *b.operations[opID]
	authority := b.reservationAuthorities[types.UID(intent.BindingUID)]
	ledgerID := b.ledger.identity
	b.mu.Unlock()

	replacement := strings.Repeat("e", 32)
	require.False(t, b.CanBindWorkerInstance(replacement))
	require.NoError(t, b.FenceReplacedWorkerPods(map[string]bool{}, "new-worker-pod"))

	require.Equal(t, priorRecord, *b.records[assignment.Generation])
	require.Equal(t, priorSession, *b.sessions[session.ID])
	require.Equal(t, priorOperation, *b.operations[opID])
	require.False(t, b.CanBindWorkerInstance(replacement))
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, b.scope, b.store, b.limits, b.now)
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.EnableOperationJournal())
	for range 2 {
		require.NoError(t, reopened.Reconstruct(func() (map[string]bool, error) { return map[string]bool{}, nil }))
		require.NoError(t, reopened.FenceReplacedWorkerPods(map[string]bool{}, "new-worker-pod"))
		require.NoError(t, reopened.RetryGenerationReleases(context.Background()))
		assert.Equal(t, priorRecord, *reopened.records[assignment.Generation])
		assert.Equal(t, priorSession, *reopened.sessions[session.ID])
		assert.Equal(t, priorOperation, *reopened.operations[opID])
		assert.False(t, reopened.CanBindWorkerInstance(replacement))
	}
	reserved, err := authority.Adopt(context.Background(), b.scope, ledgerID)
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	assert.Equal(t, priorRecord.ReservedBytes, reserved[0].Bytes)
	require.Len(t, reserved[0].Sessions, 1)
	assert.Equal(t, session.ID, reserved[0].Sessions[0].SessionID)
}

func TestFenceReplacedWorkerPodsSparesLiveAndCurrentWorkers(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	replacement := strings.Repeat("e", 32)
	b.mu.Lock()
	b.records["g-live"] = &GenerationRecord{
		Generation: "g-live", WorkerID: "source-worker-a", State: StateQuarantined,
		NativeStarted: true, NativeInstance: "instance-a", NativeStartEnforced: true,
		UnknownSessions: 3,
	}
	b.recountSessions(b.records["g-live"])
	b.mu.Unlock()
	require.False(t, b.CanBindWorkerInstance(replacement))

	require.NoError(t, b.FenceReplacedWorkerPods(map[string]bool{"source-worker-a": true}, "new-worker-pod"))
	b.mu.Lock()
	assert.Equal(t, StateQuarantined, b.records["g-live"].State,
		"a live worker's charge is never fenced from a restart report alone")
	assert.Positive(t, b.records["g-live"].UnknownSessions)
	b.mu.Unlock()
	assert.False(t, b.CanBindWorkerInstance(replacement))

	require.NoError(t, b.FenceReplacedWorkerPods(map[string]bool{}, "source-worker-a"))
	b.mu.Lock()
	assert.Equal(t, StateQuarantined, b.records["g-live"].State,
		"the running worker's own generation is never fenced")
	b.mu.Unlock()
	assert.False(t, b.CanBindWorkerInstance(replacement))

	require.NoError(t, b.FenceReplacedWorkerPods(nil, replacement))
	require.NoError(t, b.FenceReplacedWorkerPods(map[string]bool{}, ""))
	b.mu.Lock()
	assert.Equal(t, StateQuarantined, b.records["g-live"].State,
		"an unusable worker set fences nothing")
	b.mu.Unlock()
}

func TestReconstructReleasesOnlyProvenNeverStartedFill(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	assignment, perr := b.AdmitIntentFill(t.Context(), Intent{
		Key: "never-started", Namespace: "tenant-a", Digest: digest, TotalBytes: 12,
		Grant: grant("tenant-a", 10000), WorkerID: "old-source",
		IntentUID: "prefetch-uid", IntentGeneration: 1, BindingUID: "binding-uid-1",
		ReservationID: "never-started-reservation", Registrations: 1,
		ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
	})
	require.Nil(t, perr)
	require.True(t, b.records[assignment.Generation].NativeStartEnforced)
	require.False(t, b.records[assignment.Generation].NativeStarted)
	require.NoError(t, b.Reconstruct(func() (map[string]bool, error) { return map[string]bool{}, nil }))
	require.True(t, b.records[assignment.Generation].NativeReleased)
	require.NoError(t, b.RetryGenerationReleases(context.Background()))
	require.Equal(t, StateReleased, b.records[assignment.Generation].State)
}
