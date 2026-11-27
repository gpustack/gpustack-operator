package worker

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kernelkey"
)

const (
	modelKernelArtifactIdentityMessage = "a ModelKernelArtifact is an immutable identity, so its spec cannot " +
		"change after creation: publish a new generation artifact instead"

	modelKernelArtifactMaxKernelKeys = 128
	modelKernelArtifactMinFileCount  = 2
	modelKernelArtifactMaxNameLen    = 253
	modelKernelArtifactMaxUIDLen     = 128
	modelKernelArtifactMaxVersionLen = 64
)

// modelKernelArtifactCommitPattern matches a full 40-character lowercase hexadecimal Git commit SHA.
var modelKernelArtifactCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// ModelKernelArtifactWebhook validates ModelKernelArtifacts and guards status subresource writes.
//
// Spec is immutable after creation. Source nodeStore is restricted to the operator worker identity
// on CREATE so tenants cannot claim arbitrary NodeModelStore digests or flood the node pin list.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modelkernelartifacts",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:subResources=["status"]
type ModelKernelArtifactWebhook struct {
	// worker is the username the worker's own requests carry, the only writer of status and
	// nodeStore-sourced kernel artifacts.
	worker string
}

func (r *ModelKernelArtifactWebhook) SetupWebhook(ctx context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	worker, err := selfUsername(ctx)
	if err != nil {
		return nil, fmt.Errorf("learn the worker's identity for the ModelKernelArtifact rules: %w", err)
	}
	r.worker = worker

	return &workercore.ModelKernelArtifact{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelKernelArtifactWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelKernelArtifactWebhook)(nil)
)

// ReceiveDeletionUpdate keeps the status guard and spec immutability in force while a referenced
// artifact stays Terminating under its protection finalizer.
func (*ModelKernelArtifactWebhook) ReceiveDeletionUpdate() {}

func (r *ModelKernelArtifactWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	mka := obj.(*workercore.ModelKernelArtifact)
	if errs := r.validateModelKernelArtifact(ctx, mka, nil); len(errs) > 0 {
		return nil, kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelKernelArtifact").GroupKind(), mka.Name, errs,
		)
	}

	return nil, nil
}

func (r *ModelKernelArtifactWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	mka, old := newObj.(*workercore.ModelKernelArtifact), oldObj.(*workercore.ModelKernelArtifact)
	if isStatusRequest(ctx) {
		return nil, r.validateModelKernelArtifactStatus(ctx, mka)
	}
	if errs := r.validateModelKernelArtifact(ctx, mka, old); len(errs) > 0 {
		return nil, kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelKernelArtifact").GroupKind(), mka.Name, errs,
		)
	}

	return nil, nil
}

