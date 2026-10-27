package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// modelDeploymentScalerPriorDeclaration is what a deployment states.
//
// IT IS READ FROM THE DEPLOYMENT AND NOT FROM A PROFILE, because the catalog is
// selected by what the deployment states and not the other way round.
type modelDeploymentScalerPriorDeclaration struct {
	// identity is the identity the deployment states.
	identity priors.Identity

	// topology is the shape of one scaling replica the deployment states.
	topology priors.Topology

	// flags are the engine arguments the deployment passes.
	flags map[string]string

	// engineFamily is the engine the deployment runs.
	engineFamily string

	// instance is the instance type the deployment runs on.
	instance *workerapi.InstanceType
}

// shape returns the shape the deployment states.
//
// IT NAMES THE SIX ELEMENTS, because the catalog is selected by a shape and not
// by a name.
func (d modelDeploymentScalerPriorDeclaration) shape(role priors.RoleKind) priors.Shape {
	return priors.Shape{
		Role:         role,
		Model:        priors.CanonicalModelName(d.identity.Model.Name),
		Quantization: priors.CanonicalQuantization(d.identity.Model.Quantization),
		Engine:       d.engineFamily,
		Hardware:     priors.CanonicalAcceleratorFamily(d.identity.Hardware.Accelerator),
		Topology:     d.topology.Normalized(),
	}
}

// modelDeploymentScalerPriorDeclarationOf reads what the deployment states.
//
// IT READS THE MODEL, THE ENGINE, THE HARDWARE AND THE SHAPE OF THE REPLICA,
// because those four are what the catalog is selected by.
func modelDeploymentScalerPriorDeclarationOf(
	ctx context.Context, reader ctrlcli.Reader, target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) (declaration modelDeploymentScalerPriorDeclaration, err error) {
	// THE REFUSAL IS REDACTED HERE, because the reader of the parallelism
	// reports the value it read, and a value the user wrote belongs to nobody
	// else.
	defer func() {
		err = modelDeploymentScalerRedactPriorRefusal(err, role, nil)
	}()
	declaration = modelDeploymentScalerPriorDeclaration{}
	flags, err := modelDeploymentScalerPriorFlags(target.Spec.Engine.Name, role.ExtraArgs)
	if err != nil {
		return declaration, modelDeploymentScalerPriorRefusal(role.Name, "%v", err)
	}
	declaration.flags = flags
	// THE REPLICA IS HELD BEFORE THE SHAPE IS READ, because a deployment the
	// operator cannot manage has no shape a measurement applies to, and the
	// reader of the shape reports the parallelism it could not read.
	if len(role.Command) != 0 || role.ReplicaSize > 1 || role.ElasticEP != nil {
		return declaration, modelDeploymentScalerPriorRefusal(role.Name, "Catalog requires a managed single-serving-Pod replica")
	}
	topology, err := modelDeploymentScalerPriorTopology(target.Spec.Engine.Name, role)
	if err != nil {
		return declaration, err
	}
	declaration.topology = topology
	model, err := modelDeploymentScalerPriorModelIdentity(ctx, reader, target, role.Name, flags)
	if err != nil {
		return declaration, err
	}
	declaration.identity.Model = model
	declaration.identity.Engine = priors.EngineIdentity{Image: modelDeploymentScalerPriorEngineImage(target, role)}
	declaration.engineFamily = modelDeploymentScalerEngineFamily(target.Spec.Engine.Name)
	if declaration.engineFamily == "" {
		return declaration, modelDeploymentScalerPriorRefusal(role.Name, "the deployment names no known engine")
	}
	instance, hardware, err := modelDeploymentScalerPriorHardwareIdentity(ctx, reader, role)
	if err != nil {
		return declaration, err
	}
	declaration.instance = instance
	declaration.identity.Hardware = hardware
	return declaration, nil
}

// modelDeploymentScalerPriorEngineImage returns the image the deployment runs.
//
// IT FALLS BACK TO THE ENGINE NAME, because an engine is named by the operator
// and a measurement names the engine family, and the family of the name is the
// family of the engine.
func modelDeploymentScalerPriorEngineImage(target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole) string {
	if role.Image != "" {
		return role.Image
	}
	if target.Spec.Engine.Image != "" {
		return target.Spec.Engine.Image
	}
	return target.Spec.Engine.Name
}

// modelDeploymentScalerPriorRequest reads the request a capacity prior is
// resolved against.
//
// IT REDACTS EVERY ENVIRONMENT VALUE FROM EVERY REFUSAL IT RETURNS, because a
// refusal reaches the conditions of the scaler, where the value of an
// environment variable the user wrote belongs to nobody else.
func modelDeploymentScalerPriorRequest(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, profile *priors.Profile,
	declaration modelDeploymentScalerPriorDeclaration,
) (priors.Request, error) {
	request, err := modelDeploymentScalerPriorRequestMatched(target, role, profile, declaration)
	if err == nil {
		return request, nil
	}
	return request, modelDeploymentScalerRedactPriorRefusal(err, role, profile)
}

