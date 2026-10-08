package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// Routing membership across a command-mode change, in both directions.
//
// One selector cannot express the union a transition needs. A managed replica answers only when
// its engine is qualified, while a take-over replica answers whenever it is Ready -- the rule it
// has always had. Selecting the managed replicas while the take-over ones are still serving would
// drop them from the pool the moment the first replacement was admitted, and selecting every Pod
// would hand back managed replicas whose engine was never qualified. So a private routing label
// carries membership, the Service selector names it while the two modes coexist, and each replica
// keeps the rule its own deployed mode has always had.

// settleRouting brings a fixture to a state where every replica is Ready and, for a managed role,
// qualified -- the state a running deployment is in and the one a transition starts from.
func settleRouting(t *testing.T, f *lifecycleFixture, replicas int) {
	t.Helper()

	f.pass(false)
	f.admit()
	for ordinal := range replicas {
		f.setReady(ordinal, true)
		f.setEligible(ordinal, true)
	}
	f.pass(false)
}

// TestRouting_ManagedToTakeoverKeepsTheOldManagedReplicas covers the direction where the role
// starts managed and becomes take-over. The old managed replicas are not withdrawn when the first
// replacement appears: they keep serving under their existing qualification until their own turn
// comes, and one that is not qualified is not selected on the strength of being old.
func TestRouting_ManagedToTakeoverKeepsTheOldManagedReplicas(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)
	settleRouting(t, f, 2)

	assert.False(t, f.routesByMembership(),
		"a role whose replicas are all managed does not route on the membership label")
	assert.Equal(t, map[int]string{0: modelDeploymentAPIAnsweringValue, 1: modelDeploymentAPIAnsweringValue},
		f.answering(), "and both qualified managed replicas answer")

	// The role takes over its command line. The highest ordinal turns over first.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	require.NoError(t, f.cli.Update(context.Background(), edited))

	f.pass(false)
	f.admit()
	f.pass(false)
	slot := f.slotMust("server")
	f.setReady(slot.Ordinal, true)
	f.pass(false)

	assert.True(t, f.routesByMembership(),
		"the two modes cannot be selected by one rule, so the selector names the routing label")
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[0],
		"the old managed replica keeps answering while it is still serving")
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[slot.Ordinal],
		"and the take-over replica built beside it answers under the rule it always had")

	// The negative control: an old managed replica whose engine is not answering does not keep an
	// endpoint merely for being the one that is still old. The qualification is re-derived from the
	// engine every pass rather than read back off the Pod, so the control is taken on the member's
	// readiness, which every eligibility rule already treats as a definite fault.
	f.setReady(0, false)
	f.pass(false)
	assert.Empty(t, f.answering()[0],
		"an old managed replica that is not ready is not selected while the transition is on")
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[slot.Ordinal],
		"and the take-over replica beside it is untouched by that decision")

	// Once the transition is over, the label leaves the selector again: a homogeneous role keeps
	// its ordinary routing.
	for range 8 {
		if allAtImage(f, "vllm/vllm-openai:v0.25.1", 2) && !f.routesByMembership() {
			break
		}
		f.pass(false)
		f.admit()
	}
	assert.False(t, f.routesByMembership(),
		"and ordinary homogeneous routing is unchanged once no replica of the old mode is left")
}

// TestRouting_TakeoverToManagedKeepsTheOldTakeoverReplicas covers the other direction. A take-over
// replica answers whenever it is Ready, so the old ones stay selectable for as long as they run;
// the new managed ones answer only once their engine is qualified.
func TestRouting_TakeoverToManagedKeepsTheOldTakeoverReplicas(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	})
	f := newLifecycleFixture(t, md)
	settleRouting(t, f, 2)

	assert.False(t, f.routesByMembership(),
		"a role whose replicas are all take-over does not route on the membership label either")
	assert.Equal(t, map[int]string{0: modelDeploymentAPIAnsweringValue, 1: modelDeploymentAPIAnsweringValue},
		f.answering(), "and both take-over replicas answer")

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Command = nil
	require.NoError(t, f.cli.Update(context.Background(), edited))

	f.pass(false)
	f.admit()
	f.pass(false)
	slot := f.slotMust("server")

	// The negative control, taken while the old take-over replica is still serving: the managed
	// replacement is not answering yet, and neither managed rule nor take-over rule may select it.
	f.setReady(slot.Ordinal, false)
	f.pass(false)

	assert.True(t, f.routesByMembership(), "the two modes coexist, so the selector names the label")
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[0],
		"the old take-over replica is retained rather than withdrawn when a replacement appears")
	assert.Empty(t, f.answering()[slot.Ordinal],
		"while a managed replica that is not yet qualified answers nothing")

	// Once it answers it is selected, and the old take-over replica keeps answering beside it.
	f.setReady(slot.Ordinal, true)
	f.setEligible(slot.Ordinal, true)
	f.pass(false)
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[slot.Ordinal],
		"the managed replacement answers once its engine is qualified")
	assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[0],
		"and the old take-over replica is still selectable until its own turn")
}

