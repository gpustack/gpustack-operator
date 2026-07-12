package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

func TestListAggregateInstanceTypes_Result(t *testing.T) {
	cases := []struct {
		name     string
		list     AggregatedInstanceTypeList
		sorted   bool
		expected []string
	}{
		{
			name: "empty list + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{},
			},
			sorted:   true,
			expected: []string{},
		},
		{
			name: "list with cpu-only + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
				},
			},
			sorted:   true,
			expected: []string{"gpustack-cpu-only"},
		},
		{
			name: "list with acceleratable + unsorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
					{
						Name: "gpustack-nvidia-tesla-t4",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
					{
						Name: "gpustack-nvidia-a10g",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
				},
			},
			sorted:   false,
			expected: []string{"gpustack-cpu-only", "gpustack-nvidia-tesla-t4", "gpustack-nvidia-a10g"},
		},
		{
			name: "list with acceleratable + sorted",
			list: AggregatedInstanceTypeList{
				Items: []AggregatedInstanceType{
					{
						Name: "gpustack-cpu-only",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: false,
						},
					},
					{
						Name: "gpustack-nvidia-tesla-t4",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
					{
						Name: "gpustack-nvidia-a10g",
						Spec: AggregatedInstanceTypeSpec{
							Acceleratable: true,
						},
					},
				},
			},
			sorted:   true,
			expected: []string{"gpustack-nvidia-a10g", "gpustack-nvidia-tesla-t4", "gpustack-cpu-only"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			listOp := ListAggregateInstanceTypes{list: c.list}
			result := listOp.Result(c.sorted)
			actual := make([]string, len(result.Items))
			for i, item := range result.Items {
				actual[i] = item.Name
			}
			assert.Equal(t, c.expected, actual)
		})
	}
}

func instTypeRes(once, remaining, capacity string) workercore.InstanceTypeResource {
	return workercore.InstanceTypeResource{
		OnceMaxRequest: resource.MustParse(once),
		Remaining:      resource.MustParse(remaining),
		Capacity:       resource.MustParse(capacity),
	}
}

func instSpecCPUOnly() workercore.InstanceTypeSpec {
	return workercore.InstanceTypeSpec{
		GeneralGroup:  "gpustack-cpu-only",
		Acceleratable: false,
	}
}

func instSpecA10G() workercore.InstanceTypeSpec {
	return workercore.InstanceTypeSpec{
		AcceleratorGroup: "gpustack-nvidia-a10g",
		Acceleratable:    true,
		Manufacturer:     "nvidia",
		Product:          "NVIDIA-A10G",
		Family:           "Ampere",
		InstanceTypeAccelerator: workercore.InstanceTypeAccelerator{
			Memory:            "23028Mi",
			ComputeCapability: "8.6",
		},
	}
}

func instSpecTeslaT4() workercore.InstanceTypeSpec {
	return workercore.InstanceTypeSpec{
		AcceleratorGroup: "gpustack-nvidia-tesla-t4",
		Acceleratable:    true,
		Manufacturer:     "nvidia",
		Product:          "Tesla-T4",
		Family:           "Turing",
		InstanceTypeAccelerator: workercore.InstanceTypeAccelerator{
			Memory:            "15360Mi",
			ComputeCapability: "7.5",
		},
	}
}

func instStatusCPU() workercore.InstanceTypeStatus {
	return workercore.InstanceTypeStatus{
		Phase:             "Active",
		Accelerator:       instTypeRes("0", "0", "0"),
		CPU:               instTypeRes("16", "16", "16"),
		AcceleratorShared: instTypeRes("32135984Ki", "32135984Ki", "32135984Ki"),
		AcceleratorSliced: instTypeRes("104779756Ki", "104779756Ki", "104779756Ki"),
	}
}

func instStatusGPU(acc string) workercore.InstanceTypeStatus {
	return workercore.InstanceTypeStatus{
		Phase:             "Active",
		Accelerator:       instTypeRes(acc, acc, acc),
		CPU:               instTypeRes("4", "4", "4"),
		AcceleratorShared: instTypeRes("16164772Ki", "16164772Ki", "16164772Ki"),
		AcceleratorSliced: instTypeRes("104779756Ki", "104779756Ki", "104779756Ki"),
	}
}

func newInstType(genName, name string, spec workercore.InstanceTypeSpec, status workercore.InstanceTypeStatus) *worker.InstanceType {
	return &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:         name,
			GenerateName: genName,
		},
		Spec:   spec,
		Status: status,
	}
}

func cpuOnlyInst(name string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-cpu-only-h7vkb"
	}
	return newInstType("gpustack-cpu-only-", name, instSpecCPUOnly(), instStatusCPU())
}

func a10gInst(name, acc string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-nvidia-a10g-hcjmv"
	}
	return newInstType("gpustack-nvidia-a10g-", name, instSpecA10G(), instStatusGPU(acc))
}

func teslaT4Inst(name, acc string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-nvidia-tesla-t4-sh2zl"
	}
	return newInstType("gpustack-nvidia-tesla-t4-", name, instSpecTeslaT4(), instStatusGPU(acc))
}

// a10gInstCustom returns an A10G instance type whose CPU/AcceleratorShared/storage can be overridden.
// Used to construct scenarios where per-dimension max diverges from bundle-from-winner.
func a10gInstCustom(name, acc, cpu, ram, storage string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-nvidia-a10g-hcjmv"
	}
	status := workercore.InstanceTypeStatus{
		Phase:             "Active",
		Accelerator:       instTypeRes(acc, acc, acc),
		CPU:               instTypeRes(cpu, cpu, cpu),
		AcceleratorShared: instTypeRes(ram, ram, ram),
		AcceleratorSliced: instTypeRes(storage, storage, storage),
	}
	return newInstType("gpustack-nvidia-a10g-", name, instSpecA10G(), status)
}

// cpuOnlyInstCustom returns a CPU-only instance type whose CPU/AcceleratorShared/storage can be overridden.
func cpuOnlyInstCustom(name, cpu, ram, storage string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-cpu-only-h7vkb"
	}
	status := workercore.InstanceTypeStatus{
		Phase:             "Active",
		Accelerator:       instTypeRes("0", "0", "0"),
		CPU:               instTypeRes(cpu, cpu, cpu),
		AcceleratorShared: instTypeRes(ram, ram, ram),
		AcceleratorSliced: instTypeRes(storage, storage, storage),
	}
	return newInstType("gpustack-cpu-only-", name, instSpecCPUOnly(), status)
}