// modelDeploymentScalerPriorRequestMatched reads the request a capacity prior
// is resolved against, before the refusal is redacted.
//
// IT HOLDS THE DEPLOYMENT AGAINST THE PROFILE, because a measurement describes
// the deployment it was taken on and not every deployment of the shape.
func modelDeploymentScalerPriorRequestMatched(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, profile *priors.Profile,
	declaration modelDeploymentScalerPriorDeclaration,
) (priors.Request, error) {
	request := priors.Request{
		Role:     modelDeploymentScalerRoleKind(ModelDeploymentEffectiveRoleKind(role)),
		Identity: declaration.identity,
		Topology: declaration.topology,
	}
	// A MOUNTED VOLUME IS ALLOWED, because a deployment mounts the weights,
	// the caches and the logs of its own role, and the engine is matched by
	// its family rather than by the digest of the image it runs.
	if target.Spec.KVCache != nil || target.Spec.KVTransfer != nil || target.Spec.Router != nil {
		return request, modelDeploymentScalerPriorRefusal(role.Name, "generated cache, transfer and router configuration is not supported by Catalog")
	}
	for _, other := range target.Spec.Roles {
		if ModelDeploymentEffectiveRoleKind(&other) == workercore.ModelDeploymentRoleKindEncode {
			return request, modelDeploymentScalerPriorRefusal(role.Name, "generated encoder connector configuration is not supported by Catalog")
		}
	}
	deploymentEnv, envErr := modelDeploymentScalerDeploymentEnv(role)
	if envErr != nil {
		return request, modelDeploymentScalerPriorRefusal(role.Name, "%v", envErr)
	}
	if err := modelDeploymentScalerMatchEngineEnv(
		profile.ServingConfig.EngineEnv, sortedEnvironmentEntries(deploymentEnv),
		profile.ServingConfig.EngineEnvComplete,
	); err != nil {
		return request, modelDeploymentScalerPriorRefusal(role.Name, "%v", err)
	}
	if err := modelDeploymentScalerPriorModelAnnotation(target, role, profile.Identity); err != nil {
		return request, err
	}
	if err := modelDeploymentScalerPriorHardwareAnnotation(target, role, profile.Identity); err != nil {
		return request, err
	}
	if err := modelDeploymentScalerPriorEngine(target, role, profile.Identity); err != nil {
		return request, err
	}
	if err := modelDeploymentScalerPriorModel(role, profile.Identity.Model, declaration); err != nil {
		return request, err
	}
	if err := modelDeploymentScalerPriorHardware(role, profile.Identity.Hardware, declaration.instance); err != nil {
		return request, err
	}
	serving, err := modelDeploymentScalerPriorServingConfig(target.Spec.Engine.Name, role, declaration.flags, profile.ServingConfig)
	if err != nil {
		return request, err
	}
	if err := modelDeploymentScalerPriorOmittedDefaultsGate(target.Spec.Engine.Name, profile.ServingConfig, declaration.flags); err != nil {
		return request, modelDeploymentScalerPriorRefusal(role.Name, "%v", err)
	}
	if err := modelDeploymentScalerOperatorEnvGate(target.Spec.Engine.Name, target, role); err != nil {
		return request, modelDeploymentScalerPriorRefusal(role.Name, "%v", err)
	}
	request.ServingConfig = serving
	return request, nil
}

func modelDeploymentScalerPriorTopology(engine string, role *workercore.ModelDeploymentRole) (priors.Topology, error) {
	declared, err := ParseModelDeploymentDeclaredParallelism(engine, role.ExtraArgs, role.Env)
	if err != nil {
		return priors.Topology{}, modelDeploymentScalerPriorRefusal(role.Name, "unreadable parallelism: %v", err)
	}
	reading := ReadModelDeploymentRoleParallelism(engine, role)
	if !reading.Source.Complete || reading.LoadBalance != workercore.ModelDeploymentLoadBalanceInternal ||
		declared.DataParallel != 1 || declared.DataParallelLocal > 1 || declared.ElasticEPMaxDataParallel != 0 ||
		reading.Declared.DataParallelLocal != nil && *reading.Declared.DataParallelLocal == 0 ||
		len(declared.Wiring) != 0 || len(declared.Modes) != 0 || declared.AttentionDataParallel != 1 ||
		declared.AttentionContextParallel != 1 || declared.MoEDataParallel != 1 || declared.DWDPSize != 1 ||
		declared.PrefillContextParallel != 1 && declared.PrefillContextParallel != declared.DecodeContextParallel {
		return priors.Topology{}, modelDeploymentScalerPriorRefusal(role.Name, "parallelism has no proven single-Pod metric denominator")
	}
	resources := role.Resources
	if resources == nil || resources.Accelerator == nil || resources.Accelerator.Sign() <= 0 ||
		resources.AcceleratorSlicedMemoryPercentage != 0 || resources.AcceleratorSlicedCoresPercentage != 0 ||
		resources.AcceleratorPartitionedProfile != "" {
		return priors.Topology{}, modelDeploymentScalerPriorRefusal(role.Name, "Catalog requires an explicit whole-accelerator count")
	}
	count, exact := resources.Accelerator.AsInt64()
	if !exact || count > 1<<31-1 {
		return priors.Topology{}, modelDeploymentScalerPriorRefusal(role.Name, "accelerator count must be a bounded integer")
	}
	return priors.Topology{
		GPUsPerReplica: int(count), TensorParallel: declared.TensorParallel, PipelineParallel: declared.PipelineParallel,
		DataParallel: 1, LocalDataParallel: 1, ContextParallel: declared.DecodeContextParallel, ExpertParallel: declared.ExpertParallel,
	}, nil
}

