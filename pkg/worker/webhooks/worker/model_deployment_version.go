package worker

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

const modelDeploymentVLLMRouterKVAwareMinVersion = "0.31.0"

// defaultModelDeploymentEngineVersions backfills empty spec.engine.version and
// spec.roles[*].engineVersion from standard gpustack/runner image tags on both CREATE and UPDATE.
// User-supplied versions are never overwritten.
func defaultModelDeploymentEngineVersions(md *workercore.ModelDeployment) {
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
}

func modelDeploymentEffectiveEngineImage(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) string {
	if role.Image != "" {
		return role.Image
	}
	return md.Spec.Engine.Image
}

// validateModelDeploymentEngineVersions checks engine version syntax, enforces resolved versions
// for all managed roles, and refuses disagreements between parseable gpustack/runner image tags
// and explicit version fields.
func validateModelDeploymentEngineVersions(md *workercore.ModelDeployment) field.ErrorList {
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

	reportedDeploymentEmpty := false
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 {
			continue
		}
		if role.Image != "" && role.Image != md.Spec.Engine.Image && role.EngineVersion == "" {
			roleVerPath := rolesPath.Index(i).Child("engineVersion")
			errs = append(errs, field.Required(roleVerPath, fmt.Sprintf(
				"cannot determine %s engine version from image %q for role %q; set %s explicitly",
				md.Spec.Engine.Name, role.Image, role.Name, roleVerPath,
			)))
			continue
		}
		if workerctrl.ModelDeploymentEffectiveEngineVersion(md, role) != "" {
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
		ver := workerctrl.ModelDeploymentEffectiveEngineVersion(md, role)
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
				md.Spec.Engine.Name, arg, []string{inject.VLLMPrefixCachingHashAlgoArg},
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
			if name != inject.VLLMKVEventsUseIntBlockHashesEnv {
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

	isAscend := func(role *workercore.ModelDeploymentRole) bool {
		if role.AcceleratorType == "" {
			return false
		}
		accType := seen[role.AcceleratorType]
		return accType != nil && accType.Status.Detail.Manufacturer == nodefeature.ManufacturerAscend
	}

	var prefills, decodes []*workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 || !isAscend(role) {
			continue
		}
		switch workerctrl.ModelDeploymentEffectiveRoleKind(role) {
		case workercore.ModelDeploymentRoleKindPrefill:
			prefills = append(prefills, role)
		case workercore.ModelDeploymentRoleKindDecode:
			decodes = append(decodes, role)
		}
	}
	if len(prefills) == 0 || len(decodes) == 0 {
		return nil
	}

	isV2 := func(ver string) (bool, bool) {
		if ver == "" {
			return false, true
		}
		atLeast, err := inject.EngineVersionAtLeast(ver, inject.VLLMAscendMooncakeV2MinVersion)
		if err != nil {
			return false, false
		}
		return atLeast, true
	}
	displayVer := func(ver string) string {
		if ver == "" {
			return "<unset>"
		}
		return ver
	}

	var errs field.ErrorList
	for _, prefill := range prefills {
		prefillVer := workerctrl.ModelDeploymentEffectiveEngineVersion(md, prefill)
		prefillV2, prefillOK := isV2(prefillVer)
		if !prefillOK {
			continue
		}
		for _, decode := range decodes {
			decodeVer := workerctrl.ModelDeploymentEffectiveEngineVersion(md, decode)
			decodeV2, decodeOK := isV2(decodeVer)
			if !decodeOK || prefillV2 == decodeV2 {
				continue
			}
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "roles"),
				fmt.Sprintf("%s=%s, %s=%s", prefill.Name, displayVer(prefillVer), decode.Name, displayVer(decodeVer)),
				fmt.Sprintf(
					"Ascend prefill/decode pair cannot mix MooncakeConnectorV1 (< %s) and "+
						"MooncakeConnectorV2 (>= %s) across roles %q (%s) and %q (%s); "+
						"align both roles on the same connector generation",
					inject.VLLMAscendMooncakeV2MinVersion, inject.VLLMAscendMooncakeV2MinVersion,
					prefill.Name, displayVer(prefillVer), decode.Name, displayVer(decodeVer),
				),
			))
		}
	}

	return errs
}

// modelDeploymentRolePublishesKVEvents reports whether the operator synthesizes ZMQ KV-event
// publisher and replay listeners (ports 5557/5558) onto this managed role.
func modelDeploymentRolePublishesKVEvents(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	if md.DeletionTimestamp != nil {
		return false
	}

	return workerctrl.ModelDeploymentRolePublishesKVEvents(md, role)
}

// validateModelDeploymentLLMDRouterKVEventsModelName refuses "@" in spec.model.name when
// llm-d-router KV event publishing is active, because llm-d-router@v1.0.0 parseTopic splits
// "kv@pod@<model>" on "@" using strings.Split and discards model matching when len(parts) != 3.
func validateModelDeploymentLLMDRouterKVEventsModelName(md *workercore.ModelDeployment) field.ErrorList {
	if md.DeletionTimestamp != nil ||
		md.Spec.Router == nil ||
		md.Spec.Router.Name != workercore.ModelDeploymentRouterLLMD ||
		!strings.Contains(md.Spec.Model.Name, "@") {
		return nil
	}

	publishes := false
	for i := range md.Spec.Roles {
		if workerctrl.ModelDeploymentRolePublishesKVEvents(md, &md.Spec.Roles[i]) {
			publishes = true
			break
		}
	}
	if !publishes {
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "model", "name"),
		md.Spec.Model.Name,
		`must not contain "@" when llm-d-router KV cache events are enabled: `+
			`llm-d-router splits KV event topics ("kv@pod@<model>") on "@" and `+
			`drops model matching when extra "@" segments are present`,
	)}
}