// withPhase overrides an instance type's status phase; the status helpers default to "Active".
func withPhase(it *worker.InstanceType, phase string) *worker.InstanceType {
	it.Status.Phase = phase
	return it
}

type seed struct {
	cluster string
	obj     *worker.InstanceType
}

func buildState(t *testing.T, seeds ...seed) AggregatedInstanceTypeList {
	t.Helper()
	op := OpListAggregateInstanceTypes()
	for _, s := range seeds {
		require.NoError(t, op.Next(s.cluster, s.obj))
	}
	return op.Result(false)
}

func findItem(state AggregatedInstanceTypeList, name string) *AggregatedInstanceType {
	for i := range state.Items {
		if state.Items[i].Name == name {
			return &state.Items[i]
		}
	}
	return nil
}

func TestHandleAggregatedInstanceType(t *testing.T) {
	t.Run("add brand-new item emits Added with Object as *AggregatedInstanceType", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventAdded,
			Cluster: "cluster-a",
			Object:  a10gInst("", "1"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventAdded, evts[0].Type)

		item, ok := evts[0].Object.(*AggregatedInstanceType)
		require.True(t, ok, "Added event Object must be *AggregatedInstanceType, got %T", evts[0].Object)
		assert.Equal(t, "gpustack-nvidia-a10g", item.Name)

		require.Len(t, h.state.Items, 1)
		require.Len(t, h.state.Items[0].Status.Tiers, 1)
		require.Len(t, h.state.Items[0].Status.Tiers[0].Candidates, 1)
	})

	t.Run("add candidate to existing tier emits Modified", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventAdded,
			Cluster: "cluster-b",
			Object:  a10gInst("inst-b", "1"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 1)
		assert.Len(t, item.Status.Tiers[0].Candidates, 2)
	})

	t.Run("add new tier to existing item keeps tiers sorted ascending", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventAdded,
			Cluster: "cluster-b",
			Object:  a10gInst("inst-b", "4"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 2)
		assert.True(t,
			item.Status.Tiers[0].OnceMaxRequest.Accelerator.Cmp(
				item.Status.Tiers[1].OnceMaxRequest.Accelerator) < 0,
			"tiers must remain sorted ascending by accelerator after a new tier is appended")
	})

	t.Run("in-place update keeps candidate in same tier and recomputes overview", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
		))

		updated := a10gInst("inst-a", "1")
		updated.Status.CPU = instTypeRes("8", "8", "8")

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventModified,
			Cluster: "cluster-a",
			Object:  updated,
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 1)
		require.Len(t, item.Status.Tiers[0].Candidates, 1)
		assert.True(t, item.Status.Tiers[0].Candidates[0].CPU.OnceMaxRequest.Equal(resource.MustParse("8")))
		assert.True(t, item.Status.Tiers[0].OnceMaxRequest.CPU.Equal(resource.MustParse("8")))
		assert.True(t, item.Status.OnceMaxRequest.CPU.Equal(resource.MustParse("8")))
	})

	t.Run("cross-tier move into existing tier merges candidates correctly", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "2")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventModified,
			Cluster: "cluster-a",
			Object:  a10gInst("inst-a", "2"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 1, "tier Acc=1 should be removed when its only candidate moves out")
		tier := &item.Status.Tiers[0]
		assert.True(t, tier.OnceMaxRequest.Accelerator.Equal(resource.MustParse("2")))
		require.Len(t, tier.Candidates, 2)

		got := map[string]string{}
		for _, c := range tier.Candidates {
			got[c.Cluster] = c.Name
		}
		assert.Equal(t, "inst-a", got["cluster-a"], "inst-a must end up in tier Acc=2 with original name")
		assert.Equal(t, "inst-b", got["cluster-b"], "inst-b must still be there with original name")
	})

	t.Run("cross-tier move creates a new tier when no target tier exists", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "8")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventModified,
			Cluster: "cluster-a",
			Object:  a10gInst("inst-a", "4"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 2)
		assert.True(t, item.Status.Tiers[0].OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")),
			"new tier Acc=4 must be sorted before existing tier Acc=8")
		assert.True(t, item.Status.Tiers[1].OnceMaxRequest.Accelerator.Equal(resource.MustParse("8")))
	})

	t.Run("delete candidate but tier retains others", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-a",
			Object:  a10gInst("inst-a", "1"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 1)
		require.Len(t, item.Status.Tiers[0].Candidates, 1)
		assert.Equal(t, "cluster-b", item.Status.Tiers[0].Candidates[0].Cluster)
	})

	t.Run("delete candidate empties tier but item retains another tier", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "2")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-a",
			Object:  a10gInst("inst-a", "1"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers, 1)
		assert.True(t, item.Status.Tiers[0].OnceMaxRequest.Accelerator.Equal(resource.MustParse("2")))
	})

	t.Run("delete last candidate of middle item emits Deleted with the correct name", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: cpuOnlyInst("")},
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-a", obj: teslaT4Inst("inst-t4", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-a",
			Object:  a10gInst("inst-a", "1"),
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventDeleted, evts[0].Type)
		deleted, ok := evts[0].Object.(*AggregatedInstanceType)
		require.True(t, ok)
		assert.Equal(t, "gpustack-nvidia-a10g", deleted.Name,
			"Deleted event name must reflect the removed item, not the post-splice neighbor")

		assert.NotNil(t, findItem(h.state, "gpustack-cpu-only"))
		assert.NotNil(t, findItem(h.state, "gpustack-nvidia-tesla-t4"))
		assert.Nil(t, findItem(h.state, "gpustack-nvidia-a10g"))
	})

	t.Run("delete-all-cluster emits Deleted with correct name for every removed item", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: cpuOnlyInst("")},
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-a", obj: teslaT4Inst("inst-t4", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-a",
			Object:  nil,
		})

		require.Len(t, evts, 3)
		got := map[string]bool{}
		for _, e := range evts {
			assert.Equal(t, manager.WorkerEventDeleted, e.Type)
			d, ok := e.Object.(*AggregatedInstanceType)
			require.True(t, ok)
			got[d.Name] = true
		}
		assert.True(t, got["gpustack-cpu-only"], "cpu-only Deleted event missing or wrong name; got names=%v", got)
		assert.True(t, got["gpustack-nvidia-a10g"], "a10g Deleted event missing or wrong name; got names=%v", got)
		assert.True(t, got["gpustack-nvidia-tesla-t4"], "tesla-t4 Deleted event missing or wrong name; got names=%v", got)
		assert.Empty(t, h.state.Items)
	})

	t.Run("delete-all-cluster preserves candidates owned by other clusters", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: cpuOnlyInst("cpu-a")},
			seed{cluster: "cluster-b", obj: cpuOnlyInst("cpu-b")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-a",
		})

		require.Len(t, evts, 1, "only cpu-only had a cluster-a candidate; expect exactly one Modified event")
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

		cpu := findItem(h.state, "gpustack-cpu-only")
		require.NotNil(t, cpu)
		require.Len(t, cpu.Status.Tiers, 1)
		require.Len(t, cpu.Status.Tiers[0].Candidates, 1)
		assert.Equal(t, "cluster-b", cpu.Status.Tiers[0].Candidates[0].Cluster)

		gpu := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, gpu, "a10g item must remain untouched")
		require.Len(t, gpu.Status.Tiers, 1)
		require.Len(t, gpu.Status.Tiers[0].Candidates, 1)
	})

	t.Run("deleting a non-existent candidate is a no-op", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
		))

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventDeleted,
			Cluster: "cluster-b",
			Object:  a10gInst("inst-b", "1"),
		})

		assert.Empty(t, evts)
		require.Len(t, h.state.Items, 1)
		require.Len(t, h.state.Items[0].Status.Tiers[0].Candidates, 1)
	})

	t.Run("Modified event with DeletionTimestamp is treated as delete", func(t *testing.T) {
		h := OpHandleAggregatedInstanceType(buildState(t,
			seed{cluster: "cluster-a", obj: a10gInst("inst-a", "1")},
			seed{cluster: "cluster-b", obj: a10gInst("inst-b", "1")},
		))

		dying := a10gInst("inst-a", "1")
		now := meta.NewTime(time.Now())
		dying.DeletionTimestamp = &now

		evts := h.Handle(&manager.WorkerEvent{
			Type:    manager.WorkerEventModified,
			Cluster: "cluster-a",
			Object:  dying,
		})

		require.Len(t, evts, 1)
		assert.Equal(t, manager.WorkerEventModified, evts[0].Type,
			"item still has cluster-b's candidate, so we expect Modified, not Deleted")

		item := findItem(h.state, "gpustack-nvidia-a10g")
		require.NotNil(t, item)
		require.Len(t, item.Status.Tiers[0].Candidates, 1)
		assert.Equal(t, "cluster-b", item.Status.Tiers[0].Candidates[0].Cluster)
	})
}

