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
	modelDeploymentResourceNoteRouter         = "router"
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
	ctx context.Context, md *workercore.ModelDeployment,
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
		ports = append(ports, modelDeploymentServicePort(&md.Spec.Roles[i]).ContainerPort)
	}
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if len(ports) != 1 {
		return ModelDeploymentRouterObjects{}, fmt.Errorf(
			"router requires every role to use the same serving port; got %v", ports)
	}

	metrics, err := router.MetricsForEngine(md.Spec.Engine)
	if err != nil {
		return ModelDeploymentRouterObjects{}, err
	}
	metrics.Port = ports[0]
	contract := workercore.ModelDeploymentRouterStatus{
		Name: md.Spec.Router.Name,
		Metrics: &workercore.ModelDeploymentRouterMetrics{
			Port: metrics.Port, QueuedRequests: metrics.QueuedRequests,
			RunningRequests: metrics.RunningRequests, KVCacheUtilization: metrics.KVCacheUtilization,
		},
		Roles: make([]workercore.ModelDeploymentRouterRoleStatus, 0, len(md.Spec.Roles)),
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		kind := ModelDeploymentEffectiveRoleKind(role)
		selector := modelDeploymentSelectorLabels(md, role)
		selector[modelDeploymentLabelKeyRoleKind] = string(kind)
		roleStatus := workercore.ModelDeploymentRouterRoleStatus{
			Name: role.Name, Kind: kind, Selector: selector, Endpoint: modelDeploymentRoleEndpoint(md, role),
		}
		if modelDeploymentPublishesKVEvents(md, role) {
			events := inject.VLLMKVEvents(md.Name + "-" + role.Name + "." + md.Namespace + ".svc")
			roleStatus.KVEvents = &workercore.ModelDeploymentRouterKVEvents{
				Endpoint: events.Endpoint, ReplayEndpoint: events.ReplayEndpoint, Topic: events.Topic,
			}
		}
		contract.Roles = append(contract.Roles, roleStatus)
	}
	endpointSelector := strings.Join([]string{
		modelDeploymentLabelKeyName + "=" + modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance + "=" + md.Name,
		"!" + modelDeploymentRouterLabelKey,
	}, ",")
	routerInput := router.Input{
		Roles: roles, Metrics: metrics, ModelName: md.Spec.Model.Name,
		TokenizerEndpoint: modelDeploymentRoleEndpoint(md, modelDeploymentTokenizerRole(md)),
		Namespace:         md.Namespace, EndpointSelector: endpointSelector,
	}
	if md.Spec.Engine == workercore.ModelDeploymentEngineVLLM {
		routerInput.KVEvents = router.KVEvents{
			Engine: md.Spec.Engine, Port: inject.VLLMKVEventsPort,
			ReplayPort: inject.VLLMKVEventsReplayPort, Topic: inject.VLLMKVEventsTopic,
		}
	}
	eppConfig, err := router.Render(md.Spec.Router.Name, routerInput)
	if err != nil {
		return ModelDeploymentRouterObjects{}, err
	}
	extraArgs, err := json.Marshal(md.Spec.Router.ExtraArgs)
	if err != nil {
		return ModelDeploymentRouterObjects{}, fmt.Errorf("render router extra arguments: %w", err)
	}
	configMap := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Data: map[string]string{
			modelDeploymentRouterConfigKey:      eppConfig,
			modelDeploymentRouterEnvoyConfigKey: modelDeploymentRouterEnvoyConfig,
			modelDeploymentRouterSelectorKey:    endpointSelector,
			modelDeploymentRouterTargetPortsKey: joinInt32s(ports),
			modelDeploymentRouterExtraArgsKey:   string(extraArgs),
		},
	}
	note(configMap)

	// A picker named on the object is the user's own reference and is used verbatim; only the
	// default this operator picked is redirected to the cluster's mirror.
	image := md.Spec.Router.Image
	if image == "" {
		image = modelDeploymentRedirectedImage(ctx,
			settings.ModelDeploymentRouterImage.ShouldValue(ctx))
	}
	replicas := int32(1)
	if md.Spec.Router.Replicas != nil {
		replicas = *md.Spec.Router.Replicas
	}
	args := make([]string, 0, 5+len(md.Spec.Router.ExtraArgs))
	args = append(args,
		"--endpoint-selector=$(ENDPOINT_SELECTOR)",
		"--endpoint-target-ports=$(ENDPOINT_TARGET_PORTS)",
		"--config-file=/config/"+modelDeploymentRouterConfigKey,
		"--secure-serving=false",
		"--grpc-health-port=9003",
		"--metrics-endpoint-auth=false",
	)
	args = append(args, md.Spec.Router.ExtraArgs...)
	deployment := &app.Deployment{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: maps.Clone(labels)},
		Spec: app.DeploymentSpec{
			Replicas: &replicas,
			Selector: &meta.LabelSelector{MatchLabels: maps.Clone(labels)},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: maps.Clone(labels), Annotations: map[string]string{
					modelDeploymentRouterConfigHash: hashRouterConfig(configMap.Data),
				}},
				Spec: core.PodSpec{
					ServiceAccountName: name,
					Containers: []core.Container{
						{
							Name: "envoy", Image: modelDeploymentRedirectedImage(ctx,
								settings.ModelDeploymentRouterProxyImage.ShouldValue(ctx)),
							Args:  []string{"-c", "/config/" + modelDeploymentRouterEnvoyConfigKey},
							Ports: []core.ContainerPort{{Name: "http", ContainerPort: modelDeploymentRouterHTTPPort}},
							ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{
								TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(modelDeploymentRouterHTTPPort)},
							}},
							VolumeMounts: []core.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
						},
						{
							Name: "epp", Image: image, Args: args,
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
		rendered, err = renderModelDeploymentRouterObjects(ctx, md)
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
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.ConfigMap, wanted),
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
			return alignModelDeploymentRouterMetadata(actual, expected) || changed
		}, modelDeploymentResourceNoteRouter, "service"); err != nil {
		return err
	}
	if err := r.syncModelDeploymentOwnedChildren(ctx, md, objectIfPresent(rendered.ServiceAccount, wanted),
		func() ctrlcli.Object { return new(core.ServiceAccount) }, new(core.ServiceAccountList),
		alignModelDeploymentRouterMetadata, modelDeploymentResourceNoteRouter, "serviceaccount"); err != nil {
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
			return alignModelDeploymentRouterMetadata(actual, expected) || changed
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
			return alignModelDeploymentRouterMetadata(actual, expected) || changed
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

	return alignModelDeploymentRouterMetadata(actual, expected) || changed
}

func alignModelDeploymentRouterConfigMap(actual, expected *core.ConfigMap) (changed bool) {
	if !maps.Equal(actual.Data, expected.Data) {
		actual.Data = expected.Data
		changed = true
	}
	return alignModelDeploymentRouterMetadata(actual, expected) || changed
}

func alignModelDeploymentRouterMetadata(actual, expected ctrlcli.Object) (changed bool) {
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
                route: {cluster: original_destination_cluster, timeout: 86400s}
          http_filters:
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
