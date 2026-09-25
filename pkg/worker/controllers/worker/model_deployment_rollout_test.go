package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// rolloutConditionOf drives one pass's rollout record through the whole status compute, so a case
// asserts what an operator reads off the object rather than what the observer returned. A unit test
// over the observer alone would pass with the call left out of the compute.
func rolloutConditionOf(
	t *testing.T, md *workercore.ModelDeployment, rollout *modelDeploymentRollout,
) *workercore.ModelDeployment {
	t.Helper()

	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil, rollout, nil)
	require.NoError(t, err)

	return &workercore.ModelDeployment{Status: *status}
}

// TestModelDeploymentRollout_ReportsWhatThePassDecided covers the three answers a pass that compared
// hashes can give. The held case is the one this condition exists for: the other two are what make
// it legible, because a condition that is False for every rollout says nothing about being stuck.
func TestModelDeploymentRollout_ReportsWhatThePassDecided(t *testing.T) {
	cases := []struct {
		name        string
		rollout     modelDeploymentRollout
		wantStatus  string
		wantReason  string
		wantMessage string
	}{
		{
			name:        "every replica matches the render",
			rollout:     modelDeploymentRollout{accounted: 2},
			wantStatus:  "True",
			wantReason:  modelDeploymentReasonUpToDate,
			wantMessage: "every replica matches what this pass rendered",
		},
		{
			name:        "an ordinary rollout is under way",
			rollout:     modelDeploymentRollout{accounted: 2, outdated: 2},
			wantStatus:  "False",
			wantReason:  modelDeploymentReasonRolloutInProgress,
			wantMessage: "2 replicas differ from what this pass rendered and turn over one per role per pass",
		},
		{
			name:        "the store is away and the rollout is held",
			rollout:     modelDeploymentRollout{accounted: 2, outdated: 2, held: 2},
			wantStatus:  "False",
			wantReason:  modelDeploymentReasonRolloutHeldByCache,
			wantMessage: "2 of 2 replicas differ from what this pass rendered and were left in place",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observed := rolloutConditionOf(t, newRenderDeployment(), &tc.rollout)

			assert.Equal(t, tc.wantStatus, ModelDeploymentConditionReplicasUpToDate.GetStatus(observed))
			assert.Equal(t, tc.wantReason, ModelDeploymentConditionReplicasUpToDate.GetReason(observed))
			assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(observed), tc.wantMessage)
		})
	}
}

// TestModelDeploymentRollout_AHeldMessageSaysWhatReleasesIt pins the part of the message the issue
// this condition answers is actually about. A reader arriving here edited the spec and is watching
// for a rollout that is not coming, so "held" alone leaves them exactly where the accurate
// DomainRegistered message already left them: knowing something is wrong with a reuse domain, with
// no path from that to their own edit.
func TestModelDeploymentRollout_AHeldMessageSaysWhatReleasesIt(t *testing.T) {
	observed := rolloutConditionOf(t, newRenderDeployment(), &modelDeploymentRollout{
		accounted: 1, outdated: 1, held: 1,
	})
	message := ModelDeploymentConditionReplicasUpToDate.GetMessage(observed)

	assert.Contains(t, message, "KV cache connection", "the message must name what is holding it")
	assert.Contains(t, message, "withheld rather than dropped",
		"a reader deciding whether to re-apply the edit needs to know it was not lost")
	assert.Contains(t, message, "once the connection returns", "and what releases it")
}

