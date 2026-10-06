package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

const (
	elasticTestNamespace = "default"
	elasticTestDeployUID = types.UID("deployment-uid")
	elasticTestDeployNm  = "qwen"
)

var (
	elasticMaster      = elasticCapturedIdentity{Name: "qwen-ray-0", UID: types.UID("worker-master")}
	elasticWorkerOne   = elasticCapturedIdentity{Name: "qwen-ray-1", UID: types.UID("worker-one")}
	elasticWorkerTwo   = elasticCapturedIdentity{Name: "qwen-ray-2", UID: types.UID("worker-two")}
	elasticWorkerThree = elasticCapturedIdentity{Name: "qwen-ray-3", UID: types.UID("worker-three")}
)

// elasticTestOperation is a record for the supplied widths and captured set. It is always valid
// unless a case makes it otherwise, so a case that fails is failing on the decision rather than on
// a fixture that was never a record.
func elasticTestOperation(width elasticWidth, workers ...elasticCapturedIdentity) *elasticOperation {
	return &elasticOperation{
		Name:            elasticTestDeployNm,
		Namespace:       elasticTestNamespace,
		DeploymentUID:   elasticTestDeployUID,
		Generation:      1,
		Width:           width,
		Workers:         workers,
		Master:          elasticMaster,
		State:           elasticStateRecorded,
		CommandIntent:   "set the collective width",
		ResourceVersion: "",
	}
}

// elasticCompleteFacts is a fully observed upward world at the supplied target width. A case starts
// from here and removes exactly the one fact it is about, so every case differs from the positive
// by a single missing thing.
func elasticCompleteFacts(target int) elasticFacts {
	return elasticFacts{
		DeploymentUID:       elasticTestDeployUID,
		Generation:          1,
		Desired:             knownLayer(target),
		Admitted:            knownLayer(target),
		AdmittedAllocation:  knownLayer(target),
		Ray:                 knownLayer(target),
		Effective:           unknownLayer("the engine has not reported a width yet"),
		NativeComplete:      false,
		ForwardProven:       false,
		WithdrawalConfirmed: false,
		ActorMappingKnown:   false,
		ActorFree:           map[types.UID]bool{},
		ReleaseObserved:     false,
	}
}

// elasticFactsWithLayers builds a fully observed upward world from plain values. A negative number
// means the layer keeps the fully observed value, and a reason string means the layer was not read
// and this is why. The tables carry data rather
// than a function that changes it, so every case reads as the facts it is about.
func elasticFactsWithLayers(target, admitted int, admittedNo string, allocated int, allocNo string, ray int, rayNo string) elasticFacts {
	facts := elasticCompleteFacts(target)
	if admittedNo != "" {
		facts.Admitted = unknownLayer(admittedNo)
	} else if admitted >= 0 {
		facts.Admitted = knownLayer(admitted)
	}
	if allocNo != "" {
		facts.AdmittedAllocation = unknownLayer(allocNo)
	} else if allocated >= 0 {
		facts.AdmittedAllocation = knownLayer(allocated)
	}
	if rayNo != "" {
		facts.Ray = unknownLayer(rayNo)
	} else if ray >= 0 {
		facts.Ray = knownLayer(ray)
	}

	return facts
}

// TestTheKernelWithholdsTheCommandUntilItsCapacityExists covers the upward direction, where a
// command asked for ahead of its capacity is a request the cluster cannot satisfy.
//
// EVERY CASE REMOVES EXACTLY ONE FACT from a fully observed world, so each refusal names the thing
// it is refusing because of rather than passing for want of any evidence at all.
func TestTheKernelWithholdsTheCommandUntilItsCapacityExists(t *testing.T) {
	for _, tc := range []struct {
		name       string
		admitted   int
		admittedNo string
		allocated  int
		allocNo    string
		ray        int
		rayNo      string
		action     elasticAction
		reason     string
	}{
		{
			name:     "a fully admitted allocated and corroborated world may ask",
			admitted: -1, allocated: -1, ray: -1, action: elasticActionRequestScale,
		},
		{
			name: "admission below the target withholds the command", admitted: 2, ray: 4, allocated: 4,
			action: elasticActionHold, reason: "only 2 workers are admitted",
		},
		{
			name: "an unobserved admission withholds the command", admittedNo: "the listing failed", ray: 4, allocated: 4,
			action: elasticActionHold, reason: "admitted width is unknown",
		},
		{
			name: "an unallocated admitted worker withholds the command", admitted: 4, ray: 4,
			allocNo: "the ledger was not read", action: elasticActionHold, reason: "admitted allocation is unknown",
		},
		{
			name: "a smaller allocation withholds the command", admitted: 4, ray: 4, allocated: 3,
			action: elasticActionHold, reason: "only 3 workers hold an allocation",
		},
		{
			name: "a ray world that has not caught up withholds the command", admitted: 4, allocated: 4, ray: 2,
			action: elasticActionHold, reason: "the ray world holds 2 members",
		},
		{
			name: "an unobserved ray world withholds the command", admitted: 4, allocated: 4,
			rayNo: "the head did not answer", action: elasticActionHold, reason: "the ray world is unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := elasticTestOperation(
				elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
			facts := elasticFactsWithLayers(4, tc.admitted, tc.admittedNo,
				tc.allocated, tc.allocNo, tc.ray, tc.rayNo)

			decision := decideElasticOperation(op, facts)
			assert.Equal(t, tc.action, decision.Action, "reason=%s", decision.Reason)
			if tc.reason != "" {
				assert.Contains(t, decision.Reason, tc.reason)
			}
			if tc.action == elasticActionRequestScale {
				assert.True(t, !op.CommandSent,
					"deciding to ask does not itself send anything")
			}
		})
	}
}

