package worker

// The ModelExpress sidecar delivery: a per-deployment metadata server for roles whose
// engine loads weights through `--load-format modelexpress` (or the `mx` shorthand).
//
// The server is ONE object pair per ModelDeployment — a single-replica Deployment and the
// ClusterIP Service that fronts its gRPC port — because the engines it serves read it as
// `<address>:8001` from their environment, and one address must outlive any one engine
// replica's lifecycle. The server keeps its state in memory: it is a rendezvous for the
// engines of one deployment, not a store, so Recreate is the whole rollout story and a
// replaced server is empty by design (replicas republish their tensors as they load).

import (
	"context"
	"fmt"
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	// modelDeploymentModelExpressServerPort is where the mirrored server listens, and where
	// every client env var and probe points.
	modelDeploymentModelExpressServerPort int32 = 8001
	// modelDeploymentResourceNoteModelExpress is the resource note key the sidecar's
	// children carry, which the prune gate reads: a child whose note is empty is never
	// pruned, so the note is what makes the prune safe.
	modelDeploymentResourceNoteModelExpress = "modelexpress-server"
	// modelDeploymentModelExpressComponentLabel is the component label the scoping labels
	// carry, so the sidecar's children never match an engine replica selector.
	modelDeploymentModelExpressComponentLabel = "modelexpress-server"
	// modelDeploymentModelExpressContainerName names the sidecar's one container.
	modelDeploymentModelExpressContainerName = "modelexpress-server"

	// modelDeploymentLoadFormatArg is the engine flag that selects a weight loader.
	modelDeploymentLoadFormatArg = "--load-format"

	// modelDeploymentModelExpressLoadFormat is the load format the pinned upstream
	// registers, and modelDeploymentModelExpressLoadFormatAlias its upstream shorthand.
	modelDeploymentModelExpressLoadFormat      = "modelexpress"
	modelDeploymentModelExpressLoadFormatAlias = "mx"

	// The ModelExpress environment an engine replica reads. The pinned upstream's plugin
	// hook names the GPUStack strategy and its insertion point before the local cache
	// strategy; the artifact-ready hook tells the server when this replica may be graded;
	// the compile-config digest binds one source identity to one role configuration.
	modelDeploymentModelExpressEnvServerAddress      = "MX_SERVER_ADDRESS"
	modelDeploymentModelExpressEnvWorkerHost         = "MX_WORKER_HOST"
	modelDeploymentModelExpressEnvArtifactTransfer   = "MX_ARTIFACT_TRANSFER"
	modelDeploymentModelExpressEnvLoadPlugin         = "MX_LOAD_PLUGIN"
	modelDeploymentModelExpressEnvLoadPluginGPUStack = "gpustack"
	modelDeploymentModelExpressEnvLoadPluginBefore   = "MX_LOAD_PLUGIN_BEFORE"
	modelDeploymentModelExpressLoadPluginBeforeValue = "server-cache"
	modelDeploymentModelExpressEnvArtifactReadyURL   = "MX_ARTIFACT_READY_URL"
	modelDeploymentModelExpressEnvCompileDigest      = "MX_ARTIFACT_COMPILE_CONFIG_DIGEST"

	// modelDeploymentModelTransferEnvDeliveryPolicy carries the cluster's remote delivery
	// policy to the engine: Require closes the local fallback strategies, Prefer keeps
	// them behind the verified source delivery.
	modelDeploymentModelTransferEnvDeliveryPolicy = "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY"

	// modelDeploymentModelExpressDeliveryPolicyRequire is the effective policy value that
	// closes the fallback path; every other effective value keeps it open.
	modelDeploymentModelExpressDeliveryPolicyRequire = "Require"
)

