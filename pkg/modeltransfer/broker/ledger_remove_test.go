package broker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newRemoveTestLedger opens one fresh ledger and writes one record into it.
func newRemoveTestLedger(t *testing.T) (*ledger, string, string) {
	t.Helper()
	root := t.TempDir()
	l, fresh, err := openLedger(root, testScope)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, l.ensureCounter(fresh))
	generation, err := l.nextGeneration()
	require.NoError(t, err)
	rec := &GenerationRecord{
		Generation: generation, Digest: "digest", Namespace: "tenant",
		WorkerID: "worker", WorkerSecretHash: "hash", State: StateReleased,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, l.write(rec))
	return l, root, generation
}

// TestLedgerRemovalIsIdempotent checks removal and reload; crash durability follows
// from the directory sync in the source, and is not tested by this fixture.
func TestLedgerRemovalIsIdempotent(t *testing.T) {
	l, root, generation := newRemoveTestLedger(t)
	require.NoError(t, l.remove(generation))
	require.NoFileExists(t, filepath.Join(root, recordsDir, generation+".json"))
	require.NoError(t, l.remove(generation), "a repeat removal stays a no-op")
	records, err := l.records()
	require.NoError(t, err)
	require.Empty(t, records)
}

// TestLedgerRemovalNamesTheFailure pins the failed path: with the records
// directory itself gone, the removal answers an explicit error instead of a
// silent nil, so a caller keeps the record's charge held rather than
// reporting a release it could not make durable.
func TestLedgerRemovalNamesTheFailure(t *testing.T) {
	l, root, generation := newRemoveTestLedger(t)
	require.NoError(t, os.Remove(filepath.Join(root, recordsDir, generation+".json")))
	require.NoError(t, os.Remove(filepath.Join(root, recordsDir)))
	err := l.remove(generation)
	require.Error(t, err, "a removal that cannot sync the records directory must say so")
	require.Contains(t, err.Error(), recordsDir)
}
