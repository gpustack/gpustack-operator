package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil, rollout)
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
			wantMessage: "2 replicas differed from what this pass rendered and were recreated",
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

// TestModelDeploymentRollout_ANilRecordLeavesTheConditionAlone states the rule a teardown pass and a
// rebuild pass both depend on: neither compares a hash, so neither has an answer, and reporting the
// zeroed record they happen to hold would read as a rollout that completed on the one pass that is
// deleting every replica.
//
// The second half is the positive baseline. "Unchanged" is satisfied by an observer that never
// writes at all, so the same fixture is driven with a record that MUST move it.
func TestModelDeploymentRollout_ANilRecordLeavesTheConditionAlone(t *testing.T) {
	md := newRenderDeployment()
	ModelDeploymentConditionReplicasUpToDate.False(md, modelDeploymentReasonRolloutHeldByCache,
		"held by a pass that ran before this one")

	kept := rolloutConditionOf(t, md, nil)
	assert.Equal(t, "False", ModelDeploymentConditionReplicasUpToDate.GetStatus(kept))
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(kept))
	assert.Equal(t, "held by a pass that ran before this one",
		ModelDeploymentConditionReplicasUpToDate.GetMessage(kept))

	moved := rolloutConditionOf(t, md.DeepCopy(), &modelDeploymentRollout{accounted: 1})
	require.Equal(t, "True", ModelDeploymentConditionReplicasUpToDate.GetStatus(moved),
		"the fixture must be one a record that did compare does move, or the assertions above are vacuous")

	// A record that compared nothing is the same answer as no record at all, and it reaches the
	// observer from every caller rather than from a call site that remembered to check.
	empty := rolloutConditionOf(t, md.DeepCopy(), &modelDeploymentRollout{})
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(empty),
		"a record with nothing compared must not overwrite what the last comparing pass found")
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
	changed.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
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

// TestModelDeploymentReconciler_ARebuildPassClaimsNothingAboutTheRollout covers the branch that
// passes no record at all. A rebuild deletes every replica without comparing a single hash, so the
// zeroed record it happens to hold means "compared nothing", not "nothing was outdated" -- and
// reporting the second would announce the rollout as complete on the one pass that is tearing the
// whole group down.
//
// Measured: removing that branch leaves the entire package green, so this case is what holds it.
func TestModelDeploymentReconciler_ARebuildPassClaimsNothingAboutTheRollout(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaHashes(t, cli), 2)

	kvcpb := getModelDeploymentBinding(t, cli)
	kvcpb.Status.Phase = KVCachePoolPhaseDegraded
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the held state is the precondition; without it a rebuild leaving the condition alone is indistinguishable from recomputing it")

	// A replica count change resizes the group, which takes the rebuild branch: every replica goes,
	// and no hash is compared on the way.
	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md := getModelDeployment(t, cli)
	assert.NotEqual(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"a pass that compared no hashes must not report the replicas as current")
	assert.Equal(t, modelDeploymentReasonRolloutHeldByCache,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"it reports what the last pass that did compare found, unchanged")
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
// The pass that deletes cannot also create -- the group is rebuilt by the pass that finds the members
// gone -- so the two answers arrive on two passes. If the names were still held by terminating
// replicas the creates would not land either, and the pass would vouch for nothing and leave the
// condition alone; that path is pinned on the record itself, since this client removes a deleted Pod
// outright rather than leaving it terminating.
func TestModelDeploymentReconciler_ARolloutReportsInProgressThenCurrent(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)))

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Empty(t, replicaNames(t, cli), "the rollout deletes before it recreates")
	assert.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	assert.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the replacements were rendered from the current spec by the pass that created them")
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
	assert.Contains(t, message, "an edit that changes a replica's rendered Pod without changing the group's shape",
		"it states the consequence conditionally, and for the class this guard actually delays")
	assert.NotContains(t, message, "a spec edit",
		"a group-shape edit takes the rebuild branch before this guard and is not delayed at all")
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

// TestModelDeploymentReconciler_AnOutageDoesNotHoldAGroupShapeEdit bounds what the held state
// actually delays, because "a spec edit is withheld" is broader than the guard.
//
// A replica count or role set change moves the group annotations, which makes the group resize --
// and a resize takes the rebuild branch, which deletes every replica before the cache guard is
// reached. So that class of edit proceeds during an outage. What the guard delays is an edit that
// changes a replica's rendered Pod without changing the group's shape, and the message says exactly
// that rather than the wider thing.
func TestModelDeploymentReconciler_AnOutageDoesNotHoldAGroupShapeEdit(t *testing.T) {
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

	assert.Empty(t, replicaNames(t, cli),
		"the resize rebuilds the group, and the cache guard never sees it: this edit is not withheld")
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
	changed.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
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
