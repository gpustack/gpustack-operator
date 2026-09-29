package worker

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/webhook"
)

// ModelArtifactWebhook defaults and validates ModelArtifacts.
//
// EVERY RULE IS ANSWERED FROM THE OBJECT ALONE. The Secret and the claim an artifact names are not
// read here: they may be created after the artifact, and a refusal that depends on creation order
// is one a declarative apply cannot satisfy. Their absence is reported in the artifact's status.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modelartifacts",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:subResources=["status"]
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="modelartifacts",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelArtifactWebhook struct {
	// worker is the username the worker's own requests carry, the only writer of status.
	worker string
}

func (r *ModelArtifactWebhook) SetupWebhook(ctx context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	worker, err := selfUsername(ctx)
	if err != nil {
		return nil, fmt.Errorf("learn the worker's identity for the ModelArtifact status rule: %w", err)
	}
	r.worker = worker

	return &workercore.ModelArtifact{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelArtifactWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*ModelArtifactWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelArtifactWebhook)(nil)
)

// ReceiveDeletionUpdate keeps the rules in force on a Terminating artifact. The status rule must
// hold there too, since a referenced artifact stays Terminating, and mountable, until its last
// consumer goes; every other rule an update reaches compares the two objects it is handed, and
// defaulting reads nothing else.
func (*ModelArtifactWebhook) ReceiveDeletionUpdate() {}

// ModelArtifactDefaultRevision is the revision a Hugging Face source names when it names none,
// the default branch of a Hugging Face repository.
const ModelArtifactDefaultRevision = "main"

// ModelArtifactDefaultModelScopeRevision is the revision a ModelScope source names when it names
// none, the default branch of a ModelScope repository.
const ModelArtifactDefaultModelScopeRevision = "master"

// Default fills an unset hub revision with the hub's default branch, on update as well as on
// creation: a full replace that omits the revision would otherwise read as a change to nothing
// and be refused as a spec edit.
func (*ModelArtifactWebhook) Default(_ context.Context, obj runtime.Object) error {
	ma := obj.(*workercore.ModelArtifact)
	if hub := ma.Spec.Source.HuggingFace; hub != nil && hub.Revision == "" {
		hub.Revision = ModelArtifactDefaultRevision
	}
	if hub := ma.Spec.Source.ModelScope; hub != nil && hub.Revision == "" {
		hub.Revision = ModelArtifactDefaultModelScopeRevision
	}

	return nil
}

func (*ModelArtifactWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	ma := obj.(*workercore.ModelArtifact)
	if errs := validateModelArtifact(ma, nil); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.SchemeGroupVersionKind("ModelArtifact").GroupKind(), ma.Name, errs)
	}

	return nil, nil
}

func (r *ModelArtifactWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	ma, old := newObj.(*workercore.ModelArtifact), oldObj.(*workercore.ModelArtifact)
	if isStatusRequest(ctx) {
		return nil, r.validateModelArtifactStatus(ctx, ma)
	}
	if errs := validateModelArtifact(ma, old); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.SchemeGroupVersionKind("ModelArtifact").GroupKind(), ma.Name, errs)
	}

	return nil, nil
}

