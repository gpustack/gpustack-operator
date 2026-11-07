package broker

// Tests for the quarantine diagnostic stage: an allowlisted HIXL stage rides
// the existing `reason` field of /v1/quarantine — the strict decoder gains no
// new JSON member — is persisted first-wins on the generation record, and an
// unlisted or secret-bearing reason is discarded while quarantine proceeds.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// quarantineStageFixture registers and fills one worker so quarantine is
// reachable over the real control handler. It returns the pieces the tests
// need to re-open the broker and read the record.
func quarantineStageFixture(t *testing.T) (*httptest.Server, *Broker, string, string, string) {
	t.Helper()
	server, b, generation, secret := newControl(t)
	status, body := post(t, server.URL, "/v1/register", map[string]any{
		"workerID": "source-worker-a", "generation": generation, "secret": secret,
	})
	require.Equal(t, http.StatusOK, status)
	leaseID := body["leaseID"].(string)
	require.Nil(t, b.FillVerified(t.Context(), generation, "source-worker-a", leaseID, secret, 1,
		[]RegistrationDescriptor{{
			Name: "model.norm.weight", Addr: 4096, Length: 6,
			MemoryType: "HOST", Backend: "nixl", WorkerUID: "source-worker-a",
		}}))
	return server, b, generation, secret, leaseID
}

func quarantinedStage(b *Broker, generation string) (stage, state string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.records[generation]
	if rec == nil {
		return "", "<no record>"
	}
	return rec.DiagnosticStage, rec.State
}

func TestQuarantineReasonStageIsPersistedFirstWins(t *testing.T) {
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-submit",
	})
	require.Equal(t, http.StatusOK, status)
	stage, state := quarantinedStage(b, generation)
	require.Equal(t, "submit", stage)
	require.Equal(t, "Quarantined", state)

	// A later report never overwrites a previously recorded stage.
	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-connect",
	})
	require.Equal(t, http.StatusOK, status)
	stage, _ = quarantinedStage(b, generation)
	require.Equal(t, "submit", stage, "no report may overwrite a recorded stage")

	// The persisted stage survives a broker restart on the same ledger.
	require.NoError(t, b.Close())
	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	reopened.mu.Lock()
	stage = reopened.records[generation].DiagnosticStage
	reopened.mu.Unlock()
	require.Equal(t, "submit", stage, "the recorded stage must persist across restart")
}

func TestQuarantineLegacyReasonIsAcceptedWithoutStage(t *testing.T) {
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "unknown operation outcome",
	})
	require.Equal(t, http.StatusOK, status)
	stage, state := quarantinedStage(b, generation)
	require.Equal(t, "", stage, "a legacy reason records no diagnostic stage")
	require.Equal(t, "Quarantined", state)

	// A later valid stage fills the empty field after authentication,
	// without unquarantining or altering any charge.
	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-poll",
	})
	require.Equal(t, http.StatusOK, status)
	stage, state = quarantinedStage(b, generation)
	require.Equal(t, "poll", stage)
	require.Equal(t, "Quarantined", state)
}

func TestQuarantineSecretBearingReasonIsDiscardedButQuarantines(t *testing.T) {
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret,
		"reason": "hixl-submit and the lease secret is " + secret,
	})
	require.Equal(t, http.StatusOK, status,
		"an unlistable reason must still quarantine the generation")
	stage, state := quarantinedStage(b, generation)
	require.Equal(t, "", stage, "an unlisted reason must not enter the ledger")
	require.Equal(t, "Quarantined", state)
}

func TestQuarantineStrictDecoderRejectsStageField(t *testing.T) {
	// Compatibility contract: the quarantine schema gains no member. A
	// client that adds a `stage` JSON member must be rejected by the strict
	// decoder, so older binaries keep working with newer clients.
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-submit",
		"stage": "submit",
	})
	require.Equal(t, http.StatusBadRequest, status,
		"the strict decoder must reject an added stage member")
	_, state := quarantinedStage(b, generation)
	require.NotEqual(t, "Quarantined", state,
		"a rejected message must not quarantine anything")
}

