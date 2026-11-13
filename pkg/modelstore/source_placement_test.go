package modelstore

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	testGPUKey      = "nvidia-h100-80gb-hbm3"
	testGPUOS       = "linux"
	testGPUArch     = "amd64"
	testGPUPerNode  = 8
	testCPUKey      = "generic"
	testCPUPerNode  = 16
	testProfileZone = "fnv64-zone"
	testProfileHost = "fnv64-host"
)

// The flavor names ExtractNodeFlavors derives for the test fixtures below: the hardware base,
// then the topology-qualified names the pools carry — a flavor in a pool is always
// profile-qualified, because the same hardware under two profiles publishes two flavors.
const testGPUFlavorBase = "gpustack--generic--nvidia-h100-80gb-hbm3-linux-amd64-8d"

var testGPUFlavorZone = nodefeature.TopologyQualifiedFlavorName(testGPUFlavorBase, testProfileZone)

// accelNode is a managed accelerated node as the feature pipeline stamps it: the
// acceleratable key with its .count sibling, os/arch, and a topology profile. The flavor names
// it derives are the production ones, never hand-invented.
func accelNode(name, profile string) core.Node {
	return core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{
		nodefeature.TopologyProfileLabel:                                    profile,
		core.LabelOSStable:                                                  testGPUOS,
		core.LabelArchStable:                                                testGPUArch,
		nodefeature.NodeAcceleratableLabelKey:                               "true",
		nodefeature.AcceleratableFeatureLabelPrefix + testGPUKey:            "true",
		nodefeature.AcceleratableFeatureLabelPrefix + testGPUKey + ".count": "8",
	}}}
}

// cpuNode is a managed CPU-only node.
func cpuNode(name string) core.Node {
	generalKey := nodefeature.GeneralFeatureLabelPrefix + testCPUKey
	return core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{
		nodefeature.TopologyProfileLabel: testProfileZone,
		core.LabelOSStable:               testGPUOS,
		core.LabelArchStable:             testGPUArch,
		generalKey:                       "true",
		generalKey + ".count":            "16",
	}}}
}

// notedFlavor is a ResourceFlavor as the NodeFlavor reconciler stamps it: the systemmeta
// resource mark the pool lookup filters on, and the schedule labels the type's spec derives —
// never a hand-invented label set.
func notedFlavor(name string, labels map[string]string) *kueue.ResourceFlavor {
	rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels}}
	systemmeta.NoteResource(rf, ResourceFlavorResourceType, nil)
	return rf
}

// gpuAcceleratorType is the accelerated AcceleratorType the GPU fixtures' flavors back.
func gpuAcceleratorType(name, generalGroup string) *workercore.AcceleratorType {
	return &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.AcceleratorTypeSpec{
			GeneralGroup:     generalGroup,
			AcceleratorGroup: testGPUKey,
			OS:               testGPUOS,
			Arch:             testGPUArch,
		},
	}
}

func sourcePrefetch(mutate func(pf *workercore.ModelPrefetch)) *workercore.ModelPrefetch {
	pf := &workercore.ModelPrefetch{}
	pf.Spec.ArtifactRef = workercore.ModelPrefetchArtifactReference{Name: "model"}
	if mutate != nil {
		mutate(pf)
	}
	return pf
}

func selectorPlacement(match map[string]string) *workercore.ModelPrefetchPlacement {
	return &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{MatchLabels: match}}
}

func deletionTime() *meta.Time {
	now := meta.Now()
	return &now
}

func TestPrefetchTargetNodesBySelector(t *testing.T) {
	gpu1, gpu2 := accelNode("b", testProfileZone), accelNode("a", testProfileZone)
	cpu := cpuNode("c")
	pf := sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = selectorPlacement(map[string]string{nodefeature.NodeAcceleratableLabelKey: "true"})
	})

	targets, err := PrefetchTargetNodes(pf, []core.Node{gpu1, gpu2, cpu}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, targets, "selector matches are the targets, sorted")
}

func TestPrefetchTargetNodesAmbiguousPlacementRefused(t *testing.T) {
	pf := sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			AcceleratorTypes: []string{"h100-8"},
			NodeSelector:     &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
		}
	})

	_, err := PrefetchTargetNodes(pf, nil, nil, nil)
	assert.ErrorIs(t, err, ErrPrefetchPlacementAmbiguous)
}