// TestModelDeploymentRollout_ARecordThatVouchesForNothingReportsUnknown states the rule a teardown
// pass and a rebuild pass both depend on: neither can account for a replica, so neither has an
// answer -- and an axis with no answer is Unknown, which is itself an answer.
//
// It used to leave the stored value alone instead, and that was wrong in the state it is read in.
// Whatever the last answering pass wrote stays authoritative, so a steady deployment's True survives
// the very pass that deletes every replica. Both fixtures below start from a prior value and require
// it to be REPLACED, because "unchanged" was the bug.
func TestModelDeploymentRollout_ARecordThatVouchesForNothingReportsUnknown(t *testing.T) {
	for _, prior := range []struct {
		name    string
		arrange func(*workercore.ModelDeployment)
	}{
		{
			name: "a prior True is the harmful one",
			arrange: func(md *workercore.ModelDeployment) {
				ModelDeploymentConditionReplicasUpToDate.True(md, modelDeploymentReasonUpToDate,
					"every replica matches what this pass rendered")
			},
		},
		{
			name: "a prior False is stale just the same",
			arrange: func(md *workercore.ModelDeployment) {
				ModelDeploymentConditionReplicasUpToDate.False(md, modelDeploymentReasonRolloutHeldByCache,
					"held by a pass that ran before this one")
			},
		},
	} {
		t.Run(prior.name, func(t *testing.T) {
			md := newRenderDeployment()
			prior.arrange(md)

			for label, rollout := range map[string]*modelDeploymentRollout{
				"no record at all":             nil,
				"a record accounting for none": {},
			} {
				observed := rolloutConditionOf(t, md.DeepCopy(), rollout)
				assert.Equalf(t, "Unknown", ModelDeploymentConditionReplicasUpToDate.GetStatus(observed),
					"%s must not leave the previous answer standing", label)
				assert.Equalf(t, modelDeploymentReasonRolloutNotObserved,
					ModelDeploymentConditionReplicasUpToDate.GetReason(observed), "%s", label)
			}
		})
	}

	// The positive baseline: a record that DID account for a replica answers, so the Unknown above is
	// the observer distinguishing two inputs rather than an observer stuck on one value.
	md := newRenderDeployment()
	moved := rolloutConditionOf(t, md, &modelDeploymentRollout{accounted: 1})
	require.Equal(t, "True", ModelDeploymentConditionReplicasUpToDate.GetStatus(moved))
	require.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(moved))
}

// TestModelDeploymentReconciler_ReportsARolloutHeldByAnUnreachableStore is the whole issue, end to
// end: a spec edit made while the store is away changes nothing, and before this condition the
// object said so nowhere.
//
// The outage is the Binding ceasing to be Ready, which is what an unreachable master presents as --
// the domain this deployment already resolved stays in status, so both halves of the rollout guard
// hold and the replicas are deliberately left alone.
func TestModelDeploymentReconciler_ReportsARolloutHeldByAnUnreachableStore(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaHashes(t, cli)
	require.Len(t, before, 2)
	require.NotNil(t, getModelDeployment(t, cli).Status.KVCache,
		"the guard's second half reads a domain this deployment already resolved")

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	kvcpb.Status.PhaseMessage = "the master's figures could not be read this pass"
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, before, replicaHashes(t, cli),
		"the guard leaves the replicas alone, which is the behaviour this condition reports rather than changes")

	md := getModelDeployment(t, cli)
	assert.Equal(t, "False", ModelDeploymentConditionReplicasUpToDate.GetStatus(md))
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md))
	assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(md), "2 of 2 replicas")
}

// TestModelDeploymentReconciler_AResizeDuringAnOutageStillAnswersForWhatItDid covers the pass that
// scales from the outage state rather than from the steady one. The scale itself is never withheld
// -- the ordinals it adds do not exist yet and cannot be given an address that does not exist yet
// either -- so the pass creates them from the current render and vouches for each one it created.
// Reporting Unknown on that pass would announce "could not tell" on the one pass that could tell
// exactly what it built; reporting the survivors' held state is the answer that matches what
// happened.
func TestModelDeploymentReconciler_AResizeDuringAnOutageStillAnswersForWhatItDid(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaHashes(t, cli), 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the held state is the precondition; without it what follows proves nothing")

	// A replica count change scales the ordinals, and the scale proceeds during the outage: the
	// third is created from the current spec, connector or no connector.
	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 3,
		"the added ordinal is created in the outage pass, beside the two that are held")

	md := getModelDeployment(t, cli)
	assert.Equal(t, "False", ModelDeploymentConditionReplicasUpToDate.GetStatus(md))
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"the survivors are still held, and the pass says so rather than claiming it could not tell")
}

