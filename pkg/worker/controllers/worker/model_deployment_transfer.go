package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// The model-transfer delivery's rendering contract: what a remote replica mounts, who
// it authenticates as, and where the staged wheel waits.
const (
	// modelDeploymentModelTransferWheelVolumeName names the host-staged wheel volume.
	modelDeploymentModelTransferWheelVolumeName = "gpustack-model-transfer-wheel"
	// modelDeploymentModelTransferWheelHostPath is the host directory device-manager
	// stages the universal gpustack_model_transfer wheel into at startup (populated
	// from /etc/gpustack/lib/model-transfer by the stage-libs initContainer).
	modelDeploymentModelTransferWheelHostPath = "/var/lib/gpustack/operator/lib/model-transfer"
	// modelDeploymentModelTransferWheelContainerPath is where that directory is
	// mounted read-only inside the engine container, and the find-links
	// root the offline install reads.
	modelDeploymentModelTransferWheelContainerPath = modelDeploymentModelTransferWheelHostPath
	// modelDeploymentModelTransferTokenVolumeName names the projected ServiceAccount
	// token volume a remote replica authenticates to the source broker with.
	modelDeploymentModelTransferTokenVolumeName = "gpustack-model-transfer-token"
	// modelDeploymentModelTransferTokenPath is where that token is mounted.
	modelDeploymentModelTransferTokenPath = "/var/run/secrets/gpustack.io/model-transfer"
	// modelDeploymentModelTransferTokenAudience is the TokenReview audience the source
	// broker reviews session tokens against; the projected token is minted
	// for exactly this audience and nothing else.
	modelDeploymentModelTransferTokenAudience = authorization.Audience
	// modelDeploymentModelTransferTokenExpirySeconds bounds the projected token's
	// lifetime; kubelet rotates it as the window closes.
	modelDeploymentModelTransferTokenExpirySeconds = int64(3600)
	// modelDeploymentModelTransferLoadFormat is the load-format value every supported
	// engine passes its loader, selecting the model-transfer adapter.
	modelDeploymentModelTransferLoadFormat = "model-transfer"

	// The CSI volume attribute that asks the node plugin for a metadata-only
	// delivery: the key and value the driver's own model-transfer contract pins. A
	// volume carrying it never sees the complete-tree materializer and never
	// mounts a published tree.
	modelDeploymentModelTransferVolumeAttrDelivery = "model-transfer.gpustack.ai/delivery"
	modelDeploymentModelTransferDeliveryMetadata   = "metadata"

	// The pod annotation carrying the delivery a replica actually renders
	// under, stamped at render and read by the status and the fallback sweep.
	modelDeploymentModelTransferDeliveryAnnotation = "model-transfer.gpustack.ai/delivery"

	// The fallback's bound and reasons. An attempt is one remote-to-fallback
	// transition, counted in the status the next render reads its override
	// from; past the bound the overrun is the visible reason, and the
	// fallback itself is still where the replica lands.
	modelDeploymentModelTransferAttemptsBound        = int32(3)
	modelDeploymentModelTransferReasonLoadFailed     = "RemoteLoadFailed"
	modelDeploymentModelTransferReasonAttemptsSpent  = "FallbackAttemptsSpent"
	modelDeploymentModelTransferReasonRoleIneligible = "RoleIneligibleForModelTransfer"
	// modelDeploymentModelTransferReasonUnqualifiedBackend says the role's engine
	// and pool name a transfer backend that no acceptance has qualified. It is a
	// different answer from an ineligible role: the combination is supported,
	// and what is missing is the proof that the source is protected.
	modelDeploymentModelTransferReasonUnqualifiedBackend = "UnqualifiedTransferBackend"
)

// modelArtifactRemoteState is what a pass knows about the remote delivery before
// it renders anything: the deployment's own remote view from its last pass, and
// whether a live replica still serves the remote delivery.
//
// The two facts answer one question from two directions, and either one alone can
// be behind. The view is a report of an earlier pass and can name no replica
// while one still serves; the Pods are what the cluster holds right now.
type modelArtifactRemoteState struct {
	// Carried is the deployment's own remote status from its last pass.
	Carried *workercore.ModelDeploymentModelRemoteStatus
	// LiveRemote names the replicas a live Pod still renders on the remote delivery.
	LiveRemote sets.Set[string]
}

// remoteHeld says a replica still serves the remote delivery, whether the
// deployment's own view says so or a live Pod does.
func (s *modelArtifactRemoteState) remoteHeld() bool {
	if s == nil {
		return false
	}

	return s.LiveRemote.Len() > 0 || modelTransferCarriesRemote(s.Carried)
}

// liveRemoteOrdinals names the replicas a live Pod still renders on the remote
// delivery.
func (s *modelArtifactRemoteState) liveRemoteOrdinals() sets.Set[string] {
	if s == nil {
		return nil
	}

	return s.LiveRemote
}

