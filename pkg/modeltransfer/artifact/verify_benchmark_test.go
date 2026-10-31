package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelartifact"
)

// BenchmarkVerifyFiles measures bounded header inspection over eight shards.
func BenchmarkVerifyFiles(b *testing.B) {
	const shardBytes = 8 << 20
	dir := b.TempDir()
	data := make([]byte, shardBytes)
	for i := range data {
		data[i] = byte(i)
	}
	entries := make([]modelartifact.ManifestEntry, 8)
	var total int64
	for i := range entries {
		path := fmt.Sprintf("%d.safetensors", i)
		header := fmt.Sprintf(`{"tensor%d":{"dtype":"U8","shape":[%d],"data_offsets":[0,%d]}}`, i, shardBytes, shardBytes)
		shard := shardWith(header, data...)
		require.NoError(b, os.WriteFile(filepath.Join(dir, path), shard, 0o600))
		entries[i] = modelartifact.ManifestEntry{Path: path, Size: int64(len(shard)), Digest: sha256digest(shard)}
		total += int64(len(shard))
	}
	root, err := os.OpenRoot(dir)
	require.NoError(b, err)
	defer func() { _ = root.Close() }()
	previous := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(previous)
	cases := []struct {
		name    string
		workers int
	}{
		{name: "headers_serial", workers: 1},
		{name: "headers_parallel", workers: min(8, previous)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			previous := runtime.GOMAXPROCS(tc.workers)
			defer runtime.GOMAXPROCS(previous)
			verify := func(ctx context.Context, entry modelartifact.ManifestEntry, registered int64) (*verifiedFile, error) {
				return verifyOne(ctx, root, entry, registered)
			}
			b.SetBytes(total)
			b.ReportAllocs()
			for b.Loop() {
				_, err := verifyFiles(context.Background(), entries, verify)
				require.NoError(b, err)
			}
		})
	}
}