// TestTheFourLayersAreIndependentStates covers the property the whole design rests on: a missing
// layer is never filled in from a neighbor.
//
// Each case zeroes one layer and leaves the other three fully observed. A kernel that borrowed a
// value would answer from the layer that happened to be populated, and the decision would look
// ordinary while resting on nothing.
func TestTheFourLayersAreIndependentStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layer  string
		action elasticAction
		reason string
	}{
		{
			name: "an unknown desired width stops it", layer: "desired", action: elasticActionHold,
			reason: "the desired width is unknown",
		},
		{
			name: "an unknown admission stops it", layer: "admitted", action: elasticActionHold,
			reason: "admitted width is unknown",
		},
		{
			name: "an unknown allocation stops it", layer: "allocation", action: elasticActionHold,
			reason: "admitted allocation is unknown",
		},
		{
			name: "an unknown ray world stops it", layer: "ray", action: elasticActionHold,
			reason: "the ray world is unknown",
		},
		{
			name:  "an unknown effective width does not stop a command that has not been sent",
			layer: "effective", action: elasticActionRequestScale,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := elasticTestOperation(
				elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
			facts := elasticCompleteFacts(4)
			switch tc.layer {
			case "desired":
				facts.Desired = unknownLayer("not read")
			case "admitted":
				facts.Admitted = unknownLayer("not read")
			case "allocation":
				facts.AdmittedAllocation = unknownLayer("not read")
			case "ray":
				facts.Ray = unknownLayer("not read")
			case "effective":
				facts.Effective = unknownLayer("not read")
			}

			decision := decideElasticOperation(op, facts)
			assert.Equal(t, tc.action, decision.Action, "reason=%s", decision.Reason)
			if tc.reason != "" {
				assert.Contains(t, decision.Reason, tc.reason)
			}
		})
	}
}

// TestACommandThatHasLeftIsNeverSentAgain is the property the durable record exists for.
//
// EVERY AMBIGUOUS ANSWER LOOKS THE SAME from outside: a timeout, a 408, a 500, an unreadable body
// and a request that never left are indistinguishable to anything that did not record the intent
// first. Each case below is one of them, and each must reconstruct into a wait.
func TestACommandThatHasLeftIsNeverSentAgain(t *testing.T) {
	// Each case is the state of a world, written as data: whether the engine finished, what width
	// it reports, whether a request was served there, and which generation the facts were read at.
	for _, tc := range []struct {
		name       string
		native     bool
		effective  int
		forward    bool
		generation int64
		action     elasticAction
	}{
		{
			name: "a request that timed out is a wait, not a retry", native: false, effective: -1,
			action: elasticActionAwaitNative,
		},
		{
			name: "a 408 leaves the engine unreported, so it is a wait", native: false, effective: -1,
			action: elasticActionAwaitNative,
		},
		{
			name: "a 500 leaves the engine unreported, so it is a wait", native: false, effective: -1,
			action: elasticActionAwaitNative,
		},
		{
			name: "an unreadable answer is a wait", native: false, effective: -1,
			action: elasticActionAwaitNative,
		},
		{
			name: "a generation change reconstructs rather than asking again", native: true, effective: 4,
			generation: 2, action: elasticActionHold,
		},
		{
			name: "an engine that finished without a forward does not complete it", native: true,
			effective: 4, action: elasticActionAwaitForward,
		},
		{
			name: "a finished engine with a proven forward completes an upward operation", native: true,
			effective: 4, forward: true, action: elasticActionComplete,
		},
		{
			name: "an engine reporting the wrong width does not complete it", native: true, effective: 2,
			action: elasticActionHold,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := elasticTestOperation(
				elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
			// The command left and the record says so, which is what every one of these cases shares.
			op.State = elasticStateCommandSent
			op.CommandSent = true
			facts := elasticCompleteFacts(4)
			facts.NativeComplete = tc.native
			if tc.effective >= 0 {
				facts.Effective = knownLayer(tc.effective)
			}
			facts.ForwardProven = tc.forward
			facts.Generation = 1
			if tc.generation > 0 {
				facts.Generation = tc.generation
			}

			decision := decideElasticOperation(op, facts)
			assert.Equal(t, tc.action, decision.Action, "reason=%s", decision.Reason)
			assert.NotEqual(t, elasticActionRequestScale, decision.Action,
				"a record that says a command left may never decide to send another")
		})
	}
}

