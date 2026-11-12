package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepod "sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	"sigs.k8s.io/yaml"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

type goldenYAMLScenario struct {
	name             string
	md               *workercore.ModelDeployment
	objects          []ctrlcli.Object
	deliveryMode     string
	secondPassMode   string
	secondPassScale  map[string]int32
	remotePolicy     string
	sourceReady      bool
	qualifyAllTuples bool
	markPodsReady    bool
	statusMutate     func(*workercore.ModelDeployment)
}

func TestModelDeploymentGoldenYAML(t *testing.T) {
	settingtest.MergeDelegatedSettings(t, map[string]string{
		"model-deployment-routing-sidecar-image": legacyRenderPinnedSidecarImage,
		"container-registry":                     "",
		"container-namespace":                    "",
	})

	scenarios := buildGoldenYAMLScenarios()
	require.Len(t, scenarios, 33)

	goldenDir := filepath.Join("testdata", "golden-render")
	if *legacyRenderUpdate {
		require.NoError(t, os.MkdirAll(goldenDir, 0o755))
	}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			srcPath := filepath.Join(goldenDir, tc.name+".yaml")
			dstPath := filepath.Join(goldenDir, tc.name+"_dst.yaml")
			if *legacyRenderUpdate {
				inputYAML, dstYAML := runGoldenYAMLScenario(t, tc, prepareGoldenInputObjects(tc))
				require.NoError(t, os.WriteFile(srcPath, []byte(inputYAML), 0o644))
				require.NoError(t, os.WriteFile(dstPath, []byte(dstYAML), 0o644))
			}

			expectedInputBytes, err := os.ReadFile(srcPath)
			require.NoError(t, err, "missing golden input file %s (run with -update)", srcPath)
			expectedInput := string(expectedInputBytes)

			expectedDstBytes, err := os.ReadFile(dstPath)
			require.NoError(t, err, "missing golden dst file %s (run with -update)", dstPath)
			expectedDst := string(expectedDstBytes)

			parsedInputObjs := parseGoldenInputYAML(t, srcPath, expectedInput)

			renderedInput, renderedDst := runGoldenYAMLScenario(t, tc, parsedInputObjs)
			assert.Equal(t, expectedInput, renderedInput)
			assert.Equal(t, expectedDst, renderedDst)
		})
	}
}

func prepareGoldenInputObjects(tc goldenYAMLScenario) []ctrlcli.Object {
	inputMD := tc.md.DeepCopy()
	if inputMD.Namespace == "" {
		inputMD.Namespace = "default"
	}
	inputMD.TypeMeta = meta.TypeMeta{
		APIVersion: "worker.gpustack.ai/v1",
		Kind:       "ModelDeployment",
	}
	inputMD.UID = ""
	inputMD.ResourceVersion = ""
	inputMD.CreationTimestamp = meta.Time{}
	inputMD.ManagedFields = nil
	inputMD.Generation = 0
	inputMD.Status = workercore.ModelDeploymentStatus{}

	objs := make([]ctrlcli.Object, 0, 1+len(tc.objects))
	objs = append(objs, inputMD)
	for _, obj := range tc.objects {
		cloned := obj.DeepCopyObject().(ctrlcli.Object)
		setGoldenObjectTypeMeta(cloned)
		cloned.SetResourceVersion("")
		cloned.SetCreationTimestamp(meta.Time{})
		cloned.SetManagedFields(nil)
		if _, isArtifact := cloned.(*workercore.ModelArtifact); !isArtifact {
			cloned.SetUID("")
			cloned.SetGeneration(0)
		}
		objs = append(objs, cloned)
	}
	return objs
}

func setGoldenObjectTypeMeta(obj ctrlcli.Object) {
	switch typed := obj.(type) {
	case *workercore.ModelDeployment:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "ModelDeployment"}
	case *workercore.KVCachePoolBinding:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "KVCachePoolBinding"}
	case *workercore.KVCachePool:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "KVCachePool"}
	case *workercore.KVCacheBackend:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "KVCacheBackend"}
	case *workercore.ModelArtifact:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "ModelArtifact"}
	case *workercore.NodeModelStore:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "NodeModelStore"}
	case *worker.InstanceType:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "worker.gpustack.ai/v1", Kind: "InstanceType"}
	case *core.Node:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "v1", Kind: "Node"}
	case *storagev1.CSIDriver:
		typed.TypeMeta = meta.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver"}
	}
}