// TestAggregatedInstanceType_Recompute_BundleSemantics covers the rule that the
// item-level OnceMaxRequest is a coherent bundle from the tier with the largest
// primary dimension, not a per-dimension max across tiers.
func TestAggregatedInstanceType_Recompute_BundleSemantics(t *testing.T) {
	t.Run("acceleratable: high-Acc tier wins even when another tier has higher CPU", func(t *testing.T) {
		// Tier Acc=1 has the higher CPU/AcceleratorShared, but tier Acc=4 wins on the primary dimension.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator:       resource.MustParse("1"),
						CPU:               resource.MustParse("64"),
						AcceleratorShared: resource.MustParse("256Gi"),
						AcceleratorSliced: resource.MustParse("2Ti"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator:       resource.MustParse("4"),
						CPU:               resource.MustParse("8"),
						AcceleratorShared: resource.MustParse("32Gi"),
						AcceleratorSliced: resource.MustParse("500Gi"),
					}},
				},
			},
		}

		item.Recompute()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.Equal(resource.MustParse("4")), "Accelerator must be the max")
		assert.True(t, o.CPU.Equal(resource.MustParse("8")),
			"CPU must come from the Acc=4 tier (8), not the per-dim max (64)")
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("32Gi")),
			"AcceleratorShared must come from the Acc=4 tier (32Gi), not the per-dim max (256Gi)")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("500Gi")),
			"AcceleratorSliced must come from the Acc=4 tier (500Gi), not the per-dim max (2Ti)")
	})

	t.Run("cpu-only: high-CPU tier wins even when another tier has higher AcceleratorShared", func(t *testing.T) {
		// Synthetic two-tier CPU-only item: in practice CPU-only items collapse to one tier,
		// but the function must still produce a coherent bundle from the high-CPU tier.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: false},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU:               resource.MustParse("8"),
						AcceleratorShared: resource.MustParse("128Gi"),
						AcceleratorSliced: resource.MustParse("4Ti"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU:               resource.MustParse("32"),
						AcceleratorShared: resource.MustParse("16Gi"),
						AcceleratorSliced: resource.MustParse("200Gi"),
					}},
				},
			},
		}

		item.Recompute()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.CPU.Equal(resource.MustParse("32")), "CPU must be the max")
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("16Gi")),
			"AcceleratorShared must come from the high-CPU tier (16Gi), not the per-dim max (128Gi)")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("200Gi")),
			"AcceleratorSliced must come from the high-CPU tier (200Gi), not the per-dim max (4Ti)")
	})

	t.Run("acceleratable: fully-sliced tier with zero Accelerator still yields its bundle", func(t *testing.T) {
		// A sliceable accelerator whose whole card is fully sliced has Accelerator=0 but a
		// non-zero AcceleratorSliced. The seeded first tier must still be picked so the
		// AcceleratorSliced is not dropped to zero.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator:       resource.MustParse("0"),
						AcceleratorShared: resource.MustParse("0"),
						AcceleratorSliced: resource.MustParse("50"),
						CPU:               resource.MustParse("0"),
					}},
				},
			},
		}

		item.Recompute()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.IsZero())
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("50")),
			"AcceleratorSliced must survive even though the primary dimension is zero")
	})

	t.Run("empty tiers leaves overview zeroed", func(t *testing.T) {
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
		}

		item.Recompute()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.IsZero())
		assert.True(t, o.CPU.IsZero())
		assert.True(t, o.AcceleratorShared.IsZero())
		assert.True(t, o.AcceleratorSliced.IsZero())
	})
}

