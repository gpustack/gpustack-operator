package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	// ModelDeploymentResourceType is the resource note every object a ModelDeployment renders
	// carries, so a watch can tell them from every other Pod in the namespace.
	ModelDeploymentResourceType = "modeldeployments"
	// ModelDeploymentResourceNoteRole names the role a rendered object belongs to.
	ModelDeploymentResourceNoteRole = "role"

	// modelDeploymentLabelKeyName, modelDeploymentLabelKeyInstance and
	// modelDeploymentLabelKeyComponent are the identity labels a replica's Pod carries.
	//
	// They exist alongside the resource note because a Service selects on LABELS and cannot read an
	// annotation, and because they carry only identity — which deployment, which role — and never
	// anything a spec update can move. A selector that moved would orphan the Pods it used to
	// front. The same three keys, with the same meanings, front the KV cache backend's leader.
	modelDeploymentLabelKeyName      = "app.kubernetes.io/name"
	modelDeploymentLabelKeyInstance  = "app.kubernetes.io/instance"
	modelDeploymentLabelKeyComponent = "app.kubernetes.io/component"
	modelDeploymentLabelValueName    = "model-deployment"

	// modelDeploymentLabelKeyRoleKind carries the role's EFFECTIVE kind, so that something in front
	// of the replicas can tell a prefiller from a decoder by selector. It is the resolved value, not
	// the field: a role that names no kind is a server, and a selector matching the empty string
	// would miss every replica of the default shape.
	//
	// It is rendered for every deployment, including one whose only role is a server. A key that
	// appeared only under disaggregation would make "no prefiller exists" and "this deployment does
	// not label roles" the same query result.
	//
	// It is NOT in the selector labels. A Service's selector cannot change without orphaning the
	// replicas it used to front, and while a role's kind is immutable today, the reason this key
	// stays out is the same one that keeps the entrance label out: the selector carries identity,
	// and the kind is a property of the role rather than a name for it.
	//
	// Adding it rolls every existing deployment once. The Pod spec hash covers the labels -- see
	// modelDeploymentPodSpecHash -- so every replica rendered before this key existed has a stale
	// fingerprint, and the recreate rollout replaces it. The rebuild is the one the recreate policy
	// already describes, and a replica that leaves loses its cached blocks to its siblings.
	modelDeploymentLabelKeyRoleKind = "modeldeployment." + systemname.LabelPrefix + "role-kind"

	// modelDeploymentPodSpecHashAnnotation carries the fingerprint of the Pod a role's spec renders
	// to. The rollout is recreate rather than surge, so this is the whole of how a replica built
	// before a spec change is told from one built after it.
	modelDeploymentPodSpecHashAnnotation = "modeldeployment." + systemname.LabelPrefix + "pod-spec-hash"

	// modelDeploymentDefaultPort is the port a replica serves on when the role names
	// none, and it is RENDERED INTO THE ENGINE'S OWN LISTEN ARGUMENT rather than left to the engine
	// to pick, because the supported engines do not agree on a default. vLLM opens 8000 on every
	// interface; SGLang opens 30000 on the loopback address alone, which neither a Service endpoint
	// nor a kubelet probe can reach.
	modelDeploymentDefaultPort int32 = 8000
	// modelDeploymentDefaultPortName names that port on the container and on the Service fronting it.
	modelDeploymentDefaultPortName = "http"
	// A managed decoder's routing proxy owns the serving port, while vLLM listens behind it here.
	modelDeploymentInternalPort int32 = 8200
	// modelDeploymentMainContainerName is the container the role's own image and command run in,
	// and the only one a rank belongs to: every other container a Pod here carries is this
	// operator's, and joins no collective.
	modelDeploymentMainContainerName = "main"

	// modelDeploymentLeaderAddressEnv, modelDeploymentReplicaSizeEnv and
	// modelDeploymentMemberIndexEnv publish the rank layout of an instance that spans several Pods.
	// They appear only above size one, so a single-Pod instance renders exactly as it did before
	// the field existed.
	//
	// THEY ARE OWNED FOR EVERY ENGINE, unlike the connector's keys, because they describe the
	// Kubernetes shape the Pod was rendered in rather than anything one engine understands. The
	// ownership is not a preference: a user entry of the same name would be merged by VALUE onto an
	// entry that carries a fieldRef instead, and an EnvVar holding both is rejected by the API
	// server -- so an unowned key here turns a legal deployment into one that cannot render.
	modelDeploymentLeaderAddressEnv = "GPUSTACK_REPLICA_LEADER_ADDRESS"
	modelDeploymentReplicaSizeEnv   = "GPUSTACK_REPLICA_SIZE"
	modelDeploymentMemberIndexEnv   = "GPUSTACK_MEMBER_INDEX"

	// modelDeploymentEngineHostArg and modelDeploymentEnginePortArg are the two flags that decide
	// where an engine listens. They are FILLED RATHER THAN OWNED: a role that passes either one
	// keeps its own value, which is the rule the non-Kubernetes worker applies to the same two flags.
	modelDeploymentEngineHostArg = "--host"
	modelDeploymentEnginePortArg = "--port"
	// modelDeploymentEngineBindHost is every interface in the Pod's network namespace, which is what
	// a Service endpoint and a kubelet probe both dial. An engine left on its own default may open
	// the loopback address instead, where only a process inside that container can reach it.
	modelDeploymentEngineBindHost = "0.0.0.0"
	// modelDeploymentEngineTLSCertArg and modelDeploymentEngineTLSKeyArg are the ONLY two flags that
	// turn the engine's listener into a TLS one, and the whole --ssl-* family is deliberately not
	// used in their place.
	//
	// Both supported engines pass every ssl argument straight to uvicorn, which enables TLS on
	// `ssl_keyfile or ssl_certfile` and on nothing else. --ssl-ca-certs, --ssl-ciphers,
	// --ssl-keyfile-password and --ssl-cert-reqs passed ALONE all leave an ordinary HTTP server, so
	// treating the prefix as the family would publish an https:// address for a listener serving
	// plaintext.
	//
	// WHERE TO RE-CHECK THIS, since it is engine behavior and nothing here can enforce it: uvicorn's
	// Config.is_ssl, reached from vllm's api_server.py serve_http call and from sglang's
	// http_server.py launch_server. Each engine ALSO logs a line of its own about SSL being enabled,
	// computed differently -- vllm from keyfile AND certfile -- and neither line decides the
	// listener. Read the uvicorn property, not the log statement.
	//
	// THE SCOPE IS COMMAND-LINE FLAGS. An engine that grew an environment variable putting the same
	// listener on TLS would not be seen here, and both the probe and the published address would
	// then say http:// for a TLS listener. No such variable exists in either engine today.
	modelDeploymentEngineTLSCertArg = "--ssl-certfile"
	modelDeploymentEngineTLSKeyArg  = "--ssl-keyfile"
	// modelDeploymentEngineTLSClientAuthArg is the one TLS-related flag a gate cannot always follow:
	// it can demand a CLIENT certificate, and a kubelet probe has none to present. It withdraws the
	// gate only where one of the two above turned TLS on -- uvicorn builds no TLS context for it to
	// apply to otherwise -- and only at the VALUE below.
	modelDeploymentEngineTLSClientAuthArg = "--ssl-cert-reqs"
	// modelDeploymentEngineTLSClientAuthNone and modelDeploymentEngineTLSClientAuthOptional are
	// ssl.CERT_NONE and ssl.CERT_OPTIONAL, the only two values of that flag under which a kubelet
	// probe still completes: the first asks for no certificate, the second verifies one only if the
	// client offers it.
	//
	// THEY ARE AN ALLOW LIST, not a "reject on CERT_REQUIRED" test, and the difference shows on a
	// value outside the enum. ssl defines exactly these plus CERT_REQUIRED, so a 3 or a -1 is a
	// configuration this operator cannot reason about; a test naming only the rejecting value would
	// read it as safe and gate a listener whose behavior is unknown. Everything not on this list
	// withdraws the gates, which matches how an unreadable value is treated below and for the same
	// reason.
	modelDeploymentEngineTLSClientAuthNone     = 0
	modelDeploymentEngineTLSClientAuthOptional = 1

	// modelDeploymentProbePath is the route all three gates read, and it is the ONLY route both supported
	// engines answer usefully -- which they do for OPPOSITE reasons, so neither engine's behavior
	// may be used to reason about the other.
	//
	// On vllm the handler is a LIVENESS check: it answers 200 in every state and 503 only once the
	// engine is dead, and it answers 200 outright when the server carries no engine at all. What
	// makes it a readiness signal there is one layer below the handler: the server socket is bound
	// before the engine is built but never listened on until uvicorn serves, which happens after the
	// engine finishes loading. Until then the probe does not get a response to grade -- the
	// connection is refused.
	//
	// On sglang the handler IS the readiness check: the server listens from the start and answers
	// 503 while its status is still starting.
	//
	// A TCP probe would therefore be correct on vllm and vacuous on sglang, whose port accepts
	// immediately. So would /v1/models. Nothing else is shared: vllm's /ready and /readyz exist only
	// in its data-parallel supervisor, not in the server a role runs.
	modelDeploymentProbePath = "/health"

	// modelDeploymentDirectDecodeProbePath is the route the gates read in the direct-decode shape,
	// where a routing sidecar owns the service port the gates grade. The sidecar answers /health
	// itself, in every state, so that path grades the sidecar and stops saying anything about the
	// engine the moment the Pod starts. Every other route the sidecar forwards to the engine, whose
	// port refuses connections until the engine finishes loading and serves -- a refusal the
	// sidecar answers with a 503. Reading /v1/models through the sidecar therefore keeps the gates
	// on the address the Service fronts while the answer once again turns on the engine.
	//
	// The vacuity recorded above does not follow this route into the branch: the direct-decode
	// shape renders only for vLLM, the engine whose /v1/models is a readiness signal.
	modelDeploymentDirectDecodeProbePath = "/v1/models"

	// modelDeploymentProbePeriodSeconds paces all three gates, which is what makes their failure
	// thresholds comparable to each other.
	modelDeploymentProbePeriodSeconds int32 = 10
	// modelDeploymentProbeTimeoutSeconds bounds one probe request. It is above the kubelet's
	// one-second default because the route is served by the same process that is loading the model.
	modelDeploymentProbeTimeoutSeconds int32 = 3
	// modelDeploymentStartupFailureThreshold is how many periods a replica may take to load before
	// the kubelet gives up on it, which at the period above is thirty minutes.
	//
	// The budget is generous on purpose: load time grows with model size and with how the weights
	// arrive, and a threshold that fits the smallest model turns a slow start into a restart loop.
	// Overshooting costs only a replica that is already broken staying broken for longer, which the
	// deployment's own status already reports.
	modelDeploymentStartupFailureThreshold int32 = 180
	// modelDeploymentReadinessFailureThreshold is how many consecutive failures take a replica that
	// HAS served out of the Service's endpoints.
	modelDeploymentReadinessFailureThreshold int32 = 3
	// modelDeploymentLivenessFailureThreshold is how many consecutive failures restart a replica
	// whose engine answered once and then stopped answering.
	//
	// It is deliberately wider than the readiness threshold, because the two failures cost different
	// things. Losing readiness withdraws a replica from the Service and is recovered by answering
	// again, so a brief stall should reach it first. A restart throws away a model that took the
	// startup budget above to load, so only a failure that outlives the readiness one should reach
	// it.
	modelDeploymentLivenessFailureThreshold int32 = 6
)