// applyModelDeploymentModelTransferPolicy resolves a hub artifact's delivery under
// the cluster's remote policy. It runs after the base delivery (Node or
// Engine, per the delivery mode) resolved and only on an unblocked hub
// render: the upgrade carries the artifact's identity to the remote render
// with the base delivery as its declared fallback — Node for a
// verified-node-only artifact, never a weakened engine download — and
// Require engages even with no prepared source, because its missing source
// is a visible wait, not a permission problem. A policy whose CSIDriver has
// disappeared blocks under Require and falls back under Prefer: the plugin
// is what serves the metadata view a remote replica mounts.
//
// remoteState says what the deployment still serves remotely. A Prefer deployment
// whose source drained keeps the remote delivery while any of its replicas still
// renders remote: flipping the render back to the fallback would change the
// desired spec and roll exactly the replicas that already loaded, which is the
// one thing a source loss must never do.
func applyModelDeploymentModelTransferPolicy(
	ctx context.Context, cli ctrlcli.Reader, w *modelArtifactWeights, ma *workercore.ModelArtifact,
	remoteState *modelArtifactRemoteState,
) error {
	if w == nil || w.Render == nil || w.Blocked {
		return nil
	}
	if w.Render.Delivery != workercore.ModelDeploymentModelDeliveryNode &&
		w.Render.Delivery != workercore.ModelDeploymentModelDeliveryEngine {
		return nil
	}
	policy := modelDeploymentModelTransferPolicy(ctx)
	if policy != settings.ModelArtifactRemoteDeliveryRequire &&
		policy != settings.ModelArtifactRemoteDeliveryPrefer {
		return nil
	}
	resolved := ma.Status.Resolved
	if resolved == nil || resolved.ManifestDigest == "" {
		return nil
	}
	ok, err := csiModelDriverExists(ctx, cli)
	if err != nil {
		return fmt.Errorf("read model-manager CSIDriver: %w", err)
	}
	if !ok {
		if policy == settings.ModelArtifactRemoteDeliveryRequire {
			w.Blocked, w.Reason = true, modelWeightsReasonNodeUnavailable
			w.Message = fmt.Sprintf("the %s remote delivery policy needs the model-manager plugin, and the "+
				"CSIDriver %s does not exist", policy, modelstore.DriverName)
		}

		return nil
	}

	sourceReady, sourceReadyUntil, err := modelDeploymentModelTransferSourceReady(ctx, cli, ma)
	if err != nil {
		return err
	}
	w.sourceReadyUntil = sourceReadyUntil
	if policy == settings.ModelArtifactRemoteDeliveryPrefer && !sourceReady && !remoteState.remoteHeld() {
		// Nothing is prepared and no replica holds the delivery remotely:
		// the declared fallback is the delivery, and the policy has nothing
		// to prefer. With a replica still up, the delivery stays remote and
		// unprepared: a source loss never rolls a replica that serves onto a
		// disk delivery.
		return nil
	}

	base := *w.Render
	remote := *w.Render
	remote.Delivery = workercore.ModelDeploymentModelDeliveryRemote
	remote.ModelTransferPolicy = policy
	remote.SourceReady = sourceReady
	if policy == settings.ModelArtifactRemoteDeliveryPrefer {
		// Only Prefer declares a fallback: Require's answer to every
		// failure is the visible wait, and its render names no fallback a
		// reader could mistake for permission.
		remote.ModelTransferFallback = base.Delivery
	}
	remote.VerifiedNodeOnly = ma.Spec.ExpectedDigest != "" ||
		len(ma.Spec.AllowPatterns) > 0 || len(ma.Spec.IgnorePatterns) > 0
	// The metadata view is addressed by the digest, so the remote render
	// carries the node delivery's identity whatever the base was.
	remote.ArtifactName = ma.Name
	remote.ArtifactUID = string(ma.UID)
	remote.ManifestDigest = resolved.ManifestDigest
	w.Render = &remote
	return nil
}

// csiModelDriverExists reads whether the plugin's CSIDriver object exists,
// through the supplied reader. Controller callers use their informer cache.
func csiModelDriverExists(ctx context.Context, cli ctrlcli.Reader) (bool, error) {
	err := cli.Get(ctx, ctrlcli.ObjectKey{Name: modelstore.DriverName}, &storage.CSIDriver{})
	switch {
	case err == nil:
		return true, nil
	case kerrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// modelDeploymentModelTransferPolicy is the cluster's remote delivery policy as a
// ModelDeployment's reconcile reads it. It is a variable so a test can
// choose a policy without a Settings store.
var modelDeploymentModelTransferPolicy = func(ctx context.Context) string {
	return settings.ModelArtifactRemoteDeliveryPolicy.ShouldValueFromRemote(ctx)
}

// modelDeploymentModelTransferEngines are the engines a remote delivery qualifies: a
// managed role whose engine loads weights through one of the local-reader
// adapters. vLLM-Ascend is not a separate engine here — it is the vLLM
// engine on an Ascend pool, and the pool's manufacturer picks the backend.
func modelDeploymentModelTransferEngineSupported(engine string) bool {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang:
		return true
	default:
		return false
	}
}

// modelTransferTuple names one (manufacturer, engine, backend) combination. A
// backend alone names no combination: the same backend on another engine or
// another hardware is another one.
type modelTransferTuple struct {
	Manufacturer string
	Engine       string
	Backend      string
}

// modelDeploymentModelTransferQualified answers whether a transfer tuple may use
// its backend. It is a variable so a test can install a scoped fixture.
var modelDeploymentModelTransferQualified = modelDeploymentModelTransferDefaultQualified

// modelDeploymentModelTransferDefaultQualified admits the NVIDIA NIXL tuples
// for vLLM and SGLang after policy opt-in. This routing allowlist does not
// establish hardware or safety acceptance.
func modelDeploymentModelTransferDefaultQualified(tuple modelTransferTuple) bool {
	return tuple.Manufacturer == nodefeature.ManufacturerNVIDIA &&
		tuple.Backend == "nixl" &&
		modelDeploymentModelTransferEngineSupported(tuple.Engine)
}

// modelDeploymentModelTransferNoTupleQualified is the fixture answer when no
// tuple has passed a source-safety acceptance.
func modelDeploymentModelTransferNoTupleQualified(modelTransferTuple) bool {
	return false
}

// modelDeploymentModelTransferCandidateBackend names the transfer backend a
// pool's engine would use, or "" when the combination names none: NVIDIA
// pairs supported engines with nixl, and Ascend pairs supported engines with hixl.
// A candidate is not a qualification; modelDeploymentModelTransferQualifiedBackend
// decides whether the tuple may use it.
func modelDeploymentModelTransferCandidateBackend(manufacturer, engine string) string {
	switch manufacturer {
	case nodefeature.ManufacturerNVIDIA:
		if modelDeploymentModelTransferEngineSupported(engine) {
			return "nixl"
		}
	case nodefeature.ManufacturerAscend:
		if modelDeploymentModelTransferEngineSupported(engine) {
			return "hixl"
		}
	default:
		return ""
	}

	return ""
}

// modelDeploymentModelTransferQualifiedBackend names the transfer backend a
// pool may use, or "" when no acceptance has qualified that exact tuple.
func modelDeploymentModelTransferQualifiedBackend(manufacturer, engine string) string {
	candidate := modelDeploymentModelTransferCandidateBackend(manufacturer, engine)
	if candidate == "" {
		return ""
	}
	tuple := modelTransferTuple{Manufacturer: manufacturer, Engine: engine, Backend: candidate}
	if !modelDeploymentModelTransferQualified(tuple) {
		return ""
	}

	return candidate
}

// modelTransferQualification is how a role stands against the qualification
// gate.
type modelTransferQualification int

const (
	// modelTransferTupleQualified means the role's tuple passed its
	// acceptance and the role may take a remote delivery.
	modelTransferTupleQualified modelTransferQualification = iota
	// modelTransferTupleUnqualified means the role's engine and pool name a
	// candidate backend, but no acceptance has qualified that tuple.
	modelTransferTupleUnqualified
	// modelTransferRoleIneligible means the role cannot take a remote
	// delivery at all.
	modelTransferRoleIneligible
)

// modelDeploymentModelTransferRoleQualification says how a role stands against
// the qualification gate: a managed command (the operator built the argv, so
// the bootstrap and the load format are the operator's to add), a supported
// engine, and a tuple an acceptance has qualified. A take-over argv belongs to
// its author, and a remote delivery must not silently rewrite it.
//
// The three answers differ: an ineligible role names no backend at all, while
// an unqualified one names a backend that no acceptance has qualified.
func modelDeploymentModelTransferRoleQualification(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
) modelTransferQualification {
	if len(role.Command) > 0 || modelDeploymentRoleHasConflictingLoadFormat(role.ExtraArgs) {
		return modelTransferRoleIneligible
	}
	if modelDeploymentModelTransferCandidateBackend(manufacturer, md.Spec.Engine.Name) == "" {
		return modelTransferRoleIneligible
	}
	if modelDeploymentModelTransferQualifiedBackend(manufacturer, md.Spec.Engine.Name) == "" {
		return modelTransferTupleUnqualified
	}

	return modelTransferTupleQualified
}

// modelDeploymentRoleHasConflictingLoadFormat reports whether args set a
// --load-format other than model-transfer, or repeat the flag.
func modelDeploymentRoleHasConflictingLoadFormat(args []string) bool {
	count := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--load-format" {
			count++
			if i+1 >= len(args) || args[i+1] != modelDeploymentModelTransferLoadFormat {
				return true
			}
			i++
			continue
		}
		if val, ok := strings.CutPrefix(arg, "--load-format="); ok {
			count++
			if val != modelDeploymentModelTransferLoadFormat {
				return true
			}
		}
	}

	return count > 1
}

