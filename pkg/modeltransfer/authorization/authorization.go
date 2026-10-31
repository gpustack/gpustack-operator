// Package authorization decides whether one workload may open a read
// session against one source generation: it authenticates the caller's
// projected ServiceAccount token with a dedicated TokenReview audience, binds
// it to a live Pod, verifies the ModelArtifact in the Pod's own namespace,
// and validates the requested range against the node's verified published
// manifest.
//
// The five verdicts a caller must tell apart stay distinct protocol kinds:
// authentication failure, denied content, stale generation, capacity
// exhaustion, and unavailable source.
package authorization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/artifact"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// Audience is the one audience model-transfer session tokens are minted for and every
// review checks. It is deliberately neither the API server's own audience nor
// the model-manager or peer audience: a token minted for another purpose
// never opens a session.
const Audience = "gpustack-model-transfer"

// resolvedCondition is the ModelArtifact condition a source needs.
const resolvedCondition = "Resolved"

// Reviewer is the API server surface a TokenReview needs. The real client
// and the fakes in tests both satisfy it.
type Reviewer interface {
	Create(ctx context.Context, review *authentication.TokenReview,
		opts meta.CreateOptions) (*authentication.TokenReview, error)
}

// Request is one workload's ask: who it is, what it reads, and from where.
type Request struct {
	// Token is the Pod's projected ServiceAccount token, minted for Audience.
	Token string
	// PodNamespace, PodName and PodUID are the workload identity the caller
	// claims; the review and the live Pod must agree with all three.
	PodNamespace string
	PodName      string
	PodUID       string
	// ArtifactName and ArtifactUID name the ModelArtifact in the Pod's own
	// namespace.
	ArtifactName string
	ArtifactUID  string
	// ManifestDigest is the artifact content address the session is bound to.
	ManifestDigest string
	// SourceGeneration, when positive, is the manifest source generation the
	// caller expects to read from.
	SourceGeneration int64
	// Tensor, Offset and Length name the bytes the session reads first.
	Tensor string
	Offset int64
	Length int64
}

// Result is an admitted session's verified basis.
type Result struct {
	// Pod is the authenticated workload identity.
	Pod PodIdentity
	// DigestHex is the manifest digest's hexadecimal part, the node's content
	// address for the published tree.
	DigestHex string
	// Tensor is the validated descriptor of the requested range.
	Tensor protocol.TensorDescriptor
	// SourceGeneration is the live artifact specification generation.
	SourceGeneration int64
}

// PodIdentity is the workload a token was bound to.
type PodIdentity struct {
	Namespace string
	Name      string
	UID       string
	IP        string
}

// Authorizer checks session requests against the API and the node's store.
type Authorizer struct {
	// Reader reads Pods and ModelArtifacts. It is the API reader rather than
	// an informer cache. Positive verdicts may be reused for CacheTTL.
	Reader ctrlcli.Reader
	// Reviewer performs TokenReviews against Audience.
	Reviewer Reviewer
	// Store is the node's cache, read for the published tree's marker and
	// manifest.
	Store *store.Store
	// Resolver reads a manifest already derived and pinned by the broker.
	Resolver ManifestResolver
	// CacheTTL bounds positive authorization reuse. Zero disables caching.
	CacheTTL      time.Duration
	cacheMu       sync.Mutex
	podCache      map[podCacheKey]podCacheEntry
	artifactCache map[artifactCacheKey]artifactCacheEntry
}

type podCacheKey struct {
	TokenHash            [32]byte
	Namespace, Name, UID string
	Cleanup              bool
}

type podCacheEntry struct {
	Pod     PodIdentity
	Expires time.Time
}

type artifactCacheKey struct {
	Namespace, Name, UID, Digest string
	Generation                   int64
}

type artifactCacheEntry struct {
	Artifact *workercore.ModelArtifact
	Expires  time.Time
}

// ManifestResolver resolves one exact live source identity to its derived manifest.
type ManifestResolver interface {
	ResolveManifest(context.Context, string, string, string, int64) (*protocol.Manifest, error)
}

// AuthorizeSession authenticates the workload, authorizes the artifact, and
// validates the requested range. A nil error is an admission; every refusal
// is a typed protocol error whose kind a caller can branch on.
func (a *Authorizer) AuthorizeSession(ctx context.Context, req Request) (Result, *protocol.Error) {
	pod, err := a.authenticate(ctx, req, false)
	if err != nil {
		return Result{}, err
	}
	artifact, err := a.authorizeArtifact(ctx, req)
	if err != nil {
		return Result{}, err
	}
	tensor, hexDigest, err := a.validateRange(ctx, req, artifact.Generation)
	if err != nil {
		return Result{}, err
	}
	return Result{Pod: pod, DigestHex: hexDigest, Tensor: tensor, SourceGeneration: artifact.Generation}, nil
}