// ModelDeploymentRenderInput is everything one replica's Pod is rendered from.
//
// The InstanceType and the connector arrive as values rather than being read here, because the
// render must stay a pure function of its inputs: the reconciler converges the same object on every
// pass, so a render that reached for a client could return two different Pods for one spec and roll
// the deployment forever.
type ModelDeploymentRenderInput struct {
	// Deployment is the object being rendered.
	Deployment *workercore.ModelDeployment
	// Role is the entry of Deployment.Spec.Roles this replica belongs to.
	Role *workercore.ModelDeploymentRole
	// InstanceType is the type the role's Pods are admitted against. It supplies how to spell the
	// accelerator keys and the per-card unit resources the host request is derived from.
	InstanceType *worker.InstanceType
	// Connector is what the engine needs to reach the pool. Its zero value renders a replica with
	// no connector at all, which is what a deployment whose Binding has not been resolved yet gets.
	Connector ModelDeploymentConnectorRender
	// There is NO ConfigMap name here, and there is no object to name: the client configuration
	// travels in Connector.PodAnnotations and reaches the container as a downwardAPI projection of
	// it. The field this struct used to carry was never filled by anything, so the mount it guarded
	// was dead code; Connector.PodAnnotations is the carrier.
	// RuntimeClassName is the runtime class an accelerated replica needs. The reconciler resolves it,
	// because deciding it requires reading whether the class exists on the cluster.
	RuntimeClassName string
	// GeneralResourcesOvercommit mirrors the Instance path's overcommit setting, which decides
	// whether the derived CPU and memory are requested at full size or scaled down.
	GeneralResourcesOvercommit bool
	// NativeSidecar decides which shape a direct decoder's routing proxy is rendered in: a native
	// sidecar -- an init container carrying restartPolicy: Always -- when the cluster keeps that
	// field, or a classic regular container when it does not. The reconciler resolves it from the
	// cluster's version rather than the render guessing, because the field is DROPPED without an
	// error below 1.29, and a Pod rendered with it there never leaves Init.
	NativeSidecar bool
	// Ordinal is the replica this render is for, stamped into the group metadata. The template
	// half never reads it -- only the stamp does -- and the zero value renders the first replica,
	// which is what a caller with no particular replica in mind gets.
	Ordinal int

	// Member is which Pod of that replica this render is for, counting from zero. It is stamped,
	// never templated, for the same reason Ordinal is: the template is what every Pod of the role
	// shares, and a value naming one Pod inside it would make two members of one replica hash
	// differently and read as a permanent rollout.
	//
	// The zero value is the leader, which is what a caller with no particular member in mind gets
	// and is also the only member a replica of size one has.
	Member int
}

// modelDeploymentSelectorLabels is what fronts a role's replicas: the identity of the deployment and
// of the role, and nothing a spec update can move.
func modelDeploymentSelectorLabels(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) map[string]string {
	return map[string]string{
		modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance:  md.Name,
		modelDeploymentLabelKeyComponent: role.Name,
	}
}

// renderModelDeploymentPod renders one replica.
//
// It composes the two halves a replica's Pod is built in: the template, which produces everything
// the role's spec states, and the stamp, which puts the Kueue group metadata and the spec-hash
// fingerprint on afterwards. The boundary is the load-bearing part -- the template is what every
// replica of a role shares, so nothing that names one member may be produced inside it.
//
// It returns an error rather than a best-effort Pod whenever the request cannot be sized — an
// InstanceType whose accelerator detail has not been computed yet, a role with no image. Falling
// back to a whole-card or an empty request would produce a Pod that runs and charges the wrong
// quota, which is the failure this whole path exists to avoid.
func renderModelDeploymentPod(ctx context.Context, in ModelDeploymentRenderInput) (*core.Pod, error) {
	pod, err := renderModelDeploymentPodTemplate(ctx, in)
	if err != nil {
		return nil, err
	}

	stampModelDeploymentPod(pod, in.Deployment, in.Role, in.Ordinal, in.Member)

	return pod, nil
}