func TestDeploymentAcceleratorTypes(t *testing.T) {
	now := meta.Now()
	deps := []workercore.ModelDeployment{
		{
			ObjectMeta: meta.ObjectMeta{Name: "served-1", Namespace: "team-a"},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model-a"}},
				Roles: []workercore.ModelDeploymentRole{
					{AcceleratorType: "nvidia-h100"},
					{AcceleratorType: ""}, // CPU-only role skipped cleanly
					{AcceleratorType: "nvidia-a100"},
				},
			},
		},
		{
			ObjectMeta: meta.ObjectMeta{Name: "served-2", Namespace: "team-a"},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model-a"}},
				Roles: []workercore.ModelDeploymentRole{
					{AcceleratorType: "nvidia-h100"}, // duplicate
					{AcceleratorType: "amd-mi300"},
				},
			},
		},
		{
			ObjectMeta: meta.ObjectMeta{Name: "other-model", Namespace: "team-a"},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model-b"}},
				Roles: []workercore.ModelDeploymentRole{
					{AcceleratorType: "nvidia-l4"},
				},
			},
		},
		{
			ObjectMeta: meta.ObjectMeta{
				Name:              "deleting-dep",
				Namespace:         "team-a",
				DeletionTimestamp: &now,
				Finalizers:        []string{"test"},
			},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model-a"}},
				Roles: []workercore.ModelDeploymentRole{
					{AcceleratorType: "nvidia-t4"},
				},
			},
		},
	}

	got := DeploymentAcceleratorTypes(deps, "model-a")
	assert.Equal(t, []string{"amd-mi300", "nvidia-a100", "nvidia-h100"}, got,
		"collects sorted unique non-empty role.AcceleratorType, skipping CPU-only and deleted deployments")
}

func TestPrefetchTargetNodesDerivedFromOwnNamespaceDeployments(t *testing.T) {
	// The disk-only default: no placement at all derives the target set from the deployments
	// passed in — the caller's own namespace's slice, as both callers scope it — serving the
	// same artifact. A deployment of another artifact contributes nothing, an empty role name
	// contributes nothing, and a type resolving to no flavors contributes no targets.
	gpu1 := accelNode("gpu-1", testProfileZone)
	deps := []workercore.ModelDeployment{
		{
			ObjectMeta: meta.ObjectMeta{Name: "served", Namespace: "team-a"},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model"}},
				Roles: []workercore.ModelDeploymentRole{
					{AcceleratorType: "h100-8"}, {AcceleratorType: ""}, {AcceleratorType: "mi300-8"},
				},
			},
		},
		{
			ObjectMeta: meta.ObjectMeta{Name: "other", Namespace: "team-a"},
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "other"}},
				Roles: []workercore.ModelDeploymentRole{{AcceleratorType: "ignored"}},
			},
		},
	}
	pools := AcceleratorTypeFlavorNames{
		"h100-8":  {testGPUFlavorZone},
		"mi300-8": nil,
	}
	pf := sourcePrefetch(nil)

	targets, err := PrefetchTargetNodes(pf, []core.Node{gpu1}, deps, pools)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpu-1"}, targets,
		"the deployment-served type's flavor node is the derived target")
}

func TestPrefetchTargetNodesUnparsableSelectorIsAnError(t *testing.T) {
	pf := sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{
			MatchExpressions: []meta.LabelSelectorRequirement{{
				Key: "pool", Operator: "Between", Values: []string{"a", "z"},
			}},
		}}
	})
	_, err := PrefetchTargetNodes(pf, []core.Node{cpuNode("cpu-1")}, nil, nil)
	assert.Error(t, err, "an unparsable selector is an error, not an empty set")
}

