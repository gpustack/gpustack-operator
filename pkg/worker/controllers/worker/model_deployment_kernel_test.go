package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuepod "sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

func newKernelTestDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	qty1 := resource.MustParse("1")
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       "qwen",
			UID:        "md-qwen-uid",
			Generation: 1,
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name:    workercore.ModelDeploymentEngineVLLM,
				Image:   "vllm/vllm-openai:v0.19.0",
				Version: "0.19.0",
			},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name:            "decode",
					Kind:            workercore.ModelDeploymentRoleKindServer,
					AcceleratorType: "h20-8x",
					Image:           "vllm/vllm-openai:v0.19.0",
					Replicas:        1,
					ReplicaSize:     1,
					Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
			},
		},
	}
	for _, fn := range mutate {
		fn(md)
	}
	return md
}

func newNodeStoreKernelArtifact(name, namespace, manifestChar, dispatchChar string, mutate ...func(*workercore.ModelKernelArtifact)) *workercore.ModelKernelArtifact {
	manifestDigest := "sha256:" + strings.Repeat(manifestChar, 64)
	dispatchDigest := "sha256:" + strings.Repeat(dispatchChar, 64)
	now := meta.Now()
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  namespace,
			Name:       name,
			UID:        types.UID("mka-uid-" + name),
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         manifestDigest,
					DispatchManifestDigest: dispatchDigest,
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-0",
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-forger",
						UID:  "forger-uid-1",
					},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.19.0",
				Manufacturer:  nodefeature.ManufacturerNVIDIA,
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vLLM/0.19.0/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
		Status: workercore.ModelKernelArtifactStatus{
			ObservedGeneration: 1,
			Resolved: &workercore.ModelKernelArtifactResolved{
				ManifestDigest:         manifestDigest,
				DispatchManifestDigest: dispatchDigest,
				FileCount:              3,
				SizeBytes:              4096,
				ResolvedTime:           now,
				LastValidatedTime:      &now,
				DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
			},
			Nodes: &workercore.ModelArtifactNodes{Ready: 1},
		},
	}
	ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved, "ready")
	for _, fn := range mutate {
		fn(mka)
	}
	return mka
}

func newKernelTestFakeClient(objs ...ctrlcli.Object) ctrlcli.Client {
	var seq int
	all := make([]ctrlcli.Object, 0, 2+len(objs))
	all = append(all,
		newRenderAcceleratorType(),
		&nodev1.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "nvidia"}, Handler: "nvidia"},
	)
	all = append(all, objs...)
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(
			&workercore.ModelDeployment{},
			&workercore.ModelKernelArtifact{},
			&workercore.NodeModelStore{},
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
		WithObjects(all...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				if pod, ok := obj.(*core.Pod); ok {
					if pod.Name == "" && pod.GenerateName != "" {
						seq++
						pod.Name = fmt.Sprintf("%s%02d", pod.GenerateName, seq)
					}
					if pod.UID == "" {
						pod.UID = types.UID("uid-" + pod.Name)
					}
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
}

func markKernelTestPodReady(t *testing.T, cli ctrlcli.Client, pod *core.Pod, nodeName, podIP string) {
	t.Helper()
	var current core.Pod
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), &current))
	current.Spec.NodeName = nodeName
	require.NoError(t, cli.Update(context.Background(), &current))

	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), &current))
	current.Status.Phase = core.PodRunning
	current.Status.PodIP = podIP
	current.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	current.Status.ContainerStatuses = []core.ContainerStatus{{Name: modelDeploymentMainContainerName, Ready: true}}
	require.NoError(t, cli.Status().Update(context.Background(), &current))
	*pod = current
}

func findDeploymentCondition(md *workercore.ModelDeployment, condType string) *gpustack.Condition {
	for i := range md.Status.Conditions {
		if md.Status.Conditions[i].Type == condType {
			return &md.Status.Conditions[i]
		}
	}
	return nil
}

