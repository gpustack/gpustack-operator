package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// fakeReviewer answers TokenReviews from a script, recording what was asked.
type fakeReviewer struct {
	answer func(*authentication.TokenReview) (*authentication.TokenReview, error)
	err    error
	asked  []string
}

func (f *fakeReviewer) create(_ context.Context, review *authentication.TokenReview,
	_ meta.CreateOptions,
) (*authentication.TokenReview, error) {
	f.asked = review.Spec.Audiences
	if f.err != nil {
		return nil, f.err
	}
	return f.answer(review)
}

func (f *fakeReviewer) Create(ctx context.Context, review *authentication.TokenReview,
	opts meta.CreateOptions,
) (*authentication.TokenReview, error) {
	return f.create(ctx, review, opts)
}

func authenticatedReview(namespace, name, uid string) *authentication.TokenReview {
	return &authentication.TokenReview{
		Status: authentication.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{Audience},
			User: authentication.UserInfo{
				Username: "system:serviceaccount:" + namespace + ":reader",
				Extra: map[string]authentication.ExtraValue{
					"authentication.kubernetes.io/pod-name": {name},
					"authentication.kubernetes.io/pod-uid":  {uid},
				},
			},
		},
	}
}

func unauthenticatedReview() *authentication.TokenReview {
	return &authentication.TokenReview{Status: authentication.TokenReviewStatus{
		Authenticated: false, Error: "invalid token",
	}}
}

func wrongAudienceReview() *authentication.TokenReview {
	review := authenticatedReview("tenant-a", "engine-1", "pod-uid-1")
	review.Status.Audiences = []string{"gpustack-model-peer"}
	return review
}

// publishFixtureTree publishes the shared golden fixture as a verified tree,
// the reader contract's manifest stored beside it the way materialization does.
func publishFixtureTree(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		// Publish seals the tree read-only; make it removable again.
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)

	fileBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "model-transfer", "contracts",
		"fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	sum := sha256.Sum256(fileBytes)
	fileDigest := "sha256:" + hex.EncodeToString(sum[:])
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{Path: "checkpoint.safetensors", Size: int64(len(fileBytes)), Digest: fileDigest}})
	require.NoError(t, err)
	digest := manifest.Digest
	hexDigest := store.HexOf(digest)

	attempt, err := st.NewAttempt(hexDigest)
	require.NoError(t, err)
	tree := filepath.Join(attempt.Dir(), "tree")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"), fileBytes, 0o644))
	manifestBytes, err := json.Marshal(peer.Listing{Digest: digest, Entries: []peer.ManifestFile{{Path: "checkpoint.safetensors", Size: int64(len(fileBytes)), Digest: fileDigest}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"),
		manifestBytes, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest:         digest,
		SizeBytes:      734,
		ManifestFormat: modelartifact.ManifestHeader,
		Source:         store.SourceHub,
	}))
	return st, hexDigest, digest
}

func runningPod(namespace, name, uid string) *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: namespace, Name: name, UID: types.UID(uid),
	}, Spec: core.PodSpec{ServiceAccountName: "reader"}}
}

func resolvedArtifact(namespace, name, uid, digest string, hub bool,
	resolved bool, size int64,
) *workercore.ModelArtifact {
	ma := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid), Generation: 1},
	}
	if hub {
		ma.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{Repository: "org/" + name}
	}
	if resolved {
		ma.Status.ObservedGeneration = ma.Generation
		ma.Status.Resolved = &workercore.ModelArtifactResolved{
			ManifestDigest: digest,
			SizeBytes:      size,
			ResolvedTime:   meta.Now(),
		}
		ma.Status.Conditions = []gpustack.Condition{{
			Type: "Resolved", Status: meta.ConditionTrue,
		}}
	}
	return ma
}

func defaultObjects(digest string) []ctrlcli.Object {
	return []ctrlcli.Object{
		runningPod("tenant-a", "engine-1", "pod-uid-1"),
		resolvedArtifact("tenant-a", "model", "artifact-uid-1", digest, true, true, 734),
	}
}

func otherDigest() string { return "sha256:" + strings.Repeat("b", 64) }

type env struct {
	store  *store.Store
	hex    string
	digest string
}

func newEnv(t *testing.T) *env {
	st, hexDigest, digest := publishFixtureTree(t)
	return &env{store: st, hex: hexDigest, digest: digest}
}