// renderModelDeploymentPodTemplate renders the whole of a replica's Pod that its role's spec
// states: the labels, the annotations, the PodSpec and every container in it.
//
// IT PRODUCES NOTHING THAT VARIES FROM ONE REPLICA OF A ROLE TO THE NEXT, and that is the property
// the split into a template and a stamp exists to keep checkable: the Kueue group metadata and the
// spec-hash fingerprint are stamped on afterwards, because a per-replica group name differs between
// the members of one role, and a value naming one member leaking into this function would render
// each replica a different Pod with nothing erroring anywhere. The connector's PodAnnotations are
// written here rather than in the stamp because they carry the pool's endpoint and domain, which
// every replica of the role shares.
func renderModelDeploymentPodTemplate(ctx context.Context, in ModelDeploymentRenderInput) (*core.Pod, error) {
	md, role := in.Deployment, in.Role

	// A STATED IMAGE ALWAYS WINS, and synthesis is the fallback rather than the rule: it is how a
	// role runs a private build, a vendor with no published runner backend, or an Ascend family the
	// matrix does not carry. The formula reads no release matrix, so it cannot know the tag it
	// assembles was ever published - that is the accepted trade, and its failure is an
	// ImagePullBackOff rather than a silent misconfiguration.
	image := role.Image
	if image == "" {
		synthesized, err := SynthesizeModelDeploymentImage(
			md.Spec.Engine.Name, md.Spec.Engine.Version, in.InstanceType.Status.Detail)
		if err != nil {
			return nil, fmt.Errorf("role %q names no image and none could be synthesized: %w", role.Name, err)
		}
		image = synthesized
	}

	ress, err := deriveModelDeploymentResources(role, in.InstanceType)
	if err != nil {
		return nil, err
	}

	// THE ENTRANCE IS READ, NOT RECOMPUTED, and the difference is the whole point of this line.
	// role.InstanceType names an InstanceType; the label Kueue admits on names the LocalQueue that
	// fronts that type's ClusterQueue, and the type's own status is where that name is published --
	// by the same reconcile that creates the LocalQueue.
	//
	// Deriving it here from the name instead would be a second answer to one question, written by a
	// second function: equal today, because the ClusterQueue carries the InstanceType's name and
	// both sides spell it with FormatLocalQueueName, and silently divergent the day either half of
	// that stops holding. The half that drifted would be this one, whose symptom is a Pod routed at
	// a LocalQueue that does not exist -- admitted by nothing, with nothing naming the field.
	entrance := in.InstanceType.Status.Entrance
	if entrance == "" {
		// An InstanceType whose status has not been computed yet, exactly like an accelerator detail
		// that is not there: refuse to render rather than fall back. A Pod carrying no queue-name
		// label at all is not queued by Kueue, it is admitted by the kubelet directly -- charging no
		// quota and bypassing every gate the chain exists to apply.
		return nil, fmt.Errorf(
			"instance type %q publishes no queue entrance yet", in.InstanceType.Name)
	}

	// A role that replaces the command owns the whole argv, so the operator contributes neither
	// engine arguments nor the client environment that only its own arguments would have used.
	takeOver := len(role.Command) > 0

	command := role.Command
	// declaredPorts is the role's declared port list, computed once: the connector's synthesized
	// ports are deduplicated against it below, and a direct decoder's engine port must clear every
	// entry in it, not only the one the Service fronts.
	declaredPorts := modelDeploymentContainerPorts(role)
	// gradable is false for a take-over role, whose argv the operator did not build.
	var (
		scheme     core.URIScheme
		gradable   bool
		enginePort = modelDeploymentServicePort(role).ContainerPort
	)
	// READ OFF THE PROXY'S OWN FLAG, not off the transfer leg. A transfer leg renders under every
	// admitted router-and-engine pair but one -- an Ascend pair under "vllm-router" has none --
	// and only one router fronts its decoder with a proxy; deriving this from the leg would put
	// that router's proxy on the other two, where it would wait on a header nothing writes. The
	// kind is still checked here because the flag is set per role and this is where the port it
	// takes is moved.
	directDecode := !takeOver && in.Connector.RoutingSidecar &&
		ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindDecode
	if directDecode {
		enginePort = modelDeploymentInternalPort
		for modelDeploymentPortTaken(declaredPorts, enginePort) {
			enginePort++
		}
	}
	if !takeOver {
		command, err = ModelDeploymentEngineCommand(md.Spec.Engine.Name, md.Spec.Model.Name)
		if err != nil {
			return nil, err
		}
		command = append(command, in.Connector.Args...)
		command = append(command, role.ExtraArgs...)

		// READ BEFORE FILLING, and read the SAME list the filling reads. What decides whether the
		// endpoint is the operator's own is which flags were there beforehand, and scanning a
		// narrower list here than appendModelDeploymentBindArgs scans is how the two would come to
		// disagree: a connector argument carrying one of these flags would be honored by the fill
		// and invisible to the gate.
		scheme, gradable = modelDeploymentEngineTransport(command)
		command = appendModelDeploymentBindArgs(command, enginePort)
		if directDecode {
			enginePort, err = modelDeploymentCommandPort(command)
			if err != nil {
				return nil, fmt.Errorf("role %q cannot place its routing proxy: %w", role.Name, err)
			}
			for _, declared := range declaredPorts {
				if enginePort == declared.ContainerPort {
					return nil, fmt.Errorf(
						"role %q places its model server on port %d, which the role already "+
							"declares as %q", role.Name, enginePort, declared.Name)
				}
			}
			gradable = true
		}
	}

	// The connector's volume and mount arrive already built, and they are taken as a unit with the
	// annotations applied below: the volume is a downwardAPI projection of one of them, so applying
	// it without the annotation mounts an empty file. Both are empty for an engine configured
	// through the environment, which is why neither is conditional on the engine here.
	//
	// A take-over role gets NEITHER, along with no synthesized argument and no client environment.
	// The operator did not build that command line and cannot claim the container uses the cache.
	vols, mounts := convertModelDeploymentAdditionalVolumes(role.AdditionalVolumes)
	if !takeOver {
		vols = append(vols, in.Connector.Volumes...)
		mounts = append(mounts, in.Connector.VolumeMounts...)
	}

	probePath := modelDeploymentProbePath
	probeScheme := scheme
	if directDecode {
		probeScheme = core.URISchemeHTTP
		probePath = modelDeploymentDirectDecodeProbePath
	}
	startupProbe, readinessProbe, livenessProbe := modelDeploymentProbes(role, probeScheme, probePath, gradable)
	ports, err := appendModelDeploymentConnectorPorts(declaredPorts, in.Connector, takeOver)
	if err != nil {
		return nil, err
	}
	if directDecode {
		ports[0].Name = "model-server"
		ports[0].ContainerPort = enginePort
	}

	mainC := core.Container{
		Name:            modelDeploymentMainContainerName,
		Image:           image,
		ImagePullPolicy: role.ImagePullPolicy,
		Command:         command,
		Resources: getResourceRequirements(
			ress, in.InstanceType, true, in.GeneralResourcesOvercommit, true, false),
		Ports:          ports,
		Env:            mergeModelDeploymentEnv(md.Spec.Engine.Name, role, in.Connector, takeOver),
		VolumeMounts:   mounts,
		StartupProbe:   startupProbe,
		ReadinessProbe: readinessProbe,
		LivenessProbe:  livenessProbe,
	}
	if role.Privileged {
		mainC.SecurityContext = &core.SecurityContext{Privileged: ptr.To(true)}
	}

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			// The name is left empty and only a prefix is rendered: the spec declares a COUNT of
			// interchangeable replicas and carries no identity for any one of them, so the suffix
			// that tells one instance from another is assigned by the API server at creation. A
			// replacement is a new instance and carries a new suffix, never the name of the
			// replica it replaces.
			GenerateName: md.Name + "-" + role.Name + "-",
			Namespace:    md.Namespace,
			Labels:       modelDeploymentPodLabels(md, role, entrance),
		},
		Spec: core.PodSpec{
			// A replica is not a shell box: nothing nsenters into it, so it needs neither the host
			// IPC namespace nor a shared process namespace, and it never reads the API.
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			// Recreate is the rollout policy, and a replica that exits is replaced by a newly
			// created Pod rather than restarted in place with a stale spec.
			RestartPolicy:    core.RestartPolicyAlways,
			ImagePullSecrets: role.ImagePullSecrets,
			Volumes:          vols,
			Containers:       []core.Container{mainC},
		},
	}
	if directDecode {
		sidecar := renderModelDeploymentRoutingSidecar(
			ctx, role, enginePort, scheme, md.Spec.Engine.Name, in.NativeSidecar,
			in.InstanceType.Status.Detail.Manufacturer)
		if in.NativeSidecar {
			pod.Spec.InitContainers = []core.Container{sidecar}
		} else {
			// The classic shape runs the proxy as the FIRST regular container: kubelet starts
			// regular containers in list order, which is a de-facto ordering rather than the API
			// guarantee a native sidecar carries. What actually gates traffic on either shape is
			// that the engine container's probes target the proxy's port, so the Pod stays
			// unready until the proxy listens.
			//
			// What this shape LOSES is the shutdown ordering: a native sidecar is stopped only
			// after every regular container exits, while a classic one is terminated alongside
			// them, so a Pod deletion can cut the proxy before the engine finishes draining --
			// in-flight handoffs reset instead of answering, and the caller's retry lands on
			// another replica. The startup guarantee it loses buys nothing here: the engine
			// never dials the proxy, and a request that arrives before the engine listens gets a
			// 503 the proxy survives, recovering the moment the engine is up.
			pod.Spec.Containers = append([]core.Container{sidecar}, pod.Spec.Containers...)
		}
	}
	if in.RuntimeClassName != "" {
		pod.Spec.RuntimeClassName = ptr.To(in.RuntimeClassName)
	}

	// No nodeSelector entry is rendered for the accelerator model. A role takes whatever flavor its
	// pool assigns, which is what every role did before per-role model selection existed and what
	// they all do again now that it is gone: Kueue evaluates a candidate flavor per PodSet, and with
	// no selector to match against there is nothing to narrow the choice within one pool.
	systemmeta.NoteResource(pod, ModelDeploymentResourceType, map[string]string{
		ModelDeploymentResourceNoteRole: role.Name,
	})
	kubemeta.ControlOnWithoutBlock(pod, md, workercore.SchemeGroupVersionKind("ModelDeployment"))

	// THE CONNECTOR'S ANNOTATIONS ARE TEMPLATE OUTPUT, AND THE FINGERPRINT THE STAMP WRITES OVER
	// THIS POD IS THE WHOLE REASON THE CARRIER WORKS. The client configuration lives in one of
	// these annotations rather than in a ConfigMap, and the fingerprint covers {Labels, Annotations,
	// PodSpec} -- so changing the pool endpoint or the domain moves the hash and the replicas are
	// recreated to pick it up. Stamping them after that fingerprint instead would leave the hash
	// blind to the configuration and every replica holding a stale one, with nothing failing.
	//
	// Gated on the same take-over check as the volume that projects them: a role that replaced the
	// command line gets no part of the connector, and half of it would be worse than none.
	if !takeOver && len(in.Connector.PodAnnotations) > 0 {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string, len(in.Connector.PodAnnotations)+1)
		}
		for k, v := range in.Connector.PodAnnotations {
			pod.Annotations[k] = v
		}
	}

	return pod, nil
}