func TestModelDeploymentKernel_RenderAndZeroChurnParity(t *testing.T) {
	at := newRenderAcceleratorType()
	baseMD := newKernelTestDeployment()

	basePod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      baseMD,
		Role:            &baseMD.Spec.Roles[0],
		AcceleratorType: at,
	})
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(modelDeploymentRenderVersion), basePod.Annotations[modelDeploymentRenderVersionAnnotation])
	assert.Equal(t, 7, modelDeploymentRenderVersion, "modelDeploymentRenderVersion must remain 7")
	assert.False(t, modelDeploymentPodHasVolume(basePod, modelDeploymentKernelVolumeName))
	_, hasKernelEnv := envValue(basePod, modelDeploymentKernelArtifactDirEnv)
	assert.False(t, hasKernelEnv)

	baseCompilePath, err := BuildModelDeploymentCompileCacheSubPath(ModelDeploymentCompileCacheInput{
		Deployment:      baseMD,
		Role:            &baseMD.Spec.Roles[0],
		AcceleratorType: at,
	})
	require.NoError(t, err)

	// 1. NodeStore CSI delivery
	nodeStoreMD := baseMD.DeepCopy()
	nodeStoreMD.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
	digest1 := "sha256:" + strings.Repeat("1", 64)
	dispatch1 := "sha256:" + strings.Repeat("a", 64)
	nodeStorePod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      nodeStoreMD,
		Role:            &nodeStoreMD.Spec.Roles[0],
		AcceleratorType: at,
		KernelArtifact: &ModelDeploymentKernelRender{
			Delivery:               workercore.ModelDeploymentKernelDeliveryNode,
			ArtifactName:           "mk-gen-1",
			ArtifactUID:            "mka-uid-1",
			ManifestDigest:         digest1,
			DispatchManifestDigest: dispatch1,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t,
		basePod.Annotations[modelDeploymentPodSpecHashAnnotation],
		nodeStorePod.Annotations[modelDeploymentPodSpecHashAnnotation],
		"setting kernelArtifactRef must change pod-spec-hash on the targeted role",
	)
	dirVal, ok := envValue(nodeStorePod, modelDeploymentKernelArtifactDirEnv)
	require.True(t, ok, "serving container must carry GPUSTACK_KERNEL_ARTIFACT_DIR")
	assert.Equal(t, ModelDeploymentKernelMountPath, dirVal)

	var kernelVol *core.Volume
	for i := range nodeStorePod.Spec.Volumes {
		if nodeStorePod.Spec.Volumes[i].Name == modelDeploymentKernelVolumeName {
			kernelVol = &nodeStorePod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, kernelVol, "Pod must carry gpustack-kernels volume")
	require.NotNil(t, kernelVol.CSI)
	assert.Equal(t, modelstore.DriverName, kernelVol.CSI.Driver)
	assert.Equal(t, ptr.To(true), kernelVol.CSI.ReadOnly)
	assert.Nil(t, kernelVol.CSI.NodePublishSecretRef)
	assert.Equal(t, map[string]string{
		modelstore.VolumeAttrArtifactKind:   modelstore.ArtifactKindKernel,
		modelstore.VolumeAttrArtifact:       "mk-gen-1",
		modelstore.VolumeAttrArtifactUID:    "mka-uid-1",
		modelstore.VolumeAttrManifestDigest: digest1,
	}, kernelVol.CSI.VolumeAttributes)

	var kernelMount *core.VolumeMount
	for i := range nodeStorePod.Spec.Containers[0].VolumeMounts {
		if nodeStorePod.Spec.Containers[0].VolumeMounts[i].Name == modelDeploymentKernelVolumeName {
			kernelMount = &nodeStorePod.Spec.Containers[0].VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, kernelMount)
	assert.Equal(t, ModelDeploymentKernelMountPath, kernelMount.MountPath)
	assert.True(t, kernelMount.ReadOnly)

	// Compile-cache isolation across generations
	gen1CompilePath, err := BuildModelDeploymentCompileCacheSubPath(ModelDeploymentCompileCacheInput{
		Deployment:           nodeStoreMD,
		Role:                 &nodeStoreMD.Spec.Roles[0],
		AcceleratorType:      at,
		KernelManifestDigest: digest1,
	})
	require.NoError(t, err)
	assert.NotEqual(t, baseCompilePath, gen1CompilePath,
		"kernel manifest digest must isolate compilation cache subdirectory from stock")

	digest2 := "sha256:" + strings.Repeat("2", 64)
	gen2CompilePath, err := BuildModelDeploymentCompileCacheSubPath(ModelDeploymentCompileCacheInput{
		Deployment:           nodeStoreMD,
		Role:                 &nodeStoreMD.Spec.Roles[0],
		AcceleratorType:      at,
		KernelManifestDigest: digest2,
	})
	require.NoError(t, err)
	assert.NotEqual(t, gen1CompilePath, gen2CompilePath,
		"advancing kernel manifest digest must isolate compilation cache subdirectory per generation")

	nodeStoreGen2Pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      nodeStoreMD,
		Role:            &nodeStoreMD.Spec.Roles[0],
		AcceleratorType: at,
		KernelArtifact: &ModelDeploymentKernelRender{
			Delivery:               workercore.ModelDeploymentKernelDeliveryNode,
			ArtifactName:           "mk-gen-1",
			ArtifactUID:            "mka-uid-1",
			ManifestDigest:         digest2,
			DispatchManifestDigest: dispatch1,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t,
		nodeStorePod.Annotations[modelDeploymentPodSpecHashAnnotation],
		nodeStoreGen2Pod.Annotations[modelDeploymentPodSpecHashAnnotation],
		"changing resolved kernel manifestDigest must move bindingHash and pod-spec-hash",
	)

	// 2. HuggingFace CSI delivery with nodePublishSecretRef
	hfPod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      nodeStoreMD,
		Role:            &nodeStoreMD.Spec.Roles[0],
		AcceleratorType: at,
		KernelArtifact: &ModelDeploymentKernelRender{
			Delivery:       workercore.ModelDeploymentKernelDeliveryNode,
			ArtifactName:   "mk-hf",
			ArtifactUID:    "mka-uid-hf",
			ManifestDigest: digest1,
			SecretName:     "hf-kernel-token",
		},
	})
	require.NoError(t, err)
	var hfVol *core.Volume
	for i := range hfPod.Spec.Volumes {
		if hfPod.Spec.Volumes[i].Name == modelDeploymentKernelVolumeName {
			hfVol = &hfPod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, hfVol)
	require.NotNil(t, hfVol.CSI)
	require.NotNil(t, hfVol.CSI.NodePublishSecretRef)
	assert.Equal(t, "hf-kernel-token", hfVol.CSI.NodePublishSecretRef.Name)

	// 3. OCI Image delivery
	imgRef := "registry.example.com/kernels/qwen-rmsnorm@sha256:" + strings.Repeat("c", 64)
	imgPod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      nodeStoreMD,
		Role:            &nodeStoreMD.Spec.Roles[0],
		AcceleratorType: at,
		KernelArtifact: &ModelDeploymentKernelRender{
			Delivery:       workercore.ModelDeploymentKernelDeliveryImage,
			ArtifactName:   "mk-img",
			ArtifactUID:    "mka-uid-img",
			ImageReference: imgRef,
		},
	})
	require.NoError(t, err)
	var imgVol *core.Volume
	for i := range imgPod.Spec.Volumes {
		if imgPod.Spec.Volumes[i].Name == modelDeploymentKernelVolumeName {
			imgVol = &imgPod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, imgVol)
	require.NotNil(t, imgVol.Image)
	assert.Equal(t, imgRef, imgVol.Image.Reference)
	assert.Equal(t, core.PullIfNotPresent, imgVol.Image.PullPolicy)
}