// modelDeploymentArgsHaveLoadFormat reports whether args already specify
// --load-format in either two-token or --load-format=value form.
func modelDeploymentArgsHaveLoadFormat(args []string) bool {
	for _, arg := range args {
		if arg == "--load-format" || strings.HasPrefix(arg, "--load-format=") {
			return true
		}
	}

	return false
}

// modelDeploymentModelTransferReplicaKey names a role and its replica ordinal.
func modelDeploymentModelTransferReplicaKey(role string, ordinal int) string {
	return role + "/" + strconv.Itoa(ordinal)
}

// modelDeploymentModelTransferReplicaKeyOf names the replica a Pod belongs to.
func modelDeploymentModelTransferReplicaKeyOf(pod *core.Pod) string {
	return modelDeploymentModelTransferReplicaKey(
		pod.Labels[modelDeploymentLabelKeyComponent], modelDeploymentModelTransferOrdinal(pod),
	)
}

// modelDeploymentModelTransferLiveRemote names the replicas a live Pod still renders
// on the remote delivery.
//
// The Pod is the truth about that: the deployment's own remote view is a report of
// an earlier pass, and a report can name no replica while one still renders the
// delivery.
//
// A replica whose load definitively failed is not one: the sweep owns that replica
// and recreates it on the declared fallback. A replica that is merely not ready is
// one, because readiness is no proof that it never loaded.
func modelDeploymentModelTransferLiveRemote(pods []core.Pod) sets.Set[string] {
	live := sets.New[string]()
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Annotations[modelDeploymentModelTransferDeliveryAnnotation] !=
			string(workercore.ModelDeploymentModelDeliveryRemote) {
			continue
		}
		if modelDeploymentModelTransferLoadFailed(pod) {
			continue
		}
		live.Insert(modelDeploymentModelTransferReplicaKeyOf(pod))
	}

	return live
}

// modelDeploymentModelTransferSourceReady observes whether a prepared source serves
// the artifact's digest. It is a variable so a test can reach the state a prepared
// source produces; production always runs the observation.
var modelDeploymentModelTransferSourceReady = modelDeploymentModelTransferSourceObserved

// modelTransferForRole resolves a role's default weights render under a remote delivery.
// A qualified role keeps the remote render. A role whose tuple no acceptance has
// qualified takes Prefer's declared fallback, and blocks when there is no
// fallback to take or when the fallback would turn over a replica that already
// loaded: a fallback that rolls a serving replica is not a fallback. The reason
// and message are non-empty exactly when the role is blocked.
//
// servesRemote says a live replica still serves the remote delivery. The
// deployment's own remote view is a report of an earlier pass, so the Pods are
// the truth the hold rests on.
func (a *ModelDeploymentArtifactRender) modelTransferForRole(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
	servesRemote bool,
) (*ModelDeploymentArtifactRender, string, string) {
	qualification := modelDeploymentModelTransferRoleQualification(md, role, manufacturer)
	if qualification == modelTransferTupleQualified {
		remote := *a
		return &remote, "", ""
	}
	fallback := a.modelTransferFallbackRender()
	if qualification == modelTransferTupleUnqualified {
		// An unqualified tuple starts no new remote replica, and the declared
		// fallback is the answer only when it is safe to take. A fallback
		// changes the desired delivery, and the rollout then turns the replica
		// that already loaded over.
		candidate := modelDeploymentModelTransferCandidateBackend(manufacturer, md.Spec.Engine.Name)
		if fallback == nil {
			return nil, modelDeploymentModelTransferReasonUnqualifiedBackend, fmt.Sprintf(
				"role %q on a %s pool runs %s, whose %s transfer backend has passed no source-safety acceptance, "+
					"and the %s policy never falls back: no remote delivery starts until an acceptance qualifies "+
					"that exact tuple",
				role.Name, manufacturer, md.Spec.Engine.Name, candidate,
				workercore.ModelDeploymentRemotePolicyRequire,
			)
		}
		if servesRemote || modelDeploymentModelTransferRoleCarriesRemote(md, role.Name) {
			return nil, modelDeploymentModelTransferReasonUnqualifiedBackend, fmt.Sprintf(
				"role %q on a %s pool runs %s, whose %s transfer backend has passed no source-safety acceptance, "+
					"and the declared fallback would roll the replica that already loaded: the delivery is held "+
					"and no remote replica starts",
				role.Name, manufacturer, md.Spec.Engine.Name, candidate,
			)
		}
	}
	if fallback == nil {
		return nil, modelDeploymentModelTransferReasonRoleIneligible, fmt.Sprintf(
			"role %q on a %s pool runs %s, which names no transfer backend at all, and the %s policy "+
				"never falls back: choose a pool whose engine names one, or the Prefer policy",
			role.Name, manufacturer, md.Spec.Engine.Name, workercore.ModelDeploymentRemotePolicyRequire,
		)
	}
	if servesRemote || modelDeploymentModelTransferRoleCarriesRemote(md, role.Name) {
		return nil, modelDeploymentModelTransferReasonRoleIneligible, fmt.Sprintf(
			"role %q on a %s pool runs %s, which is ineligible for remote weight transfer, "+
				"and the declared fallback would roll the replica that already loaded: the delivery is held "+
				"and no remote replica starts",
			role.Name, manufacturer, md.Spec.Engine.Name,
		)
	}

	return fallback, "", ""
}

