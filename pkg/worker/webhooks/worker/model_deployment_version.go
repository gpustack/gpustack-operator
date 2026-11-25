package worker

import (
	"context"
	"encoding/json"
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

const (
	modelDeploymentVLLMRouterKVAwareMinVersion = "0.31.0"
	modelDeploymentVLLMAscendV2MinVersion      = inject.VLLMAscendMooncakeV2MinVersion
	modelDeploymentVLLMPrefixCachingHashAlgo   = inject.VLLMPrefixCachingHashAlgoArg
	modelDeploymentVLLMKVEventsIntBlockHashEnv = inject.VLLMKVEventsUseIntBlockHashesEnv
)

// oldModelDeploymentFromAdmissionContext decodes the stored ModelDeployment on UPDATE admission
// requests so engine version defaulting only backfills when an image changes. Missing admission
// context and non-UPDATE operations return isUpdate=false so CREATE defaulting runs normally.
func oldModelDeploymentFromAdmissionContext(
	ctx context.Context,
) (*workercore.ModelDeployment, bool, error) {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil || req.Operation != admissionv1.Update {
		return nil, false, nil //nolint:nilerr // Direct Default() calls outside admission carry no request.
	}
	if len(req.OldObject.Raw) == 0 {
		return nil, true, nil
	}
	old := new(workercore.ModelDeployment)
	if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
		return nil, true, fmt.Errorf("unmarshal old ModelDeployment from admission request: %w", err)
	}
	return old, true, nil
}

// defaultModelDeploymentEngineVersions backfills empty spec.engine.version and
// spec.roles[*].engineVersion from standard gpustack/runner image tags. On UPDATE it backfills
// only when the corresponding image changed, preserving legacy empty-version CRs on unrelated
// edits such as scaling or pausing. User-supplied versions are never overwritten.
func defaultModelDeploymentEngineVersions(old, md *workercore.ModelDeployment, isUpdate bool) {
	if !isUpdate {
		if md.Spec.Engine.Version == "" {
			if ver, ok := inject.ParseRunnerEngineVersion(md.Spec.Engine.Image, md.Spec.Engine.Name); ok {
				md.Spec.Engine.Version = ver
			}
		}
		for i := range md.Spec.Roles {
			role := &md.Spec.Roles[i]
			if role.EngineVersion != "" || role.Image == "" {
				continue
			}
			if ver, ok := inject.ParseRunnerEngineVersion(role.Image, md.Spec.Engine.Name); ok {
				role.EngineVersion = ver
			}
		}
		return
	}

	if old == nil {
		return
	}
	if md.Spec.Engine.Version == "" && md.Spec.Engine.Image != old.Spec.Engine.Image {
		if ver, ok := inject.ParseRunnerEngineVersion(md.Spec.Engine.Image, md.Spec.Engine.Name); ok {
			md.Spec.Engine.Version = ver
		}
	}

	oldRoles := make(map[string]*workercore.ModelDeploymentRole, len(old.Spec.Roles))
	for i := range old.Spec.Roles {
		oldRoles[old.Spec.Roles[i].Name] = &old.Spec.Roles[i]
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.EngineVersion != "" || role.Image == "" {
			continue
		}
		prev := oldRoles[role.Name]
		if prev != nil && prev.Image == role.Image {
			continue
		}
		if ver, ok := inject.ParseRunnerEngineVersion(role.Image, md.Spec.Engine.Name); ok {
			role.EngineVersion = ver
		}
	}
}

func modelDeploymentEffectiveEngineVersion(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) string {
	if role.EngineVersion != "" {
		return role.EngineVersion
	}
	return md.Spec.Engine.Version
}

func modelDeploymentEffectiveEngineImage(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) string {
	if role.Image != "" {
		return role.Image
	}
	return md.Spec.Engine.Image
}