// modelDeploymentScalerPriorModelIdentity reads the model the deployment
// serves.
//
// IT READS THE ARTIFACT WHEN THE DEPLOYMENT NAMES ONE, because the artifact
// resolves the repository and the weight commit the deployment runs.
//
// IT NAMES NO REVISION WHEN THE DEPLOYMENT STATES NONE, because a deployment
// that pins no weight commit says nothing about the weights it runs.
func modelDeploymentScalerPriorModelIdentity(
	ctx context.Context, reader ctrlcli.Reader, target *workercore.ModelDeployment, role string,
	flags map[string]string,
) (priors.ModelIdentity, error) {
	name, revision := target.Spec.Model.Name, flags["--revision"]
	if ref := target.Spec.Model.ArtifactRef; ref != nil {
		artifact := new(workercore.ModelArtifact)
		if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: target.Namespace, Name: ref.Name}, artifact); err != nil {
			if apierrors.IsNotFound(err) {
				return priors.ModelIdentity{}, modelDeploymentScalerPriorRefusal(role, "ModelArtifact %q does not exist", ref.Name)
			}
			return priors.ModelIdentity{}, err
		}
		if artifact.Status.Resolved == nil || artifact.Status.Resolved.Revision == "" {
			return priors.ModelIdentity{}, modelDeploymentScalerPriorRefusal(role, "ModelArtifact %q has no resolved weight revision", ref.Name)
		}
		source := artifact.Spec.Source.HuggingFace
		if source == nil {
			source = artifact.Spec.Source.ModelScope
		}
		if source == nil {
			return priors.ModelIdentity{}, modelDeploymentScalerPriorRefusal(role, "artifact source has no verifiable model repository identity")
		}
		name = source.Repository
		resolved := artifact.Status.Resolved.Revision
		if revision != "" && revision != resolved {
			return priors.ModelIdentity{}, modelDeploymentScalerPriorRefusal(role, "launch revision conflicts with resolved artifact")
		}
		revision = resolved
	}
	tokenizer, specified := flags["--tokenizer"]
	tokenizerRevision := flags["--tokenizer-revision"]
	if tokenizerRevision != "" {
		if !specified {
			tokenizer = name
		}
		tokenizer += "@" + tokenizerRevision
		specified = true
	}
	if !specified {
		tokenizer = ""
	}
	// THE QUANTIZATION COMES FROM THE ENGINE ARGUMENT OR FROM THE ANNOTATION,
	// because a deployment states its recipe in one of the two and a deployment
	// that states neither has chosen no recipe.
	quantization := flags["--quantization"]
	if quantization == "" {
		quantization = target.Annotations[workercore.ModelDeploymentScalerPrecisionAnnotationKey]
	}
	return priors.ModelIdentity{
		Name:         name,
		Revision:     revision,
		Tokenizer:    tokenizer,
		Quantization: quantization,
	}, nil
}

// modelDeploymentScalerPriorModel refuses a deployment that states a model the
// profile did not measure.
//
// THE MODEL NAME IS COMPARED WITHOUT ITS ORGANIZATION AND WITHOUT ITS CASE,
// because a deployment declares the model it serves and a source declares the
// model it measured, and the two spell one model differently.
//
// THE REVISION IS COMPARED ONLY WHEN BOTH SIDES PIN ONE, because a public
// source pins no weight commit and a deployment pins none either.
func modelDeploymentScalerPriorModel(
	role *workercore.ModelDeploymentRole, identity priors.ModelIdentity,
	declaration modelDeploymentScalerPriorDeclaration,
) error {
	if !priors.ModelNameMatches(identity.Name, declaration.identity.Model.Name) {
		return modelDeploymentScalerPriorRefusal(role.Name, "model name conflicts with the selected model identity")
	}
	if !priors.RevisionMatches(identity.Revision, declaration.identity.Model.Revision) {
		return modelDeploymentScalerPriorRefusal(role.Name, "model revision conflicts with the selected model identity")
	}
	if quantization, specified := declaration.flags["--quantization"]; specified &&
		priors.CanonicalQuantization(quantization) != priors.CanonicalQuantization(identity.Quantization) {
		return modelDeploymentScalerPriorRefusal(role.Name, "--quantization conflicts with declared model identity")
	}
	if declaration.identity.Model.Tokenizer != "" && !priors.TokenizerMatches(identity.Tokenizer, declaration.identity.Model.Tokenizer) {
		return modelDeploymentScalerPriorRefusal(role.Name, "--tokenizer conflicts with declared tokenizer identity and revision")
	}
	return nil
}

// modelDeploymentScalerPriorHardwareIdentity reads the hardware the deployment
// runs on.
//
// IT READS THE INSTANCE TYPE, because the instance type is what names the
// accelerator the deployment runs on.
func modelDeploymentScalerPriorHardwareIdentity(
	ctx context.Context, reader ctrlcli.Reader, role *workercore.ModelDeploymentRole,
) (*workerapi.InstanceType, priors.HardwareIdentity, error) {
	instance := new(workerapi.InstanceType)
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Name: role.InstanceType}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, priors.HardwareIdentity{}, modelDeploymentScalerPriorRefusal(role.Name, "InstanceType %q does not exist", role.InstanceType)
		}
		return nil, priors.HardwareIdentity{}, err
	}
	if !instance.Spec.Acceleratable {
		return nil, priors.HardwareIdentity{}, modelDeploymentScalerPriorRefusal(role.Name, "InstanceType has no accelerators")
	}
	identity := priors.NormalizeHardwareIdentity(priors.HardwareIdentity{Accelerator: instance.Status.Detail.Product})
	return instance, identity, nil
}

