package modelstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func entry(state workercore.NodeModelStoreModelState, downloaded, size int64, reason string) workercore.NodeModelStoreModel {
	return workercore.NodeModelStoreModel{Digest: "sha256:d", State: state, DownloadedBytes: downloaded, SizeBytes: size, Reason: reason}
}

func TestAggregateEntries(t *testing.T) {
	const (
		ready       = workercore.NodeModelStoreModelStateReady
		downloading = workercore.NodeModelStoreModelStateDownloading
		failed      = workercore.NodeModelStoreModelStateFailed
	)
	cases := []struct {
		name        string
		entries     map[string]workercore.NodeModelStoreModel
		want        [3]int32
		wantBytes   int64
		wantPercent *int32
		wantStepped *int32
		wantReasons map[string]int32
	}{
		{name: "no node holds it", entries: nil, wantReasons: map[string]int32{}},
		{
			name: "the mean covers only the downloading nodes",
			entries: map[string]workercore.NodeModelStoreModel{
				"a": entry(ready, 0, 100, ""), "b": entry(ready, 0, 100, ""),
				"c": entry(downloading, 30, 100, ""), "d": entry(downloading, 57, 100, ""),
			},
			want: [3]int32{2, 2, 0}, wantBytes: 87, wantPercent: ptr.To[int32](43), wantStepped: ptr.To[int32](40),
			wantReasons: map[string]int32{},
		},
		{
			name:    "a node just starting does not pull the ready ones down; its size unknown counts as 0%",
			entries: map[string]workercore.NodeModelStoreModel{"a": entry(ready, 0, 100, ""), "b": entry(downloading, 0, 0, "")},
			want:    [3]int32{1, 1, 0}, wantPercent: ptr.To[int32](0), wantStepped: ptr.To[int32](0), wantReasons: map[string]int32{},
		},
		{
			name: "failures are counted by reason",
			entries: map[string]workercore.NodeModelStoreModel{
				"a": entry(failed, 0, 0, "SourceUnavailable"), "b": entry(failed, 0, 0, "SourceUnavailable"),
				"c": entry(failed, 0, 0, "AccessDenied"),
			},
			want: [3]int32{0, 0, 3}, wantReasons: map[string]int32{"SourceUnavailable": 2, "AccessDenied": 1},
		},
		{
			name:    "a node past its size counts as done",
			entries: map[string]workercore.NodeModelStoreModel{"a": entry(downloading, 150, 100, "")},
			want:    [3]int32{0, 1, 0}, wantBytes: 150, wantPercent: ptr.To[int32](100), wantStepped: ptr.To[int32](100),
			wantReasons: map[string]int32{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := AggregateEntries(c.entries)
			assert.Equal(t, c.want, [3]int32{a.Ready, a.Downloading, a.Failed})
			assert.Equal(t, c.wantBytes, a.DownloadedBytes)
			p, ok := a.DownloadingPercent()
			if c.wantPercent == nil {
				assert.False(t, ok)
			} else {
				assert.Equal(t, *c.wantPercent, p)
			}
			assert.Equal(t, c.wantStepped, a.SteppedPercent())
			assert.Equal(t, c.wantReasons, a.FailureReasons)
		})
	}
}

func TestNodeEntries(t *testing.T) {
	store := func(name string, digests ...string) workercore.NodeModelStore {
		s := workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: name}}
		for _, d := range digests {
			s.Status.Models = append(s.Status.Models, workercore.NodeModelStoreModel{Digest: d})
		}
		return s
	}
	got := NodeEntries([]workercore.NodeModelStore{store("n1", "sha256:a", "sha256:b"), store("n2", "sha256:b"), store("n3")}, "sha256:b")
	assert.Len(t, got, 2)
	assert.Contains(t, got, "n1")
	assert.Contains(t, got, "n2")
}
