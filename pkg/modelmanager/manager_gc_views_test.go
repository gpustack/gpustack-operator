package modelmanager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// TestStartCollectorSweepsTheMetadataViews pins the GC wiring: the collector
// Start builds carries the store's canonical view root, so terminal view
// reclamation runs on the production path without any test setting ViewRoot
// by hand. A terminal view under the store's own layout goes at the next
// collection through exactly this constructor.
func TestStartCollectorSweepsTheMetadataViews(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(root)
	require.NoError(t, err)
	m := &Manager{Store: st, NodeName: "node-1"}
	materializer := &materialize.Materializer{Store: st, Now: time.Now, Base: context.Background()}

	collector := m.newCollector(materializer)
	require.NotNil(t, collector)
	require.NotEmpty(t, collector.ViewRoot)
	assert.True(t, strings.HasSuffix(collector.ViewRoot, filepath.Join("model-transfer", "views")),
		"the production collector sweeps the store's canonical view root, got %q", collector.ViewRoot)
	assert.True(t, strings.HasPrefix(collector.ViewRoot, root),
		"the view root lives under the store root, got %q", collector.ViewRoot)

	viewsRoot := st.ViewsRoot()
	require.NoError(t, os.MkdirAll(viewsRoot, 0o755))
	lock, err := os.OpenFile(filepath.Join(viewsRoot, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	view := filepath.Join(viewsRoot, strings.Repeat("a", 64))
	require.NoError(t, os.MkdirAll(view, 0o700))
	// The record names the source digest the ownership checks read.
	record := `{"kind":"gpustack-model-transfer-metadata-view/v1","artifactDigest":"sha256:` +
		strings.Repeat("a", 64) + `","incomplete":true,"files":[]}`
	require.NoError(t, os.WriteFile(filepath.Join(view, "view.json"), []byte(record), 0o444))
	stale := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(view, stale, stale))

	// The watermarks never force a collection; the view goes anyway, the
	// way residue of finished work does.
	collector.Watermarks = func() (int32, int32, string) { return 80, 70, "" }
	collector.Usage = func() (gc.Usage, error) { return gc.Usage{Total: 1000, Used: 10}, nil }

	_, err = collector.Collect(context.Background())
	require.NoError(t, err)
	assert.NoDirExists(t, view,
		"the collector the production path builds reclaims the terminal view")
	assert.FileExists(t, filepath.Join(viewsRoot, ".publish.lock"),
		"the lock file stays for the root's life")
}

// TestCollectorLifecyclePreservesMountedViewAndHonorsUnmountGrace exercises
// the shared view contract through Manager.newCollector and two Store handles:
// an acquired view survives corrupt EnsureView replacement and GC; unmount
// updates DigestRecord.LastUsedTime so the view stays through grace and is
// reclaimed once grace expires.
func TestCollectorLifecyclePreservesMountedViewAndHonorsUnmountGrace(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)
	csiStore, err := store.Open(root)
	require.NoError(t, err)

	hex := strings.Repeat("b", 64)
	digest := "sha256:" + hex
	fixture := filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	attempt, err := st.NewAttempt(hex)
	require.NoError(t, err)
	checkpoint, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	manifest, err := os.ReadFile(filepath.Join(fixture, "manifest.json"))
	require.NoError(t, err)
	tree := filepath.Join(attempt.Dir(), "tree")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"), checkpoint, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "manifest.json"), manifest, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "config.json"), []byte(`{"architectures":["TinyForCausalLM"]}`), 0o644))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: digest, SizeBytes: int64(len(checkpoint)) + 64,
		ManifestFormat: "model-transfer/test", Source: store.SourceHub,
	}))

	now := time.Now()
	m := &Manager{Store: st, NodeName: "node-1"}
	materializer := &materialize.Materializer{Store: st, Now: func() time.Time { return now }, Base: context.Background()}
	collector := m.newCollector(materializer)
	collector.Now = func() time.Time { return now }
	collector.Watermarks = func() (int32, int32, string) { return 80, 70, "" }
	collector.Usage = func() (gc.Usage, error) { return gc.Usage{Total: 1000, Used: 10}, nil }

	// CSI acquires before resolving, builds the view, and ages the directory
	// past grace to simulate a long-running pod.
	require.NoError(t, csiStore.Acquire(context.Background(), store.Ref{
		VolumeID: "csi-vol-1", TargetPath: "/k/csi-vol-1", Hex: hex,
	}))
	viewDir, err := metadata.EnsureView(csiStore, csiStore.ViewsRoot(), hex, digest, nil, "csi-vol-1")
	require.NoError(t, err)
	stale := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(viewDir, stale, stale))
	beforeInfo, err := os.Stat(viewDir)
	require.NoError(t, err)
	beforeIno := beforeInfo.Sys().(*syscall.Stat_t).Ino

	// A concurrent caller hitting a verify failure while csi-vol-1 is active
	// must fail closed without replacing the mounted inode, and GC must keep it.
	require.NoError(t, os.Chmod(filepath.Join(viewDir, "config.json"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(viewDir, "config.json"), []byte("corrupt"), 0o644))
	require.NoError(t, csiStore.Acquire(context.Background(), store.Ref{VolumeID: "csi-vol-2", Hex: hex}))
	_, err = metadata.EnsureView(csiStore, csiStore.ViewsRoot(), hex, digest, nil, "csi-vol-2")
	require.Error(t, err)
	require.NoError(t, csiStore.RemoveRef("csi-vol-2"))

	_, err = collector.Collect(context.Background())
	require.NoError(t, err)
	afterInfo, err := os.Stat(viewDir)
	require.NoError(t, err)
	assert.Equal(t, beforeIno, afterInfo.Sys().(*syscall.Stat_t).Ino,
		"neither a failingEnsureView nor GC may replace a referenced view directory")

	// Unmount removes the reference and records LastUsedTime, the way Reporter.Unmounted does.
	require.NoError(t, csiStore.RemoveRef("csi-vol-1"))
	unmountTime := now.Add(-time.Minute)
	require.NoError(t, st.UpdateDigest(hex, func(rec *store.DigestRecord) {
		rec.LastUsedTime = unmountTime
	}))

	// Within grace after unmount, Collect keeps the view even though its directory mtime is an hour old.
	_, err = collector.Collect(context.Background())
	require.NoError(t, err)
	assert.DirExists(t, viewDir, "the unmounted view stays through its grace period")

	// Past grace, Collect reclaims the terminal view.
	now = unmountTime.Add(gc.DefaultGrace + time.Minute)
	_, err = collector.Collect(context.Background())
	require.NoError(t, err)
	assert.NoDirExists(t, viewDir, "the terminal view is reclaimed once grace expires")
}
