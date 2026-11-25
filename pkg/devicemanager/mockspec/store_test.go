package mockspec

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestStoreInitialLoadErrors(t *testing.T) {
	dir := t.TempDir()

	_, err := NewStore("")
	require.Error(t, err)

	_, err = NewStore(dir)
	require.Error(t, err)

	_, err = NewStore(filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)

	badPath := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(badPath, []byte("preset: unknown-preset\n"), 0o600))
	_, err = NewStore(badPath)
	require.Error(t, err)

	_, err = LoadFile(badPath)
	require.Error(t, err)

	validPath := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(validPath, []byte("preset: nvidia-gb10\n"), 0o600))
	doc, err := LoadFile(validPath)
	require.NoError(t, err)
	require.Len(t, doc.ResolveNode("n").Manufacturers[nodefeature.ManufacturerNVIDIA].Slots, 1)
}

func TestStoreHotReloadAndRetention(t *testing.T) {
	dir := t.TempDir()

	// Emulate Kubernetes ConfigMap volume layout:
	//   <dir>/devices.yaml -> ..data/devices.yaml
	//   <dir>/..data       -> ..v1
	v1Dir := filepath.Join(dir, "..v1")
	require.NoError(t, os.MkdirAll(v1Dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(v1Dir, "devices.yaml"),
		[]byte("preset: nvidia-h100\ncount: 2\n"),
		0o600,
	))
	require.NoError(t, os.Symlink("..v1", filepath.Join(dir, "..data")))
	specPath := filepath.Join(dir, "devices.yaml")
	require.NoError(t, os.Symlink(filepath.Join("..data", "devices.yaml"), specPath))

	store, err := NewStore(specPath, WithLogger(klog.Background()))
	require.NoError(t, err)
	assert.Equal(t, specPath, store.Path())

	snap1 := store.Current()
	require.NotNil(t, snap1)
	assert.Equal(t, uint64(1), snap1.Revision)
	node1 := snap1.Document.ResolveNode("worker-a")
	require.Len(t, node1.Manufacturers[nodefeature.ManufacturerNVIDIA].Slots, 2)

	// Unchanged content does not bump Revision.
	snapUnchanged, err := store.Refresh()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), snapUnchanged.Revision)

	// Atomic ConfigMap symlink swap to ..v2 with single-card unqueryable fault.
	v2Dir := filepath.Join(dir, "..v2")
	require.NoError(t, os.MkdirAll(v2Dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(v2Dir, "devices.yaml"),
		[]byte("preset: nvidia-h100\ncount: 2\ncardOverrides:\n  1:\n    queryState: unqueryable\n"),
		0o600,
	))
	tmpLink := filepath.Join(dir, "..data_tmp")
	require.NoError(t, os.Symlink("..v2", tmpLink))
	require.NoError(t, os.Rename(tmpLink, filepath.Join(dir, "..data")))

	node2, err := store.ResolveNode("worker-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), store.Current().Revision)
	assert.Equal(t, QueryStateUnqueryable, node2.Manufacturers[nodefeature.ManufacturerNVIDIA].Slots[1].QueryState)

	// Malformed reload via ..v3 retains Revision 2 and returns an error.
	v3Dir := filepath.Join(dir, "..v3")
	require.NoError(t, os.MkdirAll(v3Dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(v3Dir, "devices.yaml"),
		[]byte("preset: nvidia-h100\ncardOverrides:\n  1:\n    queryState: invalid_state\n"),
		0o600,
	))
	require.NoError(t, os.Symlink("..v3", tmpLink))
	require.NoError(t, os.Rename(tmpLink, filepath.Join(dir, "..data")))

	retainedNode, err := store.ResolveNode("worker-a")
	require.Error(t, err)
	require.Error(t, store.LastError())
	assert.Equal(t, uint64(2), store.Current().Revision)
	assert.Equal(t, QueryStateUnqueryable, retainedNode.Manufacturers[nodefeature.ManufacturerNVIDIA].Slots[1].QueryState)

	// Valid recovery via ..v4 clears LastError and bumps Revision to 3.
	v4Dir := filepath.Join(dir, "..v4")
	require.NoError(t, os.MkdirAll(v4Dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(v4Dir, "devices.yaml"),
		[]byte("preset: nvidia-h100\ncount: 1\n"),
		0o600,
	))
	require.NoError(t, os.Symlink("..v4", tmpLink))
	require.NoError(t, os.Rename(tmpLink, filepath.Join(dir, "..data")))

	recoveredNode, err := store.ResolveNode("worker-a")
	require.NoError(t, err)
	assert.NoError(t, store.LastError())
	assert.Equal(t, uint64(3), store.Current().Revision)
	require.Len(t, recoveredNode.Manufacturers[nodefeature.ManufacturerNVIDIA].Slots, 1)
}

func TestStoreConcurrentAccessAndIsolation(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "devices.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte("preset: amd-mi308x\ncount: 4\n"), 0o600))

	store, err := NewStore(specPath)
	require.NoError(t, err)

	// Mutating a returned ResolvedNodeSpec must not mutate Store state.
	mutated, err := store.ResolveNode("node-0")
	require.NoError(t, err)
	mutated.Manufacturers[nodefeature.ManufacturerAMD].Slots[0].Accelerator.ID = "corrupted"

	clean, err := store.ResolveNode("node-0")
	require.NoError(t, err)
	assert.NotEqual(t, "corrupted", clean.Manufacturers[nodefeature.ManufacturerAMD].Slots[0].Accelerator.ID)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 20 {
				if id == 0 && i%5 == 0 {
					payload := "preset: amd-mi308x\ncount: 2\n"
					if i%10 == 0 {
						payload = "preset: amd-mi308x\ncount: 4\n"
					}
					tmpPath := filepath.Join(dir, "devices.yaml.tmp")
					_ = os.WriteFile(tmpPath, []byte(payload), 0o600)
					_ = os.Rename(tmpPath, specPath)
				}
				res, rerr := store.ResolveNode("node-0")
				assert.NoError(t, rerr)
				assert.NotEmpty(t, res.Manufacturers[nodefeature.ManufacturerAMD].Slots)
			}
		}(g)
	}
	wg.Wait()
}