func modelDeploymentModelTransferRoleLive(live sets.Set[string], role string) bool {
	for key := range live {
		if strings.HasPrefix(key, role+"/") {
			return true
		}
	}
	return false
}

func modelDeploymentModelTransferRoleCarriesRemote(md *workercore.ModelDeployment, role string) bool {
	if md.Status.Model == nil || md.Status.Model.Remote == nil {
		return false
	}
	for _, carried := range md.Status.Model.Remote.Roles {
		if carried.Role != role {
			continue
		}
		for _, replica := range carried.Replicas {
			if replica.Delivery == workercore.ModelDeploymentModelDeliveryRemote {
				return true
			}
		}
	}
	return false
}

// modelTransferCarriesRemote says whether the deployment's own remote status still
// names a replica on the remote delivery.
func modelTransferCarriesRemote(carried *workercore.ModelDeploymentModelRemoteStatus) bool {
	if carried == nil {
		return false
	}
	for i := range carried.Roles {
		for j := range carried.Roles[i].Replicas {
			if carried.Roles[i].Replicas[j].Delivery == workercore.ModelDeploymentModelDeliveryRemote {
				return true
			}
		}
	}

	return false
}

// modelTransferFallbackRender is the render's own declared fallback.
func (a *ModelDeploymentArtifactRender) modelTransferFallbackRender() *ModelDeploymentArtifactRender {
	if a.ModelTransferFallback == "" {
		return nil
	}

	return a.modelTransferFallbackRenderFor(a.ModelTransferFallback)
}

// modelTransferFallbackRenderFor returns the named identity-preserving fallback as
// its own render. A VERIFIED-NODE-ONLY artifact — one carrying an expected
// digest or file patterns, which only the node can honor — falls back to
// Node and is never weakened to an unverified engine download.
func (a *ModelDeploymentArtifactRender) modelTransferFallbackRenderFor(
	fallback workercore.ModelDeploymentModelDelivery,
) *ModelDeploymentArtifactRender {
	if fallback == workercore.ModelDeploymentModelDeliveryEngine && a.VerifiedNodeOnly {
		// The engine delivery cannot anchor-verify or filter; the node
		// fallback is what keeps the artifact's identity honest.
		fallback = workercore.ModelDeploymentModelDeliveryNode
	}
	switch fallback {
	case workercore.ModelDeploymentModelDeliveryNode, workercore.ModelDeploymentModelDeliveryEngine:
		out := *a
		out.Delivery = fallback
		out.ModelTransferFallback = ""

		return &out
	default:
		return nil
	}
}

// applyModelTransfer decorates a rendered main container for a remote delivery: the
// bootstrap that installs the staged wheel before the engine starts, the
// load format that selects the model-transfer adapter, and the env the adapter reads.
// It is a no-op for every other delivery.
func (a *ModelDeploymentArtifactRender) applyModelTransfer(c *core.Container) {
	if a.Delivery != workercore.ModelDeploymentModelDeliveryRemote {
		return
	}
	c.Command = modelTransferWrapCommand(c.Command)
	if !modelDeploymentArgsHaveLoadFormat(c.Args) {
		c.Args = append(c.Args, "--load-format", modelDeploymentModelTransferLoadFormat)
	}
	c.Env = append(c.Env, a.modelTransferEnv()...)
	has := false
	for i := range c.Env {
		if c.Env[i].Name == modelDeploymentModelTransferEnvTimeout {
			has = true
			break
		}
	}
	if !has {
		c.Env = append(c.Env, core.EnvVar{
			Name:  modelDeploymentModelTransferEnvTimeout,
			Value: modelDeploymentModelTransferDefaultTimeoutSeconds,
		})
	}
}

// modelDeploymentModelTransferBootstrapScript installs the staged wheel before the
// engine starts, and only when it is not already importable: the probe
// costs one interpreter start, the install runs offline from the staged
// wheel directory, and the exec that follows hands the ORIGINAL argv the
// process identity, signals and all.
const modelDeploymentModelTransferBootstrapScript = "set -e\npython3 -c 'import importlib.util, sys; " +
	"sys.exit(0 if importlib.util.find_spec(\"gpustack_model_transfer\") else 1)' || " +
	"pip install --no-deps --no-index --find-links " + modelDeploymentModelTransferWheelContainerPath +
	" gpustack-model-transfer\nexec \"$@\""

// modelTransferWrapCommand wraps an engine argv behind that bootstrap.
func modelTransferWrapCommand(argv []string) []string {
	out := make([]string, 0, len(argv)+3)
	out = append(out, "sh", "-c", modelDeploymentModelTransferBootstrapScript, "sh")

	return append(out, argv...)
}