// TestARecordedRefusalIsRetriedWithinItsBound is the one answer that is not a guess: the engine
// answered with a refusal and the record kept its text. Each case is one follow-up question about
// that refusal — whether an ordinary wait still applies, whether the engine is free to be asked
// again, and what happens when the bound is spent.
func TestARecordedRefusalIsRetriedWithinItsBound(t *testing.T) {
	refused := func() *elasticOperation {
		op := elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
		op.State = elasticStateCommandSent
		op.CommandSent = true
		op.ScaleAttempts = 1
		op.ScaleLastError = "elastic engine /scale_elastic_ep: engine reported failure (status 500)"

		return op
	}
	// The engine answered the is-scaling question and is idle, which is the only engine a refused
	// command is re-issued to.
	idle := elasticCompleteFacts(4)
	idle.ScalingKnown = true

	for _, tc := range []struct {
		name      string
		attempts  int
		state     elasticOperationState
		noRefusal bool
		facts     func() elasticFacts
		action    elasticAction
		state2    elasticOperationState
	}{
		{
			name: "a record without a refusal still waits the ordinary way", attempts: 1, noRefusal: true,
			facts:  func() elasticFacts { return elasticCompleteFacts(4) },
			action: elasticActionAwaitNative, state2: elasticStateAwaitingWidth,
		},
		{
			name: "a refusal to an idle engine is issued again", attempts: 1,
			facts:  func() elasticFacts { return idle },
			action: elasticActionRetryScale, state2: elasticStateCommandSent,
		},
		{
			name: "the last attempt before the bound is still issued again", attempts: 4,
			facts:  func() elasticFacts { return idle },
			action: elasticActionRetryScale, state2: elasticStateCommandSent,
		},
		{
			name: "a scaling engine is not asked again mid-flight", attempts: 1,
			facts:  func() elasticFacts { f := idle; f.Scaling = true; return f },
			action: elasticActionHold, state2: elasticStateCommandSent,
		},
		{
			name: "an engine that did not answer is not assumed idle", attempts: 1,
			facts:  func() elasticFacts { f := idle; f.ScalingKnown = false; return f },
			action: elasticActionHold, state2: elasticStateCommandSent,
		},
		{
			name: "the bound's end abandons the operation", attempts: 5,
			facts:  func() elasticFacts { return idle },
			action: elasticActionHold, state2: elasticStateAbandoned,
		},
		{
			name: "an abandoned record keeps holding at the same answer", attempts: 5,
			state:  elasticStateAbandoned,
			facts:  func() elasticFacts { return idle },
			action: elasticActionHold, state2: elasticStateAbandoned,
		},
		{
			name: "a refusal the engine satisfied completes instead of retrying", attempts: 1,
			facts: func() elasticFacts {
				f := idle
				f.NativeComplete = true
				f.Effective = knownLayer(4)
				f.ForwardProven = true
				return f
			},
			action: elasticActionComplete, state2: elasticStateCompleted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := refused()
			if tc.state != "" {
				op.State = tc.state
			}
			op.ScaleAttempts = tc.attempts
			if tc.noRefusal {
				op.ScaleLastError = ""
			}

			decision := decideElasticOperation(op, tc.facts())
			assert.Equal(t, tc.action, decision.Action, "reason=%s", decision.Reason)
			assert.Equal(t, tc.state2, decision.State, "reason=%s", decision.Reason)
			// Every refusal answer carries the engine's own text, so no pass of a refused command
			// is left reporting only that a width is unreported. The one completion here is the
			// engine's own success answer, which speaks for itself.
			if !tc.noRefusal && decision.Action != elasticActionComplete {
				assert.Contains(t, decision.Reason, "status 500", "reason=%s", decision.Reason)
			}
		})
	}
}

// TestARecordFromBeforeTheRetryBoundStillReads pins the record schema's one-way compatibility: a
// document the previous operator version wrote carries none of the retry fields, and this version
// must read it as the ordinary sent record it always was — and must write nothing new back into a
// record that never carried a refusal, so its documents stay byte-for-byte what they were.
func TestARecordFromBeforeTheRetryBoundStillReads(t *testing.T) {
	legacy := `{"name":"qwen","deploymentUID":"deployment-uid","namespace":"default","generation":1,` +
		`"width":{"old":2,"target":4},"master":{"name":"qwen-ray-0","uid":"worker-master"},` +
		`"commandIntent":"scale to 4","commandSent":true,"state":"CommandSent"}`
	ctx := context.Background()

	t.Run("a legacy document reads as the sent record it is", func(t *testing.T) {
		cli := elasticTestClient()
		stored := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      elasticOperationRecordName(elasticTestDeployUID),
				Namespace: elasticTestNamespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: workercore.GroupVersion.String(), Kind: "ModelDeployment",
					Name: elasticTestDeployNm, UID: elasticTestDeployUID, Controller: elasticControllerRef(),
				}},
			},
			Data: map[string]string{elasticOperationDataKey: legacy},
		}
		require.NoError(t, cli.Create(ctx, stored))

		read, err := newElasticOperationStore(cli).Read(ctx, elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
		require.NoError(t, err)
		assert.Equal(t, elasticStateCommandSent, read.State)
		assert.Equal(t, 0, read.ScaleAttempts)
		assert.Empty(t, read.ScaleLastError)
		assert.Nil(t, read.ScaleLastFailedAt)
	})

	t.Run("a refusal-free record writes none of the retry fields", func(t *testing.T) {
		op := elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
		op.State = elasticStateCommandSent
		op.CommandSent = true
		op.CommandIntent = "scale to 4"

		document, err := json.Marshal(op)
		require.NoError(t, err)
		for _, field := range []string{"scaleAttempts", "scaleLastError", "scaleLastFailedAt"} {
			assert.NotContains(t, string(document), field,
				"a record the old schema could express must decode as it always did")
		}
	})
}

