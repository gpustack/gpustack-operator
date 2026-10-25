package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// sedpExtraArgs is the SGLang External-DP launch shape: one distributed launch per instance
// spanning two member Pods, one attention-DP group per member Pod, and the FFN tensor-parallel
// across both. The member-varying values are the ones the operator stamps on every member.
func sedpExtraArgs() []string {
	return []string{
		"--tp-size", "2",
		"--attn-dp-size", "2",
		"--nnodes", "2",
		"--node-rank", "$(GPUSTACK_MEMBER_INDEX)",
		"--dist-init-addr", "$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500",
	}
}

// sedpEnv is the variable that puts the engine in Rust-server mode, where every attention-DP
// group hosts a listener of its own.
func sedpEnv() []workercore.ModelDeploymentEnvVar {
	return []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_RUST_SERVER", Value: "1"}}
}

// plainTPMultiPodArgs is the leader-served multi-Pod SGLang launch: one distributed launch
// across the members with the tensor-parallel width inside it and no attention-DP group of its
// own. The members join a collective and the group answers through its leader alone.
func plainTPMultiPodArgs() []string {
	return []string{
		"--tp-size", "2",
		"--nnodes", "2",
		"--node-rank", "$(GPUSTACK_MEMBER_INDEX)",
		"--dist-init-addr", "$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500",
	}
}

// TestSGLangExternalDPBalanceShape pins the derivation of the load-balance shape for SGLang:
// the attention-DP launch across members under the Rust server is balanced from outside, and
// every other SGLang declaration stays internal.
func TestSGLangExternalDPBalanceShape(t *testing.T) {
	t.Parallel()

	sglang := workercore.ModelDeploymentEngineSGLang
	vllm := workercore.ModelDeploymentEngineVLLM

	testCases := []struct {
		name string
		// engine is the engine the role declares, so the vote can be pinned as the SGLang
		// engine's alone.
		engine string
		// kind is the role's kind; empty means the server kind the default resolves to.
		kind      workercore.ModelDeploymentRoleKind
		extraArgs []string
		env       []workercore.ModelDeploymentEnvVar
		want      workercore.ModelDeploymentLoadBalance
	}{
		{
			name:      "the attention-DP launch across members under the Rust server is externally balanced",
			engine:    sglang,
			extraArgs: sedpExtraArgs(),
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceExternal,
		},
		{
			name:      "a prefill role carrying the launch stays unclassified",
			engine:    sglang,
			kind:      workercore.ModelDeploymentRoleKindPrefill,
			extraArgs: sedpExtraArgs(),
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "a decode role carrying the launch stays unclassified",
			engine:    sglang,
			kind:      workercore.ModelDeploymentRoleKindDecode,
			extraArgs: sedpExtraArgs(),
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "a plain tensor-parallel launch across members stays internal",
			engine:    sglang,
			extraArgs: plainTPMultiPodArgs(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "an attention width of one is one group and stays internal",
			engine:    sglang,
			extraArgs: []string{"--attn-dp-size", "1", "--nnodes", "2", "--node-rank", "$(GPUSTACK_MEMBER_INDEX)"},
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "a launch of one node has no member to balance across",
			engine:    sglang,
			extraArgs: []string{"--attn-dp-size", "2", "--nnodes", "1"},
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "without the Rust server the attention groups share one frontend",
			engine:    sglang,
			extraArgs: sedpExtraArgs(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "the Rust server switched off is not the Rust server",
			engine:    sglang,
			extraArgs: sedpExtraArgs(),
			env:       []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_RUST_SERVER", Value: "0"}},
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "the Rust server spelled true is the Rust server",
			engine:    sglang,
			extraArgs: sedpExtraArgs(),
			env:       []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_RUST_SERVER", Value: "true"}},
			want:      workercore.ModelDeploymentLoadBalanceExternal,
		},
		{
			name:      "the Rust server spelled yes is not the Rust server",
			engine:    sglang,
			extraArgs: sedpExtraArgs(),
			env:       []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_RUST_SERVER", Value: "yes"}},
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:      "the vote is the SGLang engine's alone",
			engine:    vllm,
			extraArgs: sedpExtraArgs(),
			env:       sedpEnv(),
			want:      workercore.ModelDeploymentLoadBalanceInternal,
		},
		{
			name:   "the deprecated attention spelling is not the external shape",
			engine: sglang,
			extraArgs: []string{
				"--dp-size", "2",
				"--enable-dp-attention",
				"--nnodes", "2",
				"--node-rank", "$(GPUSTACK_MEMBER_INDEX)",
			},
			env:  sedpEnv(),
			want: workercore.ModelDeploymentLoadBalanceInternal,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			role := workercore.ModelDeploymentRole{
				Kind: tc.kind, ExtraArgs: tc.extraArgs, Env: tc.env,
			}
			got := ReadModelDeploymentRoleParallelism(tc.engine, &role)
			assert.Equal(t, tc.want, got.LoadBalance)
		})
	}
}

// TestSGLangAttentionDataParallelDegree pins the attention width as a degree of the SGLang
// engine: every registered spelling is read, a malformed value is refused where the engine would
// refuse it, and the width carries no status degree because the transfer document has no field
// for it.
func TestSGLangAttentionDataParallelDegree(t *testing.T) {
	t.Parallel()

	sglang := workercore.ModelDeploymentEngineSGLang

	testCases := []struct {
		name string
		// extraArgs is the role's own argument stream.
		extraArgs []string
		// want is the declared attention width.
		want int
		// wantErr is a substring of the returned error; empty expects none.
		wantErr string
	}{
		{
			name:      "the engine's own spelling is read",
			extraArgs: []string{"--attn-dp-size", "2"},
			want:      2,
		},
		{
			name:      "the alias spelling is read",
			extraArgs: []string{"--attention-data-parallel-size", "3"},
			want:      3,
		},
		{
			name:      "the attached value form is read",
			extraArgs: []string{"--attn-dp-size=4"},
			want:      4,
		},
		{
			name:      "an undeclared width is the engine's own default of one",
			extraArgs: nil,
			want:      1,
		},
		{
			name:      "a width below one is refused",
			extraArgs: []string{"--attn-dp-size", "0"},
			wantErr:   "the --attn-dp-size value 0 is below 1",
		},
		{
			name:      "a width that is not an integer is refused",
			extraArgs: []string{"--attn-dp-size", "two"},
			wantErr:   `the --attn-dp-size value "two" is not an integer`,
		},
		{
			name:      "a width with no value is refused",
			extraArgs: []string{"--attn-dp-size"},
			wantErr:   "the --attn-dp-size declaration has no value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseModelDeploymentDeclaredParallelism(sglang, tc.extraArgs, nil)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.AttentionDataParallel)
		})
	}
}