// modelDeploymentLoadFormats reads every --load-format selection from an argv the way the
// engine's own parser reads it: vLLM rewrites underscores to dashes in long-flag keys while
// SGLang does not, a unique prefix resolves, a bare "--" ends option recognition, and a token
// after a two-token flag is consumed only when it is not itself an option. A flag with no
// consumable value records an empty string so a caller checking for conflicts can see the
// dangling occurrence.
func modelDeploymentLoadFormats(engine string, args []string) []string {
	keys := []string{modelDeploymentLoadFormatArg}
	var values []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if _, ok := ModelDeploymentResolveArg(engine, args[i], keys); !ok {
			continue
		}
		if _, value, inline := strings.Cut(args[i], "="); inline {
			values = append(values, strings.ToLower(strings.TrimSpace(value)))
			continue
		}
		if i+1 < len(args) && modelDeploymentConsumableValue(args[i+1]) {
			i++
			values = append(values, strings.ToLower(strings.TrimSpace(args[i])))
			continue
		}
		values = append(values, "")
	}

	return values
}

// extractLoadFormatFromArgs reads the effective --load-format selection from an argv:
// the last flag wins, and an argv naming none returns the empty string.
func extractLoadFormatFromArgs(engine string, args []string) string {
	values := modelDeploymentLoadFormats(engine, args)
	if len(values) == 0 {
		return ""
	}

	return values[len(values)-1]
}

// modelDeploymentRoleUsesModelExpress reports whether a role's own argv selects the
// ModelExpress load format. A role's own argv comes from ModelDeploymentRoleArgs (Command
// for a take-over role, ExtraArgs for a managed one); what the operator appends while
// rendering is not the role's own choice and is not consulted here.
func modelDeploymentRoleUsesModelExpress(engine string, role *workercore.ModelDeploymentRole) bool {
	if role == nil {
		return false
	}
	format := extractLoadFormatFromArgs(engine, ModelDeploymentRoleArgs(role))

	return format == modelDeploymentModelExpressLoadFormat ||
		format == modelDeploymentModelExpressLoadFormatAlias
}

// modelDeploymentUsesModelExpress reports whether any live role of the deployment loads
// through ModelExpress. A deleted deployment renders nothing.
func modelDeploymentUsesModelExpress(md *workercore.ModelDeployment) bool {
	if md == nil || !md.DeletionTimestamp.IsZero() {
		return false
	}
	for i := range md.Spec.Roles {
		if modelDeploymentRoleUsesModelExpress(md.Spec.Engine.Name, &md.Spec.Roles[i]) {
			return true
		}
	}

	return false
}

// modelDeploymentModelExpressResourceName names the sidecar's objects: the deployment's
// name with the sidecar's suffix. A name that would not fit a DNS label is settled by a
// digest of the pair, which neither panics on a long prefix nor collides the way two
// truncated names sharing one suffix would.
func modelDeploymentModelExpressResourceName(md *workercore.ModelDeployment) string {
	name := md.Name + "-modelexpress"
	if len(name) <= 63 {
		return name
	}
	tag := stringx.SumBySHA256(md.Name + "/modelexpress")[:8]
	room := 63 - len(tag) - 1

	return md.Name[:room] + "-" + tag
}

// modelDeploymentModelExpressServerAddress is how an engine replica reaches the sidecar:
// the Service's in-cluster address on the gRPC port.
func modelDeploymentModelExpressServerAddress(md *workercore.ModelDeployment) string {
	return modelDeploymentModelExpressResourceName(md) + "." + md.Namespace + ".svc" +
		fmt.Sprintf(":%d", modelDeploymentModelExpressServerPort)
}