// modelTransferEnv is the environment a remote replica's engine reads.
func (a *ModelDeploymentArtifactRender) modelTransferEnv() []core.EnvVar {
	env := []core.EnvVar{
		{Name: modelDeploymentModelTransferEnvArtifact, Value: a.ArtifactName},
		{Name: modelDeploymentModelTransferEnvArtifactUID, Value: a.ArtifactUID},
		{Name: modelDeploymentModelTransferEnvManifestDigest, Value: a.ManifestDigest},
		{Name: modelDeploymentModelTransferEnvMountPath, Value: ModelDeploymentModelMountPath},
		{Name: modelDeploymentModelTransferEnvToken, Value: modelDeploymentModelTransferTokenFile},
		{Name: modelDeploymentModelTransferEnvWheelDir, Value: modelDeploymentModelTransferWheelContainerPath},
		{Name: modelDeploymentModelTransferEnvLoadFormat, Value: modelDeploymentModelTransferLoadFormat},
	}
	if a.ModelTransferBackend != "" {
		env = append(env, core.EnvVar{Name: modelDeploymentModelTransferEnvBackend, Value: a.ModelTransferBackend})
	}
	if a.ModelTransferConfigMap != "" {
		env = append(env, core.EnvVar{Name: "GPUSTACK_MODEL_TRANSFER_CONFIG", Value: modelTransferClientMount + "/endpoints.json"})
	}
	for _, identity := range []struct{ name, path string }{
		{"KUBERNETES_POD_UID", "metadata.uid"},
		{"KUBERNETES_POD_NAME", "metadata.name"},
		{"KUBERNETES_POD_NAMESPACE", "metadata.namespace"},
		{"KUBERNETES_POD_IP", "status.podIP"},
	} {
		env = append(env, core.EnvVar{Name: identity.name, ValueFrom: &core.EnvVarSource{
			FieldRef: &core.ObjectFieldSelector{FieldPath: identity.path},
		}})
	}

	return env
}

// The env entries a remote replica's engine reads.
const (
	modelDeploymentModelTransferEnvArtifact       = "GPUSTACK_MODEL_TRANSFER_ARTIFACT"
	modelDeploymentModelTransferEnvArtifactUID    = "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID"
	modelDeploymentModelTransferEnvManifestDigest = "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST"
	modelDeploymentModelTransferEnvMountPath      = "GPUSTACK_MODEL_TRANSFER_MODEL_PATH"
	modelDeploymentModelTransferEnvToken          = "GPUSTACK_MODEL_TRANSFER_TOKEN"
	modelDeploymentModelTransferEnvWheelDir       = "GPUSTACK_MODEL_TRANSFER_WHEEL_DIR"
	modelDeploymentModelTransferEnvLoadFormat     = "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT"
	modelDeploymentModelTransferEnvBackend        = "GPUSTACK_MODEL_TRANSFER_BACKEND"
	// modelDeploymentModelTransferEnvTimeout bounds one remote transfer: the
	// client's probe and operation waits and the HCCL collective timeout the
	// plugin sets from it share the figure, because a broadcast queued on the
	// collective stream must outlive the fill it carries.
	modelDeploymentModelTransferEnvTimeout            = "GPUSTACK_MODEL_TRANSFER_TIMEOUT"
	modelDeploymentModelTransferDefaultTimeoutSeconds = "1800"
	modelDeploymentModelTransferTokenFile             = modelDeploymentModelTransferTokenPath + "/token"
)

// modelTransferVolumes is what a remote replica mounts: the node plugin's
// METADATA-ONLY view of the weights — never the complete tree — the
// ServiceAccount token the source broker reviews, and the host-staged wheel
// directory the bootstrap installs from. All read-only.
func (a *ModelDeploymentArtifactRender) modelTransferVolumes() ([]core.Volume, []core.VolumeMount) {
	volumes := []core.Volume{
		{
			Name: modelDeploymentModelVolumeName,
			VolumeSource: core.VolumeSource{CSI: &core.CSIVolumeSource{
				Driver:   modelstore.DriverName,
				ReadOnly: ptr.To(true),
				VolumeAttributes: map[string]string{
					modelArtifactVolumeAttrArtifact:                a.ArtifactName,
					modelArtifactVolumeAttrArtifactUID:             a.ArtifactUID,
					modelArtifactVolumeAttrDigest:                  a.ManifestDigest,
					modelDeploymentModelTransferVolumeAttrDelivery: modelDeploymentModelTransferDeliveryMetadata,
				},
			}},
		},
		{
			Name: modelDeploymentModelTransferTokenVolumeName,
			VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{
				Sources: []core.VolumeProjection{{
					ServiceAccountToken: &core.ServiceAccountTokenProjection{
						Audience:          modelDeploymentModelTransferTokenAudience,
						ExpirationSeconds: ptr.To(modelDeploymentModelTransferTokenExpirySeconds),
						Path:              "token",
					},
				}},
			}},
		},
		{
			Name: modelDeploymentModelTransferWheelVolumeName,
			VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{
				Path: modelDeploymentModelTransferWheelHostPath,
				Type: ptr.To(core.HostPathDirectoryOrCreate),
			}},
		},
	}
	mounts := []core.VolumeMount{
		{
			Name:      modelDeploymentModelVolumeName,
			MountPath: ModelDeploymentModelMountPath,
			ReadOnly:  true,
		},
		{
			Name:      modelDeploymentModelTransferTokenVolumeName,
			MountPath: modelDeploymentModelTransferTokenPath,
			ReadOnly:  true,
		},
		{
			Name:      modelDeploymentModelTransferWheelVolumeName,
			MountPath: modelDeploymentModelTransferWheelContainerPath,
			ReadOnly:  true,
		},
	}
	if a.ModelTransferConfigMap != "" {
		volumes[0].CSI.VolumeAttributes[modelstore.ModelTransferClientConfigAttribute] = a.ModelTransferConfigMap
		volumes = append(volumes, core.Volume{Name: "gpustack-model-transfer-config", VolumeSource: core.VolumeSource{
			ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: a.ModelTransferConfigMap}},
		}})
		mounts = append(mounts, core.VolumeMount{Name: "gpustack-model-transfer-config", MountPath: modelTransferClientMount, ReadOnly: true})
	}

	return volumes, mounts
}