func (*ModelKernelArtifactWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (r *ModelKernelArtifactWebhook) validateModelKernelArtifact(
	ctx context.Context, mka, old *workercore.ModelKernelArtifact,
) field.ErrorList {
	specPath := field.NewPath("spec")
	if old != nil {
		if !kubemeta.DeepEqual(mka.Spec, old.Spec) {
			return field.ErrorList{field.Invalid(specPath, mka.Spec, modelKernelArtifactIdentityMessage)}
		}
		// An unchanged spec was already validated at creation; metadata and finalizer updates must
		// remain allowed without stranding stored artifacts.
		return nil
	}

	errs := r.validateModelKernelArtifactSource(ctx, &mka.Spec.Source, specPath.Child("source"))
	errs = append(errs, validateModelKernelArtifactCompatibility(&mka.Spec.Compatibility, specPath.Child("compatibility"))...)
	return errs
}

func (r *ModelKernelArtifactWebhook) validateModelKernelArtifactSource(
	ctx context.Context, source *workercore.ModelKernelArtifactSource, sourcePath *field.Path,
) field.ErrorList {
	var members []string
	if source.NodeStore != nil {
		members = append(members, "nodeStore")
	}
	if source.HuggingFace != nil {
		members = append(members, "huggingFace")
	}
	if source.Image != nil {
		members = append(members, "image")
	}
	if len(members) != 1 {
		return field.ErrorList{field.Invalid(sourcePath, members,
			"exactly one of nodeStore, huggingFace or image is required")}
	}

	switch {
	case source.NodeStore != nil:
		return r.validateModelKernelArtifactNodeStore(ctx, source.NodeStore, sourcePath.Child("nodeStore"))
	case source.HuggingFace != nil:
		return validateModelKernelArtifactHuggingFace(source.HuggingFace, sourcePath.Child("huggingFace"))
	default:
		errs := validateModelArtifactImage(source.Image, sourcePath.Child("image"))
		return append(errs, validateModelArtifactImageVolume(sourcePath.Child("image"))...)
	}
}

func (r *ModelKernelArtifactWebhook) validateModelKernelArtifactNodeStore(
	ctx context.Context, ns *workercore.ModelKernelArtifactNodeStoreSource, path *field.Path,
) field.ErrorList {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil || r.worker == "" || req.UserInfo.Username != r.worker {
		username := ""
		if err == nil {
			username = req.UserInfo.Username
		}
		return field.ErrorList{field.Forbidden(path, fmt.Sprintf(
			"%s: only the operator worker may create a ModelKernelArtifact with spec.source.nodeStore "+
				"(%q is not the worker)",
			workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden, username,
		))}
	}

	var errs field.ErrorList
	if !modelArtifactDigestPattern.MatchString(ns.ManifestDigest) {
		errs = append(errs, field.Invalid(path.Child("manifestDigest"), ns.ManifestDigest,
			`must be "sha256:" and 64 lowercase hex`))
	}
	if !modelArtifactDigestPattern.MatchString(ns.DispatchManifestDigest) {
		errs = append(errs, field.Invalid(path.Child("dispatchManifestDigest"), ns.DispatchManifestDigest,
			`must be "sha256:" and 64 lowercase hex`))
	}
	if ns.SizeBytes <= 0 {
		errs = append(errs, field.Invalid(path.Child("sizeBytes"), ns.SizeBytes,
			"must be greater than 0"))
	}
	if ns.FileCount < modelKernelArtifactMinFileCount {
		errs = append(errs, field.Invalid(path.Child("fileCount"), ns.FileCount,
			fmt.Sprintf("must be at least %d (dispatch-manifest.json plus at least one kernel file)",
				modelKernelArtifactMinFileCount)))
	}
	seedNode := strings.TrimSpace(ns.SeedNode)
	switch {
	case seedNode == "":
		errs = append(errs, field.Required(path.Child("seedNode"),
			"must name the seed node that published the kernel tree"))
	case len(ns.SeedNode) > modelKernelArtifactMaxNameLen || seedNode != ns.SeedNode:
		errs = append(errs, field.Invalid(path.Child("seedNode"), ns.SeedNode,
			fmt.Sprintf("must be a non-blank node name of at most %d characters without surrounding whitespace",
				modelKernelArtifactMaxNameLen)))
	}

	forgerRefPath := path.Child("forgerRef")
	forgerName := strings.TrimSpace(ns.ForgerRef.Name)
	switch {
	case forgerName == "":
		errs = append(errs, field.Required(forgerRefPath.Child("name"),
			"must name the owning ModelDeploymentForger in this namespace"))
	case len(ns.ForgerRef.Name) > modelKernelArtifactMaxNameLen || forgerName != ns.ForgerRef.Name:
		errs = append(errs, field.Invalid(forgerRefPath.Child("name"), ns.ForgerRef.Name,
			fmt.Sprintf("must be at most %d characters without surrounding whitespace",
				modelKernelArtifactMaxNameLen)))
	}
	forgerUID := strings.TrimSpace(ns.ForgerRef.UID)
	switch {
	case forgerUID == "":
		errs = append(errs, field.Required(forgerRefPath.Child("uid"),
			"must record the UID of the owning ModelDeploymentForger"))
	case len(ns.ForgerRef.UID) > modelKernelArtifactMaxUIDLen || forgerUID != ns.ForgerRef.UID:
		errs = append(errs, field.Invalid(forgerRefPath.Child("uid"), ns.ForgerRef.UID,
			fmt.Sprintf("must be at most %d characters without surrounding whitespace",
				modelKernelArtifactMaxUIDLen)))
	}

	return errs
}

func validateModelKernelArtifactHuggingFace(
	hub *workercore.ModelArtifactHubSource, path *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	parts := strings.Split(hub.Repository, "/")
	if len(parts) > 2 || slices.ContainsFunc(parts, func(p string) bool {
		return !huggingFaceRepositoryPartPattern.MatchString(p) || strings.Contains(p, "--") || strings.Contains(p, "..")
	}) {
		errs = append(errs, field.Invalid(path.Child("repository"), hub.Repository,
			`must be "owner/name" or a bare name, each made of letters, digits, "-", "_" and ".", `+
				`at most 96 characters, not starting or ending with "-" or ".", and without "--" or ".."`))
	}

	switch {
	case hub.Revision == "":
		errs = append(errs, field.Required(path.Child("revision"),
			"a full 40-character lowercase hexadecimal commit SHA is required"))
	case !modelKernelArtifactCommitPattern.MatchString(hub.Revision):
		errs = append(errs, field.Invalid(path.Child("revision"), hub.Revision,
			"must be a full 40-character lowercase hexadecimal commit SHA; branches and tags are not allowed"))
	}

	return errs
}

func validateModelKernelArtifactCompatibility(
	compat *workercore.ModelKernelArtifactCompatibility, path *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	engineValid := false
	switch compat.Engine {
	case workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang:
		engineValid = true
	case "":
		errs = append(errs, field.Required(path.Child("engine"),
			"must be vLLM or SGLang"))
	default:
		errs = append(errs, field.Invalid(path.Child("engine"), compat.Engine,
			"must be vLLM or SGLang"))
	}

	versionValid := false
	switch {
	case strings.TrimSpace(compat.EngineVersion) == "":
		errs = append(errs, field.Required(path.Child("engineVersion"),
			"must be a normalized inference engine version"))
	case len(compat.EngineVersion) > modelKernelArtifactMaxVersionLen:
		errs = append(errs, field.Invalid(path.Child("engineVersion"), compat.EngineVersion,
			fmt.Sprintf("must be at most %d characters", modelKernelArtifactMaxVersionLen)))
	default:
		normVer, err := kernelkey.NormalizeEngineVersion(compat.EngineVersion)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(path.Child("engineVersion"), compat.EngineVersion, err.Error()))
		case compat.EngineVersion != normVer:
			errs = append(errs, field.Invalid(path.Child("engineVersion"), compat.EngineVersion,
				fmt.Sprintf("must be normalized engineVersion %q", normVer)))
		default:
			versionValid = true
		}
	}

	manufacturerValid := false
	if strings.TrimSpace(compat.Manufacturer) == "" {
		errs = append(errs, field.Required(path.Child("manufacturer"),
			"must be a canonical lowercase accelerator manufacturer"))
	} else {
		normManu, err := kernelkey.NormalizeManufacturer(compat.Manufacturer)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(path.Child("manufacturer"), compat.Manufacturer, err.Error()))
		case compat.Manufacturer != normManu:
			errs = append(errs, field.Invalid(path.Child("manufacturer"), compat.Manufacturer,
				fmt.Sprintf("must be canonical lowercase %q", normManu)))
		default:
			manufacturerValid = true
		}
	}

	archValid := false
	if strings.TrimSpace(compat.Architecture) == "" {
		errs = append(errs, field.Required(path.Child("architecture"),
			"must be a canonical accelerator architecture"))
	} else if manufacturerValid {
		normArch, err := kernelkey.NormalizeArch(compat.Manufacturer, compat.Architecture)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(path.Child("architecture"), compat.Architecture, err.Error()))
		case compat.Architecture != normArch:
			errs = append(errs, field.Invalid(path.Child("architecture"), compat.Architecture,
				fmt.Sprintf("must be canonical %q", normArch)))
		default:
			archValid = true
		}
	}

	keysPath := path.Child("kernelKeys")
	switch {
	case len(compat.KernelKeys) == 0:
		errs = append(errs, field.Required(keysPath, "at least one canonical LocalKernelKey is required"))
		return errs
	case len(compat.KernelKeys) > modelKernelArtifactMaxKernelKeys:
		errs = append(errs, field.TooMany(keysPath, len(compat.KernelKeys), modelKernelArtifactMaxKernelKeys))
	}

	headerValid := engineValid && versionValid && manufacturerValid && archValid
	target := kernelkey.CompatibilityTarget{
		Engine:        compat.Engine,
		EngineVersion: compat.EngineVersion,
		Manufacturer:  compat.Manufacturer,
		Arch:          compat.Architecture,
	}

	seen := sets.New[string]()
	for i, raw := range compat.KernelKeys {
		keyPath := keysPath.Index(i)
		if seen.Has(raw) {
			errs = append(errs, field.Duplicate(keyPath, raw))
			continue
		}
		seen.Insert(raw)

		parsed, err := kernelkey.Parse(raw)
		if err != nil {
			errs = append(errs, field.Invalid(keyPath, raw, err.Error()))
			continue
		}
		if headerValid {
			if err := parsed.MatchesTarget(target); err != nil {
				errs = append(errs, field.Invalid(keyPath, raw, err.Error()))
			}
		}
	}

	return errs
}