// stampModelDeploymentPod puts the Kueue group metadata on a rendered replica and then writes the
// spec-hash fingerprint, in that order. The fingerprint covers the labels and annotations it finds
// on the Pod, so writing it before the group metadata would leave it blind to a change that moves
// only the group -- a replica whose role was renamed would keep its old fingerprint and never be
// seen as outdated.
func stampModelDeploymentPod(
	pod *core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	ordinal, member int,
) {
	// THE NAME IS DERIVED RATHER THAN GENERATED, above size one, and that is what the whole
	// multi-Member shape rests on: a member cannot join a collective it cannot address, and a Pod
	// created by GenerateName has no name any sibling can predict. Every reader -- this renderer,
	// the converger, a sibling's entrypoint -- derives the same string from the same four values
	// without reading anything.
	//
	// AT SIZE ONE THE NAME STAYS GENERATED, deliberately. A replica's identity is obtained from its
	// ordinal label rather than from its name, so naming single-Member replicas would buy nothing
	// and would cost the one thing GenerateName gives: a create that cannot collide with a Pod of
	// the same ordinal still draining, which is exactly the state the converger's create gate waits
	// out. Above one the collision is unavoidable -- the address IS the name -- and the gate is
	// what handles it instead.
	if modelDeploymentRoleSize(role) > 1 {
		pod.Name = modelDeploymentMemberName(md, role.Name, ordinal, member)
		pod.GenerateName = ""

		// hostname and subdomain are what turn that name into a DNS record: the headless Service
		// named here publishes <hostname>.<subdomain> for every Pod that names it, which is
		// core/v1 behavior owing nothing to any controller.
		pod.Spec.Hostname = pod.Name
		pod.Spec.Subdomain = modelDeploymentReplicaServiceName(md, role.Name, ordinal)

		stampModelDeploymentRankEnv(pod, md, role, ordinal)
	}

	// The Kueue group metadata, which is what makes a replica's admission unit that replica alone:
	// the group name is derived per (role, ordinal), so each Pod joins its own one-member group.
	// It goes on before the fingerprint below, for the same reason the connector's annotations do:
	// the group name and the ordinal are two of the values a spec change or a scale moves, and a
	// fingerprint blind to them would leave every replica declaring a membership the deployment no
	// longer asks for.
	//
	// The labels and the annotations are applied together because the group's own type returns them
	// together -- a Pod carrying the membership label without the total count joins a group whose
	// size Kueue cannot learn, and no Workload is composed at all.
	group := ModelDeploymentPodGroup(md, role, ordinal, member)
	for k, v := range group.Labels {
		pod.Labels[k] = v
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, len(group.Annotations)+1)
	}
	for k, v := range group.Annotations {
		pod.Annotations[k] = v
	}

	// The fingerprint is written last so that it covers everything above it, and it is read back on
	// every pass to decide whether a running replica was built from the current spec.
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, 1)
	}
	pod.Annotations[modelDeploymentPodSpecHashAnnotation] = modelDeploymentPodSpecHash(pod)
}

// stampModelDeploymentRankEnv publishes the three facts a member needs to join its collective: who
// the leader is, how many members there are, and which one this is.
//
// IT PUBLISHES FACTS AND COMPOSES NO ARGUMENT. Which parallelism an engine turns on, and over how
// many ranks, stays the author's to say: the member count is a product of degrees that cannot be
// decomposed from one number, so a formula here would be a guess that runs instead of an error.
//
// THE INDEX COMES THROUGH THE DOWNWARD API RATHER THAN AS A LITERAL, and that is what keeps one
// template per ReplicaGroup: every member's container declares the same `fieldRef`, so the three
// entries are byte-identical across the members of a replica and only the label the stamp already
// wrote differs. Writing the index as a literal would work -- a live Pod is compared against the
// render of its own member index, not its replica's -- but it would make the container spec a
// per-member document, and the Boundaries invariant that one template describes a whole replica is
// worth more than the two lines it saves.
//
// ONLY THE MAIN CONTAINER IS STAMPED. The routing proxy is this operator's own sidecar and joins no
// collective; giving it ranks would describe a membership nothing acts on.
//
// A ROLE THAT TOOK OVER ITS COMMAND LINE IS STAMPED TOO, which is where the connector's rule does
// NOT carry over. The connector's entries name a config file the operator also wrote, so a take-over
// argv that never reads it has no use for them; these describe the cluster the Pod is running in, and
// an entrypoint composing its own launch is exactly what needs them.
func stampModelDeploymentRankEnv(
	pod *core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal int,
) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != modelDeploymentMainContainerName {
			continue
		}
		pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env,
			core.EnvVar{
				Name: modelDeploymentLeaderAddressEnv,
				Value: modelDeploymentMemberName(
					md, role.Name, ordinal, modelDeploymentLeaderMemberIndex) +
					"." + modelDeploymentReplicaServiceName(md, role.Name, ordinal),
			},
			core.EnvVar{
				Name:  modelDeploymentReplicaSizeEnv,
				Value: strconv.Itoa(modelDeploymentRoleSize(role)),
			},
			core.EnvVar{
				Name: modelDeploymentMemberIndexEnv,
				ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{
					FieldPath: "metadata.labels['" + modelDeploymentMemberIndexLabel + "']",
				}},
			},
		)

		return
	}
}

