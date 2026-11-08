package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// aTree publishes one ancillary and one sound safetensors file, the base
// every refusal test bends one way.
func aTree(t *testing.T, st *store.Store) published {
	t.Helper()

	return publishTree(t, st, map[string][]byte{
		"config.json": []byte(`{}`),
		"model.safetensors": buildSafetensors(t, []stTensor{{
			name: "w", dtype: "F32", shape: []int64{2}, data: []byte{
				0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0x40,
			},
		}}),
	})
}

// shardWith is a safetensors file built from a raw header string, for cases
// no well-formed builder may produce.
func shardWith(header string, data ...byte) []byte {
	shard := leBytes(int64(len(header)))
	shard = append(shard, header...)

	return append(shard, data...)
}

// Content verification belongs to cache publication; transfer reads layout only.
func TestBuildUsesPublishedPayloadWithoutRehashing(t *testing.T) {
	st := openStore(t)
	p := aTree(t, st)

	shard := filepath.Join(st.PublishedTree(p.hex), "model.safetensors")
	unseal(t, st.PublishedTree(p.hex))
	b, err := os.ReadFile(shard)
	require.NoError(t, err)
	b[len(b)-1] ^= 0xff
	require.NoError(t, os.WriteFile(shard, b, 0o644))

	_, err = build(t, st, p)
	require.NoError(t, err)
}

// Published content digests identify the cache; transfer checks file extents.
func TestBuildUsesPublishedListingAlgorithms(t *testing.T) {
	for _, digest := range []string{sha256digest(nil), gitshadigest(nil)} {
		t.Run(digest, func(t *testing.T) {
			st := openStore(t)
			p := publishTreeWith(t, st, map[string][]byte{
				"config.json": []byte(`{}`), "empty.json": {},
				"model.safetensors": shardWith(`{"w":{"dtype":"U8","shape":[1],"data_offsets":[0,1]}}`, 7),
			}, func(_ string, _ []byte) string { return digest })
			view, err := build(t, st, p)
			require.NoError(t, err)
			require.Len(t, view.Manifest.Files, 3)
			require.Len(t, view.Manifest.Tensors, 1)
		})
	}
}

// TestBuildRefusesAListingWhoseSizesOverflow pins the checked total over
// every listed size: a listing whose sizes wrap int64 is refused before the
// canonical encoding is trusted or any byte is read.
func TestBuildRefusesAListingWhoseSizesOverflow(t *testing.T) {
	st := openStore(t)
	p := aTree(t, st)
	huge := strconv.FormatInt(math.MaxInt64, 10)
	zero := "sha256:" + strings.Repeat("0", 64)
	listing := `{"digest":"` + p.digest + `","entries":[` +
		`{"path":"a","size":` + huge + `,"digest":"` + zero + `"},` +
		`{"path":"b","size":` + huge + `,"digest":"` + zero + `"}]}`
	require.NoError(t, os.WriteFile(st.PublishedManifestPath(p.hex), []byte(listing), 0o600))

	_, err := build(t, st, p)
	require.ErrorIs(t, err, ErrBoundsExceeded)
}

// TestBuildRefusesSymlinkEscapes pins that no listed path — final component
// or intermediate directory — may run through a symlink, whatever it points
// at.
func TestBuildRefusesSymlinkEscapes(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("the escape hatch"), 0o644))

	t.Run("the listed file itself is a symlink", func(t *testing.T) {
		st := openStore(t)
		p := publishTree(t, st, map[string][]byte{
			"config.json":       []byte(`{}`),
			"model.safetensors": nil,
		})
		tree := st.PublishedTree(p.hex)
		unseal(t, tree)
		require.NoError(t, os.Symlink(outside, filepath.Join(tree, "model.safetensors")))
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrFileMismatch)
		assert.Contains(t, err.Error(), "model.safetensors")
	})

	t.Run("an intermediate directory is a symlink", func(t *testing.T) {
		st := openStore(t)
		p := publishTree(t, st, map[string][]byte{
			"config.json": []byte(`{}`),
			"sub/weights.safetensors": buildSafetensors(t, []stTensor{
				{name: "w", dtype: "I8", shape: []int64{1}, data: []byte{7}},
			}),
		})
		tree := st.PublishedTree(p.hex)
		unseal(t, tree)
		require.NoError(t, os.RemoveAll(filepath.Join(tree, "sub")))
		require.NoError(t, os.Symlink(filepath.Dir(outside), filepath.Join(tree, "sub")))
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrFileMismatch)
	})
}

