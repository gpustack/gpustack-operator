package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	modelDeploymentRouterConfigKey            = "epp.yaml"
	modelDeploymentRouterEnvoyConfigKey       = "envoy.yaml"
	modelDeploymentRouterSelectorKey          = "endpoint-selector"
	modelDeploymentRouterTargetPortsKey       = "endpoint-target-ports"
	modelDeploymentRouterExtraArgsKey         = "extra-args.json"
	modelDeploymentRouterConfigHash           = "modeldeployment.gpustack.ai/router-config-hash"
	modelDeploymentRouterLabelKey             = "modeldeployment.gpustack.ai/router"
	modelDeploymentRouterHTTPPort       int32 = 8081
	modelDeploymentRouterEPPPort        int32 = 9002
	// modelDeploymentRouterMetricsPort is where every router serves Prometheus metrics, so that one
	// scrape configuration reaches all of them. The llm-d picker listens here by default; the
	// routers configured by argv are told to, because their own default is elsewhere.
	modelDeploymentRouterMetricsPort  int32 = 9090
	modelDeploymentResourceNoteRouter       = "router"
)

// ModelDeploymentRouterObjects carries the complete namespaced object set one managed router needs
// and the typed contract rendered alongside it.
type ModelDeploymentRouterObjects struct {
	Deployment     *app.Deployment
	ConfigMap      *core.ConfigMap
	Service        *core.Service
	ServiceAccount *core.ServiceAccount
	Role           *rbac.Role
	RoleBinding    *rbac.RoleBinding

	// Contract is the typed projection of the same values used to render the objects above.
	Contract workercore.ModelDeploymentRouterStatus
}