func (e *env) request() Request {
	return Request{
		Token:          "projected-token",
		PodNamespace:   "tenant-a",
		PodName:        "engine-1",
		PodUID:         "pod-uid-1",
		ArtifactName:   "model",
		ArtifactUID:    "artifact-uid-1",
		ManifestDigest: e.digest,
		Tensor:         "model.norm.weight",
		Offset:         0,
		Length:         12,
	}
}

func authorize(t *testing.T, e *env, reviewer *fakeReviewer,
	objects []ctrlcli.Object, mutate func(*Request),
) (Result, *protocol.Error) {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(objects...).Build()
	auth := &Authorizer{Reader: cli, Reviewer: reviewer, Store: e.store}
	request := e.request()
	if mutate != nil {
		mutate(&request)
	}
	result, err := auth.AuthorizeSession(context.Background(), request)
	return result, err
}

func okReviewer() *fakeReviewer {
	return &fakeReviewer{answer: func(*authentication.TokenReview) (*authentication.TokenReview, error) {
		return authenticatedReview("tenant-a", "engine-1", "pod-uid-1"), nil
	}}
}

func TestAuthorizeMetadataRequiresLiveBindingCoverage(t *testing.T) {
	e := newEnv(t)
	objects := defaultObjects(e.digest)
	objects = append(objects,
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}},
		&workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store-1"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}},
		&workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Namespace: "tenant-a", Name: "binding-1"}, Spec: workercore.ModelStoreBindingSpec{
			StoreRefs:    []workercore.ModelStoreBindingStoreReference{{Name: "store-1"}},
			SourceMemory: &workercore.ModelStoreBindingSourceMemory{Bytes: resource.MustParse("1Gi"), Registrations: 1, Sessions: 1},
		}},
	)
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
	auth := &Authorizer{Reader: reader, Reviewer: okReviewer(), Store: e.store}
	req := e.request()
	require.Nil(t, auth.AuthorizeMetadata(context.Background(), req, "node-1"))
	other := req
	other.PodUID = "another-pod"
	require.NotNil(t, auth.AuthorizeMetadata(context.Background(), other, "node-1"))
	require.NoError(t, reader.Delete(context.Background(), objects[len(objects)-1]))
	verdict := auth.AuthorizeMetadata(context.Background(), req, "node-1")
	require.NotNil(t, verdict)
	assert.Equal(t, protocol.KindDeniedContent, verdict.Kind)
}

func TestAudienceIsDedicated(t *testing.T) {
	// A token minted for the peer feature or the API server itself must not
	// open a model-transfer session, so the audience is this feature's own.
	assert.Equal(t, "gpustack-model-transfer", Audience)
}

func TestAuthorizeSessionHappyPath(t *testing.T) {
	e := newEnv(t)
	result, err := authorize(t, e, okReviewer(), defaultObjects(e.digest), nil)
	require.Nil(t, err)
	assert.Equal(t, "tenant-a", result.Pod.Namespace)
	assert.Equal(t, "engine-1", result.Pod.Name)
	assert.Equal(t, "pod-uid-1", result.Pod.UID)
	assert.Equal(t, e.hex, result.DigestHex)
	assert.Equal(t, "model.norm.weight", result.Tensor.Name)
}

func TestAuthorizeSessionReviewsAgainstDedicatedAudience(t *testing.T) {
	e := newEnv(t)
	reviewer := okReviewer()
	_, err := authorize(t, e, reviewer, defaultObjects(e.digest), nil)
	require.Nil(t, err)
	assert.Equal(t, []string{Audience}, reviewer.asked)
}