// TestModelDeploymentReconciler_AnOutageDoesNotCarryAWithheldEditOntoTheRunningReplicas pins the
// bound the rollout guard now has.
//
// The guard withholds an edit that changes a replica's rendered Pod for as long as the KV cache
// connection cannot be resolved, and an outage has no deadline. There used to be a lever out -- a
// group-shape change rebuilt the whole group past the guard -- and the per-replica groups removed
// it: a scale now adds ordinals beside the held replicas rather than replacing them, so what the
// scale creates carries the current spec (the new ordinal never served anything) while the RUNNING
// replicas keep the spec they were built from until the store returns. The recovery that remains is
// the store's own, and nothing here can speed it.
//
// THE CREATED ORDINAL PAYS A SECOND TURNOVER, and that is the cost to know about: it is built
// without a connector during the outage, so once the store returns the render gains one and it
// rolls again with the survivors. Before the split, the operator could buy exactly this trade
// deliberately; now a scale during an outage spends it, which is why the held message still names
// what waits rather than promising an escape.
func TestModelDeploymentReconciler_AnOutageDoesNotCarryAWithheldEditOntoTheRunningReplicas(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaImages(t, cli), 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	edited := getModelDeployment(t, cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), edited))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the edit has to be withheld first, or there is nothing whose carrying this case can pin")
	for name, image := range replicaImages(t, cli) {
		require.Equal(t, "vllm/vllm-openai:v0.25.1", image,
			"%s still carries the spec it was built from", name)
	}

	// The scale the lever used to be: it proceeds, but it no longer replaces anyone.
	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 3, "the third ordinal is created beside the held two")

	images := replicaImages(t, cli)
	require.Len(t, images, 3)
	held := 0
	for _, image := range images {
		if image != "vllm/vllm-openai:v0.26.0" {
			held++
		}
	}
	assert.Equal(t, 2, held,
		"the two RUNNING replicas keep the spec they were built from: only the ordinal the scale "+
			"created carries the edit, and no lever lands it on anyone else")

	// And the recovery that remains is the store's own: when it returns, every replica -- the two
	// held and the one built without a connector -- rolls to the render that has both.
	recovered := getModelDeploymentBinding(t, cli)
	recovered.Status.Phase = KVCachePoolPhaseReady
	require.NoError(t, cli.Status().Update(context.Background(), recovered))

	allCurrent := func() bool {
		images := replicaImages(t, cli)
		if len(images) != 3 {
			return false
		}
		for _, image := range images {
			if image != "vllm/vllm-openai:v0.26.0" {
				return false
			}
		}

		return true
	}
	// The turnover's currency is an admitted replica: the rollout guard trades one admitted replica
	// for the departure it allows, so a world where nothing is admitted never starts rolling at all.
	// Standing in for Kueue between passes is what keeps that currency in circulation here.
	for pass := 0; pass < 8 && !allCurrent(); pass++ {
		standInForKueue(t, cli, true)
		if _, err = reconcileModelDeployment(t, cli); err != nil {
			break
		}
	}
	require.True(t, allCurrent(),
		"every replica carries the edit once the store returns, connector and all")
	require.Len(t, replicaNames(t, cli), 3)
}

// TestModelDeploymentReconciler_AFirstPassAnswersForTheReplicasItCreated states why a create counts
// as an answer and a comparison is not the only one.
//
// A deployment's first pass compares nothing -- the loop that compares runs over replicas that exist
// -- and it creates every replica from the render it just performed, which is a stronger claim about
// them being current than reading a hash back would be. Declining to answer there would push the
// first status write of the condition into the second pass, and this controller's contract is that a
// second pass over an unchanged spec writes nothing at all.
func TestModelDeploymentReconciler_AFirstPassAnswersForTheReplicasItCreated(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2, "the pass created replicas it had not compared")

	assert.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the replicas it just rendered and created are current by construction")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Zero(t, writes.statusUpdates,
		"and the condition is already settled, so the second pass over an unchanged spec writes nothing")
}

// TestModelDeploymentReconciler_ARolloutReportsInProgressThenCurrent walks the ordinary rollout, so
// the held case has a sibling that is not held and the reason is doing work.
//
// THE ROLLOUT IS ONE REPLICA AT A TIME: a pass that finds an outdated replica deletes it and creates
// nothing beside the deletion, the next pass creates the replacement, and the two-step repeats once
// per replica that carried the earlier spec. The reasons arrive in the same order: in progress while
// a replica is away or outdated, current once the last replacement lands.
//
// THE FIXTURE STANDS IN FOR KUEUE BETWEEN PASSES because the rollout guard spends an admitted
// replica on every departure it allows -- a world where nothing is admitted never starts rolling,
// so without the stand-in this walk would not take its first step.
func TestModelDeploymentReconciler_ARolloutReportsInProgressThenCurrent(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	standInForKueue(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 1,
		"the rollout deletes one outdated replica per pass and creates nothing beside it")
	assert.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)))

	standInForKueue(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2,
		"the pass after the deletion creates the replacement")
	assert.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"one replica still carries the earlier spec, so the rollout is still in flight")

	standInForKueue(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 1, "the last outdated replica goes in its own turn")

	standInForKueue(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	assert.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the replacements were rendered from the current spec by the pass that created them")
}

