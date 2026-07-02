package kuberess

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
)

func installKueue(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack-operator/image/Dockerfile.

	name := "kueue"
	version := "0.18.1"
	if disable.Has(name) {
		return nil
	}

	release := "gpustack-kueue"
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", name, version))
	download := fmt.Sprintf("https://github.com/kubernetes-sigs/kueue/releases/download/v%[1]s/kueue-%[1]s.tgz", version)

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()

	funcMap := extendKueueChartValuesTemplateFuncMap()
	funcMap["hasAPIResource"] = func(apiversion, kind string) bool {
		_, err := kubediscovery.GetAPIResourceForGVK(helmCli.KubeClientSet().Discovery(), schema.FromAPIVersionAndKind(apiversion, kind))
		return err == nil
	}

	values := getKueueChartTemplateValues(name, valuesContext, funcMap)

	chart := &helm.Chart{
		Name:        name,
		Version:     version,
		Release:     release,
		Path:        path,
		DownloadURL: download,
		Values:      values,
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	// The gpustack chart cannot ship the node-devices AdmissionCheck: its CRD is
	// installed here, at runtime. Apply it now that Kueue is up — the worker's
	// NodeDevicesAdmissionCheckReconciler then marks it Active, and
	// NodeQueueReconciler references it from accelerated ClusterQueues. Apply only
	// sets spec, so it never clobbers the controller-owned Active condition.
	return kubeappyaml.ApplyWithRestClientGetter(ctx, nodeDevicesAdmissionCheckYAML, helmCli.KubeRestClientGetter())
}

// nodeDevicesAdmissionCheckYAML is the gate-3 AdmissionCheck object. Its name and
// controllerName are the contract shared with the worker's NodeDevicesAdmission
// controllers (_NodeDevicesAdmissionCheckName / _NodeDevicesControllerName).
const nodeDevicesAdmissionCheckYAML = `
apiVersion: kueue.x-k8s.io/v1beta2
kind: AdmissionCheck
metadata:
  name: gpustack-node-devices
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
spec:
  controllerName: worker.gpustack.ai/node-devices
`

const kueueChartValuesTemplate = `
{{- $hasCertManager := hasAPIResource "cert-manager.io/v1" "Certificate" }}

fullnameOverride: "kueue"
namespaceOverride: "{{ $.Namespace }}"

enablePrometheus: false
enableCertManager: {{ $hasCertManager }}
enableVisibilityAPF: false
enableKueueViz: false

controllerManager:
  tolerations:
    - operator: "Exists"
  manager:
    image:
{{- $registry := default "docker.io" $.ContainerRegistry }}
{{- $namespace := default "gpustack" $.ContainerNamespace }}
{{- $prefix := "mirrored" }}
{{- $image := printf "%s/%s/%s-kueue" $registry $namespace $prefix }}
      repository: "{{ $image }}"
      pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"
    podAnnotations:
      {{ $.ManagedLabel }}: "true"
    resources:
      limits:
        cpu: "2"
        memory: 4Gi
      requests:
        cpu: 100m
        memory: 128Mi
{{- if $.ImagePullSecrets }}
  imagePullSecrets:
{{- range $.ImagePullSecrets }}
    - name: {{ . }}
{{- end }}
{{- end }}

managerConfig:
  # -- controller_manager_config.yaml.
  # ControllerManager utilizes this yaml via manager-config Configmap.
  # @default -- controllerManagerConfigYaml
  controllerManagerConfigYaml: |-
    apiVersion: config.kueue.x-k8s.io/v1beta2
    kind: Configuration
    featureGates:
      AssignQueueLabelsForPods: false
      TASBalancedPlacement: true
    health:
      healthProbeBindAddress: :8081
    metrics:
      bindAddress: :8443
    # enableClusterQueueResources: true
    webhook:
      port: 9443
    leaderElection:
      leaderElect: true
      resourceName: c1f6bfd2.kueue.x-k8s.io
    controller:
      groupKindConcurrency:
        Job.batch: 5
        Pod: 5
        Workload.kueue.x-k8s.io: 5
        LocalQueue.kueue.x-k8s.io: 1
        ClusterQueue.kueue.x-k8s.io: 1
        ResourceFlavor.kueue.x-k8s.io: 1
    clientConnection:
      qps: 50
      burst: 100
    #pprofBindAddress: :8083
    #waitForPodsReady:
    #  timeout: 5m
    #  recoveryTimeout: 3m
    #  blockAdmission: false
    #  requeuingStrategy:
    #    timestamp: Eviction
    #    backoffLimitCount: null # null indicates infinite requeuing
    #    backoffBaseSeconds: 60
    #    backoffMaxSeconds: 3600
    #manageJobsWithoutQueueName: true
    managedJobsNamespaceSelector:
      matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: NotIn
          values: [ kube-system, {{ $.Namespace }} ]
{{- if $hasCertManager }}
    internalCertManagement:
      enable: false
{{- end }}
    integrations:
      frameworks:
      - "batch/job"
      - "kubeflow.org/mpijob"
      - "ray.io/rayjob"
      - "ray.io/raycluster"
      - "jobset.x-k8s.io/jobset"
      - "trainer.kubeflow.org/trainjob"
      - "kubeflow.org/paddlejob"
      - "kubeflow.org/pytorchjob"
      - "kubeflow.org/tfjob"
      - "kubeflow.org/xgboostjob"
      - "kubeflow.org/jaxjob"
      - "workload.codeflare.dev/appwrapper"
      - "pod"
      - "deployment"
      - "statefulset"
      - "leaderworkerset.x-k8s.io/leaderworkerset"
    #  externalFrameworks:
    #  - "Foo.v1.example.com"
    #fairSharing:
    #  preemptionStrategies: [LessThanOrEqualToFinalShare, LessThanInitialShare]
    #admissionFairSharing:
    #  usageHalfLifeTime: "168h" # 7 days
    #  usageSamplingInterval: "5m"
    #  resourceWeights: # optional, defaults to 1 for all resources if not specified
    #    cpu: 0    # if you want to completely ignore cpu usage
    #    memory: 0 # ignore completely memory usage
    #    example.com/gpu: 100 # and you care only about GPUs usage
{{- if $.Manufacturers }}
    resources:
      # Credits are scored on the integer base B = D = 1600000 (one whole card = B
      # credits): exclusive→B, shared→B/10, sliced.units→B/D=1 (× multiplyBy
      # .sliced). Every per-mode value stays an integer, so Kueue's ResourceValue
      # int64 quantization (q.Value(), which ceils non-CPU resources) never rounds
      # a fractional credit up to 1.
      transformations:
{{- $exclusiveCreditsFactor := getExclusiveCreditsFactor }}
{{- $sharedCreditsFactor := getSharedCreditsFactor }}
{{- $slicedCreditsFactor := getSlicedCreditsFactor }}
{{- range $.Manufacturers }}
{{- $manu := . }}
{{- $manuCreditsResName := getCreditsResourceName $manu }}
{{- $manuExclusiveResName := getExclusiveResourceName $manu }}
{{- $manuSharedResName := getSharedResourceName $manu }}
{{- $manuSlicedResName := getSlicedResourceName $manu }}
{{- $manuSlicedUnitsResName := getSlicedUnitsResourceName $manu }}
{{- $manuSlicedCoresPercentageResName := getSlicedCoresPercentageResourceName $manu }}
{{- $manuSlicedMemoryPercentageResName := getSlicedMemoryPercentageResourceName $manu }}
{{- $manuSlicedMemoryMibResName := getSlicedMemoryMibResourceName $manu }}
      # Multiply each manufacturer resource into a single credits resource, so that
      # the queue can be configured with a single credit budget. The multiplyBy
      # factor is the number of credits per unit of the input resource, so that
      # the output credits resource is always an integer.
      - input: {{ $manuExclusiveResName }}
        strategy: Replace
        outputs:
          {{ $manuCreditsResName }}: "{{ $exclusiveCreditsFactor }}"
      - input: {{ $manuSharedResName }}
        strategy: Replace
        outputs:
          {{ $manuCreditsResName }}: "{{ $sharedCreditsFactor }}"
      - input: {{ $manuSlicedUnitsResName }}
        strategy: Replace
        multiplyBy: {{ $manuSlicedResName }}
        outputs:
          {{ $manuCreditsResName }}: "{{ $slicedCreditsFactor }}"
      # The resource ".sliced" is the multiplyBy above: Kueue does not consume a
      # multiplyBy resource on Replace, so it would leak into the Pod request.
      # Drain it with empty Outputs + Replace so Kueue ignores it.
      - input: {{ $manuSlicedResName }}
        strategy: Replace
        outputs: {}
      # These node-level resources (cores-percentage / memory-percentage /
      # memory-mib) are read by the scheduler and kubelet to place a Pod on a node,
      # never as Kueue credits. Drain each with empty Outputs + Replace so a Pod
      # requesting them is not marked inadmissible against this credits-only queue.
      - input: {{ $manuSlicedCoresPercentageResName }}
        strategy: Replace
        outputs: {}
      - input: {{ $manuSlicedMemoryPercentageResName }}
        strategy: Replace
        outputs: {}
      - input: {{ $manuSlicedMemoryMibResName }}
        strategy: Replace
        outputs: {}
{{- end }}
{{- end }}
    #objectRetentionPolicies:
    #  workloads:
    #    afterFinished: null # null indicates infinite retention, 0s means no retention at all
    #    afterDeactivatedByKueue: null # null indicates infinite retention, 0s means no retention at all
`

func getKueueChartTemplateValues(name string, data map[string]any, extendFuncMap template.FuncMap) helm.TemplateValues {
	return helm.TemplateValues{
		Application:   name,
		Template:      kueueChartValuesTemplate,
		ExtendFuncMap: extendFuncMap,
		Context:       data,
	}
}

func extendKueueChartValuesTemplateFuncMap() template.FuncMap {
	return map[string]any{
		// Resource name helpers for Kueue chart values template.
		"getExclusiveResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableResourceName(s, workercore.DeviceAllocationModeExclusive))
		},
		"getSharedResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableResourceName(s, workercore.DeviceAllocationModeShared))
		},
		"getSlicedResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableResourceName(s, workercore.DeviceAllocationModeSliced))
		},
		"getSlicedUnitsResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableSlicedUnitsResourceName(s))
		},
		"getSlicedCoresPercentageResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(s))
		},
		"getSlicedMemoryPercentageResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(s))
		},
		"getSlicedMemoryMibResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableSlicedMemoryMibResourceName(s))
		},
		"getCreditsResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(nodefeature.GetAcceleratableCreditsResourceName(s))
		},
		// Kueue credits factor helpers for Kueue chart values template.
		"getExclusiveCreditsFactor": func() string {
			return strconv.Itoa(nodefeature.CreditsPerCard)
		},
		"getSharedCreditsFactor": func() string {
			return strconv.Itoa(nodefeature.CreditsPerCard / nodefeature.SharedResourceMaxSize)
		},
		"getSlicedCreditsFactor": func() string {
			return strconv.Itoa(nodefeature.CreditsPerCard / nodefeature.ResourceMaxUnits)
		},
		// hasAPIResource is a placeholder function for Kueue chart values template.
		// It will be overridden by the actual implementation in installKueue.
		"hasAPIResource": func(apiversion, kind string) bool {
			return false
		},
	}
}