func TestModelDeploymentKernel_KernelsReadyConditionsAndHold(t *testing.T) {
	csiDriver := &storagev1.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}
	seedStore := func(digest string) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-0"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: digest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
	}

	t.Run("NotApplicable when no role sets kernelArtifactRef", func(t *testing.T) {
		md := newKernelTestDeployment()
		cli := newKernelTestFakeClient(md)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionTrue, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelsNotApplicable, cond.Reason)
		require.Len(t, got.Status.Roles, 1)
		assert.Nil(t, got.Status.Roles[0].KernelArtifact)
	})

	t.Run("KernelsReady True for resolved nodeStore and status echoed", func(t *testing.T) {
		mka := newNodeStoreKernelArtifact("mk-gen-1", "team-a", "1", "a")
		md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
		})
		cli := newKernelTestFakeClient(md, mka, csiDriver, seedStore(mka.Spec.Source.NodeStore.ManifestDigest))
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1, "replica must be created when KernelsReady is True")
		assert.True(t, modelDeploymentPodHasVolume(&pods.Items[0], modelDeploymentKernelVolumeName))

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionTrue, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelsReady, cond.Reason)

		require.Len(t, got.Status.Roles, 1)
		require.NotNil(t, got.Status.Roles[0].KernelArtifact)
		assert.Equal(t, "mk-gen-1", got.Status.Roles[0].KernelArtifact.Artifact)
		assert.Equal(t, workercore.ModelDeploymentKernelDeliveryNode, got.Status.Roles[0].KernelArtifact.Delivery)
		assert.Equal(t, mka.Spec.Source.NodeStore.ManifestDigest, got.Status.Roles[0].KernelArtifact.ManifestDigest)
		assert.Equal(t, mka.Spec.Source.NodeStore.DispatchManifestDigest, got.Status.Roles[0].KernelArtifact.DispatchManifestDigest)
		assert.False(t, got.Status.Roles[0].KernelArtifact.Quarantined)
	})

	t.Run("KernelArtifactNotFound holds affected role creation while unaffected role creates normally", func(t *testing.T) {
		qty1 := resource.MustParse("1")
		md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name:            "prefill",
					Kind:            workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "h20-8x",
					Image:           "vllm/vllm-openai:v0.19.0",
					Replicas:        1,
					ReplicaSize:     1,
					Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
				{
					Name:              "decode",
					Kind:              workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType:   "h20-8x",
					Image:             "vllm/vllm-openai:v0.19.0",
					Replicas:          1,
					ReplicaSize:       1,
					Resources:         &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					KernelArtifactRef: &core.LocalObjectReference{Name: "mk-missing"},
				},
			}
		})
		cli := newKernelTestFakeClient(md, csiDriver)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1, "only unaffected prefill role should create its Pod")
		assert.Equal(t, "prefill", modelDeploymentPodRole(&pods.Items[0]))

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionFalse, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelArtifactNotFound, cond.Reason)
	})

	t.Run("KernelArtifactNotResolved holds new creation and leaves running Pods untouched", func(t *testing.T) {
		md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Replicas = 1
		})
		cli := newKernelTestFakeClient(md, csiDriver)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		// First converge stock Pod.
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var beforePods core.PodList
		require.NoError(t, cli.List(context.Background(), &beforePods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, beforePods.Items, 1)
		runningUID := beforePods.Items[0].UID
		wl := admit(composeReplicaGroup(md, "decode", 0, []*core.Pod{&beforePods.Items[0]}))
		require.NoError(t, cli.Create(context.Background(), wl))

		// Now create an unresolved ModelKernelArtifact and scale replicas to 2 while referencing it.
		unresolved := newNodeStoreKernelArtifact("mk-unresolved", "team-a", "2", "b", func(mka *workercore.ModelKernelArtifact) {
			mka.Status.Resolved = nil
			mka.Status.Conditions = nil
			ModelKernelArtifactConditionResolved.Unknown(mka, workercore.ModelKernelArtifactReasonResolving, "waiting for seed node")
		})
		require.NoError(t, cli.Create(context.Background(), unresolved))

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
		liveMD.Spec.Roles[0].Replicas = 2
		liveMD.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-unresolved"}
		require.NoError(t, cli.Update(context.Background(), &liveMD))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var afterPods core.PodList
		require.NoError(t, cli.List(context.Background(), &afterPods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, afterPods.Items, 1, "new replica creation must be held and running Pod must remain")
		assert.Equal(t, runningUID, afterPods.Items[0].UID)
		assert.Nil(t, afterPods.Items[0].DeletionTimestamp)

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionFalse, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelArtifactNotResolved, cond.Reason)
	})

	t.Run("KernelArtifactQuarantined holds rollout and echoes quarantined status", func(t *testing.T) {
		md := newKernelTestDeployment()
		cli := newKernelTestFakeClient(md, csiDriver)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		runningUID := pods.Items[0].UID
		require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(md, "decode", 0, []*core.Pod{&pods.Items[0]}))))

		quarantinedMKA := newNodeStoreKernelArtifact("mk-bad", "team-a", "3", "c", func(mka *workercore.ModelKernelArtifact) {
			mka.Status.Quarantined = true
			mka.Status.QuarantineReason = "CanaryTimeout"
			mka.Status.QuarantineMessage = "canary failed readiness within failureTimeoutSeconds"
		})
		require.NoError(t, cli.Create(context.Background(), quarantinedMKA))
		require.NoError(t, cli.Create(context.Background(), seedStore(quarantinedMKA.Spec.Source.NodeStore.ManifestDigest)))

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
		liveMD.Spec.Roles[0].Replicas = 2
		liveMD.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-bad"}
		require.NoError(t, cli.Update(context.Background(), &liveMD))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var afterPods core.PodList
		require.NoError(t, cli.List(context.Background(), &afterPods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, afterPods.Items, 1, "quarantined artifact must hold scale-up and keep running Pod untouched")
		assert.Equal(t, runningUID, afterPods.Items[0].UID)
		assert.Nil(t, afterPods.Items[0].DeletionTimestamp)

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionFalse, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelArtifactQuarantined, cond.Reason)
		require.NotNil(t, got.Status.Roles[0].KernelArtifact)
		assert.True(t, got.Status.Roles[0].KernelArtifact.Quarantined)
	})

	t.Run("NodeDeliveryUnavailable when CSIDriver is missing for nodeStore", func(t *testing.T) {
		mka := newNodeStoreKernelArtifact("mk-gen-1", "team-a", "4", "d")
		md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
		})
		// Deliberately omit CSIDriver.
		cli := newKernelTestFakeClient(md, mka, seedStore(mka.Spec.Source.NodeStore.ManifestDigest))
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		assert.Empty(t, pods.Items, "must hold creation while CSIDriver is missing")

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionFalse, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelNodeDeliveryUnavailable, cond.Reason)
	})

	t.Run("PeerSyncDisabled when multi-node consumer NodeModelStore disables peer sync", func(t *testing.T) {
		mka := newNodeStoreKernelArtifact("mk-gen-1", "team-a", "5", "e")
		md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Replicas = 2
			md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
		})
		consumerStore := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(false)},
		}
		cli := newKernelTestFakeClient(md, mka, csiDriver, seedStore(mka.Spec.Source.NodeStore.ManifestDigest), consumerStore)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		assert.Empty(t, pods.Items, "must hold creation when PeerSync is disabled on consumer node")

		var got workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		cond := findDeploymentCondition(&got, workercore.ModelDeploymentConditionTypeKernelsReady)
		require.NotNil(t, cond)
		assert.Equal(t, meta.ConditionFalse, cond.Status)
		assert.Equal(t, workercore.ModelDeploymentReasonKernelPeerSyncDisabled, cond.Reason)
	})
}

