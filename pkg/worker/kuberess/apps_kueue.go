package kuberess

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"text/template"
	"time"

	"golang.org/x/mod/semver"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

const (
	// kueueReleaseName is the Helm release name the worker installs Kueue under.
	kueueReleaseName = "gpustack-kueue"
	// kueueAPIGroup is the API group of every Kueue CRD.
	kueueAPIGroup = "kueue.x-k8s.io"
)

func installKueue(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack-operator/Dockerfile.

	name := "kueue"
	version := "0.18.2"
	chartVersion := "0.17.6"
	if disable.Has(name) {
		return nil
	}

	// If the Kubernetes version is 1.31 or later,
	// use Kueue chart 0.18 directly (which requires Kubernetes >= 1.31),
	// see https://github.com/kubernetes-sigs/kueue/pull/11568.
	if kubeVer := helmCli.KubeVersion(ctx); semver.Compare(fmt.Sprintf("v%s.%s", kubeVer.Major, kubeVer.Minor), "v1.31") >= 0 {
		chartVersion = version
	}

	release := kueueReleaseName
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", name, chartVersion))
	download := fmt.Sprintf("https://github.com/kubernetes-sigs/kueue/releases/download/v%[1]s/kueue-%[1]s.tgz", chartVersion)

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["Tag"] = "v" + version

	funcMap := extendKueueChartValuesTemplateFuncMap()
	funcMap["hasAPIResource"] = func(apiversion, kind string) bool {
		_, err := kubediscovery.GetAPIResourceForGVK(helmCli.KubeClientSet().Discovery(), schema.FromAPIVersionAndKind(apiversion, kind))
		return err == nil
	}

	values := getKueueChartTemplateValues(name, valuesContext, funcMap)

	// Self-heal a Kueue left deadlocked by an earlier upgrade before (re)installing:
	// a Terminating Kueue CRD would otherwise make every install below fail forever
	// and block the operator from starting. No-op on a healthy cluster.
	if err := reapOrphanedKueue(ctx, helmCli); err != nil {
		return fmt.Errorf("reap orphaned kueue: %w", err)
	}

	chart := &helm.Chart{
		Name:        name,
		Version:     chartVersion,
		Release:     release,
		Path:        path,
		DownloadURL: download,
		Values:      values,
		// Repair a failed Kueue release with an upgrade, never the uninstall+install
		// path: uninstalling tears down the controller while ClusterQueues still hold
		// the resource-in-use finalizer, which strands the Helm-managed CRDs.
		RepairViaUpgradeOnly: true,
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	// The gpustack chart cannot ship the node-devices AdmissionCheck: its CRD is
	// installed here, at runtime. Apply it now that Kueue is up — the worker's
	// NodeDevicesAdmissionCheckReconciler then marks it Active, and
	// InstanceTypeReconciler references it from accelerated ClusterQueues. Apply only
	// sets spec, so it never clobbers the controller-owned Active condition.
	return kubeappyaml.ApplyWithRestClientGetter(ctx, nodeDevicesAdmissionCheckYAML, helmCli.KubeRestClientGetter())
}

// reapOrphanedKueue clears the state a torn-down Kueue controller leaves behind so
// the (re)install can recreate its CRDs. Ordering is load-bearing: the Kueue
// validating webhook (failurePolicy: Fail) rejects a finalizer-clearing update once
// its Service has no endpoints, so the webhook configurations must be deleted before
// the finalizers are stripped. No-op on a healthy cluster (acts only when a
// kueue.x-k8s.io CRD is Terminating). Safe to re-run.
func reapOrphanedKueue(ctx context.Context, helmCli *helm.Client) error {
	restCfg, err := helmCli.KubeRestClientGetter().ToRESTConfig()
	if err != nil {
		return fmt.Errorf("get rest config: %w", err)
	}
	dynCli, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	cli := helmCli.KubeClientSet()
	reaped, err := reapOrphanedKueueWith(ctx, cli, dynCli)
	if err != nil {
		return err
	}
	if !reaped {
		return nil
	}

	// Wait for the freed CRDs to drain so the install below recreates them cleanly.
	return waitKueueCRDsDrained(ctx, cli, 90*time.Second, 3*time.Second)
}

// reapOrphanedKueueWith is the testable core of reapOrphanedKueue. It reports
// whether it acted (a stuck Kueue CRD was found) so the caller can wait for the
// drain only when there is something to drain.
func reapOrphanedKueueWith(ctx context.Context, cli kubernetes.Interface, dynCli dynamic.Interface) (bool, error) {
	stuck, err := listTerminatingKueueCRDs(ctx, cli)
	if err != nil {
		return false, err
	}
	if len(stuck) == 0 {
		return false, nil
	}

	// 1. Delete the Kueue admission webhook configurations first — see the ordering
	//    note on reapOrphanedKueue.
	if err := deleteKueueWebhookConfigs(ctx, cli); err != nil {
		return true, err
	}

	// 2. Strip the finalizers pinning the Terminating CRs so each stuck CRD can drain.
	for i := range stuck {
		if err := stripKueueCRFinalizers(ctx, dynCli, &stuck[i]); err != nil {
			return true, err
		}
	}

	return true, nil
}

// listTerminatingKueueCRDs returns the Kueue CRDs that are stuck Terminating.
func listTerminatingKueueCRDs(ctx context.Context, cli kubernetes.Interface) ([]apiext.CustomResourceDefinition, error) {
	list, err := cli.ApiextensionsV1().CustomResourceDefinitions().List(ctx, meta.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list CRDs: %w", err)
	}

	var stuck []apiext.CustomResourceDefinition
	for i := range list.Items {
		crd := list.Items[i]
		if crd.Spec.Group == kueueAPIGroup && crd.DeletionTimestamp != nil {
			stuck = append(stuck, crd)
		}
	}
	return stuck, nil
}

// deleteKueueWebhookConfigs deletes the Kueue admission webhook configurations. They
// are selected by the Helm release-instance label rather than by name: the chart sets
// fullnameOverride=kueue, so their names are kueue-* and this label is the stable
// identifier across that override.
func deleteKueueWebhookConfigs(ctx context.Context, cli kubernetes.Interface) error {
	sel := "app.kubernetes.io/instance=" + kueueReleaseName
	listOpts := meta.ListOptions{LabelSelector: sel}
	reg := cli.AdmissionregistrationV1()

	if err := reg.ValidatingWebhookConfigurations().DeleteCollection(ctx, meta.DeleteOptions{}, listOpts); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete kueue validating webhook configurations: %w", err)
	}
	if err := reg.MutatingWebhookConfigurations().DeleteCollection(ctx, meta.DeleteOptions{}, listOpts); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete kueue mutating webhook configurations: %w", err)
	}
	return nil
}

