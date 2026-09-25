package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDevice  = "253:1"
	testKubelet = "/var/lib/kubelet"
)

func testTarget(podUID, volume string) string {
	return testKubelet + "/pods/" + podUID + "/volumes/kubernetes.io~csi/" + volume + "/mount"
}

// publishTestTree publishes a one-file tree for hex.
func publishTestTree(t *testing.T, s *Store, hex string) {
	t.Helper()
	a, err := s.NewAttempt(hex)
	require.NoError(t, err)
	writeAttemptFile(t, a, "config.json", "{}")
	require.NoError(t, a.Publish(Marker{Digest: "sha256:" + hex}))
}

func TestRefs(t *testing.T) {
	r := Ref{
		VolumeID: "csi-1", TargetPath: testTarget("uid-1", "gpustack-model"), Hex: testHex,
		PodNamespace: "team-a", PodName: "p1", PodUID: "uid-1",
	}
	cases := []struct {
		name     string
		write    []Ref
		remove   []string
		wantRefs []Ref
		wantErr  bool
	}{
		{name: "a written reference is listed", write: []Ref{r}, wantRefs: []Ref{r}},
		{name: "a removed reference is not listed", write: []Ref{r}, remove: []string{"csi-1"}, wantRefs: []Ref{}},
		{name: "removing a missing reference is not an error", remove: []string{"csi-1", "csi-1"}, wantRefs: []Ref{}},
		{name: "a volume ID is a file name, never a path", write: []Ref{{VolumeID: "../escape"}}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openTestStore(t)
			for _, w := range c.write {
				err := s.WriteRef(w)
				if c.wantErr {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
			}
			for _, id := range c.remove {
				require.NoError(t, s.RemoveRef(id))
			}
			refs, err := s.Refs()
			require.NoError(t, err)
			assert.Equal(t, c.wantRefs, refs)
		})
	}
}

func TestClaimAndRemoveUnreferenced(t *testing.T) {
	r := Ref{VolumeID: "csi-1", TargetPath: testTarget("uid-1", "gpustack-model"), Hex: testHex}
	cases := []struct {
		name          string
		published     bool
		claim         bool
		unreadableRef bool
		wantClaimed   bool
		wantRemoved   bool
		wantErr       bool
	}{
		{name: "a claim of an unpublished tree records nothing", claim: true},
		{name: "a claimed tree is not removed", published: true, claim: true, wantClaimed: true},
		{name: "an unclaimed tree is removed", published: true, wantRemoved: true},
		{name: "a reference that cannot be read keeps the tree", published: true, unreadableRef: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openTestStore(t)
			if c.published {
				publishTestTree(t, s, testHex)
			}
			if c.claim {
				claimed, err := s.Claim(r)
				require.NoError(t, err)
				assert.Equal(t, c.wantClaimed, claimed)
				_, found, err := s.ReadRef("csi-1")
				require.NoError(t, err)
				assert.Equal(t, c.wantClaimed, found, "a reference is written only for a published tree")
			}
			if c.unreadableRef {
				require.NoError(t, os.WriteFile(filepath.Join(s.Root(), ledgerDir, refsDir, "csi-9.json"), []byte("{"), workingFileMode))
			}

			removed, err := s.RemoveUnreferenced(testHex)
			if c.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, c.wantRemoved, removed)
			assert.Equal(t, c.published && !c.wantRemoved, s.IsPublished(testHex))
			trash, err := os.ReadDir(filepath.Join(s.Root(), trashDir))
			require.NoError(t, err)
			assert.Empty(t, trash, "a removed tree is deleted, not left in trash")
		})
	}
}

func TestUpdateDigestMergesConcurrentUpdates(t *testing.T) {
	s := openTestStore(t)
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() { errs <- s.UpdateDigest(testHex, func(rec *DigestRecord) { rec.Failures++ }) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	rec, err := s.ReadDigest(testHex)
	require.NoError(t, err)
	assert.Equal(t, writers, rec.Failures, "no update is lost to another")
}

func TestDigestsSkipsAStrayFile(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.WriteDigest(testHex, DigestRecord{LastAttempt: 1}))
	require.NoError(t, os.WriteFile(filepath.Join(s.Root(), ledgerDir, digestsDir, "notes.json"), []byte("{}"), workingFileMode))

	recs, err := s.Digests()
	require.NoError(t, err, "a file that is not a record does not fail the listing")
	assert.Equal(t, map[string]DigestRecord{testHex: {LastAttempt: 1}}, recs)
}