// TestNothingIsRemovedUntilEveryWorkerIsProvenActorFree is the direction that deletes, so each case
// removes exactly one proof and each refusal names it.
func TestNothingIsRemovedUntilEveryWorkerIsProvenActorFree(t *testing.T) {
	// A downward operation whose engine has finished, whose new width is observed and served, and
	// whose serving side has confirmed the narrowing. Everything that remains is about the workers.
	settled := func() *elasticOperation {
		op := elasticTestOperation(
			elasticWidth{Old: 4, Target: 2}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
		op.State = elasticStateWithdrawn
		op.CommandSent = true

		return op
	}
	proved := func() elasticFacts {
		return elasticFacts{
			DeploymentUID:       elasticTestDeployUID,
			Generation:          1,
			Desired:             knownLayer(2),
			Admitted:            knownLayer(4),
			AdmittedAllocation:  knownLayer(4),
			Ray:                 knownLayer(2),
			Effective:           knownLayer(2),
			NativeComplete:      true,
			ForwardProven:       true,
			WithdrawalConfirmed: true,
			ActorMappingKnown:   true,
			ActorFree: map[types.UID]bool{
				elasticWorkerOne.UID:   true,
				elasticWorkerTwo.UID:   true,
				elasticWorkerThree.UID: true,
			},
		}
	}

	// Each case is a world written as data: which proofs are present, which worker has not been
	// proven, and whether the captured set has been changed underneath the record.
	for _, tc := range []struct {
		name          string
		native        bool
		effective     int
		forward       bool
		withdrawal    bool
		mappingKnown  bool
		holdsActor    types.UID
		unmapped      types.UID
		recreateThird bool
		captureMaster bool
		action        elasticAction
		reason        string
	}{
		{
			name: "a fully proven captured set may be released", native: true, effective: 2, forward: true,
			withdrawal: true, mappingKnown: true, action: elasticActionRelease,
		},
		{
			name: "an engine that has not finished withholds everything", effective: 2, forward: true,
			withdrawal: true, mappingKnown: true, action: elasticActionAwaitNative,
		},
		{
			name: "an unobserved effective width withholds everything", native: true, effective: -1, forward: true,
			withdrawal: true, mappingKnown: true, action: elasticActionHold,
			reason: "the effective width is unknown",
		},
		{
			name: "no real forward withholds everything", native: true, effective: 2,
			withdrawal: true, mappingKnown: true, action: elasticActionAwaitForward,
		},
		{
			name: "an unconfirmed withdrawal withholds everything", native: true, effective: 2, forward: true,
			mappingKnown: true, action: elasticActionAwaitWithdrawal,
		},
		{
			name: "a worker that still holds an actor withholds the whole set", native: true, effective: 2,
			forward: true, withdrawal: true, mappingKnown: true, holdsActor: elasticWorkerTwo.UID,
			action: elasticActionHold, reason: "still holds an actor",
		},
		{
			name: "an unreadable actor mapping withholds the whole set", native: true, effective: 2,
			forward: true, withdrawal: true, action: elasticActionHold,
			reason: "actor mapping could not be read",
		},
		{
			name: "a worker nothing mapped is not proven actor-free", native: true, effective: 2,
			forward: true, withdrawal: true, mappingKnown: true, unmapped: elasticWorkerThree.UID,
			action: elasticActionHold, reason: "has no actor evidence",
		},
		{
			name: "a recreated worker under the captured name withholds the set", native: true, effective: 2,
			forward: true, withdrawal: true, mappingKnown: true, recreateThird: true,
			action: elasticActionHold, reason: "has no actor evidence",
		},
		{
			name: "a captured master is never retirable", native: true, effective: 2, forward: true,
			withdrawal: true, mappingKnown: true, captureMaster: true,
			action: elasticActionHold, reason: "master is also captured as retirable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := settled()
			if tc.captureMaster {
				op.Workers = append(op.Workers, elasticMaster)
			}
			if tc.recreateThird {
				// The same name with a new identity. Looking the worker up by name would remove the
				// replacement, so the captured UID is what is looked up and it is not present.
				op.Workers[2] = elasticCapturedIdentity{
					Name: elasticWorkerThree.Name, UID: types.UID("recreated-uid"),
				}
			}
			facts := proved()
			facts.NativeComplete = tc.native
			if tc.effective >= 0 {
				facts.Effective = knownLayer(tc.effective)
			} else {
				facts.Effective = unknownLayer("the engine did not report")
			}
			facts.ForwardProven = tc.forward
			facts.WithdrawalConfirmed = tc.withdrawal
			facts.ActorMappingKnown = tc.mappingKnown
			switch {
			case tc.holdsActor != "":
				facts.ActorFree[tc.holdsActor] = false
			case tc.unmapped != "":
				delete(facts.ActorFree, tc.unmapped)
			}

			decision := decideElasticOperation(op, facts)
			assert.Equal(t, tc.action, decision.Action, "reason=%s", decision.Reason)
			if tc.reason != "" {
				assert.Contains(t, decision.Reason, tc.reason)
			}
		})
	}
}