// TestAggregatedInstanceTypeOnceMaxRequestTier_Recompute_BundleSemantics covers the
// rule that the tier-level OnceMaxRequest is the bundle of the candidate with the
// largest primary dimension (Accelerator if acceleratable, otherwise CPU).
func TestAggregatedInstanceTypeOnceMaxRequestTier_Recompute_BundleSemantics(t *testing.T) {
	t.Run("cpu-only: high-CPU candidate wins bundle even when another candidate has higher AcceleratorShared", func(t *testing.T) {
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "fat-ram", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("512Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1Ti")},
				},
				{
					Cluster: "cluster-b", Name: "fat-cpu", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("200Gi")},
				},
			},
		}

		tier.Recompute(false)

		o := tier.OnceMaxRequest
		assert.True(t, o.CPU.Equal(resource.MustParse("64")), "CPU must be max")
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("64Gi")),
			"AcceleratorShared must come from the high-CPU candidate (64Gi), not the per-dim max (512Gi)")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("200Gi")),
			"AcceleratorSliced must come from the high-CPU candidate (200Gi), not the per-dim max (1Ti)")
	})

	t.Run("acceleratable: ties on Accelerator keep the first-seen candidate's bundle", func(t *testing.T) {
		// All candidates in one acceleratable tier share the same accelerator OnceMaxRequest,
		// so the comparison `Cmp(...) < 0` is never true after the first one; the bundle is
		// fixed by the first candidate. Documenting this invariant via test.
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "first", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("2")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("8")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("32Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("500Gi")},
				},
				{
					Cluster: "cluster-b", Name: "second", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("2")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("16")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1Ti")},
				},
			},
		}

		tier.Recompute(true)

		o := tier.OnceMaxRequest
		assert.True(t, o.Accelerator.Equal(resource.MustParse("2")))
		assert.True(t, o.CPU.Equal(resource.MustParse("8")), "ties keep first-seen candidate's CPU")
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("32Gi")), "ties keep first-seen candidate's AcceleratorShared")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("500Gi")), "ties keep first-seen candidate's storage")
	})

	t.Run("acceleratable: single fully-sliced candidate with zero Accelerator keeps its bundle", func(t *testing.T) {
		// The reported case: one A10g node whose single card is fully sliced. The whole-card
		// Accelerator OnceMaxRequest is zero, but 50% is sliced out. The candidate must still
		// seed the bundle so AcceleratorSliced=50 is not dropped to zero.
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "1", Name: "gpustack-nvidia-a10g-linux-amd64", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("50")},
				},
			},
		}

		tier.Recompute(true)

		o := tier.OnceMaxRequest
		assert.True(t, o.Accelerator.IsZero())
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("50")),
			"AcceleratorSliced must survive even though the primary dimension is zero")
	})
}

// TestAggregatedInstanceType_Recompute_RemainingSum covers the rule that the
// item-level Remaining is the per-dimension sum across all tiers, independent of
// which tier wins the OnceMaxRequest bundle.
func TestAggregatedInstanceType_Recompute_RemainingSum(t *testing.T) {
	t.Run("sums Remaining across tiers regardless of primary dimension", func(t *testing.T) {
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{Remaining: AggregatedInstanceTypeOverviewResource{
						Accelerator:       resource.MustParse("2"),
						CPU:               resource.MustParse("16"),
						AcceleratorShared: resource.MustParse("32Gi"),
						AcceleratorSliced: resource.MustParse("500Gi"),
					}},
					{Remaining: AggregatedInstanceTypeOverviewResource{
						Accelerator:       resource.MustParse("4"),
						CPU:               resource.MustParse("48"),
						AcceleratorShared: resource.MustParse("128Gi"),
						AcceleratorSliced: resource.MustParse("1Ti"),
					}},
				},
			},
		}

		item.Recompute()

		r := item.Status.Remaining
		assert.True(t, r.Accelerator.Equal(resource.MustParse("6")), "Accelerator must be sum (2+4)")
		assert.True(t, r.CPU.Equal(resource.MustParse("64")), "CPU must be sum (16+48)")
		assert.True(t, r.AcceleratorShared.Equal(resource.MustParse("160Gi")), "AcceleratorShared must be sum (32Gi+128Gi)")
		assert.True(t, r.AcceleratorSliced.Equal(resource.MustParse("1524Gi")), "AcceleratorSliced must be sum (500Gi+1Ti=500Gi+1024Gi)")
	})

	t.Run("empty tiers leaves Remaining zeroed", func(t *testing.T) {
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
		}

		item.Recompute()

		r := item.Status.Remaining
		assert.True(t, r.Accelerator.IsZero())
		assert.True(t, r.CPU.IsZero())
		assert.True(t, r.AcceleratorShared.IsZero())
		assert.True(t, r.AcceleratorSliced.IsZero())
	})
}

// TestAggregatedInstanceTypeOnceMaxRequestTier_Recompute_RemainingSum covers the
// rule that the tier-level Remaining is the per-dimension sum across all candidates
// in the tier, independent of which candidate wins the OnceMaxRequest bundle.
func TestAggregatedInstanceTypeOnceMaxRequestTier_Recompute_RemainingSum(t *testing.T) {
	t.Run("sums Remaining across candidates", func(t *testing.T) {
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "small", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1"), Remaining: resource.MustParse("1")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("4"), Remaining: resource.MustParse("4")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("16Gi"), Remaining: resource.MustParse("16Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("200Gi"), Remaining: resource.MustParse("200Gi")},
				},
				{
					Cluster: "cluster-b", Name: "big", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1"), Remaining: resource.MustParse("3")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("8"), Remaining: resource.MustParse("12")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("32Gi"), Remaining: resource.MustParse("48Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("500Gi"), Remaining: resource.MustParse("800Gi")},
				},
			},
		}

		tier.Recompute(true)

		r := tier.Remaining
		assert.True(t, r.Accelerator.Equal(resource.MustParse("4")), "Accelerator must be sum (1+3)")
		assert.True(t, r.CPU.Equal(resource.MustParse("16")), "CPU must be sum (4+12)")
		assert.True(t, r.AcceleratorShared.Equal(resource.MustParse("64Gi")), "AcceleratorShared must be sum (16Gi+48Gi)")
		assert.True(t, r.AcceleratorSliced.Equal(resource.MustParse("1000Gi")), "AcceleratorSliced must be sum (200Gi+800Gi)")
	})

	t.Run("Remaining is independent of OnceMaxRequest winner", func(t *testing.T) {
		// Candidate 'fat-cpu' wins OnceMaxRequest (CPU is the primary), but Remaining
		// must still aggregate both candidates.
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "fat-ram", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0"), Remaining: resource.MustParse("0")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("4"), Remaining: resource.MustParse("10")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("512Gi"), Remaining: resource.MustParse("1Ti")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1Ti"), Remaining: resource.MustParse("2Ti")},
				},
				{
					Cluster: "cluster-b", Name: "fat-cpu", Phase: "Active",
					Accelerator:       AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0"), Remaining: resource.MustParse("0")},
					CPU:               AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64"), Remaining: resource.MustParse("128")},
					AcceleratorShared: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64Gi"), Remaining: resource.MustParse("256Gi")},
					AcceleratorSliced: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("200Gi"), Remaining: resource.MustParse("400Gi")},
				},
			},
		}

		tier.Recompute(false)

		// OnceMaxRequest still picks fat-cpu's bundle (CPU is the primary dimension).
		assert.True(t, tier.OnceMaxRequest.CPU.Equal(resource.MustParse("64")))
		assert.True(t, tier.OnceMaxRequest.AcceleratorShared.Equal(resource.MustParse("64Gi")))

		// Remaining aggregates both candidates regardless of who won.
		r := tier.Remaining
		assert.True(t, r.CPU.Equal(resource.MustParse("138")), "CPU must be sum (10+128)")
		assert.True(t, r.AcceleratorShared.Equal(resource.MustParse("1280Gi")), "AcceleratorShared must be sum (1Ti+256Gi)")
		assert.True(t, r.AcceleratorSliced.Equal(resource.MustParse("2448Gi")), "AcceleratorSliced must be sum (2Ti+400Gi)")
	})
}

