package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// spanResolver hands validateRange a bound paged manifest, the shape the
// broker's derived manifests take, so span reads run their page fetches.
type spanResolver struct{ manifest *protocol.Manifest }

func (r spanResolver) ResolveManifest(context.Context, string, string, string, int64) (*protocol.Manifest, error) {
	return r.manifest, nil
}

// pagedSpanManifest builds a two-page manifest over one file whose tensors
// abut at byte 12, with a page reader that fails the span's second page
// read: the binding consumed the first read of every page, so the failing
// read is exactly the cross-page step inside ValidTensorSpan. A nil fail
// keeps the page store healthy.
func pagedSpanManifest(t *testing.T, fail func(context.Context) error) *protocol.Manifest {
	t.Helper()
	pagesDir := filepath.Join(t.TempDir(), "pages")
	require.NoError(t, os.MkdirAll(pagesDir, 0o700))
	files := []protocol.FileDescriptor{{Path: "weights.safetensors", Size: 32}}
	page := func(tensors []protocol.TensorDescriptor) protocol.TensorPage {
		t.Helper()
		raw, err := json.Marshal(tensors)
		require.NoError(t, err)
		sum := sha256.Sum256(raw)
		ref := protocol.TensorPage{
			SHA256: hex.EncodeToString(sum[:]), Size: len(raw),
			Count: len(tensors), File: tensors[0].File,
			Begin: tensors[0].DataBegin, End: tensors[len(tensors)-1].DataEnd,
		}
		require.NoError(t, os.WriteFile(filepath.Join(pagesDir, ref.SHA256+".json"), raw, 0o600))
		return ref
	}
	pages := []protocol.TensorPage{
		page([]protocol.TensorDescriptor{{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}}),
		page([]protocol.TensorDescriptor{{Name: "last", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 12, DataEnd: 16}}),
	}
	manifest := &protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact-uid-1", SourceGeneration: 1,
		Files: files, Tensors: []protocol.TensorDescriptor{}, TensorPages: pages,
	}
	reads := map[string]int{}
	reader := func(ctx context.Context, page protocol.TensorPage) ([]byte, error) {
		reads[page.SHA256]++
		if fail != nil && page.SHA256 == pages[1].SHA256 && reads[page.SHA256] > 1 {
			return nil, fail(ctx)
		}
		return protocol.LocalPageReader(pagesDir)(ctx, page)
	}
	require.NoError(t, manifest.BindPages(context.Background(), reader))
	return manifest
}

func authorizeSpan(t *testing.T, manifest *protocol.Manifest, mutate func(*Request), ctx context.Context) (Result, *protocol.Error) {
	t.Helper()
	e := newEnv(t)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(defaultObjects(e.digest)...).Build()
	auth := &Authorizer{Reader: cli, Reviewer: okReviewer(), Store: e.store, Resolver: spanResolver{manifest}}
	request := e.request()
	request.Tensor, request.Offset, request.Length = "first", 0, 8
	if mutate != nil {
		mutate(&request)
	}
	return auth.AuthorizeSession(ctx, request)
}

func TestSessionRangeMapsSpanReadFailuresToSourceUnavailable(t *testing.T) {
	manifest := pagedSpanManifest(t, func(context.Context) error {
		return errors.New("page store unavailable")
	})
	_, verdict := authorizeSpan(t, manifest, nil, context.Background())
	require.NotNil(t, verdict, "a failing span read must not admit")
	assert.Equal(t, protocol.KindSourceUnavailable, verdict.Kind,
		"a page read failure is an unavailable source, never a range overflow")
	assert.Contains(t, verdict.Detail, "page store unavailable",
		"the verdict carries the read failure's detail")
}

func TestSessionRangeKeepsRangeOverflowForGenuinelyInvalidSpans(t *testing.T) {
	manifest := pagedSpanManifest(t, nil)
	_, verdict := authorizeSpan(t, manifest, func(r *Request) { r.Length = 12 }, context.Background())
	require.NotNil(t, verdict, "a span past the file's tensors must not admit")
	assert.Equal(t, protocol.KindRangeOverflow, verdict.Kind,
		"an invalid span keeps the range verdict the review reserved for it")
	assert.Contains(t, verdict.Detail, "exceeds tensor byte size")
}

func TestSessionRangeMapsSpanCancellationToSourceUnavailable(t *testing.T) {
	entered := make(chan struct{})
	manifest := pagedSpanManifest(t, func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	e := newEnv(t)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(defaultObjects(e.digest)...).Build()
	auth := &Authorizer{Reader: cli, Reviewer: okReviewer(), Store: e.store, Resolver: spanResolver{manifest}}
	request := e.request()
	request.Tensor, request.Offset, request.Length = "first", 0, 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		verdict *protocol.Error
	}
	done := make(chan outcome, 1)
	go func() {
		_, verdict := auth.AuthorizeSession(ctx, request)
		done <- outcome{verdict}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the span read never reached the second page")
	}
	cancel()
	select {
	case out := <-done:
		require.NotNil(t, out.verdict, "a canceled span read must not admit")
		assert.Equal(t, protocol.KindSourceUnavailable, out.verdict.Kind)
		assert.Contains(t, out.verdict.Detail, "context canceled",
			"the verdict carries the cancellation's detail")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop the span check")
	}
}
