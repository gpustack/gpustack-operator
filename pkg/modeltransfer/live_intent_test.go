package modeltransfer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

func TestValidateLiveIntentWithAcceleratorType(t *testing.T) {
	const (
		gpuKey          = "nvidia-h100-80gb-hbm3"
		gpuOS           = "linux"
		gpuArch         = "amd64"
		profileZone     = "fnv64-zone"
		flavorBase      = "gpustack--generic--nvidia-h100-80gb-hbm3-linux-amd64-8d"
		testManifestHex = "7777777777777777777777777777777777777777777777777777777777777777"
		digest          = "sha256:" + testManifestHex
	)

	flavorZone := nodefeature.TopologyQualifiedFlavorName(flavorBase, profileZone)
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: flavorZone,
			Labels: nodefeature.PoolScheduleLabels(
				true, false, "generic", gpuKey, gpuOS, gpuArch,
			),
		},
	}
	systemmeta.NoteResource(rf, modelstore.ResourceFlavorResourceType, nil)

	at := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "h100-type"},
		Spec: workercore.AcceleratorTypeSpec{
			AcceleratorGroup: gpuKey,
			GeneralGroup:     "generic",
			OS:               gpuOS,
			Arch:             gpuArch,
		},
	}

	node1 := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"pool":                                "source",
				nodefeature.TopologyProfileLabel:      profileZone,
				core.LabelOSStable:                    gpuOS,
				core.LabelArchStable:                  gpuArch,
				nodefeature.NodeAcceleratableLabelKey: "true",
				nodefeature.AcceleratableFeatureLabelPrefix + gpuKey:            "true",
				nodefeature.AcceleratableFeatureLabelPrefix + gpuKey + ".count": "8",
			},
		},
	}

	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "ds-uid", Labels: labels},
		Spec:       apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "source-1", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}}},
		Spec:       core.PodSpec{NodeName: node1.Name, Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}},
	}

	cpuSetting := &gpustack.Setting{
		ObjectMeta: meta.ObjectMeta{Name: settings.InstanceTypeAwareCPUManufacturer.Name(), Namespace: "operator"},
		Status:     gpustack.SettingStatus{Value: "false"},
	}

	kube := kubefake.NewSimpleClientset(node1, ds, pod, cpuSetting)

	pf := &workercore.ModelPrefetch{
		ObjectMeta: meta.ObjectMeta{Name: "prefetch", Namespace: "tenant", UID: "prefetch-uid", Generation: 1},
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "grant"},
			Source:      &workercore.ModelPrefetchSource{},
			Placement: &workercore.ModelPrefetchPlacement{
				AcceleratorTypes: []string{"h100-type"},
			},
		},
	}
	model := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Name: "model", Namespace: "tenant", UID: "artifact-uid", Generation: 1},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"}},
		},
		Status: workercore.ModelArtifactStatus{
			ObservedGeneration: 1,
			Resolved: &workercore.ModelArtifactResolved{
				ManifestDigest: digest,
				SizeBytes:      1024,
			},
			Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}},
		},
	}
	binding := &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid"},
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store"}},
			SourceMemory: &workercore.ModelStoreBindingSourceMemory{
				Bytes:         resource.MustParse("1Gi"),
				Registrations: 4,
				Sessions:      4,
			},
		},
	}
	modelStore := &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "store"},
		Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}},
		},
	}

	client := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node1, rf, at, pf, model, binding, modelStore).
		Build()

	cfg := &Config{
		Client:          kube,
		WorkerNamespace: "operator",
		WorkerID:        "pod-uid",
		NodeName:        "node-1",
		VendorScope:     "nvidia",
	}
	cfg.Intents = &kubernetesIntentSource{config: cfg, client: client}

	liveIntent := broker.LiveIntent{
		Namespace:        "tenant",
		IntentName:       pf.Name,
		IntentUID:        string(pf.UID),
		IntentGeneration: pf.Generation,
		BindingName:      binding.Name,
		BindingUID:       string(binding.UID),
		ArtifactName:     model.Name,
		ArtifactUID:      string(model.UID),
		SourceGeneration: model.Generation,
		Digest:           digest,
		NodeName:         cfg.NodeName,
		WorkerID:         cfg.WorkerID,
	}

	ctx := context.Background()

	t.Run("valid live intent with accelerator type succeeds", func(t *testing.T) {
		err := cfg.validateLiveIntentUncached(ctx, liveIntent)
		require.NoError(t, err)
	})

	t.Run("prefetch targeting different accelerator type leaves node outside intent", func(t *testing.T) {
		pfOther := pf.DeepCopy()
		pfOther.Name = "prefetch-other"
		pfOther.UID = "prefetch-other-uid"
		pfOther.ResourceVersion = ""
		pfOther.Spec.Placement = &workercore.ModelPrefetchPlacement{
			AcceleratorTypes: []string{"other-type"},
		}
		require.NoError(t, client.Create(ctx, pfOther))
		intentOther := liveIntent
		intentOther.IntentName = pfOther.Name
		intentOther.IntentUID = string(pfOther.UID)
		err := cfg.validateLiveIntentUncached(ctx, intentOther)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source node is outside the intent")
	})
}