func renderModelDeploymentRouterObjects(
	ctx context.Context, md *workercore.ModelDeployment, manufacturers map[string]string,
) (ModelDeploymentRouterObjects, error) {
	name := md.Name + "-router"
	labels := map[string]string{
		modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance: md.Name,
		modelDeploymentRouterLabelKey:   md.Spec.Router.Name,
	}
	note := func(object ctrlcli.Object) {
		systemmeta.NoteResource(object, ModelDeploymentResourceType,
			map[string]string{modelDeploymentResourceNoteRouter: md.Spec.Router.Name})
		kubemeta.ControlOnWithoutBlock(object, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
	}

	roles := make([]router.Role, 0, len(md.Spec.Roles))
	ports := make([]int32, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roles = append(roles, router.Role{
			Kind:         string(ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i])),
			RoleLabelKey: modelDeploymentLabelKeyRoleKind,
		})
		ports = append(ports, ModelDeploymentRoleServingPort(md, &md.Spec.Roles[i]))
	}
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if len(ports) != 1 {
		return ModelDeploymentRouterObjects{}, fmt.Errorf(
			"router requires every role to use the same serving port; got %v", ports)
	}

	contract := workercore.ModelDeploymentRouterStatus{
		Name:  md.Spec.Router.Name,
		Roles: make([]workercore.ModelDeploymentRouterRoleStatus, 0, len(md.Spec.Roles)),
	}
	// THE ENGINE METRICS TABLE IS THE PICKER'S PRECONDITION ALONE, at this call site for the same
	// reason as at the admission one: its refusal names the router from the renderer package's own
	// constant rather than from this object, so calling it for another router reports a requirement
	// in a third router's name. The other two score on their own observations and read none of it,
	// which is also why the published contract carries no metrics for them -- a status naming
	// metrics nothing scrapes is a claim about a router that is not running.
	var metrics router.Metrics
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD {
		var err error
		if metrics, err = router.MetricsForEngine(md.Spec.Engine.Name); err != nil {
			return ModelDeploymentRouterObjects{}, err
		}
		metrics.Port = ports[0]
		contract.Metrics = &workercore.ModelDeploymentRouterMetrics{
			Port: metrics.Port, QueuedRequests: metrics.QueuedRequests,
			RunningRequests: metrics.RunningRequests, KVCacheUtilization: metrics.KVCacheUtilization,
		}
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		kind := ModelDeploymentEffectiveRoleKind(role)
		// THE PUBLISHED SELECTOR NAMES THE PODS A REQUEST MAY REACH, which is one member per
		// replica -- the leader -- rather than every Pod the role runs. The field is published
		// VERBATIM for a router to be configured from, so a selector that also named the members
		// serving no API would hand that router the very defect the discovery selector below
		// exists to remove. The leader term is what keeps the two selectors in one semantics; they
		// differ in scope only, this one naming a role and that one the deployment.
		selector := modelDeploymentSelectorLabels(md, role)
		selector[modelDeploymentLabelKeyRoleKind] = string(kind)
		selector[modelDeploymentMemberIndexLabel] = strconvx.Itoa(modelDeploymentLeaderMemberIndex)
		// The renderer is handed the SAME map that is published, rather than one built beside it,
		// because a router configured by argv discovers each role by these labels: two derivations
		// of one answer would let what a user reads and what the router matches drift apart.
		roles[i].Selector = maps.Clone(selector)
		roleStatus := workercore.ModelDeploymentRouterRoleStatus{
			Name: role.Name, Kind: kind, Selector: selector, Endpoint: modelDeploymentRoleEndpoint(md, role),
		}
		if modelDeploymentPublishesKVEvents(md, role, manufacturers[role.Name]) {
			events := inject.VLLMKVEvents(md.Name + "-" + role.Name + "." + md.Namespace + ".svc")
			roleStatus.KVEvents = &workercore.ModelDeploymentRouterKVEvents{
				Endpoint: events.Endpoint, ReplayEndpoint: events.ReplayEndpoint, Topic: events.Topic,
			}
		}
		contract.Roles = append(contract.Roles, roleStatus)
	}
	// DISCOVERY NAMES ONLY THE PODS THAT ANSWER THE API: one member per replica, the leader, which
	// at size one is the replica's only Pod. Without the index term the selector would name every
	// member, a router would spread requests across the ones serving nothing, and the failures
	// would interleave with successes rather than the router being visibly broken. The term is a
	// plain equality rather than something sized per role, because a label selector is a
	// conjunction and the pickers filter by kind: two server roles of different sizes share this
	// one selector, and only a member index written on every member lets a single term hold for
	// both.
	//
	// THE EQUALITIES ARE THE SOURCE AND THE EXPRESSION IS DERIVED FROM THEM, because the two
	// routers configured by argv match on equality alone and cannot be handed an expression: one of
	// them splits each entry on its first "=" and drops an entry carrying none without a word. A
	// second literal here would let the string and the map name different sets.
	endpointLabels := map[string]string{
		modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance: md.Name,
		modelDeploymentMemberIndexLabel: strconvx.Itoa(modelDeploymentLeaderMemberIndex),
	}
	// The router's own Pod carries the first two labels and NOT the member index, so the equalities
	// already exclude it. The negation is kept in the expression, where it costs nothing and says
	// out loud what the index term achieves by arithmetic.
	//
	// The terms are listed rather than sorted out of the map because their ORDER IS RENDERED: this
	// string is a ConfigMap value and the Pod's configuration hash is taken over it, so reordering
	// it would roll every router in the cluster for a change nothing reads.
	endpointSelector := strings.Join([]string{
		modelDeploymentLabelKeyName + "=" + endpointLabels[modelDeploymentLabelKeyName],
		modelDeploymentLabelKeyInstance + "=" + endpointLabels[modelDeploymentLabelKeyInstance],
		modelDeploymentMemberIndexLabel + "=" + endpointLabels[modelDeploymentMemberIndexLabel],
		"!" + modelDeploymentRouterLabelKey,
	}, ",")
	routerInput := router.Input{
		Roles: roles, Metrics: metrics, ModelName: md.Spec.Model.Name,
		TokenizerEndpoint: modelDeploymentRoleEndpoint(md, modelDeploymentTokenizerRole(md)),
		Namespace:         md.Namespace, EndpointSelector: endpointSelector,
		EndpointLabels:          endpointLabels,
		ListenPort:              modelDeploymentRouterHTTPPort,
		MetricsPort:             modelDeploymentRouterMetricsPort,
		DiscoveryPort:           ports[0],
		RequestTimeoutSeconds:   md.Spec.Router.RequestTimeoutSeconds,
		DisaggregationThreshold: md.Spec.Router.DisaggregationThresholdTokens,
	}
	if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM {
		routerInput.KVEvents = router.KVEvents{
			Engine: md.Spec.Engine.Name, Port: inject.VLLMKVEventsPort,
			ReplayPort: inject.VLLMKVEventsReplayPort, Topic: inject.VLLMKVEventsTopic,
		}
	}
	rendered, err := router.Render(md.Spec.Router.Name, routerInput)
	if err != nil {
		return ModelDeploymentRouterObjects{}, err
	}
	extraArgs, err := json.Marshal(md.Spec.Router.ExtraArgs)
	if err != nil {
		return ModelDeploymentRouterObjects{}, fmt.Errorf("render router extra arguments: %w", err)
	}
	workload, err := renderModelDeploymentRouterWorkload(ctx, md, modelDeploymentRouterDerived{
		Name:             name,
		Rendered:         rendered,
		EndpointSelector: endpointSelector,
		TargetPorts:      joinInt32s(ports),
		ExtraArgs:        string(extraArgs),
	})
	if err != nil {
		return ModelDeploymentRouterObjects{}, err
	}

	// NO CONFIGURATION MEANS NO OBJECT. A router configured entirely by argv reads nothing from a
	// ConfigMap, and rendering an empty one would leave an object whose absence and whose emptiness
	// a reader cannot tell apart, plus a config hash taken over nothing.
	var configMap *core.ConfigMap
	annotations := modelDeploymentMetricsAnnotations(modelDeploymentRouterMetricsPort, core.URISchemeHTTP)
	if len(workload.Config) > 0 {
		configMap = &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
			Data:       workload.Config,
		}
		note(configMap)
		annotations[modelDeploymentRouterConfigHash] = hashRouterConfig(configMap.Data)
	}

	replicas := int32(1)
	if md.Spec.Router.Replicas != nil {
		replicas = *md.Spec.Router.Replicas
	}
	deployment := &app.Deployment{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Spec: app.DeploymentSpec{
			Replicas: &replicas,
			Selector: &meta.LabelSelector{MatchLabels: maps.Clone(labels)},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: maps.Clone(labels), Annotations: annotations},
				Spec: core.PodSpec{
					ServiceAccountName: name,
					ImagePullSecrets:   md.Spec.Router.ImagePullSecrets,
					Containers:         workload.Containers,
					Volumes:            workload.Volumes,
				},
			},
		},
	}
	note(deployment)

	service := &core.Service{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Spec: core.ServiceSpec{Selector: maps.Clone(labels), Ports: []core.ServicePort{{
			Name: "http", Port: modelDeploymentRouterHTTPPort,
			TargetPort: intstr.FromInt32(modelDeploymentRouterHTTPPort),
		}}},
	}
	note(service)
	contract.Endpoint = modelDeploymentRouterEndpoint(service)

	serviceAccount := &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)}}
	note(serviceAccount)
	role := &rbac.Role{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Rules:      []rbac.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}}},
	}
	note(role)
	roleBinding := &rbac.RoleBinding{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Subjects:   []rbac.Subject{{Kind: rbac.ServiceAccountKind, Name: name, Namespace: md.Namespace}},
		RoleRef:    rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "Role", Name: name},
	}
	note(roleBinding)

	return ModelDeploymentRouterObjects{
		Deployment: deployment, ConfigMap: configMap, Service: service,
		ServiceAccount: serviceAccount, Role: role, RoleBinding: roleBinding, Contract: contract,
	}, nil
}

