package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

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

// TestModelDeploymentReconciler_ARebuildPassClaimsNothingAboutTheRollout covers a rebuild reached
// from the outage state rather than from the steady one. A rebuild deletes every replica without
// comparing a single hash, so the zeroed record it holds means "accounted for nothing", not "nothing
// was outdated" -- and reporting the second would announce the rollout as complete on the one pass
// that is tearing the whole group down.
//
// The answer is Unknown rather than the previous value, whatever that value was. Keeping it was the
// earlier behavior and it is what let a steady deployment's True survive this pass; the sibling case
// starting from True is TestModelDeploymentReconciler_ARebuildDoesNotLeaveAStaleTrue.
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
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
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
		"a pass that accounted for no replica must not report them as current")
	assert.Equal(t, "Unknown", ModelDeploymentConditionReplicasUpToDate.GetStatus(md))
	assert.Equal(t, modelDeploymentReasonRolloutNotObserved,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"it reports what the last pass that did compare found, unchanged")
}

// TestModelDeploymentReconciler_AGroupShapeChangeCarriesAWithheldEditThroughAnOutage pins the lever
// that bounds what the rollout guard costs.
//
// The guard withholds an edit that changes a replica's rendered Pod for as long as the KV cache
// connection cannot be resolved, and an outage has no deadline. Without a way out, a deployment
// broken by its own spec would stay broken until the store came back. The way out is that a change
// to the replica counts or to the set of roles moves the group annotations and takes the whole-group
// rebuild, which runs before the guard: the pass that finds the group gone renders it from the
// current spec, connector or no connector, so the withheld edit lands with it.
//
// It is a lever rather than an automatic recovery because of what it costs -- every replica reloads
// its weights, and the group comes back with no connector until the store returns -- and that is a
// trade only the operator can weigh against serving the older spec a while longer. The neighboring
// rebuild case resizes during an outage too, but it stops at the condition the rebuild pass writes
// and never reads what the replacements were built from.
func TestModelDeploymentReconciler_AGroupShapeChangeCarriesAWithheldEditThroughAnOutage(t *testing.T) {
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
		"the edit has to be withheld first, or the lever below has nothing to carry")
	for name, image := range replicaImages(t, cli) {
		require.Equal(t, "vllm/vllm-openai:v0.25.1", image,
			"%s still carries the spec it was built from", name)
	}

	// The lever, pulled while the store is still away: a replica count the group has to be resized to.
	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Empty(t, replicaNames(t, cli), "the rebuild takes the whole group down before it builds one")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	images := replicaImages(t, cli)
	assert.Len(t, images, 3)
	for name, image := range images {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", image,
			"%s came back carrying the edit the guard was withholding, with the store still away", name)
	}
	rebuilt := replicaHashes(t, cli)

	// And the second half of what the lever costs. The group it brought back carries no connector, so
	// the render that regains one differs from it and rolls every replica a second time. Waiting pays
	// one rebuild for the same edit, which is why this is the operator's trade rather than the
	// rollout's.
	recovered := getModelDeploymentBinding(t, cli)
	recovered.Status.Phase = KVCachePoolPhaseReady
	require.NoError(t, cli.Status().Update(context.Background(), recovered))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the connector the replicas were built without is now part of the render")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.NotEqual(t, rebuilt, replicaHashes(t, cli),
		"the replacements carry the connector, so they are not the replicas the lever created")
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
// per replica that carried the earlier spec. There is no Workload in this fixture, so each replace
// pass creates freely -- the gate's no-Workload branch -- and the reasons arrive in the same order:
// in progress while a replica is away or outdated, current once the last replacement lands.
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

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 1,
		"the rollout deletes one outdated replica per pass and creates nothing beside it")
	assert.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2,
		"the pass after the deletion creates the replacement")
	assert.Equal(t, modelDeploymentReasonRolloutInProgress,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"one replica still carries the earlier spec, so the rollout is still in flight")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 1, "the last outdated replica goes in its own turn")

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

				gone := new(core.Pod)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
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

	// THE REPLACEMENT ENDS, AND ENDS AS THE STEADY ANSWER. The ask releases the replacement, the
	// count returns, and the condition that reported the loss goes quiet -- or the reason would be
	// a state nothing clears.
	t.Run("the replacement lands and the answer returns to UpToDate", func(t *testing.T) {
		ctx := context.Background()
		cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		names := replicaNames(t, cli)
		require.Len(t, names, 2)

		gone := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
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
		require.Len(t, replicaNames(t, cli), 2, "the ask answers for the departed member")

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

// TestModelDeploymentReconciler_ARebuildDoesNotLeaveAStaleTrue is the case the rebuild case above
// could not see, and the difference is the fixture rather than the assertion.
//
// That case sets the prior condition to RolloutHeldByCache before the rebuild, because leaving a
// False value alone is what makes "this pass answered nothing" observable. But that is precisely the
// prior value for which leaving it alone is harmless. The harmful prior value is True: a steady
// deployment reaches UpToDate, a group-shape edit then deletes every replica without vouching for
// one, and an untouched condition goes on reporting that every replica matches the render while none
// exists at all.
func TestModelDeploymentReconciler_ARebuildDoesNotLeaveAStaleTrue(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(getModelDeployment(t, cli)),
		"the steady state is the precondition; without it the stale value is not True")

	resized := getModelDeployment(t, cli)
	resized.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), resized))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Empty(t, replicaNames(t, cli), "the rebuild deleted every replica")

	md := getModelDeployment(t, cli)
	assert.Equal(t, "Unknown", ModelDeploymentConditionReplicasUpToDate.GetStatus(md),
		"a pass that vouched for no replica states that it could not tell")
	assert.NotEqual(t, modelDeploymentReasonUpToDate,
		ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		"it must not go on claiming every replica is current while none exists")
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
// branch reads — the live Pods and the group's Workload — are exactly the two that helper does not
// take, and a shortfall cannot be expressed without them.
func TestModelDeploymentRollout_TheGapARolloutOpensStaysTheRollouts(t *testing.T) {
	const group = "qwen"

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
			wantPhrase: "being replaced by the rollout in flight",
		},
		{
			name: "the previous pass said everything matched",
			priorPass: func(holder *workercore.ModelDeployment) {
				ModelDeploymentConditionReplicasUpToDate.True(holder,
					modelDeploymentReasonUpToDate, "the pass before this one")
			},
			wantReason: modelDeploymentReasonReplacementInProgress,
			wantPhrase: "",
		},
		{
			name:       "there was no previous pass",
			priorPass:  func(*workercore.ModelDeployment) {},
			wantReason: modelDeploymentReasonReplacementInProgress,
			wantPhrase: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			holder := new(workercore.ModelDeployment)
			tc.priorPass(holder)

			// One live replica of the two the role declares, so the shortfall is one and nothing
			// this pass compared differed: rollout.outdated stays zero.
			pods := []core.Pod{{ObjectMeta: meta.ObjectMeta{
				Name:      "qwen-server-abcde",
				Namespace: md.Namespace,
				Labels:    map[string]string{modelDeploymentLabelKeyComponent: "server"},
			}}}

			observeModelDeploymentRollout(holder, md, pods,
				map[string]*kueue.Workload{group: {}}, map[string]string{"server": group},
				&modelDeploymentRollout{accounted: 1})

			assert.Equal(t, tc.wantReason,
				ModelDeploymentConditionReplicasUpToDate.GetReason(holder),
				"the shortfall is attributed from what the previous pass recorded")
			if tc.wantPhrase != "" {
				assert.Contains(t, ModelDeploymentConditionReplicasUpToDate.GetMessage(holder),
					tc.wantPhrase, "and the message sends the reader to the rollout, not to a diff")
			}
		})
	}
}