// TestRouting_TheReplicaBeingReplacedIsNotSelected covers the exclusion the transition needs: a
// replica on its way out must stop receiving traffic before it is deleted, whichever mode it is in.
func TestRouting_TheReplicaBeingReplacedIsNotSelected(t *testing.T) {
	for _, command := range [][]string{nil, {"vllm", "serve", "qwen", "--port", "8000"}} {
		name := "managed"
		if command != nil {
			name = "takeover"
		}
		t.Run(name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 2
				md.Spec.Roles[0].Command = command
			})
			f := newLifecycleFixture(t, md)
			settleRouting(t, f, 2)

			edited := getModelDeployment(t, f.cli)
			edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
			require.NoError(t, f.cli.Update(context.Background(), edited))
			f.pass(false)

			slot, held := f.slot("server")
			require.True(t, held)
			assert.Empty(t, f.answering()[slot.Ordinal],
				"the replica this pass selected stops answering before it is deleted")
			assert.Equal(t, modelDeploymentAPIAnsweringValue, f.answering()[1-slot.Ordinal],
				"and the replica beside it is unaffected")
		})
	}
}

// TestRouting_AnExternalDataParallelRoleAnswersOnItsRanksOnly covers the shape the transition has
// to preserve on a role whose members are ranks of one engine: the ranks answer under the existing
// rules, and a withdrawn leader answers nothing.
func TestRouting_AnExternalDataParallelRoleAnswersOnItsRanksOnly(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].ExtraArgs = []string{
			"--data-parallel-size", "2",
			"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)",
		}
	})
	f := newLifecycleFixture(t, md)
	settleRouting(t, f, 2)

	// AN EXTERNAL DATA-PARALLEL REPLICA ANSWERS ON EVERY RANK. The shape is read off the replicas the
	// cluster is running, so both members of a two-rank group answer -- which is what the rank
	// arrangement means, and what reading the rank from the spec instead would quietly withdraw.
	for ordinal := range 2 {
		members := f.live("server", ordinal)
		require.Len(t, members, 2)
		for _, member := range members {
			assert.Equal(t, modelDeploymentAPIAnsweringValue,
				member.Labels[modelDeploymentLabelKeyAPIAnswering],
				"ordinal %d member %s answers: every rank of an External-DP group serves",
				ordinal, member.Labels[modelDeploymentMemberIndexLabel])
		}
	}

	// The transition itself: a replica is replaced while its sibling keeps answering on both ranks.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)

	slot := f.slotMust("server")
	assert.Empty(t, f.answering()[slot.Ordinal],
		"the replica under replacement stops answering, ranks included")
	for _, member := range f.live("server", 1-slot.Ordinal) {
		assert.Equal(t, modelDeploymentAPIAnsweringValue,
			member.Labels[modelDeploymentLabelKeyAPIAnswering],
			"while the untouched replica keeps every rank answering")
	}
}

// TestRouting_ALeaderServedReplicaAnswersOnItsDeployedSize covers the same rule for the other
// direction of a size edit. A replica still running four members answers on its leader, because
// that is the shape it was built at -- and the follower is withdrawn the moment the spec asks for
// one member, which is capacity this operator never chose to give up.
func TestRouting_ALeaderServedReplicaAnswersOnItsDeployedSize(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	})
	f := newLifecycleFixture(t, md)
	settleRouting(t, f, 2)

	// A take-over replica answers under the legacy rule, so the shape is the only thing deciding
	// which of its members are selected.
	leaders := 0
	for _, member := range f.live("server", 0) {
		if member.Labels[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue {
			leaders++
		}
	}
	assert.Equal(t, 1, leaders, "a leader-served replica of two members answers on its leader")

	// The size drops to one. The running replica is not the one being replaced, so it keeps
	// answering on its own shape until its own turn comes.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].ReplicaSize = 1
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)

	slot, held := f.slot("server")
	require.True(t, held)
	sibling := 1 - slot.Ordinal
	stillTwo := 0
	for _, member := range f.live("server", sibling) {
		if member.Labels[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue {
			stillTwo++
		}
	}
	assert.Equal(t, 1, stillTwo,
		"the replica the spec no longer sizes is still leader-served, so its follower stays unselected")
}