// modelDeploymentRouterWorkload is what ONE router contributes to the object set: the configuration
// only it reads, the containers only it runs, and the volumes those containers mount.
//
// Everything else a managed router needs is the same whichever one was asked for -- the object
// names and labels, the ownership notes, the Service, ServiceAccount, Role and RoleBinding, the
// discovery selector, and the contract published beside them -- so it is rendered once by the
// caller rather than once per router.
type modelDeploymentRouterWorkload struct {
	// Config is added to the shared ConfigMap. A key here must not collide with a shared one: the
	// shared entries are written first and these overwrite them, which is the wrong direction for a
	// value the caller derived.
	Config map[string]string

	// Containers is the Pod's container list, in start order.
	Containers []core.Container

	// Volumes are the volumes those containers mount, and nothing else.
	Volumes []core.Volume
}

// renderModelDeploymentRouterWorkload renders the half of a managed router that differs per router.
//
// THE PROXY, THE MOUNTED DOCUMENT AND ITS ACCESS LOG BELONG TO THE LLM-D BRANCH ALONE. That router
// is an ext_proc extension rather than a proxy, so something else has to terminate HTTP in front of
// it and be the thing a Service can point at, and its scheduling is configured by a file rather
// than by arguments. A shared half that rendered any of those would give a single-process router a
// sidecar it does not use, a document it does not read, and a second log of the same requests.
//
// The refusal at the end is not reachable through the caller, which renders the configuration
// first and is refused there for an unknown name. It is here because THIS is the function a fourth
// router is added to, and the failure it prevents is silent: a branch that returns the zero value
// renders a Deployment with no containers at all.
// modelDeploymentRouterDerived is what the shared half worked out, handed to the branch that knows
// which of it its own router reads.
type modelDeploymentRouterDerived struct {
	// Name is the name every object in the set carries.
	Name string

	// Rendered is the router's own configuration, which is a document or arguments.
	Rendered router.Output

	// EndpointSelector, TargetPorts and ExtraArgs are the same values the arguments are derived
	// from, in the spelling a ConfigMap entry takes. A router that reads them from argv does not
	// take them here as well.
	EndpointSelector string
	TargetPorts      string
	ExtraArgs        string
}