func parseGoldenInputYAML(t *testing.T, srcPath, rawYAML string) []ctrlcli.Object {
	t.Helper()
	docs := strings.Split(strings.TrimSpace(rawYAML), "\n---\n")
	require.NotEmpty(t, docs, "golden input file %s has no YAML documents", srcPath)

	objs := make([]ctrlcli.Object, 0, len(docs))
	for i, doc := range docs {
		var tm meta.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(doc), &tm), "unmarshal TypeMeta from doc %d in %s", i, srcPath)

		var obj ctrlcli.Object
		switch tm.Kind {
		case "ModelDeployment":
			obj = new(workercore.ModelDeployment)
		case "KVCachePoolBinding":
			obj = new(workercore.KVCachePoolBinding)
		case "KVCachePool":
			obj = new(workercore.KVCachePool)
		case "KVCacheBackend":
			obj = new(workercore.KVCacheBackend)
		case "ModelArtifact":
			obj = new(workercore.ModelArtifact)
		case "NodeModelStore":
			obj = new(workercore.NodeModelStore)
		case "InstanceType":
			obj = new(worker.InstanceType)
		case "Node":
			obj = new(core.Node)
		case "CSIDriver":
			obj = new(storagev1.CSIDriver)
		default:
			t.Fatalf("unsupported Kind %q in doc %d of %s", tm.Kind, i, srcPath)
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), obj), "unmarshal %s from doc %d in %s", tm.Kind, i, srcPath)
		objs = append(objs, obj)
	}
	return objs
}

