package worker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	modelDeploymentRouterConfigKey                  = "epp.yaml"
	modelDeploymentRouterEnvoyConfigKey             = "envoy.yaml"
	modelDeploymentRouterSelectorKey                = "endpoint-selector"
	modelDeploymentRouterTargetPortsKey             = "endpoint-target-ports"
	modelDeploymentRouterExtraArgsKey               = "extra-args.json"
	modelDeploymentRouterConfigHash                 = "modeldeployment.gpustack.ai/router-config-hash"
	modelDeploymentRouterLabelKey                   = "modeldeployment.gpustack.ai/router"
	modelDeploymentRouterHTTPPort             int32 = 8000
	modelDeploymentRouterUpstreamLoopbackHost       = "127.0.0.1"
	modelDeploymentRouterUpstreamLoopbackPort int32 = 8001
	modelDeploymentRouterEPPPort              int32 = 9002
	// modelDeploymentRouterMetricsPort is where every router serves Prometheus metrics, so that one
	// scrape configuration reaches all of them. The llm-d picker listens here by default; the
	// routers configured by argv are told to, because their own default is elsewhere.
	modelDeploymentRouterMetricsPort        int32 = 9090
	modelDeploymentRouterObserverProxyPort  int32 = 9091
	modelDeploymentRouterEnvoyContainerName       = "envoy"
	modelDeploymentResourceNoteRouter             = "router"
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

// modelDeploymentRouterProtectedRole reports whether any of the deployment's roles is a managed
// protected group.
//
// THE ROUTER AND THE SERVICE MUST AGREE, or a request would reach an endpoint the Service does not
// front. A protected group names the eligible endpoints from the first enable, decided or not,
// because a surface without the term does not read the eligibility labels at all.
func modelDeploymentRouterProtectedRole(md *workercore.ModelDeployment) bool {
	for i := range md.Spec.Roles {
		if modelDeploymentProtectedRole(md, &md.Spec.Roles[i]) {
			return true
		}
	}

	return false
}

// modelDeploymentVLLMRouterKVConnector selects the --kv-connector handshake for vllm-router.
// Non-vLLM deployments return empty; deployments without a prefill/decode pair return "mooncake";
// Ascend pairs and NIXL or HIXL backends return "nixl"; all other vLLM pairs return "mooncake".
func modelDeploymentVLLMRouterKVConnector(md *workercore.ModelDeployment, manufacturers map[string]string) string {
	if md == nil || md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM {
		return ""
	}
	if !ModelDeploymentDeclaresBothHalves(md) {
		return "mooncake"
	}
	for i := range md.Spec.Roles {
		kind := ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i])
		if kind != workercore.ModelDeploymentRoleKindPrefill &&
			kind != workercore.ModelDeploymentRoleKindDecode {
			continue
		}
		if manufacturers[md.Spec.Roles[i].Name] == nodefeature.ManufacturerAscend {
			return "nixl"
		}
	}
	if md.Spec.KVTransfer != nil &&
		(md.Spec.KVTransfer.Backend == workercore.ModelDeploymentKVTransferBackendNIXL ||
			md.Spec.KVTransfer.Backend == workercore.ModelDeploymentKVTransferBackendHIXL) {
		return "nixl"
	}
	return "mooncake"
}

// modelDeploymentRouterRequiresAllowlistProxy reports whether argv-configured routers
// (vllm-router and sglang-gateway) require an Envoy inference-allowlist sidecar in front of
// their API listener.
func modelDeploymentRouterRequiresAllowlistProxy(md *workercore.ModelDeployment) bool {
	if md == nil {
		return false
	}
	for i := range md.Spec.Roles {
		if ModelDeploymentRoleSupportsEngineSentinel(md, &md.Spec.Roles[i]) {
			return true
		}
	}
	return false
}

