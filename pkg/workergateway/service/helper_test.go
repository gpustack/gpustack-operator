package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
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

func instTypeRes(once, remaining, capacity string) worker.InstanceTypeResource {
	return worker.InstanceTypeResource{
		OnceMaxRequest: resource.MustParse(once),
		Remaining:      resource.MustParse(remaining),
		Capacity:       resource.MustParse(capacity),
	}
}

func instSpecCPUOnly() worker.InstanceTypeSpec {
	return worker.InstanceTypeSpec{
		Group:         "gpustack-cpu-only",
		Acceleratable: false,
	}
}

func instSpecA10G() worker.InstanceTypeSpec {
	return worker.InstanceTypeSpec{
		Group:             "gpustack-nvidia-a10g",
		Acceleratable:     true,
		Manufacturer:      "nvidia",
		Product:           "NVIDIA-A10G",
		Memory:            "23028Mi",
		Family:            "Ampere",
		ComputeCapability: "8.6",
	}
}

func instSpecTeslaT4() worker.InstanceTypeSpec {
	return worker.InstanceTypeSpec{
		Group:             "gpustack-nvidia-tesla-t4",
		Acceleratable:     true,
		Manufacturer:      "nvidia",
		Product:           "Tesla-T4",
		Memory:            "15360Mi",
		Family:            "Turing",
		ComputeCapability: "7.5",
	}
}

func instStatusCPU() worker.InstanceTypeStatus {
	return worker.InstanceTypeStatus{
		Phase:        "Active",
		Accelerator:  instTypeRes("0", "0", "0"),
		CPU:          instTypeRes("16", "16", "16"),
		RAM:          instTypeRes("32135984Ki", "32135984Ki", "32135984Ki"),
		LocalStorage: instTypeRes("104779756Ki", "104779756Ki", "104779756Ki"),
	}
}

func instStatusGPU(acc string) worker.InstanceTypeStatus {
	return worker.InstanceTypeStatus{
		Phase:        "Active",
		Accelerator:  instTypeRes(acc, acc, acc),
		CPU:          instTypeRes("4", "4", "4"),
		RAM:          instTypeRes("16164772Ki", "16164772Ki", "16164772Ki"),
		LocalStorage: instTypeRes("104779756Ki", "104779756Ki", "104779756Ki"),
	}
}

func newInstType(genName, name string, spec worker.InstanceTypeSpec, status worker.InstanceTypeStatus) *worker.InstanceType {
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

// a10gInstCustom returns an A10G instance type whose CPU/RAM/storage can be overridden.
// Used to construct scenarios where per-dimension max diverges from bundle-from-winner.
func a10gInstCustom(name, acc, cpu, ram, storage string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-nvidia-a10g-hcjmv"
	}
	status := worker.InstanceTypeStatus{
		Phase:        "Active",
		Accelerator:  instTypeRes(acc, acc, acc),
		CPU:          instTypeRes(cpu, cpu, cpu),
		RAM:          instTypeRes(ram, ram, ram),
		LocalStorage: instTypeRes(storage, storage, storage),
	}
	return newInstType("gpustack-nvidia-a10g-", name, instSpecA10G(), status)
}

// cpuOnlyInstCustom returns a CPU-only instance type whose CPU/RAM/storage can be overridden.
func cpuOnlyInstCustom(name, cpu, ram, storage string) *worker.InstanceType {
	if name == "" {
		name = "gpustack-cpu-only-h7vkb"
	}
	status := worker.InstanceTypeStatus{
		Phase:        "Active",
		Accelerator:  instTypeRes("0", "0", "0"),
		CPU:          instTypeRes(cpu, cpu, cpu),
		RAM:          instTypeRes(ram, ram, ram),
		LocalStorage: instTypeRes(storage, storage, storage),
	}
	return newInstType("gpustack-cpu-only-", name, instSpecCPUOnly(), status)
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

// TestAggregatedInstanceType_RecomputeOnceMaxRequest_BundleSemantics covers the rule
// that the item-level OnceMaxRequest is a coherent bundle from the tier with the
// largest primary dimension, not a per-dimension max across tiers.
func TestAggregatedInstanceType_RecomputeOnceMaxRequest_BundleSemantics(t *testing.T) {
	t.Run("acceleratable: high-Acc tier wins even when another tier has higher CPU", func(t *testing.T) {
		// Tier Acc=1 has the higher CPU/RAM, but tier Acc=4 wins on the primary dimension.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator:  resource.MustParse("1"),
						CPU:          resource.MustParse("64"),
						RAM:          resource.MustParse("256Gi"),
						LocalStorage: resource.MustParse("2Ti"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						Accelerator:  resource.MustParse("4"),
						CPU:          resource.MustParse("8"),
						RAM:          resource.MustParse("32Gi"),
						LocalStorage: resource.MustParse("500Gi"),
					}},
				},
			},
		}

		item.RecomputeOnceMaxRequest()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.Equal(resource.MustParse("4")), "Accelerator must be the max")
		assert.True(t, o.CPU.Equal(resource.MustParse("8")),
			"CPU must come from the Acc=4 tier (8), not the per-dim max (64)")
		assert.True(t, o.RAM.Equal(resource.MustParse("32Gi")),
			"RAM must come from the Acc=4 tier (32Gi), not the per-dim max (256Gi)")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("500Gi")),
			"LocalStorage must come from the Acc=4 tier (500Gi), not the per-dim max (2Ti)")
	})

	t.Run("cpu-only: high-CPU tier wins even when another tier has higher RAM", func(t *testing.T) {
		// Synthetic two-tier CPU-only item: in practice CPU-only items collapse to one tier,
		// but the function must still produce a coherent bundle from the high-CPU tier.
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: false},
			Status: AggregatedInstanceTypeStatus{
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU:          resource.MustParse("8"),
						RAM:          resource.MustParse("128Gi"),
						LocalStorage: resource.MustParse("4Ti"),
					}},
					{OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
						CPU:          resource.MustParse("32"),
						RAM:          resource.MustParse("16Gi"),
						LocalStorage: resource.MustParse("200Gi"),
					}},
				},
			},
		}

		item.RecomputeOnceMaxRequest()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.CPU.Equal(resource.MustParse("32")), "CPU must be the max")
		assert.True(t, o.RAM.Equal(resource.MustParse("16Gi")),
			"RAM must come from the high-CPU tier (16Gi), not the per-dim max (128Gi)")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("200Gi")),
			"LocalStorage must come from the high-CPU tier (200Gi), not the per-dim max (4Ti)")
	})

	t.Run("empty tiers leaves overview zeroed", func(t *testing.T) {
		item := AggregatedInstanceType{
			Spec: AggregatedInstanceTypeSpec{Acceleratable: true},
		}

		item.RecomputeOnceMaxRequest()

		o := item.Status.OnceMaxRequest
		assert.True(t, o.Accelerator.IsZero())
		assert.True(t, o.CPU.IsZero())
		assert.True(t, o.RAM.IsZero())
		assert.True(t, o.LocalStorage.IsZero())
	})
}

