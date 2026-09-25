package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestModelDeploymentKVIdentity(t *testing.T) {
	hub := func(digest string) *workercore.ModelArtifact {
		ma := artifactFixture("", true, true)
		ma.Status.Resolved.ManifestDigest = digest
		return ma
	}
	claim := func(uid string) *workercore.ModelArtifact {
		ma := artifactFixture("models", true, true)
		ma.UID = types.UID(uid)
		return ma
	}
	cases := []struct {
		name string
		a, b *workercore.ModelArtifact
		same bool
	}{
		{name: "one digest is one identity", a: hub(testArtifactDigest), b: hub(testArtifactDigest), same: true},
		{name: "two digests are two", a: hub(testArtifactDigest), b: hub("sha256:" + strings.Repeat("2", 64))},
		{name: "one claim artifact is one identity", a: claim("uid-a"), b: claim("uid-a"), same: true},
		{name: "two claim artifacts are two, even on one claim", a: claim("uid-a"), b: claim("uid-b")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := modelArtifactKVIdentity(c.a), modelArtifactKVIdentity(c.b)
			assert.Equal(t, c.same, a == b)
			for _, id := range []string{a, b} {
				assert.Len(t, id, 34)
				assert.True(t, strings.HasPrefix(id, "m-"))
				assert.NotContains(t, id, "@")
				assert.NotContains(t, id, "_")
				assert.NotContains(t, id, ":")
			}
		})
	}
	assert.Equal(t, "m-"+strings.Repeat("1", 32), modelArtifactKVIdentity(hub(testArtifactDigest)))
}

func TestModelDeploymentKVIdentityReachesTheEngine(t *testing.T) {
	connection := connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA)
	cases := []struct {
		name    string
		ref     bool
		store   bool
		wantKey bool
	}{
		{name: "an artifact and a store render the prefix", ref: true, store: true, wantKey: true},
		{name: "without an artifact nothing is rendered", store: true},
		{name: "without a store nothing is rendered", ref: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := artifactDeploymentFixture(1)
			md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}}
			if !c.ref {
				md.Spec.Model.ArtifactRef = nil
			}
			cli := newModelDeploymentClient(md, newRenderInstanceType(), artifactFixture("", true, true))
			r := &ModelDeploymentReconciler{Client: cli}
			weights, err := r.resolveModelDeploymentWeights(context.Background(), md)
			require.NoError(t, err)
			var conn *ModelDeploymentConnectorInput
			if c.store {
				conn = &connection
			}

			desired, err := r.renderModelDeploymentPods(context.Background(), md, conn, nil, weights)
			require.NoError(t, err)
			cmd := strings.Join(desired["server"][0][0].Spec.Containers[0].Command, " ")
			assert.Equal(t, c.wantKey, strings.Contains(cmd, `"cache_prefix":"m-`+strings.Repeat("1", 32)+`"`), cmd)
		})
	}
}