func modelDeploymentEngineVersionInputsChanged(old, md *workercore.ModelDeployment) bool {
	if old == nil {
		return true
	}
	if md.Spec.Engine.Image != old.Spec.Engine.Image ||
		md.Spec.Engine.Version != old.Spec.Engine.Version {
		return true
	}

	oldRoles := make(map[string]*workercore.ModelDeploymentRole, len(old.Spec.Roles))
	for i := range old.Spec.Roles {
		oldRoles[old.Spec.Roles[i].Name] = &old.Spec.Roles[i]
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		prev, ok := oldRoles[role.Name]
		if !ok {
			if len(role.Command) == 0 || role.Image != "" || role.EngineVersion != "" {
				return true
			}
			continue
		}
		if role.Image != prev.Image || role.EngineVersion != prev.EngineVersion {
			return true
		}
		if len(prev.Command) > 0 && len(role.Command) == 0 {
			return true
		}
	}
	return false
}

// validateModelDeploymentEngineVersions checks engine version syntax, enforces resolved versions
// on CREATE and image/version UPDATEs for managed roles, and refuses disagreements between
// parseable gpustack/runner image tags and explicit version fields.
func validateModelDeploymentEngineVersions(old, md *workercore.ModelDeployment) field.ErrorList {
	if md.DeletionTimestamp != nil {
		return nil
	}

	var errs field.ErrorList
	engineVerPath := field.NewPath("spec", "engine", "version")
	rolesPath := field.NewPath("spec", "roles")

	engineVerSyntaxValid := true
	if md.Spec.Engine.Version != "" {
		if err := inject.ValidateEngineVersion(md.Spec.Engine.Version); err != nil {
			engineVerSyntaxValid = false
			errs = append(errs, field.Invalid(engineVerPath, md.Spec.Engine.Version, err.Error()))
		}
	}

	roleVerSyntaxValid := make([]bool, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roleVerSyntaxValid[i] = true
		if md.Spec.Roles[i].EngineVersion == "" {
			continue
		}
		if err := inject.ValidateEngineVersion(md.Spec.Roles[i].EngineVersion); err != nil {
			roleVerSyntaxValid[i] = false
			errs = append(errs, field.Invalid(
				rolesPath.Index(i).Child("engineVersion"), md.Spec.Roles[i].EngineVersion, err.Error(),
			))
		}
	}

	if !modelDeploymentEngineVersionInputsChanged(old, md) {
		return errs
	}

	reportedDeploymentEmpty := false
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 {
			continue
		}
		if modelDeploymentEffectiveEngineVersion(md, role) != "" {
			continue
		}
		if role.Image != "" {
			roleVerPath := rolesPath.Index(i).Child("engineVersion")
			errs = append(errs, field.Required(roleVerPath, fmt.Sprintf(
				"cannot determine %s engine version from image %q for role %q; set %s or spec.engine.version explicitly",
				md.Spec.Engine.Name, role.Image, role.Name, roleVerPath,
			)))
			continue
		}
		if !reportedDeploymentEmpty {
			reportedDeploymentEmpty = true
			errs = append(errs, field.Required(engineVerPath, fmt.Sprintf(
				"cannot determine %s engine version from image %q; set spec.engine.version or %s explicitly",
				md.Spec.Engine.Name, md.Spec.Engine.Image, rolesPath.Index(i).Child("engineVersion"),
			)))
		}
	}

	if md.Spec.Engine.Version != "" && engineVerSyntaxValid {
		if parsed, ok := inject.ParseRunnerEngineVersion(md.Spec.Engine.Image, md.Spec.Engine.Name); ok &&
			parsed != md.Spec.Engine.Version {
			errs = append(errs, field.Invalid(engineVerPath, md.Spec.Engine.Version, fmt.Sprintf(
				"spec.engine.version %q disagrees with runner image %q (parsed version %q); "+
					"update or clear spec.engine.version alongside spec.engine.image",
				md.Spec.Engine.Version, md.Spec.Engine.Image, parsed,
			)))
		}
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.EngineVersion == "" || !roleVerSyntaxValid[i] {
			continue
		}
		effectiveImg := modelDeploymentEffectiveEngineImage(md, role)
		parsed, ok := inject.ParseRunnerEngineVersion(effectiveImg, md.Spec.Engine.Name)
		if !ok || parsed == role.EngineVersion {
			continue
		}
		roleVerPath := rolesPath.Index(i).Child("engineVersion")
		imgField := "spec.engine.image"
		if role.Image != "" {
			imgField = rolesPath.Index(i).Child("image").String()
		}
		errs = append(errs, field.Invalid(roleVerPath, role.EngineVersion, fmt.Sprintf(
			"%s %q disagrees with runner image %q (parsed version %q); update or clear %s alongside %s",
			roleVerPath, role.EngineVersion, effectiveImg, parsed, roleVerPath, imgField,
		)))
	}

	return errs
}