// TestAggregatedInstanceType_LessTierByPrimary verifies the sort comparator picks
// the right dimension: Accelerator when acceleratable, otherwise CPU. All three
// sort sites (Result, Handle's cross-tier move, Handle's new-tier append) share
// this comparator, so locking its behavior here protects every site at once.
func TestAggregatedInstanceType_LessTierByPrimary(t *testing.T) {
	t.Run("acceleratable: compares Accelerator, ignores CPU", func(t *testing.T) {
		// Tier 0 has higher CPU but lower Accelerator; the helper must still
		// place it before tier 1 because Accelerator is the primary dimension.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator: resource.MustParse("1"),
						CPU:         resource.MustParse("64"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator: resource.MustParse("4"),
						CPU:         resource.MustParse("8"),
					}},
				},
			},
		}

		assert.True(t, item.lessTierByPrimary(0, 1), "Acc=1 must come before Acc=4")
		assert.False(t, item.lessTierByPrimary(1, 0))
	})

	t.Run("cpu-only: compares CPU", func(t *testing.T) {
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: false},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU: resource.MustParse("8"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU: resource.MustParse("32"),
					}},
				},
			},
		}

		assert.True(t, item.lessTierByPrimary(0, 1), "CPU=8 must come before CPU=32")
		assert.False(t, item.lessTierByPrimary(1, 0))
	})
}

// TestListAggregateInstanceTypes_Result_BundleAggregation drives the full Next/Result path
// and asserts that the item-level OnceMaxRequest matches the bundle of the high-primary
// candidate end-to-end.
func TestListAggregateInstanceTypes_Result_BundleAggregation(t *testing.T) {
	t.Run("acceleratable item picks bundle from the highest-Acc tier", func(t *testing.T) {
		// Two A10G candidates: Acc=1 with fat CPU/AcceleratorShared, Acc=4 with lean CPU/AcceleratorShared.
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", a10gInstCustom("a10g-fat-1", "1", "64", "256Gi", "2Ti")))
		require.NoError(t, op.Next("cluster-b", a10gInstCustom("a10g-lean-4", "4", "8", "32Gi", "500Gi")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		assert.Equal(t, "gpustack-nvidia-a10g", item.Name)
		require.Len(t, item.Status.Tiers, 2)

		// Tier ordering is ascending by primary (Acc).
		assert.True(t, item.Status.Tiers[0].OnceMaxRequest.Accelerator.Equal(resource.MustParse("1")))
		assert.True(t, item.Status.Tiers[1].OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")))

		// Item overview must equal the Acc=4 tier's bundle, not per-dim max.
		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.Equal(resource.MustParse("4")))
		assert.True(t, o.CPU.Equal(resource.MustParse("8")), "must not pull CPU=64 from the Acc=1 tier")
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("32Gi")), "must not pull AcceleratorShared=256Gi from the Acc=1 tier")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("500Gi")), "must not pull storage=2Ti from the Acc=1 tier")
	})

	t.Run("cpu-only item picks tier-level bundle from the highest-CPU candidate", func(t *testing.T) {
		// All CPU-only candidates collapse into one tier (Acc=0). Within the tier,
		// the candidate with the highest CPU defines the tier-level and item-level bundle.
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", cpuOnlyInstCustom("cpu-fat-ram", "4", "512Gi", "1Ti")))
		require.NoError(t, op.Next("cluster-b", cpuOnlyInstCustom("cpu-fat-cpu", "64", "64Gi", "200Gi")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		assert.Equal(t, "gpustack-cpu-only", item.Name)
		require.Len(t, item.Status.Tiers, 1, "CPU-only items must collapse into a single tier")
		require.Len(t, item.Status.Tiers[0].Candidates, 2)

		o := item.Status.OnceMaxRequest
		assert.True(t, o.CPU.Equal(resource.MustParse("64")))
		assert.True(t, o.AcceleratorShared.Equal(resource.MustParse("64Gi")), "must not pull AcceleratorShared=512Gi from the lower-CPU candidate")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("200Gi")), "must not pull storage=1Ti from the lower-CPU candidate")
	})

	t.Run("Result sorts CPU-only items' tiers (degenerate single tier remains valid)", func(t *testing.T) {
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", cpuOnlyInst("")))

		result := op.Result(true)

		require.Len(t, result.Items, 1)
		require.Len(t, result.Items[0].Status.Tiers, 1)
	})
}