// TestRouting_TheLabelWriteIsBoundToTheObservedPod covers the window between reading a member and
// writing its membership. A name is not an identity: a take-over replica that is deleted and
// recreated as a managed one under the same name must not inherit the membership the controller read
// off the replica that is gone.
func TestRouting_TheLabelWriteIsBoundToTheObservedPod(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	})
	f := newLifecycleFixture(t, md)
	f.pass(false)
	f.admit()
	f.setReady(0, true)
	f.pass(false)
	require.Equal(t, map[int]string{0: modelDeploymentAPIAnsweringValue}, f.answering(),
		"the take-over replica answers under the legacy rule")

	// The member goes unready, so this pass has a write to make: it withdraws the membership it
	// carries. The replacement arrives in the window between the observation and that write.
	takeover := f.live("server", 0)
	require.Len(t, takeover, 1)
	f.setReady(0, false)

	managed := takeover[0].DeepCopy()
	managed.UID = types.UID("pod-the-managed-one")
	delete(managed.Annotations, "prometheus.io/port")
	delete(managed.Labels, modelDeploymentLabelKeyAPIAnswering)
	managed.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}

	swapped := new(bool)
	r := &ModelDeploymentReconciler{
		Client:    f.interceptReplace(t, takeover[0], managed, swapped),
		APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
	}
	_, err := reconcileModelDeploymentWith(t, r)
	require.True(t, *swapped, "the member was replaced in the window under test")
	if err != nil {
		assert.True(t, kerrors.IsConflict(err),
			"the write is refused as a conflict rather than landing on a stranger: %v", err)
	}

	survivor := new(core.Pod)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKeyFromObject(managed), survivor))
	assert.NotContains(t, survivor.Labels, modelDeploymentLabelKeyAPIAnswering,
		"and the replacement carries no membership read off the replica it replaced")
}

// interceptReplace returns a client that swaps one Pod for another the first time that Pod is
// patched, which is the window a stale cached object is written through.
func (f *lifecycleFixture) interceptReplace(
	t *testing.T, old, replacement *core.Pod, swapped *bool,
) ctrlcli.Client {
	t.Helper()

	return ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
		Patch: func(
			ctx context.Context, next ctrlcli.WithWatch, obj ctrlcli.Object,
			patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
		) error {
			pod, ok := obj.(*core.Pod)
			if !ok || *swapped || pod.Name != old.Name {
				return next.Patch(ctx, obj, patch, opts...)
			}

			*swapped = true
			// The member holds Kueue's finalizer, so the delete is accepted and the object stays
			// until it is released -- which is the one thing this window is not about.
			held := new(core.Pod)
			require.NoError(t, f.cli.Get(ctx, ctrlcli.ObjectKeyFromObject(old), held))
			held.Finalizers = nil
			require.NoError(t, f.cli.Update(ctx, held))
			require.NoError(t, f.cli.Delete(ctx, held))
			arriving := replacement.DeepCopy()
			arriving.Finalizers = nil
			arriving.ResourceVersion = ""
			require.NoError(t, f.cli.Create(ctx, arriving))

			return next.Patch(ctx, obj, patch, opts...)
		},
	})
}