func TestPrefetchSourceTargetNodesExplicitOnly(t *testing.T) {
	gpu1 := accelNode("gpu-1", testProfileZone)
	gpu2 := accelNode("gpu-2", testProfileHost) // same hardware, other profile: another flavor
	pools := AcceleratorTypeFlavorNames{
		"h100-8": {testGPUFlavorZone, "gpustack--generic--nvidia-h100-80gb-hbm3-linux-amd64-8d-" + testProfileHost},
		"ghost":  nil,
	}

	// A source with no placement at all is a refusal — structurally, not because the caller
	// forgot to pass deployments: the signature carries none.
	pf := sourcePrefetch(nil)
	_, err := PrefetchSourceTargetNodes(pf, []core.Node{gpu1, gpu2}, pools)
	assert.ErrorIs(t, err, ErrPrefetchPlacementMissing)

	// A source naming types resolves only the flavors it names; a type with no synced flavors
	// contributes an empty set, not an error, and the target set is sorted.
	pf = sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			AcceleratorTypes: []string{"h100-8", "ghost"},
		}
	})
	targets, err := PrefetchSourceTargetNodes(pf, []core.Node{gpu2, gpu1}, pools)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpu-1", "gpu-2"}, targets,
		"both profiles' flavors of the named type are targets, sorted")

	// A topology-qualified flavor name matches only the node set carrying that profile.
	pools = AcceleratorTypeFlavorNames{"h100-8": {testGPUFlavorZone}}
	targets, err = PrefetchSourceTargetNodes(pf, []core.Node{gpu1, gpu2}, pools)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpu-1"}, targets,
		"the zone-qualified flavor does not reach the host-profile node")

	// A source naming a selector never consults deployments — there is no parameter to
	// consult.
	pf = sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = selectorPlacement(map[string]string{core.LabelArchStable: testGPUArch})
	})
	targets, err = PrefetchSourceTargetNodes(pf, []core.Node{gpu1, gpu2}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"gpu-1", "gpu-2"}, targets)

	// Both kinds set is the shared ambiguity refusal.
	pf = sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			AcceleratorTypes: []string{"h100-8"},
			NodeSelector:     &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
		}
	})
	_, err = PrefetchSourceTargetNodes(pf, []core.Node{gpu1, gpu2}, pools)
	assert.ErrorIs(t, err, ErrPrefetchPlacementAmbiguous)
}

func TestResolvePlacementInstanceTypeFlavorNames(t *testing.T) {
	ctx := context.Background()
	collapsed := notedFlavor(testGPUFlavorZone, nodefeature.PoolScheduleLabels(
		true, false, testCPUKey, testGPUKey, testGPUOS, testGPUArch,
	))
	aware := notedFlavor("flavor-qualified-aware", nodefeature.PoolScheduleLabels(
		true, true, "amd-epyc", testGPUKey, testGPUOS, testGPUArch,
	))

	t.Run("unknown type resolves empty, not an error", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(collapsed).Build()
		pools, err := ResolvePlacementAcceleratorTypeFlavorNames(ctx, cli, []string{"ghost"}, false)
		require.NoError(t, err)
		assert.Empty(t, pools["ghost"])
	})

	t.Run("no type names resolves nothing", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		pools, err := ResolvePlacementAcceleratorTypeFlavorNames(ctx, cli, nil, false)
		require.NoError(t, err)
		assert.Nil(t, pools)
	})

	t.Run("the pool follows the type's schedule labels", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(gpuAcceleratorType("h100-8", testCPUKey), collapsed, aware).Build()
		pools, err := ResolvePlacementAcceleratorTypeFlavorNames(ctx, cli, []string{"h100-8"}, false)
		require.NoError(t, err)
		assert.Equal(t, []string{"flavor-qualified-aware", testGPUFlavorZone}, pools["h100-8"],
			"the lookup is a label containment: an aware flavor is a superset of a collapsed type's schedule labels, so both are the pool")
	})

	t.Run("cpu awareness splits the pool", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(gpuAcceleratorType("h100-8", "amd-epyc"), collapsed, aware).Build()

		pools, err := ResolvePlacementAcceleratorTypeFlavorNames(ctx, cli, []string{"h100-8"}, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"flavor-qualified-aware"}, pools["h100-8"],
			"an aware pool selects the CPU-split flavor")

		pools, err = ResolvePlacementAcceleratorTypeFlavorNames(ctx, cli, []string{"h100-8"}, false)
		require.NoError(t, err)
		assert.Equal(t, []string{"flavor-qualified-aware", testGPUFlavorZone}, pools["h100-8"],
			"read unaware, the same type's pool is both flavors: the flag decides the split")
	})

	t.Run("api errors propagate", func(t *testing.T) {
		_, err := ResolvePlacementAcceleratorTypeFlavorNames(ctx, failingReader{},
			[]string{"h100-8"}, false)
		assert.Error(t, err)
	})
}

// failingReader fails every read, the stand-in for a transient API error: the resolver must
// return it, not an empty answer.
type failingReader struct{}

