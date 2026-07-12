package apistatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

func TestGetSummaryOfClusterQueue(t *testing.T) {
	reservedStatus := kueue.ClusterQueueStatus{
		FlavorsReservation: []kueue.FlavorUsage{
			{
				Name: "flavor-a",
				Resources: []kueue.ResourceUsage{
					{Name: "cpu", Total: resource.MustParse("4")},
				},
			},
		},
	}

	cases := []struct {
		name string

		cq *kueue.ClusterQueue

		wantPhase   string
		wantMessage string // asserted only when non-empty
	}{
		{
			name: "hold and drain with reserved is draining",
			cq: &kueue.ClusterQueue{
				Spec:   kueue.ClusterQueueSpec{StopPolicy: ptr.To(kueue.HoldAndDrain)},
				Status: reservedStatus,
			},
			wantPhase:   "Draining",
			wantMessage: "cluster queue is draining admitted workloads",
		},
		{
			name: "hold and drain fully drained is inactive",
			cq: &kueue.ClusterQueue{
				Spec: kueue.ClusterQueueSpec{StopPolicy: ptr.To(kueue.HoldAndDrain)},
			},
			wantPhase:   "Inactive",
			wantMessage: "cluster queue is held and drained",
		},
		{
			name: "hold is inactive regardless of reservation",
			cq: &kueue.ClusterQueue{
				Spec:   kueue.ClusterQueueSpec{StopPolicy: ptr.To(kueue.Hold)},
				Status: reservedStatus,
			},
			wantPhase:   "Inactive",
			wantMessage: "cluster queue is held",
		},
		{
			name: "none delegates to the active condition summary",
			cq: &kueue.ClusterQueue{
				Spec: kueue.ClusterQueueSpec{StopPolicy: ptr.To(kueue.None)},
				Status: kueue.ClusterQueueStatus{
					Conditions: []meta.Condition{
						{Type: string(ClusterQueueConditionActive), Status: meta.ConditionTrue},
					},
				},
			},
			wantPhase: "Active",
		},
		{
			name: "nil stop policy delegates to the condition summary",
			cq: &kueue.ClusterQueue{
				Status: kueue.ClusterQueueStatus{
					Conditions: []meta.Condition{
						{Type: string(ClusterQueueConditionActive), Status: meta.ConditionFalse},
					},
				},
			},
			wantPhase: "Inactive",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			phase, message := GetSummaryOfClusterQueue(c.cq)
			assert.Equal(t, c.wantPhase, phase, "phase")
			if c.wantMessage != "" {
				assert.Equal(t, c.wantMessage, message, "message")
			}
		})
	}
}