// TestListAggregateInstanceTypes_Result_RemainingAggregation drives the full Next/Result
// path and asserts that tier-level and item-level Remaining are per-dimension sums across
// all candidates — independent of the bundle-from-winner OnceMaxRequest.
func TestListAggregateInstanceTypes_Result_RemainingAggregation(t *testing.T) {
	t.Run("acceleratable: item Remaining sums across tiers and candidates", func(t *testing.T) {
		// Tier Acc=1 has one candidate; tier Acc=4 has two candidates with identical Acc.
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", a10gInstCustom("a10g-1a", "1", "8", "32Gi", "500Gi")))
		require.NoError(t, op.Next("cluster-b", a10gInstCustom("a10g-4b", "4", "16", "64Gi", "1Ti")))
		require.NoError(t, op.Next("cluster-c", a10gInstCustom("a10g-4c", "4", "32", "128Gi", "2Ti")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		require.Len(t, item.Status.Tiers, 2)

		// Tier Acc=1 sits before tier Acc=4 (ascending by primary).
		tier1 := item.Status.Tiers[0]
		tier4 := item.Status.Tiers[1]
		assert.True(t, tier1.OnceMaxRequest.Accelerator.Equal(resource.MustParse("1")))
		assert.True(t, tier4.OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")))

		// Tier-level Remaining sums each tier's candidates.
		assert.True(t, tier1.Remaining.Accelerator.Equal(resource.MustParse("1")), "tier Acc=1 has one candidate with Acc.Remaining=1")
		assert.True(t, tier1.Remaining.CPU.Equal(resource.MustParse("8")))
		assert.True(t, tier4.Remaining.Accelerator.Equal(resource.MustParse("8")), "tier Acc=4 has two candidates with Acc.Remaining=4 each")
		assert.True(t, tier4.Remaining.CPU.Equal(resource.MustParse("48")), "16+32")
		assert.True(t, tier4.Remaining.AcceleratorShared.Equal(resource.MustParse("192Gi")), "64Gi+128Gi")
		assert.True(t, tier4.Remaining.AcceleratorSliced.Equal(resource.MustParse("3Ti")), "1Ti+2Ti")

		// Item-level Remaining sums across both tiers.
		r := item.Status.Remaining
		assert.True(t, r.Accelerator.Equal(resource.MustParse("9")), "1 + (4+4)")
		assert.True(t, r.CPU.Equal(resource.MustParse("56")), "8 + (16+32)")
		assert.True(t, r.AcceleratorShared.Equal(resource.MustParse("224Gi")), "32Gi + (64Gi+128Gi)")
	})

	t.Run("cpu-only: item Remaining sums across candidates in single tier", func(t *testing.T) {
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", cpuOnlyInstCustom("cpu-a", "4", "16Gi", "200Gi")))
		require.NoError(t, op.Next("cluster-b", cpuOnlyInstCustom("cpu-b", "64", "256Gi", "1Ti")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		require.Len(t, item.Status.Tiers, 1)

		r := item.Status.Remaining
		assert.True(t, r.Accelerator.IsZero(), "CPU-only items have no accelerator")
		assert.True(t, r.CPU.Equal(resource.MustParse("68")), "4+64")
		assert.True(t, r.AcceleratorShared.Equal(resource.MustParse("272Gi")), "16Gi+256Gi")
		assert.True(t, r.AcceleratorSliced.Equal(resource.MustParse("1224Gi")), "200Gi+1Ti")

		// Tier-level Remaining matches item-level for the single tier.
		assert.True(t, item.Status.Tiers[0].Remaining.CPU.Equal(r.CPU))
		assert.True(t, item.Status.Tiers[0].Remaining.AcceleratorShared.Equal(r.AcceleratorShared))
		assert.True(t, item.Status.Tiers[0].Remaining.AcceleratorSliced.Equal(r.AcceleratorSliced))
	})
}

// TestListAggregateInstanceTypes_PhaseFiltering covers the batch (Next/Result) path: only
// Active candidates contribute to the tier/item OnceMaxRequest and Remaining totals, while
// non-Active candidates stay listed with their recorded Phase.
func TestListAggregateInstanceTypes_PhaseFiltering(t *testing.T) {
	t.Run("inactive candidate is retained but excluded from tier/item totals", func(t *testing.T) {
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", a10gInstCustom("a10g-active", "4", "8", "32Gi", "500Gi")))
		require.NoError(t, op.Next("cluster-b",
			withPhase(a10gInstCustom("a10g-inactive", "4", "16", "64Gi", "1Ti"), "Inactive")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		require.Len(t, item.Status.Tiers, 1, "both candidates share Acc=4, so a single tier")
		tier := item.Status.Tiers[0]
		require.Len(t, tier.Candidates, 2, "the inactive candidate is retained, not dropped")

		byName := map[string]AggregatedInstanceTypeOnceMaxRequestCandidate{}
		for _, c := range tier.Candidates {
			byName[c.Name] = c
		}
		assert.Equal(t, "Active", byName["a10g-active"].Phase)
		assert.Equal(t, "Inactive", byName["a10g-inactive"].Phase, "candidate Phase must be recorded")

		// Totals reflect the Active candidate only (CPU=8), never the inactive one (CPU=16).
		assert.True(t, tier.OnceMaxRequest.CPU.Equal(resource.MustParse("8")),
			"OnceMaxRequest must not pull CPU=16 from the inactive candidate")
		assert.True(t, tier.Remaining.CPU.Equal(resource.MustParse("8")),
			"Remaining must sum Active candidates only")
		assert.True(t, item.Status.Remaining.CPU.Equal(resource.MustParse("8")))
		assert.True(t, item.Status.OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")))
	})

	t.Run("all-inactive tier zeroes its stats but keeps its candidates and stable identity", func(t *testing.T) {
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", withPhase(a10gInst("inst-a", "4"), "Inactive")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		require.Len(t, item.Status.Tiers, 1)
		require.Len(t, item.Status.Tiers[0].Candidates, 1)
		assert.True(t, item.Status.Tiers[0].OnceMaxRequest.Accelerator.IsZero(), "all-inactive tier contributes zero")
		assert.True(t, item.Status.Tiers[0].Remaining.Accelerator.IsZero())
		assert.True(t, item.Status.OnceMaxRequest.Accelerator.IsZero())
		assert.True(t, item.Status.Tiers[0].Candidates[0].Accelerator.OnceMaxRequest.Equal(resource.MustParse("4")),
			"the candidate's raw accelerator is the tier's stable identity")
	})

	t.Run("item overview picks the active fully-sliced tier over an all-inactive whole-card tier", func(t *testing.T) {
		// Seed-collision guard: an inactive whole-card tier (raw Acc=4, zeroed by Phase filtering)
		// and an active fully-sliced tier (raw Acc=0, Sliced=50) both recompute to Accelerator=0.
		// The all-zero inactive tier must not seed the item bundle and mask the 50.
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a",
			withPhase(a10gInstCustom("a10g-whole-inactive", "4", "8", "32Gi", "0"), "Inactive")))
		require.NoError(t, op.Next("cluster-b",
			a10gInstCustom("a10g-sliced-active", "0", "0", "0", "50")))

		result := op.Result(false)

		require.Len(t, result.Items, 1)
		item := result.Items[0]
		require.Len(t, item.Status.Tiers, 2, "distinct raw accelerators keep two tiers")

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.IsZero(), "no whole card is requestable once max")
		assert.True(t, o.AcceleratorSliced.Equal(resource.MustParse("50")),
			"the active fully-sliced tier's bundle must win, not the zeroed inactive tier")
		assert.True(t, item.Status.Remaining.AcceleratorSliced.Equal(resource.MustParse("50")))
		assert.True(t, item.Status.Remaining.CPU.IsZero(), "the inactive whole-card CPU must not count")
	})
}

// TestHandleAggregatedInstanceType_PhaseAwareTierIdentity covers the streaming path: a tier whose
// only candidate is inactive recomputes to zero stats, yet a later same-accelerator Active candidate
// must still join it (matched on the stable candidate value) rather than spawn a duplicate tier.
func TestHandleAggregatedInstanceType_PhaseAwareTierIdentity(t *testing.T) {
	h := OpHandleAggregatedInstanceType(buildState(t,
		seed{cluster: "cluster-a", obj: withPhase(a10gInst("inst-a", "4"), "Inactive")},
	))

	item0 := findItem(h.state, "gpustack-nvidia-a10g")
	require.NotNil(t, item0)
	require.Len(t, item0.Status.Tiers, 1)
	require.True(t, item0.Status.Tiers[0].OnceMaxRequest.Accelerator.IsZero(),
		"the seeded all-inactive tier recomputes to zero stats")

	evts := h.Handle(&manager.WorkerEvent{
		Type:    manager.WorkerEventAdded,
		Cluster: "cluster-b",
		Object:  a10gInst("inst-b", "4"),
	})

	require.Len(t, evts, 1)
	assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

	item := findItem(h.state, "gpustack-nvidia-a10g")
	require.NotNil(t, item)
	require.Len(t, item.Status.Tiers, 1, "the Active candidate must join the existing Acc=4 tier, not duplicate it")
	require.Len(t, item.Status.Tiers[0].Candidates, 2)
	assert.True(t, item.Status.Tiers[0].OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")),
		"tier totals now reflect the Active candidate")
}

// TestHandleAggregatedInstanceType_InactiveCrossTierMoveDoesNotLeakCapacity covers the streaming
// cross-tier move that lands an inactive candidate in a brand-new tier: that tier must be recomputed
// (Phase-filtered to zero) before the item overview aggregates it, so the moved candidate's raw
// resources never surface as fleet capacity.
func TestHandleAggregatedInstanceType_InactiveCrossTierMoveDoesNotLeakCapacity(t *testing.T) {
	h := OpHandleAggregatedInstanceType(buildState(t,
		seed{cluster: "cluster-a", obj: a10gInst("active-a", "4")},
		seed{cluster: "cluster-b", obj: withPhase(a10gInst("inactive-b", "2"), "Inactive")},
	))

	// The inactive candidate's raw accelerator changes to 8, moving it into a brand-new tier.
	evts := h.Handle(&manager.WorkerEvent{
		Type:    manager.WorkerEventModified,
		Cluster: "cluster-b",
		Object:  withPhase(a10gInst("inactive-b", "8"), "Inactive"),
	})

	require.Len(t, evts, 1)
	assert.Equal(t, manager.WorkerEventModified, evts[0].Type)

	item := findItem(h.state, "gpustack-nvidia-a10g")
	require.NotNil(t, item)

	var movedTier *AggregatedInstanceTypeOnceMaxRequestTier
	for i := range item.Status.Tiers {
		if item.Status.Tiers[i].Candidates[0].Accelerator.OnceMaxRequest.Equal(resource.MustParse("8")) {
			movedTier = &item.Status.Tiers[i]
		}
	}
	require.NotNil(t, movedTier, "the moved candidate must occupy its own Acc=8 tier")
	assert.True(t, movedTier.OnceMaxRequest.Accelerator.IsZero(),
		"an inactive candidate contributes zero to its new tier")
	assert.True(t, item.Status.OnceMaxRequest.Accelerator.Equal(resource.MustParse("4")),
		"the inactive candidate's raw Acc=8 must not become the item's OnceMaxRequest")
}

// TestListAggregateInstanceTypes_GroupingIgnoresInactiveAndDisplayName covers the cross-cluster
// grouping identity: the same hardware collapses into one aggregated item even when clusters
// disagree on Inactive/DisplayName, and the stored item keeps the first-seen DisplayName.
func TestListAggregateInstanceTypes_GroupingIgnoresInactiveAndDisplayName(t *testing.T) {
	a := a10gInst("inst-a", "1")
	a.Spec.DisplayName = "A10G Prod"
	b := a10gInst("inst-b", "1")
	b.Spec.Inactive = true
	b.Spec.DisplayName = "A10G Staging"

	state := buildState(t,
		seed{cluster: "cluster-a", obj: a},
		seed{cluster: "cluster-b", obj: b},
	)

	require.Len(t, state.Items, 1, "hardware-identical types must not split on Inactive/DisplayName")
	item := state.Items[0]
	require.Len(t, item.Status.Tiers, 1)
	require.Len(t, item.Status.Tiers[0].Candidates, 2)
	assert.Equal(t, "A10G Prod", item.Spec.DisplayName, "stored DisplayName is the first-seen one")
}

func newFlavor(name string, spec worker.InstanceTypeFlavorSpec) *worker.InstanceTypeFlavor {
	return &worker.InstanceTypeFlavor{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

func flavorSpecA10G() worker.InstanceTypeFlavorSpec {
	return worker.InstanceTypeFlavorSpec{
		AcceleratorGroup: "gpustack-nvidia-a10g",
		Acceleratable:    true,
		Manufacturer:     "nvidia",
		Product:          "NVIDIA A10G",
		Family:           "ampere",
		Memory:           "23028Mi",
	}
}

func flavorSpecTeslaT4() worker.InstanceTypeFlavorSpec {
	return worker.InstanceTypeFlavorSpec{
		AcceleratorGroup: "gpustack-nvidia-tesla-t4",
		Acceleratable:    true,
		Manufacturer:     "nvidia",
		Product:          "Tesla T4",
		Family:           "turing",
		Memory:           "15360Mi",
	}
}

func flavorSpecCPUGroup(group string) worker.InstanceTypeFlavorSpec {
	return worker.InstanceTypeFlavorSpec{
		GeneralGroup:  group,
		Acceleratable: false,
	}
}

func aggFlavorSpec(spec worker.InstanceTypeFlavorSpec, clusters ...string) AggregatedInstanceTypeFlavorSpec {
	return AggregatedInstanceTypeFlavorSpec{
		InstanceTypeFlavorSpec: spec,
		Clusters:               clusters,
	}
}

type flavorSeed struct {
	cluster string
	obj     *worker.InstanceTypeFlavor
}

func TestListClusterInstanceTypeFlavors_Result(t *testing.T) {
	type row struct{ cluster, name string }
	cases := []struct {
		name  string
		seeds []flavorSeed
		want  []row
	}{
		{
			name: "empty yields empty list",
			want: []row{},
		},
		{
			name: "same flavor from two clusters yields one row per cluster",
			seeds: []flavorSeed{
				{cluster: "cluster-a", obj: newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())},
				{cluster: "cluster-b", obj: newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())},
			},
			want: []row{
				{cluster: "cluster-a", name: "gpustack-nvidia-a10g"},
				{cluster: "cluster-b", name: "gpustack-nvidia-a10g"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := OpListClusterInstanceTypeFlavors()
			for _, s := range c.seeds {
				require.NoError(t, op.Next(s.cluster, s.obj))
			}
			result := op.Result()
			got := make([]row, len(result.Items))
			for i, item := range result.Items {
				got[i] = row{cluster: item.Cluster, name: item.Name}
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestListClusterInstanceTypeFlavors_Next(t *testing.T) {
	t.Run("clears ManagedFields", func(t *testing.T) {
		op := OpListClusterInstanceTypeFlavors()
		flavor := newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())
		flavor.ManagedFields = []meta.ManagedFieldsEntry{{Manager: "worker"}}

		require.NoError(t, op.Next("cluster-a", flavor))

		result := op.Result()
		require.Len(t, result.Items, 1)
		assert.Nil(t, result.Items[0].ManagedFields)
	})

	t.Run("rejects a non-flavor object", func(t *testing.T) {
		op := OpListClusterInstanceTypeFlavors()
		assert.Error(t, op.Next("cluster-a", &worker.InstanceType{}))
	})
}

func TestListAggregateInstanceTypeFlavors_Next(t *testing.T) {
	t.Run("identical Spec across clusters collapses to one entry with sorted clusters", func(t *testing.T) {
		op := OpListAggregateInstanceTypeFlavors()
		require.NoError(t, op.Next("cluster-b", newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())))
		require.NoError(t, op.Next("cluster-a", newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())))

		result := op.Result(false)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "gpustack-nvidia-a10g", result.Items[0].Name)
		assert.Equal(t, []string{"cluster-a", "cluster-b"}, result.Items[0].Spec.Clusters)
	})

	t.Run("differing Specs are preserved with their own clusters", func(t *testing.T) {
		op := OpListAggregateInstanceTypeFlavors()
		require.NoError(t, op.Next("cluster-a", newFlavor("gpustack-nvidia-a10g", flavorSpecA10G())))
		require.NoError(t, op.Next("cluster-b", newFlavor("gpustack-nvidia-tesla-t4", flavorSpecTeslaT4())))

		result := op.Result(false)
		require.Len(t, result.Items, 2)
		assert.Equal(t, []string{"cluster-a"}, result.Items[0].Spec.Clusters)
		assert.Equal(t, []string{"cluster-b"}, result.Items[1].Spec.Clusters)
	})

	t.Run("rejects a non-flavor object", func(t *testing.T) {
		op := OpListAggregateInstanceTypeFlavors()
		assert.Error(t, op.Next("cluster-a", &worker.InstanceType{}))
	})
}

func TestListAggregateInstanceTypeFlavors_Result(t *testing.T) {
	cases := []struct {
		name     string
		items    []AggregatedInstanceTypeFlavor
		sorted   bool
		expected []string
	}{
		{
			name:     "empty list + sorted",
			items:    []AggregatedInstanceTypeFlavor{},
			sorted:   true,
			expected: []string{},
		},
		{
			name: "unsorted preserves insertion order",
			items: []AggregatedInstanceTypeFlavor{
				{Name: "gpustack-cpu-only", Spec: aggFlavorSpec(flavorSpecCPUGroup("generic"))},
				{Name: "gpustack-nvidia-tesla-t4", Spec: aggFlavorSpec(flavorSpecTeslaT4())},
				{Name: "gpustack-nvidia-a10g", Spec: aggFlavorSpec(flavorSpecA10G())},
			},
			sorted:   false,
			expected: []string{"gpustack-cpu-only", "gpustack-nvidia-tesla-t4", "gpustack-nvidia-a10g"},
		},
		{
			name: "sorted puts accelerated first then name ascending",
			items: []AggregatedInstanceTypeFlavor{
				{Name: "gpustack-cpu-only", Spec: aggFlavorSpec(flavorSpecCPUGroup("generic"))},
				{Name: "gpustack-nvidia-tesla-t4", Spec: aggFlavorSpec(flavorSpecTeslaT4())},
				{Name: "gpustack-nvidia-a10g", Spec: aggFlavorSpec(flavorSpecA10G())},
			},
			sorted:   true,
			expected: []string{"gpustack-nvidia-a10g", "gpustack-nvidia-tesla-t4", "gpustack-cpu-only"},
		},
		{
			name: "sorted is deterministic within the generic group",
			items: []AggregatedInstanceTypeFlavor{
				{Name: "gpustack-cpu-intel", Spec: aggFlavorSpec(flavorSpecCPUGroup("intel"))},
				{Name: "gpustack-cpu-amd", Spec: aggFlavorSpec(flavorSpecCPUGroup("amd"))},
			},
			sorted:   true,
			expected: []string{"gpustack-cpu-amd", "gpustack-cpu-intel"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := ListAggregateInstanceTypeFlavors{
				list: AggregatedInstanceTypeFlavorList{Items: c.items},
			}
			result := op.Result(c.sorted)
			actual := make([]string, len(result.Items))
			for i, item := range result.Items {
				actual[i] = item.Name
			}
			assert.Equal(t, c.expected, actual)
		})
	}
}
