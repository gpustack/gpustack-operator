package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

var testNow = time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC)

func hexOf(c byte) string { return strings.Repeat(string(c), 64) }

// testCache is a store on a filesystem simulated as 1000 bytes, whose usage is a fixed base plus
// the size of every published tree and partial.
type testCache struct {
	store *store.Store
	base  uint64
}

func newTestCache(t *testing.T, base uint64) *testCache {
	t.Helper()
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
	return &testCache{store: st, base: base}
}

func (c *testCache) usage() (Usage, error) {
	used := c.base
	markers, err := c.store.Published()
	if err != nil {
		return Usage{}, err
	}
	for _, m := range markers {
		used += uint64(m.SizeBytes)
	}
	partials, err := c.store.Partials()
	if err != nil {
		return Usage{}, err
	}
	for _, p := range partials {
		used += uint64(p.SizeBytes)
	}
	return Usage{Total: 1000, Used: used}, nil
}

// publish adds a published tree of size bytes, last used at lastUsed.
func (c *testCache) publish(t *testing.T, hex string, size int64, lastUsed time.Time) {
	t.Helper()
	a, err := c.store.NewAttempt(hex)
	require.NoError(t, err)
	p, err := a.FilePath("model.bin")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	require.NoError(t, a.Publish(store.Marker{Digest: "sha256:" + hex, SizeBytes: size, PublishedTime: lastUsed}))
	rec, err := c.store.ReadDigest(hex)
	require.NoError(t, err)
	rec.LastUsedTime = lastUsed
	require.NoError(t, c.store.WriteDigest(hex, rec))
}

func (c *testCache) collector(referenced, downloading map[string]bool) *Collector {
	return &Collector{
		Store:       c.store,
		Usage:       c.usage,
		Watermarks:  func() (int32, int32, bool) { return 80, 70, true },
		Referenced:  func() (map[string]bool, error) { return referenced, nil },
		Downloading: func() map[string]bool { return downloading },
		Now:         func() time.Time { return testNow },
	}
}

// claim records a mount of hex the way a mount does, after the collector's view of the references.
func (c *testCache) claim(t *testing.T, hex string) {
	t.Helper()
	published, err := c.store.Claim(store.Ref{VolumeID: "csi-" + hex[:8], TargetPath: "/var/lib/kubelet/pods/p/" + hex[:8], Hex: hex})
	require.NoError(t, err)
	require.True(t, published)
}

func (c *testCache) published(t *testing.T) []string {
	t.Helper()
	markers, err := c.store.Published()
	require.NoError(t, err)
	hexes := make([]string, 0, len(markers))
	for _, m := range markers {
		hexes = append(hexes, store.HexOf(m.Digest))
	}
	return hexes
}

func TestCollect(t *testing.T) {
	old, older, oldest := testNow.Add(-2*time.Hour), testNow.Add(-3*time.Hour), testNow.Add(-4*time.Hour)
	cases := []struct {
		name        string
		base        uint64
		trees       map[string]time.Time // hex -> last use, each 100 bytes
		referenced  map[string]bool
		downloading map[string]bool
		// claimed are mounted after the collector read the references, so only the store knows.
		claimed      []string
		refsErr      bool
		unconfigured bool
		wantKept     []string
		wantLow      bool
		wantSkipped  bool
	}{
		{
			name: "below the high watermark nothing goes, even unreferenced",
			base: 500, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): older},
			wantKept: []string{hexOf('a'), hexOf('b')},
		},
		{
			name: "above it the oldest unreferenced go until the low watermark",
			base: 450, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): older, hexOf('c'): old, hexOf('d'): old},
			// 850 used: removing a (750) then b (650) reaches 70%.
			wantKept: []string{hexOf('c'), hexOf('d')},
		},
		{
			name: "a referenced tree is never removed, however old",
			base: 450, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): oldest, hexOf('c'): older, hexOf('d'): old},
			referenced: map[string]bool{hexOf('a'): true},
			// 850 used: a is skipped, b (750) and c (650) go.
			wantKept: []string{hexOf('a'), hexOf('d')},
		},
		{
			name: "a tree within its grace stays",
			base: 500, trees: map[string]time.Time{hexOf('a'): testNow.Add(-5 * time.Minute), hexOf('b'): old, hexOf('c'): older, hexOf('d'): oldest},
			wantKept: []string{hexOf('a'), hexOf('b')},
		},
		{
			name: "nothing removable leaves capacity low",
			base: 700, trees: map[string]time.Time{hexOf('a'): old, hexOf('b'): old},
			referenced: map[string]bool{hexOf('a'): true, hexOf('b'): true},
			wantKept:   []string{hexOf('a'), hexOf('b')}, wantLow: true,
		},
		{
			name: "references that cannot be read remove nothing and say so",
			base: 450, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): older, hexOf('c'): old, hexOf('d'): old},
			refsErr:  true,
			wantKept: []string{hexOf('a'), hexOf('b'), hexOf('c'), hexOf('d')}, wantSkipped: true,
		},
		{
			name: "no configuration applied yet removes nothing and says so",
			base: 450, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): older, hexOf('c'): old, hexOf('d'): old},
			unconfigured: true,
			wantKept:     []string{hexOf('a'), hexOf('b'), hexOf('c'), hexOf('d')}, wantSkipped: true,
		},
		{
			name: "a tree claimed after the references were read is kept",
			base: 450, trees: map[string]time.Time{hexOf('a'): oldest, hexOf('b'): older, hexOf('c'): old, hexOf('d'): old},
			claimed: []string{hexOf('a')},
			// 850 used: a is claimed, b (750) and c (650) go.
			wantKept: []string{hexOf('a'), hexOf('d')},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := newTestCache(t, c.base)
			for hex, used := range c.trees {
				cache.publish(t, hex, 100, used)
			}

			for _, hex := range c.claimed {
				cache.claim(t, hex)
			}
			col := cache.collector(c.referenced, c.downloading)
			if c.refsErr {
				col.Referenced = func() (map[string]bool, error) { return nil, errors.New("unreadable reference") }
			}
			if c.unconfigured {
				col.Watermarks = func() (int32, int32, bool) { return 0, 0, false }
			}

			res, err := col.Collect(context.Background())
			require.NoError(t, err)
			assert.ElementsMatch(t, c.wantKept, cache.published(t))
			assert.Equal(t, c.wantLow, res.CapacityLow)
			assert.Equal(t, c.wantSkipped, res.Skipped != "", "skipped: %q", res.Skipped)
		})
	}
}