func renderModelDeploymentRouterWorkload(
	ctx context.Context, md *workercore.ModelDeployment, in modelDeploymentRouterDerived,
) (modelDeploymentRouterWorkload, error) {
	switch md.Spec.Router.Name {
	case workercore.ModelDeploymentRouterLLMD:
		return renderModelDeploymentLLMDRouterWorkload(ctx, md, in), nil
	case workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentRouterSGLang:
		return renderModelDeploymentArgvRouterWorkload(ctx, md, in.Rendered), nil
	default:
		return modelDeploymentRouterWorkload{},
			fmt.Errorf("router %q renders no workload", md.Spec.Router.Name)
	}
}

// renderModelDeploymentArgvRouterWorkload renders a router that is one process configured entirely
// by its command line.
//
// IT CONTRIBUTES NO CONFIGURATION AT ALL, which is why the caller renders no ConfigMap for it. The
// selector and the target port do reach this router, but as arguments -- writing them into an
// object as well would put a second copy of the same derivation where nothing reads it, and leave a
// reader of the ConfigMap unable to tell which copy the process is actually using.
func renderModelDeploymentArgvRouterWorkload(
	ctx context.Context, md *workercore.ModelDeployment, rendered router.Output,
) modelDeploymentRouterWorkload {
	image := md.Spec.Router.Image
	if image == "" {
		image = redirectedImage(ctx, settings.ModelDeploymentRouterImage.ShouldValue(ctx))
	}

	// The user's arguments land AFTER the derived ones, which is the order the refusal in admission
	// is written against: a key the operator owns is refused there, so anything surviving to here
	// names something the operator did not set.
	args := slices.Concat(rendered.Arguments, md.Spec.Router.ExtraArgs)

	return modelDeploymentRouterWorkload{
		Containers: []core.Container{{
			// The command comes from the render beside the arguments, because the image holds three
			// programs and declares no entrypoint. Arguments without it are handed to the image's
			// own CMD position, where the first flag is read as the program name.
			Name: "router", Image: image, Command: rendered.Command, Args: args,
			ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, image),
			Ports: []core.ContainerPort{
				{Name: "http", ContainerPort: modelDeploymentRouterHTTPPort},
				{Name: "metrics", ContainerPort: modelDeploymentRouterMetricsPort},
			},
			// The router answers on the port the Service targets, so a TCP probe on it is the same
			// question the Service asks. There is no second process to gate on, unlike the picker
			// whose readiness is its own gRPC health server behind a proxy.
			ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
				TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(modelDeploymentRouterHTTPPort)},
			}},
		}},
	}
}