func runGoldenYAMLScenario(
	t *testing.T, tc goldenYAMLScenario, inputObjs []ctrlcli.Object,
) (string, string) {
	t.Helper()

	origDeliveryMode := modelArtifactDeliveryMode
	origPolicy := modelDeploymentModelTransferPolicy
	origReady := modelDeploymentModelTransferSourceReady
	origQualified := modelDeploymentModelTransferQualified
	t.Cleanup(func() {
		modelArtifactDeliveryMode = origDeliveryMode
		modelDeploymentModelTransferPolicy = origPolicy
		modelDeploymentModelTransferSourceReady = origReady
		modelDeploymentModelTransferQualified = origQualified
	})

	activeMode := tc.deliveryMode
	if activeMode == "" {
		activeMode = settings.ModelArtifactDeliveryNode
	}
	modelArtifactDeliveryMode = func(context.Context) string { return activeMode }

	remotePolicy := tc.remotePolicy
	if remotePolicy == "" {
		remotePolicy = settings.ModelArtifactRemoteDeliveryDisabled
	}
	modelDeploymentModelTransferPolicy = func(context.Context) string { return remotePolicy }
	modelDeploymentModelTransferSourceReady = func(
		context.Context, ctrlcli.Reader, *workercore.ModelArtifact,
	) (bool, time.Time, error) {
		return tc.sourceReady, time.Time{}, nil
	}
	if tc.qualifyAllTuples {
		modelDeploymentModelTransferQualified = func(modelTransferTuple) bool {
			return true
		}
	}

	require.NotEmpty(t, inputObjs)
	rawInputMD, ok := inputObjs[0].(*workercore.ModelDeployment)
	require.True(t, ok, "first document in scenario input must be ModelDeployment")

	inputDocs := make([]string, 0, len(inputObjs))
	for _, obj := range inputObjs {
		cloned := obj.DeepCopyObject().(ctrlcli.Object)
		setGoldenObjectTypeMeta(cloned)
		b, mErr := yaml.Marshal(cloned)
		require.NoError(t, mErr)
		inputDocs = append(inputDocs, strings.TrimSpace(string(b)))
	}
	inputYAML := strings.Join(inputDocs, "\n---\n") + "\n"

	md := rawInputMD.DeepCopy()
	md.UID = "md-uid"
	md.ResourceVersion = "1"
	md.Generation = 1
	if tc.statusMutate != nil {
		tc.statusMutate(md)
	}

	baseObjs := goldenBaseClusterFixtures()
	for _, extra := range inputObjs[1:] {
		baseObjs = append(baseObjs, extra.DeepCopyObject().(ctrlcli.Object))
	}
	baseObjs = append(baseObjs, md)

	nextIndexPerPrefix := map[string]int{}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(
			&workercore.ModelDeployment{},
			&workercore.KVCachePoolBinding{},
			&workercore.Devices{},
		).
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
			pod, ok := obj.(*core.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithIndex(&core.Pod{}, kueuepod.PodGroupNameCacheKey, func(obj ctrlcli.Object) []string {
			pod, ok := obj.(*core.Pod)
			if !ok {
				return nil
			}
			group := pod.Labels[kueuepodconst.GroupNameLabel]
			if group == "" {
				return nil
			}
			return []string{group}
		}).
		WithIndex(&core.Pod{}, modelDeploymentDrainIndexPodUID, func(obj ctrlcli.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&workercore.ModelPrefetch{}, modelPrefetchArtifactIndex, modelPrefetchArtifactIndexValues).
		WithObjects(baseObjs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				switch typed := obj.(type) {
				case *core.Pod:
					if typed.Name == "" && typed.GenerateName != "" {
						ord := typed.Labels[modelDeploymentReplicaOrdinalLabel]
						mem := typed.Labels[modelDeploymentMemberIndexLabel]
						if ord != "" && mem != "" {
							typed.Name = fmt.Sprintf("%s%s-%s", typed.GenerateName, ord, mem)
						} else {
							idx := nextIndexPerPrefix[typed.GenerateName]
							nextIndexPerPrefix[typed.GenerateName] = idx + 1
							typed.Name = fmt.Sprintf("%s%02d", typed.GenerateName, idx)
						}
					}
					if typed.UID == "" {
						typed.UID = types.UID("pod-uid-" + typed.Name)
					}
				case *kueue.Workload:
					if typed.UID == "" {
						typed.UID = types.UID("wl-uid-" + typed.Name)
					}
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	reconciler := &ModelDeploymentReconciler{
		Client:    cli,
		APIReader: cli,
		Recorder:  ctrlrecord.NewFakeRecorder(128),
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

	_, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	if tc.secondPassMode != "" {
		activeMode = tc.secondPassMode
	}
	if len(tc.secondPassScale) > 0 {
		var current workercore.ModelDeployment
		require.NoError(t, cli.Get(ctx, req.NamespacedName, &current))
		for i := range current.Spec.Roles {
			if target, ok := tc.secondPassScale[current.Spec.Roles[i].Name]; ok {
				current.Spec.Roles[i].Replicas = target
			}
		}
		require.NoError(t, cli.Update(ctx, &current))
	}
	if tc.markPodsReady {
		var pods core.PodList
		require.NoError(t, cli.List(ctx, &pods, ctrlcli.InNamespace(md.Namespace)))
		for i := range pods.Items {
			pod := pods.Items[i].DeepCopy()
			pod.Spec.NodeName = "node-0"
			pod.Status.Phase = core.PodRunning
			pod.Status.Conditions = []core.PodCondition{{
				Type:   core.PodReady,
				Status: core.ConditionTrue,
			}}
			pod.Status.ContainerStatuses = []core.ContainerStatus{{
				Name:  modelDeploymentMainContainerName,
				Ready: true,
			}}
			require.NoError(t, cli.Update(ctx, pod))
		}
	}

	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var podList core.PodList
	require.NoError(t, cli.List(ctx, &podList, ctrlcli.InNamespace(md.Namespace)))
	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].Name < podList.Items[j].Name
	})

	var svcList core.ServiceList
	require.NoError(t, cli.List(ctx, &svcList, ctrlcli.InNamespace(md.Namespace)))
	sort.Slice(svcList.Items, func(i, j int) bool {
		return svcList.Items[i].Name < svcList.Items[j].Name
	})

	dstDocs := make([]string, 0, len(podList.Items)+len(svcList.Items))
	for i := range podList.Items {
		pod := podList.Items[i].DeepCopy()
		pod.TypeMeta = meta.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		}
		pod.UID = ""
		pod.ResourceVersion = ""
		pod.CreationTimestamp = meta.Time{}
		pod.ManagedFields = nil
		pod.Generation = 0
		podBytes, mErr := yaml.Marshal(pod)
		require.NoError(t, mErr)
		dstDocs = append(dstDocs, strings.TrimSpace(string(podBytes)))
	}

	for i := range svcList.Items {
		svc := svcList.Items[i].DeepCopy()
		svc.TypeMeta = meta.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		}
		svc.UID = ""
		svc.ResourceVersion = ""
		svc.CreationTimestamp = meta.Time{}
		svc.ManagedFields = nil
		svc.Generation = 0
		svcBytes, mErr := yaml.Marshal(svc)
		require.NoError(t, mErr)
		dstDocs = append(dstDocs, strings.TrimSpace(string(svcBytes)))
	}

	return inputYAML, strings.Join(dstDocs, "\n---\n") + "\n"
}

