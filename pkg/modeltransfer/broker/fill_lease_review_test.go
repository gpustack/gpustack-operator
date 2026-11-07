package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func TestReviewFillRefusesLeaseExpiredDuringManifestRead(t *testing.T) {
	f := newPagedSourceFixture(t)
	f.b.mu.Lock()
	rec := f.b.records[f.generation]
	rec.State = StateFilling
	descriptors := append([]RegistrationDescriptor(nil), rec.Descriptors...)
	expires := rec.LeaseExpires
	f.b.manifestCache = nil
	f.b.mu.Unlock()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.b.setPageReader(func(dir string) protocol.PageReader {
		read := protocol.LocalPageReader(dir)
		return func(ctx context.Context, page protocol.TensorPage) ([]byte,
			error,
		) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return read(ctx, page)
		}
	})
	done := make(chan *protocol.Error, 1)
	go func() {
		done <- f.b.FillVerified(t.Context(), f.generation, "source", f.leaseID, f.workerSecret, int32(len(descriptors)), descriptors, f.instance)
	}()
	awaitGateEntered(t, entered)
	f.b.SetNow(func() time.Time { return expires.Add(time.Nanosecond) })
	unblock()
	require.NotNil(t, <-done, "expired worker lease must be refused at registration publication")
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	require.Equal(t, StateFilling, f.b.records[f.generation].State)
}