// TestSGLangAttentionDataParallelCarriesNoStatusDegree pins the width's place in the transfer
// document: the status reports the degrees it has a field for, and the attention width is not
// one of them, so declaring it changes no status degree.
func TestSGLangAttentionDataParallelCarriesNoStatusDegree(t *testing.T) {
	t.Parallel()

	role := workercore.ModelDeploymentRole{ExtraArgs: []string{"--attn-dp-size", "2"}}
	got := ReadModelDeploymentRoleParallelism(workercore.ModelDeploymentEngineSGLang, &role)

	assert.Equal(t, workercore.ModelDeploymentParallelismDeclaredStatus{}, got.Declared,
		"the attention width has no status degree")
}

// TestSGLangExternalDPRoutingSelectsEveryMember pins the published surface of an SGLang
// External-DP role: every member is selectable, so no selector names a member index. A role that
// is not externally balanced keeps its leader-only selection.
func TestSGLangExternalDPRoutingSelectsEveryMember(t *testing.T) {
	t.Parallel()

	for _, routerName := range []string{
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentRouterVLLM,
		workercore.ModelDeploymentRouterSGLang,
	} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%t", routerName, external), func(t *testing.T) {
				t.Parallel()
				md := newRenderDeployment()
				md.Spec.KVCache = nil
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: routerName}
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Image = "gpustack/runner:v0.1.31"
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].ExtraArgs = sedpExtraArgs()
				md.Spec.Roles[0].Env = sedpEnv()
				if !external {
					md.Spec.Roles[0].ExtraArgs = plainTPMultiPodArgs()
					md.Spec.Roles[0].Env = nil
				}
				ModelDeploymentConditionEndpointEligibility.True(md, modelDeploymentReasonEndpointsQualified, "all groups qualified")

				objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
				require.NoError(t, err)
				role := &md.Spec.Roles[0]

				discovery := routingDiscoverySelector(t, objects.Deployment.Spec.Template.Spec.Containers[0].Args, objects.ConfigMap)
				published := labels.SelectorFromSet(objects.Contract.Roles[0].Selector)
				service := labels.SelectorFromSet(renderModelDeploymentRoleService(md, role, nil, nil, false).Spec.Selector)

				for name, selector := range map[string]labels.Selector{
					"discovery": discovery,
					"published": published,
					"service":   service,
				} {
					_, namesMember := selector.RequiresExactMatch(modelDeploymentMemberIndexLabel)
					if external {
						assert.False(t, namesMember, "%s must not name a member index", name)
					} else {
						assert.True(t, namesMember, "%s must name the leader's member index", name)
					}
				}
			})
		}
	}
}