func TestAuthenticatePodBoundClaims(t *testing.T) {
	cases := []struct {
		name       string
		username   string
		podNames   []string
		podUIDs    []string
		legacy     bool
		noReader   bool
		nilReview  bool
		emptyToken bool
		wantError  bool
	}{
		{name: "bound token"},
		{name: "legacy tuple is not a bound claim", legacy: true, wantError: true},
		{name: "wrong namespace", username: "system:serviceaccount:tenant-b:reader", wantError: true},
		{name: "wrong service account", username: "system:serviceaccount:tenant-a:other", wantError: true},
		{name: "non service account", username: "admin", wantError: true},
		{name: "wrong Pod name", podNames: []string{"other"}, wantError: true},
		{name: "missing Pod UID", podUIDs: []string{}, wantError: true},
		{name: "ambiguous Pod UID", podUIDs: []string{"pod-uid-1", "other"}, wantError: true},
		{name: "no live reader", noReader: true, wantError: true},
		{name: "nil review", nilReview: true, wantError: true},
		{name: "empty token", emptyToken: true, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review := authenticatedReview("tenant-a", "engine-1", "pod-uid-1")
			if tc.username != "" {
				review.Status.User.Username = tc.username
			}
			if tc.podNames != nil {
				review.Status.User.Extra["authentication.kubernetes.io/pod-name"] = tc.podNames
			}
			if tc.podUIDs != nil {
				review.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = tc.podUIDs
			}
			if tc.legacy {
				review.Status.User.Extra = map[string]authentication.ExtraValue{
					"kubernetes.io": {"tenant-a", "engine-1", "pod-uid-1", "reader"},
				}
			}
			if tc.nilReview {
				review = nil
			}
			auth := &Authorizer{
				Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithObjects(runningPod("tenant-a", "engine-1", "pod-uid-1")).Build(),
				Reviewer: &fakeReviewer{answer: func(*authentication.TokenReview) (*authentication.TokenReview, error) {
					return review, nil
				}},
			}
			if tc.noReader {
				auth.Reader = nil
			}
			req := Request{Token: "projected-token", PodNamespace: "tenant-a", PodName: "engine-1", PodUID: "pod-uid-1"}
			if tc.emptyToken {
				req.Token = ""
			}
			identity, err := auth.AuthenticatePod(context.Background(), req)
			if tc.wantError {
				require.NotNil(t, err)
				assert.Equal(t, protocol.KindAuthentication, err.Kind)
				return
			}
			require.Nil(t, err)
			assert.Equal(t, PodIdentity{Namespace: req.PodNamespace, Name: req.PodName, UID: req.PodUID}, identity)
		})
	}
}