// TestModelDeploymentReconciler_AReplacementIsNotARollout is the distinction this condition exists
// to make, end to end, in one table: a replica missing because the SPEC changed is a rollout, and a
// replica missing while the spec never moved is a replacement. The two states look alike from the
// count alone -- one declared replica is absent either way -- and before they were told apart the
// second read as UpToDate, exactly the steady state, while the deployment was serving below the
// count it declared.
//
// THE REPLACEMENT ROW IS THE PASS THAT WAITS. The group's Workload exists and has not asked for the
// replacement yet, which is the window a departure spends in: creating before the ask is the move
// Kueue answers by evicting the replacement itself. The Workload is also what makes the state
// legible at all -- Kueue composes one only for a group that once counted its full total, so a
// short count under an existing Workload is a lost replica rather than a deployment still
// assembling its first set.
func TestModelDeploymentReconciler_AReplacementIsNotARollout(t *testing.T) {
	cases := []struct {
		name       string
		arrange    func(t *testing.T) ctrlcli.Client
		wantReason string
		wantIn     string
	}{
		{
			name: "the spec changed, so the missing replica is a rollout",
			arrange: func(t *testing.T) ctrlcli.Client {
				cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
					newRenderBinding(), newRenderPool(), newRenderBackend())

				_, err := reconcileModelDeployment(t, cli)
				require.NoError(t, err)

				changed := getModelDeployment(t, cli)
				changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
				require.NoError(t, cli.Update(context.Background(), changed))

				_, err = reconcileModelDeployment(t, cli)
				require.NoError(t, err)

				return cli
			},
			wantReason: modelDeploymentReasonRolloutInProgress,
			wantIn:     "turn over one per role per pass",
		},
		{
			name: "a replica left while nothing changed the spec",
			arrange: func(t *testing.T) ctrlcli.Client {
				ctx := context.Background()
				cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

				_, err := reconcileModelDeployment(t, cli)
				require.NoError(t, err)
				names := replicaNames(t, cli)
				require.Len(t, names, 2)

				// The replica leaves the way a live cluster loses it: the delete lands, Kueue's
				// finalizer holds the Pod, and it stays on the books under the ordinal's group. A
				// Pod deleted without the finalizer is GONE, and a gone ordinal has no ask to wait
				// on -- the pass creates freely and the count is whole again.
				gone := new(core.Pod)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
				gone.Finalizers = []string{kueuepodconst.PodFinalizer}
				require.NoError(t, cli.Update(ctx, gone))
				require.NoError(t, cli.Delete(ctx, gone))
				require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), false)))

				_, err = reconcileModelDeployment(t, cli)
				require.NoError(t, err)

				return cli
			},
			wantReason: modelDeploymentReasonReplacementInProgress,
			wantIn:     "no rollout is in flight",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := getModelDeployment(t, tc.arrange(t))

			assert.Equal(t, tc.wantReason, ModelDeploymentConditionReplicasUpToDate.GetReason(md))
			assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(md), tc.wantIn)
		})
	}

	// THE REPLACEMENT ENDS, AND ENDS AS THE STEADY ANSWER. The departing member leaving is what
	// releases the replacement, the count returns, and the condition that reported the loss goes
	// quiet -- or the reason would be a state nothing clears.
	//
	// WHAT RELEASES IT IS THE SLOT EMPTYING, NOT THE GROUP ASKING. A departing member still on the
	// books is still a member of a group that declares one, so creating beside it would put two
	// there and Kueue would delete the newer of them -- the replacement itself. The ask is asserted
	// here precisely because it must NOT move anything: it is no longer an input to this decision,
	// and a case that never set it could not tell that apart from a case where it still mattered.
	t.Run("the replacement lands and the answer returns to UpToDate", func(t *testing.T) {
		ctx := context.Background()
		cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		names := replicaNames(t, cli)
		require.Len(t, names, 2)

		gone := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
		gone.Finalizers = []string{kueuepodconst.PodFinalizer}
		require.NoError(t, cli.Update(ctx, gone))
		require.NoError(t, cli.Delete(ctx, gone))
		require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), false)))

		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		require.Equal(t, modelDeploymentReasonReplacementInProgress,
			ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
			"the wait window is the state the row above pinned; this walk starts from it")

		setGroupAsk(t, cli, true)
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		require.Len(t, replicaNames(t, cli), 2,
			"the ask moves nothing: the departing member is still on the books, so its slot is "+
				"still taken and no replacement is created beside it")
		require.Equal(t, modelDeploymentReasonReplacementInProgress,
			ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
			"and the deployment still reports the loss it is waiting out")

		// Kueue releases the departing member: its finalizer goes and the object with it, which is
		// the event the create gate has been reading for.
		departing := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, departing))
		departing.Finalizers = nil
		require.NoError(t, cli.Update(ctx, departing))

		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		require.Len(t, replicaNames(t, cli), 2,
			"the slot read empty, so the replacement lands beside the survivor and the count is "+
				"whole again -- the departed member is gone rather than counted alongside it")

		assert.Equal(t, modelDeploymentReasonUpToDate,
			ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
			"the count is whole again and every replica matches the render")
	})
}

