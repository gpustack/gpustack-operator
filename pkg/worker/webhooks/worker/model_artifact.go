package worker

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"unicode"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
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
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="modelartifacts",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelArtifactWebhook struct{}

func (*ModelArtifactWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	return &workercore.ModelArtifact{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelArtifactWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*ModelArtifactWebhook)(nil)
)

// ModelArtifactDefaultRevision is the revision a hub source names when it names none, the default
// branch of a Hugging Face repository.
const ModelArtifactDefaultRevision = "main"

// Default fills an unset hub revision with "main", on update as well as on creation: a full
// replace that omits the revision would otherwise read as a change from "main" to nothing and be
// refused as a spec edit.
func (*ModelArtifactWebhook) Default(_ context.Context, obj runtime.Object) error {
	ma := obj.(*workercore.ModelArtifact)
	if hub := ma.Spec.Source.HuggingFace; hub != nil && hub.Revision == "" {
		hub.Revision = ModelArtifactDefaultRevision
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

func (*ModelArtifactWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	ma, old := newObj.(*workercore.ModelArtifact), oldObj.(*workercore.ModelArtifact)
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

// modelArtifactModelScopeMessage is why the reserved member is refused, and what opening it needs.
const modelArtifactModelScopeMessage = "ModelScope is not accepted in this version. Opening it needs " +
	"branch resolution cross-checked against git, a file listing that re-lists per directory where " +
	"the API silently truncates, errors classified by the envelope code, and an engine runner whose " +
	"ModelScope SDK accepts a commit as the revision"

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
	if len(members) != 1 {
		return field.ErrorList{field.Invalid(sourcePath, members,
			"exactly one of huggingFace or persistentVolumeClaim is required")}
	}

	switch {
	case source.ModelScope != nil:
		return field.ErrorList{field.Forbidden(sourcePath.Child("modelScope"), modelArtifactModelScopeMessage)}
	case source.HuggingFace != nil:
		return validateModelArtifactHuggingFace(source.HuggingFace, sourcePath.Child("huggingFace"))
	default:
		return validateModelArtifactClaim(source.PersistentVolumeClaim, sourcePath.Child("persistentVolumeClaim"))
	}
}

func validateModelArtifactHuggingFace(hub *workercore.ModelArtifactHubSource, path *field.Path) field.ErrorList {
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