func (*ModelArtifactWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// modelArtifactIdentityMessage is why the whole spec is frozen.
const modelArtifactIdentityMessage = "a ModelArtifact is an identity, so its spec cannot change after " +
	"creation: a different source or revision is a different artifact, created rather than edited"

// huggingFaceRepositoryPartPattern is one part of a Hugging Face repository id, as the Hub names
// them: letters, digits, "-", "_" and ".", not starting or ending with "-" or ".", at most 96.
var huggingFaceRepositoryPartPattern = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_.-]{0,94}[A-Za-z0-9_])?$`)

func validateModelArtifact(ma, old *workercore.ModelArtifact) field.ErrorList {
	specPath := field.NewPath("spec")
	if old != nil {
		if !kubemeta.DeepEqual(ma.Spec, old.Spec) {
			return field.ErrorList{field.Invalid(specPath, ma.Spec, modelArtifactIdentityMessage)}
		}
		// An unchanged spec was judged when it was stored. Judging it again would strand an object
		// stored before a later rule: no finalizer or label could ever be written to it.
		return nil
	}

	sourcePath := specPath.Child("source")
	source := ma.Spec.Source
	var members []string
	if source.HuggingFace != nil {
		members = append(members, "huggingFace")
	}
	if source.ModelScope != nil {
		members = append(members, "modelScope")
	}
	if source.PersistentVolumeClaim != nil {
		members = append(members, "persistentVolumeClaim")
	}
	if source.Image != nil {
		members = append(members, "image")
	}
	if len(members) != 1 {
		return field.ErrorList{field.Invalid(sourcePath, members,
			"exactly one of huggingFace, modelScope, persistentVolumeClaim or image is required")}
	}

	var errs field.ErrorList
	switch {
	case source.HuggingFace != nil:
		errs = validateModelArtifactHub(source.HuggingFace, sourcePath.Child("huggingFace"))
	case source.ModelScope != nil:
		errs = validateModelArtifactHub(source.ModelScope, sourcePath.Child("modelScope"))
	case source.Image != nil:
		errs = validateModelArtifactImage(source.Image, sourcePath.Child("image"))
		errs = append(errs, validateModelArtifactImageVolume(sourcePath.Child("image"))...)
	default:
		errs = validateModelArtifactClaim(source.PersistentVolumeClaim, sourcePath.Child("persistentVolumeClaim"))
	}

	return append(errs, validateModelArtifactPatterns(&ma.Spec, specPath,
		source.HuggingFace != nil || source.ModelScope != nil, source.Image != nil)...)
}

// modelArtifactMaxPatterns and modelArtifactMaxPatternLength bound a list of patterns and each of
// its patterns; the schema bounds the count as well, and cannot bound an item.
const (
	modelArtifactMaxPatterns      = 32
	modelArtifactMaxPatternLength = 256
)

// validateModelArtifactPatterns accepts allow and ignore patterns on a hub source only: every
// other source's content is the user's and is mounted whole.
func validateModelArtifactPatterns(spec *workercore.ModelArtifactSpec, specPath *field.Path, hub, image bool) field.ErrorList {
	// whole is what the refusal says is mounted whole instead of being selected from.
	whole := "a claim's directory is mounted whole"
	if image {
		whole = "an image is mounted whole"
	}
	var errs field.ErrorList
	for _, list := range []struct {
		name     string
		patterns []string
	}{
		{"allowPatterns", spec.AllowPatterns},
		{"ignorePatterns", spec.IgnorePatterns},
	} {
		path := specPath.Child(list.name)
		switch {
		case len(list.patterns) == 0:
			continue
		case !hub:
			errs = append(errs, field.Forbidden(path,
				"patterns select files of a hub source; "+whole))
			continue
		case len(list.patterns) > modelArtifactMaxPatterns:
			errs = append(errs, field.TooMany(path, len(list.patterns), modelArtifactMaxPatterns))
			continue
		}
		for i, p := range list.patterns {
			if p == "" || len(p) > modelArtifactMaxPatternLength ||
				strings.ContainsFunc(p, unicode.IsControl) {
				errs = append(errs, field.Invalid(path.Index(i), p, fmt.Sprintf(
					"must be 1 to %d characters without control characters", modelArtifactMaxPatternLength)))
			}
		}
	}

	return errs
}

// validateModelArtifactHub validates a hub source's shape — the rules the two hubs share: the
// repository id and the revision's characters. ModelScope ids and Hugging Face ids are written
// the same way, so one validator serves both; what resolves a revision differs and lives in the
// client, not here.
func validateModelArtifactHub(hub *workercore.ModelArtifactHubSource, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	parts := strings.Split(hub.Repository, "/")
	if len(parts) > 2 || slices.ContainsFunc(parts, func(p string) bool {
		return !huggingFaceRepositoryPartPattern.MatchString(p) || strings.Contains(p, "--") || strings.Contains(p, "..")
	}) {
		errs = append(errs, field.Invalid(path.Child("repository"), hub.Repository,
			`must be "owner/name" or a bare name, each made of letters, digits, "-", "_" and ".", `+
				`at most 96 characters, not starting or ending with "-" or ".", and without "--" or ".."`))
	}

	// Default fills an empty revision on creation; one still empty here reached validation some
	// other way, and resolving "" would ask the Hub for nothing.
	revision := hub.Revision
	switch {
	case revision == "":
		errs = append(errs, field.Required(path.Child("revision"), "a branch, a tag or a commit"))
	case strings.ContainsFunc(revision, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		errs = append(errs, field.Invalid(path.Child("revision"), revision,
			"must not contain whitespace or control characters"))
	}

	return errs
}

func validateModelArtifactClaim(claim *workercore.ModelArtifactPersistentVolumeClaimSource, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if claim.ClaimName == "" {
		errs = append(errs, field.Required(path.Child("claimName"), "the claim in this namespace holding the weights"))
	}
	// The schema refuses an absolute path; a ".." element needs a negative lookahead its patterns do
	// not have, so this half is here.
	if strings.HasPrefix(claim.Path, "/") || slices.Contains(strings.Split(claim.Path, "/"), "..") {
		errs = append(errs, field.Invalid(path.Child("path"), claim.Path,
			`must be relative to the volume's root and must not contain a ".." element`))
	}

	return errs
}