// TestModelDeploymentRollout_AHeldMessageDoesNotInventAnEdit pins the limit of what this condition
// can know. A pass during an outage renders every replica without a connector, so every replica's
// hash differs from the desired one whether or not anyone edited the deployment -- an ordinary store
// blink puts every deployment on the pool into the held state. The message therefore states the
// consequence conditionally and never asserts that a change is waiting.
func TestModelDeploymentRollout_AHeldMessageDoesNotInventAnEdit(t *testing.T) {
	observed := rolloutConditionOf(t, newRenderDeployment(), &modelDeploymentRollout{
		accounted: 2, outdated: 2, held: 2,
	})
	message := ModelDeploymentConditionReplicasUpToDate.GetMessage(observed)

	assert.NotContains(t, message, "The change is withheld",
		"an outage holds the rollout whether or not an edit is waiting, so the message cannot name one")
	assert.Contains(t, message, "an edit that changes a running replica's rendered Pod",
		"it states the consequence conditionally, and for the class this guard actually delays")
	assert.NotContains(t, message, "a spec edit",
		"the ordinals a scale adds are created during the outage too, so the class this names is "+
			"narrower than every spec edit")
}

// TestModelDeploymentReconciler_HoldsWithNoEditAtAll is that limit end to end: nothing about the
// deployment changed, and the store going away is enough to hold the rollout.
func TestModelDeploymentReconciler_HoldsWithNoEditAtAll(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaHashes(t, cli)
	require.Len(t, before, 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, before, replicaHashes(t, cli), "no edit was made and no replica moves")
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the connector the replicas were built with is gone from the render, which is a held rollout on its own")
}

// TestModelDeploymentReconciler_AnOutageDoesNotHoldTheScaleItself bounds what the held state
// actually delays, because "a spec edit is withheld" is broader than the guard.
//
// A scale names ordinals that do not exist yet, and a replica that does not exist cannot be given
// an address that does not exist either -- so the creates proceed during the outage, rendered
// without a connector, and the deployment reaches its new count while the store is away. What the
// guard delays is an edit to a replica that is RUNNING, and the held message says exactly that
// rather than the wider thing.
func TestModelDeploymentReconciler_AnOutageDoesNotHoldTheScaleItself(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 3,
		"the scale proceeds during the outage: the added ordinal exists, rendered without a "+
			"connector, and the deployment reaches its new count with the store away")
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the two running replicas are held -- the scale was not withheld, and their edit still is")
}

// TestModelDeploymentReconciler_ClearsTheHeldConditionWhenTheStoreReturns is the other half of the
// same story, and the reason the message promises the change is only withheld: the withheld edit
// rolls out on the pass that resolves a connection again.
func TestModelDeploymentReconciler_ClearsTheHeldConditionWhenTheStoreReturns(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaHashes(t, cli), 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the outage must reach the held state, or what follows proves nothing")

	kvcpb = getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseReady
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md := getModelDeployment(t, cli)
	assert.NotEqual(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"the connection resolved again, so nothing is held")
}

