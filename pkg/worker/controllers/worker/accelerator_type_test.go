package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
)

func reconcileAcceleratorTypeN(t *testing.T, cli ctrlcli.Client, name string, n int) {
	t.Helper()
	r := &AcceleratorTypeReconciler{Client: cli, APIReader: cli}
	for range n {
		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
		require.NoError(t, err)
	}
}

func getAcceleratorType(t *testing.T, cli ctrlcli.Client, name string) *workercore.AcceleratorType {
	t.Helper()
	at := new(workercore.AcceleratorType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, at))
	return at
}

func TestAcceleratorTypeReconciler_CreatesClusterQueueAndStatus(t *testing.T) {
	key := "nvidia-a10g"
	canonicalCQName := "gpustack--nvidia-a10g-linux-amd64"
	at := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "custom-a10g"},
		Spec: workercore.AcceleratorTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			OS:               "linux",
			Arch:             "amd64",
			DisplayName:      "NVIDIA A10G",
		},
	}

	flavorLabels := map[string]string{
		nodefeature.NodeAcceleratableLabelKey:             "true",
		nodefeature.AcceleratableFeatureLabelPrefix + key: "true",
		core.LabelOSStable:                                "linux",
		core.LabelArchStable:                              "amd64",
	}
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name:   "gpustack--generic--nvidia-a10g-linux-amd64-1d",
			Labels: flavorLabels,
		},
	}
	systemmeta.NoteResource(rf, _ResourceFlavorResType, map[string]string{
		"manufacturer": nodefeature.ManufacturerNVIDIA,
		"product":      "NVIDIA A10G",
		"family":       "Ampere",
		"memory":       "24Gi",
		"cores":        "80",
	})

	dev := nodeDevices("node-0", map[string]string{
		systemname.ManagedLabelKey:                        "true",
		nodefeature.AcceleratableFeatureLabelPrefix + key: "true",
		core.LabelOSStable:                                "linux",
		core.LabelArchStable:                              "amd64",
	}, cardFree(), cardFree())

	cli := buildInstanceTypeClient(at, rf, &dev)
	reconcileAcceleratorTypeN(t, cli, at.Name, 3)

	cq, err := getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err, "ClusterQueue created under canonical pool queue name")
	assert.True(t, systemmeta.MatchResource(cq, _ClusterQueueResType), "ClusterQueue stamped with instancetypes resource type")
	assert.Equal(t, "true", cq.Labels[nodefeature.AcceleratableFeatureLabelPrefix+key])

	got := getAcceleratorType(t, cli, at.Name)
	assert.True(t, systemmeta.IsLocked(got), "finalizer held")
	assert.Equal(t, nodefeature.FormatLocalQueueName(canonicalCQName), got.Status.Entrance)
	assert.Equal(t, nodefeature.ManufacturerNVIDIA, got.Status.Detail.Manufacturer)
	assert.Equal(t, "24Gi", got.Status.Detail.Memory)
	assert.Equal(t, int64(2), got.Status.Accelerator.Capacity.Value())
	assert.Equal(t, int64(2), got.Status.Accelerator.Remaining.Value())
}

