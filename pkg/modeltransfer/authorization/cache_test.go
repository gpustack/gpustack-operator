package authorization

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authentication "k8s.io/api/authentication/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

type countingReader struct {
	ctrlcli.Reader
	reads int
}

func (r *countingReader) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	r.reads++
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestPositiveAuthorizationCache(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ttl         time.Duration
		wantReads   int
		wantReviews int
	}{
		{"disabled", 0, 4, 2},
		{"enabled", time.Minute, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			reader := &countingReader{Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(defaultObjects(e.digest)...).Build()}
			reviews := 0
			reviewer := &fakeReviewer{answer: func(*authentication.TokenReview) (*authentication.TokenReview, error) {
				reviews++
				return authenticatedReview("tenant-a", "engine-1", "pod-uid-1"), nil
			}}
			a := &Authorizer{Reader: reader, Reviewer: reviewer, Store: e.store, CacheTTL: tc.ttl}
			for range 2 {
				_, err := a.AuthorizeSession(ctx, e.request())
				require.Nil(t, err)
			}
			assert.Equal(t, tc.wantReads, reader.reads)
			assert.Equal(t, tc.wantReviews, reviews)
		})
	}
}

func TestAuthorizationCacheExpiryAndIsolation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	reader := &countingReader{Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(defaultObjects(e.digest)...).Build()}
	a := &Authorizer{Reader: reader, Reviewer: okReviewer(), CacheTTL: time.Minute}
	req := e.request()
	first, err := a.authorizeArtifact(ctx, req)
	require.Nil(t, err)
	first.Generation = 99
	second, err := a.authorizeArtifact(ctx, req)
	require.Nil(t, err)
	assert.EqualValues(t, 1, second.Generation, "cached artifacts must not share mutable state")
	for key, entry := range a.artifactCache {
		entry.Expires = time.Now().Add(-time.Second)
		a.artifactCache[key] = entry
	}
	_, err = a.authorizeArtifact(ctx, req)
	require.Nil(t, err)
	assert.Equal(t, 2, reader.reads)
	_, err = a.authenticate(ctx, req, false)
	require.Nil(t, err)
	_, err = a.authenticate(ctx, req, true)
	require.Nil(t, err)
	assert.Equal(t, 4, reader.reads, "cleanup must not reuse read authorization")
	for key, entry := range a.podCache {
		entry.Expires = time.Now().Add(-time.Second)
		a.podCache[key] = entry
	}
	_, err = a.authenticate(ctx, req, false)
	require.Nil(t, err)
	assert.Equal(t, 5, reader.reads)
	req.ArtifactUID = "another-artifact"
	for range 2 {
		_, err = a.authorizeArtifact(ctx, req)
		require.NotNil(t, err)
	}
	assert.Equal(t, 7, reader.reads, "negative verdicts must not be cached")
}