func TestModelDeploymentKernel_WatchMapper(t *testing.T) {
	mdMatch := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "qwen-match"
		md.Spec.Roles = append(md.Spec.Roles, md.Spec.Roles[0])
		md.Spec.Roles[0].Name = "prefill"
		md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-shared"}
		md.Spec.Roles[1].Name = "decode"
		md.Spec.Roles[1].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-shared"}
	})
	mdUnrelated := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "qwen-other"
	})
	mdOtherNS := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
		md.Namespace = "team-b"
		md.Name = "qwen-team-b"
		md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-shared"}
	})
	mka := newNodeStoreKernelArtifact("mk-shared", "team-a", "6", "f")

	cli := newKernelTestFakeClient(mdMatch, mdUnrelated, mdOtherNS, mka)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	reqs := r.mapModelDeploymentKernelArtifact(context.Background(), mka)
	assert.Equal(t, []ctrlreconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "qwen-match"},
	}}, reqs)
}

func TestModelDeploymentKernel_CanaryReplacementSlotRequireReadiness(t *testing.T) {
	csiDriver := &storagev1.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}
	mka := newNodeStoreKernelArtifact("mk-gen-1", "team-a", "7", "8")
	seedStore := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-0"},
		Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: mka.Spec.Source.NodeStore.ManifestDigest,
				State:  workercore.NodeModelStoreModelStateReady,
			}},
		},
	}

	var sentinelDispatchDigest atomic.Value
	var sentinelLoadErr atomic.Value
	var sentinelLoadedCount atomic.Int32
	sentinelDispatchDigest.Store("")
	sentinelLoadErr.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/gpustack/v1/ready":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ready":   true,
				"boot_id": "boot-canary-1",
			})
		case "/gpustack/v1/runtime_config":
			count := int(sentinelLoadedCount.Load())
			keys := make([]string, 0, count)
			for i := range count {
				keys = append(keys, fmt.Sprintf("vLLM/0.19.0/nvidia/sm_90a/rmsnorm/%016x", i+1))
			}
			payload := map[string]any{
				"engine":                   "vllm",
				"boot_id":                  "boot-canary-1",
				"ready":                    true,
				"paused":                   false,
				"loaded_kernel_keys":       keys,
				"dispatch_manifest_digest": sentinelDispatchDigest.Load().(string),
				"kernel_load_error":        sentinelLoadErr.Load().(string),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	cli := newKernelTestFakeClient(md, mka, csiDriver, seedStore)
	r := &ModelDeploymentReconciler{
		Client:    cli,
		APIReader: cli,
		Recorder:  ctrlrecord.NewFakeRecorder(64),
		runtimeControlTarget: func(context.Context, *workercore.ModelDeployment, *workercore.ModelDeploymentRole, *core.Pod) (runtimecontrol.Target, error) {
			return runtimecontrol.Target{Scheme: "http", Host: host, Port: port}, nil
		},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

	// Initial convergence of 2 stock replicas (ordinal 0 and ordinal 1).
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var initialPods core.PodList
	require.NoError(t, cli.List(context.Background(), &initialPods, ctrlcli.InNamespace(md.Namespace)))
	require.Len(t, initialPods.Items, 2)

	var stockOrd0UID, stockOrd1UID types.UID
	for i := range initialPods.Items {
		pod := initialPods.Items[i].DeepCopy()
		markKernelTestPodReady(t, cli, pod, "node-0", host)
		ord, ok := modelDeploymentPodOrdinal(pod)
		require.True(t, ok)
		if ord == 0 {
			stockOrd0UID = pod.UID
		} else {
			stockOrd1UID = pod.UID
		}
		require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(md, "decode", ord, []*core.Pod{pod}))))
	}

	// Promote kernelArtifactRef -> mk-gen-1.
	var liveMD workercore.ModelDeployment
	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	liveMD.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "mk-gen-1"}
	liveMD.Generation++
	require.NoError(t, cli.Update(context.Background(), &liveMD))

	// Pass 1: Selects highest ordinal (ordinal 1) as canary, records RequireReadiness=true, deletes stock ordinal 1.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	slots := modelDeploymentReplacementSlotsOf(&liveMD)
	slot, held := slots.slotFor("decode")
	require.True(t, held, "decode role must hold a replacement slot")
	assert.Equal(t, 1, slot.Ordinal)
	assert.True(t, slot.RequireReadiness, "kernel-delta replacement slot must set RequireReadiness=true")
	assert.Equal(t, mka.Spec.Source.NodeStore.DispatchManifestDigest, slot.DispatchManifestDigest)

	// Pass 2: Vacant ordinal 1 creates canary replacement Pod (still NotReady). Admit its Kueue Workload!
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var afterCreatePods core.PodList
	require.NoError(t, cli.List(context.Background(), &afterCreatePods, ctrlcli.InNamespace(md.Namespace)))
	require.Len(t, afterCreatePods.Items, 2)
	var canaryPod *core.Pod
	for i := range afterCreatePods.Items {
		ord, _ := modelDeploymentPodOrdinal(&afterCreatePods.Items[i])
		if ord == 0 {
			assert.Equal(t, stockOrd0UID, afterCreatePods.Items[i].UID, "sibling replica 0 must stay untouched")
		} else {
			assert.NotEqual(t, stockOrd1UID, afterCreatePods.Items[i].UID)
			canaryPod = afterCreatePods.Items[i].DeepCopy()
		}
	}
	require.NotNil(t, canaryPod)
	canaryWL := admit(composeReplicaGroup(&liveMD, "decode", 1, []*core.Pod{canaryPod}))
	canaryWL.UID = types.UID("wl-canary-ord-1")
	require.NoError(t, cli.Create(context.Background(), canaryWL))

	// Pass 3: Canary Workload IS Admitted, but PodReady is still False -> slot MUST NOT clear!
	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter, 0*res.RequeueAfter, "RequireReadiness wait must schedule a requeue")

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	slot, held = modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, held, "admitted but unready canary Pod must NOT clear RequireReadiness slot")
	assert.True(t, slot.RequireReadiness)

	// Verify sibling ordinal 0 is STILL untouched!
	var checkPods core.PodList
	require.NoError(t, cli.List(context.Background(), &checkPods, ctrlcli.InNamespace(md.Namespace)))
	require.True(t, slices.ContainsFunc(checkPods.Items, func(p core.Pod) bool {
		return p.UID == stockOrd0UID && p.DeletionTimestamp == nil
	}), "sibling replica 0 must never be torn down while canary is unready")

	// Pass 4: Mark canary Pod Ready=True, but sentinel reports wrong dispatch_manifest_digest / load error -> slot STILL holds!
	markKernelTestPodReady(t, cli, canaryPod, "node-0", host)
	sentinelDispatchDigest.Store("sha256:" + strings.Repeat("f", 64))
	sentinelLoadErr.Store("unsupported_kernel_artifact")

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	_, held = modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, held, "canary with kernel_load_error / mismatched dispatch digest must NOT clear slot")
	require.NoError(t, cli.List(context.Background(), &checkPods, ctrlcli.InNamespace(md.Namespace)))
	require.True(t, slices.ContainsFunc(checkPods.Items, func(p core.Pod) bool {
		return p.UID == stockOrd0UID && p.DeletionTimestamp == nil
	}), "sibling replica 0 must stay untouched when canary fails kernel load verification")

	// Pass 5: Sentinel confirms matching dispatch_manifest_digest and 2 loaded_kernel_keys -> slot clears and ordinal 0 rolls next!
	sentinelLoadErr.Store("")
	sentinelDispatchDigest.Store(mka.Spec.Source.NodeStore.DispatchManifestDigest)
	sentinelLoadedCount.Store(2)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	prePass5Slot, preHeld := modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, preHeld)
	renderedByRole, renderErr := r.renderModelDeploymentPods(context.Background(), &liveMD, nil, nil, nil)
	require.NoError(t, renderErr)
	admittedNow, whyNotAdmitted, admitErr := r.replacementAdmitted(context.Background(), &liveMD, prePass5Slot, renderedByRole["decode"][1])
	require.NoError(t, admitErr)
	require.True(t, admittedNow, "expected canary ordinal 1 to pass replacementAdmitted, got why=%q", whyNotAdmitted)

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	nextSlot, nextHeld := modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, nextHeld, "once canary ordinal 1 clears, ordinal 0 should begin replacement on the same pass")
	assert.Equal(t, 0, nextSlot.Ordinal, "sibling ordinal 0 only begins replacement after canary ordinal 1 passed readiness + runtime_config")
	assert.True(t, nextSlot.RequireReadiness)

	// Pass 6: Complete ordinal 0 replacement and verify status.roles[0].kernelArtifact.loadedKernels == 2.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var finalPods core.PodList
	require.NoError(t, cli.List(context.Background(), &finalPods, ctrlcli.InNamespace(md.Namespace)))
	require.Len(t, finalPods.Items, 2)
	for i := range finalPods.Items {
		ord, _ := modelDeploymentPodOrdinal(&finalPods.Items[i])
		if ord == 0 {
			p0 := finalPods.Items[i].DeepCopy()
			markKernelTestPodReady(t, cli, p0, "node-0", host)
			p0WL := admit(composeReplicaGroup(&liveMD, "decode", 0, []*core.Pod{p0}))
			p0WL.UID = types.UID("wl-canary-ord-0")
			require.NoError(t, cli.Create(context.Background(), p0WL))
		}
	}

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	_, stillHeld := modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	assert.False(t, stillHeld, "all replacement slots must clear once ordinal 0 also confirms kernels")
	require.NotNil(t, liveMD.Status.Roles[0].KernelArtifact)
	assert.Equal(t, int32(2), liveMD.Status.Roles[0].KernelArtifact.LoadedKernels)
}