func TestQuarantineStageRequiresAuthentication(t *testing.T) {
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": "wrong-" + secret, "reason": "hixl-submit",
	})
	require.Equal(t, http.StatusForbidden, status)
	stage, state := quarantinedStage(b, generation)
	require.Equal(t, "", stage, "an unauthenticated report records no stage")
	require.NotEqual(t, "Quarantined", state)
}

func TestQuarantineStageWriteFailureRollsBackAndRetryPersists(t *testing.T) {
	// A real ledger write failure, injected the same way
	// TestAdmissionDoesNotExposeFailedLocalConfirmation does — a directory
	// parked on the record's tmp path — so OpenFile fails deterministically
	// without mocks. The in-memory fields must roll back so a retry
	// persists the stage, and the quarantine is retained through the
	// failure.
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()
	blocked := filepath.Join(b.root, recordsDir, generation+".json.tmp")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	defer func() {
		if err := os.Remove(blocked); err != nil {
			t.Logf("cleanup blocked path: %v", err)
		}
	}()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-submit",
	})
	require.NotEqual(t, http.StatusOK, status,
		"a failed ledger write must not answer success")
	b.mu.Lock()
	state := b.records[generation].State
	stage := b.records[generation].DiagnosticStage
	b.mu.Unlock()
	require.Equal(t, "Quarantined", state,
		"the quarantine itself is retained even though the write failed")
	require.Equal(t, "", stage,
		"the unfilled stage field must roll back on a failed write")

	// Remove the block: the retry persists the stage durably.
	require.NoError(t, os.Remove(blocked))
	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "hixl-submit",
	})
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, b.Close())
	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	reopened.mu.Lock()
	stage = reopened.records[generation].DiagnosticStage
	reopenedState := reopened.records[generation].State
	reopened.mu.Unlock()
	require.Equal(t, "submit", stage,
		"the retried report must persist the stage across restart")
	require.Equal(t, "Quarantined", reopenedState,
		"the retry must durably persist the transition into quarantine, "+
			"not just the hint")
}

func TestQuarantineUnstagedRetryPersistsAfterWriteFailure(t *testing.T) {
	// When the first quarantine ledger write fails, the in-memory state is
	// held as Quarantined so no new work is admitted, but the on-disk record
	// is still Serving. A subsequent retry that carries no valid stage hint
	// must still write the Quarantined state to the ledger rather than
	// returning 200 OK without persisting.
	server, b, generation, secret, leaseID := quarantineStageFixture(t)
	defer server.Close()
	blocked := filepath.Join(b.root, recordsDir, generation+".json.tmp")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	defer func() {
		_ = os.Remove(blocked)
	}()

	status, _ := post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "unknown operation outcome",
	})
	require.NotEqual(t, http.StatusOK, status,
		"a failed ledger write must not answer success")
	_, state := quarantinedStage(b, generation)
	require.Equal(t, "Quarantined", state,
		"the in-memory quarantine must still hold after a write failure")

	// While the ledger is still blocked, an unstaged retry must also fail
	// rather than claiming durability.
	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "unknown operation outcome",
	})
	require.NotEqual(t, http.StatusOK, status,
		"an unpersisted quarantine retry without a stage must not answer 200 while write fails")

	// Unblock the ledger: an unstaged retry must now persist StateQuarantined.
	require.NoError(t, os.Remove(blocked))
	status, _ = post(t, server.URL, "/v1/quarantine", map[string]any{
		"workerID": "source-worker-a", "generation": generation,
		"leaseID": leaseID, "secret": secret, "reason": "unknown operation outcome",
	})
	require.Equal(t, http.StatusOK, status)

	require.NoError(t, b.Close())
	reopened, openErr := Open(b.root, testScope, b.store, b.limits,
		func() time.Time { return testNow })
	require.NoError(t, openErr)
	defer reopened.Close()
	reopened.mu.Lock()
	reopenedState := reopened.records[generation].State
	reopened.mu.Unlock()
	require.Equal(t, "Quarantined", reopenedState,
		"the unstaged retry must durably persist StateQuarantined across restart")
}