func TestAcceleratorTypeAndInstanceType_SharePoolClusterQueueAndCoordinateInactive(t *testing.T) {
	key := "nvidia-a10g"
	canonicalCQName := "gpustack--nvidia-a10g-linux-amd64"

	at := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{
			Name:       canonicalCQName,
			UID:        "uid-at",
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.AcceleratorTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			OS:               "linux",
			Arch:             "amd64",
		},
	}
	itSmall := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       "a10g-small",
			UID:        "uid-it-small",
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			Acceleratable:    true,
			OS:               "linux",
			Arch:             "amd64",
			UnitResources:    workercore.InstanceTypeUnitResources{CPU: "4", RAM: "16Gi"},
			LocalStorage:     "100Gi",
		},
	}
	itLarge := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       "a10g-large",
			UID:        "uid-it-large",
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			Acceleratable:    true,
			OS:               "linux",
			Arch:             "amd64",
			UnitResources:    workercore.InstanceTypeUnitResources{CPU: "8", RAM: "32Gi"},
			LocalStorage:     "200Gi",
		},
	}
	cq := newInstanceTypeQueue(key, true, cpuResourceGroup("gpustack--nvidia-a10g-linux-amd64-1d", 4))
	cq.Name = canonicalCQName
	cq.Spec.StopPolicy = ptr.To(kueue.None)

	cli := buildInstanceTypeClient(at, itSmall, itLarge, cq)
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)
	reconcileInstanceTypeN(t, cli, itSmall.Name, 1)
	reconcileInstanceTypeN(t, cli, itLarge.Name, 1)

	// Marking one InstanceType inactive while others remain active keeps the shared ClusterQueue active (None).
	gotSmall := getInstanceType(t, cli, itSmall.Name)
	gotSmall.Spec.Inactive = true
	require.NoError(t, cli.Update(context.Background(), gotSmall))
	reconcileInstanceTypeN(t, cli, itSmall.Name, 1)

	gotCQ, err := getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err)
	assert.Equal(t, kueue.None, ptr.Deref(gotCQ.Spec.StopPolicy, kueue.None),
		"shared ClusterQueue stays active while at least one fronting object is active")
	assert.False(t, getInstanceType(t, cli, itLarge.Name).Spec.Inactive)
	assert.False(t, getAcceleratorType(t, cli, at.Name).Spec.Inactive)

	// Marking all remaining fronting objects inactive puts the shared ClusterQueue on Hold.
	gotLarge := getInstanceType(t, cli, itLarge.Name)
	gotLarge.Spec.Inactive = true
	require.NoError(t, cli.Update(context.Background(), gotLarge))
	reconcileInstanceTypeN(t, cli, itLarge.Name, 1)

	gotAt := getAcceleratorType(t, cli, at.Name)
	gotAt.Spec.Inactive = true
	require.NoError(t, cli.Update(context.Background(), gotAt))
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)

	gotCQ, err = getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err)
	assert.Equal(t, kueue.Hold, ptr.Deref(gotCQ.Spec.StopPolicy, kueue.None),
		"shared ClusterQueue transitions to Hold once all fronting objects are inactive")

	// Reactivating any single fronting object releases the shared ClusterQueue back to None.
	gotAt = getAcceleratorType(t, cli, at.Name)
	gotAt.Spec.Inactive = false
	require.NoError(t, cli.Update(context.Background(), gotAt))
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)

	gotCQ, err = getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err)
	assert.Equal(t, kueue.None, ptr.Deref(gotCQ.Spec.StopPolicy, kueue.None),
		"shared ClusterQueue returns to None when any fronting object becomes active")

	// While AcceleratorType is active and InstanceTypes are inactive, inactive InstanceTypes report Inactive phase.
	reconcileInstanceTypeN(t, cli, itSmall.Name, 1)
	assert.Equal(t, InstanceTypePhaseInactive, getInstanceType(t, cli, itSmall.Name).Status.Phase,
		"deactivated InstanceType reports Inactive phase even while shared ClusterQueue is active")

	// Deleting the last active fronting object (AcceleratorType) while inactive InstanceTypes remain
	// transitions the shared ClusterQueue to Hold before releasing AcceleratorType.
	require.NoError(t, cli.Delete(context.Background(), gotAt))
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)

	gotCQ, err = getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err)
	assert.Equal(t, kueue.Hold, ptr.Deref(gotCQ.Spec.StopPolicy, kueue.None),
		"deleting last active fronting object holds shared ClusterQueue when remaining siblings are inactive")
}

