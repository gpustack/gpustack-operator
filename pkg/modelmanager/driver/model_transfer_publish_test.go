package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// holdClaimLock takes the store's cross-process claim lock the way another process would hold it,
// so a mount waits inside its claim until its context ends. Cleanup releases the lock.
func holdClaimLock(t *testing.T, st *store.Store) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(st.Root(), "ledger", "claims.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}

// TestNodePublishKeepsTheCallerCancellationTyped pins that a claim the caller gave up on leaves
// the CSI call with the caller's own gRPC code, not an internal fault kubelet cannot tell from a
// real failure.
func TestNodePublishKeepsTheCallerCancellationTyped(t *testing.T) {
	hex := strings.Repeat("a", 64)
	attrs := func(delivery bool) map[string]string {
		vc := map[string]string{AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex}
		if delivery {
			vc[attrModelTransferDelivery] = modelTransferDeliveryMetadata
		}

		return vc
	}
	cases := []struct {
		name     string
		delivery bool
		deadline bool
		wantCode codes.Code
	}{
		{
			name:     "a deadlined complete-tree claim answers DeadlineExceeded",
			deadline: true,
			wantCode: codes.DeadlineExceeded,
		},
		{
			name:     "a canceled complete-tree claim answers Canceled",
			wantCode: codes.Canceled,
		},
		{
			name:     "a deadlined metadata-view claim answers DeadlineExceeded",
			delivery: true,
			deadline: true,
			wantCode: codes.DeadlineExceeded,
		},
		{
			name:     "a canceled metadata-view claim answers Canceled",
			delivery: true,
			wantCode: codes.Canceled,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newModelTransferEnv(t, hex)
			holdClaimLock(t, env.d.Store)
			var ctx context.Context
			var cancel context.CancelFunc
			if c.deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				t.Cleanup(func() { timer.Stop() })
			}
			defer cancel()
			_, err := env.d.NodePublishVolume(ctx, modelTransferRequest(env.target("typed"), attrs(c.delivery)))
			require.Error(t, err)
			assert.Equal(t, c.wantCode, status.Code(err), err)
			_, found, err := env.d.Store.ReadRef("csi-vol-1")
			require.NoError(t, err)
			assert.False(t, found, "a claim that gave up records nothing")
		})
	}
}

// publishFullTree publishes a complete tree for hex with the metadata files
// the model-transfer view needs.
func publishFullTree(t *testing.T, st *store.Store, hex string) {
	t.Helper()
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	attempt, err := st.NewAttempt(hex)
	require.NoError(t, err)
	for _, name := range []string{"manifest.json", "config.json"} {
		var b []byte
		if name == "manifest.json" {
			b, err = os.ReadFile(filepath.Join(fixture, name))
			require.NoError(t, err)
		} else {
			b = []byte(`{"architectures":["TinyForCausalLM"],"tie_word_embeddings":true}`)
		}
		p, err := attempt.FilePath(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, b, 0o644))
	}
	checkpoint, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	p, err := attempt.FilePath("checkpoint.safetensors")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, checkpoint, 0o644))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: "sha256:" + hex, SizeBytes: int64(len(checkpoint)) + 512,
		ManifestFormat: "model-transfer/test", Source: store.SourceHub,
	}))
}

// newModelTransferEnv is a driver env with a published tree and its artifact.
func newModelTransferEnv(t *testing.T, hex string) *testEnv {
	t.Helper()
	artifacts := testArtifact("team-a", "model", "uid-ma", "sha256:"+hex, true)
	env := newTestEnv(t, artifacts)
	publishFullTree(t, env.d.Store, hex)
	return env
}

func modelTransferRequest(target string, attrs map[string]string) *csi.NodePublishVolumeRequest {
	return publishRequest(target, kubeletVolumeContext(attrs, "team-a"))
}

func TestNodePublishModelTransferMetadataMountsTheView(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)
	target := env.target("pod-1")

	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}))
	require.NoError(t, err)

	// The mount binds a metadata view, not the published tree.
	source := env.mounter.mounted[target]
	require.True(t, strings.Contains(source, "views"), source)
	assert.NotEqual(t, env.d.Store.PublishedTree(hex), source)

	// The mount holds a reference on the published tree it derives from.
	ref, found, err := env.d.Store.ReadRef("csi-vol-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, hex, ref.Hex)

	// The view itself is verified and never a complete tree.
	assert.False(t, env.d.Store.IsPublished(filepath.Base(source)))
}

func TestNodePublishModelTransferMetadataIsIdempotent(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)
	target := env.target("pod-1")
	attrs := map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}
	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, attrs))
	require.NoError(t, err)
	_, err = env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, attrs))
	require.NoError(t, err)
}

func TestNodePublishModelTransferMetadataRefusesUnauthorizedRequests(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)
	target := env.target("pod-1")

	// The artifact resolves to another digest: the model-transfer path refuses it the
	// way the complete-tree path does.
	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma",
		AttrManifestDigest:        "sha256:" + strings.Repeat("b", 64),
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}))
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// A wrong UID is refused too.
	_, err = env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "someone-else",
		AttrManifestDigest:        "sha256:" + hex,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestNodePublishModelTransferMetadataNeverDownloadsACheckpoint(t *testing.T) {
	hex := strings.Repeat("a", 64)
	artifacts := testArtifact("team-a", "model", "uid-ma", "sha256:"+hex, true)
	env := newTestEnv(t, artifacts) // nothing published

	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(env.target("pod-1"), map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}))
	require.NotEqual(t, codes.OK, status.Code(err))
	// The full-checkpoint materializer was never asked: a metadata-only
	// request must not trigger a complete download.
	assert.Empty(t, env.materializer.requests)
	assert.Contains(t, err.Error(), "no verified published tree")
}