// modelDeploymentScalerPriorHardware refuses a deployment whose instance type
// contradicts the hardware the profile measured.
//
// THE PRODUCT IS COMPARED BY ITS CANONICAL NAME, because a vendor publishes the
// same accelerator under a product line name and under a part number.
func modelDeploymentScalerPriorHardware(
	role *workercore.ModelDeploymentRole, identity priors.HardwareIdentity, instance *workerapi.InstanceType,
) error {
	normalized := priors.NormalizeHardwareIdentity(identity)
	if product := instance.Status.Detail.Product; product != "" &&
		priors.NormalizeHardwareIdentity(priors.HardwareIdentity{Accelerator: product}).Accelerator != normalized.Accelerator {
		return modelDeploymentScalerPriorRefusal(role.Name, "InstanceType product conflicts with the declared hardware identity")
	}
	if memory := instance.Status.Detail.Memory; memory != "" {
		quantity, err := resource.ParseQuantity(memory)
		if err != nil || quantity.Cmp(*resource.NewQuantity(int64(normalized.MemoryGB)*(1<<30), resource.BinarySI)) != 0 {
			return modelDeploymentScalerPriorRefusal(role.Name, "InstanceType memory conflicts with the declared hardware identity")
		}
	}
	return nil
}

// modelDeploymentScalerPriorModelAnnotation checks the model identity the
// deployment states.
//
// THE ANNOTATION IS OPTIONAL, because a deployment names the model it serves
// and that name is the model identity. The deployment also names the model in
// the engine arguments it passes, and admission holds the served name and the
// declared name to the same value.
//
// WHEN THE ANNOTATION IS THERE IT STATES EITHER A NAME OR A DIGEST, because a
// digest is the identity and a name is the model the identity names.
//
// THE PRECISION IS OPTIONAL AND FOLDED, because a recipe is spelled several
// ways and the spelling says nothing about the weights.
func modelDeploymentScalerPriorModelAnnotation(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, declared priors.Identity,
) error {
	annotation := target.Annotations[workercore.ModelDeploymentScalerModelIDAnnotationKey]
	if annotation != "" {
		if digest, isDigest := strings.CutPrefix(annotation, "model:"); isDigest {
			modelID, err := priors.Digest(declared.Model)
			if err != nil {
				return err
			}
			if digest != modelID {
				return modelDeploymentScalerPriorRefusal(role.Name, "model-id does not match the selected model identity")
			}
		} else if !priors.ModelNameMatches(declared.Model.Name, annotation) {
			return modelDeploymentScalerPriorRefusal(role.Name, "model-id does not name the selected model")
		}
	}
	precision := target.Annotations[workercore.ModelDeploymentScalerPrecisionAnnotationKey]
	if precision != "" &&
		priors.CanonicalQuantization(precision) != priors.CanonicalQuantization(declared.Model.Quantization) {
		return modelDeploymentScalerPriorRefusal(role.Name, "precision does not match the selected model identity")
	}
	return nil
}

// modelDeploymentScalerPriorHardwareAnnotation checks the hardware identity
// the deployment states.
//
// THE ANNOTATION IS OPTIONAL, because a deployment selects the profile it
// wants and the profile names the hardware it was measured on.
//
// WHEN THE ANNOTATION IS THERE IT STATES EITHER A NAME OR A DIGEST, because a
// digest is the identity and a name is the accelerator the identity names.
func modelDeploymentScalerPriorHardwareAnnotation(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, declared priors.Identity,
) error {
	hardwareKey := workercore.ModelDeploymentScalerHardwareIDAnnotationKey
	switch ModelDeploymentEffectiveRoleKind(role) {
	case workercore.ModelDeploymentRoleKindPrefill:
		if target.Annotations[workercore.ModelDeploymentScalerPrefillHardwareIDAnnotationKey] != "" {
			hardwareKey = workercore.ModelDeploymentScalerPrefillHardwareIDAnnotationKey
		}
	case workercore.ModelDeploymentRoleKindDecode:
		if target.Annotations[workercore.ModelDeploymentScalerDecodeHardwareIDAnnotationKey] != "" {
			hardwareKey = workercore.ModelDeploymentScalerDecodeHardwareIDAnnotationKey
		}
	}
	annotation := target.Annotations[hardwareKey]
	if annotation == "" {
		return nil
	}
	if digest, isDigest := strings.CutPrefix(annotation, "hardware:"); isDigest {
		hardwareID, err := priors.Digest(declared.Hardware)
		if err != nil {
			return err
		}
		if digest != hardwareID {
			return modelDeploymentScalerPriorRefusal(role.Name, "hardware-id does not match the selected hardware identity")
		}
		return nil
	}
	named := priors.NormalizeHardwareIdentity(priors.HardwareIdentity{Accelerator: annotation})
	if named.Accelerator != priors.NormalizeHardwareIdentity(declared.Hardware).Accelerator {
		return modelDeploymentScalerPriorRefusal(role.Name, "hardware-id does not name the selected hardware")
	}
	return nil
}

// modelDeploymentScalerPriorEngine checks the engine the deployment runs.
//
// THE ENGINE FAMILY MUST MATCH, because a measurement taken with one engine
// says nothing about another one.
//
// THE IMAGE DIGEST IS NOT REQUIRED, because a deployment names the image it
// runs and a source names the image it measured, and the two differ in a tag
// or a digest while the engine is the same one.
func modelDeploymentScalerPriorEngine(
	target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, declared priors.Identity,
) error {
	family := modelDeploymentScalerEngineFamily(target.Spec.Engine.Name)
	if family == "" {
		return modelDeploymentScalerPriorRefusal(role.Name, "the deployment names no known engine")
	}
	image := role.Image
	if image == "" {
		image = target.Spec.Engine.Image
	}
	// THE ROLE IMAGE MUST RUN THE ENGINE THE DEPLOYMENT NAMES, when the image
	// names a family at all.
	if imageFamily := priors.EngineFamily(image); imageFamily != "" && imageFamily != family {
		return modelDeploymentScalerPriorRefusal(role.Name, "the engine image of the role does not match the deployment engine")
	}
	// THE PROFILE MUST HAVE BEEN MEASURED WITH THE ENGINE THE DEPLOYMENT RUNS.
	// A PROFILE IMAGE THAT NAMES NO FAMILY IS ACCEPTED ONLY WHEN THE
	// DEPLOYMENT RUNS THE SAME IMAGE, because a comparison of two images that
	// name no family says nothing about the engine behind them.
	profileFamily := priors.EngineFamily(declared.Engine.Image)
	if profileFamily == "" {
		if declared.Engine.Image != image {
			return modelDeploymentScalerPriorRefusal(role.Name, "the selected profile names no engine family and the deployment runs another image")
		}
		return nil
	}
	if profileFamily != family {
		return modelDeploymentScalerPriorRefusal(role.Name, "the engine family of the selected profile does not match the deployment engine")
	}
	return nil
}