func TestCollectCountsWhatItRemovedWhenItFailsLater(t *testing.T) {
	// 850 of 1000 used: removing a leaves 750, above the low watermark, so usage is read again.
	c := newTestCache(t, 650)
	c.publish(t, hexOf('a'), 100, testNow.Add(-4*time.Hour))
	c.publish(t, hexOf('b'), 100, testNow.Add(-3*time.Hour))
	col := c.collector(nil, nil)
	calls := 0
	col.Usage = func() (Usage, error) {
		if calls++; calls > 1 {
			return Usage{}, errors.New("statfs failed")
		}
		return c.usage()
	}
	before := testutil.ToFloat64(metrics.GCRemovedBytes)

	res, err := col.Collect(context.Background())
	require.Error(t, err)
	assert.Equal(t, int64(100), res.RemovedBytes)
	assert.InDelta(t, 100, testutil.ToFloat64(metrics.GCRemovedBytes)-before, 0, "the removal before the failure is counted")
}

func TestCollectPartials(t *testing.T) {
	cache := newTestCache(t, 0)
	for _, hex := range []string{hexOf('a'), hexOf('b'), hexOf('c')} {
		a, err := cache.store.NewAttempt(hex)
		require.NoError(t, err)
		p, err := a.FilePath("model.bin")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, []byte("partial"), 0o644))
		if hex != hexOf('c') {
			stale := testNow.Add(-25 * time.Hour)
			require.NoError(t, os.Chtimes(p, stale, stale))
			require.NoError(t, os.Chtimes(filepath.Dir(p), stale, stale))
			require.NoError(t, os.Chtimes(a.Dir(), stale, stale))
		}
	}

	// a is stale and idle; b is stale but its attempt runs; c is recent.
	res, err := cache.collector(nil, map[string]bool{hexOf('b'): true}).Collect(context.Background())
	require.NoError(t, err)
	partials, err := cache.store.Partials()
	require.NoError(t, err)
	left := make([]string, 0, len(partials))
	for _, p := range partials {
		left = append(left, p.Hex)
	}
	assert.ElementsMatch(t, []string{hexOf('b'), hexOf('c')}, left)
	assert.Equal(t, int64(len("partial")), res.RemovedBytes)
}

func TestReserve(t *testing.T) {
	old := testNow.Add(-2 * time.Hour)
	cases := []struct {
		name       string
		base       uint64
		bytes      int64
		referenced map[string]bool
		wantErr    bool
	}{
		{name: "room under the high watermark", base: 400, bytes: 100},
		{name: "room made by collection", base: 500, bytes: 100},
		{name: "no room left", base: 500, bytes: 150, referenced: map[string]bool{hexOf('a'): true, hexOf('b'): true}, wantErr: true},
		{name: "more than the filesystem", base: 0, bytes: 2000, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := newTestCache(t, c.base)
			cache.publish(t, hexOf('a'), 100, old)
			cache.publish(t, hexOf('b'), 100, old)

			err := cache.collector(c.referenced, nil).Reserve(context.Background(), c.bytes)
			if !c.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, download.ReasonInsufficientCapacity, download.ReasonOf(err))
		})
	}
}