// modelArtifactClusterVersion is the Kubernetes version the capability rules read. It is a
// variable so a test can choose a version without a cluster: the snapshot's Configure ignores
// later calls, so a test cannot re-point the snapshot itself.
var modelArtifactClusterVersion = func() kubediscovery.Version {
	return system.LoopbackKubeVersion.Get()
}

// validateModelArtifactImageVolume refuses an image source where the cluster cannot serve image
// volumes. The ImageVolume feature is on by default from Kubernetes 1.35; an apiserver that does
// not know the volumes[].image field drops it without an error, so the weights would be silently
// absent. It reads the startup version snapshot, and a version it cannot read refuses: a version
// nobody can read must not unlock the capability.
func validateModelArtifactImageVolume(path *field.Path) field.ErrorList {
	version := modelArtifactClusterVersion()
	if kubediscovery.SupportsFeature(&version, kubediscovery.FeatureImageVolume) {
		return nil
	}

	return field.ErrorList{field.Forbidden(path, fmt.Sprintf(
		"this cluster's Kubernetes version %q does not serve image volumes: the ImageVolume feature is "+
			"on by default from 1.35, and 1.33 or 1.34 would need the gate opened on the apiserver, which "+
			"this version does not offer; deliver the weights from a hub or a PersistentVolumeClaim, or "+
			"upgrade the cluster", version.GitVersion))}
}

// modelArtifactDigestPattern is the digest half of a pinned image reference: "sha256:" and 64
// lowercase hex.
var modelArtifactDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// validateModelArtifactImage refuses a reference that does not pin a digest. A tag is mutable, so
// one artifact could deliver different weights on different pulls, and the identity a frozen
// reference pins would be nothing.
func validateModelArtifactImage(image *workercore.ModelArtifactImageSource, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	reference := image.Reference
	ref, digest, pinned := strings.Cut(reference, "@")
	switch {
	case !pinned:
		errs = append(errs, field.Invalid(path.Child("reference"), reference,
			"must pin the image to its digest: registry/repository@sha256:<64 hex>; a tag is mutable, "+
				"so one artifact could deliver different weights on different pulls"))
	case !modelArtifactDigestPattern.MatchString(digest):
		errs = append(errs, field.Invalid(path.Child("reference"), reference,
			`must pin the digest as "sha256:" and 64 lowercase hex`))
	case ref == "" || strings.ContainsAny(ref, "@ \t\n\r"):
		errs = append(errs, field.Invalid(path.Child("reference"), reference,
			"must be one image reference without whitespace, followed by @ and the digest"))
	}

	return errs
}