// modelDeploymentScalerEngineFamily returns the engine family a deployment
// runs.
func modelDeploymentScalerEngineFamily(engine string) string {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		return "vllm"
	case workercore.ModelDeploymentEngineSGLang:
		return "sglang"
	default:
		return ""
	}
}

// modelDeploymentScalerPriorServingConfig reads the serving configuration the
// deployment runs under.
//
// IT ADOPTS THE SETTING THE MEASUREMENT RAN UNDER FOR EVERY SETTING THE
// DEPLOYMENT DOES NOT STATE, because a flag the deployment leaves out is not a
// statement about the engine and the operator owns the flags it renders
// itself.
//
// IT KEEPS EVERY SETTING THE DEPLOYMENT STATES, so a deployment that states a
// setting the measurement ran under differently is refused by the comparison
// against the profile.
//
// IT KEEPS THE ROUTER OF THE DEPLOYMENT, because a deployment with a router
// is refused before this gate and a measurement taken behind a router does
// not describe a deployment without one.
func modelDeploymentScalerPriorServingConfig(
	engine string, role *workercore.ModelDeploymentRole, flags map[string]string, profile priors.ServingConfig,
) (priors.ServingConfig, error) {
	deploymentEnv, err := modelDeploymentScalerDeploymentEnv(role)
	if err != nil {
		return priors.ServingConfig{}, err
	}
	result := priors.ServingConfig{
		Router:              "none",
		KVCacheDType:        profile.KVCacheDType,
		Offload:             profile.Offload,
		PrefixCaching:       profile.PrefixCaching,
		SpeculativeDecoding: profile.SpeculativeDecoding,
		LaunchParameters:    slices.Clone(role.ExtraArgs),
		EngineEnv:           sortedEnvironmentEntries(deploymentEnv),
	}
	if dtype, stated := flags["--kv-cache-dtype"]; stated {
		result.KVCacheDType = dtype
	}
	if offload, stated := flags["--cpu-offload-gb"]; stated && offload != "0" && profile.Offload == "none" {
		result.Offload = "cpu"
	}
	switch {
	case engine == workercore.ModelDeploymentEngineVLLM && flags["--enable-prefix-caching"] != "":
		result.PrefixCaching = "enabled"
	case engine == workercore.ModelDeploymentEngineVLLM && flags["--no-enable-prefix-caching"] != "":
		result.PrefixCaching = "disabled"
	case engine == workercore.ModelDeploymentEngineSGLang && flags["--disable-radix-cache"] != "":
		result.PrefixCaching = "disabled"
	}
	return result, nil
}

// sortedEnvironmentEntries renders an effective environment as the sorted
// KEY=value list the catalog schema stores.
func sortedEnvironmentEntries(env map[string]string) []string {
	entries := make([]string, 0, len(env))
	for name, value := range env {
		entries = append(entries, name+"="+value)
	}
	slices.Sort(entries)
	return entries
}

// modelDeploymentScalerDeploymentEnv returns the effective literal environment
// of a role, last-wins per name.
//
// EVERY ENTRY IS A LITERAL NAME=VALUE PAIR BY API SHAPE — this
// ModelDeploymentEnvVar carries no indirection, so there is no value source to
// refuse here.
func modelDeploymentScalerDeploymentEnv(role *workercore.ModelDeploymentRole) (map[string]string, error) {
	env := map[string]string{}
	for i := range role.Env {
		entry := &role.Env[i]
		if entry.Name == "" {
			return nil, fmt.Errorf("environment variable has an empty name")
		}
		env[entry.Name] = entry.Value
	}
	return env, nil
}

// modelDeploymentScalerMatchEngineEnv refuses unless the deployment's
// effective environment agrees with the environment the prior was measured
// under.
//
// IT IS BIDIRECTIONAL AND EXACT FOR THE VARIABLES THE RECORD CARRIES: a
// variable the measurement recorded and the deployment lacks, and any value
// difference are each a refusal, because every one of them changes the engine
// configuration the measured capacity was produced under.
//
// IT HOLDS THE DEPLOYMENT AGAINST NO VARIABLE THE RECORD DOES NOT CARRY WHEN
// THE RECORD IS INCOMPLETE, because a source that could not watch the whole
// environment says nothing about the variables it did not see, and an
// environment that was not observed is not an environment that was empty.
//
// IT NAMES NO VALUE, because a refusal reaches the conditions of the scaler
// and a value the user wrote belongs to nobody else.
func modelDeploymentScalerMatchEngineEnv(recorded, deployed []string, complete bool) error {
	measured := map[string]string{}
	for _, entry := range recorded {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return fmt.Errorf("the catalog records a malformed engine environment entry")
		}
		if previous, seen := measured[name]; seen && previous != value {
			return fmt.Errorf("the catalog records environment variable %q twice with different values", name)
		}
		measured[name] = value
	}
	deployedEnv := map[string]string{}
	for _, entry := range deployed {
		name, value, _ := strings.Cut(entry, "=")
		if strings.Contains(value, "$") {
			return fmt.Errorf("environment variable %q uses runtime expansion the catalog cannot evaluate", name)
		}
		deployedEnv[name] = value
	}
	var unrecorded, differing []string
	for name, value := range deployedEnv {
		prior, ok := measured[name]
		if !ok {
			if complete {
				unrecorded = append(unrecorded, name)
			}
			continue
		}
		if value != prior {
			differing = append(differing, name)
		}
	}
	var missing []string
	for name := range measured {
		if _, carried := deployedEnv[name]; !carried {
			missing = append(missing, name)
		}
	}
	slices.Sort(unrecorded)
	slices.Sort(differing)
	slices.Sort(missing)
	if len(unrecorded) > 0 {
		return fmt.Errorf("environment variables the catalog does not record: %v", unrecorded)
	}
	if len(differing) > 0 {
		return fmt.Errorf("environment variables with values differing from the recorded measurement: %v", differing)
	}
	if len(missing) > 0 {
		return fmt.Errorf("environment variables the prior was measured with are missing: %v", missing)
	}
	return nil
}