// modelDeploymentRouterIsolationReady reports whether a deployment's managed router has converged
// onto the allowlist-enforcing revision before worker Pods activate runtime-control routes.
func modelDeploymentRouterIsolationReady(
	ctx context.Context,
	reader ctrlcli.Reader,
	md *workercore.ModelDeployment,
	routerObjects ModelDeploymentRouterObjects,
) (bool, string, error) {
	if md == nil || md.Spec.Router == nil || !modelDeploymentRouterRequiresAllowlistProxy(md) {
		return true, "", nil
	}
	if routerObjects.Deployment == nil {
		return false, "router deployment template is not ready", nil
	}
	wantHash := routerObjects.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash]
	if wantHash == "" {
		return false, "router allowlist configuration hash is empty", nil
	}

	if routerObjects.ConfigMap != nil {
		cm := new(core.ConfigMap)
		if cmErr := reader.Get(ctx, ctrlcli.ObjectKey{
			Namespace: md.Namespace,
			Name:      routerObjects.ConfigMap.Name,
		}, cm); cmErr != nil {
			if apierrors.IsNotFound(cmErr) {
				return false, fmt.Sprintf("waiting for router configmap %q to be created", routerObjects.ConfigMap.Name), nil
			}
			return false, "", cmErr
		}
		if !modelDeploymentOwns(cm, md) {
			return false, "", fmt.Errorf("configmap %s/%s is not owned by this deployment", cm.Namespace, cm.Name)
		}
		if hashRouterConfig(cm.Data) != wantHash {
			return false, fmt.Sprintf("waiting for router configmap %q to adopt allowlist config", cm.Name), nil
		}
	}

	dep := new(app.Deployment)
	err := reader.Get(ctx, ctrlcli.ObjectKey{
		Namespace: md.Namespace,
		Name:      routerObjects.Deployment.Name,
	}, dep)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("waiting for router deployment %q to be created", routerObjects.Deployment.Name), nil
		}
		return false, "", err
	}
	if !modelDeploymentOwns(dep, md) {
		return false, "", fmt.Errorf("router deployment %s/%s is not owned by this deployment", dep.Namespace, dep.Name)
	}
	if dep.Spec.Template.Annotations[modelDeploymentRouterConfigHash] != wantHash {
		return false, fmt.Sprintf("waiting for router deployment %q to adopt allowlist config", dep.Name), nil
	}
	hasEnvoy := false
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == modelDeploymentRouterEnvoyContainerName {
			hasEnvoy = true
			break
		}
	}
	if !hasEnvoy {
		return false, fmt.Sprintf("waiting for router deployment %q to include envoy sidecar", dep.Name), nil
	}

	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired <= 0 {
		return false, fmt.Sprintf("router deployment %q has zero desired replicas", dep.Name), nil
	}
	if dep.Status.ReadyReplicas <= 0 {
		return false, fmt.Sprintf(
			"waiting for router deployment %q rollout (observedGeneration=%d/%d, replicas=%d, updated=%d/%d, ready=%d/%d)",
			dep.Name,
			dep.Status.ObservedGeneration,
			dep.Generation,
			dep.Status.Replicas,
			dep.Status.UpdatedReplicas,
			desired,
			dep.Status.ReadyReplicas,
			desired,
		), nil
	}
	// Verify that every non-terminated router Pod (including terminating legacy Pods) enforces the
	// Envoy sidecar with the desired router config hash before trusting Deployment status counts.
	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		return false, fmt.Sprintf("router deployment %q has empty selector", dep.Name), nil
	}
	var routerPods core.PodList
	listOpts := []ctrlcli.ListOption{
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels(dep.Spec.Selector.MatchLabels),
	}
	if listErr := reader.List(ctx, &routerPods, listOpts...); listErr != nil {
		return false, "", listErr
	}
	liveRouterPods := 0
	readyEnvoyPods := 0
	allEnforceEnvoy := true
	for i := range routerPods.Items {
		p := &routerPods.Items[i]
		if p.Status.Phase == core.PodSucceeded || p.Status.Phase == core.PodFailed {
			continue
		}
		liveRouterPods++
		podHasEnvoy := false
		for j := range p.Spec.Containers {
			if p.Spec.Containers[j].Name == modelDeploymentRouterEnvoyContainerName {
				podHasEnvoy = true
				break
			}
		}
		if !podHasEnvoy || p.Annotations[modelDeploymentRouterConfigHash] != wantHash {
			allEnforceEnvoy = false
			break
		}
		if p.DeletionTimestamp == nil && podIsReady(p) {
			readyEnvoyPods++
		}
	}
	if liveRouterPods > 0 {
		if allEnforceEnvoy && readyEnvoyPods > 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf(
			"waiting for router deployment %q rollout (observedGeneration=%d/%d, replicas=%d, updated=%d/%d, ready=%d/%d)",
			dep.Name,
			dep.Status.ObservedGeneration,
			dep.Generation,
			dep.Status.Replicas,
			dep.Status.UpdatedReplicas,
			desired,
			dep.Status.ReadyReplicas,
			desired,
		), nil
	}
	// When no router Pod objects exist (for example in unit tests driving Deployment status
	// directly), fall back to Deployment status counts once every replica matches the latest revision.
	if dep.Status.ObservedGeneration == dep.Generation &&
		dep.Status.Replicas > 0 &&
		dep.Status.UpdatedReplicas == dep.Status.Replicas {
		return true, "", nil
	}
	return false, fmt.Sprintf(
		"waiting for router deployment %q rollout (observedGeneration=%d/%d, replicas=%d, updated=%d/%d, ready=%d/%d)",
		dep.Name,
		dep.Status.ObservedGeneration,
		dep.Generation,
		dep.Status.Replicas,
		dep.Status.UpdatedReplicas,
		desired,
		dep.Status.ReadyReplicas,
		desired,
	), nil
}

func renderModelDeploymentRouterObjects(
	ctx context.Context, md *workercore.ModelDeployment, manufacturers map[string]string,
	retainedEligibility bool,
) (ModelDeploymentRouterObjects, error) {
	return renderModelDeploymentRouterObjectsWithProxy(
		ctx, md, manufacturers, retainedEligibility, modelDeploymentRouterRequiresAllowlistProxy(md),
	)
}