func TestCoordinatedTeardown_SharedPoolClusterQueue(t *testing.T) {
	key := "nvidia-a10g"
	canonicalCQName := "gpustack--nvidia-a10g-linux-amd64"
	now := meta.Now()

	at := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{
			Name:       canonicalCQName,
			UID:        "uid-at",
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.AcceleratorTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			OS:               "linux",
			Arch:             "amd64",
		},
	}
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:              "a10g-small",
			UID:               "uid-it",
			DeletionTimestamp: &now,
			Finalizers:        []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: key,
			Acceleratable:    true,
			OS:               "linux",
			Arch:             "amd64",
			UnitResources:    workercore.InstanceTypeUnitResources{CPU: "4", RAM: "16Gi"},
			LocalStorage:     "100Gi",
		},
	}
	inst := &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      "work-0",
		},
		Spec: workercore.InstanceSpec{
			Type: it.Name,
		},
	}
	cq := newInstanceTypeQueue(key, true, cpuResourceGroup("gpustack--nvidia-a10g-linux-amd64-1d", 4))
	cq.Name = canonicalCQName
	cq.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}

	cli := buildInstanceTypeClient(at, it, inst, cq)

	// Deleting InstanceType while AcceleratorType still fronts the pool stops Instances of it.Name
	// and unlocks the InstanceType immediately WITHOUT deleting the shared ClusterQueue.
	reconcileInstanceTypeN(t, cli, it.Name, 1)

	gotInst := new(workercore.Instance)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(inst), gotInst))
	assert.True(t, gotInst.Spec.Stop, "active Instance referencing terminating InstanceType is stopped")

	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: it.Name}, new(workercore.InstanceType))
	assert.True(t, kerrors.IsNotFound(err), "terminating InstanceType unlocked and removed while sibling AcceleratorType lives")

	gotCQ, err := getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err, "shared ClusterQueue is preserved while AcceleratorType still fronts it")
	assert.Nil(t, gotCQ.DeletionTimestamp, "shared ClusterQueue is not marked for deletion")

	// Now delete the last fronting object (AcceleratorType): it deletes the shared ClusterQueue
	// and waits for it to disappear before unlocking.
	require.NoError(t, cli.Delete(context.Background(), at))
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)

	gotCQ, err = getClusterQueue(t, cli, canonicalCQName)
	require.NoError(t, err)
	assert.NotNil(t, gotCQ.DeletionTimestamp, "last fronting object requests ClusterQueue deletion")

	// Release Kueue finalizer so ClusterQueue disappears, then reconcile AcceleratorType again.
	gotCQ.Finalizers = nil
	require.NoError(t, cli.Update(context.Background(), gotCQ))
	reconcileAcceleratorTypeN(t, cli, at.Name, 1)

	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: at.Name}, new(workercore.AcceleratorType))
	assert.True(t, kerrors.IsNotFound(err), "last fronting AcceleratorType unlocked once ClusterQueue is gone")
}