func TestDigestRecords(t *testing.T) {
	want := DigestRecord{
		LastAttempt: 3, LastUsedTime: time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC), Failures: 2,
		Reason: "IntegrityMismatch", Message: "config.json: hash mismatch",
		RetryTime: time.Date(2026, 9, 25, 6, 2, 0, 0, time.UTC),
	}
	cases := []struct {
		name    string
		write   bool
		remove  bool
		hex     string
		want    DigestRecord
		wantAll map[string]DigestRecord
		wantErr bool
	}{
		{name: "a digest without a record has the zero record", hex: testHex, wantAll: map[string]DigestRecord{}},
		{name: "a written record reads back and is listed", write: true, hex: testHex, want: want, wantAll: map[string]DigestRecord{testHex: want}},
		{name: "a removed record is not listed", write: true, remove: true, hex: testHex, wantAll: map[string]DigestRecord{}},
		{name: "a digest is a file name, never a path", hex: "../" + testHex[:61], wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openTestStore(t)
			if c.write {
				require.NoError(t, s.WriteDigest(testHex, want))
			}
			if c.remove {
				require.NoError(t, s.RemoveDigest(testHex))
			}
			got, err := s.ReadDigest(c.hex)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			all, err := s.Digests()
			require.NoError(t, err)
			assert.Equal(t, c.wantAll, all)
		})
	}
}

func TestParseMountInfo(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []Mount
		wantErr bool
	}{
		{
			name: "a bind mount of a published tree",
			in: "36 35 253:1 /var/lib/gpustack/models/published/" + testHex + "/tree " +
				testTarget("uid-1", "gpustack-model") + " ro,relatime shared:1 - ext4 /dev/vda1 rw\n",
			want: []Mount{{
				Device: testDevice, Root: "/var/lib/gpustack/models/published/" + testHex + "/tree",
				MountPoint: testTarget("uid-1", "gpustack-model"),
			}},
		},
		{
			name: "octal escapes are decoded",
			in:   `40 35 0:44 /with\040space /mnt/with\040space rw - tmpfs tmpfs rw` + "\n",
			want: []Mount{{Device: "0:44", Root: "/with space", MountPoint: "/mnt/with space"}},
		},
		{name: "a short line is refused", in: "short line\n", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mounts, err := ParseMountInfo(strings.NewReader(c.in))
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, mounts)
		})
	}
}

func TestRebuild(t *testing.T) {
	s := openTestStore(t)
	kept := Ref{VolumeID: "csi-kept", TargetPath: testTarget("uid-1", "gpustack-model"), Hex: testHex, PodName: "p1"}
	corrected := Ref{VolumeID: "csi-corrected", TargetPath: testTarget("uid-2", "gpustack-model"), Hex: testHex, PodName: "p2"}
	dropped := Ref{VolumeID: "csi-dropped", TargetPath: testTarget("uid-3", "gpustack-model"), Hex: testHex, PodName: "p3"}
	for _, r := range []Ref{kept, corrected, dropped} {
		require.NoError(t, s.WriteRef(r))
	}

	published := func(hex string) string { return "/var/lib/gpustack/models/published/" + hex + "/tree" }
	mounts := []Mount{
		{Device: testDevice, Root: published(testHex), MountPoint: kept.TargetPath},
		{Device: testDevice, Root: published(otherHex), MountPoint: corrected.TargetPath},
		// Mounted, and the ledger lost it: adopted.
		{Device: testDevice, Root: published(testHex), MountPoint: testTarget("uid-4", "weights")},
		// The same shape on another filesystem is not the cache's.
		{Device: "8:1", Root: published(testHex), MountPoint: testTarget("uid-5", "weights")},
		// The cache's filesystem mounted somewhere that is not a CSI target.
		{Device: testDevice, Root: published(testHex), MountPoint: "/mnt/elsewhere"},
		// A partial is never a mount source the rebuild accepts.
		{Device: testDevice, Root: "/var/lib/gpustack/models/partial/" + testHex + "/1/tree", MountPoint: testTarget("uid-6", "weights")},
	}

	got, err := s.Rebuild(mounts, testDevice)
	require.NoError(t, err)
	assert.Equal(t, []Ref{kept}, got.Kept)
	corrected.Hex = otherHex
	assert.Equal(t, []Ref{corrected}, got.Corrected)
	assert.Equal(t, []Ref{dropped}, got.Dropped)
	adopted := Ref{
		VolumeID: EphemeralVolumeID("uid-4", "weights"), TargetPath: testTarget("uid-4", "weights"),
		Hex: testHex, PodUID: "uid-4",
	}
	assert.Equal(t, []Ref{adopted}, got.Adopted)

	ledger, err := s.Refs()
	require.NoError(t, err)
	assert.Equal(t, got.References(), ledger, "the ledger is rewritten to what the rebuild keeps")
}

func TestEphemeralVolumeID(t *testing.T) {
	// kubelet's formula, computed independently: "csi-" + sha256("uid" + "vol").
	assert.Equal(t, "csi-59a84980f8ef4826da54c7ced3605cf80e85f6bb08948be904a09383f8317b93", EphemeralVolumeID("uid", "vol"))
}