func renderModelDeploymentRouterObjectsWithProxy(
	ctx context.Context, md *workercore.ModelDeployment, manufacturers map[string]string,
	retainedEligibility, requireAllowlistProxy bool,
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
	// The router that takes a static encoder list is told the per-role Service URL, not a replica
	// address: the Service fronts every replica, so a scale changes nothing the router was told
	// and does not restart it.
	var encoderURLs []string
	for i := range md.Spec.Roles {
		roleKind := ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i])
		kind, ok := ModelDeploymentRoleUpstreamKind(roleKind)
		if !ok {
			return ModelDeploymentRouterObjects{}, fmt.Errorf("unsupported role kind %q", md.Spec.Roles[i].Kind)
		}
		roles = append(roles, router.Role{
			Kind:         kind,
			RoleLabelKey: modelDeploymentLabelKeyRoleKind,
		})
		ports = append(ports, ModelDeploymentRoleServingPort(md, &md.Spec.Roles[i]))
		if roleKind == workercore.ModelDeploymentRoleKindEncode {
			encoderURLs = append(encoderURLs, modelDeploymentRoleEndpoint(md, &md.Spec.Roles[i]))
		}
	}
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if len(ports) != 1 {
		return ModelDeploymentRouterObjects{}, fmt.Errorf(
			"router requires every role to use the same serving port; got %v", ports,
		)
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
		engine, ok := ModelDeploymentEngineUpstreamName(md.Spec.Engine.Name)
		if !ok {
			return ModelDeploymentRouterObjects{}, fmt.Errorf("unsupported engine %q", md.Spec.Engine.Name)
		}
		if metrics, err = router.MetricsForEngine(engine); err != nil {
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
		// Keep legacy leader-only routing until the eligibility gate is active.
		// Once qualified, External-DP members all serve HTTP and must remain selectable.
		selector := modelDeploymentSelectorLabels(md, role)
		selector[modelDeploymentLabelKeyRoleKind] = roles[i].Kind
		selector[modelDeploymentMemberIndexLabel] = strconvx.Itoa(modelDeploymentLeaderMemberIndex)
		// Unsupported shapes never gain an eligibility selector that no pass can satisfy.
		if modelDeploymentEligibilitySelectorActive(md, role, nil, retainedEligibility) {
			selector[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
			if modelDeploymentRoleExternalDP(md, role) {
				delete(selector, modelDeploymentMemberIndexLabel)
			}
		}
		// The renderer is handed the SAME map that is published for Pod-watching routers. For
		// sgl-router, Kubernetes EndpointSlice discovery matches labels copied from the per-role
		// Service rather than Pod-only member-index or endpoint-eligible labels, while those Pod
		// terms narrow endpoints through Service.spec.selector.
		if md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL {
			sliceSelector := modelDeploymentSelectorLabels(md, role)
			sliceSelector[modelDeploymentLabelKeyRoleKind] = roles[i].Kind
			roles[i].Selector = sliceSelector
		} else {
			roles[i].Selector = maps.Clone(selector)
		}
		roleStatus := workercore.ModelDeploymentRouterRoleStatus{
			Name: role.Name, Kind: kind, Selector: selector, Endpoint: modelDeploymentRoleEndpoint(md, role),
		}
		if publish, topic, _ := modelDeploymentKVEventsConfig(md, role); publish {
			events := inject.KVEventsForTopic(md.Name+"-"+role.Name+"."+md.Namespace+".svc", topic)
			roleStatus.KVEvents = &workercore.ModelDeploymentRouterKVEvents{
				Endpoint: events.Endpoint, ReplayEndpoint: events.ReplayEndpoint, Topic: events.Topic,
			}
		}
		contract.Roles = append(contract.Roles, roleStatus)
	}
	// THE ROLES WHOSE ELIGIBILITY THE OPERATOR CONTROLS, AND THE ROLES IT CANNOT JUDGE. It is the
	// stable effective policy and not the pass's verdict: a role the operator can never ask about
	// carries no eligibility label and never will, and a role it can ask about may be qualified or
	// withdrawn. One selector is one AND of every term, so a deployment holding both cannot say
	// "required of this role and not of that one", and the candidate marker is what names them.
	controllable, unjudgeable := 0, 0
	for i := range md.Spec.Roles {
		if modelDeploymentRoleJudged(md, &md.Spec.Roles[i]) {
			controllable++
		} else {
			unjudgeable++
		}
	}
	// Start with legacy leader-only discovery. Qualified External-DP roles use the
	// eligibility label instead, which also excludes internal followers in mixed roles.
	// Derive every selector form from these equalities so all routers discover the same set.
	endpointLabels := map[string]string{
		modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance: md.Name,
		modelDeploymentMemberIndexLabel: strconvx.Itoa(modelDeploymentLeaderMemberIndex),
	}
	// Whether the deployment's shapes can ever activate the gate, and whether they all can. These
	// are two questions rather than one because a single AND answers only the first: what a role
	// that cannot activate must be spared is a requirement per role, which is what the published
	// selectors already are.
	endpointActivatable, everyActivatable := false, true
	for i := range md.Spec.Roles {
		if modelDeploymentGroupForwardActivatable(md, &md.Spec.Roles[i]) {
			endpointActivatable = true

			continue
		}
		everyActivatable = false
	}
	// THE PICKER NAMES EACH ROLE IN ITS OWN STAGE FILTER, so a deployment whose roles disagree
	// about activatability carries the requirement there and not here. One selector is one AND of
	// every term: it cannot say "required of the encoder and not of the prefiller", and a role
	// that can never activate carries no label for such a term to demand.
	//
	// SO IN THE ENCODER SHAPE IT NEVER CARRIES THE TERM AT ALL -- not decided, not retained. The
	// stage filter withdraws the encoder on its own, and a term here would only subtract the roles
	// the gate may not judge: an encoder outage would take text-only serving down with it. It is
	// also what makes the selector constant for the life of the deployment, so a fault costs a
	// stage a candidate rather than a restart of every router in the set.
	//
	// THE ENCODER IS WHAT NARROWS IT, and admission is why that is enough. A deployment declaring
	// one holds at most one role of each non-Server kind and may not pair an Encode role with a
	// Server role, so an encoder beside a role that cannot activate is exactly the prefill/decode
	// pair of one. A deployment of several Server roles can be mixed without ever declaring one,
	// and there the stage filters are shared by kind: they cannot hold two roles' requirements
	// apart, and the roles' membership rules still need this selector. Those deployments render as
	// they always did.
	stageFiltered := endpointActivatable && !everyActivatable &&
		md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD &&
		modelDeploymentDeclaresEncoder(md)
	// The eligibility equality rides beside the leader term for the same reason it rides on the
	// published selector: a member whose eligibility the reconciler has not written is not an
	// endpoint a request may reach, however much its peers need its record. A deployment in which
	// NO role's group-forward capability can ever activate renders WITHOUT it — a selector no
	// pass would ever satisfy would hand the router zero endpoints and call it discovery — while
	// a deployment whose roles all can keeps it, because once activation is possible the
	// observation states govern the per-pod label, not this selector.
	//
	// THE FIRST-ENABLE NARROWING REACHES THE DISCOVERY SELECTOR ONLY WHEN THE DEPLOYMENT'S ROLES
	// AGREE. One selector is one AND of every term, so a deployment whose roles disagree cannot hold
	// their requirements apart here, and narrowing it would withhold the roles that can be eligible.
	// Such a deployment renders as it always did.
	//
	// WHERE NO TERM IS RENDERED AND A ROLE REQUIRES ONE, THE CANDIDATE MARKER STANDS IN. It names
	// the members that may be discovered, which is the eligible members of the protected roles and
	// the members of the compatibility roles alike, and it is what a mixed union needs: the
	// eligibility term would withhold the roles that carry no requirement, and the marker withholds
	// the protected members that are not eligible, which is the whole point of the requirement.
	//
	// IT STANDS IN ONLY WHERE THE ELIGIBILITY TERM IS NOT RENDERED AT ALL. A deployment that renders
	// it deployment-wide already withholds the roles that carry no requirement, and that is today's
	// behavior and stays it.
	markerBacked := false
	switch {
	case controllable > 0 && unjudgeable > 0:
		// THE MIXED UNION. One role's eligibility the operator controls and one role it cannot judge
		// stand in one deployment, so the eligibility term cannot name them: it would withhold the
		// role that carries no requirement, and it would admit the members of the role it does that
		// were withdrawn.
		//
		// THE CANDIDATE MARKER NAMES THE MEMBERS THAT MAY BE DISCOVERED, which is the eligible
		// members of the roles the operator judges and the members of the role it cannot alike.
		//
		endpointLabels[modelDeploymentRouterCandidateLabel] = modelDeploymentEndpointEligibleValue
		// THE MEMBER INDEX IS DROPPED, and this is the shape's own rule and not a narrowing of
		// it: the member index is what selects one member of a replica, and a replica whose
		// members all answer must be discovered whole. The marker says which members may be
		// discovered, so the index has nothing left to say.
		delete(endpointLabels, modelDeploymentMemberIndexLabel)
		markerBacked = true
	case (endpointActivatable && !stageFiltered &&
		(modelDeploymentEligibilityDecided(md) || retainedEligibility ||
			(everyActivatable && (modelDeploymentRouterProtectedRole(md) ||
				modelDeploymentAnyRoleDeclaresPauseOrRuntimeControl(md) ||
				modelDeploymentAnyRoleHasObservedRuntimeControl(md))))) ||
		(unjudgeable == 0 && modelDeploymentAnyRolePaused(md)):
		endpointLabels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		for i := range md.Spec.Roles {
			if modelDeploymentRoleExternalDP(md, &md.Spec.Roles[i]) {
				// Eligibility selects every answering External-DP member and only the
				// leaders of internal roles, including deployments with both shapes.
				delete(endpointLabels, modelDeploymentMemberIndexLabel)
				break
			}
		}
	}
	// The router Pod has neither a member index nor endpoint eligibility.
	// Keep the explicit exclusion in the expression as well.
	//
	// The terms are listed rather than sorted out of the map because their ORDER IS RENDERED: this
	// string is a ConfigMap value and the Pod's configuration hash is taken over it, so reordering
	// it would roll every router in the cluster for a change nothing reads.
	endpointSelectorTerms := []string{
		modelDeploymentLabelKeyName + "=" + endpointLabels[modelDeploymentLabelKeyName],
		modelDeploymentLabelKeyInstance + "=" + endpointLabels[modelDeploymentLabelKeyInstance],
	}
	if member, present := endpointLabels[modelDeploymentMemberIndexLabel]; present {
		endpointSelectorTerms = append(endpointSelectorTerms, modelDeploymentMemberIndexLabel+"="+member)
	}
	if len(endpointLabels[modelDeploymentLabelKeyEndpointEligible]) > 0 {
		endpointSelectorTerms = append(endpointSelectorTerms,
			modelDeploymentLabelKeyEndpointEligible+"="+endpointLabels[modelDeploymentLabelKeyEndpointEligible])
	}
	if len(endpointLabels[modelDeploymentRouterCandidateLabel]) > 0 {
		endpointSelectorTerms = append(endpointSelectorTerms,
			modelDeploymentRouterCandidateLabel+"="+endpointLabels[modelDeploymentRouterCandidateLabel])
	}
	endpointSelectorTerms = append(endpointSelectorTerms, "!"+modelDeploymentRouterLabelKey)
	// An engine that reaches its encoder by address has its prefiller dial the encoder itself, so
	// the router never sends it a request. The encoder is kept out of the candidate set rather than
	// left in it as an endpoint no stage selects. The term is appended last, and only here, so a
	// selector of any other deployment renders as it did.
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD &&
		modelDeploymentEPDSplitsEncoderByAddress(md.Spec.Engine.Name) &&
		modelDeploymentDeclaresEncoder(md) {
		if encodeKind, ok := ModelDeploymentRoleUpstreamKind(workercore.ModelDeploymentRoleKindEncode); ok {
			endpointSelectorTerms = append(endpointSelectorTerms,
				modelDeploymentLabelKeyRoleKind+"!="+encodeKind)
		}
	}
	endpointSelector := strings.Join(endpointSelectorTerms, ",")
	listenHost := ""
	listenPort := modelDeploymentRouterHTTPPort
	if requireAllowlistProxy &&
		(md.Spec.Router.Name == workercore.ModelDeploymentRouterVLLM ||
			md.Spec.Router.Name == workercore.ModelDeploymentRouterSGLang ||
			md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL) {
		listenHost = modelDeploymentRouterUpstreamLoopbackHost
		listenPort = modelDeploymentRouterUpstreamLoopbackPort
	}
	routerEndpointLabels := endpointLabels
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL {
		routerEndpointLabels = map[string]string{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		}
		for i := range roles {
			if roles[i].Kind != "encode" {
				routerEndpointLabels[modelDeploymentLabelKeyRoleKind] = roles[i].Kind
				break
			}
		}
	}
	routerInput := router.Input{
		Roles: roles, Metrics: metrics, ModelName: md.Spec.Model.Name,
		TokenizerEndpoint: modelDeploymentRoleEndpoint(md, modelDeploymentTokenizerRole(md)),
		Namespace:         md.Namespace, EndpointSelector: endpointSelector,
		EndpointLabels:          routerEndpointLabels,
		EncoderURLs:             encoderURLs,
		ListenHost:              listenHost,
		ListenPort:              listenPort,
		MetricsPort:             modelDeploymentRouterMetricsPort,
		DiscoveryPort:           ports[0],
		RequestTimeoutSeconds:   md.Spec.Router.RequestTimeoutSeconds,
		DisaggregationThreshold: md.Spec.Router.DisaggregationThresholdTokens,
		// The handshake follows the transfer backend and the pool hardware the pair runs,
		// resolved in one place so the router and the engine halves cannot disagree.
		KVConnector: modelDeploymentVLLMRouterKVConnector(md, manufacturers),
		ExtraArgs:   md.Spec.Router.ExtraArgs,
	}
	// THE SGLANG EXTERNAL DP SHAPE NAMES ITS WIDTH TO THE GATEWAY. The gateway's --dp-aware fans
	// its worker registration out per dp member, which is the one-worker-per-member contract the
	// shape is held to; a width under two renders nothing, and so does every other engine. The
	// first external role the declaration carries decides the width: a deployment declaring more
	// than one such role is outside the validated shapes, and the later roles are not read.
	if md.Spec.Engine.Name == workercore.ModelDeploymentEngineSGLang {
		for i := range md.Spec.Roles {
			if !modelDeploymentRoleExternalDP(md, &md.Spec.Roles[i]) {
				continue
			}
			reading, err := scanModelDeploymentParallelism(
				md.Spec.Engine.Name, md.Spec.Roles[i].ExtraArgs, md.Spec.Roles[i].Env,
			)
			if err != nil {
				return ModelDeploymentRouterObjects{}, fmt.Errorf(
					"read the external-dp width the router renders: %w", err,
				)
			}
			routerInput.AttentionDataParallelSize = int32(reading.declared.AttentionDataParallel)
			break
		}
	}
	if stageFiltered {
		routerInput.EligibleLabelKey = modelDeploymentLabelKeyEndpointEligible
	}
	if markerBacked {
		// THE MARKER-BACKED DISCOVERY NAMES THE CANDIDATES OF EVERY KIND, and a per-kind filter
		// that demanded one role's eligibility would withhold the compatibility candidates of that
		// kind. The candidate term is the one every candidate of the kind carries, so the filter
		// admits the kind's safe union without choosing a role.
		routerInput.CandidateLabelKey = modelDeploymentRouterCandidateLabel
		routerInput.CandidateLabelValue = modelDeploymentEndpointEligibleValue
	}
	if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM {
		engine, _ := ModelDeploymentEngineUpstreamName(md.Spec.Engine.Name)
		routerInput.KVEvents = router.KVEvents{
			Engine: engine, Port: inject.VLLMKVEventsPort,
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
		Name:                  name,
		Rendered:              rendered,
		EndpointSelector:      endpointSelector,
		TargetPorts:           joinInt32s(ports),
		ExtraArgs:             string(extraArgs),
		RequireAllowlistProxy: requireAllowlistProxy,
	})
	if err != nil {
		return ModelDeploymentRouterObjects{}, err
	}

	// NO CONFIGURATION MEANS NO OBJECT. A router configured entirely by argv reads nothing from a
	// ConfigMap, and rendering an empty one would leave an object whose absence and whose emptiness
	// a reader cannot tell apart, plus a config hash taken over nothing.
	var configMap *core.ConfigMap
	metricsPort := modelDeploymentRouterMetricsPort
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL && !requireAllowlistProxy {
		metricsPort = modelDeploymentRouterHTTPPort
	}
	annotations := modelDeploymentMetricsAnnotations(metricsPort, core.URISchemeHTTP)
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
	roleRules := []rbac.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"pods"},
		Verbs:     []string{"get", "list", "watch"},
	}}
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL {
		roleRules = []rbac.PolicyRule{{
			APIGroups: []string{"discovery.k8s.io"},
			Resources: []string{"endpointslices"},
			Verbs:     []string{"get", "list", "watch"},
		}}
	}
	role := &rbac.Role{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Rules:      roleRules,
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

	// RequireAllowlistProxy fronts argv-configured routers with the shared Envoy inference
	// allowlist sidecar while binding the router process to loopback.
	RequireAllowlistProxy bool
}