// validateModelDeploymentVLLMRouterKVAware enforces the vLLM >= 0.31.0 version floor and
// conditional operator ownership of --prefix-caching-hash-algo and
// VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES for roles where vllm-router kv_aware routing is active.
func validateModelDeploymentVLLMRouterKVAware(md *workercore.ModelDeployment) field.ErrorList {
	if md.DeletionTimestamp != nil ||
		md.Spec.Router == nil ||
		md.Spec.Router.Name != workercore.ModelDeploymentRouterVLLM ||
		md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM {
		return nil
	}

	kvAware := router.ParseVLLMRouterKVAware(
		md.Spec.Router.ExtraArgs, workerctrl.ModelDeploymentDeclaresBothHalves(md),
	)
	if !kvAware.Any {
		return nil
	}

	var errs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 {
			continue
		}
		upstreamKind, ok := workerctrl.ModelDeploymentRoleUpstreamKind(
			workerctrl.ModelDeploymentEffectiveRoleKind(role),
		)
		if !ok || !kvAware.ForRoleKind(upstreamKind) {
			continue
		}

		rolePath := rolesPath.Index(i)
		ver := modelDeploymentEffectiveEngineVersion(md, role)
		verPath := field.NewPath("spec", "engine", "version")
		if role.EngineVersion != "" || role.Image != "" {
			verPath = rolePath.Child("engineVersion")
		}
		if ver == "" {
			errs = append(errs, field.Invalid(verPath, ver, fmt.Sprintf(
				"vllm-router kv_aware routing on role %q requires vLLM >= %s "+
					"(for GET /kv_event_sources discovery), but no engine version is resolved",
				role.Name, modelDeploymentVLLMRouterKVAwareMinVersion,
			)))
		} else if atLeast, err := inject.EngineVersionAtLeast(
			ver, modelDeploymentVLLMRouterKVAwareMinVersion,
		); err == nil && !atLeast {
			errs = append(errs, field.Invalid(verPath, ver, fmt.Sprintf(
				"vllm-router kv_aware routing on role %q requires vLLM >= %s "+
					"(for GET /kv_event_sources discovery), got %q",
				role.Name, modelDeploymentVLLMRouterKVAwareMinVersion, ver,
			)))
		}

		for j, arg := range role.ExtraArgs {
			if arg == "--" {
				break
			}
			owned, ok := workerctrl.ModelDeploymentResolveArg(
				md.Spec.Engine.Name, arg, []string{modelDeploymentVLLMPrefixCachingHashAlgo},
			)
			if !ok {
				continue
			}
			subject := fmt.Sprintf("%q", owned)
			if name := workerctrl.ModelDeploymentArgName(arg); name != owned {
				subject = fmt.Sprintf("%q is %q, which", name, owned)
			}
			errs = append(errs, field.Invalid(rolePath.Child("extraArgs").Index(j), arg, fmt.Sprintf(
				"%s is set by the operator for engine %q when vllm-router kv_aware routing is active "+
					"and must not be supplied here, because two values for it cannot be told apart; "+
					"replace the whole command line through %s to own it instead",
				subject, md.Spec.Engine.Name, rolePath.Child("command"),
			)))
		}

		for j := range role.Env {
			name := role.Env[j].Name
			if name != modelDeploymentVLLMKVEventsIntBlockHashEnv {
				continue
			}
			errs = append(errs, field.Invalid(rolePath.Child("env").Index(j), name, fmt.Sprintf(
				"%q is set by the operator for engine %q when vllm-router kv_aware routing is active "+
					"and must not be supplied here; replace the whole command line through %s to own it instead",
				name, md.Spec.Engine.Name, rolePath.Child("command"),
			)))
		}
	}

	return errs
}