func goldenBaseClusterFixtures() []ctrlcli.Object {
	h20 := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "h20-8x"
		it.Status.Entrance = "queue-for-h20-8x"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
		it.Status.Detail.Memory = "96Gi"
	})

	h20Sliced := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "h20-sliced"
		it.Status.Entrance = "queue-for-h20-sliced"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
		it.Status.Detail.Memory = "96Gi"
		it.Status.Detail.SlicedDetail.Logical.Count = 128
	})

	a100Mig := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "a100-mig"
		it.Status.Entrance = "queue-for-a100-mig"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
		it.Status.Detail.Memory = "80Gi"
		it.Status.Detail.SlicedDetail.Physical.Count = 2
		it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
			{
				Name:      "3g.40gb",
				MemoryMib: 40 << 10,
				Count:     2,
			},
		}
	})

	mi300x := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "mi300x-8x"
		it.Status.Entrance = "queue-for-mi300x-8x"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAMD
		it.Status.Detail.Memory = "192Gi"
		it.Status.Detail.RuntimeVersion = "6.3"
		it.Status.Detail.RuntimeVersions = []string{"6.3"}
	})

	atlas910b := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "atlas-910b"
		it.Status.Entrance = "queue-for-atlas-910b"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		it.Status.Detail.Memory = "64Gi"
		it.Status.Detail.RuntimeVersion = "8.2"
		it.Status.Detail.RuntimeVersions = []string{"8.2"}
	})

	return []ctrlcli.Object{
		h20,
		h20Sliced,
		a100Mig,
		mi300x,
		atlas910b,
		&nodev1.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "nvidia"}, Handler: "nvidia"},
		&nodev1.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "rocm"}, Handler: "rocm"},
		&nodev1.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "ascend"}, Handler: "ascend"},
	}
}

func goldenKVCacheFixtures(bindingName, protocol string) []ctrlcli.Object {
	poolName := "pool-" + bindingName
	backendName := "backend-" + bindingName

	binding := &workercore.KVCachePoolBinding{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      bindingName,
		},
		Spec: workercore.KVCachePoolBindingSpec{
			PoolRef: workercore.KVCachePoolBindingPoolReference{Name: poolName},
			Domain:  workercore.KVCachePoolBindingDomain{Name: "chat", BlockSize: 256, Dtype: "bfloat16"},
			Quota:   workercore.KVCachePoolBindingQuota{Ceiling: resource.MustParse("100Gi")},
		},
		Status: workercore.KVCachePoolBindingStatus{
			Phase:        KVCachePoolPhaseReady,
			PhaseMessage: "the master reports this domain's figures",
		},
	}
	KVCachePoolBindingConditionQuotaGranted.True(binding, "Granted", "the master granted 100Gi")
	for i := range binding.Status.Conditions {
		binding.Status.Conditions[i].LastTransitionTime = meta.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	pool := &workercore.KVCachePool{
		ObjectMeta: meta.ObjectMeta{
			Name: poolName,
		},
		Spec: workercore.KVCachePoolSpec{
			Backends: []string{backendName},
		},
		Status: workercore.KVCachePoolStatus{
			ClientEndpoint: "shared-kv-master.gpustack-system.svc:50051",
		},
	}

	backend := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{
			Name: backendName,
		},
		Spec: workercore.KVCacheBackendSpec{
			Transport: workercore.KVCacheBackendTransport{
				Protocol: protocol,
			},
		},
	}

	return []ctrlcli.Object{binding, pool, backend}
}