func renderModelDeploymentRouterWorkload(
	ctx context.Context, md *workercore.ModelDeployment, in modelDeploymentRouterDerived,
) (modelDeploymentRouterWorkload, error) {
	switch md.Spec.Router.Name {
	case workercore.ModelDeploymentRouterLLMD:
		return renderModelDeploymentLLMDRouterWorkload(ctx, md, in), nil
	case workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentRouterSGLang, workercore.ModelDeploymentRouterSGL:
		return renderModelDeploymentArgvRouterWorkload(ctx, md, in), nil
	default:
		return modelDeploymentRouterWorkload{},
			fmt.Errorf("router %q renders no workload", md.Spec.Router.Name)
	}
}

// renderModelDeploymentArgvRouterWorkload renders a router configured by its command line.
// When RequireAllowlistProxy is false, it renders a single container on port 8000 with no
// ConfigMap. When RequireAllowlistProxy is true, it fronts the loopback router process on port
// 8001 with an Envoy sidecar on port 8000 enforcing the shared inference allowlist.
func renderModelDeploymentArgvRouterWorkload(
	ctx context.Context, md *workercore.ModelDeployment, in modelDeploymentRouterDerived,
) modelDeploymentRouterWorkload {
	image := md.Spec.Router.Image
	if image == "" {
		image = redirectedImage(ctx, settings.ModelDeploymentRouterImage.ShouldValue(ctx))
	}

	// The user's arguments land AFTER the derived ones, which is the order the refusal in admission
	// is written against: a key the operator owns is refused there, so anything surviving to here
	// names something the operator did not set.
	args := slices.Concat(in.Rendered.Arguments, md.Spec.Router.ExtraArgs)
	isSGLRouter := md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL

	if !in.RequireAllowlistProxy {
		ports := []core.ContainerPort{{Name: "http", ContainerPort: modelDeploymentRouterHTTPPort}}
		if !isSGLRouter {
			ports = append(ports, core.ContainerPort{Name: "metrics", ContainerPort: modelDeploymentRouterMetricsPort})
		}
		return modelDeploymentRouterWorkload{
			Containers: []core.Container{{
				// The command comes from the render beside the arguments, because the image holds three
				// programs and declares no entrypoint. Arguments without it are handed to the image's
				// own CMD position, where the first flag is read as the program name.
				Name: "router", Image: image, Command: in.Rendered.Command, Args: args,
				ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, image),
				Ports:           ports,
				// The router answers on the port the Service targets, so a TCP probe on it is the same
				// question the Service asks. There is no second process to gate on, unlike the picker
				// whose readiness is its own gRPC health server behind a proxy.
				ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
					TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(modelDeploymentRouterHTTPPort)},
				}},
			}},
		}
	}

	envoyConfig := renderModelDeploymentRouterLoopbackEnvoyConfig(md.Spec.Router.RequestTimeoutSeconds)
	envoyContainer := renderModelDeploymentRouterEnvoyContainer(ctx, md)
	routerPorts := []core.ContainerPort{
		{Name: "http-upstream", ContainerPort: modelDeploymentRouterUpstreamLoopbackPort},
		{Name: "metrics", ContainerPort: modelDeploymentRouterMetricsPort},
	}
	if isSGLRouter {
		envoyConfig = renderModelDeploymentRouterSGLLoopbackEnvoyConfig(md.Spec.Router.RequestTimeoutSeconds)
		envoyContainer.Ports = append(envoyContainer.Ports,
			core.ContainerPort{Name: "metrics", ContainerPort: modelDeploymentRouterMetricsPort},
		)
		routerPorts = []core.ContainerPort{
			{Name: "http-upstream", ContainerPort: modelDeploymentRouterUpstreamLoopbackPort},
		}
	}

	return modelDeploymentRouterWorkload{
		Config: map[string]string{
			modelDeploymentRouterEnvoyConfigKey: envoyConfig,
		},
		Containers: []core.Container{
			envoyContainer,
			{
				Name: "router", Image: image, Command: in.Rendered.Command, Args: args,
				ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, image),
				Ports:           routerPorts,
				// The router binds 127.0.0.1:8001 behind the Envoy allowlist sidecar, so a bare TCP probe on
				// 8001 dials <PodIP>:8001 from kubelet and always fails with connection refused. Probe
				// GET /observer/endpoints through Envoy's :9091 observer listener, which forwards straight to
				// 127.0.0.1:8001 and returns 503 until the loopback router serves HTTP (and 200 even before
				// any worker endpoints register).
				ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
					HTTPGet: &core.HTTPGetAction{
						Path: observerURLPath,
						Port: intstr.FromInt32(modelDeploymentRouterObserverProxyPort),
					},
				}},
			},
		},
		Volumes: []core.Volume{{Name: "config", VolumeSource: core.VolumeSource{
			ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: in.Name}},
		}}},
	}
}