func renderModelDeploymentLLMDRouterWorkload(
	ctx context.Context, md *workercore.ModelDeployment, in modelDeploymentRouterDerived,
) modelDeploymentRouterWorkload {
	name := in.Name
	// A picker named on the object is the user's own reference and is used verbatim; only the
	// default this operator picked is redirected to the cluster's mirror.
	image := md.Spec.Router.Image
	if image == "" {
		image = redirectedImage(ctx,
			settings.ModelDeploymentRouterImage.ShouldValue(ctx))
	}
	proxyImage := redirectedImage(ctx,
		settings.ModelDeploymentRouterProxyImage.ShouldValue(ctx))

	args := make([]string, 0, 5+len(md.Spec.Router.ExtraArgs))
	// TWO OF THESE TURN OFF A DEFAULT THAT IS ON UPSTREAM, and that is the design rather than an
	// oversight. The endpoint picker defaults both --secure-serving and --metrics-endpoint-auth to
	// true, so a reader comparing this list against upstream finds two inversions; without the
	// reason beside them they read as a bug, and the next person proposes putting them back.
	//
	// A ROUTER MANAGES EAST-WEST TRAFFIC: it picks which replica of this deployment serves a request
	// that is already inside the cluster. Transport security and caller authentication are
	// NORTH-SOUTH concerns, and they belong to the gateway that admits traffic into the cluster,
	// where one policy covers every workload instead of each workload carrying its own.
	//
	// Neither flag would protect anything where it sits, either. --secure-serving puts TLS on the
	// ext_proc gRPC server, whose only client is the Envoy container in THIS Pod dialing 127.0.0.1
	// -- a loopback socket inside one network namespace. --metrics-endpoint-auth puts authentication
	// on the picker's own /metrics, which is scraped from inside the cluster.
	//
	// So neither is offered as a field, and both are refused in spec.router.extraArgs: the boundary
	// has one answer for the whole installation rather than a different one per object.
	args = append(args,
		"--endpoint-selector=$(ENDPOINT_SELECTOR)",
		"--endpoint-target-ports=$(ENDPOINT_TARGET_PORTS)",
		"--config-file=/config/"+modelDeploymentRouterConfigKey,
		"--secure-serving=false",
		"--grpc-health-port=9003",
		"--metrics-endpoint-auth=false",
	)
	args = append(args, md.Spec.Router.ExtraArgs...)

	return modelDeploymentRouterWorkload{
		// The picker reads the first two as a mounted file and the next two through its
		// environment; the last is here so that a change to arguments the user declared moves the
		// configuration hash, which is what rolls the Pod.
		Config: map[string]string{
			modelDeploymentRouterConfigKey: in.Rendered.Document,
			modelDeploymentRouterEnvoyConfigKey: renderModelDeploymentRouterEnvoyConfig(
				md.Spec.Router.RequestTimeoutSeconds),
			modelDeploymentRouterSelectorKey:    in.EndpointSelector,
			modelDeploymentRouterTargetPortsKey: in.TargetPorts,
			modelDeploymentRouterExtraArgsKey:   in.ExtraArgs,
		},
		Containers: []core.Container{
			{
				Name: "envoy", Image: proxyImage,
				// The same policy as the container below, for the same reason: this image
				// is resolved through the redirect too, so it is as replaceable as the
				// other one, and a mutable tag left on the default policy goes stale on
				// a node that already has it.
				//
				// RESOLVED rather than passed through, because the field is optional and
				// nothing defaults it: an empty value rendered here is one the API server
				// fills in on write, which the aligner then reads back as a difference and
				// corrects on every pass, updating the Deployment forever.
				ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, proxyImage),
				Args:            []string{"-c", "/config/" + modelDeploymentRouterEnvoyConfigKey},
				Ports:           []core.ContainerPort{{Name: "http", ContainerPort: modelDeploymentRouterHTTPPort}},
				ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(modelDeploymentRouterHTTPPort)},
				}},
				VolumeMounts: []core.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
			},
			{
				// As above: this container runs one of the three programs the image carries, and
				// the image names none of them itself.
				Name: "epp", Image: image, Command: in.Rendered.Command, Args: args,
				ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, image),
				Env: []core.EnvVar{
					configMapEnv(modelDeploymentRouterSelectorKey, "ENDPOINT_SELECTOR", name),
					configMapEnv(modelDeploymentRouterTargetPortsKey, "ENDPOINT_TARGET_PORTS", name),
					{Name: "NAMESPACE", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					{Name: "POD_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"}}},
				},
				Ports: []core.ContainerPort{
					{Name: "grpc", ContainerPort: modelDeploymentRouterEPPPort},
					{Name: "grpc-health", ContainerPort: 9003},
					{Name: "metrics", ContainerPort: 9090},
				},
				ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
					GRPC: &core.GRPCAction{Port: 9003},
				}},
				VolumeMounts: []core.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
			},
		},
		Volumes: []core.Volume{{Name: "config", VolumeSource: core.VolumeSource{
			ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: name}},
		}}},
	}
}

func modelDeploymentTokenizerRole(md *workercore.ModelDeployment) *workercore.ModelDeploymentRole {
	roles := slices.Clone(md.Spec.Roles)
	slices.SortFunc(roles, func(a, b workercore.ModelDeploymentRole) int {
		return strings.Compare(a.Name, b.Name)
	})
	for i := range roles {
		if ModelDeploymentEffectiveRoleKind(&roles[i]) == workercore.ModelDeploymentRoleKindPrefill {
			return &roles[i]
		}
	}

	// No prefill: a server does both halves of a request, so its tokenizer is the one a router
	// scoring whole requests should agree with. Admission does not require a routed deployment to
	// declare either kind, so the positional fallback stays -- but as the LAST resort, deterministic
	// rather than "whatever sorts first" by accident.
	for i := range roles {
		if ModelDeploymentEffectiveRoleKind(&roles[i]) == workercore.ModelDeploymentRoleKindServer {
			return &roles[i]
		}
	}

	return &roles[0]
}