func goldenModelArtifactFixtures() []ctrlcli.Object {
	digest := "sha256:" + strings.Repeat("a", 64)
	artifact := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "default",
			Name:       "qwen-72b",
			UID:        "artifact-uid-1",
			Generation: 1,
		},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{
					Repository: "Qwen/Qwen2.5-72B-Instruct",
				},
			},
		},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{
				ManifestDigest: digest,
				Revision:       "0123456789abcdef0123456789abcdef01234567",
				SizeBytes:      100 << 30,
			},
		},
	}
	ModelArtifactConditionResolved.True(artifact, "Resolved", "resolved")
	for i := range artifact.Status.Conditions {
		artifact.Status.Conditions[i].LastTransitionTime = meta.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "node-0",
		},
		Status: core.NodeStatus{
			Conditions: []core.NodeCondition{
				{
					Type:   core.NodeReady,
					Status: core.ConditionTrue,
				},
			},
		},
	}

	nms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{
			Name: "node-0",
		},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{
				{
					Digest: digest,
					State:  workercore.NodeModelStoreModelStateReady,
				},
			},
		},
	}

	csiDriver := &storagev1.CSIDriver{
		ObjectMeta: meta.ObjectMeta{
			Name: modelstore.DriverName,
		},
	}
	return []ctrlcli.Object{artifact, node, nms, csiDriver}
}