// AuthenticatePod verifies the reviewed token and its live Pod binding without
// authorizing content. It permits an exact owner to close an existing session
// after its artifact or grant disappears, including while its Pod terminates.
// The caller must still match the persisted session owner before releasing it.
func (a *Authorizer) AuthenticatePod(ctx context.Context, req Request) (PodIdentity, *protocol.Error) {
	return a.authenticate(ctx, req, true)
}

// AuthenticateActivePod verifies the token's live workload binding for further
// content reads. Terminating and finished Pods may only use cleanup operations.
func (a *Authorizer) AuthenticateActivePod(ctx context.Context, req Request) (PodIdentity, *protocol.Error) {
	return a.authenticate(ctx, req, false)
}

// AuthorizeMetadata checks a live workload, artifact, and source-node grant
// without requiring a tensor or a native source generation.
func (a *Authorizer) AuthorizeMetadata(ctx context.Context, req Request, nodeName string) *protocol.Error {
	if _, err := a.authenticate(ctx, req, false); err != nil {
		return err
	}
	if _, err := a.authorizeArtifact(ctx, req); err != nil {
		return err
	}
	if nodeName == "" {
		return protocol.New(protocol.KindSourceUnavailable, "source", "source node is unknown")
	}
	node := &core.Node{}
	if err := a.Reader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, node); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "source", "source node cannot be read")
	}
	var bindings workercore.ModelStoreBindingList
	if err := a.Reader.List(ctx, &bindings, ctrlcli.InNamespace(req.PodNamespace)); err != nil {
		return protocol.New(protocol.KindSourceUnavailable, "source", "bindings cannot be read")
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		// A Binding that names no sourceMemory section follows the node's
		// configured capacity, so it still grants source access on nodes it
		// covers; a deleting one grants nothing further.
		if binding.DeletionTimestamp != nil {
			continue
		}
		covered, err := modelstore.BindingCoversNode(ctx, a.Reader, binding, node)
		if err != nil {
			return protocol.New(protocol.KindSourceUnavailable, "source", "source coverage cannot be read")
		}
		if covered {
			return nil
		}
	}
	return protocol.New(protocol.KindDeniedContent, "source", "no live binding grants source access on this node")
}

// authenticate reviews the token against Audience, extracts the bound Pod
// identity from the review's kubernetes.io extra claims, and confirms it
// against the live Pod and its ServiceAccount. New sessions also require a
// nonterminating Pod that has not finished; cleanup grants no further reads.
func (a *Authorizer) authenticate(ctx context.Context, req Request, cleanup bool) (PodIdentity, *protocol.Error) {
	if a.CacheTTL <= 0 {
		return a.authenticateLive(ctx, req, cleanup)
	}
	key := podCacheKey{sha256.Sum256([]byte(req.Token)), req.PodNamespace, req.PodName, req.PodUID, cleanup}
	a.cacheMu.Lock()
	now := time.Now()
	if entry, ok := a.podCache[key]; ok && now.Before(entry.Expires) {
		a.cacheMu.Unlock()
		return entry.Pod, nil
	}
	for key, entry := range a.podCache {
		if !now.Before(entry.Expires) {
			delete(a.podCache, key)
		}
	}
	a.cacheMu.Unlock()
	pod, err := a.authenticateLive(ctx, req, cleanup)
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if err == nil {
		if a.podCache == nil {
			a.podCache = make(map[podCacheKey]podCacheEntry)
		}
		a.podCache[key] = podCacheEntry{pod, now.Add(a.CacheTTL)}
	}
	return pod, err
}