// TestAReleaseIsNotCompleteUntilItIsObserved closes the last gap: a delete that was sent is not a
// release, and a released operation is not finished until the ledger and the quota say so.
func TestAReleaseIsNotCompleteUntilItIsObserved(t *testing.T) {
	op := elasticTestOperation(
		elasticWidth{Old: 4, Target: 2}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
	op.State = elasticStateReleased
	op.CommandSent = true

	facts := elasticFacts{
		DeploymentUID:       elasticTestDeployUID,
		Generation:          1,
		ActorMappingKnown:   true,
		ActorFree:           map[types.UID]bool{elasticWorkerOne.UID: true},
		ReleaseObserved:     false,
		NativeComplete:      true,
		Effective:           knownLayer(2),
		ForwardProven:       true,
		WithdrawalConfirmed: true,
	}

	held := elasticReleaseComplete(op, facts)
	assert.Equal(t, elasticActionHold, held.Action, "reason=%s", held.Reason)
	assert.Contains(t, held.Reason, "not been seen released")

	facts.ReleaseObserved = true
	done := elasticReleaseComplete(op, facts)
	assert.Equal(t, elasticActionComplete, done.Action)
	assert.Equal(t, elasticStateCompleted, done.State)
}

// TestTheStoreKeepsTheRecordUnderTheDeployment covers the durable half: a record is owned by the
// deployment's own UID, is written before anything may be asked of the engine, and is readable only
// by the deployment it belongs to.
func TestTheStoreKeepsTheRecordUnderTheDeployment(t *testing.T) {
	t.Run("a stored record reads back with its owner and version", func(t *testing.T) {
		cli := elasticTestClient()
		store := newElasticOperationStore(cli)
		op := elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)

		stored, err := store.Create(context.Background(), op)
		require.NoError(t, err)
		require.NotEmpty(t, stored.ResourceVersion, "a stored record carries the object's version")

		read, err := store.Read(
			context.Background(), elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
		require.NoError(t, err)
		assert.Equal(t, op.Width, read.Width)
		assert.Equal(t, op.Workers, read.Workers)
		assert.Equal(t, elasticTestDeployUID, read.DeploymentUID)
	})

	t.Run("a record is owned by the deployment's own uid", func(t *testing.T) {
		cli := elasticTestClient()
		store := newElasticOperationStore(cli)
		_, err := store.Create(context.Background(), elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree))
		require.NoError(t, err)

		stored := new(corev1.ConfigMap)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{
			Namespace: elasticTestNamespace,
			Name:      elasticOperationRecordName(elasticTestDeployUID),
		}, stored))
		owner := metav1.GetControllerOf(stored)
		require.NotNil(t, owner)
		assert.Equal(t, elasticTestDeployUID, owner.UID)
		assert.Equal(t, "ModelDeployment", owner.Kind)
	})

	t.Run("a record naming another deployment is not this one's record", func(t *testing.T) {
		cli := elasticTestClient()
		store := newElasticOperationStore(cli)
		_, err := store.Create(context.Background(), elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree))
		require.NoError(t, err)

		_, err = store.Read(context.Background(), elasticTestNamespace, elasticTestDeployNm,
			types.UID("someone-else"))
		require.Error(t, err, "a record owned by one deployment is not readable as another's")
	})
}

// TestTheStoreRefusesAnythingThatIsNotAUsableRecord covers every shape that would otherwise decode
// into a record and be acted on.
func TestTheStoreRefusesAnythingThatIsNotAUsableRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		owner  *types.UID
		reason string
	}{
		{
			name:   "an empty body is refused",
			body:   "",
			reason: "empty",
		},
		{
			name:   "malformed json is refused",
			body:   "{not json",
			reason: "not usable",
		},
		{
			name:   "a trailing value is refused",
			body:   `{"deploymentUID":"deployment-uid","generation":1,"width":{"old":2,"target":4},"state":"Recorded","master":{"name":"m","uid":"u"},"commandIntent":"go"} {"another":1}`,
			reason: "trailing JSON value",
		},
		{
			name:   "an unknown field is refused",
			body:   `{"deploymentUID":"deployment-uid","generation":1,"width":{"old":2,"target":4},"state":"Recorded","master":{"name":"m","uid":"u"},"surprise":true}`,
			reason: "not usable",
		},
		{
			name:   "a record naming no deployment is refused",
			body:   `{"generation":1,"width":{"old":2,"target":4},"state":"Recorded","master":{"name":"m","uid":"u"}}`,
			reason: "names no deployment",
		},
		{
			name:   "a non-positive width is refused",
			body:   `{"deploymentUID":"deployment-uid","generation":1,"width":{"old":0,"target":4},"state":"Recorded","master":{"name":"m","uid":"u"}}`,
			reason: "not a resize",
		},
		{
			name:   "a record with no captured identity is refused",
			body:   `{"deploymentUID":"deployment-uid","generation":1,"width":{"old":2,"target":4},"state":"Recorded"}`,
			reason: "captured no master identity",
		},
		{
			name:   "a record with no controlling owner is refused",
			body:   `{"deploymentUID":"deployment-uid","generation":1,"width":{"old":2,"target":4},"state":"Recorded","master":{"name":"m","uid":"u"}}`,
			owner:  new(types.UID),
			reason: "no controlling owner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := elasticTestClient()
			store := newElasticOperationStore(cli)
			_, err := store.Create(context.Background(), elasticTestOperation(
				elasticWidth{Old: 2, Target: 4}, elasticWorkerOne))
			require.NoError(t, err)

			stored := new(corev1.ConfigMap)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{
				Namespace: elasticTestNamespace,
				Name:      elasticOperationRecordName(elasticTestDeployUID),
			}, stored))
			stored.Data = map[string]string{elasticOperationDataKey: tc.body}
			if tc.owner != nil {
				stored.OwnerReferences = nil
			}
			require.NoError(t, cli.Update(context.Background(), stored))

			_, err = store.Read(
				context.Background(), elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

// TestAWriteAgainstAMovedRecordChangesNothing is the optimistic half, and it is checked by reading
// the stored object afterwards rather than by the error alone.
func TestAWriteAgainstAMovedRecordChangesNothing(t *testing.T) {
	cli := elasticTestClient()
	store := newElasticOperationStore(cli)
	op, err := store.Create(context.Background(), elasticTestOperation(
		elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree))
	require.NoError(t, err)
	readVersion := op.ResourceVersion

	// The record moves on while this pass holds the older version.
	stored := new(corev1.ConfigMap)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{
		Namespace: elasticTestNamespace, Name: elasticOperationRecordName(elasticTestDeployUID),
	}, stored))
	document := map[string]any{
		"deploymentUID": elasticTestDeployUID, "generation": 1,
		"width": map[string]any{"old": 2, "target": 4},
		"state": elasticStateCommandSent, "master": map[string]any{"name": "m", "uid": "u"},
		"commandIntent": "the other writer's intent", "commandSent": true,
	}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	stored.Data = map[string]string{elasticOperationDataKey: string(raw)}
	require.NoError(t, cli.Update(context.Background(), stored))

	// The pass holding the older version tries to write its own decision. A completed record is one
	// whose command left, so it says so rather than contradicting its own state.
	op.State = elasticStateCompleted
	op.CommandSent = true
	writeErr := store.Update(context.Background(), op)

	require.Error(t, writeErr)
	assert.True(t, apierrors.IsConflict(writeErr), "a moved record is a conflict, not a bad write")
	assert.Equal(t, readVersion, op.ResourceVersion,
		"a failed write leaves the caller's version exactly as it was")

	after, err := store.Read(
		context.Background(), elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
	require.NoError(t, err)
	assert.Equal(t, elasticStateCommandSent, after.State, "the other writer's record is intact")
	assert.True(t, after.CommandSent)
	assert.Equal(t, "the other writer's intent", after.CommandIntent)
}