// TestBuildRefusesBrokenSafetensors pins the header refusals, each naming
// its own rule, with the length bound checked before the bytes are read.
func TestBuildRefusesBrokenSafetensors(t *testing.T) {
	tests := []struct {
		name  string
		shard []byte
	}{
		{name: "shorter than the length prefix", shard: []byte("safet")},
		{name: "header length under the minimum", shard: leBytes(1)},
		{name: "header length above the bound", shard: leBytes(metadata.MaxHeaderBytes + 1)},
		{
			name:  "truncated header",
			shard: shardWith(strings.Repeat("a", 100)),
		},
		{
			name:  "header is not JSON",
			shard: shardWith("nope"),
		},
		{
			name: "header entry with an unknown field",
			shard: shardWith(
				`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4],"extra":1}}`,
				1, 2, 3, 4,
			),
		},
		{
			name: "header with a duplicate key",
			shard: shardWith(
				`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4]},`+
					`"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`,
				1, 2, 3, 4,
			),
		},
		{
			name:  "header declaring no tensor",
			shard: shardWith(`{}`),
		},
		{
			name: "data end beyond the file",
			shard: shardWith(
				`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,999]}}`,
			),
		},
		{
			name: "empty dtype",
			shard: shardWith(
				`{"a":{"dtype":"","shape":[1],"data_offsets":[0,1]}}`, 0,
			),
		},
		{
			name: "non-empty tensor with empty span",
			shard: shardWith(
				`{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,0]}}`,
			),
		},
		{
			name: "negative shape",
			shard: shardWith(
				`{"a":{"dtype":"F32","shape":[-1],"data_offsets":[0,0]}}`,
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			p := publishTree(t, st, map[string][]byte{
				"config.json":       []byte(`{}`),
				"model.safetensors": tc.shard,
			})
			_, err := build(t, st, p)
			require.ErrorIs(t, err, ErrInvalidSafetensors)
		})
	}

	t.Run("an empty safetensors file", func(t *testing.T) {
		st := openStore(t)
		p := publishTree(t, st, map[string][]byte{
			"config.json":       []byte(`{}`),
			"model.safetensors": {},
		})
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrInvalidSafetensors)
	})
}

// TestBuildRefusesOverlappingAndDuplicateTensors pins the tensor-layout
// refusals: two tensors on one byte range, or one tensor name in two files.
func TestBuildRefusesOverlappingAndDuplicateTensors(t *testing.T) {
	t.Run("overlapping ranges in one file", func(t *testing.T) {
		st := openStore(t)
		p := publishTree(t, st, map[string][]byte{
			"config.json": []byte(`{}`),
			"model.safetensors": shardWith(
				`{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,8]},`+
					`"b":{"dtype":"F32","shape":[2],"data_offsets":[4,12]}}`,
				1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12,
			),
		})
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrInvalidSafetensors)
	})

	t.Run("one tensor name in two files", func(t *testing.T) {
		shard := buildSafetensors(t, []stTensor{
			{name: "same", dtype: "I8", shape: []int64{1}, data: []byte{9}},
		})
		st := openStore(t)
		p := publishTree(t, st, map[string][]byte{
			"a.safetensors": shard,
			"b.safetensors": shard,
		})
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrInvalidBundle)
	})
}