// stripKueueCRFinalizers clears the finalizers on every Terminating custom resource
// of the given Kueue CRD so the CRD can finish deleting. Non-Terminating CRs are left
// untouched, so a live Kueue's queues keep their accounting finalizer.
func stripKueueCRFinalizers(ctx context.Context, dynCli dynamic.Interface, crd *apiext.CustomResourceDefinition) error {
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  storageCRDVersion(crd),
		Resource: crd.Spec.Names.Plural,
	}

	list, err := dynCli.Resource(gvr).List(ctx, meta.ListOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list %s: %w", gvr.Resource, err)
	}

	const patch = `{"metadata":{"finalizers":[]}}`
	for i := range list.Items {
		obj := list.Items[i]
		if obj.GetDeletionTimestamp() == nil || len(obj.GetFinalizers()) == 0 {
			continue
		}
		// An empty namespace addresses cluster-scoped resources (e.g. ClusterQueue).
		_, err := dynCli.Resource(gvr).Namespace(obj.GetNamespace()).
			Patch(ctx, obj.GetName(), types.MergePatchType, []byte(patch), meta.PatchOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("strip finalizers on %s/%s: %w", gvr.Resource, obj.GetName(), err)
		}
	}
	return nil
}

// storageCRDVersion returns the CRD's storage version, falling back to the first
// served then the first declared version. The storage version is listed and patched
// without conversion; a non-storage served version is materialized through the CRD's
// conversion webhook, which fails once that webhook's backing Service is gone (the
// exact state this reaper runs in).
func storageCRDVersion(crd *apiext.CustomResourceDefinition) string {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return crd.Spec.Versions[i].Name
		}
	}
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Served {
			return crd.Spec.Versions[i].Name
		}
	}
	if len(crd.Spec.Versions) > 0 {
		return crd.Spec.Versions[0].Name
	}
	return ""
}

// waitKueueCRDsDrained blocks until no Kueue CRD is Terminating, bounded by timeout.
func waitKueueCRDsDrained(ctx context.Context, cli kubernetes.Interface, timeout, interval time.Duration) error {
	return waitx.PollUntilContextTimeout(ctx, interval, timeout, false,
		func(ctx context.Context) error {
			stuck, err := listTerminatingKueueCRDs(ctx, cli)
			if err != nil {
				return err
			}
			if len(stuck) > 0 {
				return fmt.Errorf("%d kueue CRD(s) still terminating", len(stuck))
			}
			return nil
		})
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
      tag: "{{ $.Tag }}"
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
      # Enables resources.quotaCheckStrategy below (alpha in Kueue 0.18). The bundled
      # controller image is always 0.18, so the gate is always available.
      QuotaCheckStrategy: true
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
    resources:
      # Kueue is not the authoritative ledger in this operator — the Devices CR
      # is. A ClusterQueue advertises exactly one coarse admission dimension (cpu
      # for general pools, the manufacturer credits for accelerated pools), while a
      # Workload still carries the Pod resources the queue does not cover: memory
      # and ephemeral-storage on every pool, cpu on accelerated pools, and the
      # node-level .sliced.* counters the scheduler/kubelet read. Check only the
      # covered dimension and ignore the rest, instead of refusing to assign a
      # flavor for an uncovered resource (which would mark the Workload inadmissible).
      quotaCheckStrategy: IgnoreUndeclared
{{- if $.Manufacturers }}
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
      # Multiply each manufacturer resource into a single credits resource, so that
      # the queue can be configured with a single credit budget. The multiplyBy
      # factor is the number of credits per unit of the input resource, so that
      # the output credits resource is always an integer. The ".sliced" multiplyBy
      # leaks through unconsumed and the node-level .sliced.* counters pass straight
      # through; quotaCheckStrategy above lets Kueue ignore them.
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