func (failingReader) Get(context.Context, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
	return errors.New("etcd unavailable")
}

func (failingReader) List(context.Context, ctrlcli.ObjectList, ...ctrlcli.ListOption) error {
	return errors.New("etcd unavailable")
}

func TestBindingCoversNode(t *testing.T) {
	liveStore := &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "pool-a"},
		Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "a"}},
		},
	}
	deletingStore := liveStore.DeepCopy()
	deletingStore.Name = "pool-dying"
	deletingStore.DeletionTimestamp = deletionTime()
	deletingStore.Finalizers = []string{"gpustack.ai/test-held"}
	badSelectorStore := &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "pool-bad"},
		Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{
			MatchExpressions: []meta.LabelSelectorRequirement{{
				Key: "pool", Operator: "Between", Values: []string{"a", "z"},
			}},
		}},
	}
	node := accelNode("gpu-1", testProfileZone)
	node.Labels["pool"] = "a"
	ctx := context.Background()
	newClient := func(objs ...ctrlcli.Object) ctrlcli.Client {
		return ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	}
	bindingRef := func(names ...string) *workercore.ModelStoreBinding {
		binding := &workercore.ModelStoreBinding{}
		for _, name := range names {
			binding.Spec.StoreRefs = append(binding.Spec.StoreRefs,
				workercore.ModelStoreBindingStoreReference{Name: name})
		}
		return binding
	}

	t.Run("live store selector covers the node", func(t *testing.T) {
		covered, err := BindingCoversNode(ctx, newClient(liveStore), bindingRef("pool-a"), &node)
		require.NoError(t, err)
		assert.True(t, covered)
	})

	t.Run("deleted binding grants nothing", func(t *testing.T) {
		binding := bindingRef("pool-a")
		binding.DeletionTimestamp = deletionTime()
		covered, err := BindingCoversNode(ctx, newClient(liveStore), binding, &node)
		require.NoError(t, err)
		assert.False(t, covered)
	})

	t.Run("missing store covers nothing and fabricates no grant", func(t *testing.T) {
		covered, err := BindingCoversNode(ctx, newClient(liveStore), bindingRef("ghost"), &node)
		require.NoError(t, err)
		assert.False(t, covered, "the ghost covers nothing; the binding webhook refuses it")
	})

	t.Run("deleting store covers nothing", func(t *testing.T) {
		covered, err := BindingCoversNode(ctx, newClient(deletingStore), bindingRef("pool-dying"), &node)
		require.NoError(t, err)
		assert.False(t, covered)
	})

	t.Run("invalid selector covers nothing", func(t *testing.T) {
		covered, err := BindingCoversNode(ctx, newClient(badSelectorStore), bindingRef("pool-bad"), &node)
		require.NoError(t, err)
		assert.False(t, covered, "a selector that cannot parse refuses, it does not match all")
	})

	t.Run("api error is not an empty answer", func(t *testing.T) {
		_, err := BindingCoversNode(ctx, failingReader{}, bindingRef("pool-a"), &node)
		assert.Error(t, err)
	})

	t.Run("no store refs cover nothing", func(t *testing.T) {
		covered, err := BindingCoversNode(ctx, newClient(), &workercore.ModelStoreBinding{}, &node)
		require.NoError(t, err)
		assert.False(t, covered)
	})
}

func TestResolveInstanceTypeFlavorNamesNotFoundIsNotAnError(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	pools, err := ResolvePlacementAcceleratorTypeFlavorNames(context.Background(), cli,
		[]string{"absent"}, false)
	require.NoError(t, err)
	assert.NotNil(t, pools)
	assert.Empty(t, pools["absent"], "the type maps to an explicit empty entry")
}

func TestPrefetchTargetNodesMissingFlavorsStayEmpty(t *testing.T) {
	// "Missing type/flavors means empty target": a placed prefetch over a type the cluster
	// never heard of targets nothing and says nothing; only API errors propagate.
	pf := sourcePrefetch(func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{AcceleratorTypes: []string{"ghost"}}
	})
	targets, err := PrefetchTargetNodes(pf, []core.Node{accelNode("gpu-1", testProfileZone)},
		nil, AcceleratorTypeFlavorNames{"ghost": nil})
	require.NoError(t, err)
	assert.Empty(t, targets)
}
