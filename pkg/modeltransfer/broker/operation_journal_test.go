package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationJournalBoundedDocumentRead(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: 16},
		{name: "over limit", size: 17, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt.json")
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(" ", tt.size)), 0o600))
			raw, err := readOperationDocument(path, 16)
			if tt.wantErr {
				require.ErrorContains(t, err, "byte bound")
				return
			}
			require.NoError(t, err)
			require.Len(t, raw, tt.size)
		})
	}
}

func TestOperationJournalFailedReloadClosesAdmission(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	path := filepath.Join(b.root, operationDir, strings.Repeat("a", 32)+".json")
	require.NoError(t, os.Symlink("missing", path))
	require.Error(t, b.EnableOperationJournal())
	require.False(t, b.operationsReady)
}

func TestOperationStagingCleanupPreservesUncommittedEvidenceOnCorruption(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			b, _, _ := newBroker(t, defaultLimits())
			require.NoError(t, b.EnableOperationJournal())
			id := strings.Repeat("a", 32)
			op := &OperationRecord{Request: TransferRequest{RequestID: id, SessionID: "session"}, SourceLeaseID: "lease", State: "unknown"}
			require.NoError(t, b.persistOperationLocked(op))
			staging := filepath.Join(b.root, operationDir, id+".json.tmp")
			unowned := filepath.Join(b.root, operationDir, "unrelated.tmp")
			require.NoError(t, os.WriteFile(staging, []byte("uncommitted"), 0o600))
			require.NoError(t, os.WriteFile(unowned, []byte("keep"), 0o600))
			if corrupt {
				require.NoError(t, os.WriteFile(filepath.Join(b.root, operationDir, id+".json"), []byte("{"), 0o600))
			}
			err := b.EnableOperationJournal()
			if corrupt {
				require.Error(t, err)
				require.FileExists(t, staging)
			} else {
				require.NoError(t, err)
				require.NoFileExists(t, staging)
				require.Equal(t, "unknown", b.operations[id].State)
			}
			raw, err := os.ReadFile(unowned)
			require.NoError(t, err)
			require.Equal(t, "keep", string(raw))
		})
	}
}

func TestOperationJournalByteBoundPreservesReceipt(t *testing.T) {
	for _, tt := range []struct {
		name    string
		excess  int64
		wantErr bool
	}{
		{name: "exact capacity"},
		{name: "one byte over", excess: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, _, _ := newBroker(t, defaultLimits())
			require.NoError(t, b.EnableOperationJournal())
			op := &OperationRecord{Request: TransferRequest{RequestID: strings.Repeat("a", 32), SessionID: "session"}, SourceLeaseID: "lease", State: "unknown"}
			raw, err := json.Marshal(op)
			require.NoError(t, err)
			before := int64(operationJournalBytes-len(raw)) + tt.excess
			b.operationBytes = before
			err = b.persistOperationLocked(op)
			if tt.wantErr {
				require.Error(t, err)
				require.Equal(t, before, b.operationBytes)
				require.Empty(t, b.operations)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, operationJournalBytes, b.operationBytes)
			require.Equal(t, op, b.operations[op.Request.RequestID])
		})
	}
}

func TestOperationJournalMillionReceiptBound(t *testing.T) {
	b, _, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	require.Equal(t, 1_048_576, operationLimit)
	for i := range operationLimit {
		b.operations[fmt.Sprintf("%032x", i)] = nil
	}
	op := &OperationRecord{Request: TransferRequest{RequestID: strings.Repeat("f", 32), SessionID: "session"}, SourceLeaseID: "lease", State: "unknown"}
	require.ErrorContains(t, b.persistOperationLocked(op), "full")
	delete(b.operations, strings.Repeat("0", 32))
	require.NoError(t, b.persistOperationLocked(op))
	require.Len(t, b.operations, operationLimit)
	// Replacing an existing receipt needs no additional count slot.
	require.NoError(t, b.persistOperationLocked(op))
}

func TestTargetWindowValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		target TransferTarget
		ip     string
		want   bool
	}{
		{"nixl", TransferTarget{Backend: "nixl", Address: 1, Length: 64, Metadata: "YQ=="}, "", true},
		{"nixl malformed metadata", TransferTarget{Backend: "nixl", Address: 1, Length: 64, Metadata: "%"}, "", false},
		{"hixl owner address", TransferTarget{Backend: "hixl", Address: 1, Length: 64, Endpoint: "10.0.0.1:3000"}, "10.0.0.1", true},
		{"hixl foreign address", TransferTarget{Backend: "hixl", Address: 1, Length: 64, Endpoint: "10.0.0.2:3000"}, "10.0.0.1", false},
		{"maximum window", TransferTarget{Backend: "nixl", Address: 1, Length: 1 << 40, Metadata: "YQ=="}, "", true},
		{"oversized", TransferTarget{Backend: "nixl", Address: 1, Length: (1 << 40) + 1, Metadata: "YQ=="}, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, validTarget(tt.target, 1, tt.ip)) })
	}
}

func TestOperationRestartNeverRepostsAndHoldsDrain(t *testing.T) {
	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	id := strings.Repeat("a", 32)
	op := &OperationRecord{Request: TransferRequest{RequestID: id, SessionID: "session", PodUID: "owner", TargetInstanceID: strings.Repeat("b", 32), Generation: "old", SourcePodUID: "pod", WorkerInstanceID: "boot"}, SourceLeaseID: "lease", State: "dispatching"}
	b.mu.Lock()
	require.NoError(t, b.persistOperationLocked(op))
	b.mu.Unlock()
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, b.scope, st, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(t, reopened.EnableOperationJournal())
	private := &rejectTestSource{}
	reopened.SetPrivateSource(private)
	got, err := reopened.OperationStatus(context.Background(), id, "session", "owner", strings.Repeat("b", 32))
	require.NoError(t, err)
	assert.Equal(t, "unknown", got.State)
	assert.Equal(t, 0, private.calls)
	reopened.assignmentsFrozen, reopened.inventoryRequired, reopened.inventoryVerified = true, true, true
	assert.False(t, reopened.ReadyForDrain())
}

type rejectTestSource struct{ calls int }

func (s *rejectTestSource) Call(context.Context, string, any) (PrivateReply, error) {
	s.calls++
	return PrivateReply{}, errors.New("connection lost")
}

func TestUnadmittedReceiptSurvivesRestartAndRefusesConflict(t *testing.T) {
	b, st, _ := newBroker(t, defaultLimits())
	require.NoError(t, b.EnableOperationJournal())
	owner := SessionOwner{PodUID: "owner", Namespace: "tenant", RequestID: strings.Repeat("a", 32), TargetInstanceID: strings.Repeat("b", 32), OpenFingerprint: "fingerprint"}
	require.True(t, b.RecordUnadmitted(owner))
	require.True(t, b.RecordUnadmitted(owner))
	changed := owner
	changed.OpenFingerprint = "different"
	require.False(t, b.RecordUnadmitted(changed))
	require.NoError(t, b.Close())
	reopened, err := Open(b.root, b.scope, st, b.limits, func() time.Time { return testNow })
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(t, reopened.EnableOperationJournal())
	assert.True(t, reopened.RecordUnadmitted(owner))
	assert.False(t, reopened.RecordUnadmitted(changed))
}