// A Ready managed replica with revoked qualification stays unselected during a command change.
// Drive the membership writer and compare complete Service selectors.
// Qualified leaders remain selected; qualification is injected at the writer's input.
func TestRouting_ARevokedManagedReplicaKeepsTheGateWhileTheRoleTurnsOver(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})
	f := newLifecycleFixture(t, md)
	f.pass(false)
	f.admit()
	require.Len(t, f.live("server", 0), 2, "setup requires a replica of two members")
	f.setReady(0, true)
	managed := f.live("server", 0)
	require.Len(t, managed, 2)

	// The role is turning over, and its replicas have not. One replica is still the managed pair the
	// operator built, and that pair keeps the gate.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	require.NoError(t, f.cli.Update(context.Background(), edited))
	role := &getModelDeployment(t, f.cli).Spec.Roles[0]
	require.NotEmpty(t, role.Command, "setup requires the role to have gained a command line")

	require.True(t, modelDeploymentReplicaRunsCommand(managed),
		"setup requires the replica still to be one this operator built")

	// The gate is asked of the mode the replica runs. Asking it of the role instead is the defect.
	assert.False(t, modelDeploymentEligibilitySelectorActive(md, role, nil, true),
		"the role's own command line would take the gate away")
	assert.True(t, modelDeploymentEligibilitySelectorActive(md,
		modelDeploymentEffectiveRoleForEligibility(role, managed), nil, true),
		"while a replica still running the operator's command line keeps it")

	// And the writer publishes that gate: the role Service keeps the term it was narrowed by while
	// a managed replica of the role is still standing.
	// The published Services are read against the deployment as the cluster now holds it, with the
	// eligibility condition decided, which is what a narrowed selector means.
	published := getModelDeployment(t, f.cli)
	ModelDeploymentConditionEndpointEligibility.True(published,
		modelDeploymentReasonEndpointsQualified, "every group qualified")
	require.NoError(t, f.cli.Status().Update(context.Background(), published))

	r := &ModelDeploymentReconciler{Client: f.cli, APIReader: f.cli}
	cases := []struct {
		name    string
		state   modelDeploymentGroupForwardState
		verdict modelDeploymentLegVerdict
		want    bool
	}{
		{name: "revoked", state: modelDeploymentGroupForwardFailed, verdict: modelDeploymentLegFailed},
		{name: "qualified", state: modelDeploymentGroupForwardVerified, verdict: modelDeploymentLegVerified, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byMember := make(map[types.UID]modelDeploymentInstanceQualification, len(managed))
			for _, member := range managed {
				byMember[member.UID] = modelDeploymentInstanceQualification{
					GroupForward: modelDeploymentGroupForward{State: tc.state},
					Legs: []modelDeploymentQualificationLeg{{
						Name: modelDeploymentLegGroupForward, Verdict: tc.verdict,
					}},
				}
			}
			require.NoError(t, r.convergeModelDeploymentEndpointEligibility(context.Background(), published,
				replicaPods(t, f.cli), byMember, nil))
			actual := replicaPods(t, f.cli)
			require.NoError(t, r.syncModelDeploymentService(context.Background(), published, true, actual))
			for _, name := range []string{md.Name, md.Name + "-server"} {
				svc := new(core.Service)
				require.NoError(t, f.cli.Get(context.Background(),
					ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name}, svc))
				for _, member := range f.live("server", 0) {
					assert.Equal(t, tc.want && modelDeploymentAnsweringMemberLeader(member),
						labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(member.Labels)),
						"%s selects only qualified answering members", name)
				}
			}
		})
	}

	// A Ready, revoked, managed member answers nothing under that gate, whichever mode it runs.
	view := modelDeploymentReplicaView{Members: managed}
	for _, member := range managed {
		require.True(t, podIsReady(member),
			"setup requires the member to stay Ready: readiness is not what withdrew it")
		assert.False(t, modelDeploymentPodAnswersAPI(member, view, md.Spec.Engine.Name,
			false, true, nil),
			"a managed member whose qualification was revoked answers nothing while the gate is active")
		if modelDeploymentAnsweringMemberLeader(member) {
			assert.True(t, modelDeploymentPodAnswersAPI(member, view, md.Spec.Engine.Name,
				false, false, nil),
				"and the legacy rule is kept only for a role whose gate was never activated")
		}
	}
}

// TestRouting_APeerServiceIsNeverNarrowedByTheTransition covers the Services a transition must
// leave alone. A member's peers are addressed by DNS through the headless Services, and narrowing
// those would break the collective that is still running.
func TestRouting_APeerServiceIsNeverNarrowedByTheTransition(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})
	f := newLifecycleFixture(t, md)
	settleRouting(t, f, 2)

	headless := new(core.ServiceList)
	require.NoError(t, f.cli.List(context.Background(), headless, ctrlcli.InNamespace("team-a")))
	before := make(map[string]map[string]string, len(headless.Items))
	for i := range headless.Items {
		before[headless.Items[i].Name] = headless.Items[i].Spec.Selector
	}
	require.NotEmpty(t, before)

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
	require.NoError(t, f.cli.Update(context.Background(), edited))
	for range 3 {
		f.pass(false)
		f.admit()
		f.pass(false)
		if f.routesByMembership() {
			break
		}
	}
	require.True(t, f.routesByMembership(), "the two modes coexist at the point under test")

	// The front Services are the ones the transition re-points, and the peer Services are not. A
	// member's peers are addressed by DNS through the headless Services, and narrowing those would
	// break the collective that is still running.
	front := map[string]bool{f.md.Name: true}
	for i := range f.md.Spec.Roles {
		front[f.md.Name+"-"+f.md.Spec.Roles[i].Name] = true
	}

	after := new(core.ServiceList)
	require.NoError(t, f.cli.List(context.Background(), after, ctrlcli.InNamespace("team-a")))
	peers := 0
	for i := range after.Items {
		require.Contains(t, before, after.Items[i].Name, "a Service appeared that no pass rendered")
		if front[after.Items[i].Name] {
			assert.Contains(t, after.Items[i].Spec.Selector, modelDeploymentLabelKeyAPIAnswering,
				"%s names the routing label while the two modes coexist", after.Items[i].Name)

			continue
		}
		peers++
		assert.Equal(t, before[after.Items[i].Name], after.Items[i].Spec.Selector,
			"%s keeps the selector the collective resolves its peers through", after.Items[i].Name)
	}
	assert.Positive(t, peers, "the deployment has peer Services to leave alone")
}
