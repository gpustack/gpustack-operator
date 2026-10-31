package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeartbeatWriteFailurePreservesState(t *testing.T) {
	server, b, generation, secret, lease := quarantineStageFixture(t)
	defer server.Close()
	before := *b.records[generation]
	blocked := filepath.Join(b.root, recordsDir, generation+".json.tmp")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	result, err := b.Heartbeat(generation, "source-worker-a", lease, secret, Health{DrainRequested: true, ResidentBytes: 1})
	require.NotNil(t, err)
	require.Nil(t, result)
	require.Equal(t, before, *b.records[generation])
	require.NoError(t, os.Remove(blocked))
	result, err = b.Heartbeat(generation, "source-worker-a", lease, secret, Health{DrainRequested: true, ResidentBytes: 1})
	require.Nil(t, err)
	require.Equal(t, ActionDrain, result.Action)
	require.Equal(t, int64(1), b.records[generation].ResidentBytes)
}

func TestFillWriteFailureDoesNotAdmitReaders(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	req := fill("tenant-a", digest, 12)
	admitted, perr := b.AdmitFill(req)
	require.Nil(t, perr)
	lease, perr := b.RegisterWorker(admitted.Generation, req.WorkerID, admitted.WorkerSecret)
	require.Nil(t, perr)
	before := *b.records[admitted.Generation]
	blocked := filepath.Join(b.root, recordsDir, admitted.Generation+".json.tmp")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	descriptors := []RegistrationDescriptor{{Name: "weights", Addr: 1, Length: 12, MemoryType: "HOST", Backend: "nixl", WorkerUID: req.WorkerID}}
	perr = b.FillVerified(admitted.Generation, req.WorkerID, lease.ID, admitted.WorkerSecret, 1, descriptors)
	require.NotNil(t, perr)
	require.Equal(t, before, *b.records[admitted.Generation])
	_, perr = b.AdmitSession(testOwner("tenant-a"), "tenant-a", admitted.Generation)
	require.NotNil(t, perr)
	require.NoError(t, os.Remove(blocked))
	require.Nil(t, b.FillVerified(admitted.Generation, req.WorkerID, lease.ID, admitted.WorkerSecret, 1, descriptors))
	_, perr = b.AdmitSession(testOwner("tenant-a"), "tenant-a", admitted.Generation)
	require.Nil(t, perr)
}

func TestSessionManifestPageRefusesMissingValidator(t *testing.T) {
	b, req, identity, _ := manifestPageFixture(t, 1, 0)
	b.SetIntentValidator(nil)
	raw, err := b.sessionManifestPage(context.Background(), req, identity)
	require.Error(t, err)
	require.Empty(t, raw)
}