func TestKubeletCap(t *testing.T) {
	const total = 1000 << 30
	cases := []struct {
		name       string
		file       string
		noFile     bool
		want       int32
		wantSource string
	}{
		{name: "an absent file takes kubelet's defaults", noFile: true, want: 80, wantSource: "no kubelet configuration file"},
		{name: "an unreadable file takes kubelet's defaults", file: "{not yaml", want: 80, wantSource: "cannot be read"},
		{name: "a file setting none takes kubelet's defaults", file: "kind: KubeletConfiguration\n", want: 80, wantSource: "configuration file at"},
		{
			name: "percent thresholds from the file",
			file: "evictionHard:\n  nodefs.available: \"5%\"\n  imagefs.available: \"8%\"\nimageGCHighThresholdPercent: 90\n",
			want: 85, wantSource: "configuration file at",
		},
		{
			name: "a quantity threshold, as a share of the filesystem",
			file: "evictionHard:\n  nodefs.available: \"200Gi\"\n  imagefs.available: \"1%\"\nimageGCHighThresholdPercent: 99\n",
			want: 75, wantSource: "configuration file at",
		},
		{name: "image collection is the lowest", file: "imageGCHighThresholdPercent: 70\n", want: 65},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if !c.noFile {
				require.NoError(t, os.WriteFile(path, []byte(c.file), 0o644))
			}
			got := KubeletCap(path, total)
			assert.Equal(t, c.want, got.Percent)
			assert.Contains(t, got.Source, c.wantSource)
		})
	}
}

func TestEffectiveWatermarks(t *testing.T) {
	high, low, msg := Effective(80, 70, nil)
	assert.Equal(t, []int32{80, 70}, []int32{high, low})
	assert.Empty(t, msg, "a dedicated filesystem applies the Setting as written")

	high, low, msg = Effective(95, 90, &Cap{Percent: 80, Source: "kubelet's default thresholds"})
	assert.Equal(t, []int32{80, 79}, []int32{high, low})
	assert.Contains(t, msg, "capped at 80%")
	assert.Contains(t, msg, "kubelet's default thresholds")

	high, low, _ = Effective(75, 60, &Cap{Percent: 80})
	assert.Equal(t, []int32{75, 60}, []int32{high, low}, "a watermark under the cap is kept")
}

func TestCollectPrunesStaleFailureRecords(t *testing.T) {
	stale, recent := testNow.Add(-25*time.Hour), testNow.Add(-time.Hour)
	cases := []struct {
		name        string
		record      store.DigestRecord
		published   bool
		partial     bool
		downloading bool
		wantKept    bool
	}{
		{name: "a failure long past with nothing on disk is dropped", record: store.DigestRecord{Reason: download.ReasonSourceUnavailable, RetryTime: stale}},
		{name: "a recent failure is kept", record: store.DigestRecord{Reason: download.ReasonSourceUnavailable, RetryTime: recent}, wantKept: true},
		{name: "a failure whose partial is on disk is kept", record: store.DigestRecord{Reason: download.ReasonCanceled, RetryTime: stale}, partial: true, wantKept: true},
		{name: "a failure of a digest now published is kept", record: store.DigestRecord{Reason: download.ReasonSourceUnavailable, RetryTime: stale}, published: true, wantKept: true},
		{name: "a failure of a digest being materialized is kept", record: store.DigestRecord{Reason: download.ReasonSourceUnavailable, RetryTime: stale}, downloading: true, wantKept: true},
		{name: "a record without a failure is kept", record: store.DigestRecord{LastUsedTime: stale}, wantKept: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := newTestCache(t, 0)
			hex := hexOf('a')
			if c.published {
				cache.publish(t, hex, 100, stale)
			}
			if c.partial {
				a, err := cache.store.NewAttempt(hex)
				require.NoError(t, err)
				p, err := a.FilePath("model.bin")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
			}
			rec, err := cache.store.ReadDigest(hex)
			require.NoError(t, err)
			c.record.LastAttempt = rec.LastAttempt
			if c.published {
				c.record.LastUsedTime = rec.LastUsedTime
			}
			require.NoError(t, cache.store.WriteDigest(hex, c.record))

			_, err = cache.collector(nil, map[string]bool{hex: c.downloading}).Collect(context.Background())
			require.NoError(t, err)
			all, err := cache.store.Digests()
			require.NoError(t, err)
			_, kept := all[hex]
			assert.Equal(t, c.wantKept, kept)
		})
	}
}
