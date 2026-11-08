package gc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// metadataViewRoot is where this node's metadata views live, beside the
// published layout, under the same root the collector sweeps.
func metadataViewRoot(c *testCache) string {
	return c.store.ViewsRoot()
}

// stageMetadataView writes a terminal view the way a builder promotes it:
// a writable directory named by the record's content address, holding the
// sealed record. The record derives the view from the digest hex the
// caller names, so the directory's name differs from the source digest
// the way a real view's does. Returns the view's directory name.
func stageMetadataView(t *testing.T, root, digestHex string, files ...string) string {
	t.Helper()
	if len(files) == 0 {
		files = []string{"config.json"}
	}
	recordFiles := make([]metadata.ViewFile, 0, len(files))
	for i, name := range files {
		recordFiles = append(recordFiles, metadata.ViewFile{
			Name: name, Size: int64(i + 1), SHA256: fmt.Sprintf("%064d", i),
		})
	}
	raw, err := json.Marshal(metadata.View{
		Kind:           metadata.ViewKind,
		ArtifactDigest: "sha256:" + digestHex,
		Incomplete:     true,
		Files:          recordFiles,
	})
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	name := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "view.json"), raw, 0o444))

	return name
}

// ageView stamps a view directory to a moment the collector's fixed Now
// measures, the way a real view carries its promotion time.
func ageView(t *testing.T, dir string, at time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(dir, at, at))
}

// TestCollectReclaimsTerminalMetadataViews runs against a real view root:
// a final view whose last reference went, however old its record, leaves
// at the next collection; everything with a reason to stay stays.
func TestCollectReclaimsTerminalMetadataViews(t *testing.T) {
	sourceHex := hexOf('a')
	cases := []struct {
		name        string
		trees       map[string]time.Time
		viewRefs    []string // live references naming the view's source digest
		pinned      map[string]bool
		recent      bool // the view was used within the grace
		refsUnread  bool // the references cannot be read at all
		unknown     bool // also stage a 64-hex directory with no readable record
		wantGone    bool
		wantUnknown bool
	}{
		{
			name:     "an unreferenced final view goes, however old",
			wantGone: true,
		},
		{
			name:     "the source of a built view keeps its view while mounted",
			trees:    map[string]time.Time{sourceHex: testNow.Add(-4 * time.Hour)},
			viewRefs: []string{"csi-aaaaaaaa"},
			wantGone: false,
		},
		{
			name:     "a view whose source is pinned is kept even unreferenced",
			trees:    map[string]time.Time{sourceHex: testNow.Add(-4 * time.Hour)},
			pinned:   map[string]bool{sourceHex: true},
			wantGone: false,
		},
		{
			name:     "a view referenced by a live mount is kept",
			viewRefs: []string{"csi-mount-1"},
			wantGone: false,
		},
		{
			name:     "a view used within the grace is kept",
			recent:   true,
			wantGone: false,
		},
		{
			name:       "a view whose references cannot be read is kept",
			refsUnread: true,
			wantGone:   false,
		},
		{
			name:        "a directory with no readable record is unknown and kept",
			unknown:     true,
			wantGone:    true,
			wantUnknown: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t, 1)
			for hex, lastUsed := range tc.trees {
				c.publish(t, hex, 10, lastUsed)
			}
			root := metadataViewRoot(c)
			require.NoError(t, os.MkdirAll(root, 0o755))
			// A root that served a build keeps its publish lock for its life.
			lock, err := os.OpenFile(filepath.Join(root, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			require.NoError(t, err)
			require.NoError(t, lock.Close())
			viewName := stageMetadataView(t, root, sourceHex)
			ageView(t, filepath.Join(root, viewName), testNow.Add(-time.Hour))
			unknownName := ""
			if tc.unknown {
				// A candidate-shaped directory whose record cannot be read:
				// ownership the collector cannot prove gone.
				unknownName = "beef" + sourceHex[4:]
				require.NoError(t, os.MkdirAll(filepath.Join(root, unknownName), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, unknownName, "view.json"), []byte("{"), 0o444))
				ageView(t, filepath.Join(root, unknownName), testNow.Add(-time.Hour))
			}

			for _, volumeID := range tc.viewRefs {
				ref := store.Ref{VolumeID: volumeID, TargetPath: "/k/" + volumeID, Hex: sourceHex}
				require.NoError(t, c.store.WriteRef(ref))
			}
			if tc.recent {
				require.NoError(t, c.store.UpdateDigest(sourceHex, func(rec *store.DigestRecord) {
					rec.LastUsedTime = testNow.Add(-time.Minute)
				}))
			}

			referenced := func() (map[string]bool, error) {
				if tc.refsUnread {
					return nil, os.ErrPermission
				}
				refs, err := c.store.Refs()
				if err != nil {
					return nil, err
				}
				m := map[string]bool{}
				for _, r := range refs {
					m[r.Hex] = true
				}
				return m, nil
			}
			col := c.collector(nil, nil, tc.pinned)
			col.Referenced = referenced
			col.ViewRoot = root

			_, err = col.Collect(t.Context())
			require.NoError(t, err)

			if tc.wantGone {
				assert.NoDirExists(t, filepath.Join(root, viewName),
					"the terminal view %s should be reclaimed", viewName)
			} else {
				assert.DirExists(t, filepath.Join(root, viewName),
					"the view %s should be kept", viewName)
			}
			if tc.wantUnknown {
				assert.DirExists(t, filepath.Join(root, unknownName),
					"the unknown directory %s should be kept", unknownName)
			}
			assert.FileExists(t, filepath.Join(root, ".publish.lock"),
				"the lock file stays for the root's life")
		})
	}
}