func TestModelDeploymentKernel_NonKernelReplacementKeepsAdmissionOnlyBehavior(t *testing.T) {
	md := newKernelTestDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	cli := newKernelTestFakeClient(md)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(32)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var pods core.PodList
	require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
	require.Len(t, pods.Items, 2)
	for i := range pods.Items {
		ord, _ := modelDeploymentPodOrdinal(&pods.Items[i])
		require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(md, "decode", ord, []*core.Pod{&pods.Items[i]}))))
	}

	// Non-kernel spec edit.
	var liveMD workercore.ModelDeployment
	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	liveMD.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
	liveMD.Generation++
	require.NoError(t, cli.Update(context.Background(), &liveMD))

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	slot, held := modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, held)
	assert.Equal(t, 1, slot.Ordinal)
	assert.False(t, slot.RequireReadiness, "non-kernel spec edit must keep RequireReadiness=false")
	assert.Empty(t, slot.DispatchManifestDigest)

	// Create replacement ordinal 1 (leave PodReady False!) and admit its Workload.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
	for i := range pods.Items {
		ord, _ := modelDeploymentPodOrdinal(&pods.Items[i])
		if ord == 1 {
			require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(&liveMD, "decode", 1, []*core.Pod{&pods.Items[i]}))))
		}
	}

	// Slot 1 clears on Kueue Admission alone (even with PodReady=False) and immediately begins slot 0.
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMD))
	slot, held = modelDeploymentReplacementSlotsOf(&liveMD).slotFor("decode")
	require.True(t, held)
	assert.Equal(t, 0, slot.Ordinal, "non-kernel replacement clears ordinal 1 upon Kueue Admission without waiting for PodReady")
}