func modelDeploymentRouterEndpoint(service *core.Service) string {
	return "http://" + service.Name + "." + service.Namespace + ".svc:" +
		fmt.Sprintf("%d", service.Spec.Ports[0].Port)
}

func (r *ModelDeploymentReconciler) syncModelDeploymentRouter(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	var rendered ModelDeploymentRouterObjects
	if md.Spec.Router != nil {
		var err error
		rendered, err = renderModelDeploymentRouterObjects(
			ctx, md, r.modelDeploymentRoleManufacturers(ctx, md))
		if err != nil {
			return err
		}
	}

	wanted := md.Spec.Router != nil
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.Deployment, wanted),
		func() ctrlcli.Object { return new(app.Deployment) }, new(app.DeploymentList),
		func(actual, expected ctrlcli.Object) bool {
			return alignModelDeploymentRouterDeployment(actual.(*app.Deployment), expected.(*app.Deployment))
		}, modelDeploymentResourceNoteRouter, "deployment"); err != nil {
		return err
	}
	// A router configured entirely by argv renders no ConfigMap, so the desired list is empty for it
	// and the prune path removes one a previous shape left behind. The typed nil is tested for
	// rather than the interface, which would be non-nil while holding it.
	if err := r.syncModelDeploymentOwnedChildren(ctx, md,
		objectIfPresent(rendered.ConfigMap, wanted && rendered.ConfigMap != nil),
		func() ctrlcli.Object { return new(core.ConfigMap) }, new(core.ConfigMapList),
		func(actual, expected ctrlcli.Object) bool {
			return alignModelDeploymentRouterConfigMap(actual.(*core.ConfigMap), expected.(*core.ConfigMap))
		}, modelDeploymentResourceNoteRouter, "configmap"); err != nil {
		return err
	}
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.Service, wanted),
		func() ctrlcli.Object { return new(core.Service) }, new(core.ServiceList),
		func(actual, expected ctrlcli.Object) bool {
			changed := alignModelDeploymentService(actual.(*core.Service), expected.(*core.Service))
			return alignModelDeploymentChildMetadata(actual, expected) || changed
		}, modelDeploymentResourceNoteRouter, "service"); err != nil {
		return err
	}
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.ServiceAccount, wanted),
		func() ctrlcli.Object { return new(core.ServiceAccount) }, new(core.ServiceAccountList),
		alignModelDeploymentChildMetadata, modelDeploymentResourceNoteRouter, "serviceaccount"); err != nil {
		return err
	}
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.Role, wanted),
		func() ctrlcli.Object { return new(rbac.Role) }, new(rbac.RoleList),
		func(actual, expected ctrlcli.Object) bool {
			role, want := actual.(*rbac.Role), expected.(*rbac.Role)
			changed := false
			if !kubemeta.DeepEqual(role.Rules, want.Rules) {
				role.Rules = want.Rules
				changed = true
			}
			return alignModelDeploymentChildMetadata(actual, expected) || changed
		}, modelDeploymentResourceNoteRouter, "role"); err != nil {
		return err
	}
	return r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.RoleBinding, wanted),
		func() ctrlcli.Object { return new(rbac.RoleBinding) }, new(rbac.RoleBindingList),
		func(actual, expected ctrlcli.Object) bool {
			binding, want := actual.(*rbac.RoleBinding), expected.(*rbac.RoleBinding)
			changed := false
			if !kubemeta.DeepEqual(binding.Subjects, want.Subjects) {
				binding.Subjects = want.Subjects
				changed = true
			}
			return alignModelDeploymentChildMetadata(actual, expected) || changed
		}, modelDeploymentResourceNoteRouter, "rolebinding")
}

func objectIfPresent(object ctrlcli.Object, present bool) []ctrlcli.Object {
	if !present {
		return nil
	}
	return []ctrlcli.Object{object}
}

