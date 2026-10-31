package report

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

// modelTransferEnv is a report environment whose reporter reads a broker ledger.
type modelTransferEnv struct {
	*testEnv
	ledgerRoot string
}

func newModelTransferEnv(t *testing.T) *modelTransferEnv {
	t.Helper()
	env := newTestEnv(t, validSpec())
	root := t.TempDir()
	env.r.ModelTransferStateRoot = root
	return &modelTransferEnv{testEnv: env, ledgerRoot: root}
}

// writeGeneration writes one generation record straight into the ledger the
// way the broker does, so the reporter reads real durable state.
func (e *modelTransferEnv) writeGeneration(t *testing.T, rec broker.GenerationRecord) {
	t.Helper()
	dir := filepath.Join(e.ledgerRoot, "generations")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	encoded, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, rec.Generation+".json"),
		encoded, 0o644))
}

func TestReportCarriesModelTransferGenerations(t *testing.T) {
	env := newModelTransferEnv(t)
	digest := "sha256:" + hexOf('a')
	env.writeGeneration(t, broker.GenerationRecord{
		Generation: "7", Digest: digest,
		State:         string(workercore.NodeModelStoreModelTransferGenerationStateServing),
		ResidentBytes: 400, Registrations: 2, ActiveSessions: 1, RejectedAdmissions: 3,
		LeaseID:      "lease-1",
		LeaseExpires: testNow.Add(time.Minute),
	})

	require.NoError(t, env.r.Report(context.Background()))
	status := env.nms(t).Status
	require.NotNil(t, status.ModelTransfer)
	require.Len(t, status.ModelTransfer.Generations, 1)
	entry := status.ModelTransfer.Generations[0]
	assert.Equal(t, "7", entry.Generation)
	assert.Equal(t, digest, entry.Digest)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateServing, entry.State)
	assert.EqualValues(t, 400, entry.ResidentBytes)
	assert.EqualValues(t, 2, entry.Registrations)
	assert.EqualValues(t, 1, entry.ActiveSessions)
	assert.EqualValues(t, 3, entry.RejectedAdmissions)

	// The reported status names no tenant and no worker, even though the
	// ledger entry does.
	encoded, err := json.Marshal(status.ModelTransfer)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "tenant")
	assert.NotContains(t, string(encoded), "worker")
}

func TestReportModelTransferAbsentWithoutGenerations(t *testing.T) {
	env := newModelTransferEnv(t)
	require.NoError(t, env.r.Report(context.Background()))
	assert.Nil(t, env.nms(t).Status.ModelTransfer)

	// A generation that drains away leaves the report, not a zero entry:
	// the absence is the invalidation.
}

func TestReportModelTransferExpiredLeaseReadsDraining(t *testing.T) {
	env := newModelTransferEnv(t)
	env.writeGeneration(t, broker.GenerationRecord{
		Generation: "9", Digest: "sha256:" + hexOf('b'),
		State:         string(workercore.NodeModelStoreModelTransferGenerationStateServing),
		ResidentBytes: 100,
		LeaseID:       "lease-2",
		// The lease expired long before the report's clock.
		LeaseExpires: testNow.Add(-2 * time.Hour),
	})
	require.NoError(t, env.r.Report(context.Background()))
	status := env.nms(t).Status
	require.NotNil(t, status.ModelTransfer)
	require.Len(t, status.ModelTransfer.Generations, 1)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateDraining,
		status.ModelTransfer.Generations[0].State)
}

func TestReportModelTransferDoesNotTouchDiskReadiness(t *testing.T) {
	env := newModelTransferEnv(t)
	env.publish(t, hexOf('a'), 300)
	env.writeGeneration(t, broker.GenerationRecord{
		Generation: "3", Digest: "sha256:" + hexOf('a'),
		State:         string(workercore.NodeModelStoreModelTransferGenerationStateDraining),
		ResidentBytes: 300,
	})
	require.NoError(t, env.r.Report(context.Background()))
	status := env.nms(t).Status

	// The published tree stays disk-ready while its generation drains.
	require.Len(t, status.Models, 1)
	assert.Equal(t, workercore.NodeModelStoreModelStateReady, status.Models[0].State)
	require.NotNil(t, status.ModelTransfer)
	assert.Equal(t, workercore.NodeModelStoreModelTransferGenerationStateDraining,
		status.ModelTransfer.Generations[0].State)
}

func TestReportWithoutModelTransferRootWritesNoModelTransfer(t *testing.T) {
	env := newTestEnv(t, validSpec())
	require.NoError(t, env.r.Report(context.Background()))
	assert.Nil(t, env.nms(t).Status.ModelTransfer)
}

func TestReportModelTransferVendorLedgers(t *testing.T) {
	env := newModelTransferEnv(t)
	for _, vendor := range []string{"ascend", "nvidia"} {
		child := &modelTransferEnv{testEnv: env.testEnv, ledgerRoot: filepath.Join(env.ledgerRoot, vendor)}
		child.writeGeneration(t, broker.GenerationRecord{
			Generation: "g-" + vendor + "-" + strings.Repeat("a", 32) + "-1",
			Digest:     "sha256:" + hexOf('a'), State: broker.StateServing,
			ResidentBytes: 64, LeaseID: "lease", LeaseExpires: testNow.Add(time.Minute),
		})
	}
	require.NoError(t, env.r.Report(context.Background()))
	modelTransfer := env.nms(t).Status.ModelTransfer
	require.NotNil(t, modelTransfer)
	require.Len(t, modelTransfer.Generations, 2)
	assert.Contains(t, modelTransfer.Generations[0].Generation, "ascend")
	assert.Contains(t, modelTransfer.Generations[1].Generation, "nvidia")
}

func TestReportModelTransferUnreadableStateInvalidatesServing(t *testing.T) {
	env := newModelTransferEnv(t)
	env.publish(t, hexOf('a'), 300)
	env.writeGeneration(t, broker.GenerationRecord{
		Generation: "1", Digest: "sha256:" + hexOf('a'), State: broker.StateServing,
		LeaseID: "lease", LeaseExpires: testNow.Add(time.Minute),
	})
	require.NoError(t, env.r.Report(context.Background()))
	require.NotNil(t, env.nms(t).Status.ModelTransfer)
	require.NoError(t, os.WriteFile(filepath.Join(env.ledgerRoot, "generations", "1.json"), []byte("{"), 0o600))
	require.NoError(t, env.r.Report(context.Background()))
	status := env.nms(t).Status
	assert.Nil(t, status.ModelTransfer, "unreadable state must invalidate a previous Serving advertisement")
	assert.Equal(t, "False", string(findCondition(status.Conditions, "ModelTransferReady").Status))
	assert.Equal(t, "True", string(findCondition(status.Conditions, ConditionReady).Status))
	require.Len(t, status.Models, 1)
	assert.Equal(t, workercore.NodeModelStoreModelStateReady, status.Models[0].State)
}