// modelDeploymentScalerPriorOmittedDefaultsGate refuses a deployment that
// omits an engine argument the catalog records a contradicting effective
// default for.
//
// AN OMITTED ARGUMENT IS NOT A REFUSAL, because a flag the deployment leaves
// at the engine default is not a statement about the engine and the catalog
// says nothing about a default it did not record. The setting the catalog
// recorded is adopted for it.
//
// IT REFUSES A DEFAULT THAT CONTRADICTS THE SERVING CONFIGURATION, because a
// measurement that offloaded the KV cache to the host while the catalog says
// the offload was none describes a different engine.
func modelDeploymentScalerPriorOmittedDefaultsGate(
	engine string, profile priors.ServingConfig, flags map[string]string,
) error {
	omitted := []string{"--dtype", "--cpu-offload-gb"}
	if engine == workercore.ModelDeploymentEngineVLLM {
		omitted = append(omitted, "--swap-space")
	}
	for _, flag := range omitted {
		if flags[flag] != "" {
			continue
		}
		recorded, found := profile.EffectiveDefaults[flag]
		if !found || recorded == "" {
			continue
		}
		if flag == "--dtype" {
			if !slices.Contains(modelDeploymentScalerDtypeSpellings, recorded) {
				return fmt.Errorf("the catalog records an effective default the engine does not accept for the omitted %s", flag)
			}
			continue
		}
		value, err := strconv.ParseUint(recorded, 10, 32)
		if err != nil {
			return fmt.Errorf("the catalog records an effective default that is not a whole number for the omitted %s", flag)
		}
		if profile.Offload == "none" && value != 0 {
			return fmt.Errorf(
				"the catalog records an effective default that offloads the KV cache while the serving configuration says the offload was none",
			)
		}
	}
	return nil
}

// modelDeploymentScalerDtypeSpellings are the dtype spellings the engines
// accept.
//
// IT IS A CLOSED LIST AND NOT A SHAPE CHECK, because a spelling the list does
// not carry is a spelling this operator cannot vouch for, and an unvouched
// default is not a default the engine ran under.
var modelDeploymentScalerDtypeSpellings = []string{
	"auto", "half", "bfloat16", "bf16", "float16", "fp16", "float32", "fp32",
}

// modelDeploymentScalerOperatorEnvGate refuses a deployment whose engine would
// carry an environment variable the catalog cannot account for.
//
// THE OPERATOR WRITES VARIABLES INTO THE ENGINE CONTAINER, and a variable it
// writes and the measurement did not run under is a difference between the two
// engine configurations. The operator knows every name it writes, so the names
// it writes are declared capacity-neutral below, and a name it starts writing
// that the declaration does not carry refuses the prior until an operator says
// the variable is neutral.
//
// IT CHECKS THE DECLARED TABLE AND THE ACTUAL RENDER, because the render is
// what reaches the container. A table the operator writes by hand says nothing
// about the render that runs, and a name the render writes and the table does
// not carry is a name the measurement did not run under.
//
// IT ACCOUNTS FOR THE DEFAULTED NAMES AND THE ARTIFACT NAMES, because the
// connector is refused before this gate and the rank names belong to an
// instance that spans several Pods, which is refused as well.
//
// IT NAMES NO VALUE, because a value is a secret and a name is not.
func modelDeploymentScalerOperatorEnvGate(
	engine string, target *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) error {
	declared := slices.Clone(modelDeploymentDefaultedEnvNames)
	if target.Spec.Model.ArtifactRef != nil {
		declared = append(declared, modelDeploymentArtifactOwnedKeys[engine].Env...)
	}
	rendered := modelDeploymentScalerRenderedOperatorEnv(engine, role)
	return modelDeploymentScalerAccountForInjectedEnv(append(declared, rendered...))
}

// modelDeploymentScalerRenderedOperatorEnv returns the names the operator
// writes into the engine container, as the renderer writes them.
//
// IT READS THE RENDER, because the render is the document the engine runs
// with. A gate that reads a table cannot see a name the renderer starts
// writing.
//
// IT LEAVES OUT THE NAMES THE ROLE SETS, because those are the deployment's
// own variables. The recorded environment covers them, and the gate is about
// what the operator adds to them.
//
// IT RENDERS WITHOUT A CONNECTOR AND WITHOUT AN ARTIFACT, because the
// connector and the artifact are refused before this gate and their names are
// accounted for where they are refused.
func modelDeploymentScalerRenderedOperatorEnv(engine string, role *workercore.ModelDeploymentRole) []string {
	env := mergeModelDeploymentEnv(engine, role, ModelDeploymentConnectorRender{}, nil, false)
	names := make([]string, 0, len(env))
	for _, entry := range env {
		if modelDeploymentUserSetsEnv(role.Env, entry.Name) {
			continue
		}
		names = append(names, entry.Name)
	}
	return names
}