// validateModelDeploymentAscendPDVersionPair refuses an Ascend vLLM prefill/decode pair whose
// managed halves straddle the 0.30.0 MooncakeConnectorV1/MooncakeConnectorV2 boundary.
func validateModelDeploymentAscendPDVersionPair(
	md *workercore.ModelDeployment, seen map[string]*worker.AcceleratorType,
) field.ErrorList {
	if md.DeletionTimestamp != nil ||
		md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM ||
		!workerctrl.ModelDeploymentDeclaresBothHalves(md) {
		return nil
	}

	var prefill, decode *workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 {
			continue
		}
		switch workerctrl.ModelDeploymentEffectiveRoleKind(role) {
		case workercore.ModelDeploymentRoleKindPrefill:
			prefill = role
		case workercore.ModelDeploymentRoleKindDecode:
			decode = role
		}
	}
	if prefill == nil || decode == nil {
		return nil
	}

	isAscend := func(role *workercore.ModelDeploymentRole) bool {
		if role.AcceleratorType == "" {
			return false
		}
		accType := seen[role.AcceleratorType]
		return accType != nil && accType.Status.Detail.Manufacturer == nodefeature.ManufacturerAscend
	}
	if !isAscend(prefill) || !isAscend(decode) {
		return nil
	}

	isV2 := func(ver string) (bool, bool) {
		if ver == "" {
			return false, true
		}
		atLeast, err := inject.EngineVersionAtLeast(ver, modelDeploymentVLLMAscendV2MinVersion)
		if err != nil {
			return false, false
		}
		return atLeast, true
	}

	prefillVer := modelDeploymentEffectiveEngineVersion(md, prefill)
	decodeVer := modelDeploymentEffectiveEngineVersion(md, decode)
	prefillV2, prefillOK := isV2(prefillVer)
	decodeV2, decodeOK := isV2(decodeVer)
	if !prefillOK || !decodeOK || prefillV2 == decodeV2 {
		return nil
	}

	displayVer := func(ver string) string {
		if ver == "" {
			return "<unset>"
		}
		return ver
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "roles"),
		fmt.Sprintf("%s=%s, %s=%s", prefill.Name, displayVer(prefillVer), decode.Name, displayVer(decodeVer)),
		fmt.Sprintf(
			"Ascend prefill/decode pair cannot mix MooncakeConnectorV1 (< %s) and "+
				"MooncakeConnectorV2 (>= %s) across roles %q (%s) and %q (%s); "+
				"align both roles on the same connector generation",
			modelDeploymentVLLMAscendV2MinVersion, modelDeploymentVLLMAscendV2MinVersion,
			prefill.Name, displayVer(prefillVer), decode.Name, displayVer(decodeVer),
		),
	)}
}

// modelDeploymentRolePublishesKVEvents reports whether the operator synthesizes ZMQ KV-event
// publisher and replay listeners (ports 5557/5558) onto this managed role.
func modelDeploymentRolePublishesKVEvents(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	if len(role.Command) > 0 || md.Spec.Router == nil {
		return false
	}

	kind := workerctrl.ModelDeploymentEffectiveRoleKind(role)
	switch md.Spec.Engine.Name {
	case workercore.ModelDeploymentEngineVLLM:
		switch md.Spec.Router.Name {
		case workercore.ModelDeploymentRouterLLMD:
			return kind == workercore.ModelDeploymentRoleKindServer ||
				kind == workercore.ModelDeploymentRoleKindPrefill
		case workercore.ModelDeploymentRouterVLLM:
			if md.DeletionTimestamp != nil {
				return false
			}
			upstreamKind, ok := workerctrl.ModelDeploymentRoleUpstreamKind(kind)
			if !ok {
				return false
			}
			kvAware := router.ParseVLLMRouterKVAware(
				md.Spec.Router.ExtraArgs, workerctrl.ModelDeploymentDeclaresBothHalves(md),
			)
			return kvAware.ForRoleKind(upstreamKind)
		}
	case workercore.ModelDeploymentEngineSGLang:
		if md.DeletionTimestamp != nil {
			return false
		}
		if md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL {
			return kind == workercore.ModelDeploymentRoleKindServer ||
				kind == workercore.ModelDeploymentRoleKindPrefill
		}
	}

	return false
}