// TestCollectBoundedMetadataViewResidue turns views over repeatedly:
// build, mount, unmount, again. Every terminal view goes at the next
// collection, so the residue after a full turn is bounded, not growing.
func TestCollectBoundedMetadataViewResidue(t *testing.T) {
	c := newTestCache(t, 1)
	sourceHex := hexOf('a')
	c.publish(t, sourceHex, 10, testNow.Add(-4*time.Hour))
	root := metadataViewRoot(c)
	require.NoError(t, os.MkdirAll(root, 0o755))
	lock, err := os.OpenFile(filepath.Join(root, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, lock.Close())

	col := c.collector(nil, nil)
	col.ViewRoot = root

	const turns = 3
	released := map[string]bool{}
	for i := 0; i < turns; i++ {
		// Each turn builds a distinct view of the same source digest, the
		// way an artifact update turns views over.
		viewName := stageMetadataView(t, root, sourceHex, fmt.Sprintf("t%d.json", i))
		ageView(t, filepath.Join(root, viewName), testNow.Add(-time.Hour))

		volumeID := "csi-turn-" + fmt.Sprintf("%04d", i)
		published, err := c.store.Claim(t.Context(), store.Ref{
			VolumeID: volumeID, TargetPath: "/k/" + volumeID, Hex: sourceHex,
		})
		require.NoError(t, err)
		require.True(t, published, "the source tree of a mounted view stays claimable")
		// An unmount releases the reference the way NodeUnpublishVolume does.
		require.NoError(t, c.store.RemoveRef(volumeID))

		_, err = col.Collect(t.Context())
		require.NoError(t, err)
		assert.NoDirExists(t, filepath.Join(root, viewName),
			"turn %d: the released view is reclaimed", i)
		released[viewName] = true
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var residue []string
	for _, e := range entries {
		if e.Name() != ".publish.lock" {
			residue = append(residue, e.Name())
		}
	}
	assert.Empty(t, residue, "repeated turnover leaves only the lock file behind")
	assert.Len(t, released, turns, "every turn retired its own distinct view")
}

// TestCollectIsolatesPerViewErrorsFromTreeGC verifies that a per-view
// record fault skips only that view while still reclaiming other terminal
// views and eligible published trees.
func TestCollectIsolatesPerViewErrorsFromTreeGC(t *testing.T) {
	c := newTestCache(t, 760)
	brokenHex := hexOf('a')
	goodHex := hexOf('b')
	treeHex := hexOf('c')
	c.publish(t, treeHex, 90, testNow.Add(-4*time.Hour))

	root := metadataViewRoot(c)
	require.NoError(t, os.MkdirAll(root, 0o755))
	brokenView := stageMetadataView(t, root, brokenHex)
	goodView := stageMetadataView(t, root, goodHex)
	ageView(t, filepath.Join(root, brokenView), testNow.Add(-time.Hour))
	ageView(t, filepath.Join(root, goodView), testNow.Add(-time.Hour))

	// Corrupt brokenHex's digest record when reclaimViews reads Pinned (after
	// pruneRecords has already scanned ledger/digests) so ReadDigest(brokenHex)
	// fails inside reclaimViews while tree candidates remain unaffected.
	digestFile := filepath.Join(c.store.Root(), "ledger", "digests", brokenHex+".json")
	col := c.collector(nil, nil)
	col.Pinned = func() map[string]bool {
		require.NoError(t, os.MkdirAll(filepath.Dir(digestFile), 0o755))
		require.NoError(t, os.WriteFile(digestFile, []byte("{corrupt"), 0o600))
		return nil
	}
	col.ViewRoot = root
	res, err := col.Collect(t.Context())
	require.NoError(t, err)
	assert.DirExists(t, filepath.Join(root, brokenView),
		"a view whose digest record is unreadable is skipped and preserved")
	assert.NoDirExists(t, filepath.Join(root, goodView),
		"other terminal views are still reclaimed in the same pass")
	assert.False(t, c.store.IsPublished(treeHex),
		"published tree eviction continues past a per-view error")
	assert.Equal(t, int64(90), res.RemovedBytes)
}