// alignModelDeploymentRouterDeployment brings a live router Deployment up to what was rendered.
//
// IT DELIBERATELY DOES NOT COMPARE THE SELECTOR, which is immutable in apps/v1 and would make every
// later Update fail if it ever differed. The rendered selector embeds `spec.router.name`, so the
// only way to change it is to change that field -- and admission freezes it after creation. The
// value can differ between two deployments and never within one, which is what makes leaving the
// selector alone safe rather than merely untested.
//
// If that freeze is ever lifted, this function is where the breakage lands, and comparing the
// selector is NOT the fix: the object has to be deleted and recreated, because the API server will
// not accept the new selector on the existing one.
func alignModelDeploymentRouterDeployment(actual, expected *app.Deployment) (changed bool) {
	if !kubemeta.DeepEqual(actual.Spec.Replicas, expected.Spec.Replicas) {
		actual.Spec.Replicas = expected.Spec.Replicas
		changed = true
	}
	actualPod, expectedPod := &actual.Spec.Template, &expected.Spec.Template
	if !kubemeta.DeepEqual(actualPod.Labels, expectedPod.Labels) {
		actualPod.Labels = expectedPod.Labels
		changed = true
	}
	if !kubemeta.DeepEqual(actualPod.Annotations, expectedPod.Annotations) {
		actualPod.Annotations = expectedPod.Annotations
		changed = true
	}
	if actualPod.Spec.ServiceAccountName != expectedPod.Spec.ServiceAccountName {
		actualPod.Spec.ServiceAccountName = expectedPod.Spec.ServiceAccountName
		changed = true
	}
	// Compared although the pull policy below rides inside the containers: a secret added or
	// removed here changes what a pull can authenticate, and a field rendered but never aligned
	// is one an edit moves only by recreating the Deployment by hand.
	if !kubemeta.DeepEqual(actualPod.Spec.ImagePullSecrets, expectedPod.Spec.ImagePullSecrets) {
		actualPod.Spec.ImagePullSecrets = expectedPod.Spec.ImagePullSecrets
		changed = true
	}
	if !kubemeta.DeepEqual(actualPod.Spec.Volumes, expectedPod.Spec.Volumes) {
		actualPod.Spec.Volumes = expectedPod.Spec.Volumes
		changed = true
	}
	if len(actualPod.Spec.Containers) != len(expectedPod.Spec.Containers) {
		actualPod.Spec.Containers = expectedPod.Spec.Containers
		changed = true
	} else {
		for i := range expectedPod.Spec.Containers {
			if alignRenderedContainer(&actualPod.Spec.Containers[i], expectedPod.Spec.Containers[i]) {
				changed = true
			}
		}
	}

	return alignModelDeploymentChildMetadata(actual, expected) || changed
}

func alignModelDeploymentRouterConfigMap(actual, expected *core.ConfigMap) (changed bool) {
	if !maps.Equal(actual.Data, expected.Data) {
		actual.Data = expected.Data
		changed = true
	}
	return alignModelDeploymentChildMetadata(actual, expected) || changed
}

// alignModelDeploymentChildMetadata converges a child's labels and resource note onto the rendered
// ones. The note half is LOAD-BEARING rather than cosmetic: it is written at Create only, while the
// prune gate skips any child whose note is empty -- so a stripped note would make the object
// permanently exempt from pruning, and this is what keeps the gate honest.
func alignModelDeploymentChildMetadata(actual, expected ctrlcli.Object) (changed bool) {
	if !maps.Equal(actual.GetLabels(), expected.GetLabels()) {
		actual.SetLabels(maps.Clone(expected.GetLabels()))
		changed = true
	}
	if !systemmeta.EqualResourceTypeAndNotes(expected, actual) {
		systemmeta.SyncResourceTypeAndNotes(expected, actual)
		changed = true
	}
	return changed
}

func configMapEnv(key, name, configMap string) core.EnvVar {
	optional := false
	return core.EnvVar{Name: name, ValueFrom: &core.EnvVarSource{ConfigMapKeyRef: &core.ConfigMapKeySelector{
		LocalObjectReference: core.LocalObjectReference{Name: configMap}, Key: key, Optional: &optional,
	}}}
}

func joinInt32s(values []int32) string {
	parts := make([]string, len(values))
	for i := range values {
		parts[i] = fmt.Sprintf("%d", values[i])
	}
	return strings.Join(parts, ",")
}

func hashRouterConfig(data map[string]string) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "unhashable-" + err.Error()
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