func TestAuthorizeSessionRefusals(t *testing.T) {
	e := newEnv(t)

	terminating := runningPod("tenant-a", "engine-1", "pod-uid-1")
	terminating.DeletionTimestamp = ptr.To(meta.Now())
	terminating.Finalizers = []string{"gpustack.io/protect"}

	unresolvedSizeArtifact := resolvedArtifact("tenant-a", "model", "artifact-uid-1",
		e.digest, true, true, 0)
	deletingArtifact := resolvedArtifact("tenant-a", "model", "artifact-uid-1", e.digest, true, true, 734)
	deletingArtifact.DeletionTimestamp = ptr.To(meta.Now())
	deletingArtifact.Finalizers = []string{"test.gpustack.ai/hold"}

	cases := []struct {
		name     string
		reviewer *fakeReviewer
		objects  []ctrlcli.Object
		mutate   func(*Request)
		wantKind protocol.Kind
	}{
		{
			name: "unauthenticated token",
			reviewer: &fakeReviewer{answer: func(*authentication.TokenReview) (*authentication.TokenReview, error) {
				return unauthenticatedReview(), nil
			}},
			objects:  defaultObjects(e.digest),
			wantKind: protocol.KindAuthentication,
		},
		{
			name: "wrong audience",
			reviewer: &fakeReviewer{answer: func(*authentication.TokenReview) (*authentication.TokenReview, error) {
				return wrongAudienceReview(), nil
			}},
			objects:  defaultObjects(e.digest),
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "token review unavailable",
			reviewer: &fakeReviewer{err: errors.New("apiserver unreachable")},
			objects:  defaultObjects(e.digest),
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "pod uid mismatch",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.PodUID = "another-uid" },
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "pod namespace mismatch",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.PodNamespace = "tenant-b" },
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "pod missing",
			reviewer: okReviewer(),
			objects:  []ctrlcli.Object{defaultObjects(e.digest)[1]},
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "pod terminating",
			reviewer: okReviewer(),
			objects:  []ctrlcli.Object{terminating, defaultObjects(e.digest)[1]},
			wantKind: protocol.KindAuthentication,
		},
		{
			name:     "artifact missing",
			reviewer: okReviewer(),
			objects:  []ctrlcli.Object{defaultObjects(e.digest)[0]},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "artifact terminating",
			reviewer: okReviewer(),
			objects:  []ctrlcli.Object{defaultObjects(e.digest)[0], deletingArtifact},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "artifact uid mismatch",
			reviewer: okReviewer(),
			objects: []ctrlcli.Object{
				defaultObjects(e.digest)[0],
				resolvedArtifact("tenant-a", "model", "another-uid", e.digest, true, true, 734),
			},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "artifact not on a hub",
			reviewer: okReviewer(),
			objects: []ctrlcli.Object{
				defaultObjects(e.digest)[0],
				resolvedArtifact("tenant-a", "model", "artifact-uid-1", e.digest, false, true, 734),
			},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "artifact unresolved",
			reviewer: okReviewer(),
			objects: []ctrlcli.Object{
				defaultObjects(e.digest)[0],
				resolvedArtifact("tenant-a", "model", "artifact-uid-1", e.digest, true, false, 734),
			},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "artifact digest mismatch",
			reviewer: okReviewer(),
			objects: []ctrlcli.Object{
				defaultObjects(e.digest)[0],
				resolvedArtifact("tenant-a", "model", "artifact-uid-1", otherDigest(), true, true, 734),
			},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "unresolved byte total",
			reviewer: okReviewer(),
			objects:  []ctrlcli.Object{defaultObjects(e.digest)[0], unresolvedSizeArtifact},
			wantKind: protocol.KindDeniedContent,
		},
		{
			name:     "range past tensor end",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.Offset, r.Length = 8, 16 },
			wantKind: protocol.KindRangeOverflow,
		},
		{
			name:     "range int64 overflow",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.Offset, r.Length = 4, 1<<63-1 },
			wantKind: protocol.KindRangeOverflow,
		},
		{
			name:     "stale manifest source generation",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.SourceGeneration = 2 },
			wantKind: protocol.KindStaleGeneration,
		},
		{
			name:     "negative source generation",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.SourceGeneration = -1 },
			wantKind: protocol.KindInvalidDescriptor,
		},
		{
			name:     "range on unknown tensor",
			reviewer: okReviewer(),
			objects:  defaultObjects(e.digest),
			mutate:   func(r *Request) { r.Tensor = "nope" },
			wantKind: protocol.KindInvalidDescriptor,
		},
		{
			name:     "digest with no published tree",
			reviewer: okReviewer(),
			objects: []ctrlcli.Object{
				defaultObjects(e.digest)[0],
				resolvedArtifact("tenant-a", "model", "artifact-uid-1", otherDigest(), true, true, 734),
			},
			mutate:   func(r *Request) { r.ManifestDigest = otherDigest() },
			wantKind: protocol.KindSourceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := authorize(t, e, tc.reviewer, tc.objects, tc.mutate)
			require.NotNil(t, err, "the request must be refused")
			assert.Equal(t, tc.wantKind, err.Kind, "reason: %s", err.Error())
		})
	}
}

func TestRefusalMessageNamesNoTenantSecret(t *testing.T) {
	// A cross-tenant refusal repeats the rule, not the other tenant's
	// content or the token it presented.
	e := newEnv(t)
	_, err := authorize(t, e, okReviewer(), []ctrlcli.Object{
		defaultObjects(e.digest)[0],
		resolvedArtifact("tenant-a", "model", "artifact-uid-1", otherDigest(), true, true, 734),
	}, nil)
	require.NotNil(t, err)
	assert.NotContains(t, err.Error(), "projected-token")
}

func TestAuthorizationHidesInfrastructureErrors(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reviewFailure   bool
		artifactFailure bool
	}{
		{name: "token review", reviewFailure: true},
		{name: "pod read"},
		{name: "artifact read", artifactFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			internal := errors.New("https://private-control-plane:6443/internal/request timed out")
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			reader := ctrlinterceptor.NewClient(cli, ctrlinterceptor.Funcs{
				Get: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
					return internal
				},
			})
			reviewer := okReviewer()
			if tc.reviewFailure {
				reviewer.err = internal
			}
			auth := &Authorizer{Reader: reader, Reviewer: reviewer}
			req := Request{Token: "token", PodNamespace: "tenant-a", PodName: "engine-1", PodUID: "pod-uid-1"}
			var refusal *protocol.Error
			if tc.artifactFailure {
				_, refusal = auth.authorizeArtifactLive(context.Background(), req)
			} else {
				_, refusal = auth.AuthenticatePod(context.Background(), req)
			}
			require.NotNil(t, refusal)
			assert.NotContains(t, refusal.Error(), "private-control-plane")
			assert.NotContains(t, refusal.Error(), "internal/request")
		})
	}
}