func TestAcceleratorTypeReconciler_DeletesAcceleratorTypeWhenClusterQueueDeleted(t *testing.T) {
	key := "nvidia-a10g"
	canonicalCQName := "gpustack--nvidia-a10g-linux-amd64"

	t.Run("immediate cluster queue deletion deletes standalone derived accelerator type without recreating queue", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{
				Name:   canonicalCQName,
				Labels: map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
			},
			Spec: workercore.AcceleratorTypeSpec{
				GeneralGroup:     "generic",
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
			},
		}
		cli := buildInstanceTypeClient(at)
		reconcileAcceleratorTypeN(t, cli, at.Name, 1)

		cq, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err)
		got := getAcceleratorType(t, cli, at.Name)
		assert.NotEmpty(t, got.Status.Entrance)

		// Delete the backing ClusterQueue directly; standalone derived AcceleratorType deletes itself in sync.
		require.NoError(t, cli.Delete(context.Background(), cq))
		reconcileAcceleratorTypeN(t, cli, at.Name, 2)

		err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: at.Name}, new(workercore.AcceleratorType))
		assert.True(t, kerrors.IsNotFound(err), "standalone derived AcceleratorType deleted automatically when ClusterQueue is deleted")
		_, err = getClusterQueue(t, cli, canonicalCQName)
		assert.True(t, kerrors.IsNotFound(err), "deleted ClusterQueue is not recreated by standalone derived AcceleratorType")
	})

	t.Run("terminating cluster queue marks standalone derived accelerator type for deletion and unlocks once queue vanishes", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{
				Name:       canonicalCQName,
				Labels:     map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
				Finalizers: []string{systemmeta.LockedResourceFinalizer},
			},
			Spec: workercore.AcceleratorTypeSpec{
				GeneralGroup:     "generic",
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
			},
		}
		cq := newInstanceTypeQueue(key, true, cpuResourceGroup("gpustack--nvidia-a10g-linux-amd64-1d", 4))
		cq.Name = canonicalCQName
		cq.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}

		cli := buildInstanceTypeClient(at, cq)
		reconcileAcceleratorTypeN(t, cli, at.Name, 1)

		// Mark ClusterQueue for deletion (held by Kueue finalizer).
		require.NoError(t, cli.Delete(context.Background(), cq))
		reconcileAcceleratorTypeN(t, cli, at.Name, 2)

		gotAt := getAcceleratorType(t, cli, at.Name)
		assert.NotNil(t, gotAt.DeletionTimestamp, "standalone derived AcceleratorType marked for deletion when ClusterQueue is terminating")
		assert.Equal(t, InstanceTypePhaseInactive, gotAt.Status.Phase)

		// Release Kueue finalizer so ClusterQueue disappears; AcceleratorType unlocks and vanishes.
		gotCQ, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err)
		gotCQ.Finalizers = nil
		require.NoError(t, cli.Update(context.Background(), gotCQ))
		reconcileAcceleratorTypeN(t, cli, at.Name, 1)

		err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: at.Name}, new(workercore.AcceleratorType))
		assert.True(t, kerrors.IsNotFound(err), "AcceleratorType removed once terminating ClusterQueue vanishes")
	})

	t.Run("admin-authored accelerator type recreates deleted cluster queue and preserves admin state", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{Name: "custom-admin-a10g"},
			Spec: workercore.AcceleratorTypeSpec{
				GeneralGroup:     "generic",
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
				Inactive:         true,
				DisplayName:      "Admin A10G Pool",
			},
		}
		cli := buildInstanceTypeClient(at)
		reconcileAcceleratorTypeN(t, cli, at.Name, 2)

		cqBefore, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err)
		assert.NotEmpty(t, getAcceleratorType(t, cli, at.Name).Status.Entrance)

		// Delete the ClusterQueue under a live admin-authored AcceleratorType: it must recreate the queue
		// rather than destroying the admin-authored CR.
		cqBefore.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}
		require.NoError(t, cli.Update(context.Background(), cqBefore))
		require.NoError(t, cli.Delete(context.Background(), cqBefore))

		r := &AcceleratorTypeReconciler{Client: cli}
		res, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: at.Name}})
		require.NoError(t, err)
		assert.Equal(t, 15*time.Second, res.RequeueAfter, "terminating ClusterQueue requeues without deleting admin AcceleratorType")

		gotDuring := getAcceleratorType(t, cli, at.Name)
		assert.Nil(t, gotDuring.DeletionTimestamp)
		assert.True(t, gotDuring.Spec.Inactive)

		gotTerminatingCQ, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err)
		gotTerminatingCQ.Finalizers = nil
		require.NoError(t, cli.Update(context.Background(), gotTerminatingCQ))

		reconcileAcceleratorTypeN(t, cli, at.Name, 1)
		cqAfter, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err, "deleted ClusterQueue is recreated for live admin-authored AcceleratorType")
		assert.Nil(t, cqAfter.DeletionTimestamp)
		gotAfter := getAcceleratorType(t, cli, at.Name)
		assert.Nil(t, gotAfter.DeletionTimestamp)
		assert.True(t, gotAfter.Spec.Inactive, "admin-authored Spec.Inactive is preserved across queue recreation")
	})

	t.Run("derived accelerator type sharing pool with live instance type survives cluster queue deletion and recreates queue", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{
				Name:   canonicalCQName,
				Labels: map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
			},
			Spec: workercore.AcceleratorTypeSpec{
				GeneralGroup:     "generic",
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
			},
		}
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "workspace-a10g"},
			Spec: workercore.InstanceTypeSpec{
				GeneralGroup:     "generic",
				Acceleratable:    true,
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
			},
		}
		cli := buildInstanceTypeClient(at, it)
		reconcileAcceleratorTypeN(t, cli, at.Name, 1)
		reconcileInstanceTypeN(t, cli, it.Name, 1)

		cq, err := getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err)
		require.NoError(t, cli.Delete(context.Background(), cq))

		reconcileAcceleratorTypeN(t, cli, at.Name, 1)
		gotAt := getAcceleratorType(t, cli, at.Name)
		assert.Nil(t, gotAt.DeletionTimestamp, "derived AcceleratorType must not delete itself while a live InstanceType shares the pool")
		_, err = getClusterQueue(t, cli, canonicalCQName)
		require.NoError(t, err, "shared ClusterQueue is recreated cleanly without racing self-deletion")
	})

	t.Run("stale cache NotFound on ClusterQueue does not delete AcceleratorType when APIReader finds queue", func(t *testing.T) {
		at := &workercore.AcceleratorType{
			ObjectMeta: meta.ObjectMeta{
				Name:       canonicalCQName,
				Labels:     map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
				Finalizers: []string{systemmeta.LockedResourceFinalizer},
			},
			Spec: workercore.AcceleratorTypeSpec{
				GeneralGroup:     "generic",
				AcceleratorGroup: key,
				OS:               "linux",
				Arch:             "amd64",
			},
			Status: workercore.AcceleratorTypeStatus{
				Entrance: nodefeature.FormatLocalQueueName(canonicalCQName),
			},
		}
		cq := newInstanceTypeQueue(key, true, cpuResourceGroup("gpustack--nvidia-a10g-linux-amd64-1d", 4))
		cq.Name = canonicalCQName

		// Simulate a stale informer cache missing the ClusterQueue while the uncached APIReader finds it.
		cachedCli := buildInstanceTypeClient(at)
		apiReaderCli := buildInstanceTypeClient(at, cq)
		r := &AcceleratorTypeReconciler{Client: cachedCli, APIReader: apiReaderCli}
		_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: at.Name}})
		require.NoError(t, err)

		gotAt := getAcceleratorType(t, cachedCli, at.Name)
		assert.Nil(t, gotAt.DeletionTimestamp, "stale cache miss must not delete AcceleratorType when APIReader confirms ClusterQueue exists")
	})
}