func buildGoldenYAMLScenarios() []goldenYAMLScenario {
	const (
		vllmImage       = "vllm/vllm-openai:v0.19.0"
		vllmAscendImage = "quay.io/ascend/vllm-ascend:v0.19.0"
		sglangImage     = "lmsysorg/sglang:v0.5.18"
		modelID         = "Qwen/Qwen2.5-72B-Instruct"
		vlModelID       = "Qwen/Qwen2.5-VL-7B-Instruct"
	)

	shm32Gi := resource.MustParse("32Gi")
	qty1 := resource.MustParse("1")
	qty2 := resource.MustParse("2")

	return []goldenYAMLScenario{
		// 1. Aggregated Server (10 scenarios)
		{
			name: "server-vllm-default",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "server-sglang-default",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineSGLang, Image: sglangImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        sglangImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "server-command-takeover",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Command:      []string{"vllm", "serve", modelID},
							ExtraArgs:    []string{"--max-model-len=32768"},
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "server-customization-volumes-tls-topology",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:                          "main",
							Kind:                          workercore.ModelDeploymentRoleKindServer,
							InstanceType:                  "h20-8x",
							Image:                         vllmImage,
							Replicas:                      1,
							ReplicaSize:                   1,
							Privileged:                    true,
							ShmSize:                       &shm32Gi,
							StartupTimeoutSeconds:         ptr.To(int32(3600)),
							TerminationGracePeriodSeconds: ptr.To(int64(60)),
							Ports: []workercore.ModelDeploymentPort{
								{Port: 9000, Protocol: core.ProtocolTCP},
							},
							Topology: &workercore.ModelDeploymentRoleTopology{
								RequiredLevel: "topology.kubernetes.io/zone",
							},
							ExtraArgs: []string{
								"--ssl-certfile", "/etc/tls/tls.crt",
								"--ssl-keyfile", "/etc/tls/tls.key",
							},
							AdditionalVolumes: []workercore.ModelDeploymentAdditionalVolume{
								{
									MountPath: "/etc/config",
									ReadOnly:  true,
									ConfigMap: &core.LocalObjectReference{Name: "app-config"},
								},
								{
									MountPath: "/etc/tls",
									ReadOnly:  true,
									Secret:    &core.LocalObjectReference{Name: "tls-secret"},
								},
								{
									MountPath:             "/mnt/cache",
									PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "model-cache"},
								},
								{
									MountPath: "/var/log/host",
									HostPath:  &core.HostPathVolumeSource{Path: "/var/log/gpustack"},
								},
							},
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "server-sliced-and-mig-accelerator",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "sliced",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-sliced",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator:                       ptr.To(qty1),
								AcceleratorSlicedMemoryPercentage: 50,
								AcceleratorSlicedCoresPercentage:  25,
							},
						},
						{
							Name:         "mig",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "a100-mig",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator:                   ptr.To(qty1),
								AcceleratorPartitionedProfile: "3g.40gb",
							},
						},
					},
				},
			},
		},
		{
			name: "server-multipod-collective",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  2,
							ExtraArgs: []string{
								"--tensor-parallel-size=2",
								"--pipeline-parallel-size=2",
							},
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
						},
					},
				},
			},
		},
		{
			name: "server-multipod-external-dp",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  2,
							ExtraArgs: []string{
								"--data-parallel-size=2",
								"--data-parallel-size-local=1",
							},
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "server-multipod-rdma-interfaces",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  2,
							ExtraArgs: []string{
								"--tensor-parallel-size=2",
							},
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
					},
				},
			},
		},
		{
			name: "server-elastic-ep",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     2,
							ReplicaSize:  1,
							ElasticEP: &workercore.ModelDeploymentRoleElasticEP{
								Width: 2,
							},
							ExtraArgs: []string{
								"--tensor-parallel-size=1",
								"--enable-expert-parallel",
							},
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
					},
				},
			},
		},
		{
			name: "server-routed-llmd",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:  workercore.ModelDeploymentModel{Name: modelID},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Router: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},

		// 2. Shared KVCache Pool Attachment (3 scenarios)
		{
			name:    "server-kvcache-tcp",
			objects: goldenKVCacheFixtures("shared-kv", "TCP"),
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:   workercore.ModelDeploymentModel{Name: modelID},
					Engine:  workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name:    "server-kvcache-rdma",
			objects: goldenKVCacheFixtures("shared-kv", "RDMA"),
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:   workercore.ModelDeploymentModel{Name: modelID},
					Engine:  workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
					},
				},
			},
		},
		{
			name:    "server-kvcache-efa",
			objects: goldenKVCacheFixtures("shared-kv", "EFA"),
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model:   workercore.ModelDeploymentModel{Name: modelID},
					Engine:  workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
					},
				},
			},
		},

		// 3. Disaggregated Prefill + Decode (14 scenarios)
		{
			name: "pd-unrouted-lone-pair",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage, "", nil, nil, 0,
			),
		},
		{
			name: "pd-vllm-nvidia-default",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD, nil, nil, 0,
			),
		},
		{
			name: "pd-vllm-nvidia-mooncake-rdma",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "Mooncake", Protocol: "RDMA"}, nil, 1,
			),
		},
		{
			name: "pd-vllm-nvidia-mooncake-efa",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "Mooncake", Protocol: "EFA"}, nil, 1,
			),
		},
		{
			name: "pd-vllm-nvidia-nixl-ucx-vllm-router",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterVLLM,
				&workercore.ModelDeploymentKVTransfer{Backend: "NIXL", Protocol: "RDMA"}, nil, 1,
			),
		},
		{
			name: "pd-vllm-nvidia-nixl-efa",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "NIXL", Protocol: "EFA"}, nil, 1,
			),
		},
		{
			name: "pd-vllm-amd-nixl",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "mi300x-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "NIXL"}, nil, 0,
			),
		},
		{
			name: "pd-vllm-ascend-hixl",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "atlas-910b", vllmAscendImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "HIXL"}, nil, 0,
			),
		},
		{
			name:    "pd-vllm-nvidia-multiconnector-kvcache",
			objects: goldenKVCacheFixtures("shared-kv", "TCP"),
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineVLLM, "h20-8x", vllmImage,
				workercore.ModelDeploymentRouterLLMD,
				&workercore.ModelDeploymentKVTransfer{Backend: "NIXL"},
				&workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}}, 0,
			),
		},
		{
			name: "pd-sglang-nvidia-mooncake-tcp",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineSGLang, "h20-8x", sglangImage,
				workercore.ModelDeploymentRouterSGLang,
				&workercore.ModelDeploymentKVTransfer{Backend: "Mooncake", Protocol: "TCP"}, nil, 0,
			),
		},
		{
			name: "pd-sglang-nvidia-mooncake-rdma",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineSGLang, "h20-8x", sglangImage,
				workercore.ModelDeploymentRouterSGLang,
				&workercore.ModelDeploymentKVTransfer{Backend: "Mooncake", Protocol: "RDMA"}, nil, 1,
			),
		},
		{
			name: "pd-sglang-nvidia-nixl-efa",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineSGLang, "h20-8x", sglangImage,
				workercore.ModelDeploymentRouterSGLang,
				&workercore.ModelDeploymentKVTransfer{Backend: "NIXL", Protocol: "EFA"}, nil, 1,
			),
		},
		{
			name: "pd-sglang-ascend-hixl",
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineSGLang, "atlas-910b", sglangImage,
				workercore.ModelDeploymentRouterSGLang,
				&workercore.ModelDeploymentKVTransfer{Backend: "HIXL"}, nil, 0,
			),
		},
		{
			name:    "pd-sglang-unrouted-kvcache",
			objects: goldenKVCacheFixtures("shared-kv", "TCP"),
			md: newGoldenPDDeployment(
				"qwen", modelID, workercore.ModelDeploymentEngineSGLang, "h20-8x", sglangImage, "",
				nil, &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}}, 0,
			),
		},

		// 4. Multimodal Encode + Prefill + Decode (EPD) (3 scenarios)
		{
			name: "epd-vllm-nvidia-nixl-network",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen-vl"},
				Spec: workercore.ModelDeploymentSpec{
					Model:      workercore.ModelDeploymentModel{Name: vlModelID},
					Engine:     workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Router:     &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
					KVTransfer: &workercore.ModelDeploymentKVTransfer{Backend: "NIXL"},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "encode",
							Kind:         workercore.ModelDeploymentRoleKindEncode,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "prefill",
							Kind:         workercore.ModelDeploymentRoleKindPrefill,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "decode",
							Kind:         workercore.ModelDeploymentRoleKindDecode,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "epd-vllm-ascend-shared-pvc",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen-vl"},
				Spec: workercore.ModelDeploymentSpec{
					Model:      workercore.ModelDeploymentModel{Name: vlModelID},
					Engine:     workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmAscendImage},
					Router:     &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
					KVTransfer: &workercore.ModelDeploymentKVTransfer{Backend: "HIXL"},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "encode",
							Kind:         workercore.ModelDeploymentRoleKindEncode,
							InstanceType: "atlas-910b",
							Image:        vllmAscendImage,
							Replicas:     1,
							ReplicaSize:  1,
							AdditionalVolumes: []workercore.ModelDeploymentAdditionalVolume{
								{
									MountPath:             "/shared/encodings",
									PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "ec-cache"},
								},
							},
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "prefill",
							Kind:         workercore.ModelDeploymentRoleKindPrefill,
							InstanceType: "atlas-910b",
							Image:        vllmAscendImage,
							Replicas:     1,
							ReplicaSize:  1,
							AdditionalVolumes: []workercore.ModelDeploymentAdditionalVolume{
								{
									MountPath:             "/shared/encodings",
									PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "ec-cache"},
								},
							},
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "decode",
							Kind:         workercore.ModelDeploymentRoleKindDecode,
							InstanceType: "atlas-910b",
							Image:        vllmAscendImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name: "epd-sglang-nvidia",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen-vl"},
				Spec: workercore.ModelDeploymentSpec{
					Model:      workercore.ModelDeploymentModel{Name: vlModelID},
					Engine:     workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineSGLang, Image: sglangImage},
					Router:     &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGLang},
					KVTransfer: &workercore.ModelDeploymentKVTransfer{Backend: "Mooncake", Protocol: "TCP"},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "encode",
							Kind:         workercore.ModelDeploymentRoleKindEncode,
							InstanceType: "h20-8x",
							Image:        sglangImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "prefill",
							Kind:         workercore.ModelDeploymentRoleKindPrefill,
							InstanceType: "h20-8x",
							Image:        sglangImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "decode",
							Kind:         workercore.ModelDeploymentRoleKindDecode,
							InstanceType: "h20-8x",
							Image:        sglangImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},

		// 5. ModelArtifact Weight Delivery (3 scenarios)
		{
			name:            "artifact-node-and-engine",
			objects:         goldenModelArtifactFixtures(),
			deliveryMode:    settings.ModelArtifactDeliveryNode,
			secondPassMode:  settings.ModelArtifactDeliveryEngine,
			secondPassScale: map[string]int32{"main": 2},
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model: workercore.ModelDeploymentModel{
						Name:        modelID,
						ArtifactRef: &core.LocalObjectReference{Name: "qwen-72b"},
					},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name:             "artifact-remote-nixl-and-hixl",
			objects:          goldenModelArtifactFixtures(),
			deliveryMode:     settings.ModelArtifactDeliveryNode,
			remotePolicy:     settings.ModelArtifactRemoteDeliveryPrefer,
			sourceReady:      true,
			qualifyAllTuples: true,
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model: workercore.ModelDeploymentModel{
						Name:        modelID,
						ArtifactRef: &core.LocalObjectReference{Name: "qwen-72b"},
					},
					Engine:     workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					KVTransfer: &workercore.ModelDeploymentKVTransfer{Protocol: "EFA"},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "nvidia-ucx",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:         "nvidia-efa",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
						{
							Name:         "ascend-hixl",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "atlas-910b",
							Image:        vllmAscendImage,
							Replicas:     1,
							ReplicaSize:  1,
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
		{
			name:             "artifact-modelexpress-d2d",
			objects:          goldenModelArtifactFixtures(),
			deliveryMode:     settings.ModelArtifactDeliveryNode,
			remotePolicy:     settings.ModelArtifactRemoteDeliveryPrefer,
			sourceReady:      true,
			qualifyAllTuples: true,
			markPodsReady:    true,
			statusMutate: func(md *workercore.ModelDeployment) {
				md.Status.Model = &workercore.ModelDeploymentModelStatus{
					Artifact:       "qwen-72b",
					Revision:       "0123456789abcdef0123456789abcdef01234567",
					ManifestDigest: "sha256:" + strings.Repeat("a", 64),
					Delivery:       workercore.ModelDeploymentModelDeliveryRemote,
					Remote: &workercore.ModelDeploymentModelRemoteStatus{
						Policy: workercore.ModelDeploymentRemotePolicyPrefer,
						Roles: []workercore.ModelDeploymentRemoteRoleStatus{
							{
								Role: "main",
								Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{
									{
										Ordinal:          0,
										Delivery:         workercore.ModelDeploymentModelDeliveryNode,
										Load:             workercore.ModelDeploymentRemoteLoadStateLoaded,
										Reason:           modelDeploymentModelTransferReasonLoadFailed,
										FallbackAttempts: 1,
									},
								},
							},
						},
					},
				}
			},
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "qwen"},
				Spec: workercore.ModelDeploymentSpec{
					Model: workercore.ModelDeploymentModel{
						Name:        modelID,
						ArtifactRef: &core.LocalObjectReference{Name: "qwen-72b"},
					},
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: vllmImage},
					Roles: []workercore.ModelDeploymentRole{
						{
							Name:         "main",
							Kind:         workercore.ModelDeploymentRoleKindServer,
							InstanceType: "h20-8x",
							Image:        vllmImage,
							Replicas:     2,
							ReplicaSize:  1,
							ExtraArgs:    []string{"--load-format", "modelexpress"},
							Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					},
				},
			},
		},
	}
}

