package protocol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadSpanManifest(t *testing.T) *Manifest {
	t.Helper()
	path := writePagedFixture(t, [][]TensorDescriptor{
		{{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}},
		{{Name: "last", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 12, DataEnd: 16}},
	})
	manifest, err := LoadManifest(t.Context(), path, "")
	require.NoError(t, err)
	return manifest
}

func spanTensor(t *testing.T, manifest *Manifest) TensorDescriptor {
	t.Helper()
	tensor, err := manifest.Tensor(context.Background(), "first")
	require.NoError(t, err)
	return tensor
}

func TestValidTensorSpanKeepsTheRangeVerdictFreeOfReadFailures(t *testing.T) {
	manifest := loadSpanManifest(t)
	tensor := spanTensor(t, manifest)
	boom := errors.New("page store unavailable")
	underlying := manifest.pageIndex.reader
	manifest.pageIndex.reader = func(ctx context.Context, page TensorPage) ([]byte, error) {
		if page.SHA256 == manifest.TensorPages[1].SHA256 {
			return nil, boom
		}
		return underlying(ctx, page)
	}
	valid, err := manifest.ValidTensorSpan(context.Background(), tensor, 0, 8)
	assert.False(t, valid, "the span verdict stays false when it cannot be proven")
	require.ErrorIs(t, err, boom, "the page read failure is returned with its identity intact")
	assert.Equal(t, boom, err, "the raw reader error is not rewrapped or reclassified")
	assert.NotEqual(t, KindRangeOverflow, KindOf(err), "a read failure is not a range verdict")
}

func TestValidTensorSpanPreservesContextCancellation(t *testing.T) {
	manifest := loadSpanManifest(t)
	tensor := spanTensor(t, manifest)
	entered := make(chan struct{})
	underlying := manifest.pageIndex.reader
	manifest.pageIndex.reader = func(ctx context.Context, page TensorPage) ([]byte, error) {
		if page.SHA256 == manifest.TensorPages[1].SHA256 {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return underlying(ctx, page)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := manifest.ValidTensorSpan(ctx, tensor, 0, 8)
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	select {
	case <-entered:
	case <-deadline:
		t.Fatal("the span read never reached the second page")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled, "the context error keeps its identity")
		assert.Equal(t, context.Canceled, err, "the raw context error is returned unchanged")
	case <-deadline:
		t.Fatal("the span read did not return after cancellation")
	}
}

func TestValidTensorSpanPreCanceledReadsNothing(t *testing.T) {
	manifest := loadSpanManifest(t)
	tensor := spanTensor(t, manifest)
	calls := 0
	manifest.pageIndex.reader = func(ctx context.Context, page TensorPage) ([]byte, error) {
		calls++
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	valid, err := manifest.ValidTensorSpan(ctx, tensor, 0, 8)
	assert.False(t, valid)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, calls, "a canceled span check reads no pages")
}

func TestValidTensorSpanReservesFalseNilForGenuinelyInvalidSpans(t *testing.T) {
	for _, tc := range []struct {
		name           string
		offset, length int64
		want           bool
		wantWrites     int
	}{
		{name: "slice inside the tensor", offset: 0, length: 2, want: true, wantWrites: 0},
		{name: "contiguous run ending on a boundary", offset: 0, length: 8, want: true, wantWrites: 1},
		{name: "run past the file's tensors", offset: 0, length: 12, want: false, wantWrites: 1},
		{name: "offset beyond the tensor", offset: 5, length: 1, want: false, wantWrites: 0},
		{name: "negative length", offset: 0, length: -1, want: false, wantWrites: 0},
		{name: "length overflows the address space", offset: 0, length: 1<<63 - 1, want: false, wantWrites: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := loadSpanManifest(t)
			tensor := spanTensor(t, manifest)
			writes := 0
			underlying := manifest.pageIndex.reader
			manifest.pageIndex.reader = func(ctx context.Context, page TensorPage) ([]byte, error) {
				writes++
				return underlying(ctx, page)
			}
			valid, err := manifest.ValidTensorSpan(context.Background(), tensor, tc.offset, tc.length)
			require.NoError(t, err, "a healthy page store never turns a range question into an error")
			assert.Equal(t, tc.want, valid)
			assert.Equal(t, tc.wantWrites, writes,
				"only spans that leave the tensor read pages; the starting page is already cached")
		})
	}
}

func TestValidTensorSpanUnverifiedPagesFailClosed(t *testing.T) {
	manifest := &Manifest{
		ContractVersion: ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
		Files: []FileDescriptor{{Path: "weights.safetensors", Size: 32}},
		TensorPages: []TensorPage{{
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Size:   9, Count: 1, File: "weights.safetensors", Begin: 8, End: 12,
		}},
	}
	tensor := TensorDescriptor{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}
	valid, err := manifest.ValidTensorSpan(context.Background(), tensor, 0, 8)
	assert.False(t, valid)
	require.NotNil(t, err, "unverified pages are an unavailable source, not an invalid span")
	assert.Equal(t, KindSourceUnavailable, KindOf(err))
}