// renderModelDeploymentModelExpressObjects renders the sidecar's object pair, or nothing
// for a deployment that loads no role through ModelExpress.
func renderModelDeploymentModelExpressObjects(
	ctx context.Context, md *workercore.ModelDeployment,
) (*appsv1.Deployment, *corev1.Service) {
	if !modelDeploymentUsesModelExpress(md) {
		return nil, nil
	}
	name := modelDeploymentModelExpressResourceName(md)
	labels := map[string]string{
		modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance: md.Name,
		"app.kubernetes.io/component":   modelDeploymentModelExpressComponentLabel,
	}
	note := func(object ctrlcli.Object) {
		systemmeta.NoteResource(object, ModelDeploymentResourceType,
			map[string]string{modelDeploymentResourceNoteModelExpress: md.Name})
		kubemeta.ControlOnWithoutBlock(object, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
	}

	port := modelDeploymentModelExpressServerPort
	container := corev1.Container{
		Name:    modelDeploymentModelExpressContainerName,
		Image:   redirectedImage(ctx, strings.TrimSpace(settings.ModelDeploymentModelExpressServerImage.ShouldValue(ctx))),
		Command: []string{"modelexpress-server"},
		Env: []corev1.EnvVar{
			{Name: "MX_METADATA_BACKEND", Value: "memory"},
			{Name: "MODEL_EXPRESS_SERVER_PORT", Value: fmt.Sprintf("%d", port)},
		},
		Ports: []corev1.ContainerPort{{
			Name: "grpc", ContainerPort: port, Protocol: corev1.ProtocolTCP,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(port),
				},
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(port),
				},
			},
		},
	}
	// RECREATE, NOT ROLLING. The server's state is one deployment's rendezvous: a rolling
	// update would run two servers at once and split the replicas' registrations between
	// them, and the readiness the old server still reports would be nobody's truth. One
	// server at a time, empty on replacement, is the contract the engines tolerate.
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &meta.LabelSelector{MatchLabels: maps.Clone(labels)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: maps.Clone(labels)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					Containers:                   []corev1.Container{container},
				},
			},
		},
	}
	note(deployment)

	service := &corev1.Service{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: maps.Clone(labels),
			Ports: []corev1.ServicePort{{
				Name: "grpc", Port: port, TargetPort: intstr.FromInt32(port),
				Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	note(service)

	return deployment, service
}

// syncModelDeploymentModelExpress converges the sidecar's objects onto the render, and
// prunes them when no role loads through ModelExpress anymore. Like the router sync, the
// caller logs a failure and carries on to the status write rather than aborting the pass.
func (r *ModelDeploymentReconciler) syncModelDeploymentModelExpress(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	deployment, service := renderModelDeploymentModelExpressObjects(ctx, md)
	wanted := deployment != nil
	if err := r.syncModelDeploymentOwnedChildren(ctx, md,
		objectIfPresent(deployment, wanted),
		func() ctrlcli.Object { return new(appsv1.Deployment) }, new(appsv1.DeploymentList),
		func(actual, expected ctrlcli.Object) bool {
			return alignModelDeploymentRouterDeployment(
				actual.(*appsv1.Deployment), expected.(*appsv1.Deployment),
			)
		}, modelDeploymentResourceNoteModelExpress, "deployment",
	); err != nil {
		return err
	}

	return r.syncModelDeploymentOwnedChildren(ctx, md,
		objectIfPresent(service, wanted),
		func() ctrlcli.Object { return new(corev1.Service) }, new(corev1.ServiceList),
		func(actual, expected ctrlcli.Object) bool {
			changed := alignModelDeploymentService(actual.(*corev1.Service), expected.(*corev1.Service))
			return alignModelDeploymentChildMetadata(actual, expected) || changed
		}, modelDeploymentResourceNoteModelExpress, "service",
	)
}

// modelDeploymentModelExpressDeliveryPolicy reads the cluster's remote delivery policy
// for the env an engine replica reads: Require names itself, every other effective
// policy renders Prefer.
func modelDeploymentModelExpressDeliveryPolicy(ctx context.Context) string {
	if modelDeploymentModelTransferPolicy(ctx) == settings.ModelArtifactRemoteDeliveryRequire {
		return modelDeploymentModelExpressDeliveryPolicyRequire
	}

	return "Prefer"
}

// modelExpressStaging is what one ModelExpress role's Pod gains over its base delivery:
// the volumes and their mounts, and the environment. Under a remote delivery the remote
// decoration has already mounted the client volumes and wrapped the command, so the
// staging adds ONLY what that decoration lacks: the ModelExpress environment and the
// load-format name. Under every other delivery the staging carries the client volumes —
// the SAME trio the remote decoration mounts, under the SAME names and paths — and the
// one command wrap. The weights volume is deliberately absent in both cases: the base
// delivery decides it, and a seed replica without a ready source keeps the node's own
// view to load from and publish.
type modelExpressStaging struct {
	volumes     []corev1.Volume
	mounts      []corev1.VolumeMount
	ownedEnv    []corev1.EnvVar
	env         []corev1.EnvVar
	wrapCommand bool
}

// modelExpressStagingFor renders that decoration for one role, or nil when the role does
// not load through ModelExpress or names no hub artifact to publish. backend is the
// transfer backend the role's pool and engine name, which the client requires whether or
// not the weights render remote: a source that becomes ready later is reached through
// the same env a remote replica reads.
func modelExpressStagingFor(
	ctx context.Context, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	artifact *ModelDeploymentArtifactRender, compileDigest string,
	scheme corev1.URIScheme, enginePort int32, gradable bool, backend string,
) *modelExpressStaging {
	if !modelDeploymentRoleUsesModelExpress(md.Spec.Engine.Name, role) ||
		artifact == nil || artifact.Hub != modelDeploymentHubHuggingFace {
		return nil
	}
	env := modelExpressDeliveryEnv(ctx, md)
	env = append(env,
		corev1.EnvVar{
			Name:  modelDeploymentModelExpressEnvArtifactReadyURL,
			Value: modelDeploymentModelExpressReadyURL(scheme, enginePort, gradable),
		},
		corev1.EnvVar{Name: modelDeploymentModelExpressEnvCompileDigest, Value: compileDigest},
	)
	if backend != "" {
		// The backend the client dials is required under every delivery: a
		// Node-rendered seed replica reaches a source that becomes ready later
		// through the same env a remote replica reads.
		env = append(env, corev1.EnvVar{Name: modelDeploymentModelTransferEnvBackend, Value: backend})
	}
	staging := &modelExpressStaging{env: env}
	if artifact.Delivery == workercore.ModelDeploymentModelDeliveryRemote {
		// The remote decoration has mounted the client volumes, wrapped the command,
		// and rendered the client env: this staging adds nothing those steps already
		// did, or the pod would carry a duplicate volume, a double mount, and a
		// doubly nested bootstrap. The load-format name it corrects below is the one
		// thing the remote decoration gets wrong for this role.
		return staging
	}
	// Under a Node or Engine delivery nothing else has decorated the container, so the
	// staging carries the client volumes and the full client env itself.
	staging.wrapCommand = true
	staging.volumes, staging.mounts = modelTransferClientVolumes(artifact.ModelTransferConfigMap)
	staging.ownedEnv = artifact.modelTransferEnv()
	staging.env = append(staging.env, corev1.EnvVar{
		Name:  modelDeploymentModelTransferEnvTimeout,
		Value: modelDeploymentModelTransferDefaultTimeoutSeconds,
	})
	if backend == "nixl" {
		nixlPlugin := "UCX"
		if artifact.ModelTransferEFA {
			nixlPlugin = "LIBFABRIC"
		}
		staging.env = append(staging.env, corev1.EnvVar{
			Name:  modelDeploymentModelTransferEnvNIXLBackend,
			Value: nixlPlugin,
		})
	}

	return staging
}

// applyToContainer lands the parts of the staging that live on the main container: the
// one command wrap a non-remote delivery needs, the environment the client and the
// plugin hook read, and the mounts. The load-format env is corrected last, so a remote
// decoration's model-transfer name cannot stand where this role reads modelexpress.
func (s *modelExpressStaging) applyToContainer(c *corev1.Container) {
	if s.wrapCommand {
		c.Command = modelTransferWrapCommand(c.Command)
	}
	for _, e := range s.ownedEnv {
		c.Env = setModelDeploymentEnvVar(c.Env, e)
	}
	c.Env = appendModelDeploymentEnvIfAbsent(c.Env, s.env)
	c.Env = setModelDeploymentEnv(c.Env, modelDeploymentModelTransferEnvLoadFormat,
		modelDeploymentModelExpressLoadFormat)
	c.VolumeMounts = append(c.VolumeMounts, s.mounts...)
}

// applyToPod lands the pod-level parts of the staging: the client volumes a non-remote
// delivery needs.
func (s *modelExpressStaging) applyToPod(pod *corev1.Pod) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, s.volumes...)
}