// modelDeploymentScalerAccountForInjectedEnv refuses an injected environment
// variable the operator has not declared capacity-neutral.
//
// IT IS A SEPARATE FUNCTION SO THE GATE CAN BE EXERCISED WITH A NAME THE
// OPERATOR DOES NOT WRITE YET, because a gate that cannot be exercised with
// the case it exists for cannot be trusted.
func modelDeploymentScalerAccountForInjectedEnv(names []string) error {
	for _, name := range names {
		if !slices.Contains(modelDeploymentScalerCapacityNeutralEnvNames, name) {
			return fmt.Errorf(
				"the operator injects the environment variable %q and the catalog does not account for it", name,
			)
		}
	}
	return nil
}

// modelDeploymentScalerCapacityNeutralEnvNames are the environment variables
// the operator writes into the engine container that the operator declares not
// to change the engine's measured capacity.
//
// IT IS A LIST OF NAMES AND NOT OF VALUES, because a value is a secret and a
// name is not.
//
// IT IS WRITTEN OUT RATHER THAN DERIVED FROM THE RENDER, because a list taken
// from the render would grow with the render and would never notice that a
// new variable was written. The gate above compares the two, and the
// disagreement is the signal: an operator who writes a new variable has to say
// here that the variable does not change the capacity.
//
// THE CACHE DIRECTORIES SAY WHERE THE ENGINE KEEPS WHAT IT COMPILED, and the
// credentials and the endpoints say WHERE THE WEIGHTS COME FROM, and neither
// says how the engine serves.
var modelDeploymentScalerCapacityNeutralEnvNames = []string{
	"HF_HUB_CACHE", "HF_XET_CACHE", "PIP_CACHE_DIR", "TORCH_HOME",
	"FLASHINFER_WORKSPACE_BASE", "TORCHINDUCTOR_CACHE_DIR", "TRITON_CACHE_DIR",
	"VLLM_CACHE_ROOT", "SGLANG_CACHE_DIR",
	"MC_TE_METRIC", "MC_FORCE_TCP", "MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER",
	"MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER", "MC_TCP_LANES_PER_PEER",
	"MC_TCP_ADMISSION_TIMEOUT_MS",
	"HF_TOKEN", "HF_ENDPOINT", "HF_HOME",
	"MODELSCOPE_API_TOKEN", "MODELSCOPE_DOMAIN", "MODELSCOPE_CACHE",
	"VLLM_USE_MODELSCOPE", "SGLANG_USE_MODELSCOPE",
}

// modelDeploymentScalerRedactPriorRefusal removes the environment values from
// a refusal.
//
// IT REDACTS THE WHOLE CHAIN, because a value can reach the message through the
// reader that parsed it, and a refusal that is scrubbed in one place is
// scrubbed in every place the value could have entered.
//
// IT KEEPS THE NAME OF THE VARIABLE, because the name says which variable the
// refusal is about and a name is not the value.
//
// IT LEAVES AN ERROR THAT IS NOT A REFUSAL ALONE, because a reader failure
// keeps its own type for the admission and the retry, and it carries no
// environment value.
func modelDeploymentScalerRedactPriorRefusal(
	err error, role *workercore.ModelDeploymentRole, profile *priors.Profile,
) error {
	var refusal *priors.RefusalError
	if !errors.As(err, &refusal) {
		return err
	}
	values := modelDeploymentScalerEnvValues(role, profile)
	redacted := *refusal
	redacted.Message = modelDeploymentScalerRedactEnvValues(refusal.Message, values)
	if len(refusal.Detail) > 0 {
		detail := make(map[string]string, len(refusal.Detail))
		for name, value := range refusal.Detail {
			detail[name] = modelDeploymentScalerRedactEnvValues(value, values)
		}
		redacted.Detail = detail
	}
	return &redacted
}

// modelDeploymentScalerEnvValues returns the values the role's environment and
// the recorded environment carry.
//
// IT ACCEPTS A PROFILE THAT IS NOT YET KNOWN, because the declaration is read
// before the catalog is searched and the reader of the parallelism reports a
// value of the deployment.
func modelDeploymentScalerEnvValues(role *workercore.ModelDeploymentRole, profile *priors.Profile) []string {
	values := make([]string, 0, len(role.Env))
	for i := range role.Env {
		if role.Env[i].Value != "" {
			values = append(values, role.Env[i].Value)
		}
	}
	if profile == nil {
		return values
	}
	for _, entry := range profile.ServingConfig.EngineEnv {
		_, value, found := strings.Cut(entry, "=")
		if found && value != "" {
			values = append(values, value)
		}
	}
	return values
}

// modelDeploymentScalerRedactEnvValues removes the values from a message.
//
// IT REMOVES THE QUOTED FORM AND THE WHOLE TOKEN, because a value of one
// character stands inside every number and every identifier, and a message with
// every number removed says less than the message did.
//
// IT FINDS THE TOKEN BY ITS NEIGHBORS AND NOT BY A WORD BOUNDARY, because a
// word boundary exists next to a word character, and a value that starts with
// a minus sign or a dot has none at its front. A negative number is a value
// like any other, and it reaches the message the same way.
func modelDeploymentScalerRedactEnvValues(message string, values []string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(
			message, strconv.Quote(value), strconv.Quote(modelDeploymentScalerRedactedValue),
		)
		message = modelDeploymentScalerRedactWholeToken(message, value)
	}
	return message
}