func (a *Authorizer) authenticateLive(ctx context.Context, req Request, cleanup bool) (PodIdentity, *protocol.Error) {
	if a.Reviewer == nil || a.Reader == nil {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"token reviewer and live Pod reader must be configured")
	}
	if req.Token == "" || req.PodNamespace == "" || req.PodName == "" || req.PodUID == "" {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token and complete Pod identity are required")
	}
	review, err := a.Reviewer.Create(ctx, &authentication.TokenReview{
		Spec: authentication.TokenReviewSpec{Token: req.Token, Audiences: []string{Audience}},
	}, meta.CreateOptions{})
	if err != nil {
		log.FromContext(ctx).Error(err, "review model-transfer token")
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token could not be reviewed")
	}
	if review == nil || !review.Status.Authenticated {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token did not authenticate")
	}
	if !contains(review.Status.Audiences, Audience) {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token was not minted for the model-transfer audience")
	}
	bound, ok := podIdentityOf(review)
	if !ok {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token carries no Pod identity")
	}
	if bound.Namespace != req.PodNamespace || bound.Name != req.PodName || bound.UID != req.PodUID {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token's Pod identity does not match the request")
	}

	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: bound.Namespace, Name: bound.Name}}
	if err := a.Reader.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), pod); err != nil {
		if kerrors.IsNotFound(err) {
			return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
				"the token's Pod does not exist")
		}
		log.FromContext(ctx).Error(err, "read model-transfer token Pod")
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token's Pod could not be read")
	}
	if string(pod.UID) != bound.UID {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the Pod's UID changed since the token was issued")
	}
	_, account, accountErr := serviceaccount.SplitUsername(review.Status.User.Username)
	if accountErr != nil || pod.Spec.ServiceAccountName != account {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the token's ServiceAccount does not match the Pod")
	}
	if !cleanup && (pod.DeletionTimestamp != nil || pod.Status.Phase == core.PodSucceeded ||
		pod.Status.Phase == core.PodFailed) {
		return PodIdentity{}, protocol.New(protocol.KindAuthentication, "session",
			"the Pod is terminating or has finished")
	}
	bound.IP = pod.Status.PodIP
	return bound, nil
}

// authorizeArtifact verifies the ModelArtifact in the Pod's own namespace:
// present, the one the request names, a hub artifact, resolved, digesting to
// the requested digest, with a resolved byte total. A digest is a content
// address and proves nothing about access: the artifact in the namespace is
// the access, which is why every rule here is about it.
func (a *Authorizer) authorizeArtifact(ctx context.Context, req Request) (*workercore.ModelArtifact, *protocol.Error) {
	if a.CacheTTL <= 0 {
		return a.authorizeArtifactLive(ctx, req)
	}
	key := artifactCacheKey{req.PodNamespace, req.ArtifactName, req.ArtifactUID, req.ManifestDigest, req.SourceGeneration}
	a.cacheMu.Lock()
	now := time.Now()
	if entry, ok := a.artifactCache[key]; ok && now.Before(entry.Expires) {
		a.cacheMu.Unlock()
		return entry.Artifact.DeepCopy(), nil
	}
	for key, entry := range a.artifactCache {
		if !now.Before(entry.Expires) {
			delete(a.artifactCache, key)
		}
	}
	a.cacheMu.Unlock()
	artifact, err := a.authorizeArtifactLive(ctx, req)
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if err == nil {
		if a.artifactCache == nil {
			a.artifactCache = make(map[artifactCacheKey]artifactCacheEntry)
		}
		a.artifactCache[key] = artifactCacheEntry{artifact.DeepCopy(), now.Add(a.CacheTTL)}
	}
	return artifact, err
}

func (a *Authorizer) authorizeArtifactLive(ctx context.Context, req Request) (*workercore.ModelArtifact, *protocol.Error) {
	deny := func(detail string) *protocol.Error {
		return protocol.New(protocol.KindDeniedContent, "artifact", "%s", detail)
	}
	artifact := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{
		Namespace: req.PodNamespace, Name: req.ArtifactName,
	}}
	if err := a.Reader.Get(ctx, ctrlcli.ObjectKeyFromObject(artifact), artifact); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, deny("the namespace has no such ModelArtifact")
		}
		log.FromContext(ctx).Error(err, "read authorized model artifact")
		return nil, protocol.New(protocol.KindSourceUnavailable, "artifact",
			"the ModelArtifact could not be read")
	}
	switch {
	case string(artifact.UID) != req.ArtifactUID:
		return nil, deny("the ModelArtifact is not the one the request names")
	case artifact.DeletionTimestamp != nil:
		return nil, deny("the ModelArtifact is terminating")
	case artifact.Spec.Source.HuggingFace == nil && artifact.Spec.Source.ModelScope == nil:
		return nil, deny("only a hub artifact is served by a source generation")
	case artifact.Generation < 1 || artifact.Status.ObservedGeneration != artifact.Generation:
		return nil, deny("the ModelArtifact status is not current")
	case !conditionTrue(artifact.Status.Conditions, resolvedCondition) || artifact.Status.Resolved == nil:
		return nil, deny("the ModelArtifact is not resolved; new sessions wait until it is")
	case artifact.Status.Resolved.ManifestDigest != req.ManifestDigest:
		return nil, deny("the ModelArtifact resolves to another digest")
	case artifact.Status.Resolved.SizeBytes <= 0:
		// An unknown size must not become zero-cost memory admission.
		return nil, deny("the ModelArtifact's resolved size is unknown")
	}
	return artifact, nil
}