// setModelDeploymentEnv replaces one environment entry in place, or appends it when the
// container carries none, and drops any later duplicate of the same name so only one
// entry survives.
func setModelDeploymentEnv(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	return setModelDeploymentEnvVar(env, corev1.EnvVar{Name: name, Value: value})
}

// setModelDeploymentEnvVar replaces one environment entry in place, or appends it when
// the container carries none, and drops any later duplicate of the same name so only one
// entry survives. The operator's own renders are the values this helper settles: a user
// entry that admission let through a different name is untouched.
func setModelDeploymentEnvVar(env []corev1.EnvVar, entry corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if e.Name == entry.Name {
			if !replaced {
				out = append(out, *entry.DeepCopy())
				replaced = true
			}

			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, *entry.DeepCopy())
	}

	return out
}

// appendModelDeploymentEnvIfAbsent adds the operator's entries to a container's env,
// skipping a name the container already carries and skipping an empty entry: a user's
// own entry wins, because the operator's value here is a default the user can aim
// differently, and an entry with nothing to say is not rendered at all.
func appendModelDeploymentEnvIfAbsent(env, add []corev1.EnvVar) []corev1.EnvVar {
	for _, e := range add {
		if e.Value == "" && e.ValueFrom == nil {
			continue
		}
		exists := false
		for i := range env {
			if env[i].Name == e.Name {
				exists = true

				break
			}
		}
		if !exists {
			env = append(env, *e.DeepCopy())
		}
	}

	return env
}