func modelDeploymentCommandPort(command []string) (int32, error) {
	for i := len(command) - 1; i >= 0; i-- {
		if ModelDeploymentArgName(command[i]) != modelDeploymentEnginePortArg {
			continue
		}
		_, value, inline := strings.Cut(command[i], "=")
		if !inline {
			if i+1 >= len(command) {
				return 0, fmt.Errorf("%s has no value", modelDeploymentEnginePortArg)
			}
			value = command[i+1]
		}
		port, err := strconv.ParseInt(value, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return 0, fmt.Errorf("%s has invalid value %q", modelDeploymentEnginePortArg, value)
		}
		return int32(port), nil
	}
	return 0, fmt.Errorf("%s is missing", modelDeploymentEnginePortArg)
}

// renderModelDeploymentRoutingSidecar builds the decode Pod's routing proxy. Which handshake it
// speaks follows the ENGINE and the pool's vendor, because the connector is how the sidecar asks
// the prefiller's bootstrap registry for transfer endpoints and each engine serves a different
// one: the value names a connector the sidecar dispatches on, not a flavor of one protocol.
//
// THE BOOTSTRAP PORT IS ONE VALUE WRITTEN AT BOTH ENDS. The prefiller names it in its own launch
// argument, rendered by the inject package from its constant, and the sidecar reads it here. Under
// mooncake the port travels as a flag; under SGLang it cannot, because the sidecar's SGLang port
// is a process-wide constant with no flag at all, overridable only through the
// SGLANG_BOOTSTRAP_PORT environment variable (llm-d/llm-d-router@v0.10.0
// `pkg/sidecar/proxy/connector_sglang.go:38-50`). Both ends read the same constant, so a pair
// cannot disagree about where the registry lives.
//
// ASCEND GETS NO PORT FLAG, because its pair has no registry to point at: the vLLM-Ascend
// connector hands the decoder the prefiller's host and port inside the per-request
// kv_transfer_params, and the sidecar's nixlv2 mode relays exactly that document
// (llm-d/llm-d-router@v0.10.0 `pkg/sidecar/proxy/connector_nixlv2.go:162-170,405`). The connector
// value is the sidecar's own constant vocabulary (`pkg/sidecar/constants/constants.go:21`,
// KVConnectorNIXLV2), spelled here rather than derived from anything the engine names.
func renderModelDeploymentRoutingSidecar(
	ctx context.Context,
	role *workercore.ModelDeploymentRole, enginePort int32, engineScheme core.URIScheme,
	engine string, native bool, manufacturer string,
) core.Container {
	externalPort := modelDeploymentServicePort(role)
	args := []string{
		fmt.Sprintf("--port=%d", externalPort.ContainerPort),
		fmt.Sprintf("--model-server-port=%d", enginePort),
	}
	env := []core.EnvVar{{
		Name: "POD_IP",
		ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{
			FieldPath: "status.podIP",
		}},
	}}
	switch {
	case engine == workercore.ModelDeploymentEngineSGLang:
		args = append(args, "--kv-connector=sglang")
		env = append(env, core.EnvVar{
			Name:  "SGLANG_BOOTSTRAP_PORT",
			Value: strconv.Itoa(int(inject.SGLangBootstrapPort)),
		})
	case manufacturer == nodefeature.ManufacturerAscend:
		args = append(args, "--kv-connector=nixlv2")
	default:
		args = append(args,
			"--kv-connector=mooncake",
			fmt.Sprintf("--mooncake-bootstrap-port=%d", inject.VLLMMooncakeBootstrapPort))
	}
	args = append(args, "--secure-proxy=false")
	if engineScheme == core.URISchemeHTTPS {
		args = append(args, "--enable-tls=decoder", "--tls-insecure-skip-verify=decoder")
	}

	return core.Container{
		Name:            "routing-proxy",
		Image:           redirectedImage(ctx, settings.ModelDeploymentRoutingSidecarImage.ShouldValue(ctx)),
		ImagePullPolicy: core.PullIfNotPresent,
		Args:            args,
		// The per-container restart policy is what makes the native shape a sidecar rather than a
		// plain init container, and it is ONLY rendered there: a regular container has no business
		// carrying it, so the classic shape leaves the field nil and lets the Pod's own
		// restartPolicy restart the proxy, which is the same kubelet mechanism one level up.
		RestartPolicy: func() *core.ContainerRestartPolicy {
			if !native {
				return nil
			}
			return ptr.To(core.ContainerRestartPolicyAlways)
		}(),
		Ports: []core.ContainerPort{{
			Name: externalPort.Name, Protocol: core.ProtocolTCP,
			ContainerPort: externalPort.ContainerPort,
		}},
		Env: env,
		SecurityContext: &core.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			RunAsNonRoot:             ptr.To(true),
		},
	}
}

// appendModelDeploymentConnectorPorts merges the connector's synthesized ports into the
// container's declared ones. A synthesized port whose number the template already declares under
// the same name is one endpoint declared twice, so the duplicate is dropped; the same number
// under a different name is two endpoints claiming one socket, which no merge can settle, so the
// render is refused.
func appendModelDeploymentConnectorPorts(
	ports []core.ContainerPort, connector ModelDeploymentConnectorRender, takeOver bool,
) ([]core.ContainerPort, error) {
	if takeOver {
		return ports, nil
	}

	for _, synthesized := range connector.Ports {
		declared := false
		for _, p := range ports {
			if p.ContainerPort != synthesized.ContainerPort {
				continue
			}
			if p.Name != synthesized.Name {
				return nil, fmt.Errorf(
					"the connector publishes port %d as %q but the template declares it as %q",
					synthesized.ContainerPort, synthesized.Name, p.Name)
			}
			declared = true
		}
		if !declared {
			ports = append(ports, synthesized)
		}
	}

	return ports, nil
}

// modelDeploymentPortTaken reports whether the number already appears among a container's
// declared ports.
func modelDeploymentPortTaken(ports []core.ContainerPort, port int32) bool {
	for _, p := range ports {
		if p.ContainerPort == port {
			return true
		}
	}
	return false
}

// modelDeploymentPodLabels is what a replica carries: the selector, the queue-name entrance label
// that routes it into the role's pool, the role-kind label whatever fronts the replicas selects on,
// and the part-of label every object this operator renders carries.
//
// The entrance label sits outside the selector deliberately. It follows the role's InstanceType,
// which a spec update can change, and a selector that moved with it would orphan every replica
// already running. The role-kind label is outside it for the reason stated on the constant.
//
// The entrance arrives as a VALUE, read from the InstanceType's status by the caller rather than
// derived from role.InstanceType here, so that this operator and the reconcile that creates the
// LocalQueue cannot disagree about the queue's name. See renderModelDeploymentPod for what deriving
// it would cost.
//
// The kind is resolved through ModelDeploymentEffectiveRoleKind rather than read off the field, so
// this label and status.roles[].kind can never name the same role differently.
func modelDeploymentPodLabels(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, entrance string,
) map[string]string {
	labels := modelDeploymentSelectorLabels(md, role)
	labels[kueuectrlconst.QueueLabel] = entrance
	labels[modelDeploymentLabelKeyRoleKind] = string(ModelDeploymentEffectiveRoleKind(role))
	labels["app.kubernetes.io/part-of"] = "gpustack-operator-worker"

	return labels
}