// validateRange checks the requested tensor range against the verified
// published manifest of the digest's tree: the tree must be published with a
// matching marker, and the range must lie inside the tensor the manifest
// describes.
func (a *Authorizer) validateRange(ctx context.Context, req Request, sourceGeneration int64) (protocol.TensorDescriptor, string, *protocol.Error) {
	if req.SourceGeneration < 0 {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindInvalidDescriptor,
			"generation", "the source generation must not be negative")
	}
	hexDigest := store.HexOf(req.ManifestDigest)
	if hexDigest == "" {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindInvalidDescriptor,
			"digest", "the digest is not a sha256 content address")
	}
	if sourceGeneration < 1 || (req.SourceGeneration > 0 && req.SourceGeneration != sourceGeneration) {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindStaleGeneration,
			"generation", "the artifact source generation does not match the request")
	}
	var manifest *protocol.Manifest
	if a.Resolver != nil {
		var err error
		manifest, err = a.Resolver.ResolveManifest(ctx, req.PodNamespace, req.ArtifactUID, req.ManifestDigest, sourceGeneration)
		if err != nil || manifest == nil {
			return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindSourceUnavailable, "source", "the derived manifest is unavailable")
		}
	} else {
		var err *protocol.Error
		manifest, err = a.buildManifest(ctx, req, sourceGeneration, hexDigest)
		if err != nil {
			return protocol.TensorDescriptor{}, "", err
		}
	}
	tensor, err := manifest.TensorContext(ctx, req.Tensor)
	if err != nil {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindInvalidDescriptor,
			"range", "unknown tensor %q", req.Tensor)
	}
	size, err := tensor.ByteSize()
	if err != nil {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindInvalidDescriptor,
			"range", "the tensor descriptor is invalid: %v", err)
	}
	if !manifest.ValidTensorSpan(ctx, tensor, req.Offset, req.Length) {
		return protocol.TensorDescriptor{}, "", protocol.New(protocol.KindRangeOverflow,
			"range", "the range [%d, +%d) exceeds tensor byte size %d",
			req.Offset, req.Length, size)
	}
	return tensor, hexDigest, nil
}

func (a *Authorizer) buildManifest(ctx context.Context, req Request, sourceGeneration int64, hexDigest string) (*protocol.Manifest, *protocol.Error) {
	if a.Store == nil {
		return nil, protocol.New(protocol.KindSourceUnavailable,
			"source", "no cache store is configured")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		log.FromContext(ctx).Error(err, "create model-transfer cache claim identity")
		return nil, protocol.New(protocol.KindSourceUnavailable,
			"source", "cannot create a cache claim")
	}
	ref := "model-transfer.auth." + hex.EncodeToString(id[:])
	claimed, err := a.Store.Claim(store.Ref{VolumeID: ref, Hex: hexDigest})
	if err != nil || !claimed {
		return nil, protocol.New(protocol.KindSourceUnavailable,
			"source", "cannot claim the verified published tree")
	}
	defer func() {
		if err := a.Store.RemoveRef(ref); err != nil {
			log.FromContext(ctx).Error(err, "release model-transfer authorization claim", "ref", ref)
		}
	}()
	view, err := artifact.Build(ctx, a.Store, req.ArtifactUID, sourceGeneration, req.ManifestDigest)
	if err != nil {
		return nil, protocol.New(protocol.KindSourceUnavailable,
			"source", "the verified transfer manifest could not be derived")
	}
	return view.Manifest, nil
}

// podIdentityOf reads the bound Pod claims and the ServiceAccount namespace
// from the API server's TokenReview result. Legacy unbound tokens are refused.
func podIdentityOf(review *authentication.TokenReview) (PodIdentity, bool) {
	namespace, _, err := serviceaccount.SplitUsername(review.Status.User.Username)
	if err != nil {
		return PodIdentity{}, false
	}
	names := review.Status.User.Extra[serviceaccount.PodNameKey]
	uids := review.Status.User.Extra[serviceaccount.PodUIDKey]
	if len(names) != 1 || len(uids) != 1 || names[0] == "" || uids[0] == "" {
		return PodIdentity{}, false
	}
	return PodIdentity{Namespace: namespace, Name: names[0], UID: uids[0]}, true
}

func conditionTrue(conds []gpustack.Condition, typ string) bool {
	for _, c := range conds {
		if c.Type == typ {
			return c.Status == meta.ConditionTrue
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