// modelDeploymentScalerRedactWholeToken replaces the value where it stands as
// a token of its own.
//
// IT LEAVES A VALUE THAT IS PART OF A LONGER WORD ALONE, because a value of
// one character stands inside every identifier and every number.
func modelDeploymentScalerRedactWholeToken(message, value string) string {
	var b strings.Builder
	b.Grow(len(message))
	rest := message
	offset := 0
	for {
		index := strings.Index(rest, value)
		if index < 0 {
			b.WriteString(rest)
			return b.String()
		}
		start := offset + index
		end := start + len(value)
		if modelDeploymentScalerTokenStandsAlone(message, start, end, value) {
			b.WriteString(rest[:index])
			b.WriteString(modelDeploymentScalerRedactedValue)
			rest = rest[index+len(value):]
			offset = end
			continue
		}
		b.WriteString(rest[:index+1])
		rest = rest[index+1:]
		offset = start + 1
	}
}

// modelDeploymentScalerTokenStandsAlone reports whether the value stands
// between two characters that do not make it part of a longer word.
func modelDeploymentScalerTokenStandsAlone(message string, start, end int, value string) bool {
	before := byte(0)
	if start > 0 {
		before = message[start-1]
	}
	after := byte(0)
	if end < len(message) {
		after = message[end]
	}
	return modelDeploymentScalerTokenEdge(value[0], before) &&
		modelDeploymentScalerTokenEdge(value[len(value)-1], after)
}

// modelDeploymentScalerTokenEdge reports whether the character beside the
// value leaves the value a token of its own.
//
// A WORD CHARACTER NEEDS A NON-WORD CHARACTER BESIDE IT, because a word
// character beside a word character makes one longer word. A CHARACTER THAT IS
// NOT A WORD CHARACTER STANDS NEXT TO ANYTHING, because a minus sign belongs
// to the number it precedes.
func modelDeploymentScalerTokenEdge(edge, neighbor byte) bool {
	if modelDeploymentScalerWordByte(edge) {
		return !modelDeploymentScalerWordByte(neighbor)
	}
	return true
}

// modelDeploymentScalerWordByte reports whether the byte is a letter, a digit
// or an underscore.
func modelDeploymentScalerWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// modelDeploymentScalerRedactedValue is what a redacted value is replaced with.
const modelDeploymentScalerRedactedValue = "[redacted]"

// modelDeploymentScalerPriorFlags reads identity-sensitive flags with the engine's spelling rules.
// The full ordered argv remains in the catalog match, including unrecognized performance arguments.
func modelDeploymentScalerPriorFlags(engine string, args []string) (map[string]string, error) {
	if engine != workercore.ModelDeploymentEngineVLLM && engine != workercore.ModelDeploymentEngineSGLang {
		return nil, fmt.Errorf("unsupported engine %q", engine)
	}
	values := map[string]string{}
	valueFlags := []string{
		"--dtype", "--kv-cache-dtype", "--revision", "--tokenizer", "--tokenizer-revision",
		"--quantization", "--cpu-offload-gb", "--swap-space",
	}
	boolFlags := []string{"--enable-prefix-caching", "--no-enable-prefix-caching", "--disable-radix-cache"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return nil, fmt.Errorf("catalog cannot interpret arguments after --")
		}
		key, value, inline := strings.Cut(arg, "=")
		if engine == workercore.ModelDeploymentEngineVLLM {
			key = strings.ReplaceAll(key, "_", "-")
			if strings.HasPrefix(key, "-q") && len(key) > 2 && !inline {
				value, key, inline = key[2:], "--quantization", true
			}
		}
		if key == "-q" && engine == workercore.ModelDeploymentEngineVLLM {
			key = "--quantization"
		}
		if key == "--tokenizer-path" && engine == workercore.ModelDeploymentEngineSGLang {
			key = "--tokenizer"
		}
		if key == "--model" || key == "--model-path" || strings.Contains(key, "speculative") ||
			strings.Contains(key, "hierarchical-cache") || strings.Contains(key, "kv-transfer") ||
			strings.Contains(key, "config") || strings.Contains(key, "lora") {
			return nil, fmt.Errorf("catalog cannot establish settings from %s", key)
		}
		if slices.Contains(boolFlags, key) {
			if inline {
				return nil, fmt.Errorf("%s requires its bare boolean spelling", key)
			}
			if engine == workercore.ModelDeploymentEngineSGLang && key != "--disable-radix-cache" ||
				engine == workercore.ModelDeploymentEngineVLLM && key == "--disable-radix-cache" {
				return nil, fmt.Errorf("%s is not an engine-native cache flag", key)
			}
			values[key] = "true"
			switch key {
			case "--enable-prefix-caching":
				delete(values, "--no-enable-prefix-caching")
			case "--no-enable-prefix-caching":
				delete(values, "--enable-prefix-caching")
			}
			continue
		}
		if !slices.Contains(valueFlags, key) {
			for _, known := range append(slices.Clone(valueFlags), boolFlags...) {
				if strings.HasPrefix(key, "--") && strings.HasPrefix(known, key) {
					return nil, fmt.Errorf("catalog requires the full identity flag %s", known)
				}
			}
			continue
		}
		if !inline {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, fmt.Errorf("%s requires a value", key)
			}
			i++
			value = args[i]
		}
		if value == "" {
			return nil, fmt.Errorf("%s requires a nonempty value", key)
		}
		values[key] = value
	}
	return values, nil
}