// modelDeploymentContainerPorts renders the ports a replica exposes.
//
// A role that names none still gets one, because a replica nothing can reach serves nothing and the
// Service fronting the deployment needs a target. Every supported engine's OpenAI-compatible server
// listens on 8000 by default.
func modelDeploymentContainerPorts(role *workercore.ModelDeploymentRole) []core.ContainerPort {
	if len(role.Ports) == 0 {
		return []core.ContainerPort{{
			Name:          modelDeploymentDefaultPortName,
			Protocol:      core.ProtocolTCP,
			ContainerPort: modelDeploymentDefaultPort,
		}}
	}

	return slicex.Transform(role.Ports, func(p workercore.ModelDeploymentPort) core.ContainerPort {
		return core.ContainerPort{
			Name:          modelDeploymentPortName(p),
			Protocol:      p.Protocol,
			ContainerPort: p.Port,
		}
	})
}

// modelDeploymentPortName derives a container port's name from its protocol and number, because a
// container port name must satisfy Kubernetes' own DNS-label rule and a user-supplied free-form
// name need not. The Instance path derives its own names the same way.
func modelDeploymentPortName(port workercore.ModelDeploymentPort) string {
	return strings.ToLower(fmt.Sprintf("%s-%d", port.Protocol, port.Port))
}

// convertModelDeploymentAdditionalVolumes turns a role's declared volumes into Pod volumes and
// container mounts. An entry with no source is skipped rather than rendered: a volume with an empty
// source would make the API server refuse the whole Pod on every reconcile.
func convertModelDeploymentAdditionalVolumes(
	avs []workercore.ModelDeploymentAdditionalVolume,
) (vols []core.Volume, mounts []core.VolumeMount) {
	if len(avs) == 0 {
		return nil, nil
	}

	vols = make([]core.Volume, 0, len(avs))
	mounts = make([]core.VolumeMount, 0, len(avs))
	for i := range avs {
		av := &avs[i]

		var vs core.VolumeSource
		switch {
		case av.ConfigMap != nil:
			vs.ConfigMap = &core.ConfigMapVolumeSource{
				LocalObjectReference: *av.ConfigMap,
			}
		case av.Secret != nil:
			vs.Secret = &core.SecretVolumeSource{
				SecretName: av.Secret.Name,
			}
		case av.HostPath != nil:
			vs.HostPath = av.HostPath.DeepCopy()
		default:
			continue
		}

		name := additionalVolumeName(i)
		vols = append(vols, core.Volume{
			Name:         name,
			VolumeSource: vs,
		})
		mounts = append(mounts, core.VolumeMount{
			Name:      name,
			MountPath: av.MountPath,
			ReadOnly:  av.ReadOnly,
			SubPath:   av.SubPath,
		})
	}

	return vols, mounts
}

// appendModelDeploymentBindArgs fills the engine's listen address when the role did not supply it.
//
// THE ENGINES DO NOT AGREE ON A DEFAULT, so leaving the address to them makes the Service correct
// for one and wrong for the other: vLLM opens every interface on 8000, while SGLang opens the
// loopback address on 30000, where nothing outside that container can reach it -- not the Service
// this operator renders, and not a kubelet probe. Rendering both flags makes the address the Service
// sends to and the address the engine opens ONE decision instead of two defaults that happen to
// agree on one engine.
//
// A FLAG THE ROLE ALREADY PASSED IS LEFT ALONE, matching "--flag value" and "--flag=value" alike.
// These are filled, not owned: a user who names a port keeps it, and the collision that ownership
// exists to prevent -- two values for one flag with no way to tell which won -- cannot arise,
// because nothing is appended when one is already there.
func appendModelDeploymentBindArgs(command []string, port int32) []string {
	for _, kv := range [][2]string{
		{modelDeploymentEngineHostArg, modelDeploymentEngineBindHost},
		{modelDeploymentEnginePortArg, strconv.Itoa(int(port))},
	} {
		if modelDeploymentArgsName(command, kv[0]) {
			continue
		}

		command = append(command, kv[0], kv[1])
	}

	return command
}

// modelDeploymentArgsName reports whether a command line already carries this flag, in either
// spelling.
func modelDeploymentArgsName(command []string, name string) bool {
	for _, arg := range command {
		if ModelDeploymentArgName(arg) == name {
			return true
		}
	}

	return false
}

// modelDeploymentEngineTransport reports the scheme the engine's listener speaks, and whether the
// operator can grade that listener at all.
//
// TWO FLAGS TURN THE LISTENER INTO A TLS ONE, and the rest of the --ssl-* family does not. Both
// supported engines hand every ssl argument to uvicorn without deciding anything themselves, and
// uvicorn enables TLS on `ssl_keyfile or ssl_certfile` alone. A CA bundle, a cipher list, a key
// password or a client-certificate mode passed BY ITSELF leaves an ordinary HTTP server -- so a
// prefix over the whole family would publish https:// for a listener serving plaintext, and withhold
// a gate that would have worked. Each engine also logs its own idea of "SSL enabled" from a
// different expression; those lines decide nothing and are not what this follows.
//
// THE TWO ANSWERS ARE INDEPENDENT, and deciding either one mid-scan would decide the other. Whether
// a client certificate may be demanded is only meaningful once TLS is on, and the two flags can
// arrive in either order, so both are collected before either answer is formed.
//
// IT READS THE RENDERED COMMAND LINE where one exists, and it must then be called BEFORE the listen
// address is filled in. The fill skips a flag wherever it already appears -- base argv, connector
// arguments, extra arguments -- so a check reading a narrower list would honor a connector-supplied
// port and then grade the operator's own.
//
// WHAT WITHDRAWS THE GATE. --host and --port move WHERE the engine listens, and the operator then
// leaves them alone, so it no longer knows the address and cannot grade it; the scheme such a
// listener speaks is still observable, and is still reported. --ssl-cert-reqs can demand a CLIENT
// certificate, which a kubelet probe has none to present -- but only where TLS is actually on,
// because uvicorn builds no TLS context for it to apply to otherwise.
//
// Everything else about TLS stays gated: the address is still the operator's own, only the transport
// moved, and the kubelet does not verify the server certificate on an HTTPS probe, so a self-signed
// pair is graded as readily as a public one.
func modelDeploymentEngineTransport(command []string) (core.URIScheme, bool) {
	var tls, clientAuth, moved bool

	for i, arg := range command {
		switch ModelDeploymentArgName(arg) {
		case modelDeploymentEngineHostArg, modelDeploymentEnginePortArg:
			moved = true
		case modelDeploymentEngineTLSCertArg, modelDeploymentEngineTLSKeyArg:
			tls = true
		case modelDeploymentEngineTLSClientAuthArg:
			// Assigned rather than OR-ed, so a repeated flag settles on its last occurrence, which
			// is the value the engine's own parser keeps.
			clientAuth = modelDeploymentDemandsClientCert(command, i)
		}
	}

	scheme := core.URISchemeHTTP
	if tls {
		scheme = core.URISchemeHTTPS
	}

	return scheme, !moved && (!tls || !clientAuth)
}

// modelDeploymentDemandsClientCert reports whether the --ssl-cert-reqs at index i asks for a client
// certificate the kubelet cannot present.
//
// ANYTHING BUT A KNOWN-PERMISSIVE VALUE MEANS YES, which is the cautious direction for this
// particular question. Withdrawing a gate costs a replica the readiness signal it had before any of
// this existed; fitting one to a listener that then refuses the probe leaves a replica serving every
// real client while the kubelet restarts it every startup budget. The first is a lost improvement,
// the second is an outage -- so an unreadable value, a missing one, and a number outside the enum
// are all treated alike as the refusal.
//
// The value is read as an INTEGER rather than compared as text, because the engine's parser does the
// same and "02" is CERT_REQUIRED to it.
func modelDeploymentDemandsClientCert(command []string, i int) bool {
	_, value, ok := strings.Cut(command[i], "=")
	if !ok {
		if i+1 >= len(command) {
			// The flag ends the command line, so it has no value to read at all.
			return true
		}

		value = command[i+1]
	}

	mode, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return true
	}

	return mode != modelDeploymentEngineTLSClientAuthNone &&
		mode != modelDeploymentEngineTLSClientAuthOptional
}