func TestAcceleratorTypeHelpers(t *testing.T) {
	ctx := context.Background()
	at := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "custom-h100"},
		Spec: workercore.AcceleratorTypeSpec{
			GeneralGroup:     "generic",
			AcceleratorGroup: "nvidia-h100",
			OS:               "linux",
			Arch:             "amd64",
		},
		Status: workercore.AcceleratorTypeStatus{
			Detail: workercore.InstanceTypeDetail{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Memory:       quantityToString(resource.MustParse("80Gi")),
				SlicedDetail: workercore.AcceleratorSlicedDetail{
					Physical: workercore.AcceleratorSlicedPhysicalDetail{
						Profiles: []workercore.AcceleratorSlicedPhysicalDetailProfile{
							{Name: "1g.10gb", MemoryMib: 10240, Count: 7},
						},
					},
				},
			},
		},
	}

	assert.Equal(t, "gpustack--nvidia-h100-linux-amd64", AcceleratorTypePoolQueueName(ctx, at))
	lbs := AcceleratorTypeScheduleLabels(ctx, at)
	assert.Equal(t, "true", lbs[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-h100"])
	assert.Equal(t, "true", lbs[nodefeature.NodeAcceleratableLabelKey])

	vram, err := AcceleratorTypeCardVRAMMib(at)
	require.NoError(t, err)
	assert.Equal(t, int64(81920), vram)

	prof, ready, found := AcceleratorTypePartitionProfile(at, "1g.10gb")
	assert.True(t, ready)
	assert.True(t, found)
	assert.Equal(t, int64(10240), prof.MemoryMib)

	pct, sizeable := AcceleratorTypePartitionProfileMemoryPercent(at, "1g.10gb")
	assert.True(t, sizeable)
	assert.Equal(t, int64(12), pct)
}

func quantityToString(q resource.Quantity) string {
	return q.String()
}
