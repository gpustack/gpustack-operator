package kuberess

import (
	"context"
	"fmt"
	"path/filepath"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/system"
)

func installKueue(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack/image/Dockerfile.

	name := "kueue"
	version := "0.17.2"
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
		// Skip installation if "v1beta2.kueue.x-k8s.io" ApiService is ready,
		// which means the cluster has already installed Kueue but not the same release.
		SkippedInstallationIfApiServiceReady: fmt.Sprintf("%s.%s", kueue.SchemeGroupVersion.Version, kueue.SchemeGroupVersion.Group),
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	return nil
}

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
      transformations:
{{- range $.Manufacturers }}
{{- $manu := . }}
{{- $manuCreditsResName := getCreditsResourceName $manu }}
{{- $manuExclusiveResName := getExclusiveResourceName $manu }}
{{- $manuSharedResName := getSharedResourceName $manu }}
{{- $manuSlicedResName := getSlicedResourceName $manu }}
      - input: {{ $manuExclusiveResName }}
        strategy: Replace
        outputs:
          {{ $manuCreditsResName }}: "1"
      - input: {{ $manuSharedResName }}
        strategy: Replace
        outputs:
          {{ $manuCreditsResName }}: "0.1"
      - input: {{ $manuSlicedResName }}
        strategy: Replace
        outputs:
          {{ $manuCreditsResName }}: "0.0001"
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
		"getExclusiveResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(devicefeature.GetResourceName(s, workercore.DeviceAllocationModeExclusive))
		},
		"getSharedResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(devicefeature.GetResourceName(s, workercore.DeviceAllocationModeShared))
		},
		"getSlicedResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(devicefeature.GetResourceName(s, workercore.DeviceAllocationModeSliced))
		},
		"getCreditsResourceName": func(v any) string {
			s, ok := v.(string)
			if !ok {
				panic(fmt.Sprintf("manufacturer should be string, but got %T", v))
			}
			return string(devicefeature.GetCreditsResourceName(s))
		},
		"hasAPIResource": func(apiversion, kind string) bool {
			return false
		},
	}
}