const (
	// modelDeploymentRouterEnvoyTimeoutPlaceholder is substituted, not formatted. The template is
	// full of Envoy's own percent-delimited operators, so passing it through a format verb would
	// either mangle them or need every one of them escaped -- a second copy of the format language
	// living in Go's.
	modelDeploymentRouterEnvoyTimeoutPlaceholder = "<ROUTE-TIMEOUT>"

	// modelDeploymentRouterDefaultRouteTimeout is what the proxy keeps when the field is unset. A
	// day, which is the value this proxy has always carried; the two routers configured by argv
	// keep their own half hour, and the field is what makes the three agree.
	modelDeploymentRouterDefaultRouteTimeout int32 = 86400
)

// renderModelDeploymentRouterEnvoyConfig fills the one value the proxy template takes.
func renderModelDeploymentRouterEnvoyConfig(timeoutSeconds *int32) string {
	timeout := modelDeploymentRouterDefaultRouteTimeout
	if timeoutSeconds != nil {
		timeout = *timeoutSeconds
	}

	return strings.ReplaceAll(modelDeploymentRouterEnvoyConfig,
		modelDeploymentRouterEnvoyTimeoutPlaceholder, strconvx.Itoa(timeout))
}

const modelDeploymentRouterEnvoyConfig = `static_resources:
  listeners:
  - name: http
    address:
      socket_address: {address: 0.0.0.0, port_value: 8081}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: router
          route_config:
            name: router
            virtual_hosts:
            - name: model
              domains: ["*"]
              routes:
              - match: {prefix: "/"}
                route: {cluster: original_destination_cluster, timeout: <ROUTE-TIMEOUT>s}
          # ONE ACCESS LOG, AND IT IS NOT A FIELD. The gap it fills is that nothing is logged at
          # all, so there is no value a user would choose; what a reader needs is the endpoint the
          # picker selected beside the outcome, which is the pair no other log on this Pod carries.
          # The upstream host is that endpoint, and the response flags and code details are what
          # separate "the model refused" from "the proxy never reached one".
          access_log:
          - name: envoy.access_loggers.stdout
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
              log_format:
                json_format:
                  start_time: "%START_TIME%"
                  method: "%REQ(:METHOD)%"
                  path: "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%"
                  protocol: "%PROTOCOL%"
                  response_code: "%RESPONSE_CODE%"
                  response_flags: "%RESPONSE_FLAGS%"
                  response_code_details: "%RESPONSE_CODE_DETAILS%"
                  duration: "%DURATION%"
                  upstream_host: "%UPSTREAM_HOST%"
                  request_id: "%REQ(X-REQUEST-ID)%"
                  bytes_received: "%BYTES_RECEIVED%"
                  bytes_sent: "%BYTES_SENT%"
          http_filters:
          # The original-destination cluster routes on x-gateway-destination-endpoint, and
          # failure_mode_allow lets a request through when the EPP -- the only legitimate
          # writer of that header -- is down. A client-supplied copy must never reach the
          # cluster, or the caller picks the upstream instead of the endpoint selector.
          #
          # The removal is a filter placed ahead of ext_proc rather than a connection-manager
          # or route-level setting, and each of those is wrong for its own reason. The
          # connection manager HAS NO SUCH FIELD: request_headers_to_remove does not exist on
          # HttpConnectionManager in any Envoy release, so configuring it there is not a
          # late removal but a parse failure that stops the proxy from starting at all. The
          # route level does exist, and runs after ext_proc, which would strip the header the
          # EPP had just set. Filters run in order, so one placed here drops the client's copy
          # before the EPP is consulted and leaves the EPP's own copy alone.
          - name: envoy.filters.http.header_mutation
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.header_mutation.v3.HeaderMutation
              mutations:
                request_mutations:
                - remove: x-gateway-destination-endpoint
          - name: envoy.filters.http.ext_proc
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
              failure_mode_allow: true
              grpc_service:
                envoy_grpc: {cluster_name: ext_proc, authority: "localhost:9002"}
                timeout: 10s
              processing_mode:
                request_header_mode: SEND
                response_header_mode: SEND
                request_body_mode: FULL_DUPLEX_STREAMED
                response_body_mode: FULL_DUPLEX_STREAMED
                request_trailer_mode: SEND
                response_trailer_mode: SEND
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: original_destination_cluster
    type: ORIGINAL_DST
    connect_timeout: 1s
    lb_policy: CLUSTER_PROVIDED
    original_dst_lb_config:
      use_http_header: true
      http_header_name: x-gateway-destination-endpoint
  - name: ext_proc
    type: STATIC
    connect_timeout: 1s
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    load_assignment:
      cluster_name: ext_proc
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: {address: 127.0.0.1, port_value: 9002}
`