// modelDeploymentModelTransferReplicaOverride reads the persisted delivery override
// for one replica: the fallback a bounded transition put it on. The empty
// delivery means the replica renders remote as the policy resolved it.
func modelDeploymentModelTransferReplicaOverride(
	md *workercore.ModelDeployment, role string, ordinal int,
) workercore.ModelDeploymentModelDelivery {
	if md.Status.Model == nil || md.Status.Model.Remote == nil {
		return ""
	}
	remote := md.Status.Model.Remote
	for i := range remote.Roles {
		if remote.Roles[i].Role != role {
			continue
		}
		for j := range remote.Roles[i].Replicas {
			r := &remote.Roles[i].Replicas[j]
			if int(r.Ordinal) == ordinal && r.Delivery != workercore.ModelDeploymentModelDeliveryRemote {
				return r.Delivery
			}
		}
	}

	return ""
}

// modelDeploymentModelTransferSweep carries the remote-to-fallback transition: a
// remote replica — a (role, ordinal) group, one member or several — whose load
// failed under Prefer is swept ONCE per pass, every member deleted so the
// replica recreates on its declared fallback, and the attempt counted in the
// deployment's status where the next render reads its override from. The swept
// pods are returned so the caller also deletes the Kueue Workload holding their
// finalizers. A remote Pod is never mutated in place — an immutable Pod cannot
// grow a CSI volume — a replica falls back only when every member of it failed
// its load, and Require never sweeps: its failures are the visible answer, not
// a fallback.
func modelDeploymentModelTransferSweep(
	ctx context.Context, cli ctrlcli.Client, md *workercore.ModelDeployment,
	pods []core.Pod, policy string, fallback workercore.ModelDeploymentModelDelivery,
) ([]core.Pod, error) {
	if policy != settings.ModelArtifactRemoteDeliveryPrefer || fallback == "" {
		return nil, nil
	}

	// Group the candidates per replica first, so a multi-member replica is
	// one decision and one attempt, not one per member.
	type replica struct {
		role    string
		ordinal int
		members []*core.Pod
		failed  bool
		serving bool
		unknown bool
	}
	groups := map[string]*replica{}
	var order []string
	var swept []core.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Annotations[modelDeploymentModelTransferDeliveryAnnotation] !=
			string(workercore.ModelDeploymentModelDeliveryRemote) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			if modelDeploymentModelTransferReplicaOverride(md, pod.Labels[modelDeploymentLabelKeyComponent],
				modelDeploymentModelTransferOrdinal(pod)) == fallback {
				swept = append(swept, *pod)
			}
			continue
		}
		role := pod.Labels[modelDeploymentLabelKeyComponent]
		ordinal := modelDeploymentModelTransferOrdinal(pod)
		key := role + "/" + strconv.Itoa(ordinal)
		g, ok := groups[key]
		if !ok {
			g = &replica{role: role, ordinal: ordinal}
			groups[key] = g
			order = append(order, key)
		}
		if modelDeploymentMainContainerReady(pod) {
			// Any member serving means the replica loaded: it is the last
			// replica a source loss is allowed to touch. The member is
			// remembered on its own, so the answer does not depend on the
			// order the members arrive in.
			g.serving = true
			continue
		}
		g.members = append(g.members, pod)
		if modelDeploymentModelTransferLoadFailed(pod) {
			g.failed = true
			continue
		}
		// A member whose load is under way, or whose load history the Pod
		// cannot say, holds the replica: readiness is no proof that it never
		// loaded, so the sweep may not delete a member that may have loaded.
		g.unknown = true
	}

	sort.Strings(order)
	for _, key := range order {
		g := groups[key]
		// A replica falls back only when every member of it failed its load.
		// A member that serves has loaded, and a member whose load history is
		// unknown may have loaded, so neither may be deleted by the sweep.
		if g.serving || g.unknown || !g.failed || len(g.members) == 0 {
			continue
		}
		if modelDeploymentModelTransferReplicaOverride(md, g.role, g.ordinal) != fallback {
			previous := md.Status.DeepCopy()
			attempts := modelDeploymentModelTransferBumpAttempt(md, g.role, g.ordinal, fallback)
			if attempts > modelDeploymentModelTransferAttemptsBound {
				modelDeploymentModelTransferMarkReason(md, g.role, g.ordinal,
					modelDeploymentModelTransferReasonAttemptsSpent)
			}
			// Persist the replacement decision before deleting any member. A retry
			// resumes this decision instead of charging the same failure again.
			if err := cli.Status().Update(ctx, md); err != nil {
				md.Status = *previous
				return swept, err
			}
		}
		for _, pod := range g.members {
			if err := cli.Delete(ctx, pod); ctrlcli.IgnoreNotFound(err) != nil {
				return swept, err
			}
			swept = append(swept, *pod)
		}
	}

	return swept, nil
}

// modelDeploymentModelTransferBumpAttempt moves one replica onto its declared
// fallback in the status, counting the attempt.
func modelDeploymentModelTransferBumpAttempt(
	md *workercore.ModelDeployment, role string, ordinal int,
	fallback workercore.ModelDeploymentModelDelivery,
) int32 {
	if md.Status.Model == nil {
		md.Status.Model = &workercore.ModelDeploymentModelStatus{}
	}
	model := md.Status.Model
	if model.Remote == nil {
		model.Remote = &workercore.ModelDeploymentModelRemoteStatus{
			Policy: workercore.ModelDeploymentRemotePolicyPrefer,
		}
	}
	entry := modelDeploymentModelTransferReplicaEntry(model.Remote, role, ordinal)
	entry.FallbackAttempts++
	entry.Delivery = fallback
	entry.Reason = modelDeploymentModelTransferReasonLoadFailed

	return entry.FallbackAttempts
}

// modelDeploymentModelTransferMarkReason restates one replica's reason without
// touching its count.
func modelDeploymentModelTransferMarkReason(md *workercore.ModelDeployment, role string, ordinal int, reason string) {
	if md.Status.Model == nil || md.Status.Model.Remote == nil {
		return
	}
	modelDeploymentModelTransferReplicaEntry(md.Status.Model.Remote, role, ordinal).Reason = reason
}