// TestModelDeploymentReconciler_AScaleStillVouchesForTheSurvivors is the shape the old stale-True
// hazard settled into. There is no rebuild pass any more -- the one pass that used to account for
// no replica -- so the harmful prior value True can only survive a pass that compares nothing, and
// the only such pass left is a teardown, whose Unknown is the observer case's own. What a scale
// does instead is vouch: the survivors are compared against the render of their own ordinals, and
// the condition says True because it looked and they match -- not because a pass that looked at
// nothing left the old value standing.
func TestModelDeploymentReconciler_AScaleStillVouchesForTheSurvivors(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the steady state is the precondition; without it the value is not stale-able")

	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 1
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 1, "the trim removed the highest ordinal")

	md := getModelDeployment(t, cli)
	assert.Equal(t, "True", ModelDeploymentConditionReplicasUpToDate.GetStatus(md),
		"the pass compared the survivor it kept, so it answers rather than leaving the old value")
	assert.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"and the answer is earned: the survivor matches the render of its own ordinal")
}

// TestModelDeploymentRollout_TheInProgressMessageTellsTheCadence pins the tense and the count. The
// pass turns an outdated set over one replica per role per pass, so a message that said "%d replicas
// ... were deleted" claimed deletions that pass had not issued yet -- it read the DIFFERING count as
// the DELETED count. What the pass does is delete at most one per role; the pass that observes a
// deleted one gone creates its replacement, and a reader told the replacements were already made
// would stop watching for them.
func TestModelDeploymentRollout_TheInProgressMessageTellsTheCadence(t *testing.T) {
	observed := rolloutConditionOf(t, newRenderDeployment(), &modelDeploymentRollout{
		accounted: 3, outdated: 3,
	})
	message := ModelDeploymentConditionReplicasUpToDate.GetMessage(observed)

	assert.NotContains(t, message, "were deleted",
		"the pass deleted at most one replica per role, so the differing count is not a deleted count")
	assert.NotContains(t, message, "were recreated",
		"the pass deletes and requeues; nothing is created until a later pass")
	assert.Contains(t, message, "one per role per pass",
		"it states the cadence, which is what tells a reader how long the rollout takes")
	assert.Contains(t, message, "at most one replica per role",
		"and what this pass itself did")
}

// TestModelDeploymentRollout_TheGapARolloutOpensStaysTheRollouts covers the one verdict that is not
// answerable from the objects, and the one case where the same observation has two causes.
//
// A rollout's last pass deletes the last outdated replica and creates nothing; the pass after it
// sees a shortfall with nothing outdated, which is what a replica leaving on its own also looks
// like. The two are told apart by what this controller said one pass ago, so the case that matters
// is the pair: the SAME inputs with only the previous verdict different must produce two different
// answers. A test carrying only the rollout half would pass against an implementation that returned
// that reason unconditionally.
//
// It calls the observer directly rather than through rolloutConditionOf, because the inputs this
// branch reads — the live Pods and each replica's Workload — are exactly the two that helper does not
// take, and a shortfall cannot be expressed without them.
func TestModelDeploymentRollout_TheGapARolloutOpensStaysTheRollouts(t *testing.T) {
	cases := []struct {
		name       string
		priorPass  func(*workercore.ModelDeployment)
		wantReason string
		wantPhrase string
	}{
		{
			name: "the previous pass was rolling out",
			priorPass: func(holder *workercore.ModelDeployment) {
				ModelDeploymentConditionReplicasUpToDate.False(holder,
					modelDeploymentReasonRolloutInProgress, "the pass before this one")
			},
			wantReason: modelDeploymentReasonRolloutInProgress,
			wantPhrase: "being replaced by the rollout in flight: role server replica 1",
		},
		{
			name: "the previous pass said everything matched",
			priorPass: func(holder *workercore.ModelDeployment) {
				ModelDeploymentConditionReplicasUpToDate.True(holder,
					modelDeploymentReasonUpToDate, "the pass before this one")
			},
			wantReason: modelDeploymentReasonReplacementInProgress,
			wantPhrase: "are missing: role server replica 1",
		},
		{
			name:       "there was no previous pass",
			priorPass:  func(*workercore.ModelDeployment) {},
			wantReason: modelDeploymentReasonReplacementInProgress,
			wantPhrase: "are missing: role server replica 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			holder := new(workercore.ModelDeployment)
			tc.priorPass(holder)

			// One live replica of the two the role declares, sitting at ORDINAL 0, so the empty slot
			// is ordinal 1 specifically and nothing this pass compared differed:
			// rollout.outdated stays zero.
			pods := []core.Pod{{ObjectMeta: meta.ObjectMeta{
				Name:      "qwen-server-abcde",
				Namespace: md.Namespace,
				UID:       types.UID("uid-qwen-server-0"),
				Labels: map[string]string{
					modelDeploymentLabelKeyComponent:   "server",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			}}}

			// The survivor carries a workload of its own, which is what makes the empty slot read
			// as a departure rather than as a role assembling its first set.
			survivor := &kueue.Workload{}
			survivor.Name, survivor.Namespace = "pod-qwen-server-abcde-abcde", md.Namespace
			survivor.OwnerReferences = []meta.OwnerReference{{
				APIVersion: "v1", Kind: "Pod", Name: pods[0].Name, UID: pods[0].UID,
			}}

			observeModelDeploymentRollout(holder, md, pods,
				modelDeploymentReplicaWorkloads(pods, []*kueue.Workload{survivor}),
				&modelDeploymentRollout{accounted: 1})

			assert.Equal(t, tc.wantReason,
				ModelDeploymentConditionReplicasUpToDate.GetReason(holder),
				"the shortfall is attributed from what the previous pass recorded")
			if tc.wantPhrase != "" {
				assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(holder),
					tc.wantPhrase, "and the message names the empty slot: %s",
					ModelDeploymentConditionReplicasUpToDate.GetMessage(holder))
			}
		})
	}
}

