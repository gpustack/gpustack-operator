package report

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
)

// progressStep is one report of TestReportWritesProgressOnThresholds: the clock moves by after, the
// download holds downloaded of its 1000 bytes and the partials grow by grow, and the writes so far
// are wantWrites.
type progressStepCase struct {
	name       string
	after      time.Duration
	downloaded int64
	grow       int64
	newEntry   bool
	wantWrites int64
}

// TestReportWritesProgressOnThresholds pins the progress write rule: progress fields alone are
// written only after 30 seconds and a 5% step, never for the partials' bytes alone, and a state
// change is written at once with the current progress.
func TestReportWritesProgressOnThresholds(t *testing.T) {
	steps := []progressStepCase{
		{name: "a new download is a state change", wantWrites: 1},
		{name: "10% moved but only 10 s after the write", after: 10 * time.Second, downloaded: 100, grow: 100, wantWrites: 1},
		{name: "35 s after the write and 12% moved", after: 25 * time.Second, downloaded: 120, grow: 20, wantWrites: 2},
		{name: "40 s after the write, 2% moved", after: 40 * time.Second, downloaded: 140, grow: 20, wantWrites: 2},
		{name: "the partials grow alone", after: 40 * time.Second, downloaded: 140, grow: 300, wantWrites: 2},
		{name: "5% moved, 80 s after the write", after: 0, downloaded: 170, wantWrites: 3},
		{name: "a big move 1 s after a write", after: time.Second, downloaded: 900, wantWrites: 3},
		{name: "a state change 1 s later carries the current progress", after: time.Second, downloaded: 910, newEntry: true, wantWrites: 4},
	}

	env := newTestEnv(t, validSpec())
	now := testNow
	env.r.Now = func() time.Time { return now }
	hex := hexOf('a')
	env.running[hex] = true
	var downloaded int64
	env.r.Progress = func() map[string]materialize.DownloadProgress {
		return map[string]materialize.DownloadProgress{hex: {
			DownloadedBytes: downloaded, SizeBytes: 1000, Source: workercore.NodeModelStoreModelSourceHub,
		}}
	}
	a, err := env.store.NewAttempt(hex)
	require.NoError(t, err)
	partial, err := a.FilePath("model.bin")
	require.NoError(t, err)
	var partialSize int64

	for _, s := range steps {
		now = now.Add(s.after)
		downloaded = s.downloaded
		partialSize += s.grow
		require.NoError(t, os.WriteFile(partial, make([]byte, partialSize), 0o644))
		if s.newEntry {
			env.running[hexOf('b')] = true
		}
		require.NoError(t, env.r.Report(context.Background()))
		assert.Equal(t, s.wantWrites, env.writes.Load(), s.name)
	}

	entry := env.nms(t).Status.Models
	require.NotEmpty(t, entry)
	var got workercore.NodeModelStoreModel
	for _, m := range entry {
		if m.Digest == "sha256:"+hex {
			got = m
		}
	}
	assert.Equal(t, int64(910), got.DownloadedBytes, "the state change carried the current progress")
	assert.Equal(t, int64(1000), got.SizeBytes)
	assert.Equal(t, workercore.NodeModelStoreModelSourceHub, got.Source)
}

func TestReportWritesAPartialRemovalWithoutADownload(t *testing.T) {
	env := newTestEnv(t, validSpec())
	a, err := env.store.NewAttempt(hexOf('a'))
	require.NoError(t, err)
	partial, err := a.FilePath("model.bin")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(partial, make([]byte, 100), 0o644))
	require.NoError(t, env.r.Report(context.Background()))
	require.Equal(t, int64(1), env.writes.Load())

	require.NoError(t, os.Remove(partial))
	require.NoError(t, env.r.Report(context.Background()))
	assert.Equal(t, int64(2), env.writes.Load(), "with nothing downloading, stored bytes moving is a state change")
	assert.Zero(t, env.nms(t).Status.Capacity.StoredBytes)
}

func TestReportNamesTheSourceOfPublishedContent(t *testing.T) {
	env := newTestEnv(t, validSpec())
	env.publish(t, hexOf('a'), 300)
	require.NoError(t, env.r.Report(context.Background()))
	models := env.nms(t).Status.Models
	require.Len(t, models, 1)
	assert.Empty(t, models[0].Source, "a tree published without a recorded source has none")
	assert.Zero(t, models[0].DownloadedBytes, "only a download carries progress")
}