// modelDeploymentModelTransferReplicaEntry finds or creates one replica's entry,
// keeping the roles and the replicas within a role ordered for a stable
// status.
func modelDeploymentModelTransferReplicaEntry(
	remote *workercore.ModelDeploymentModelRemoteStatus, role string, ordinal int,
) *workercore.ModelDeploymentRemoteReplicaStatus {
	ri := sort.Search(len(remote.Roles), func(i int) bool {
		return remote.Roles[i].Role >= role
	})
	if ri >= len(remote.Roles) || remote.Roles[ri].Role != role {
		remote.Roles = append(remote.Roles, workercore.ModelDeploymentRemoteRoleStatus{})
		copy(remote.Roles[ri+1:], remote.Roles[ri:])
		remote.Roles[ri] = workercore.ModelDeploymentRemoteRoleStatus{Role: role}
	}
	roleEntry := &remote.Roles[ri]
	rj := sort.Search(len(roleEntry.Replicas), func(i int) bool {
		return roleEntry.Replicas[i].Ordinal >= int32(ordinal)
	})
	if rj < len(roleEntry.Replicas) && roleEntry.Replicas[rj].Ordinal == int32(ordinal) {
		return &roleEntry.Replicas[rj]
	}
	roleEntry.Replicas = append(roleEntry.Replicas, workercore.ModelDeploymentRemoteReplicaStatus{})
	copy(roleEntry.Replicas[rj+1:], roleEntry.Replicas[rj:])
	roleEntry.Replicas[rj] = workercore.ModelDeploymentRemoteReplicaStatus{Ordinal: int32(ordinal)}

	return &roleEntry.Replicas[rj]
}

// modelDeploymentModelTransferLoadFailed says whether a remote replica's load attempt
// is over: the main container exited nonzero, or the Pod itself failed. A
// Pod still pulling or starting is a load under way, and a ready one
// already loaded.
func modelDeploymentModelTransferLoadFailed(pod *core.Pod) bool {
	if pod.Status.Phase == core.PodFailed {
		return true
	}
	for i := range pod.Status.ContainerStatuses {
		s := &pod.Status.ContainerStatuses[i]
		if s.Name != modelDeploymentMainContainerName || s.State.Terminated == nil {
			continue
		}
		if s.State.Terminated.ExitCode != 0 {
			return true
		}
	}

	return false
}

// modelDeploymentModelTransferOrdinal reads the replica ordinal a Pod was stamped
// with; zero is a real position and the unstampable default.
func modelDeploymentModelTransferOrdinal(pod *core.Pod) int {
	v, err := strconv.Atoi(pod.Labels[modelDeploymentReplicaOrdinalLabel])
	if err != nil {
		return 0
	}

	return v
}

// modelDeploymentModelTransferObserveStatus rebuilds status.model.remote from the
// pass's Pods, and it is the view the aggregate delivery string refuses to
// summarize: the delivery each replica renders under, how far its load got,
// and why a replica is not remote anymore.
//
// The load observations keep the prerequisites apart from the progress: a
// Pending replica with no prepared source is AwaitingSource — the thing that
// blocks its startup — and one with a source is AwaitingMetadata, whose view
// has not mounted yet; a Running replica is Loading; a Ready one is Loaded.
// A Loaded entry is not re-derived downward on a later pass: a replica whose
// container is ready keeps Loaded even when its source has drained, which is
// exactly the retention the contract asks for. A fallback entry with no live
// Pod is carried forward from the previous status: the replica it describes
// is being recreated on the fallback this pass rendered for it.
func modelDeploymentModelTransferObserveStatus(
	holder *workercore.ModelDeployment, pods []core.Pod, weights *modelArtifactWeights,
	carried *workercore.ModelDeploymentModelRemoteStatus, sourceReady bool,
) {
	if holder.Status.Model == nil || weights == nil || weights.Render == nil ||
		weights.Render.Delivery != workercore.ModelDeploymentModelDeliveryRemote {
		return
	}

	model := holder.Status.Model
	model.Remote = &workercore.ModelDeploymentModelRemoteStatus{
		Policy: workercore.ModelDeploymentRemotePolicy(weights.Render.ModelTransferPolicy),
	}

	seen := map[string]map[int32]bool{}
	for _, view := range modelDeploymentGroupPodsByReplica(pods) {
		observed := modelDeploymentModelTransferReplicaLoad(view, sourceReady)
		if observed.Delivery == "" {
			continue
		}
		role, ordinal := view.Role, int32(view.Ordinal)
		if seen[role] == nil {
			seen[role] = map[int32]bool{}
		}
		seen[role][ordinal] = true
		entry := modelDeploymentModelTransferReplicaEntry(model.Remote, role, int(ordinal))
		entry.Delivery, entry.Load, entry.Reason = observed.Delivery, observed.Load, observed.Reason
		// The attempts a replica spent are its history, and the history
		// outlives the pod that spent them: a replacement fallback pod is a
		// new observation of the same replica.
		var prev *workercore.ModelDeploymentRemoteReplicaStatus
		if carried != nil {
			for i := range carried.Roles {
				if carried.Roles[i].Role != role {
					continue
				}
				for j := range carried.Roles[i].Replicas {
					if carried.Roles[i].Replicas[j].Ordinal == ordinal {
						prev = &carried.Roles[i].Replicas[j]
					}
				}
			}
		}
		if prev != nil {
			entry.FallbackAttempts = prev.FallbackAttempts
		}

		if entry.Delivery != workercore.ModelDeploymentModelDeliveryRemote && entry.Reason == "" &&
			prev != nil {
			entry.Reason = prev.Reason
		}
	}

	// A fallback entry whose replica is between its delete and its recreate
	// has no live Pod to observe; dropping it would render the next pass
	// remote again and loop the transition the sweep just made. The carry is
	// bounded by what the deployment still declares: an ordinal past a
	// role's replica count, or a role that was removed or renamed, has no
	// recreation ahead of it, so its stale entry is refused rather than
	// carried.
	declared := modelDeploymentDeclaredRoleReplicas(holder)
	if carried == nil {
		return
	}
	for i := range carried.Roles {
		carriedRole := &carried.Roles[i]
		limit, declaredRole := declared[carriedRole.Role]
		if !declaredRole {
			continue
		}
		for j := range carriedRole.Replicas {
			r := &carriedRole.Replicas[j]
			if r.Delivery == workercore.ModelDeploymentModelDeliveryRemote ||
				r.Ordinal >= limit || seen[carriedRole.Role][r.Ordinal] {
				continue
			}
			entry := modelDeploymentModelTransferReplicaEntry(model.Remote, carriedRole.Role, int(r.Ordinal))
			if entry.Delivery == workercore.ModelDeploymentModelDeliveryRemote {
				continue
			}
			entry.Delivery = r.Delivery
			entry.Load = r.Load
			entry.Reason = r.Reason
			entry.FallbackAttempts = r.FallbackAttempts
		}
	}
}