func TestNodePublishModelTransferMetadataRejectsUnknownRequiredFiles(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)

	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(env.target("pod-1"), map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery:      modelTransferDeliveryMetadata,
		attrModelTransferRequiredFiles: "config.json,weights.bin",
	}))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "weights.bin")
}

func TestNodePublishModelTransferMetadataUnknownDeliveryValue(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)

	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(env.target("pod-1"), map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery: "everything",
	}))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodeUnpublishModelTransferMetadata(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)
	target := env.target("pod-1")
	attrs := map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: "sha256:" + hex,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}
	_, err := env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, attrs))
	require.NoError(t, err)

	_, err = env.d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "csi-vol-1", TargetPath: target,
	})
	require.NoError(t, err)
	_, found, err := env.d.Store.ReadRef("csi-vol-1")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, []string{hex}, env.events.unmounted)
}

// TestNodePublishModelTransferMetadataFromAPreStagedViewWithoutACheckpoint pins the
// destination-node case: a node holding no full checkpoint for the digest
// still mounts its pre-staged, verified metadata view, and nothing about
// that mount marks a checkpoint published.
func TestNodePublishModelTransferMetadataFromAPreStagedViewWithoutACheckpoint(t *testing.T) {
	hex := strings.Repeat("e", 64)
	digest := "sha256:" + hex
	artifacts := testArtifact("team-a", "model", "uid-ma", digest, true)
	env := newTestEnv(t, artifacts) // nothing published for this hex

	// Pre-stage a verified view into the store's views root, the way a
	// remote metadata delivery would.
	viewRoot := filepath.Join(env.d.Store.Root(), "model-transfer", "views")
	require.NoError(t, os.MkdirAll(viewRoot, 0o755))
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	staging := filepath.Join(t.TempDir(), "staging")
	tree := t.TempDir()
	manifest, err := os.ReadFile(filepath.Join(fixture, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tree, "manifest.json"), manifest, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "config.json"),
		[]byte(`{"architectures":["TinyForCausalLM"]}`), 0o644))
	checkpoint, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"),
		checkpoint, 0o644))
	view, err := metadata.Build(tree, staging, digest, nil)
	require.NoError(t, err)
	final := filepath.Join(viewRoot, view.ID())
	require.NoError(t, os.Rename(staging, final))

	target := env.target("pod-9")
	_, err = env.d.NodePublishVolume(context.Background(), modelTransferRequest(target, map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: digest,
		attrModelTransferDelivery: modelTransferDeliveryMetadata,
	}))
	require.NoError(t, err, "a pre-staged view must mount without a checkpoint")

	// The mount binds the view, and the digest never became published.
	assert.Equal(t, final, env.mounter.mounted[target])
	assert.False(t, env.d.Store.IsPublished(hex),
		"a metadata-only mount must never register as a published tree")

	// The reference stands so Unpublish can release it.
	ref, found, err := env.d.Store.ReadRef("csi-vol-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, hex, ref.Hex)

	_, err = env.d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "csi-vol-1", TargetPath: target,
	})
	require.NoError(t, err)
	_, found, err = env.d.Store.ReadRef("csi-vol-1")
	require.NoError(t, err)
	assert.False(t, found)
	assert.False(t, env.d.Store.IsPublished(hex))
}

// TestNodePublishModelTransferMetadataMapsViewRefusals pins the CSI shape of each
// view refusal: the caller's mistakes are argument errors, the node's state
// is a precondition, and nothing else is smeared across them.
func TestNodePublishModelTransferMetadataMapsViewRefusals(t *testing.T) {
	hex := strings.Repeat("a", 64)
	env := newModelTransferEnv(t, hex)
	digest := "sha256:" + hex

	// Unknown required file: the request named a file no view carries.
	_, err := env.d.NodePublishVolume(context.Background(),
		modelTransferRequest(env.target("pod-a"), map[string]string{
			AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: digest,
			attrModelTransferDelivery:      modelTransferDeliveryMetadata,
			attrModelTransferRequiredFiles: "config.json,weights.bin",
		}))
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// A required file the verified source lacks: the node's state.
	_, err = env.d.NodePublishVolume(context.Background(),
		modelTransferRequest(env.target("pod-b"), map[string]string{
			AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: digest,
			attrModelTransferDelivery:      modelTransferDeliveryMetadata,
			attrModelTransferRequiredFiles: "config.json,generation_config.json",
		}))
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "message: %v", err)
	assert.Contains(t, err.Error(), "generation_config.json",
		"the refusal names the missing file")

	// A stale digest: the artifact resolves elsewhere; the node's state.
	_, err = env.d.NodePublishVolume(context.Background(),
		modelTransferRequest(env.target("pod-c"), map[string]string{
			AttrArtifact: "model", AttrArtifactUID: "uid-ma",
			AttrManifestDigest:        "sha256:" + strings.Repeat("b", 64),
			attrModelTransferDelivery: modelTransferDeliveryMetadata,
		}))
	require.Equal(t, codes.PermissionDenied, status.Code(err),
		"a digest the artifact does not resolve to is an authorization refusal")
}
