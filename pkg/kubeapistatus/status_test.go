package kubeapistatus

import (
	"strings"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/assert"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type _Status struct {
	Phase        string
	PhaseMessage string
	Conditions   []meta.Condition
}

func (in *_Status) SetPhase(phase string) {
	in.Phase = phase
}

func (in *_Status) SetPhaseMessage(phaseMessage string) {
	in.PhaseMessage = phaseMessage
}

func TestWalker_sxs(t *testing.T) {
	// 1. Define resource with status.
	type ExampleResource struct {
		Status _Status
	}

	// 2. Define the condition types of ExampleResource,
	// condition type can be past tense or present tense.
	const (
		ExampleResourceStatusProgressing    ConditionType = "Progressing"
		ExampleResourceStatusReplicaFailure ConditionType = "ReplicaFailure"
		ExampleResourceStatusAvailable      ConditionType = "Available"
	)

	// 2.1  clarify the condition type and its status meaning as below.
	//      | Condition Type |     Condition Status    | Human Readable Status | Human Sensible Status |
	//      | -------------- | ----------------------- | --------------------- | --------------------- |
	//      | Progressing    | Unknown                 | Progressing           | PhaseIsTransitioning  |
	//      | Progressing    | False                   | Progressing           | Error                 |
	//      | Progressing    | True(ReplicaSetUpdated) | Progressing           | PhaseIsTransitioning  |
	//      | Progressing    | True(DeploymentPaused)  | Pausing               | PhaseIsTransitioning  |
	//      | Progressing    | True                    | Progressed            | Done                  |
	//      | ReplicaFailure | Unknown                 | ReplicaDeploying      | PhaseIsTransitioning  |
	//      | ReplicaFailure | False                   | ReplicaDeployed       | Done                  |
	//      | ReplicaFailure | True                    | ReplicaDeployFailed   | Error                 |
	//      | Available      | Unknown                 | Preparing             | PhaseIsTransitioning  |
	//      | Available      | False                   | Unavailable           | Error                 |
	//      | Available      | True                    | Available             | Done                  |

	// 3. Create a flow to connect the above condition types.
	f := NewSummarizer(
		// Define paths.
		[][]ConditionType{
			{
				ExampleResourceStatusProgressing,
				ExampleResourceStatusReplicaFailure,
				ExampleResourceStatusAvailable,
			},
		},
		// Arrange the default step decision logic.
		func(d Decision[ConditionType]) {
			d.Make(ExampleResourceStatusProgressing,
				func(st meta.ConditionStatus, reason string) (string, string, StatusScore) {
					if st == meta.ConditionTrue && reason != "ReplicaSetUpdated" {
						return "Progressed", "", StatusDone
					}

					if st == meta.ConditionUnknown && reason == "DeploymentPaused" {
						return "Pausing", "", StatusTransitioning
					}

					return "Progressing", "", StatusTransitioning
				})

			d.Make(ExampleResourceStatusReplicaFailure,
				func(st meta.ConditionStatus, reason string) (string, string, StatusScore) {
					switch st {
					case meta.ConditionFalse:
						return "ReplicaDeployed", "", StatusDone
					case meta.ConditionTrue:
						return "ReplicaDeployed", "", StatusInterrupted
					}

					return "ReplicaDeploying", "", StatusTransitioning
				})
		},
	)

	var p printer

	// 4. Create an instance of ExampleResource.
	var r ExampleResource
	// 4.1  at beginning, the status is empty(we haven't configured any conditions or summary result),
	//      the path will walk to the end step and display the info of the last step,
	//      so we should get a done available summary,
	//      which can treat as Default Status.
	f.Summarize(&r.Status)
	p.Dump("Default Available [D]", r.Status)
	// 4.2  marked the "Progressing" status to Unknown, which means progressing,
	//      we should get a transitioning progressing summary.
	ExampleResourceStatusProgressing.Unknown(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Progressing [T]", r.Status)
	// 4.3  marked the "Progressing" status to True with ReplicaSetUpdated reason,
	//      we should still get a transitioning progressing summary.
	r.Status.Conditions[0].Status = meta.ConditionTrue
	r.Status.Conditions[0].Reason = "ReplicaSetUpdated"
	f.Summarize(&r.Status)
	p.Dump("Still Progressing [T]", r.Status)
	// 4.4  marked the "Progressing" reason to NewReplicaSetAvailable,
	//      we should get a done progressing summary.
	//      at the same time, we haven't configured other conditions,
	//      so we only can see the progressing result.
	r.Status.Conditions[0].Reason = "NewReplicaSetAvailable"
	f.Summarize(&r.Status)
	p.Dump("Progressed [D]", r.Status)
	// 4.5  marked the "ReplicaFailure" status to Unknown, which means replica deploying,
	//      we should get a transitioning replica deploying summary.
	ExampleResourceStatusReplicaFailure.Unknown(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Replica Deploying [T]", r.Status)
	// 4.6  marked the "ReplicaFailure" status to True, which means replica deploying failed,
	//      we should get a failed replica deploy summary.
	ExampleResourceStatusReplicaFailure.True(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Replica Deploy Failed [E]", r.Status)
	// 4.7  marked the "Available" status to Unknown,
	//      we still get a failed replica deploy summary,
	//      as the path cannot move the next step as the "ReplicaFailure" step is not False.
	ExampleResourceStatusAvailable.Unknown(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Still Replica Deploy Failed [E]", r.Status)
	// 4.8  until marked the "ReplicaFailure" status to False or remove "ReplicaFailure" condition,
	//      we will get a transitioning preparing summary.
	ExampleResourceStatusReplicaFailure.False(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Preparing [T]", r.Status)
	// 4.9  marked the "Available" status to False, which means replica deploying failed,
	//      we should get an error unavailable summary.
	ExampleResourceStatusAvailable.False(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Unavailable [E]", r.Status)
	// 4.10 marked the "Progressing" status to Unknown, which means progressing again,
	//      we should get a transitioning progressing summary.
	ExampleResourceStatusProgressing.Unknown(&r, "", "")
	f.Summarize(&r.Status)
	p.Dump("Progressing Again [T]", r.Status)

	t.Log(p.String())
}

func TestWalker_multiple(t *testing.T) {
	const (
		ExampleResourceStatusDeployed ConditionType = "Deployed"
		ExampleResourceStatusReady    ConditionType = "Ready"
		ExampleResourceStatusDeleted  ConditionType = "Deleted"
	)

	f := NewSummarizer(
		[][]ConditionType{
			{
				ExampleResourceStatusDeployed,
				ExampleResourceStatusReady,
			},
			{
				ExampleResourceStatusDeleted,
			},
		},
		func(d Decision[ConditionType]) {
			d.Make(ExampleResourceStatusDeleted,
				func(st meta.ConditionStatus, reason string) (string, string, StatusScore) {
					switch st {
					case meta.ConditionUnknown:
						return "Deleting", "", StatusTransitioning
					case meta.ConditionFalse:
						return "DeleteFailed", "", StatusInterrupted
					}

					return "Deleted", "", StatusDone
				})
		},
	)

	type (
		input struct {
			Status _Status
			Before func(*input)
		}
	)

	testCases := []struct {
		name     string
		given    input
		expected string
	}{
		{
			name: "no conditions",
			given: input{
				Status: _Status{
					Conditions: nil,
				},
			},
			expected: "Ready",
		},
		{
			name: "first deploy",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionUnknown,
						},
					},
				},
			},
			expected: "Deploying",
		},
		{
			name: "deployed",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionUnknown,
						},
					},
				},
			},
			expected: "Preparing",
		},
		{
			name: "redeploy",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionUnknown,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionTrue,
						},
					},
				},
			},
			expected: "Deploying",
		},
		{
			name: "redeploy but failed",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionFalse,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionTrue,
						},
					},
				},
			},
			expected: "DeployFailed",
		},
		{
			name: "delete",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusDeleted),
							Status: meta.ConditionUnknown,
						},
					},
				},
			},
			expected: "Deleting",
		},
		{
			name: "delete but failed",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusDeleted),
							Status: meta.ConditionFalse,
						},
					},
				},
			},
			expected: "DeleteFailed",
		},
		{
			name: "delete failed but redeploy",
			given: input{
				Status: _Status{
					Conditions: []meta.Condition{
						{
							Type:   string(ExampleResourceStatusDeployed),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusReady),
							Status: meta.ConditionTrue,
						},
						{
							Type:   string(ExampleResourceStatusDeleted),
							Status: meta.ConditionFalse,
						},
					},
				},
				Before: func(i *input) {
					// Remove deleted status and mark deployed status.
					ExampleResourceStatusDeployed.Reset(i, "", "")
				},
			},
			expected: "Deploying",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.given.Before != nil {
				tc.given.Before(&tc.given)
			}
			actual := tc.given.Status
			f.Summarize(&actual)
			assert.Equal(t, tc.expected, actual.Phase, "case %q", tc.name)
		})
	}
}

type printer struct {
	sb strings.Builder
}

func (p *printer) Dump(title string, result _Status) {
	p.sb.WriteString(title)
	p.sb.WriteString(": ")
	spew.Fdump(&p.sb, result.Phase)
	p.sb.WriteString("\n")
}

func (p *printer) String() string {
	return p.sb.String()
}