// modelExpressDeliveryEnv is the environment one ModelExpress replica reads beyond what
// the transfer client env already carries: the sidecar address, the replica's own
// address, the plugin hook, and the delivery policy.
func modelExpressDeliveryEnv(
	ctx context.Context, md *workercore.ModelDeployment,
) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: modelDeploymentModelExpressEnvServerAddress, Value: modelDeploymentModelExpressServerAddress(md)},
		{Name: modelDeploymentModelExpressEnvWorkerHost, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"},
		}},
		{Name: modelDeploymentModelExpressEnvArtifactTransfer, Value: "1"},
		{Name: modelDeploymentModelExpressEnvLoadPlugin, Value: modelDeploymentModelExpressEnvLoadPluginGPUStack},
		{Name: modelDeploymentModelExpressEnvLoadPluginBefore, Value: modelDeploymentModelExpressLoadPluginBeforeValue},
		{Name: modelDeploymentModelTransferEnvDeliveryPolicy, Value: modelDeploymentModelExpressDeliveryPolicy(ctx)},
	}
}

// modelDeploymentModelExpressReadyURL is where the artifact-ready hook grades this
// replica: its own engine's health endpoint. A role whose engine the gates cannot grade,
// or one with no serving port, publishes none.
func modelDeploymentModelExpressReadyURL(
	scheme corev1.URIScheme, enginePort int32, gradable bool,
) string {
	if !gradable || enginePort <= 0 {
		return ""
	}
	if scheme == "" {
		scheme = corev1.URISchemeHTTP
	}

	return strings.ToLower(string(scheme)) + "://127.0.0.1:" + fmt.Sprintf("%d", enginePort) + "/health"
}

// modelDeploymentModelExpressCompileConfigDigest names one role's compilation identity:
// a digest over the WHOLE compilation-cache subpath, so the hardware that ran the
// compiler, the engine that drove it, the parallel shape it compiled for, and the
// configuration digest beneath them all reach the source identity. Two roles that would
// not share compiled kernels never share a published identity either.
func modelDeploymentModelExpressCompileConfigDigest(compileCacheSubPath string) string {
	if compileCacheSubPath == "" {
		return ""
	}

	return stringx.SumBySHA256(compileCacheSubPath)[:16]
}
