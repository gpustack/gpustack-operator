package worker

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// fitDemandContainer is one container of a fit-demand case: its requests and limits, and whether it
// is an init container.
type fitDemandContainer struct {
	init     bool
	requests core.ResourceList
	limits   core.ResourceList
}

func fitDemandPodSet(ctrs ...fitDemandContainer) *kueue.PodSet {
	ps := &kueue.PodSet{Name: "main", Count: 3}
	for i, c := range ctrs {
		ctr := core.Container{
			Name:      "c" + strconv.Itoa(i),
			Resources: core.ResourceRequirements{Requests: c.requests, Limits: c.limits},
		}
		if c.init {
			ps.Template.Spec.InitContainers = append(ps.Template.Spec.InitContainers, ctr)
		} else {
			ps.Template.Spec.Containers = append(ps.Template.Spec.Containers, ctr)
		}
	}
	return ps
}

func TestPodSetFitDemand(t *testing.T) {
	const (
		sliced = core.ResourceName("nvidia.com/gpu.sliced")
		units  = core.ResourceName("nvidia.com/gpu.sliced.units")
		shared = core.ResourceName("nvidia.com/gpu.shared")
		excl   = core.ResourceName("nvidia.com/gpu")
		part   = core.ResourceName("nvidia.com/gpu.partitioned")
	)

	cases := []struct {
		name       string
		podSet     *kueue.PodSet
		wantUnits  int32
		wantShared int32
	}{
		{
			name:      "a slice is read per Pod, not scaled by the Pod count",
			podSet:    fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{sliced: qty("1"), units: qty("800000")}}),
			wantUnits: 800000,
		},
		{
			name:       "a shared request of one card",
			podSet:     fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{shared: qty("1")}}),
			wantShared: 1,
		},
		{
			name:       "a shared request of three cards",
			podSet:     fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{shared: qty("3")}}),
			wantShared: 3,
		},
		{
			name: "two shared holders need the larger count, not the sum",
			podSet: fitDemandPodSet(
				fitDemandContainer{requests: core.ResourceList{shared: qty("2")}},
				fitDemandContainer{requests: core.ResourceList{shared: qty("3")}},
			),
			wantShared: 3,
		},
		{
			name:   "exclusive asks for no fit pin",
			podSet: fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{excl: qty("2")}}),
		},
		{
			name: "a partition asks for no fit pin",
			podSet: fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{
				part: qty("1"), "nvidia.com/gpu.partitioned.mig-1g.10gb": qty("1"),
			}}),
		},
		{
			name:   "no accelerator",
			podSet: fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{core.ResourceCPU: qty("1")}}),
		},
		{
			name: "an init container's slice counts",
			podSet: fitDemandPodSet(fitDemandContainer{
				init: true, requests: core.ResourceList{sliced: qty("1"), units: qty("480000")},
			}),
			wantUnits: 480000,
		},
		{
			name:      "a slice set only in limits counts",
			podSet:    fitDemandPodSet(fitDemandContainer{limits: core.ResourceList{sliced: qty("1"), units: qty("640000")}}),
			wantUnits: 640000,
		},
		{
			name: "two containers with different units take the larger",
			podSet: fitDemandPodSet(
				fitDemandContainer{requests: core.ResourceList{sliced: qty("1"), units: qty("320000")}},
				fitDemandContainer{requests: core.ResourceList{units: qty("960000")}},
			),
			wantUnits: 960000,
		},
		{
			name:      "units above the int32 range are clamped",
			podSet:    fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{sliced: qty("1"), units: qty("9223372036854775807")}}),
			wantUnits: math.MaxInt32,
		},
		{
			name:   "units with no card request behind them ask for nothing",
			podSet: fitDemandPodSet(fitDemandContainer{requests: core.ResourceList{units: qty("800000")}}),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotUnits, gotShared := PodSetFitDemand(c.podSet)
			assert.Equal(t, c.wantUnits, gotUnits, "sliced units")
			assert.Equal(t, c.wantShared, gotShared, "shared cards")
		})
	}
}