// sedpMember is a member running the SGLang launch, with the Rust-server variable on its own
// container: the drain reads the member's own command and its own environment, never the
// role's description.
func sedpMember(ordinal, member int, uid string, command []string, rustServer bool) core.Pod {
	pod := shapeMember(ordinal, member, uid, command...)
	if rustServer {
		pod.Spec.Containers[0].Env = []core.EnvVar{{Name: "SGLANG_RUST_SERVER", Value: "1"}}
	}
	return pod
}

// sedpCommand is the rendered command of an SGLang member of the attention-DP launch.
func sedpCommand() []string {
	return []string{
		"python", "-m", "sglang.launch_server",
		"--tp-size", "2", "--attn-dp-size", "2", "--nnodes", "2",
		"--node-rank", "0", "--dist-init-addr", "leader:29500",
	}
}

// plainTPCommand is the rendered command of an SGLang member of a leader-served launch: the
// tensor-parallel width inside the collective, and no attention-DP group of its own.
func plainTPCommand() []string {
	return []string{
		"python", "-m", "sglang.launch_server",
		"--tp-size", "2", "--nnodes", "2",
		"--node-rank", "0", "--dist-init-addr", "leader:29500",
	}
}

// TestSEDPDrainReadsTheMembersTheShapeMakesAnswerable pins the drain unit against the External
// classification: a leader-served multi-Pod SGLang replica drains through its leader alone, and
// an attention-DP replica under the Rust server is read as the disaggregated shape it is.
//
// THE READ SET IS THE REPAIR, so it is asserted as the set of members read rather than as a
// state: a member read here that should not be, or one missing, is the defect.
func TestSEDPDrainReadsTheMembersTheShapeMakesAnswerable(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		pods []core.Pod
		// wantState is the reservation the plan must reach.
		wantState workercore.ModelDeploymentRetirementState
		// wantReads is the count of reads per member.
		wantReads map[string]int
		// wantReason is a substring of the reservation's reason; empty expects none.
		wantReason string
	}{
		{
			name: "a leader-served multi-Pod SGLang replica drains through its leader alone",
			md: retirementDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].ReplicaSize = 2
			}),
			pods: []core.Pod{
				sedpMember(1, 0, "m-0", plainTPCommand(), false),
				sedpMember(1, 1, "m-1", plainTPCommand(), false),
			},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			name: "an attention-DP replica under the Rust server is read as the disaggregated shape",
			md: retirementDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].ReplicaSize = 2
			}),
			pods: []core.Pod{
				sedpMember(1, 0, "m-0", sedpCommand(), true),
				sedpMember(1, 1, "m-1", sedpCommand(), true),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no verifiable decoder-side release",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingDrainReader{answer: sglangIdleDrain}
			plan := planFor(t, tc.md, tc.pods, reader)

			assert.Equal(t, tc.wantState, plan.Reservation.State,
				"reason was %q", plan.Reservation.Reason)
			if tc.wantReason != "" {
				assert.Contains(t, plan.Reservation.Reason, tc.wantReason)
			}
			assert.Equal(t, tc.wantReads, reader.readCounts(),
				"the read set is the repair; a member read here that should not be, or missing, is the defect")
		})
	}
}