// TestBuildRefusesABundleWithoutTensors pins that a tree with no safetensors
// tensor is no transfer source, however well formed its files are.
func TestBuildRefusesABundleWithoutTensors(t *testing.T) {
	st := openStore(t)
	p := publishTree(t, st, map[string][]byte{
		"config.json": []byte(`{}`), "weights.bin": []byte("pickle"),
	})
	_, err := build(t, st, p)
	require.ErrorIs(t, err, ErrNoTensors)
}

// TestBuildBoundsTheListing pins the strict decode of the listing itself:
// oversized, over-counted, repeated-field, unknown-field, and trailing-token
// listings are refused before any file is read.
func TestBuildBoundsTheListing(t *testing.T) {
	st := openStore(t)
	p := aTree(t, st)
	overwrite := func(t *testing.T, listing string) {
		t.Helper()
		require.NoError(t, os.WriteFile(st.PublishedManifestPath(p.hex), []byte(listing), 0o600))
	}
	entry := func(i int) string {
		return `{"path":"f` + strconv.Itoa(i) + `.bin","size":1,"digest":"sha256:` +
			strings.Repeat("0", 64) + `"}`
	}

	tests := []struct {
		name    string
		listing string
	}{
		{name: "empty listing", listing: ``},
		{
			name: "listing above the byte bound",
			listing: `{"digest":"` + p.digest + `","entries":[],"padding":"` +
				strings.Repeat("a", int(MaxListingBytes)) + `"}`,
		},
		{
			name: "listing above the file-count bound",
			listing: `{"digest":"` + p.digest + `","entries":[` + entry(0) +
				strings.Repeat(","+entry(1), MaxListingEntries) + `]}`,
		},
		{name: "repeated top-level field", listing: `{"digest":"a","digest":"` + p.digest + `","entries":[]}`},
		{name: "unknown top-level field", listing: `{"digest":"` + p.digest + `","entries":[],"extra":1}`},
		{name: "trailing JSON token", listing: `{"digest":"` + p.digest + `","entries":[]} {"x":1}`},
		{name: "missing entries", listing: `{"digest":"` + p.digest + `"}`},
		{
			name:    "entry missing its digest",
			listing: `{"digest":"` + p.digest + `","entries":[{"path":"a","size":1}]}`,
		},
		{
			name: "entry with a fractional size",
			listing: `{"digest":"` + p.digest + `","entries":[{"path":"a","size":1.5,` +
				`"digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`,
		},
		{
			name: "entry with an unknown field",
			listing: `{"digest":"` + p.digest + `","entries":[{"path":"a","size":1,` +
				`"digest":"sha256:` + strings.Repeat("0", 64) + `","extra":2}]}`,
		},
		{name: "entries that are not an array", listing: `{"digest":"` + p.digest + `","entries":{}}`},
		{
			name: "a negative size",
			listing: `{"digest":"` + p.digest + `","entries":[{"path":"a","size":-1,` +
				`"digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			overwrite(t, tc.listing)
			_, err := build(t, st, p)
			require.ErrorIs(t, err, ErrListingInvalid)
		})
	}

	t.Run("a listing naming another digest", func(t *testing.T) {
		st := openStore(t)
		p := aTree(t, st)
		require.NoError(t, os.WriteFile(st.PublishedManifestPath(p.hex),
			[]byte(`{"digest":"sha256:`+strings.Repeat("3", 64)+`","entries":[]}`), 0o600))
		_, err := build(t, st, p)
		require.ErrorIs(t, err, ErrStaleDigest)
	})

	t.Run("a listing that re-encodes to another digest", func(t *testing.T) {
		st := openStore(t)
		p := aTree(t, st)
		raw, err := os.ReadFile(st.PublishedManifestPath(p.hex))
		require.NoError(t, err)
		var listing peer.Listing
		require.NoError(t, json.Unmarshal(raw, &listing))
		listing.Entries[1].Size++
		b, err := json.Marshal(listing)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(st.PublishedManifestPath(p.hex), b, 0o600))
		_, err = build(t, st, p)
		require.ErrorIs(t, err, ErrContentMismatch)
	})
}

// sparseBytes is the data section of the sparse-tensor fixture: far larger
// than any buffer, and never materialized on disk.
const sparseBytes = 1 << 24

// sparseHeader is the header of the sparse-tensor fixture.
func sparseHeader() string {
	return fmt.Sprintf(
		`{"big":{"dtype":"U8","shape":[%d],"data_offsets":[0,%d]}}`, sparseBytes, sparseBytes,
	)
}

// publishSparse seals a tree whose one tensor is a sixteen-mebibyte run of
// zeros held by a sparse file: one real byte at the data section's start,
// the rest a hole the filesystem fills with zeros.
func publishSparse(t *testing.T, st *store.Store) published {
	t.Helper()
	header := sparseHeader()
	prefix := shardWith(header, 0)
	config := []byte(`{}`)
	// The shard's listed digest is its whole file: the header prefix plus
	// the sparseBytes of zeros the filesystem holds for the hole.
	whole := sha256.New()
	whole.Write(prefix[:len(prefix)-1])
	whole.Write(make([]byte, sparseBytes))
	entries := []modelartifact.ManifestEntry{
		{Path: "config.json", Size: int64(len(config)), Digest: sha256digest(config)},
		{
			Path: "model.safetensors", Size: int64(len(prefix)) - 1 + sparseBytes,
			Digest: "sha256:" + hex.EncodeToString(whole.Sum(nil)),
		},
	}
	canonical, err := modelartifact.NewManifest(entries)
	require.NoError(t, err)
	hexDigest := store.HexOf(canonical.Digest)
	attempt, err := st.NewAttempt(hexDigest)
	require.NoError(t, err)
	configPath, err := attempt.FilePath("config.json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, config, 0o644))
	shardPath, err := attempt.FilePath("model.safetensors")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(shardPath, prefix, 0o644))
	f, err := os.OpenFile(shardPath, os.O_WRONLY, 0o644)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(int64(len(prefix))-1+sparseBytes))
	require.NoError(t, f.Close())

	listing := peer.Listing{Digest: canonical.Digest}
	for _, e := range canonical.Entries {
		listing.Entries = append(listing.Entries, peer.ManifestFile{
			Path: e.Path, Size: e.Size, Digest: e.Digest,
		})
	}
	b, err := json.Marshal(listing)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"), b, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: canonical.Digest, ManifestFormat: modelartifact.ManifestHeader,
		SizeBytes: canonical.SizeBytes, FileCount: canonical.FileCount,
		PublishedTime: time.Now(), Source: store.SourceHub,
	}))

	return published{hex: hexDigest, digest: canonical.Digest}
}

// TestBuildStreamsALargeSparseTensor checks layout for a large published shard.
func TestBuildStreamsALargeSparseTensor(t *testing.T) {
	st := openStore(t)
	p := publishSparse(t, st)

	view, err := build(t, st, p)
	require.NoError(t, err)
	require.Len(t, view.Manifest.Tensors, 1)
	tensor := view.Manifest.Tensors[0]
	dataStart := int64(8 + len(sparseHeader()))
	assert.EqualValues(t, sparseBytes, tensor.Shape[0])
	assert.EqualValues(t, dataStart, tensor.DataBegin)
	assert.EqualValues(t, dataStart+sparseBytes, tensor.DataEnd)
	assert.Equal(t, 1, view.Registrations)
	assert.EqualValues(t, dataStart+sparseBytes, view.RegisteredBytes)
}

// TestAddChecked pins the checked arithmetic behind every total: an int64
// overflow is refused, not wrapped.
func TestAddChecked(t *testing.T) {
	sum, err := addChecked(1, math.MaxInt64-1)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), sum)

	_, err = addChecked(math.MaxInt64-1, 2)
	require.ErrorIs(t, err, ErrBoundsExceeded)

	_, err = addChecked(math.MaxInt64, math.MaxInt64)
	require.ErrorIs(t, err, ErrBoundsExceeded)

	sum, err = addChecked(0, -5)
	require.NoError(t, err)
	assert.Equal(t, int64(-5), sum)
}

// TestBuildHonorsContextCancellation pins that a canceled Build stops
// reading before any IO.
func TestBuildHonorsContextCancellation(t *testing.T) {
	st := openStore(t)
	p := aTree(t, st)

	t.Run("canceled before the call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Build(ctx, st, testUID, 1, p.digest)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestBuildRefusesBrokenPublishedHeader(t *testing.T) {
	tests := []struct {
		name  string
		shard []byte
	}{
		{name: "short prefix", shard: []byte("short")},
		{name: "excessive header", shard: leBytes(metadata.MaxHeaderBytes + 1)},
		{name: "invalid JSON", shard: shardWith("nope")},
		{name: "invalid range", shard: shardWith(`{"w":{"dtype":"U8","shape":[2],"data_offsets":[0,2]}}`, 1)},
	}
	for _, tc := range tests {
		for _, algorithm := range []string{modelartifact.DigestSHA256, modelartifact.DigestGitSHA1} {
			t.Run(tc.name+"/"+algorithm, func(t *testing.T) {
				st := openStore(t)
				p := publishTreeWith(t, st, map[string][]byte{"model.safetensors": tc.shard},
					func(_ string, _ []byte) string {
						if algorithm == modelartifact.DigestGitSHA1 {
							return gitshadigest([]byte("other content"))
						}
						return sha256digest([]byte("other content"))
					})
				_, err := build(t, st, p)
				require.ErrorIs(t, err, ErrInvalidSafetensors)
			})
		}
	}
}

func TestVerifyFilesBoundsConcurrencyAndPreservesOrder(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	entries := make([]modelartifact.ManifestEntry, 7)
	for i := range entries {
		entries[i] = modelartifact.ManifestEntry{Path: fmt.Sprintf("%d.safetensors", i), Size: 1}
	}
	started := make(chan struct{}, len(entries))
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var active atomic.Int32
	type outcome struct {
		files *verifiedFiles
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		files, err := verifyFiles(context.Background(), entries,
			func(_ context.Context, entry modelartifact.ManifestEntry, _ int64) (*verifiedFile, error) {
				count := active.Add(1)
				defer active.Add(-1)
				if count > 2 {
					return nil, errors.New("worker count exceeds its bound")
				}
				started <- struct{}{}
				<-release
				return &verifiedFile{path: entry.Path, size: 1, tensors: []protocol.TensorDescriptor{{Name: entry.Path}}}, nil
			})
		done <- outcome{files, err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("files did not begin concurrently")
		}
	}
	assert.EqualValues(t, 2, active.Load())
	releaseOnce.Do(func() { close(release) })
	result := <-done
	require.NoError(t, result.err)
	assert.EqualValues(t, 0, active.Load())
	require.Len(t, result.files.files, len(entries))
	for i, file := range result.files.files {
		assert.Equal(t, entries[i].Path, file.path)
	}
	assert.EqualValues(t, 7, result.files.registeredBytes)
	assert.Equal(t, 7, result.files.registrations)
}

func TestVerifyFilesKeepsFirstRefusalAndJoinsWorkers(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	first := errors.New("first file refusal")
	second := errors.New("second file refusal")
	secondFinished := make(chan struct{})
	var finished atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := verifyFiles(ctx, []modelartifact.ManifestEntry{{Path: "a"}, {Path: "b"}, {Path: "c"}},
		func(ctx context.Context, entry modelartifact.ManifestEntry, _ int64) (*verifiedFile, error) {
			defer finished.Add(1)
			if entry.Path == "a" {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-secondFinished:
					return nil, first
				}
			}
			if entry.Path == "b" {
				close(secondFinished)
				return nil, second
			}
			return nil, errors.New("a later file started after the first refusal")
		})
	require.ErrorIs(t, err, first)
	assert.EqualValues(t, 2, finished.Load())
}

func TestVerifyFilesCancellationJoinsActiveWorkers(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	var finished atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := verifyFiles(ctx, []modelartifact.ManifestEntry{{Path: "a"}, {Path: "b"}},
			func(ctx context.Context, _ modelartifact.ManifestEntry, _ int64) (*verifiedFile, error) {
				started <- struct{}{}
				<-ctx.Done()
				finished.Add(1)
				return nil, ctx.Err()
			})
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("verification workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop verification workers")
	}
	assert.EqualValues(t, 2, finished.Load())
}

// TestVerifyFilesCancellationDispatchesWithExitedWorkers pins the schedule
// the dispatch-loop suspicion worried about: cancellation lands while every
// worker has already left, and the consumer still performs one dispatch send
// into the jobs buffer before observing the cancellation. The taken-always-
// leads-consumed invariant keeps that send below the buffer capacity, so the
// run must join instead of deadlocking.
func TestVerifyFilesCancellationDispatchesWithExitedWorkers(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() {
		_, err := verifyFiles(ctx, []modelartifact.ManifestEntry{{Path: "a"}, {Path: "b"}, {Path: "c"}, {Path: "d"}},
			func(ctx context.Context, _ modelartifact.ManifestEntry, _ int64) (*verifiedFile, error) {
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			})
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("verification workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled,
			"the consumer exits through its own cancellation check after the buffered dispatch")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation deadlocked the consumer behind its buffered dispatch")
	}
}

func TestVerifyFilesEnforcesAggregateBounds(t *testing.T) {
	tensors := make([]protocol.TensorDescriptor, MaxTensors+1)
	tests := []struct {
		name    string
		sizes   []int64
		counts  []int
		wantErr error
	}{
		{name: "registered byte boundary", sizes: []int64{MaxRegisteredBytes - 1, 1}, counts: []int{1, 1}},
		{name: "registered byte excess", sizes: []int64{MaxRegisteredBytes, 1}, counts: []int{1, 1}, wantErr: ErrBoundsExceeded},
		{name: "tensor boundary", sizes: []int64{1, 1}, counts: []int{MaxTensors - 1, 1}},
		{name: "tensor excess", sizes: []int64{1, 1}, counts: []int{MaxTensors, 1}, wantErr: ErrBoundsExceeded},
		{name: "no tensors", sizes: []int64{1, 1}, counts: []int{0, 0}, wantErr: ErrNoTensors},
		{name: "empty listing", wantErr: ErrNoTensors},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries := make([]modelartifact.ManifestEntry, len(tc.sizes))
			for i, size := range tc.sizes {
				entries[i] = modelartifact.ManifestEntry{Path: strconv.Itoa(i), Size: size}
			}
			_, err := verifyFiles(context.Background(), entries,
				func(_ context.Context, entry modelartifact.ManifestEntry, _ int64) (*verifiedFile, error) {
					i, parseErr := strconv.Atoi(entry.Path)
					if parseErr != nil {
						return nil, parseErr
					}
					return &verifiedFile{path: entry.Path, size: entry.Size, tensors: tensors[:tc.counts[i]]}, nil
				})
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestVerifyOneChecksRegistrationBoundBeforeStreaming(t *testing.T) {
	dir := t.TempDir()
	shard := shardWith("nope")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.safetensors"), shard, 0o600))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	entry := modelartifact.ManifestEntry{Path: "model.safetensors", Size: int64(len(shard)), Digest: sha256digest(shard)}
	_, err = verifyOne(context.Background(), root, entry, MaxRegisteredBytes+1)
	require.ErrorIs(t, err, ErrBoundsExceeded)
}