// TestModelDeploymentRollout_AMissingWorkloadAloneNamesNoReplacement pins the distinction the
// per-role lookup could not make: a role that lost ONE replica's workload while the others keep
// theirs is not a replacement, because no replica is missing.
//
// THE FIXTURE IS THE ONE THAT FOUND THIS. Three replicas declared, three Pods live, and the
// middle one's workload deleted by hand while its siblings' survive: the old role-granularity
// lookup hit a sibling's workload for the whole role, and the quota axis reported the survivors'
// reservation for the missing one. The rollout axis stayed silent here -- a missing WORKLOAD is not
// a missing REPLICA -- and this is the pin that it stays that way when the lookup finally knows
// about replicas: the verdict is UpToDate, and the quota condition owns the missing reservation.
func TestModelDeploymentRollout_AMissingWorkloadAloneNamesNoReplacement(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	holder := new(workercore.ModelDeployment)

	pods := make([]core.Pod, 0, 3)
	wls := make([]*kueue.Workload, 0, 2)
	for ordinal := 0; ordinal < 3; ordinal++ {
		pod := core.Pod{ObjectMeta: meta.ObjectMeta{
			Name:      fmt.Sprintf("qwen-server-%d", ordinal),
			Namespace: md.Namespace,
			UID:       types.UID(fmt.Sprintf("uid-%d", ordinal)),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent:   "server",
				modelDeploymentReplicaOrdinalLabel: fmt.Sprint(ordinal),
			},
		}}
		pods = append(pods, pod)

		if ordinal == 1 {
			continue
		}
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = fmt.Sprintf("pod-qwen-server-%d-abcde", ordinal), md.Namespace
		wl.OwnerReferences = []meta.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		}}
		wls = append(wls, wl)
	}

	observeModelDeploymentRollout(holder, md, pods,
		modelDeploymentReplicaWorkloads(pods, wls), &modelDeploymentRollout{accounted: 3})

	assert.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(holder),
		"no replica is missing, so the rollout axis has nothing to report however many workloads are")
}