// modelDeploymentRoleArgs is the part of a replica's command line the ROLE contributes, which is the
// only part that can carry a listen or a TLS flag.
//
// It exists so that a caller with nothing but the spec -- the published endpoint is derived from the
// spec alone -- reaches the same answer the rendered command line would give it.
//
// IT IS A NARROWER LIST THAN THE RENDER SCANS, and the two agree only because of an invariant this
// function cannot enforce: the engine's base argv and the connector's rendered arguments carry no
// listen or TLS flag. A connector that grew one would put an HTTPS probe on a replica whose
// published address still said http://. That is why the invariant is asserted against what
// inject.Render actually emits, rather than left as a promise here.
//
// A take-over role contributes its whole argv and nothing else: the extra arguments are not appended
// to a command the user replaced, so reading them here would describe a command line that is not
// being run.
func modelDeploymentRoleArgs(role *workercore.ModelDeploymentRole) []string {
	if len(role.Command) > 0 {
		return role.Command
	}

	return role.ExtraArgs
}

// modelDeploymentProbes renders the startup, readiness and liveness gates a replica is graded by, or
// nothing at all for a role that took over the command line.
//
// WITHOUT THEM THE KUBELET REPORTS READY AS SOON AS THE PROCESS STARTS, which is a different fact
// from the engine serving and is separated from it by the whole model load. Every reader of
// status.roles[].ready and of status.endpoint takes the first for the second.
//
// THE THREE GATES ARE NOT REDUNDANT. Readiness alone would have to carry the entire load window in
// its failure threshold, which is the same as having no early failure detection at all; a startup
// gate carries that window instead and hands over to the other two once the engine has answered
// once.
//
// THE LIVENESS GATE IS SAFE ONLY BECAUSE THE STARTUP GATE EXISTS. On one supported engine this route
// answers 503 for the whole load, and a liveness gate reading it on its own would restart a replica
// that is loading normally. It never sees that window: the kubelet suppresses both liveness and
// readiness until the startup gate has succeeded, so by the time liveness runs, a 503 means an
// engine that answered once and stopped.
//
// WITHOUT IT SUCH A REPLICA HAS NO RECOVERY PATH. Losing readiness only withdraws it from the
// Service; the Pod keeps running, keeps the accelerators Kueue admitted it with, and this controller
// deletes a replica only when the deployment is torn down, when the spec no longer names its role,
// when a scale-down sheds its ordinal, or when its rendered spec changed -- never because it
// stopped answering.
//
// A TAKE-OVER ROLE GETS NEITHER, for the reason the command, the connector volumes and the client
// environment above it are also withheld: the operator did not build that command line, so it cannot
// claim the container serves this route on this port. Rendering a gate against a command it did not
// write would leave a working replica permanently unready, which is a worse failure than the one
// probes are being added for.
//
// A ROLE THAT MOVES WHERE THE ENGINE LISTENS GETS NEITHER. The operator renders --host and --port
// only when the role passed neither, so in every other case the address the gate reads is the
// operator's own and the engine is on it; a role that supplies one moves the engine without moving
// the Service, and the gate would report a working replica as broken.
//
// A ROLE THAT MOVES ONLY HOW IT LISTENS STAYS GATED, over HTTPS. Losing readiness to one ordinary
// TLS flag would hand back the defect these gates remove, and it is not necessary: the address is
// still the operator's own, and the kubelet does not verify the server certificate. The one
// exception is the flag that can demand a CLIENT certificate, which a probe cannot present.
//
// A DECLARED ports LIST NO LONGER WITHHOLDS THE GATES, because the rendered --port is taken from
// the same figure the Service targets. The two agreeing is what makes "ready" and "the endpoint
// answers" one fact rather than two.
//
// A PORT DECLARED AS UDP OR SCTP GETS NEITHER, and this one fails the other way round. The protocol
// is an enum the role may set and the Service copies it, while an HTTP engine speaks TCP whatever
// the declaration says -- so an HTTP gate would SUCCEED against a replica whose published endpoint
// forwards a protocol nothing answers, and report as serving a deployment that cannot be reached.
// Every other case here withholds the gates to avoid calling a working replica broken; this one
// withholds them to avoid calling a broken one working.
//
// Where the port IS the operator's own, it is read from modelDeploymentServicePort rather than
// picked here, so the address the gate grades and the address the Service sends traffic to cannot
// become two different ports. That is what makes "ready" and "the endpoint answers" one fact rather
// than two that a test has to compare.
//
// THE ROUTE IS THE CALLER'S, because one shape cannot use the shared one: a direct decoder's
// service port is owned by its routing sidecar, which answers /health itself, so the gates read
// the route the sidecar forwards to the engine instead -- a different path on the SAME address,
// which leaves the one-fact property above intact.
func modelDeploymentProbes(
	role *workercore.ModelDeploymentRole, scheme core.URIScheme, path string, gradable bool,
) (startup, readiness, liveness *core.Probe) {
	if !gradable {
		return nil, nil, nil
	}

	servicePort := modelDeploymentServicePort(role)
	if servicePort.Protocol != core.ProtocolTCP {
		return nil, nil, nil
	}

	port := intstr.FromInt32(servicePort.ContainerPort)
	gate := func(failureThreshold int32) *core.Probe {
		return &core.Probe{
			ProbeHandler: core.ProbeHandler{
				HTTPGet: &core.HTTPGetAction{
					Path:   path,
					Port:   port,
					Scheme: scheme,
				},
			},
			PeriodSeconds:    modelDeploymentProbePeriodSeconds,
			TimeoutSeconds:   modelDeploymentProbeTimeoutSeconds,
			FailureThreshold: failureThreshold,
		}
	}

	return gate(modelDeploymentStartupFailureThreshold),
		gate(modelDeploymentReadinessFailureThreshold),
		gate(modelDeploymentLivenessFailureThreshold)
}

// mergeModelDeploymentEnv folds the operator's environment and the role's into one list.
//
// The order of the result is the order of authority, most authoritative first, so that reading a
// rendered Pod answers "who set this" without consulting the table: what the operator OWNS, then
// what it merely DEFAULTS, then what the user appended. Owned entries are refused at admission, so
// skipping them here is belt and braces rather than the enforcement — but a renderer that let one
// through would hand the failure to the engine, one layer away from the field that caused it.
//
// A role that takes over the command line gets none of the operator's entries. Its argv never names
// the file they point at, so setting them would describe a configuration nothing reads.
func mergeModelDeploymentEnv(
	engine string, role *workercore.ModelDeploymentRole,
	connector ModelDeploymentConnectorRender, takeOver bool,
) []core.EnvVar {
	if takeOver {
		return mergeModelDeploymentUserEnv(nil, engine, role.Env)
	}

	env := make([]core.EnvVar, 0, len(connector.Env)+len(connector.DefaultedEnv)+len(role.Env))
	env = append(env, connector.Env...)
	for _, e := range connector.DefaultedEnv {
		if modelDeploymentUserSetsEnv(role.Env, e.Name) {
			continue
		}
		env = append(env, e)
	}

	return mergeModelDeploymentUserEnv(env, engine, role.Env)
}

// mergeModelDeploymentUserEnv appends the user's entries to what the operator already rendered,
// letting a later tier replace an earlier one by name and never replacing what the operator owns.
func mergeModelDeploymentUserEnv(
	env []core.EnvVar, engine string, userEnv []workercore.ModelDeploymentEnvVar,
) []core.EnvVar {
	for i := range userEnv {
		if ModelDeploymentOwnsEnv(engine, userEnv[i].Name) {
			continue
		}
		replaced := false
		for j := range env {
			if env[j].Name != userEnv[i].Name {
				continue
			}
			env[j].Value = userEnv[i].Value
			replaced = true

			break
		}
		if !replaced {
			env = append(env, core.EnvVar{Name: userEnv[i].Name, Value: userEnv[i].Value})
		}
	}

	return env
}

// modelDeploymentUserSetsEnv reports whether the user supplied the named variable.
func modelDeploymentUserSetsEnv(userEnv []workercore.ModelDeploymentEnvVar, name string) bool {
	for i := range userEnv {
		if userEnv[i].Name == name {
			return true
		}
	}

	return false
}