// modelDeploymentDeclaredRoleReplicas maps each role to the replica count the deployment declares,
// as the pass's own role statuses recorded it: they are derived from the spec ahead of this
// observation, which reads a status holder whose spec is not the deployment's. A role the pass did
// not record declares nothing, so nothing of it is carried.
func modelDeploymentDeclaredRoleReplicas(holder *workercore.ModelDeployment) map[string]int32 {
	declared := make(map[string]int32, len(holder.Status.Roles))
	for i := range holder.Status.Roles {
		declared[holder.Status.Roles[i].Name] = holder.Status.Roles[i].Desired
	}

	return declared
}

// modelDeploymentModelTransferReplicaLoad combines every member's load observation.
// A failed or incomplete member prevents Loaded, independent of Pod list order.
func modelDeploymentModelTransferReplicaLoad(
	view modelDeploymentReplicaView, sourceReady bool,
) workercore.ModelDeploymentRemoteReplicaStatus {
	entry := workercore.ModelDeploymentRemoteReplicaStatus{Load: workercore.ModelDeploymentRemoteLoadStateLoaded}
	priority := 0
	mixedDelivery := false
	if modelDeploymentDeployedReplicaShape(view).modelDeploymentReplicaLegVerdict() != modelDeploymentLegVerified {
		entry.Load = workercore.ModelDeploymentRemoteLoadStateAwaitingMetadata
		priority = 2
	}
	for _, pod := range view.Members {
		delivery := workercore.ModelDeploymentModelDelivery(pod.Annotations[modelDeploymentModelTransferDeliveryAnnotation])
		if delivery != "" {
			mixedDelivery = mixedDelivery || (entry.Delivery != "" && entry.Delivery != delivery)
			entry.Delivery = delivery
		}
		load, reason, rank := workercore.ModelDeploymentRemoteLoadStateLoaded, "", 0
		switch {
		case modelDeploymentModelTransferLoadFailed(pod):
			load, reason, rank = workercore.ModelDeploymentRemoteLoadStateUnknown, modelDeploymentModelTransferLoadFailReason(pod), 4
		case delivery == "":
			load, reason, rank = workercore.ModelDeploymentRemoteLoadStateUnknown, "a replica member has no delivery observation", 4
		case modelDeploymentMainContainerReady(pod):
		case pod.Status.Phase == core.PodRunning:
			load, rank = workercore.ModelDeploymentRemoteLoadStateLoading, 1
		case delivery == workercore.ModelDeploymentModelDeliveryRemote && !sourceReady:
			load, reason, rank = workercore.ModelDeploymentRemoteLoadStateAwaitingSource,
				"no prepared source serves this artifact's digest yet; the replica holds before its load starts", 3
		default:
			load, rank = workercore.ModelDeploymentRemoteLoadStateAwaitingMetadata, 2
		}
		if rank > priority || (rank == priority && reason < entry.Reason) {
			entry.Load, entry.Reason, priority = load, reason, rank
		}
	}
	if mixedDelivery {
		entry.Delivery = workercore.ModelDeploymentModelDeliveryRemote
		entry.Load, entry.Reason = workercore.ModelDeploymentRemoteLoadStateUnknown, "replica members disagree on weight delivery"
	}
	return entry
}

// modelDeploymentModelTransferLoadFailReason names why a load attempt ended.
func modelDeploymentModelTransferLoadFailReason(pod *core.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		s := &pod.Status.ContainerStatuses[i]
		if s.Name == modelDeploymentMainContainerName && s.State.Terminated != nil {
			return fmt.Sprintf("the engine container exited with code %d: %s",
				s.State.Terminated.ExitCode, s.State.Terminated.Reason)
		}
	}

	return "the replica failed before its load completed"
}

// modelDeploymentModelTransferWeightsVoice reports confirmed loading for every
// desired replica. Loaded engines remain ready after their source drains.
func modelDeploymentModelTransferWeightsVoice(
	holder *workercore.ModelDeployment, pods []core.Pod, weights *modelArtifactWeights,
) {
	loaded := map[string]int32{}
	for _, view := range modelDeploymentGroupPodsByReplica(pods) {
		role := modelDeploymentRoleByName(holder, view.Role)
		if role == nil || !view.Seated || view.Ordinal < 0 || int32(view.Ordinal) >= role.Replicas {
			continue
		}
		observed := modelDeploymentModelTransferReplicaLoad(view, weights.Render.SourceReady)
		if observed.Delivery != "" && observed.Load == workercore.ModelDeploymentRemoteLoadStateLoaded {
			loaded[view.Role]++
		}
	}
	var waiting int32
	for _, role := range holder.Spec.Roles {
		waiting += max(0, role.Replicas-loaded[role.Name])
	}
	switch {
	case waiting == 0:
		ModelDeploymentConditionWeightsReady.True(holder, modelWeightsReasonLoaded,
			"every desired replica loaded its weights")
	case !weights.Render.SourceReady:
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonAwaitingSource, fmt.Sprintf(
			"%d replicas have not completed loading %s and no prepared source is available", waiting, weights.Render.ManifestDigest,
		))
	default:
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonLoading, fmt.Sprintf(
			"%d replicas have not completed loading %s", waiting, weights.Render.ManifestDigest,
		))
	}
}