func renderModelDeploymentRouterEnvoyContainer(
	ctx context.Context, md *workercore.ModelDeployment,
) core.Container {
	proxyImage := redirectedImage(ctx,
		settings.ModelDeploymentRouterProxyImage.ShouldValue(ctx))

	return core.Container{
		Name: modelDeploymentRouterEnvoyContainerName, Image: proxyImage,
		// Resolved rather than passed through so an empty policy does not drift against
		// API server defaulting on subsequent reconciles.
		ImagePullPolicy: kvcache.ResolvePullPolicy(md.Spec.Router.ImagePullPolicy, proxyImage),
		Args:            []string{"-c", "/config/" + modelDeploymentRouterEnvoyConfigKey},
		Ports:           []core.ContainerPort{{Name: "http", ContainerPort: modelDeploymentRouterHTTPPort}},
		ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
			TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(modelDeploymentRouterHTTPPort)},
		}},
		VolumeMounts: []core.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
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
				md.Spec.Router.RequestTimeoutSeconds,
			),
			modelDeploymentRouterSelectorKey:    in.EndpointSelector,
			modelDeploymentRouterTargetPortsKey: in.TargetPorts,
			modelDeploymentRouterExtraArgsKey:   in.ExtraArgs,
		},
		Containers: []core.Container{
			renderModelDeploymentRouterEnvoyContainer(ctx, md),
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
	ctx context.Context, md *workercore.ModelDeployment, eligibilityDecided bool,
) error {
	var rendered ModelDeploymentRouterObjects
	if md.Spec.Router != nil {
		// RETENTION: the discovery equality, once published, survives a quiet pass on EVERY
		// shipped profile. The llm-d profile publishes the ConfigMap; the vllm-router and
		// sglang-gateway profiles configure by argv and render none, so absence of a ConfigMap is
		// not proof a surface was never narrowed -- the live router Deployment's args and env are
		// read too. The static activatability half still governs: a deployment whose shapes
		// stopped being activatable loses the term on this pass.
		//
		// IT IS READ FROM THE DOCUMENT AS WELL AS THE SELECTOR KEY, because the two are two places
		// one requirement lives. Where a deployment's roles disagree about activatability the
		// requirement sits in a stage filter rather than the selector, and a pass that read only
		// the selector would find nothing, decide the gate had never been active, and put back the
		// endpoints the fault withdrew. The document is PARSED rather than searched: a model name
		// the user chose can be the very string of this label, and reading that as a demand would
		// activate a gate on a deployment that never decided one.
		needle := modelDeploymentLabelKeyEndpointEligible + "=" + modelDeploymentEndpointEligibleValue
		// THE MARKER IS THE OTHER NAME THE REQUIREMENT TRAVELS UNDER, and a deployment whose roles
		// disagree carries it instead of the eligibility term. A quiet pass must find the
		// requirement wherever it was published, or it would conclude the gate had never fired and
		// put back the endpoints the fault withdrew.
		candidateNeedle := modelDeploymentRouterCandidateLabel + "=" + modelDeploymentEndpointEligibleValue
		retained := false
		liveConfig := new(core.ConfigMap)
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, liveConfig)
		switch {
		case err == nil:
			retained = strings.Contains(liveConfig.Data[modelDeploymentRouterSelectorKey], needle) ||
				strings.Contains(liveConfig.Data[modelDeploymentRouterSelectorKey], candidateNeedle) ||
				router.DocumentStageFilterDemands(
					liveConfig.Data[modelDeploymentRouterConfigKey],
					modelDeploymentLabelKeyEndpointEligible, modelDeploymentEndpointEligibleValue,
				) ||
				router.DocumentStageFilterDemands(
					liveConfig.Data[modelDeploymentRouterConfigKey],
					modelDeploymentRouterCandidateLabel, modelDeploymentEndpointEligibleValue,
				)
		case apierrors.IsNotFound(err):
		default:
			return err
		}
		if !retained {
			liveDeployment := new(app.Deployment)
			err = r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, liveDeployment)
			switch {
			case err == nil:
				for _, container := range liveDeployment.Spec.Template.Spec.Containers {
					for _, arg := range container.Args {
						if strings.Contains(arg, needle) || strings.Contains(arg, candidateNeedle) {
							retained = true
						}
					}
					for _, env := range container.Env {
						if strings.Contains(env.Value, needle) || strings.Contains(env.Value, candidateNeedle) {
							retained = true
						}
					}
				}
			case apierrors.IsNotFound(err):
			default:
				return err
			}
		}
		if !retained && md.Spec.Router.Name == workercore.ModelDeploymentRouterSGL {
			for i := range md.Spec.Roles {
				svc := new(core.Service)
				svcErr := r.Client.Get(ctx, ctrlcli.ObjectKey{
					Namespace: md.Namespace,
					Name:      md.Name + "-" + md.Spec.Roles[i].Name,
				}, svc)
				switch {
				case svcErr == nil:
					if svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue ||
						svc.Annotations[modelDeploymentEligibilityActiveAnnotation] == modelDeploymentEndpointEligibleValue {
						retained = true
					}
				case apierrors.IsNotFound(svcErr):
				default:
					return svcErr
				}
				if retained {
					break
				}
			}
		}

		rendered, err = renderModelDeploymentRouterObjects(
			ctx, md, r.modelDeploymentRoleManufacturers(ctx, md), retained || eligibilityDecided,
		)
		if err != nil {
			return err
		}
	}

	wanted := md.Spec.Router != nil
	// Sync ConfigMap before Deployment so any newly scheduled router Pod boots against the
	// current allowlist policy rather than a stale ConfigMap revision.
	if err := r.syncModelDeploymentOwnedChildren(ctx, md,
		objectIfPresent(rendered.ConfigMap, wanted && rendered.ConfigMap != nil),
		func() ctrlcli.Object { return new(core.ConfigMap) }, new(core.ConfigMapList),
		func(actual, expected ctrlcli.Object) bool {
			return alignModelDeploymentRouterConfigMap(actual.(*core.ConfigMap), expected.(*core.ConfigMap))
		}, modelDeploymentResourceNoteRouter, "configmap"); err != nil {
		return err
	}
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.Deployment, wanted),
		func() ctrlcli.Object { return new(app.Deployment) }, new(app.DeploymentList),
		func(actual, expected ctrlcli.Object) bool {
			return alignModelDeploymentRouterDeployment(actual.(*app.Deployment), expected.(*app.Deployment))
		}, modelDeploymentResourceNoteRouter, "deployment"); err != nil {
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
	if expected.Spec.Strategy.Type != "" && actual.Spec.Strategy.Type != expected.Spec.Strategy.Type {
		actual.Spec.Strategy = expected.Spec.Strategy
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
	return stringx.SumBytesBySHA256(encoded)
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

	modelDeploymentRouterEnvoyOriginalDstCluster = "original_destination_cluster"
	modelDeploymentRouterEnvoyLoopbackCluster    = "router_upstream"
)

type modelDeploymentRouterEnvoyRoute struct {
	Method string
	Path   string
	Prefix bool
}

// modelDeploymentRouterInferenceRoutes is the single allowlist shared by all Envoy-fronted
// router profiles. Any request not matching an entry falls through to a 403 direct_response.
var modelDeploymentRouterInferenceRoutes = []modelDeploymentRouterEnvoyRoute{
	{Method: "POST", Path: "/v1/chat/completions"},
	{Method: "POST", Path: "/v1/completions"},
	{Method: "POST", Path: "/v1/embeddings"},
	{Method: "POST", Path: "/v1/responses"},
	{Method: "POST", Path: "/v1/rerank"},
	{Method: "POST", Path: "/v1/score"},
	{Method: "POST", Path: "/v1/classify"},
	{Method: "POST", Path: "/v1/audio/transcriptions"},
	{Method: "POST", Path: "/v1/messages"},
	{Method: "POST", Path: "/v1/messages/count_tokens"},
	{Method: "GET", Path: "/v1/models"},
	{Method: "GET", Path: "/v1/models/", Prefix: true},
	{Method: "POST", Path: "/tokenize"},
	{Method: "POST", Path: "/v1/tokenize"},
	{Method: "POST", Path: "/detokenize"},
	{Method: "POST", Path: "/v1/detokenize"},
	{Method: "POST", Path: "/v1/chat/completions/render"},
	{Method: "GET", Path: "/health"},
	// Multimodal inference routes for vllm-omni and sglang-omni compatibility.
	{Method: "POST", Path: "/v1/audio/speech"},
	{Method: "POST", Path: "/v1/images/generations"},
	{Method: "POST", Path: "/v1/images/edits"},
	{Method: "POST", Path: "/v1/videos"},
	{Method: "GET", Path: "/v1/videos/", Prefix: true},
}

// renderModelDeploymentRouterEnvoyConfig renders the Envoy configuration for llm-d-router,
// enforcing the shared inference allowlist ahead of ext_proc and original_destination_cluster.
func renderModelDeploymentRouterEnvoyConfig(timeoutSeconds *int32) string {
	return buildModelDeploymentRouterEnvoyConfig(
		modelDeploymentRouterEnvoyOriginalDstCluster,
		timeoutSeconds,
		true,
		false,
		false,
		modelDeploymentRouterEnvoyLLMDHTTPFilters,
		modelDeploymentRouterEnvoyLLMDClusters,
	)
}

// renderModelDeploymentRouterLoopbackEnvoyConfig renders the Envoy sidecar configuration that
// fronts vllm-router and sglang-gateway on 0.0.0.0:8000 and forwards only the shared inference
// allowlist to the loopback router listener on 127.0.0.1:8001, plus an unexposed Pod-internal
// observer listener on 0.0.0.0:9091 forwarding only GET /observer/endpoints.
func renderModelDeploymentRouterLoopbackEnvoyConfig(timeoutSeconds *int32) string {
	return buildModelDeploymentRouterEnvoyConfig(
		modelDeploymentRouterEnvoyLoopbackCluster,
		timeoutSeconds,
		false,
		true,
		false,
		modelDeploymentRouterEnvoyLoopbackHTTPFilters,
		modelDeploymentRouterEnvoyLoopbackClusters,
	)
}

// renderModelDeploymentRouterSGLLoopbackEnvoyConfig renders the Envoy sidecar configuration that
// fronts sgl-router on 0.0.0.0:8000, exposes GET /observer/endpoints on 0.0.0.0:9091, and exposes
// GET /metrics on 0.0.0.0:9090 because sgl-router serves Prometheus metrics on its main HTTP server.
func renderModelDeploymentRouterSGLLoopbackEnvoyConfig(timeoutSeconds *int32) string {
	return buildModelDeploymentRouterEnvoyConfig(
		modelDeploymentRouterEnvoyLoopbackCluster,
		timeoutSeconds,
		false,
		true,
		true,
		modelDeploymentRouterEnvoyLoopbackHTTPFilters,
		modelDeploymentRouterEnvoyLoopbackClusters,
	)
}

func buildModelDeploymentRouterEnvoyConfig(
	cluster string, timeoutSeconds *int32, disableExtProcOnDeny, includeObserverListener, includeMetricsListener bool, httpFilters, clusters string,
) string {
	timeout := modelDeploymentRouterDefaultRouteTimeout
	if timeoutSeconds != nil {
		timeout = *timeoutSeconds
	}

	var b strings.Builder
	b.WriteString(modelDeploymentRouterEnvoyConfigPrefix)
	b.WriteString(renderModelDeploymentRouterEnvoyRoutes(cluster, disableExtProcOnDeny))
	b.WriteString(modelDeploymentRouterEnvoyAccessLog)
	b.WriteString("          http_filters:\n")
	b.WriteString(httpFilters)
	if includeObserverListener {
		b.WriteString(strings.ReplaceAll(
			modelDeploymentRouterEnvoyObserverListener,
			"__OBSERVER_PROXY_PORT__",
			strconvx.Itoa(modelDeploymentRouterObserverProxyPort),
		))
	}
	if includeMetricsListener {
		b.WriteString(strings.ReplaceAll(
			modelDeploymentRouterEnvoyMetricsListener,
			"__METRICS_PROXY_PORT__",
			strconvx.Itoa(modelDeploymentRouterMetricsPort),
		))
	}
	b.WriteString("  clusters:\n")
	b.WriteString(clusters)

	return strings.ReplaceAll(b.String(),
		modelDeploymentRouterEnvoyTimeoutPlaceholder, strconvx.Itoa(timeout))
}

func renderModelDeploymentRouterEnvoyRoutes(cluster string, disableExtProcOnDeny bool) string {
	var b strings.Builder
	for _, route := range modelDeploymentRouterInferenceRoutes {
		b.WriteString("              - match:\n")
		if route.Prefix {
			fmt.Fprintf(&b, "                  prefix: %q\n", route.Path)
		} else {
			fmt.Fprintf(&b, "                  path: %q\n", route.Path)
		}
		b.WriteString("                  headers:\n")
		b.WriteString("                  - name: \":method\"\n")
		fmt.Fprintf(&b, "                    string_match: {exact: %q}\n", route.Method)
		fmt.Fprintf(&b, "                route: {cluster: %s, timeout: %ss}\n",
			cluster, modelDeploymentRouterEnvoyTimeoutPlaceholder)
	}
	b.WriteString("              - match: {prefix: \"/\"}\n")
	b.WriteString("                direct_response: {status: 403}\n")
	if disableExtProcOnDeny {
		b.WriteString("                typed_per_filter_config:\n")
		b.WriteString("                  envoy.filters.http.ext_proc:\n")
		b.WriteString("                    \"@type\": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute\n")
		b.WriteString("                    disabled: true\n")
	}

	return b.String()
}

const modelDeploymentRouterEnvoyConfigPrefix = `static_resources:
  listeners:
  - name: http
    address:
      socket_address: {address: 0.0.0.0, port_value: 8000}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: router
          normalize_path: true
          merge_slashes: true
          path_with_escaped_slashes_action: REJECT_REQUEST
          route_config:
            name: router
            virtual_hosts:
            - name: model
              domains: ["*"]
              routes:
`

const modelDeploymentRouterEnvoyAccessLog = `          # ONE ACCESS LOG, AND IT IS NOT A FIELD. The gap it fills is that nothing is logged at
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
`

const modelDeploymentRouterEnvoyLLMDHTTPFilters = `          # The original-destination cluster routes on x-gateway-destination-endpoint, and
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
`

const modelDeploymentRouterEnvoyLLMDClusters = `  - name: original_destination_cluster
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

const modelDeploymentRouterEnvoyLoopbackHTTPFilters = `          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`

// modelDeploymentRouterEnvoyObserverListener binds 0.0.0.0:__OBSERVER_PROXY_PORT__ inside the
// router Pod for kubelet readiness probes and cluster-internal controller observation (not exposed
// on the router Service).
const modelDeploymentRouterEnvoyObserverListener = `  - name: observer
    address:
      socket_address: {address: 0.0.0.0, port_value: __OBSERVER_PROXY_PORT__}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: router_observer
          normalize_path: true
          merge_slashes: true
          path_with_escaped_slashes_action: REJECT_REQUEST
          route_config:
            name: router_observer
            virtual_hosts:
            - name: observer
              domains: ["*"]
              routes:
              - match:
                  path: "/observer/endpoints"
                  headers:
                  - name: ":method"
                    string_match: {exact: "GET"}
                route: {cluster: router_upstream, timeout: 5s}
              - match: {prefix: "/"}
                direct_response: {status: 403}
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`

// modelDeploymentRouterEnvoyMetricsListener binds 0.0.0.0:__METRICS_PROXY_PORT__ inside the
// sgl-router Pod so Prometheus scrapes GET /metrics on :9090 without exposing /metrics on the
// client-facing :8000 listener.
const modelDeploymentRouterEnvoyMetricsListener = `  - name: metrics
    address:
      socket_address: {address: 0.0.0.0, port_value: __METRICS_PROXY_PORT__}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: router_metrics
          normalize_path: true
          merge_slashes: true
          path_with_escaped_slashes_action: REJECT_REQUEST
          route_config:
            name: router_metrics
            virtual_hosts:
            - name: metrics
              domains: ["*"]
              routes:
              - match:
                  path: "/metrics"
                  headers:
                  - name: ":method"
                    string_match: {exact: "GET"}
                route: {cluster: router_upstream, timeout: 5s}
              - match: {prefix: "/"}
                direct_response: {status: 403}
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`

const modelDeploymentRouterEnvoyLoopbackClusters = `  - name: router_upstream
    type: STATIC
    connect_timeout: 1s
    lb_policy: ROUND_ROBIN
    load_assignment:
      cluster_name: router_upstream
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: {address: 127.0.0.1, port_value: 8001}
`