// TestModelDeploymentRollout_MissingSlotsFillLowestFirst pins the arithmetic the message names
// slots by, against the one input where it can go wrong.
//
// A ROLE CARRYING A REPLICA WITH NO ORDINAL still counts that replica against the shortfall without
// claiming any slot for it, and the empty slots must then be enumerated lowest-first and only as
// many as the count is short -- the converger's own fill order -- or the message names a slot the
// next create will not fill. The no-ordinal replica here is not decoration: without it the
// arithmetic has no way to disagree with the obvious reading, and the case passes against an
// implementation that simply numbers the gaps 0, 1, 2.
func TestModelDeploymentRollout_MissingSlotsFillLowestFirst(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	holder := new(workercore.ModelDeployment)
	ModelDeploymentConditionReplicasUpToDate.True(holder,
		modelDeploymentReasonUpToDate, "the pass before this one")

	// Ordinals 1 and 2 occupied, one legacy replica with no ordinal at all: three live Pods against
	// four declared, so the count is short by one and the unoccupied ordinals are 0 and 3.
	pods := []core.Pod{
		{ObjectMeta: meta.ObjectMeta{
			Name: "qwen-server-1", Namespace: md.Namespace, UID: types.UID("uid-1"),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent: "server", modelDeploymentReplicaOrdinalLabel: "1",
			},
		}},
		{ObjectMeta: meta.ObjectMeta{
			Name: "qwen-server-2", Namespace: md.Namespace, UID: types.UID("uid-2"),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent: "server", modelDeploymentReplicaOrdinalLabel: "2",
			},
		}},
		{ObjectMeta: meta.ObjectMeta{
			Name: "qwen-server-abcde", Namespace: md.Namespace, UID: types.UID("uid-legacy"),
			Labels: map[string]string{modelDeploymentLabelKeyComponent: "server"},
		}},
	}

	survivor := &kueue.Workload{}
	survivor.Name, survivor.Namespace = "pod-qwen-server-1-abcde", md.Namespace
	survivor.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: pods[0].Name, UID: pods[0].UID,
	}}

	observeModelDeploymentRollout(holder, md, pods,
		modelDeploymentReplicaWorkloads(pods, []*kueue.Workload{survivor}),
		&modelDeploymentRollout{accounted: 3})

	assert.Equal(t, modelDeploymentReasonReplacementInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(holder))

	msg := ModelDeploymentConditionReplicasUpToDate.GetMessage(holder)
	assert.Contains(t, msg, "role server replica 0",
		"the one slot named is the lowest unoccupied one, which is the one the converger fills: %s", msg)
	assert.NotContains(t, msg, "replica 3",
		"a no-ordinal replica absorbed the shortfall, so slot 3 is not named: %s", msg)
}

// TestModelDeploymentRollout_ATopSlotGapReadsAsReplacementWhileAssembled pins the documented
// boundary from the other side: it is the classification the boundary comment says the
// indistinguishable case falls into, stated as a test so the comment cannot outlive the code.
//
// A SCALE-UP WHOSE CREATE HAS NOT LANDED and a top-slot replica that departed produce the same
// observation -- ordinals 0 and 1 live with workloads, slot 2 empty -- and this axis classifies
// both as replacement, because the role assembled once and nothing prior says a rollout is in
// flight. The boundary is real and it is deliberate: no reading available to a single pass could
// sort those two apart, and the alternative (reading a top gap as assembly) turns a top-slot
// rollout turnover into UpToDate mid-flight, which is a regression against yesterday's behavior
// rather than a refinement of it.
func TestModelDeploymentRollout_ATopSlotGapReadsAsReplacementWhileAssembled(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	holder := new(workercore.ModelDeployment)
	ModelDeploymentConditionReplicasUpToDate.True(holder,
		modelDeploymentReasonUpToDate, "the pass before this one")

	pods := make([]core.Pod, 0, 2)
	wls := make([]*kueue.Workload, 0, 2)
	for ordinal := 0; ordinal < 2; ordinal++ {
		pod := core.Pod{ObjectMeta: meta.ObjectMeta{
			Name:      fmt.Sprintf("qwen-server-%d", ordinal),
			Namespace: md.Namespace,
			UID:       types.UID(fmt.Sprintf("uid-%d", ordinal)),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent:   "server",
				modelDeploymentReplicaOrdinalLabel: fmt.Sprint(ordinal),
			},
		}}
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = fmt.Sprintf("pod-qwen-server-%d-abcde", ordinal), md.Namespace
		wl.OwnerReferences = []meta.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		}}

		pods = append(pods, pod)
		wls = append(wls, wl)
	}

	observeModelDeploymentRollout(holder, md, pods,
		modelDeploymentReplicaWorkloads(pods, wls), &modelDeploymentRollout{accounted: 2})

	assert.Equal(t, modelDeploymentReasonReplacementInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(holder))
	assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(holder),
		"role server replica 2",
		"the top slot is named, and classified as the boundary comment says it is")
}