// TestANewRecordValidatesItsIdentityAndWidths keeps an unusable record from ever being stored, so
// the kernel is never handed one that was written by accident.
func TestANewRecordValidatesItsIdentityAndWidths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*elasticOperation)
		reason string
	}{
		{
			name:   "a record naming no deployment is refused",
			mutate: func(op *elasticOperation) { op.DeploymentUID = "" },
			reason: "names no deployment",
		},
		{
			name:   "a record bound to no generation is refused",
			mutate: func(op *elasticOperation) { op.Generation = 0 },
			reason: "no spec generation",
		},
		{
			name:   "a zero old width is refused",
			mutate: func(op *elasticOperation) { op.Width.Old = 0 },
			reason: "not a resize",
		},
		{
			name:   "a negative target width is refused",
			mutate: func(op *elasticOperation) { op.Width.Target = -4 },
			reason: "not a resize",
		},
		{
			name:   "a target equal to the old width is refused",
			mutate: func(op *elasticOperation) { op.Width.Target = op.Width.Old },
			reason: "equals the old one",
		},
		{
			name:   "a record with no state is refused",
			mutate: func(op *elasticOperation) { op.State = "" },
			reason: "carries no state",
		},
		{
			name:   "a captured worker with no identity is refused",
			mutate: func(op *elasticOperation) { op.Workers = []elasticCapturedIdentity{{Name: "ghost"}} },
			reason: "has no identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newElasticOperationStore(elasticTestClient())
			op := elasticTestOperation(elasticWidth{Old: 2, Target: 4}, elasticWorkerOne)
			tc.mutate(op)

			stored, err := store.Create(context.Background(), op)
			require.Error(t, err)
			assert.Nil(t, stored, "nothing is returned that could be acted on")
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

// TestFactsAboutAnotherDeploymentDecideNothing is the isolation rule, in one place.
func TestFactsAboutAnotherDeploymentDecideNothing(t *testing.T) {
	op := elasticTestOperation(
		elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
	facts := elasticCompleteFacts(4)
	facts.DeploymentUID = types.UID("another-deployment")

	decision := decideElasticOperation(op, facts)
	assert.Equal(t, elasticActionHold, decision.Action, "reason=%s", decision.Reason)
	assert.Contains(t, decision.Reason, "another-deployment")
}

// elasticTestClient is a fake API server: the same client the store would use in a cluster, with
// the objects the record needs to exist.
func elasticTestClient(objs ...ctrlcli.Object) ctrlcli.Client {
	md := &workercore.ModelDeployment{ObjectMeta: metav1.ObjectMeta{
		Name: elasticTestDeployNm, Namespace: elasticTestNamespace, UID: elasticTestDeployUID,
	}}
	all := append([]ctrlcli.Object{md}, objs...)

	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(all...).
		WithInterceptorFuncs(interceptor.Funcs{}).
		Build()
}

// ownerMutations are the ways a stored record's controlling owner can stop being the deployment the
// caller is asking about. They are data: a row states which field a writer changed, and the shared
// loop below applies it.
var ownerMutations = []struct {
	name  string
	field string
	value string
}{
	{name: "the owner is removed"},
	{name: "the owner is no longer the controller", field: "controller"},
	{name: "the owner names another kind", field: "kind", value: "Pod"},
	{name: "the owner names another API version", field: "apiVersion", value: "v1"},
	{name: "the owner names another deployment", field: "name", value: "another-deployment"},
	{name: "the owner carries another UID", field: "uid", value: "another-uid"},
}

// mutateControllingOwner applies one row to a stored record's owner references.
func mutateControllingOwner(owner *metav1.OwnerReference, field, value string) {
	if field == "" {
		return
	}
	switch field {
	case "controller":
		owner.Controller = new(bool)
	case "kind":
		owner.Kind = value
	case "apiVersion":
		owner.APIVersion = value
	case "name":
		owner.Name = value
	case "uid":
		owner.UID = types.UID(value)
	}
}

// TestTheStoreRefusesARecordThatIsNoLongerThisDeployments pins the two identity rules the store
// exists to keep, against the real store rather than a helper.
//
// A record that changed hands since it was written is another deployment's record, so reading it
// is reading someone else's decision and writing it replaces their bytes. A fresh resourceVersion
// is not permission: it says the object moved, not that it moved to the same owner.
func TestTheStoreRefusesARecordThatIsNoLongerThisDeployments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		reject   bool
	}{
		{name: "the expected name matches", expected: elasticTestDeployNm},
		{name: "a different expected name is refused", expected: "another-deployment", reject: true},
		{name: "a missing expected name is refused", expected: "", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newElasticOperationStore(elasticTestClient())
			_, err := store.Create(context.Background(),
				elasticTestOperation(elasticWidth{Old: 2, Target: 4}, elasticWorkerOne))
			require.NoError(t, err)

			got, err := store.Read(
				context.Background(), elasticTestNamespace, tc.expected, elasticTestDeployUID)

			if tc.reject {
				require.Error(t, err)
				assert.Nil(t, got, "a refused read returns no record at all")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, elasticTestDeployNm, got.Name)
			assert.Equal(t, elasticTestDeployUID, got.DeploymentUID)
		})
	}

	for _, tc := range ownerMutations {
		t.Run("an update after "+tc.name+" is refused", func(t *testing.T) {
			ctx := context.Background()
			client := elasticTestClient()
			store := newElasticOperationStore(client)
			op, err := store.Create(ctx, elasticTestOperation(elasticWidth{Old: 2, Target: 4}, elasticWorkerOne))
			require.NoError(t, err)

			key := ctrlcli.ObjectKey{
				Namespace: elasticTestNamespace, Name: elasticOperationRecordName(elasticTestDeployUID),
			}
			stored := new(corev1.ConfigMap)
			require.NoError(t, client.Get(ctx, key, stored))
			owner := metav1.GetControllerOf(stored)
			require.NotNil(t, owner, "the record starts owned by the deployment")
			if tc.field == "" {
				stored.OwnerReferences = nil
			} else {
				mutateControllingOwner(&stored.OwnerReferences[0], tc.field, tc.value)
			}
			require.NoError(t, client.Update(ctx, stored))
			require.NoError(t, client.Get(ctx, key, stored))

			op.ResourceVersion = stored.ResourceVersion
			before := stored.DeepCopy()
			callerVersion := op.ResourceVersion
			op.CommandSent = true
			op.State = elasticStateCommandSent

			err = store.Update(ctx, op)

			require.Error(t, err, "a record owned by anyone else is not this deployment's to write")
			after := new(corev1.ConfigMap)
			require.NoError(t, client.Get(ctx, key, after))
			assert.Equal(t, before.Data, after.Data, "a refused write must not change the stored bytes")
			assert.Equal(t, before.OwnerReferences, after.OwnerReferences)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
				"a refused write must not bump the object")
			assert.Equal(t, callerVersion, op.ResourceVersion,
				"a refused write must not leave the caller a version that is now someone else's")
		})
	}
}