func newGoldenPDDeployment(
	name, modelID, engineName, instanceType, image, routerName string,
	kvTransfer *workercore.ModelDeploymentKVTransfer,
	kvCache *workercore.ModelDeploymentKVCache,
	interfaces int64,
) *workercore.ModelDeployment {
	qty1 := resource.MustParse("1")
	res := &workercore.ModelDeploymentRoleResources{
		Accelerator: ptr.To(qty1),
	}
	if interfaces > 0 {
		res.Interface = ptr.To(*resource.NewQuantity(interfaces, resource.DecimalSI))
	}
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: name},
		Spec: workercore.ModelDeploymentSpec{
			Model:      workercore.ModelDeploymentModel{Name: modelID},
			Engine:     workercore.ModelDeploymentEngine{Name: engineName, Image: image},
			KVTransfer: kvTransfer,
			KVCache:    kvCache,
			Roles: []workercore.ModelDeploymentRole{
				{
					Name:         "prefill",
					Kind:         workercore.ModelDeploymentRoleKindPrefill,
					InstanceType: instanceType,
					Image:        image,
					Replicas:     1,
					ReplicaSize:  1,
					Resources:    res.DeepCopy(),
				},
				{
					Name:         "decode",
					Kind:         workercore.ModelDeploymentRoleKindDecode,
					InstanceType: instanceType,
					Image:        image,
					Replicas:     1,
					ReplicaSize:  1,
					Resources:    res.DeepCopy(),
				},
			},
		},
	}
	if routerName != "" {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: routerName}
	}
	return md
}