// TestAggregatedInstanceTypeOnceMaxRequestTier_RecomputeOnceMaxRequest_BundleSemantics
// covers the rule that the tier-level OnceMaxRequest is the bundle of the candidate
// with the largest primary dimension (Accelerator if acceleratable, otherwise CPU).
func TestAggregatedInstanceTypeOnceMaxRequestTier_RecomputeOnceMaxRequest_BundleSemantics(t *testing.T) {
	t.Run("cpu-only: high-CPU candidate wins bundle even when another candidate has higher RAM", func(t *testing.T) {
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "fat-ram",
					Accelerator:  AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					CPU:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
					RAM:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("512Gi")},
					LocalStorage: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1Ti")},
				},
				{
					Cluster: "cluster-b", Name: "fat-cpu",
					Accelerator:  AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("0")},
					CPU:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64")},
					RAM:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64Gi")},
					LocalStorage: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("200Gi")},
				},
			},
		}

		tier.RecomputeOnceMaxRequest(false)

		o := tier.OnceMaxRequest
		assert.True(t, o.CPU.Equal(resource.MustParse("64")), "CPU must be max")
		assert.True(t, o.RAM.Equal(resource.MustParse("64Gi")),
			"RAM must come from the high-CPU candidate (64Gi), not the per-dim max (512Gi)")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("200Gi")),
			"LocalStorage must come from the high-CPU candidate (200Gi), not the per-dim max (1Ti)")
	})

	t.Run("acceleratable: ties on Accelerator keep the first-seen candidate's bundle", func(t *testing.T) {
		// All candidates in one acceleratable tier share the same accelerator OnceMaxRequest,
		// so the comparison `Cmp(...) < 0` is never true after the first one; the bundle is
		// fixed by the first candidate. Documenting this invariant via test.
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster: "cluster-a", Name: "first",
					Accelerator:  AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("2")},
					CPU:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("8")},
					RAM:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("32Gi")},
					LocalStorage: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("500Gi")},
				},
				{
					Cluster: "cluster-b", Name: "second",
					Accelerator:  AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("2")},
					CPU:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("16")},
					RAM:          AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("64Gi")},
					LocalStorage: AggregatedInstanceTypeResource{OnceMaxRequest: resource.MustParse("1Ti")},
				},
			},
		}

		tier.RecomputeOnceMaxRequest(true)

		o := tier.OnceMaxRequest
		assert.True(t, o.Accelerator.Equal(resource.MustParse("2")))
		assert.True(t, o.CPU.Equal(resource.MustParse("8")), "ties keep first-seen candidate's CPU")
		assert.True(t, o.RAM.Equal(resource.MustParse("32Gi")), "ties keep first-seen candidate's RAM")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("500Gi")), "ties keep first-seen candidate's storage")
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
		// Two A10G candidates: Acc=1 with fat CPU/RAM, Acc=4 with lean CPU/RAM.
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
		assert.True(t, o.RAM.Equal(resource.MustParse("32Gi")), "must not pull RAM=256Gi from the Acc=1 tier")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("500Gi")), "must not pull storage=2Ti from the Acc=1 tier")
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
		assert.True(t, o.RAM.Equal(resource.MustParse("64Gi")), "must not pull RAM=512Gi from the lower-CPU candidate")
		assert.True(t, o.LocalStorage.Equal(resource.MustParse("200Gi")), "must not pull storage=1Ti from the lower-CPU candidate")
	})

	t.Run("Result sorts CPU-only items' tiers (degenerate single tier remains valid)", func(t *testing.T) {
		op := OpListAggregateInstanceTypes()
		require.NoError(t, op.Next("cluster-a", cpuOnlyInst("")))

		result := op.Result(true)

		require.Len(t, result.Items, 1)
		require.Len(t, result.Items[0].Status.Tiers, 1)
	})
}