// TestAWriteAgainstTheRightOwnerStillWrites is the positive the refusals above sit next to: the
// rules must not refuse everything.
func TestAWriteAgainstTheRightOwnerStillWrites(t *testing.T) {
	ctx := context.Background()
	client := elasticTestClient()
	store := newElasticOperationStore(client)
	op, err := store.Create(ctx, elasticTestOperation(elasticWidth{Old: 2, Target: 4}, elasticWorkerOne))
	require.NoError(t, err)

	key := ctrlcli.ObjectKey{
		Namespace: elasticTestNamespace, Name: elasticOperationRecordName(elasticTestDeployUID),
	}
	before := new(corev1.ConfigMap)
	require.NoError(t, client.Get(ctx, key, before))
	op.CommandSent = true
	op.State = elasticStateCommandSent

	require.NoError(t, store.Update(ctx, op))

	after := new(corev1.ConfigMap)
	require.NoError(t, client.Get(ctx, key, after))
	assert.NotEqual(t, before.Data, after.Data, "the accepted write changed the record")
	read, err := store.Read(ctx, elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
	require.NoError(t, err)
	assert.True(t, read.CommandSent)
	assert.Equal(t, elasticStateCommandSent, read.State)
}

// elasticRecoveryRecords builds the two records one interrupted-upscale recovery swaps between:
// the stale sent upward command the pass decided against, and the release-only record recovery
// retires it into at the proven old width.
func elasticRecoveryRecords() (current, recovered *elasticOperation) {
	current = elasticTestOperation(
		elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
	current.State = elasticStateCommandSent
	current.CommandSent = true

	recovered = elasticTestOperation(
		elasticWidth{Old: 4, Target: 2}, elasticWorkerTwo, elasticWorkerThree)
	recovered.State = elasticStateReleased
	recovered.CommandSent = true
	recovered.CommandIntent = "recovered at already observed native width 2"
	recovered.Release = &modelDeploymentRetirementRelease{
		ModelDeploymentUID: elasticTestDeployUID, ObservedGeneration: 1, RoleName: "server",
	}

	return current, recovered
}

// TestTheRecoverySwapIsOneAtomicWrite covers the store's one envelope-moving transition: the
// stale interrupted record is replaced by its recovery record in a single write, so no crash and
// no failed write can leave the deployment with no record between the two, and every refusal
// leaves the stale record exactly as the pass read it.
func TestTheRecoverySwapIsOneAtomicWrite(t *testing.T) {
	recordKey := ctrlcli.ObjectKey{
		Namespace: elasticTestNamespace, Name: elasticOperationRecordName(elasticTestDeployUID),
	}

	t.Run("the recovery record replaces the stale one in place", func(t *testing.T) {
		ctx := context.Background()
		client := elasticTestClient()
		store := newElasticOperationStore(client)
		current, err := store.Create(ctx, elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree))
		require.NoError(t, err)
		_, recovered := elasticRecoveryRecords()
		before := new(corev1.ConfigMap)
		require.NoError(t, client.Get(ctx, recordKey, before))

		require.NoError(t, store.RecoverInterrupted(ctx, current, recovered))

		after := new(corev1.ConfigMap)
		require.NoError(t, client.Get(ctx, recordKey, after))
		assert.Equal(t, before.Name, after.Name, "the swap reuses the stale record's object")
		assert.NotEqual(t, before.Data, after.Data, "the swap wrote the recovery record")
		read, err := store.Read(ctx, elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
		require.NoError(t, err)
		assert.Equal(t, elasticWidth{Old: 4, Target: 2}, read.Width)
		assert.Equal(t, elasticStateReleased, read.State)
		assert.Equal(t, "recovered at already observed native width 2", read.CommandIntent)
		assert.Len(t, read.Workers, 2)
		assert.NotNil(t, read.Release)
		assert.Equal(t, after.ResourceVersion, recovered.ResourceVersion,
			"the swap leaves the caller the version it can retry against")
	})

	t.Run("a failed swap write leaves the stale record in place", func(t *testing.T) {
		ctx := context.Background()
		client := ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object,
					opts ...ctrlcli.UpdateOption,
				) error {
					if _, ok := obj.(*corev1.ConfigMap); ok {
						return fmt.Errorf("the record write is unavailable")
					}
					return cli.Update(ctx, obj, opts...)
				},
			}).
			Build()
		store := newElasticOperationStore(client)
		toStore, recovered := elasticRecoveryRecords()
		current, err := store.Create(ctx, toStore)
		require.NoError(t, err)
		staleVersion := current.ResourceVersion

		err = store.RecoverInterrupted(ctx, current, recovered)
		require.Error(t, err)

		read, err := store.Read(ctx, elasticTestNamespace, elasticTestDeployNm, elasticTestDeployUID)
		require.NoError(t, err, "the stale record survives a failed swap")
		assert.Equal(t, elasticStateCommandSent, read.State)
		assert.Equal(t, elasticWidth{Old: 2, Target: 4}, read.Width)
		assert.Equal(t, "set the collective width", read.CommandIntent)
		assert.Equal(t, staleVersion, read.ResourceVersion, "the stale record moved not at all")
		assert.Empty(t, recovered.ResourceVersion,
			"a failed write leaves the recovery record nothing to retry against")
	})

	t.Run("a stale record that moved underneath is refused", func(t *testing.T) {
		ctx := context.Background()
		client := elasticTestClient()
		store := newElasticOperationStore(client)
		current, err := store.Create(ctx, elasticTestOperation(
			elasticWidth{Old: 2, Target: 4}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree))
		require.NoError(t, err)

		// The stored record is rewritten to a different operation under the same name, and the
		// caller's version is refreshed to match, so the version check alone would pass and the
		// envelope comparison is what stands between the swap and a record it never decided on.
		stored := new(corev1.ConfigMap)
		require.NoError(t, client.Get(ctx, recordKey, stored))
		moved := elasticTestOperation(
			elasticWidth{Old: 4, Target: 6}, elasticWorkerOne, elasticWorkerTwo, elasticWorkerThree)
		moved.State = elasticStateCommandSent
		moved.CommandSent = true
		raw, err := json.Marshal(moved)
		require.NoError(t, err)
		stored.Data = map[string]string{elasticOperationDataKey: string(raw)}
		require.NoError(t, client.Update(ctx, stored))
		require.NoError(t, client.Get(ctx, recordKey, stored))
		before := stored.DeepCopy()
		current.ResourceVersion = stored.ResourceVersion

		_, recovered := elasticRecoveryRecords()
		err = store.RecoverInterrupted(ctx, current, recovered)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "retargeted to another width")

		after := new(corev1.ConfigMap)
		require.NoError(t, client.Get(ctx, recordKey, after))
		assert.Equal(t, before.Data, after.Data, "a refused swap must not change the stored bytes")
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
			"a refused swap must not bump the object")
	})
}