// deriveModelDeploymentResources turns a role's accelerator request into the full resource request
// one replica makes.
//
// CPU, memory and ephemeral storage are DERIVED here because they are not expressible on the role
// at all. The Instance path derives the same values in its mutating webhook and writes them onto the
// object; this CRD's mutating webhook defaults only the accelerator count, so the derivation happens
// at render time and the values live only on the Pod. The arithmetic is the same one, restricted to the case that is
// the only one reachable here — nothing declared — so an InstanceType's per-unit resources scaled by
// the requested share is the whole of it.
//
// A request that cannot be sized is an error rather than a fallback. Sizing a partition or a slice
// as a whole card would charge quota for something other than what runs, and the state that causes
// it (an accelerator detail not computed yet) clears on its own, so the caller can retry.
//
// It takes no overcommit flag, unlike the webhook path it mirrors. Overcommit decides whether a
// value a user DECLARED is recomputed, and nothing is declared here; the request-versus-limit split
// it also drives is applied downstream by getResourceRequirements.
func deriveModelDeploymentResources(
	role *workercore.ModelDeploymentRole,
	instType *worker.InstanceType,
) (*workercore.InstanceResources, error) {
	ress := new(workercore.InstanceResources)
	if rr := role.Resources; rr != nil {
		ress.Accelerator = rr.Accelerator
		ress.AcceleratorSlicedMemoryPercentage = rr.AcceleratorSlicedMemoryPercentage
		ress.AcceleratorSlicedCoresPercentage = rr.AcceleratorSlicedCoresPercentage
		ress.AcceleratorPartitionedProfile = rr.AcceleratorPartitionedProfile
	}

	if err := sizeModelDeploymentHostResources(ress, instType); err != nil {
		return nil, err
	}

	// Default the local storage the way the Instance webhook does: 15Gi, never above what the
	// InstanceType offers.
	def := resource.NewQuantity(15<<30, resource.BinarySI) // 15Gi
	if instType.Spec.LocalStorage != "" {
		if maxStg, err := resource.ParseQuantity(instType.Spec.LocalStorage); err == nil && def.Cmp(maxStg) > 0 {
			def = &maxStg
		}
	}
	ress.LocalStorage = *def

	return ress, nil
}

// sizeModelDeploymentHostResources fills the CPU and memory a replica asks of its host.
func sizeModelDeploymentHostResources(
	ress *workercore.InstanceResources, instType *worker.InstanceType,
) error {
	if !instType.Spec.Acceleratable {
		// A non-accelerated pool sizes one unit's worth of host resources, matching what the
		// Instance path gives a request that declares no CPU of its own.
		cpu, ram, err := modelDeploymentUnitResources(instType, 1)
		if err != nil {
			return err
		}
		ress.CPU, ress.RAM = cpu, ram

		return nil
	}

	slicing := ress.AcceleratorSlicedMemoryPercentage != 0 || ress.AcceleratorSlicedCoresPercentage != 0
	partitioning := ress.AcceleratorPartitionedProfile != ""
	if (slicing || partitioning) && !instType.Status.Detail.AcceleratorReady() {
		// The share of a card being asked for cannot be read yet. This clears once the detail is
		// computed, so it is a retryable error and never a whole-card fallback.
		return fmt.Errorf("instance type %s is not ready yet (accelerator detail not computed); retry", instType.Name)
	}

	partitionPct, sizeable := PartitionProfileMemoryPercent((*workercore.InstanceType)(instType),
		ress.AcceleratorPartitionedProfile)
	if !sizeable {
		return fmt.Errorf("instance type %s is not ready yet (partition profile %q cannot be sized "+
			"from the observed accelerator detail); retry", instType.Name, ress.AcceleratorPartitionedProfile)
	}

	switch {
	case partitionPct > 0:
		// A hardware partition holds a share of ONE card, so the host resources follow the share of
		// that card's VRAM the profile occupies — the same VRAM-anchored fraction a logical slice
		// uses, so a partition and a slice of equal size cost equal host resources.
		return modelDeploymentSizeByPercent(ress, instType, partitionPct)
	case instType.Status.Detail.IsLogicallySliceable() && slicing:
		// When only one of the two percentages is set, the other follows it, so a bare memory
		// request yields an equal compute share and the reverse.
		switch {
		case ress.AcceleratorSlicedMemoryPercentage > 0 && ress.AcceleratorSlicedCoresPercentage == 0:
			ress.AcceleratorSlicedCoresPercentage = ress.AcceleratorSlicedMemoryPercentage
		case ress.AcceleratorSlicedCoresPercentage > 0 && ress.AcceleratorSlicedMemoryPercentage == 0:
			ress.AcceleratorSlicedMemoryPercentage = ress.AcceleratorSlicedCoresPercentage
		}
		// The memory percentage is the fraction of the card actually reserved; the compute
		// percentage throttles GPU cores and not host resources.
		return modelDeploymentSizeByPercent(ress, instType, int64(ress.AcceleratorSlicedMemoryPercentage))
	}

	// A whole-card request scales the unit resources by the card count. A replica asking for no
	// accelerator at all — legitimate for a small model on an accelerated pool — still needs a host
	// to run on, and is sized as one unit rather than as nothing.
	cards := int64(1)
	if ress.Accelerator != nil && ress.Accelerator.Value() > 0 {
		cards = ress.Accelerator.Value()
	}
	cpu, ram, err := modelDeploymentUnitResources(instType, cards)
	if err != nil {
		return err
	}
	ress.CPU, ress.RAM = cpu, ram

	return nil
}

// modelDeploymentSizeByPercent sizes the host resources as a percentage of one card's worth.
func modelDeploymentSizeByPercent(
	ress *workercore.InstanceResources, instType *worker.InstanceType, pct int64,
) error {
	cpu, err := quantityx.StringPercentMultiply(instType.Spec.UnitResources.CPU, pct)
	if err != nil {
		return fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err)
	}
	ram, err := quantityx.StringPercentMultiply(instType.Spec.UnitResources.RAM, pct)
	if err != nil {
		return fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err)
	}
	ress.CPU, ress.RAM = cpu, ram

	return nil
}

// modelDeploymentUnitResources scales one card's worth of host resources by a whole-card count.
func modelDeploymentUnitResources(
	instType *worker.InstanceType, multiplier int64,
) (cpu, ram resource.Quantity, err error) {
	cpu, err = quantityx.StringMultiply(instType.Spec.UnitResources.CPU, multiplier)
	if err != nil {
		return cpu, ram, fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err)
	}
	ram, err = quantityx.StringMultiply(instType.Spec.UnitResources.RAM, multiplier)
	if err != nil {
		return cpu, ram, fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err)
	}

	return cpu, ram, nil
}

// modelDeploymentPodSpecHash fingerprints a rendered replica.
//
// It covers the labels, the annotations and the spec, because a change to any of them is a change
// the running replica does not have: the entrance label decides which pool admits it, and the
// resource note decides which watch sees it. The hash annotation itself is stripped, so the
// fingerprint never covers itself — a value that did would change on every render and roll the
// deployment forever.
func modelDeploymentPodSpecHash(pod *core.Pod) string {
	annotations := make(map[string]string, len(pod.Annotations))
	for k, v := range pod.Annotations {
		if k == modelDeploymentPodSpecHashAnnotation {
			continue
		}
		annotations[k] = v
	}

	subject := struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        core.PodSpec      `json:"spec"`
	}{
		Labels:      pod.Labels,
		Annotations: annotations,
		Spec:        pod.Spec,
	}

	// JSON rather than fmt: it orders map keys, so two renders of one spec hash identically. A
	// marshal failure is not reachable for a PodSpec, and treating it as an empty digest would
	// silently disable every rollout, so it is surfaced as a value nothing can match.
	encoded, err := json.Marshal(subject)
	if err != nil {
		return "unhashable-" + err.Error()
	}

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}
