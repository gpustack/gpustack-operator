// The retirement reservation: the protocol table, the interception table, and the drain seam's
// own rules.
//
// The two tables are the reason this file is shaped the way it is. THE CANNOT-BYPASS TABLE asks a
// single question six times, once per path in the controller that can delete a replica, and the
// NO-RESERVATION TABLE asks the same six questions of a pass with nothing reserved. Between them
// they pin the two properties this feature lives or dies on: a reserved replica cannot be taken by
// anything except the protocol, and adding the protocol changed nothing for a deployment that has
// none. Both are asked as executed behavior rather than asserted in prose, because a guard that is
// only ever argued about is a guard that is one refactor away from being wrong.
//
// The remaining cases are one behavior each. The drain seam's rules are tested against a SCRIPTED
// reader rather than a real engine, so that "two consecutive complete zeros" is an assertion about
// the protocol and not about a fake: the fake is handed one answer at a time and the test drives
// the sequence.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// scriptedDrainReader is the seam's test double: one queued answer per read, keyed by member UID.
//
// IT ANSWERS ONE READ AT A TIME ON PURPOSE. The protocol's rule is that two consecutive complete
// reads must both be idle, and a fake that returned a single standing answer could not express a
// sequence at all -- the test would then be asserting that the fake said what it said. Queuing one
// answer per read makes the sequence the test's to write.
type scriptedDrainReader struct {
	answers map[types.UID][]modelDeploymentDrainAnswer
	errs    map[types.UID][]error
	reads   map[types.UID]int
}

func (s *scriptedDrainReader) Drain(
	_ context.Context, target modelDeploymentDrainTarget,
) (modelDeploymentDrainAnswer, error) {
	uid := target.PodUID
	if s.reads == nil {
		s.reads = map[types.UID]int{}
	}
	read := s.reads[uid]
	s.reads[uid] = read + 1

	if queue := s.errs[uid]; read < len(queue) && queue[read] != nil {
		return modelDeploymentDrainAnswer{}, queue[read]
	}
	if queue := s.answers[uid]; read < len(queue) {
		return queue[read], nil
	}

	// Past the end of the script the member keeps reporting the last scripted answer, so a test
	// that under-scripts sees a hold rather than a panic.
	if queue := s.answers[uid]; len(queue) > 0 {
		return queue[len(queue)-1], nil
	}

	return modelDeploymentDrainAnswer{State: modelDeploymentDrainUnknown, Reason: "not scripted"}, nil
}

// idleDrain is a complete, envelope-matched zero for the engine's whole expected series.
//
// IT CARRIES EVERY GAUGE THE ENGINE NAMES, and that is the point of building it from the tree's
// own map rather than from a literal: a zero that omitted one of the two vLLM gauges is exactly
// the read the protocol refuses, so a hand-written fixture would have been testing the refusal
// while claiming to test the drain.
func idleDrain() modelDeploymentDrainAnswer {
	series := map[string]float64{}
	for _, metric := range modelDeploymentInFlightMetrics[workercore.ModelDeploymentEngineVLLM] {
		series[metric] = 0
	}

	return modelDeploymentDrainAnswer{
		State: modelDeploymentDrainIdle, Complete: true, Series: series,
	}
}

// busyDrain is a complete read with work in it.
func busyDrain() modelDeploymentDrainAnswer {
	answer := idleDrain()
	for metric := range answer.Series {
		answer.Series[metric] = 3
	}
	answer.State = modelDeploymentDrainBusy

	return answer
}

// retirementDeployment is a two-replica deployment with a fixed clock, so a phase's start and its
// expiry are moments the test chooses rather than moments it waits for.
// newRetirementInterceptedClient wraps a client so a test can observe or fail one kind of write,
// which is how the delete precondition and the crash window are pinned rather than assumed.
func newRetirementInterceptedClient(
	funcs ctrlinterceptor.Funcs, objs ...ctrlcli.Object,
) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
}

func retirementDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	return newRenderDeployment(append([]func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) { md.Spec.KVCache = nil },
	}, mutate...)...)
}

// retirementReservation builds the persisted operation the FSM re-enters at.
func retirementReservation(
	state workercore.ModelDeploymentRetirementState, role string, ordinal int32,
	uids []string, mutate ...func(*workercore.ModelDeploymentRetirementStatus),
) *workercore.ModelDeploymentRetirementStatus {
	started := time.Now().Add(-time.Second)
	reservation := &workercore.ModelDeploymentRetirementStatus{
		RoleName:           role,
		ReplicaOrdinal:     ordinal,
		ObservedGeneration: 1,
		TargetMemberUIDs:   uids,
		TargetWorkloadUID:  "wl-target",
		State:              state,
		StartedAt:          meta.NewTime(started),
		Deadline:           meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget)),
		PhaseStartedAt:     meta.NewTime(started),
	}
	for _, m := range mutate {
		m(reservation)
	}

	return reservation
}

// holdReconciler builds a reconciler whose retirement reader is the scripted one and whose clock is
// the given moment.
func holdReconciler(cli ctrlcli.Client, reader modelDeploymentDrainReader, now time.Time) *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{
		Client:      cli,
		APIReader:   cli,
		Recorder:    ctrlrecord.NewFakeRecorder(64),
		drainReader: reader,
		clock:       func() time.Time { return now },
	}
}

// reserve writes a reservation onto a deployment's status and returns the deployment to seed.
func reserve(md *workercore.ModelDeployment, reservation *workercore.ModelDeploymentRetirementStatus) *workercore.ModelDeployment {
	md.Status.Retirement = reservation

	return md
}

// TestRetirementPhaseEndsAtTheEarlierOfOverallAndPhase pins the frozen boundary rule.
//
// A per-phase budget longer than what remains of the operation must not extend the operation, and
// an operation whose overall budget has passed must not be handed a fresh phase by a generous
// per-phase number. The two are separate failures and the table carries both.
func TestRetirementPhaseEndsAtTheEarlierOfOverallAndPhase(t *testing.T) {
	testCases := []struct {
		name           string
		phaseBudget    time.Duration
		phaseAgo       time.Duration
		overallRemains time.Duration
		wantExhausted  bool
	}{
		{
			name:           "a phase inside both budgets has not run out",
			phaseBudget:    modelDeploymentRetirementDrainBudget,
			phaseAgo:       10 * time.Second,
			overallRemains: 200 * time.Second,
			wantExhausted:  false,
		},
		{
			name:           "the phase budget is what ends a phase inside the overall budget",
			phaseBudget:    modelDeploymentRetirementDrainBudget,
			phaseAgo:       modelDeploymentRetirementDrainBudget + time.Second,
			overallRemains: 200 * time.Second,
			wantExhausted:  true,
		},
		{
			name:           "the overall budget ends a phase its own budget would have allowed",
			phaseBudget:    modelDeploymentRetirementDrainBudget,
			phaseAgo:       10 * time.Second,
			overallRemains: -5 * time.Second,
			wantExhausted:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ONE CONTROLLED MOMENT FOR BOTH. Reading the wall clock twice would let the two
			// readings differ, so a case whose whole subject is the exact boundary could pass or
			// fail on how long the fixture took to build rather than on the arithmetic.
			now := time.Now()
			started := now.Add(-tc.phaseAgo)
			reservation := retirementReservation(
				workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"u"},
				func(r *workercore.ModelDeploymentRetirementStatus) {
					r.PhaseStartedAt = meta.NewTime(started)
					r.Deadline = meta.NewTime(now.Add(tc.overallRemains))
				},
			)

			assert.Equal(t, tc.wantExhausted,
				modelDeploymentRetirementBudgetSpent(reservation, tc.phaseBudget, now))
		})
	}
}

// TestProductionDrainReaderHoldsUntilTheTransportLands pins the seam's unwired behavior.
//
// A reader that answered Idle while nothing is wired would turn "nobody is watching" into "the
// queue is empty", which is the exact inversion the seam exists to make impossible. The
// production wiring therefore refuses, and the protocol that reads it holds.
func TestProductionDrainReaderHoldsUntilTheTransportLands(t *testing.T) {
	r := new(ModelDeploymentReconciler)

	answer, err := r.modelDeploymentDrainReaderOf().Drain(context.Background(), modelDeploymentDrainTarget{
		PodUID: "u", Engine: workercore.ModelDeploymentEngineVLLM,
	})
	require.NoError(t, err, "an unwired seam is a known state of the tree, not a transport failure")

	assert.Equal(t, modelDeploymentDrainUnsupported, answer.State)
	assert.NotEmpty(t, answer.Reason, "the refusal names the engine, so a status read says which one")
}

// TestDrainNeverCountsAnIncompleteReadAsIdle is the seam's completeness rule, and it is the case a
// naive reader fails: it scraped two of the four SGLang gauges, summed them, and reported a zero.
func TestDrainNeverCountsAnIncompleteReadAsIdle(t *testing.T) {
	md := retirementDeployment()
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}

	testCases := []struct {
		name       string
		answer     modelDeploymentDrainAnswer
		wantDrain  bool
		wantReason string
	}{
		{
			name: "a complete envelope-matched zero is idle",
			answer: modelDeploymentDrainAnswer{
				State: modelDeploymentDrainIdle, Complete: true,
				Series: map[string]float64{
					"vllm:num_requests_running": 0,
					"vllm:num_requests_waiting": 0,
				},
			},
			wantDrain: true,
		},
		{
			name: "an incomplete envelope is not idle whatever the numbers say",
			answer: modelDeploymentDrainAnswer{
				State: modelDeploymentDrainIdle, Complete: false,
				Series: map[string]float64{"vllm:num_requests_running": 0},
			},
			wantReason: "incomplete envelope",
		},
		{
			name: "a reader claiming completeness while missing a series is still not idle",
			answer: modelDeploymentDrainAnswer{
				State: modelDeploymentDrainIdle, Complete: true,
				Series: map[string]float64{"vllm:num_requests_running": 0},
			},
			// A DIFFERENT REASON FROM THE INCOMPLETE ENVELOPE ABOVE, because the two are different
			// faults: a reader that says the envelope was incomplete is telling us what it knows,
			// while a reader that claims completeness and is missing a gauge is not.
			wantReason: "vllm:num_requests_waiting",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))
			// The member is seeded, because the release capture reads the target back from the
			// server before it will authorize a delete. A pod handed to the planner without
			// existing on the server is a target the capture cannot observe.
			md = withResourceVersion(md)
			seeded := make([]ctrlcli.Object, 0, 1+len(pods))
			seeded = append(seeded, md.DeepCopy())
			for i := range pods {
				seeded = append(seeded, pods[i].DeepCopy())
			}
			cli := newModelDeploymentClient(seeded...)
			reader := &scriptedDrainReader{
				answers: map[types.UID][]modelDeploymentDrainAnswer{
					"member-1": {tc.answer, tc.answer},
				},
			}
			r := holdReconciler(cli, reader, time.Now())

			plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

			if tc.wantDrain {
				assert.Equal(t, workercore.ModelDeploymentRetirementStateDeleting,
					plan.Reservation.State, "two complete zero reads commit the delete")
				return
			}
			assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining,
				plan.Reservation.State, "an incomplete read holds and never deletes")
			assert.Contains(t, plan.Reservation.Reason, tc.wantReason)
		})
	}
}

// withResourceVersion gives a fixture deployment the resource version a real server hands out.
//
// The release record is written with a real optimistic lock, and a lock compares against the
// version it read. Seeding an object while leaving the caller's own copy without one describes a
// state no server produces, so the fixture is corrected here rather than by weakening the lock.
func withResourceVersion(md *workercore.ModelDeployment) *workercore.ModelDeployment {
	if md.ResourceVersion == "" {
		md.ResourceVersion = "1"
	}

	return md
}

// TestDrainNeedsTwoConsecutiveCompleteZeros pins the observation rule rather than the sample.
//
// One read of a queue gauge that catches the moment between two requests says zero while the
// replica is serving. A protocol that deleted on that reading would cut live traffic, so the second
// read exists, and it must be a second READ rather than a second pass.
func TestDrainNeedsTwoConsecutiveCompleteZeros(t *testing.T) {
	testCases := []struct {
		name      string
		answers   []modelDeploymentDrainAnswer
		wantDrain bool
	}{
		{
			name:      "two idle reads drain",
			answers:   []modelDeploymentDrainAnswer{idleDrain(), idleDrain()},
			wantDrain: true,
		},
		{
			name:    "one idle read followed by a busy one holds",
			answers: []modelDeploymentDrainAnswer{idleDrain(), busyDrain()},
		},
		{
			name:    "a busy first read holds even though the second is idle",
			answers: []modelDeploymentDrainAnswer{busyDrain(), idleDrain()},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment()
			pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))
			// The member is seeded, because the release capture reads the target back from the
			// server before it will authorize a delete. A pod handed to the planner without
			// existing on the server is a target the capture cannot observe.
			md = withResourceVersion(md)
			seeded := make([]ctrlcli.Object, 0, 1+len(pods))
			seeded = append(seeded, md.DeepCopy())
			for i := range pods {
				seeded = append(seeded, pods[i].DeepCopy())
			}
			cli := newModelDeploymentClient(seeded...)
			reader := &scriptedDrainReader{
				answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": tc.answers},
			}
			r := holdReconciler(cli, reader, time.Now())

			plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

			if tc.wantDrain {
				assert.Equal(t, workercore.ModelDeploymentRetirementStateDeleting,
					plan.Reservation.State)

				return
			}
			assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining,
				plan.Reservation.State)
			assert.True(t, plan.Hold, "a held member is still held")
		})
	}
}

// TestDrainHoldsOnAnythingThatIsNotAnObservation is the remaining seam answers, each a distinct
// refusal: an unbindable collection is not a zero, and neither is an absent one.
func TestDrainHoldsOnAnythingThatIsNotAnObservation(t *testing.T) {
	testCases := []struct {
		name  string
		queue []modelDeploymentDrainAnswer
		errs  []error
	}{
		{
			name: "an unbindable collection is not a zero",
			errs: []error{errors.New("the scrape target does not resolve")},
		},
		{
			name:  "an unobserved member is not a zero",
			queue: []modelDeploymentDrainAnswer{{State: modelDeploymentDrainUnknown, Reason: "no scrape target"}},
		},
		{
			name: "an engine with no measurement is refused rather than summed",
			queue: []modelDeploymentDrainAnswer{{
				State: modelDeploymentDrainUnsupported, Reason: "no collector",
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment()
			pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))
			cli := newModelDeploymentClient(md.DeepCopy())
			reader := &scriptedDrainReader{
				answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": tc.queue},
				errs:    map[types.UID][]error{"member-1": tc.errs},
			}
			r := holdReconciler(cli, reader, time.Now())

			plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

			assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining,
				plan.Reservation.State, "a failed observation holds and never deletes")
			assert.True(t, plan.Hold)
		})
	}
}

// TestDisaggregatedRetirementIsRefusedRatherThanMeasuredByThePrefiller pins the P/D case.
//
// A decoder waiting to receive KV is in the PREFILLER's transfer queue, and the prefiller reports
// that work while the decoder that will serve it reports none. Reading the prefill gauges as the
// decoder-side release would delete a decode cluster in the middle of a prefill.
func TestDisaggregatedRetirementIsRefusedRatherThanMeasuredByThePrefiller(t *testing.T) {
	// The shape is declared the way the tree reads it: an external load-balance argument on the
	// role, which is what makes the group disaggregated rather than merely multi-rank.
	md := retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-external-lb"}
	})
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))
	cli := newModelDeploymentClient(md.DeepCopy())
	reader := &scriptedDrainReader{
		// Even a perfect reading cannot retire this shape, which is what the case is about.
		answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": {idleDrain(), idleDrain()}},
	}
	r := holdReconciler(cli, reader, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining, plan.Reservation.State)
	assert.Contains(t, plan.Reservation.Reason, "decoder-side release")
}

// TestBudgetExpiryAbortsAndRetains is the abort contract, and the reason it exists in the shape it
// does: the operation stops with every member, the Workload and the capacity still standing.
func TestBudgetExpiryAbortsAndRetains(t *testing.T) {
	md := retirementDeployment()
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
	exhausted := retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"},
		func(r *workercore.ModelDeploymentRetirementStatus) {
			r.PhaseStartedAt = meta.NewTime(time.Now().Add(-modelDeploymentRetirementDrainBudget))
		})
	md = reserve(md, exhausted)
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": {idleDrain(), idleDrain()}},
	}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State)
	assert.Empty(t, plan.Deletes, "an abort before deletion issues no delete")
	assert.True(t, plan.Hold, "the retained replica stays held, or it is not retained")
	assert.Contains(t, plan.Reservation.Reason, "retained")
	assert.False(t, plan.InFlight, "an abort is terminal until new intent, not a requeue loop")
}

// TestAnAbortedReservationIsNotRetriedUnchanged pins the difference between an abort and a step
// that keeps trying.
//
// A replica that cannot drain would otherwise be deleted once per reconcile: the FSM would re-enter
// the step that aborted, see the budget spent, and abort again -- forever, with the status flapping
// rather than the deployment progressing.
func TestAnAbortedReservationIsNotRetriedUnchanged(t *testing.T) {
	// THE ORDINAL IS NOT RE-DECLARED, so adoption does not run first. A healthy group at an ordinal
	// the spec names again is canceled before the state machine is reached, which would leave this
	// case asserting the adoption rule rather than the abort rule it is about.
	md := retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	})
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"},
		func(r *workercore.ModelDeploymentRetirementStatus) { r.Reason = "the drain budget expired" },
	))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": {idleDrain(), idleDrain()}},
	}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State)
	assert.Empty(t, plan.Deletes, "unchanged intent never re-enters the step that aborted")
	assert.False(t, plan.InFlight,
		"an abort asks for no requeue: the protocol is not running, and a pass that kept asking "+
			"to be re-entered would be the delete-once-per-reconcile the abort exists to prevent")
}

// TestRestartResumesFromThePersistedState pins that a restart re-enters rather than re-decides.
//
// The reservation is the protocol's progress. A controller that restarted it from the beginning
// would spend the whole operation's budget again on a replica that was already withdrawn, and a
// deployment that restarts often would never finish at all.
func TestRestartResumesFromThePersistedState(t *testing.T) {
	md := retirementRouterBacked(retirementDeployment())
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
	for i := range pods {
		pods[i].Status.Phase = core.PodRunning
		pods[i].Status.PodIP = "10.0.2.1"
	}
	// The operation enters at Disqualified, one step before the withdrawal answer is read, so the
	// pass has to resume the transition the previous pass left rather than the one after it. Entering
	// at Withdrawing would land directly in the drain step and never touch the resumed fields.
	resumed := retirementReservation(
		workercore.ModelDeploymentRetirementStateDisqualified, "server", 1, []string{"member-1"})
	md = reserve(md, resumed)
	// The pods are seeded because the router view binds each worker's address to a live Pod; with
	// nothing on the server the view is refused as indeterminate and every row asserts a hold.
	seeded := make([]ctrlcli.Object, 0, 4+len(pods))
	router, routerRS, routerDeployment := ownedRouterFixture(md)
	seeded = append(seeded, md.DeepCopy(), router, routerRS, routerDeployment)
	for i := range pods {
		seeded = append(seeded, pods[i].DeepCopy())
	}
	cli := newModelDeploymentClient(seeded...)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), pods)

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	// The pass re-enters at Withdrawing. The declared router reports no member of the target still
	// served, so the withdrawal is confirmed and the operation advances -- and what matters is that
	// it advanced FROM there rather than re-running Disqualified and re-spending that phase's budget.
	assert.Equal(t, workercore.ModelDeploymentRetirementStateWithdrawing, plan.Reservation.State,
		"the pass re-entered at the persisted state and advanced exactly the one step it was on, "+
			"rather than re-deciding the operation from Admitted")
	assert.Equal(t, resumed.StartedAt, plan.Reservation.StartedAt,
		"the operation's own start survives the restart, which is what makes this a resume: a "+
			"pass that re-decided would have moved it and spent the whole budget again")
	assert.Equal(t, resumed.PhaseStartedAt, plan.Reservation.PhaseStartedAt,
		"the withdrawal is ONE phase in two steps, so the move out of Disqualified does not take "+
			"a fresh budget and the phase keeps the start Disqualified gave it")
	assert.Equal(t, resumed.LastConsumedRetryToken, plan.Reservation.LastConsumedRetryToken,
		"everything the operation has already consumed is still consumed after the restart")
}

// TestExternalTerminationIsRecorded is the case the abort contract is careful about.
//
// A target that a preemption, a node failure or a hand removed cannot be retained, so reporting the
// clean abort a budget expiry earns would be claiming three retentions that did not happen.
func TestExternalTerminationIsRecorded(t *testing.T) {
	md := retirementDeployment()
	// The UIDs the reservation holds name nothing in this list: the target is gone.
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateWithdrawing, "server", 1, []string{"vanished"}))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, nil)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State)
	assert.Contains(t, plan.Reservation.Reason, "terminated outside this operator")
	assert.NotContains(t, plan.Reservation.Reason, "capacity are retained",
		"the clean abort's wording claims three retentions that did not happen, so it is not used")
	assert.False(t, plan.Hold, "a target that resolves to nothing has nothing left to hold")
}

// TestAReDeclaredHeldGroupIsAdoptedAndCancelled pins the duplicate rule.
//
// A scale-up that names the ordinal again while the operation is aborted means the replica is
// wanted after all. Canceling is the answer; refusing would deadlock the create gate, and running
// the protocol anyway would delete a replica the spec had just asked for.
func TestAReDeclaredHeldGroupIsAdoptedAndCancelled(t *testing.T) {
	testCases := []struct {
		name    string
		ready   *bool
		want    bool
		because string
	}{
		{
			name:    "a healthy group at a re-declared ordinal is adopted",
			ready:   healthBool(true),
			want:    true,
			because: "the spec wants the replica back and the group qualifies",
		},
		{
			name:    "a group that does not qualify is not adopted",
			ready:   healthBool(false),
			want:    false,
			because: "a broken group is what the operation was aborting",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment()
			pods := []core.Pod{healthPod("server", 1, 0, tc.ready, "member-1")}
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"}))
			cli := newModelDeploymentClient(md.DeepCopy())
			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

			plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

			assert.Equal(t, tc.want, plan.Clear, tc.because)
			if !tc.want {
				assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted,
					plan.Reservation.State, "an operation that is not adopted stays where it is")
			}
		})
	}
}

// TestANewUIDAtTheOrdinalIsExternalTerminationNotAdoption is the half of adoption that must not
// happen.
//
// A replacement at the same ordinal is a DIFFERENT group. The reservation's targets no longer
// resolve, and absorbing the replacement would cancel a reservation about replicas that are still
// standing.
func TestANewUIDAtTheOrdinalIsExternalTerminationNotAdoption(t *testing.T) {
	md := retirementDeployment()
	// The live Pod is at the right ordinal and healthy, but it is not the one reserved.
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "replacement")}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"reserved-member"}))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	assert.False(t, plan.Clear, "a different UID at the ordinal is not the reserved group")
	assert.Contains(t, plan.Reservation.Reason, "terminated outside this operator")
}

// TestRetryDirectiveOrdering pins the crash contract, which is the whole point of the sequence.
//
// A token equal to the one already consumed is a replay and costs nothing. A crash between the
// status write and the annotation clear leaves the token consumed with the annotation still
// present, and the next pass must clear it without consuming it twice.
func TestRetryDirectiveOrdering(t *testing.T) {
	t.Run("a replayed token is a no-op that only clears the annotation", func(t *testing.T) {
		md := retirementDeployment(func(md *workercore.ModelDeployment) {
			md.Annotations = map[string]string{
				modelDeploymentRetirementRetryAnnotation: "server:1:token-a",
			}
		})
		cli := newModelDeploymentClient(reserve(md.DeepCopy(), retirementReservation(
			workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"},
			func(r *workercore.ModelDeploymentRetirementStatus) {
				r.LastConsumedRetryToken = "token-a"
			})))
		r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
		stored := getModelDeployment(t, cli)

		consumed := r.consumeModelDeploymentRetry(context.Background(), stored,
			&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement})
		assert.False(t, consumed, "a token equal to the one already consumed changes nothing")
		assert.NotContains(t, getModelDeployment(t, cli).Annotations,
			modelDeploymentRetirementRetryAnnotation,
			"the replay still clears the annotation, because that is the only thing left to do")
	})

	t.Run("a crash after the status write leaves the token consumed and the annotation present",
		func(t *testing.T) {
			md := retirementDeployment(func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					modelDeploymentRetirementRetryAnnotation: "server:1:token-a",
				}
			})
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"member-1"}))
			// The annotation clear is the write that fails, which is the crash window: the status
			// write has already reached the API server by the time it is reached. The failure is a
			// window rather than a broken client, so the pass after it runs against a working one.
			crashed := true
			cli := newRetirementInterceptedClient(ctrlinterceptor.Funcs{
				Patch: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object,
					patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
				) error {
					if crashed {
						return errors.New("the api server is unreachable")
					}

					return c.Patch(ctx, obj, patch, opts...)
				},
			}, md, newRenderInstanceType())
			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			stored := getModelDeployment(t, cli)

			require.True(t, r.consumeModelDeploymentRetry(context.Background(), stored,
				&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement}))

			after := getModelDeployment(t, cli)
			assert.Equal(t, "token-a", after.Status.Retirement.LastConsumedRetryToken,
				"the token is consumed: the write that made it so reached the server")
			assert.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted,
				after.Status.Retirement.State, "the transition is persisted with the token")
			assert.Contains(t, after.Annotations, modelDeploymentRetirementRetryAnnotation,
				"the annotation is still there, which is exactly the crash window")

			// The next pass takes the replay branch and clears it, consuming nothing twice.
			crashed = false
			r2 := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			consumed := r2.consumeModelDeploymentRetry(context.Background(), after,
				&modelDeploymentRetirementPlan{Reservation: after.Status.Retirement})
			assert.False(t, consumed, "a token already in the reservation is a replay")
			assert.NotContains(t, getModelDeployment(t, cli).Annotations,
				modelDeploymentRetirementRetryAnnotation)
			assert.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted,
				getModelDeployment(t, cli).Status.Retirement.State,
				"the replay changed nothing but the annotation")
		})
}

// TestRetryDirectiveRefusals is the three negatives, each a different refusal with a different
// resulting state. A malformed value and a value naming another replica are both LEFT IN PLACE:
// half-consuming a directive is worse than ignoring one.
func TestRetryDirectiveRefusals(t *testing.T) {
	testCases := []struct {
		name          string
		directive     string
		role          string
		ordinal       int32
		wantConsumed  bool
		wantRemaining bool
	}{
		{
			name:          "a value with a missing part is refused and left",
			directive:     "server:token-a",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:          "a two-part value whose ordinal parses is still refused",
			directive:     "server:1",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:          "a value with an empty token is refused and left",
			directive:     "server:1:",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:          "an ordinal that is not a number is refused and left",
			directive:     "server:one:token-a",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:          "a token naming another role is refused and left",
			directive:     "other:1:token-a",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:          "a token naming another ordinal is refused and left",
			directive:     "server:9:token-a",
			role:          "server",
			ordinal:       1,
			wantRemaining: true,
		},
		{
			name:         "a token naming this replica is consumed",
			directive:    "server:1:token-a",
			role:         "server",
			ordinal:      1,
			wantConsumed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment(func(md *workercore.ModelDeployment) {
				md.Annotations = map[string]string{
					modelDeploymentRetirementRetryAnnotation: tc.directive,
				}
			})
			cli := newModelDeploymentClient(reserve(md.DeepCopy(), retirementReservation(
				workercore.ModelDeploymentRetirementStateAborted, tc.role, tc.ordinal,
				[]string{"member-1"})))
			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			stored := getModelDeployment(t, cli)

			consumed := r.consumeModelDeploymentRetry(context.Background(), stored,
				&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement})
			assert.Equal(t, tc.wantConsumed, consumed)

			after := getModelDeployment(t, cli)
			if tc.wantRemaining {
				assert.Contains(t, after.Annotations, modelDeploymentRetirementRetryAnnotation,
					"a refused directive is left exactly as it was found")
				assert.Empty(t, after.Status.Retirement.LastConsumedRetryToken)
			}
		})
	}
}

// TestRetryIsIgnoredWhileTheOperationIsNotAborted pins that a directive never resumes a step that
// has not finished.
func TestRetryIsIgnoredWhileTheOperationIsNotAborted(t *testing.T) {
	md := retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Annotations = map[string]string{
			modelDeploymentRetirementRetryAnnotation: "server:1:token-a",
		}
	})
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	stored := getModelDeployment(t, cli)

	consumed := r.consumeModelDeploymentRetry(context.Background(), stored,
		&modelDeploymentRetirementPlan{Reservation: stored.Status.Retirement})

	assert.False(t, consumed, "there is nothing to retry until the operation aborts")
	assert.Contains(t, getModelDeployment(t, cli).Annotations,
		modelDeploymentRetirementRetryAnnotation)
}

// TestTheProtocolCompletesTheWholeProtocol drives the operation end to end, one pass per state.
//
// The value is in the SEQUENCE rather than in any single pass: each step is asserted to have been
// left behind before the next one begins, so a protocol that reached Deleting without ever having
// withdrawn would still fail this case rather than pass it.
func TestTheProtocolCompletesTheWholeProtocol(t *testing.T) {
	md := retirementRouterBacked(retirementDeployment())
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	target.Status.Phase = core.PodRunning
	target.Status.PodIP = "10.0.3.1"
	pods := []core.Pod{*target}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAdmitted, "server", 1, []string{"member-1"}))

	reader := &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{
			"member-1": {idleDrain(), idleDrain()},
		},
	}
	cli := newModelDeploymentClient(append(
		[]ctrlcli.Object{md, newRenderInstanceType(), target}, ownedRouterObjects(md)...)...)
	r := retirementRouterReconciler(cli, reader, time.Now(), pods)

	states := make([]workercore.ModelDeploymentRetirementState, 0, 8)
	for pass := 0; pass < 6; pass++ {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err)

		after := getModelDeployment(t, cli)
		if after.Status.Retirement == nil {
			states = append(states, workercore.ModelDeploymentRetirementStateCompleted)

			break
		}
		states = append(states, after.Status.Retirement.State)
	}

	assert.Equal(t, []workercore.ModelDeploymentRetirementState{
		// Admitted is not in the list because the rendered target never carried the eligibility
		// key: the whole-group predicate already refuses a reserved replica, so the first pass
		// finds it out of selection and moves on. The states below are the ones the protocol
		// actually spends its budget in, in order.
		workercore.ModelDeploymentRetirementStateDisqualified,
		workercore.ModelDeploymentRetirementStateWithdrawing,
		workercore.ModelDeploymentRetirementStateDraining,
		// The delete is committed here, and the Pod goes with it.
		workercore.ModelDeploymentRetirementStateDeleting,
		// The target is gone, so the operation settles and then completes and clears.
		workercore.ModelDeploymentRetirementStateSettling,
		workercore.ModelDeploymentRetirementStateCompleted,
	}, states, "the protocol visits every state in order and clears only at the end")

	assert.Nil(t, getModelDeployment(t, cli).Status.Retirement,
		"a completed operation is cleared rather than left as a tombstone that keeps holding")
	survivors := retirementReplicaNames(t, cli)
	assert.Len(t, survivors, 1, "the target is gone and exactly one replica of the two survives")
	assert.NotContains(t, survivors, "qwen-server-one", "the target is the replica that left")
}

// TestTheProtocolDeleteCarriesItsUIDPrecondition pins the precondition on the protocol's own
// delete, which is what makes the UID set load-bearing rather than advisory.
func TestTheProtocolDeleteCarriesItsUIDPrecondition(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"}))

	var preconditions []string
	cli := newRetirementInterceptedClient(
		ctrlinterceptor.Funcs{
			Delete: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object,
				opts ...ctrlcli.DeleteOption,
			) error {
				options := &ctrlcli.DeleteOptions{}
				options.ApplyOptions(opts)
				if pod, ok := obj.(*core.Pod); ok {
					preconditions = append(preconditions, string(pod.UID))
					if options.Preconditions == nil || options.Preconditions.UID == nil {
						t.Errorf("replica %s was deleted without a UID precondition", pod.Name)
					}
				}

				return c.Delete(ctx, obj, opts...)
			},
		}, md, newRenderInstanceType(), target)
	r := holdReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": {idleDrain(), idleDrain()}},
	}, time.Now())

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	assert.Contains(t, preconditions, "member-1",
		"the protocol deletes the UID it reserved, not whatever holds the name")
}

// TestAScaleDownRunsTheWholeProtocolFromNoReservation is the lifecycle the other pass-through
// cases split in two.
//
// Every other case here seeds a reservation, so each proves the protocol resumes correctly INTO a
// state it was handed. None of them proves the chain runs from nothing: the spec says one replica,
// two exist, no operation is recorded, and the first pass has to notice the removal, admit it, and
// delete nothing. That first step is the one the seeding hides. The replica set is carried in the
// same table because it is what the create gate looks at -- a retiring ordinal that refilled before
// its operation cleared would turn a retirement back into a rollout, and the last step is that the
// ordinal is free again once the operation has actually ended.
func TestAScaleDownRunsTheWholeProtocolFromNoReservation(t *testing.T) {
	testCases := []struct {
		wantState    workercore.ModelDeploymentRetirementState
		wantCleared  bool
		wantOrdinals []string
		why          string
	}{
		{
			wantState:    workercore.ModelDeploymentRetirementStateAdmitted,
			wantOrdinals: []string{"0", "1"},
			why:          "the first pass notices the removal, admits it, and deletes nothing",
		},
		{
			// The whole-group predicate already refuses a reserved replica, so the pass after the
			// admission finds the target out of selection rather than serving it. This is the state
			// every seeded case skips, and it is why the seeded whole-protocol test does not name it.
			wantState:    workercore.ModelDeploymentRetirementStateDisqualified,
			wantOrdinals: []string{"0", "1"},
			why:          "a reserved replica is out of selection, so the operation disqualifies itself",
		},
		{
			wantState:    workercore.ModelDeploymentRetirementStateWithdrawing,
			wantOrdinals: []string{"0", "1"},
			why:          "the operation stops selecting the target from the routers",
		},
		{
			wantState:    workercore.ModelDeploymentRetirementStateDraining,
			wantOrdinals: []string{"0", "1"},
			why:          "the operation reads the target's own in-flight gauges before deleting it",
		},
		{
			wantState:    workercore.ModelDeploymentRetirementStateDeleting,
			wantOrdinals: []string{"0"},
			why: "the delete is committed on this pass, so the target is gone from here on and " +
				"nothing refills the ordinal while the operation still has phases left to run",
		},
		{
			wantState:    workercore.ModelDeploymentRetirementStateSettling,
			wantOrdinals: []string{"0"},
			why:          "the target is gone, so the operation settles",
		},
		{
			// The last phase sets Completed and Clear in the same pass, so Completed is never a
			// state a reader of the status can observe -- what a reader sees is its absence. The
			// table says so rather than naming a state the API never carries.
			wantState:    workercore.ModelDeploymentRetirementStateCompleted,
			wantCleared:  true,
			wantOrdinals: []string{"0"},
			why: "the operation reaches Completed and clears in one pass, leaving the one replica " +
				"the spec asks for",
		},
	}

	md := retirementRouterBacked(retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	}))
	kept := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	target.Status.Phase = core.PodRunning
	target.Status.PodIP = "10.0.4.1"
	kept.Status.Phase = core.PodRunning
	kept.Status.PodIP = "10.0.4.2"
	pods := []core.Pod{*kept, *target}
	cli := newModelDeploymentClient(append(
		[]ctrlcli.Object{md, newRenderInstanceType(), kept, target}, ownedRouterObjects(md)...)...)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"member-1": {idleDrain(), idleDrain()}},
	}, time.Now(), pods)

	require.Nil(t, getModelDeployment(t, cli).Status.Retirement,
		"the deployment starts with no operation, which is the whole point of this case")

	for _, tc := range testCases {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err)

		after := getModelDeployment(t, cli)
		if tc.wantCleared {
			assert.Nil(t, after.Status.Retirement, tc.why)
		} else {
			require.NotNil(t, after.Status.Retirement, tc.why)
			assert.Equal(t, tc.wantState, after.Status.Retirement.State, tc.why)
		}
		assert.Equal(t, tc.wantOrdinals, replicaOrdinals(t, cli), tc.why)
	}

	assert.Nil(t, getModelDeployment(t, cli).Status.Retirement,
		"a completed operation is cleared rather than left as a tombstone that keeps holding the ordinal")

	// THE ORDINAL IS FREE ONLY NOW. Re-declaring the replica the operation retired is the check the
	// table cannot make while the operation is running, because a gate that never opened and a gate
	// that was merely closed both look the same from inside the operation.
	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, cli.Update(context.Background(), scaled))
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	assert.Equal(t, []string{"0", "1"}, replicaOrdinals(t, cli),
		"the retired ordinal is usable again once the operation has cleared")
}

// replicaOrdinals lists the ordinals a deployment currently occupies, which is what the create gate
// reads and what this test is about. Pod names are not: a replica the controller creates is named
// after a render hash, so a recreated ordinal comes back under a different name than the one it
// retired under.
func replicaOrdinals(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()
	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))

	ordinals := make([]string, 0, len(podList.Items))
	for _, pod := range replicaPods(t, cli) {
		ordinals = append(ordinals, pod.Labels[modelDeploymentReplicaOrdinalLabel])
	}
	slices.Sort(ordinals)

	return ordinals
}

// retirementReplicaNames lists the deployment's serving replicas, leaving out the Router pod.
//
// replicaNames counts every pod in the namespace, which is right for a fixture with no router and
// wrong for one with: the Router is discovered alongside the replicas but is not one of them, and
// a test asserting "exactly one replica of the two survives" must not see it.
func retirementReplicaNames(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()

	pods := replicaPods(t, cli)
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	slices.Sort(names)

	return names
}

// TestTheCreateGateDoesNotRefillTheSlotTheOperationIsEmptying pins the seventh guard, which the
// six named paths do not cover.
//
// After the protocol's delete lands the ordinal reads free, and a create gate that filled it would
// build a replacement one pass before the reservation cleared. Filling the slot the operation is
// about to release is how a retirement turns back into a rollout.
func TestTheCreateGateDoesNotRefillTheSlotTheOperationIsEmptying(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 2
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateSettling, "server", 1, nil))
	cli := newModelDeploymentClient(md, newRenderInstanceType())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 1,
		"a two-replica deployment whose reserved slot is emptying keeps ONE replica: the create "+
			"gate does not refill the ordinal until the reservation clears")
}

// retirementPathCase is one way this controller can delete a replica, built so the SAME fixture is
// asked the same question twice: once with the replica reserved, and once with nothing reserved.
type retirementPathCase struct {
	name string
	// why names the path in the controller's own terms, so a failure says which path leaked.
	why string
	// build returns the deployment, the objects to seed, and the name of the replica the case is
	// about -- the one the path would delete.
	build func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string)
	// removalIntent is that this path would delete a replica the spec no longer wants, which is
	// the set the protocol now takes over at admission. Everything else keeps the pre-existing
	// behavior exactly, and the table asserts both halves rather than one of them.
	removalIntent bool
	// survives asserts the reserved replica is still there. gone asserts it was removed; for a
	// removal-intent path that is after the protocol has run, not in the pass that noticed.
	survives func(t *testing.T, cli ctrlcli.Client, target string)
	gone     func(t *testing.T, cli ctrlcli.Client, target string)
}

// podSurvives is the common assertion: the replica the path would have deleted is still standing.
func podSurvives(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	assert.Contains(t, replicaNames(t, cli), target)
}

func podIsGone(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	assert.NotContains(t, replicaNames(t, cli), target)
}

// workloadSurvives asserts the group Workload is still there, which is the half of a departure that
// a Pod-only assertion cannot see: Kueue holds a deleted member of a serving group until that
// group's Workload goes, so the Workload delete is the one that actually finishes the job.
// workloadOwnsTarget reports whether any Workload in the namespace still owns the named replica.
// The question is asked about the Pod rather than about the namespace, because a deployment with a
// surviving replica keeps its Workload and a namespace-wide emptiness check would report that as a
// retained target.
func workloadOwnsTarget(t *testing.T, cli ctrlcli.Client, target string) bool {
	t.Helper()

	pod := new(core.Pod)
	if err := cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: target}, pod); err != nil {
		// A Pod that is not there cannot be owned, and saying so is the answer rather than a
		// failure: the case may be asking about a replica the path has already taken.
		return false
	}

	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))
	owned := sets.New(pod.UID)
	for i := range wlList.Items {
		if modelDeploymentWorkloadOwnsAny(&wlList.Items[i], owned) {
			return true
		}
	}

	return false
}

func workloadSurvives(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	assert.True(t, workloadOwnsTarget(t, cli, target),
		"the group's Workload is what holds the replica, so it is still there")
}

func workloadIsGone(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	assert.False(t, workloadOwnsTarget(t, cli, target),
		"the Workload delete is what actually releases the replica, so its absence is the "+
			"departure having happened rather than merely having been requested")
}

// podAndWorkloadGone is the two halves together, for the path that issues both.
func podAndWorkloadGone(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	podIsGone(t, cli, target)
	workloadIsGone(t, cli, target)
}

func podAndWorkloadSurvive(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()
	podSurvives(t, cli, target)
	workloadSurvives(t, cli, target)
}

// The six paths, in the order the converge reaches them. Each one is a real fixture rather than a
// call into the guard, because the guard is not the question: the question is whether a path that
// has never heard of the reservation can still take the replica out from under it.
var retirementDeletionPaths = []retirementPathCase{
	{
		name:          "a role the spec no longer names",
		why:           "the departed-role release",
		removalIntent: true,
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name: "extra", Replicas: 1, InstanceType: "h20-8x",
					Image: "vllm/vllm-openai:v0.25.1",
				})
			})
			md.Spec.Roles[0].Replicas = 1
			target := surplusReplicaAt(t, md, "qwen-extra-zero", 0, "")
			// The Pod is rendered from the first role, so its identity label is corrected to the
			// role the case is about. Without this the spec still declares the Pod's own role and
			// the path under test is never reached -- a fixture that quietly tests nothing.
			target.Labels[modelDeploymentLabelKeyComponent] = "extra"
			md.Spec.Roles = md.Spec.Roles[:1] // the role is scaled away

			return md, []ctrlcli.Object{newRenderInstanceType(), target}, target.Name
		},
		survives: podAndWorkloadSurvive,
		gone:     podAndWorkloadGone,
	},
	{
		name: "a replica already in a terminal phase",
		why:  "the terminal-pod departure",
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment()
			md.Spec.Roles[0].Replicas = 1
			target := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
			target.Status.Phase = core.PodFailed
			target.Status.Reason = "Evicted"

			return md, []ctrlcli.Object{newRenderInstanceType(), target}, target.Name
		},
		survives: externallyTerminated,
		gone:     podIsGone,
	},
	{
		name:          "an ordinal the role no longer declares",
		why:           "the surplus shed",
		removalIntent: true,
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment()
			md.Spec.Roles[0].Replicas = 1
			keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
			target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")

			return md, []ctrlcli.Object{newRenderInstanceType(), keeper, target}, target.Name
		},
		survives: podSurvives,
		gone:     podIsGone,
	},
	{
		name:          "a surplus replica's own Workload",
		why:           "the Workload delete that accompanies the surplus shed",
		removalIntent: true,
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment()
			md.Spec.Roles[0].Replicas = 1
			keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
			target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")

			return md, []ctrlcli.Object{newRenderInstanceType(), keeper, target}, target.Name
		},
		survives: workloadSurvives,
		gone:     workloadIsGone,
	},
	{
		name: "a replica built from an earlier spec",
		why:  "the rollout turn-over",
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment()
			// ONE DECLARED REPLICA, so the rollout's currency -- admitted replicas against the
			// declared count -- is met and the guard lets the turn-over through. A deployment short
			// of its count holds the rollout on purpose, and the case would then be asserting that
			// a path which correctly did not fire was intercepted.
			md.Spec.Roles[0].Replicas = 1
			target := surplusReplicaAt(t, md, "qwen-server-zero", 0, "a-hash-no-render-produces")
			md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"

			return md, []ctrlcli.Object{newRenderInstanceType(), target}, target.Name
		},
		survives: podAndWorkloadSurvive,
		gone:     podAndWorkloadGone,
	},
	{
		name: "a replica the kubelet is already taking away",
		why: "the stranded sweep, and the case where the protocol cannot hold: the member is " +
			"terminating, so it has already left and there is nothing to retain",
		build: func(t *testing.T) (*workercore.ModelDeployment, []ctrlcli.Object, string) {
			md := retirementDeployment()
			md.Spec.Roles[0].Replicas = 1
			target := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
			// A member on its way out is one this operator did not send away, and Kueue is still
			// holding its finalizer -- which is the whole reason the stranded sweep exists. The
			// finalizer is what makes the object legal: a Pod with a deletion timestamp and no
			// finalizer would leave the instant it were created.
			target.Finalizers = []string{kueuepodconst.PodFinalizer}
			now := meta.Now()
			target.DeletionTimestamp = &now

			return md, []ctrlcli.Object{newRenderInstanceType(), target}, target.Name
		},
		survives: externallyTerminated,
		gone:     noReservation,
	},
}

// externallyTerminated asserts the operation recorded the loss rather than the clean abort a
// budget expiry earns. A reserved replica the kubelet is taking away cannot be retained, and the
// status saying so is the whole contract: an abort that claimed retained members, a retained
// Workload and retained capacity would be claiming three things that do not exist.
func externallyTerminated(t *testing.T, cli ctrlcli.Client, _ string) {
	t.Helper()

	reservation := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, reservation, "the operation is still there to say what happened")
	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, reservation.State)
	assert.Contains(t, reservation.Reason, "terminated outside this operator")
}

// noReservation asserts the pass invented no operation at all, which is what a deployment that
// never opted into retirement must see.
func noReservation(t *testing.T, cli ctrlcli.Client, target string) {
	t.Helper()

	assert.Nil(t, getModelDeployment(t, cli).Status.Retirement)
	podIsGone(t, cli, target)
}

// TestEveryOwnerDeletionPathHoldsAReservedTarget is the cannot-bypass table.
//
// EACH ROW IS ONE MUTATION'S WORTH OF PROTECTION, and the six delete sites are disjoint, so a
// guard added to five of them still fails this table on the sixth. That is why the table exists
// rather than a single end-to-end test: an end-to-end test reaches whichever path the fixture
// happens to drive, and a path nobody drove is a path nobody is watching.
func TestEveryOwnerDeletionPathHoldsAReservedTarget(t *testing.T) {
	for _, tc := range retirementDeletionPaths {
		t.Run(tc.name, func(t *testing.T) {
			md, objs, target := tc.build(t)
			// The reservation names the replica by UID, which is the only identity that survives
			// the path having already decided to delete it.
			reserved := false
			for _, obj := range objs {
				if pod, ok := obj.(*core.Pod); ok && pod.Name == target {
					reserved = true
				}
			}
			require.True(t, reserved, "the fixture's target must be one of the seeded objects")

			uid := types.UID("member-1")
			for _, obj := range objs {
				if pod, ok := obj.(*core.Pod); ok && pod.Name == target {
					pod.UID = uid
				}
			}
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateWithdrawing, "server", 1, []string{string(uid)},
				func(r *workercore.ModelDeploymentRetirementStatus) {
					r.RoleName = modelDeploymentPodRole(findPod(objs, target))
				},
			))
			cli := newModelDeploymentClient(append(objs, md)...)
			// The Kueue stand-in fills in only the UIDs a Pod does not already carry, so the
			// reservation's UID set above is the one the composed Workload will own.
			standInForKueue(t, cli, true)

			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			_, err := reconcileModelDeploymentWith(t, r)
			require.NoError(t, err)

			tc.survives(t, cli, target)
		})
	}
}

// TestNoReservationLeavesEveryPathUnchanged is the other half of the same table, and it is the one
// that makes adding a protocol to this controller safe.
//
// A guard that rejects an unreserved replica would be a defect that only appears in deployments
// that never opted into retirement, which is every deployment in the cluster. Each row runs the
// SAME fixture with the reservation removed, so the two tables cannot drift apart.
func TestNoReservationLeavesEveryPathUnchanged(t *testing.T) {
	for _, tc := range retirementDeletionPaths {
		t.Run(tc.name, func(t *testing.T) {
			md, objs, target := tc.build(t)
			// The path is exercised against a deployment whose ingress can be OBSERVED, because a
			// deployment declaring no router now holds by design and none of these rows is about
			// that hold. The router reports no member of the target still served, so the removal
			// the path wanted is carried out by the protocol rather than refused forever.
			md = retirementRouterBacked(md)
			objs = append(objs, ownedRouterObjects(md)...)
			cli := newModelDeploymentClient(append(objs, md)...)
			standInForKueue(t, cli, true)

			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			r.servingViewFetch = func(_ context.Context, _ string) ([]byte, error) {
				return retirementRouterView(nil, nil), nil
			}
			_, err := reconcileModelDeploymentWith(t, r)
			require.NoError(t, err)

			if !tc.removalIntent {
				assert.Nil(t, getModelDeployment(t, cli).Status.Retirement,
					"a path with no removal intent must not invent an operation")
				tc.gone(t, cli, target)

				return
			}

			// A REMOVAL-INTENT PASS ADMITS INSTEAD OF DELETING, and that is the whole of the
			// retarget: the protocol owns this deletion now. The first pass admits and the replica
			// is still there; running the protocol out is what removes it. A path with no removal
			// intent above is unchanged, and that is the half of the rule the other half rests on.
			assert.NotNil(t, getModelDeployment(t, cli).Status.Retirement,
				"a removal intent admits an operation rather than issuing the delete")
			driveModelDeploymentRetirement(t, cli)
			tc.gone(t, cli, target)
		})
	}
}

// findPod returns one of the seeded Pods by name.
func findPod(objs []ctrlcli.Object, name string) *core.Pod {
	for _, obj := range objs {
		if pod, ok := obj.(*core.Pod); ok && pod.Name == name {
			return pod
		}
	}

	return &core.Pod{}
}

// TestAnExplicitZeroHoldsWithCapacityPreserved pins the scale-to-zero case.
//
// A spec of zero replicas makes every ordinal surplus by the ordinary rule, and the target is held
// anyway: the deployment asks for nothing, the cluster still has a replica serving, and the honest
// report is a desired count of zero beside an observed count above it. Forcing the count to the
// declared value on expiry would report a capacity release that did not happen.
func TestAnExplicitZeroHoldsWithCapacityPreserved(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 0
	target := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	target.UID = "member-1"
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateWithdrawing, "server", 0, []string{"member-1"}))
	cli := newModelDeploymentClient(md, newRenderInstanceType(), target)
	standInForKueue(t, cli, true)

	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	assert.Equal(t, []string{"qwen-server-zero"}, replicaNames(t, cli),
		"a spec declaring zero replicas does not drop the replica the protocol is holding")
	assert.Equal(t, int32(0), getModelDeployment(t, cli).Spec.Roles[0].Replicas,
		"the desired count is what the spec says")
}

// TestAProtocolThatIsNotAbortedIgnoresItsOwnRetryDirective is the negative of the retry rule at the
// whole-pass level, and it is what keeps a directive from resuming a step that never finished.
func TestAProtocolThatIsNotAbortedIgnoresItsOwnRetryDirective(t *testing.T) {
	md := retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Annotations = map[string]string{
			modelDeploymentRetirementRetryAnnotation: "server:1:token-a",
		}
	})
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateWithdrawing, "server", 1, []string{"member-1"}))
	pods := []core.Pod{healthPod("server", 1, 0, healthBool(true), "member-1")}
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining, plan.Reservation.State,
		"the pass advanced its own step and did not jump back to Admitted")
	assert.Empty(t, plan.Reservation.LastConsumedRetryToken,
		"an unconsumed directive is not consumed by a pass that has nothing to retry")
	assert.Contains(t, md.Annotations, modelDeploymentRetirementRetryAnnotation)
}

// TestARouterResidualBlocksDeletion pins the withdrawal answer's own rule, asked per member.
//
// A deployment of three replicas has a serving count of two once the target stops serving, so
// reading the deployment-wide aggregate as "the target is still being served" would hold the
// operation until the rest of the deployment went too. The residual has to be asked about the
// target's own members, and one of them still answering is enough to block.
func TestARouterResidualBlocksDeletion(t *testing.T) {
	testCases := []struct {
		name    string
		serving []string
		want    workercore.ModelDeploymentRetirementState
	}{
		{
			// The withdrawal is CONFIRMED when nothing serves the target, and the operation
			// moves on to Draining. It is read as one step: the operation is at Disqualified and
			// the answer either lets it advance or leaves it there naming the residual.
			name: "no member served is a confirmed withdrawal",
			want: workercore.ModelDeploymentRetirementStateWithdrawing,
		},
		{
			name:    "one member still served blocks the delete",
			serving: []string{"member-1"},
			want:    workercore.ModelDeploymentRetirementStateDisqualified,
		},
		{
			// THE OTHER REPLICA IS THE ROW THAT MATTERS MOST. The deployment-wide serving count is
			// one in this case and would read as "something is still serving" under any rule that
			// asked the aggregate; the residual has to be asked about the target's own members or
			// the operation would hold until the whole deployment drained.
			name:    "only a different replica's members served does not block",
			serving: []string{"other-1"},
			want:    workercore.ModelDeploymentRetirementStateWithdrawing,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment()
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
			// The members are Running, because the binder refuses an address that resolves to a
			// Pod which is not live and reports the whole view as unusable -- a refused view holds
			// for a different reason and would make every row of this table assert the same thing.
			// The members are RENDERED rather than hand-built, so they carry every label the
			// deployment's own Pod listing selects on. A hand-built Pod that the listing skipped
			// would leave the binder with nothing to resolve against, and every row would be
			// asserting an unusable view rather than the residual it is about.
			pods := []core.Pod{
				*surplusReplicaAt(t, md, "qwen-server-one", 1, ""),
				*surplusReplicaAt(t, md, "qwen-server-zero", 0, ""),
			}
			pods[0].UID = "member-1"
			pods[1].UID = "other-1"
			for i := range pods {
				pods[i].Status.Phase = core.PodRunning
				pods[i].Status.PodIP = "10.0.1." + strconvx.Itoa(1+i)
			}
			// The operation enters at Disqualified, because that is the step the withdrawal answer
			// is read on: Disqualified waits for the target to be out of selection, and the
			// transition out of it is where the Routers are asked whether anything still serves it.
			md = reserve(md, retirementReservation(
				workercore.ModelDeploymentRetirementStateDisqualified, "server", 1,
				[]string{"member-1"}))

			// THE PODS ARE SEEDED, because the residual is read through a binding that resolves
			// each worker's address to a live Pod. With no Pod on the server the view is refused as
			// indeterminate, every case would hold, and the "no residual" rows would be asserting
			// a refusal rather than the absence they are about.
			seeded := make([]ctrlcli.Object, 0, len(pods)+4)
			seeded = append(seeded, md.DeepCopy(), newRenderInstanceType())
			seeded = append(seeded, ownedRouterObjects(md)...)
			for i := range pods {
				seeded = append(seeded, pods[i].DeepCopy())
			}
			cli := newRetirementInterceptedClient(ctrlinterceptor.Funcs{}, seeded...)
			r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
			r.servingViewFetch = func(_ context.Context, _ string) ([]byte, error) {
				return retirementRouterView(tc.serving, pods), nil
			}

			plan := r.planModelDeploymentRetirement(context.Background(), md, pods)

			assert.Equal(t, tc.want, plan.Reservation.State,
				"a residual on the target's own members holds the operation at Disqualified; a "+
					"withdrawal with no residual on it advances whatever the rest of the "+
					"deployment is doing")
			if tc.want == workercore.ModelDeploymentRetirementStateDisqualified {
				assert.True(t, plan.Hold, "a target with a residual on it is held, not deleted")
				assert.Contains(t, plan.Reservation.Reason, "still served")
			}
		})
	}
}

// retirementRouterPod builds the Router Pod the serving collection discovers by label, with the
// address the fetch is dialed against. The workers it reports come from the view, not from here.
//
// IT CARRIES NO KUEUE GROUP LABEL, and that is the production shape rather than an omission:
// renderModelDeploymentRouterObjects gives the Router the name, instance and router labels and
// nothing else, because the Router is not a Kueue-managed member of a serving group. A group label
// here would invent Router quota the real object never asks for.
//
// ITS UID IS DERIVED FROM ITS NAME, because two Router processes are two Pods and a Pod's UID is
// what the observation collection groups their per-process boot generations by. One shared literal
// made a multi-router fixture report two independent processes as one, which is the exact
// disagreement the generation rules exist to catch.
func retirementRouterPod(name, ip string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: "team-a", UID: types.UID("router-uid-" + name),
			Labels: map[string]string{
				modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance: "qwen",
				modelDeploymentRouterLabelKey:   "llm-d-router",
			},
		},
		Status: core.PodStatus{PodIP: ip},
	}
}

// retirementRouterView renders the observer payload the routers serve, naming the given members as
// still in selection. The addresses resolve to the Pod UIDs the collection binds at read time, so
// the binding is by live Pod rather than by anything the payload claims about identity.
func retirementRouterView(serving []string, pods []core.Pod) []byte {
	// EACH WORKER'S ADDRESS IS THE LIVE POD'S OWN, read out of the Pod list rather than written
	// here. The collection binds a worker's address to the Pod it resolves to at read time and
	// refuses a view it cannot bind, so a payload whose addresses were invented would be rejected
	// as indeterminate and the case would be asserting a hold it never arranged.
	ipOf := func(uid string) string {
		for i := range pods {
			if string(pods[i].UID) == uid {
				return pods[i].Status.PodIP
			}
		}

		return ""
	}
	workers := make([]map[string]any, 0, len(serving))
	for _, uid := range serving {
		workers = append(workers, map[string]any{
			"url":  "http://" + ipOf(uid) + ":8000",
			"port": "8000", "role": "server",
			"pod_hint": uid, "worker_id": uid,
			"selection": map[string]any{
				"registered": true, "healthy": true,
				"circuit_open": false, "in_selection": true,
			},
		})
	}

	payload, err := json.Marshal(map[string]any{
		"router":            map[string]any{"boot_generation": 1, "now_ms": 1},
		"registry_revision": 1,
		"workers":           workers,
	})
	if err != nil {
		panic(err)
	}

	return payload
}

// TestASettledReservationWritesNothing pins the level-based property at the level of writes rather
// than of state.
//
// A controller that runs on every Pod event writes to the object on every one of them unless the
// pass has something to say, and the reservation is the field most able to churn: its reason and
// its state both move with every observation. A pass that rewrites an unchanged reservation would
// keep the object permanently dirty, and the write would wake the deployment that produced it.
func TestASettledReservationWritesNothing(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateWithdrawing, "server", 1, []string{"member-1"}))
	cli := newModelDeploymentClient(md, newRenderInstanceType(), target)
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	// The first pass settles the reservation on the object; the second has nothing left to say.
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	settled := getModelDeployment(t, cli)
	require.NotNil(t, settled.Status.Retirement)

	writes := new(modelDeploymentWrites)
	counting := newCountingModelDeploymentClient(writes, settled, newRenderInstanceType())
	r2 := holdReconciler(counting, &scriptedDrainReader{}, time.Now())
	_, err = reconcileModelDeploymentWith(t, r2)
	require.NoError(t, err)

	// THE COUNTER IS READ FROM A STEADY STATE, and the first counted pass is a warm-up rather than
	// the measurement. A pass can legitimately settle something else on the way through -- the
	// first pass over a deployment it has never described moves fields unrelated to the
	// reservation -- and counting that would make this a test of how many unrelated fields the
	// status carries rather than of the reservation being quiet.
	writes.statusUpdates = 0
	_, err = reconcileModelDeploymentWith(t, r2)
	require.NoError(t, err)

	assert.Zero(t, writes.statusUpdates,
		"a pass that reached the state it was already in writes nothing, or the reservation keeps "+
			"the object dirty on every Pod event")
}

// retirementIdleReader stands the drain seam up as idle, for the tests whose subject is which
// replica the convergence takes rather than the drain itself.
//
// IT IS A TEST FIXTURE AND NOT A PRODUCTION WIRING, and the difference is the whole reason it is
// named here. The production reader refuses, so a scale-down admits a reservation and then holds at
// Draining until the transport lands; a test that stood the seam up for real would be asserting a
// collector this tree does not have. These tests are about the bookkeeping either side of the
// delete, so they supply the missing measurement and leave the protocol to run.
type retirementIdleReader struct{ idle modelDeploymentDrainAnswer }

func (r retirementIdleReader) Drain(
	_ context.Context, _ modelDeploymentDrainTarget,
) (modelDeploymentDrainAnswer, error) {
	return r.idle, nil
}

// reconcileModelDeploymentDraining reconciles with the seam stood up, so a pass that admits a
// reservation can go on to complete one.
//
// THE ROUTER FETCH IS SCRIPTED HERE TOO, answering with a view that serves no member. A deployment
// that declares a router has its ingress observed through that router, and leaving the transport
// unwired would dial the router pod's address for real and hang the test rather than answer it. The
// scripted answer is the one a fixture driving an operation to its end needs: the observer was
// asked, and it reported that nothing serves the target. A deployment that declares NO router
// never reaches this code at all -- the direct-Service hold answers before any fetch is made.
func reconcileModelDeploymentDraining(t *testing.T, cli ctrlcli.Client) (ctrl.Result, error) {
	t.Helper()
	retirementAssignPodUIDs(t, cli)
	seedHealthyRouterOwnership(t, cli, getModelDeployment(t, cli))

	return reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		drainReader: retirementIdleReader{idle: idleDrain()},
		servingViewFetch: func(_ context.Context, _ string) ([]byte, error) {
			return retirementRouterView(nil, nil), nil
		},
	})
}

// driveModelDeploymentRetirement runs passes until the operation this pass admitted has finished.
//
// IT IS SEVERAL PASSES BECAUSE THE PROTOCOL IS A SEQUENCE. A scale-down that is now a retirement
// takes a pass to admit, one to withdraw, one to drain and one to delete, and a test that asserted
// its subject after a single pass would be asserting the first step of a protocol rather than the
// replica the convergence takes. The bound is a failure rather than a loop: an operation that
// cannot finish is a defect, and a test that waited for it would report a timeout instead.
func driveModelDeploymentRetirement(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for pass := 0; pass < 8; pass++ {
		_, err := reconcileModelDeploymentDraining(t, cli)
		require.NoError(t, err)
		if getModelDeployment(t, cli).Status.Retirement == nil {
			return
		}
	}

	t.Fatal("the retirement protocol did not finish within the pass bound")
}

// reconcileModelDeploymentRetiring reconciles once and, if that pass admitted a retirement, runs
// the operation to its end before returning.
//
// IT IS SAFE AT EVERY CALL SITE, which is what makes it usable in tests whose subject is the
// convergence rather than the protocol. A pass with no removal intent admits nothing, so the loop
// never runs and the pass is the ordinary one it always was. A pass that did admit one has changed
// the test's world -- the replica is now being retired rather than deleted -- and the assertions
// after it are about the end of that change, so the change is carried to its end first.
//
// The seam is stood up as idle for the same reason as driveModelDeploymentRetirement: the
// production transport refuses, and a test about which replica leaves is not a test about the drain.
func reconcileModelDeploymentRetiring(t *testing.T, cli ctrlcli.Client) (ctrl.Result, error) {
	t.Helper()

	res, err := reconcileModelDeploymentDraining(t, cli)
	if err != nil {
		return res, err
	}
	// The bound covers several operations, not one: a removal the protocol owns is serialized, so
	// a role losing two replicas runs two of them in sequence and each takes its own passes.
	for pass := 0; pass < 32; pass++ {
		if getModelDeployment(t, cli).Status.Retirement == nil {
			break
		}
		if _, err = reconcileModelDeploymentDraining(t, cli); err != nil {
			return ctrl.Result{}, err
		}
	}

	return res, nil
}

// retirementAssignPodUIDs gives every replica pod an identity, the way the API server does before
// this controller ever sees one.
//
// IT IS NOT COSMETIC. A retirement names its target by member UID, and a pod without one is not
// nameable: the reservation would bind the empty string, which matches every pod in the
// deployment, and the protocol would delete the whole thing. The reconciler refuses to admit such a
// reservation and holds the removal instead, which is right in production and would make these
// tests wait for a drain that can never come. Assigning the identity here mirrors the only
// environment this code actually runs in.
func retirementAssignPodUIDs(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for i, pod := range replicaPods(t, cli) {
		if pod.UID != "" {
			continue
		}
		identified := pod.DeepCopy()
		identified.UID = types.UID("uid-" + pod.Name)
		require.NoError(t, cli.Update(context.Background(), identified), "pod %d", i)
	}
}

// TestARemovalIntentAdmitsAndTheAdmittingPassDeletesNothing pins the admission leg itself, and its
// two halves are the two things a removal path must now do instead of deleting.
//
// THE PASS THAT ADMITS DELETES NOTHING. A 2-to-1 scale-down used to issue its delete on the first
// pass that noticed the surplus; it now admits on that pass and holds, and the replica is still
// standing when the pass returns. The delete arrives several passes later, after a withdrawal and
// a drain, which is the entire point of the protocol existing.
func TestARemovalIntentAdmitsAndTheAdmittingPassDeletesNothing(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 1
	surplus := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	surplus.UID, keeper.UID = "target", "keeper"
	cli := newModelDeploymentClient(md, newRenderInstanceType(), surplus, keeper)
	standInForKueue(t, cli, true)
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	admitted := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, admitted, "the removal intent admits an operation")
	assert.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted, admitted.State)
	assert.Equal(t, "server", admitted.RoleName)
	assert.Equal(t, int32(1), admitted.ReplicaOrdinal,
		"the reservation names the replica the surplus rule pointed at")
	assert.Equal(t, []string{"target"}, admitted.TargetMemberUIDs,
		"the reservation binds the members as they are now, by identity")
	assert.Equal(t, md.Generation, admitted.ObservedGeneration)
	assert.True(t, admitted.StartedAt.Equal(&admitted.PhaseStartedAt),
		"the operation's first phase starts when it is admitted")
	assert.Equal(t, admitted.StartedAt.Add(modelDeploymentRetirementOverallBudget),
		admitted.Deadline.Time,
		"the overall budget runs from admission, not from the first pass that noticed")

	assert.Contains(t, replicaNames(t, cli), "qwen-server-one",
		"the pass that admits deletes nothing: the replica is still standing when it returns")
}

// TestASecondRemovalIntentIsRefusedWhileOneIsInFlight pins the serialization, and the reason it is
// serialized rather than queued.
func TestASecondRemovalIntentIsRefusedWhileOneIsInFlight(t *testing.T) {
	md := retirementRouterBacked(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	first := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	first.UID = "first"
	second := surplusReplicaAt(t, md, "qwen-server-two", 2, "")
	second.UID = "second"
	cli := newModelDeploymentClient(append(
		[]ctrlcli.Object{md, newRenderInstanceType(), first, second}, ownedRouterObjects(md)...)...)
	standInForKueue(t, cli, true)
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	r.servingViewFetch = func(_ context.Context, _ string) ([]byte, error) {
		return retirementRouterView(nil, nil), nil
	}

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	admitted := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, admitted, "the first intent admits")
	assert.Equal(t, []string{"first"}, admitted.TargetMemberUIDs,
		"the operation names the replica it took over, and only that one")

	names := replicaNames(t, cli)
	assert.Contains(t, names, "qwen-server-two",
		"the second intent is refused rather than queued, so its replica is still there")

	// IT IS NOT LOST. Nothing records the refusal, and that is the point: the removal intent is
	// still standing when the operation ends, so the pass after that admits it. A recorded queue
	// would be a second thing to reconcile and a second thing to lose.
	driveModelDeploymentRetirement(t, cli)
	assert.NotContains(t, replicaNames(t, cli), "qwen-server-two",
		"the refused intent is admitted and carried out once the operation ends")
	assert.NotContains(t, replicaNames(t, cli), "qwen-server-one")
}

// TestTheRolloutStepsOverAReservedReplica pins the interaction that is easiest to get wrong and was
// found only by running the surplus case end to end.
//
// THE ROLLOUT CONDEMNS A REPLICA WHOSE MEMBERS DISAGREE WITH THE SPEC, and while the retirement
// protocol is removing one member of a doubled ordinal that ordinal disagrees BY DESIGN. The
// rollout would therefore see a replica that is momentarily over-complete, condemn the whole
// ordinal, and delete both members -- undoing the surplus rule's careful choice of which member to
// keep with a blunter one. A reservation names a slot, so the rollout is asked to step over that
// slot entirely rather than to act on the members it does not hold.
func TestTheRolloutStepsOverAReservedReplica(t *testing.T) {
	md := newRenderDeployment() // declares two
	stale := surplusReplicaAt(t, md, "qwen-server-dup-z", 0, "a-hash-no-render-produces")
	keeper := surplusReplicaAt(t, md, "qwen-server-dup-a", 0, "")
	seated := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	stale.UID, keeper.UID, seated.UID = "stale", "keeper", "seated"
	cli := newModelDeploymentClient(md, newRenderInstanceType(), stale, keeper, seated)
	standInForKueue(t, cli, true)
	r := holdReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"stale": {idleDrain(), idleDrain()}},
	}, time.Now())

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	reservation := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, reservation, "the surplus sheds a member and the protocol takes it over")
	assert.Equal(t, []string{"stale"}, reservation.TargetMemberUIDs,
		"the reservation names the member the surplus rule chose, not the ordinal's both members")
	assert.Contains(t, replicaNames(t, cli), "qwen-server-dup-a",
		"the member the current render describes survives: the rollout did not condemn the ordinal")
}

// TestTheCommitLeavesASharedGroupWorkloadAlone pins the group-scope guard on the Workload delete.
//
// A Kueue group is a role's serving set, not one replica's, so the Workload that releases a member's
// finalizer is the same object for every replica of the role. Deleting it for the sake of one
// retiring replica releases every member and takes the whole deployment down. Only a Workload that
// owns nothing outside the target is deleted.
func TestTheCommitLeavesASharedGroupWorkloadAlone(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	survivor := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	target.UID, survivor.UID = "target", "survivor"
	cli := newModelDeploymentClient(md, newRenderInstanceType(), target, survivor)

	// ONE WORKLOAD OWNING BOTH, which is what a shared serving group composes.
	shared := &kueue.Workload{}
	shared.Name, shared.Namespace = "shared", "team-a"
	shared.OwnerReferences = []meta.OwnerReference{
		{APIVersion: "v1", Kind: "Pod", Name: target.Name, UID: target.UID},
		{APIVersion: "v1", Kind: "Pod", Name: survivor.Name, UID: survivor.UID},
	}
	require.NoError(t, cli.Create(context.Background(), shared))

	plan := &modelDeploymentRetirementPlan{
		DeletesAllowed: true,
		Deletes:        []core.Pod{*target},
	}
	require.NoError(t, r2(cli).commitModelDeploymentRetirementWorkload(context.Background(), md, plan,
		[]core.Pod{*target, *survivor}))

	found := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), found, ctrlcli.InNamespace("team-a")))
	assert.Len(t, found.Items, 1,
		"a Workload that still owns a surviving member is not deleted: the group is still serving")

	// AND A WORKLOAD THAT OWNS ONLY THE TARGET IS DELETED, because it is about to be nothing.
	sole := &kueue.Workload{}
	sole.Name, sole.Namespace = "sole", "team-a"
	sole.OwnerReferences = []meta.OwnerReference{
		{APIVersion: "v1", Kind: "Pod", Name: target.Name, UID: target.UID},
	}
	require.NoError(t, cli.Create(context.Background(), sole))
	require.NoError(t, r2(cli).commitModelDeploymentRetirementWorkload(context.Background(), md, plan,
		[]core.Pod{*target, *survivor}))

	found = new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), found, ctrlcli.InNamespace("team-a")))
	assert.Len(t, found.Items, 1, "the Workload owning nothing but the target is gone")
}

func r2(cli ctrlcli.Client) *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(8)}
}

// TestAdmissionRefusesATargetItCannotName pins the empty-identity fail-open.
//
// THE TARGET IS RESOLVED BY UID, so a reservation bound to the empty string names EVERY Pod in the
// deployment and the protocol deletes all of it. That is a cluster of one misplaced bracket away
// from deleting a whole deployment over a missing identity, so admission refuses to bind a member
// that has none -- and it HOLDS rather than falling through, because the only way to know a
// removal is safe is to have an identity to check it against.
func TestAdmissionRefusesATargetItCannotName(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 1
	surplus := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	// NEITHER POD CARRIES A UID, which is the state this refuses.
	cli := newModelDeploymentClient(md, newRenderInstanceType(), surplus, keeper)
	r := r2(cli)

	plan := &modelDeploymentRetirementPlan{DeletesAllowed: true}
	held := r.refuseModelDeploymentRemoval(context.Background(), md, plan, surplus,
		[]core.Pod{*surplus, *keeper}, nil)

	assert.True(t, held, "an unnameable target holds the removal")
	assert.Nil(t, plan.Reservation, "no reservation is written, so none can name every Pod")
	assert.ElementsMatch(t, []string{"qwen-server-one", "qwen-server-zero"}, replicaNames(t, cli),
		"the removal holds rather than being issued: nothing is deleted on a target that could not "+
			"be identified")
}

// TestTheDrainIsIdempotentUnderUnchangedIntent pins that a re-admission does not happen on every
// pass, which is what stops a removal from being re-admitted once per reconcile.
func TestTheDrainIsIdempotentUnderUnchangedIntent(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 1
	surplus := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	surplus.UID, keeper.UID = "target", "keeper"
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"target"}))
	cli := newModelDeploymentClient(md, newRenderInstanceType(), surplus, keeper)
	standInForKueue(t, cli, true)

	// THE READER NEVER GOES IDLE, so the operation stays where it is and every pass sees the same
	// intent. An unchanged intent must not re-admit: a re-admission each pass would restart the
	// budget each pass, so the budget would never expire and the abort could never be reached.
	reader := &scriptedDrainReader{answers: map[types.UID][]modelDeploymentDrainAnswer{
		"target": {busyDrain(), busyDrain()},
	}}
	r := holdReconciler(cli, reader, time.Now())

	first := getModelDeployment(t, cli)
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	after := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, after)
	assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining, after.State)
	assert.Equal(t, first.Status.Retirement.StartedAt, after.StartedAt,
		"unchanged intent does not re-admit, so the operation's clock is not restarted")
	assert.Equal(t, first.Status.Retirement.PhaseStartedAt, after.PhaseStartedAt,
		"and neither is the phase's, so a drain budget can actually expire")
}

// TestNoPathButTheThreeRemovalIntentsEverAdmits pins which paths are allowed to start an operation.
//
// THE ROLLOUT, the terminal-pod departure and the stranded sweep are all deletions, and none of them
// may start a retirement. A retirement is a withdrawal a user asked for, and a rollout replacing a
// replica under an unchanged replica count is not one: admitting there would make every spec edit
// wait out a protocol the user never asked for, and would make a rollout unrepresentable while one
// happened to be in flight.
func TestNoPathButTheThreeRemovalIntentsEverAdmits(t *testing.T) {
	testCases := []struct {
		name string
		why  string
		pod  func(t *testing.T, md *workercore.ModelDeployment) *core.Pod
	}{
		{
			name: "a terminal replica",
			why:  "it has already departed, so there is nothing to withdraw",
			pod: func(t *testing.T, md *workercore.ModelDeployment) *core.Pod {
				pod := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
				pod.Status.Phase = core.PodFailed

				return pod
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := retirementDeployment()
			md.Spec.Roles[0].Replicas = 1
			target := tc.pod(t, md)
			keeper := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
			target.UID, keeper.UID = "target", "keeper"
			cli := newModelDeploymentClient(md, newRenderInstanceType(), target, keeper)

			// A SINGLE PASS, driven the ordinary way, with no admission helper in the path: a pass
			// that is not a removal intent must leave the object without an operation.
			_, err := reconcileModelDeploymentWith(t, holdReconciler(cli, nil, time.Now()))
			require.NoError(t, err)

			reservation := getModelDeployment(t, cli).Status.Retirement
			if reservation != nil {
				assert.NotEqual(t, 0, len(reservation.TargetMemberUIDs),
					tc.why+", so nothing that admits was reached")
			}
		})
	}
}

// TestTheHoldSurvivesIntoTheCommittedDelete pins that a hold does not lapse at Deleting.
//
// THE OPERATION IS PAST THE POINT OF NO RETURN, and the convergence is still looking at the same
// Pods on the same pass. A hold that lifted here would let the surplus rule -- which reads the Pods
// as they are, and the spec says one replica -- collect the very replica the protocol is in the
// middle of deleting. The protocol deletes a replica member by member; the surplus rule's decision
// is per-replica; and in this state the two disagree.
func TestTheHoldSurvivesIntoTheCommittedDelete(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 1
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	target.UID, keeper.UID = "target", "keeper"
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDeleting, "server", 1, []string{"target"}))
	cli := newModelDeploymentClient(md, newRenderInstanceType(), target, keeper)
	standInForKueue(t, cli, true)
	r := holdReconciler(cli, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{"target": {idleDrain(), idleDrain()}},
	}, time.Now())

	plan := r.planModelDeploymentRetirement(context.Background(), md, []core.Pod{*target, *keeper})

	assert.True(t, plan.Hold,
		"a committed delete is still a hold: the target is this operation's until the API server "+
			"says otherwise")
	assert.True(t, plan.holds(target.UID),
		"and the target specifically, which is the one the surplus rule would otherwise collect")
	assert.False(t, plan.holds(keeper.UID), "a replica the operation does not name is not held")
}

// advancingReconciler builds a reconciler whose clock is a variable the test moves, so a phase
// budget is spent by time passing rather than by repeated calls at one instant. Repeated calls at
// a single moment cannot spend any budget at all, which is why every expiry case here advances it.
func advancingReconciler(
	cli ctrlcli.Client, reader modelDeploymentDrainReader, now *time.Time,
) *ModelDeploymentReconciler {
	r := holdReconciler(cli, reader, *now)
	r.clock = func() time.Time { return *now }

	return r
}

// TestTheWithdrawalBudgetIsSpentByAnAdvancingClock pins that the withdrawal phase budget is
// reachable at all.
//
// The operation waits at Admitted for the target to leave selection, and the health predicate
// removes the eligibility key on the ordinary convergence, so a pass that polls Admitted can see
// the key still present many times over. If each such pass restarted the phase clock, the budget
// would reset on every poll and the withdrawal could never time out however long the target
// refused to leave selection. The clock here advances, so the budget is genuinely spendable.
func TestTheWithdrawalBudgetIsSpentByAnAdvancingClock(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	// THE KEY STAYS ON, because that is the situation the budget exists for: the target is still
	// in endpoint selection, so the operation waits and the wait is bounded.
	if target.Labels == nil {
		target.Labels = map[string]string{}
	}
	target.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
	target.Status.Phase = core.PodRunning

	started := time.Now()
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAdmitted, "server", 1, []string{"member-1"},
		func(r *workercore.ModelDeploymentRetirementStatus) {
			r.StartedAt = meta.NewTime(started)
			r.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			r.PhaseStartedAt = meta.NewTime(started)
		}))

	cli := newModelDeploymentClient(md, newRenderInstanceType(), target)
	now := started
	r := advancingReconciler(cli, &scriptedDrainReader{}, &now)

	// The operation polls at Admitted while the key is on it. No budget is spent, and the phase
	// start is the one the operation was admitted with.
	for pass := 0; pass < 5; pass++ {
		now = now.Add(2 * time.Second)
		plan := r.planModelDeploymentRetirement(context.Background(), md, []core.Pod{*target})
		require.Equal(t, workercore.ModelDeploymentRetirementStateAdmitted, plan.Reservation.State,
			"a target still in selection keeps the operation waiting")
		assert.True(t, plan.Reservation.PhaseStartedAt.Time.Equal(started),
			"waiting must not move the phase start, or the budget resets on every poll")
	}

	// Time passes beyond the withdrawal budget with the key still present.
	now = now.Add(modelDeploymentRetirementWithdrawalBudget)
	plan := r.planModelDeploymentRetirement(context.Background(), md, []core.Pod{*target})

	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State,
		"a target that will not leave selection runs the withdrawal budget out and aborts")
	assert.Contains(t, plan.Reservation.Reason, "retained",
		"an abort before deletion retains the members, the workload and the capacity")
	assert.True(t, plan.Hold, "and the retained target stays held rather than being collected")
}

// TestARetainedAbortedReservationRefusesASecondRemoval pins that a retained abort serializes
// removal intents.
//
// An abort deliberately retains the replica, so a later scale-down that deleted it would not have
// retained anything. The second intent below names a different replica: it is refused, and the
// retained reservation is left exactly as it was rather than being overwritten by the new target.
func TestARetainedAbortedReservationRefusesASecondRemoval(t *testing.T) {
	md := retirementDeployment()
	// One replica is declared, so the target at ordinal one is surplus and is not a re-declared
	// slot. Adoption is for a replica the spec wants again, and a fixture that made the retained
	// target re-declared would be cleared by adoption before the second intent was ever asked.
	md.Spec.Roles[0].Replicas = 1
	retained := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	retained.UID = "retained"
	retained.Status.Phase = core.PodRunning
	other := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	other.UID = "other"
	other.Status.Phase = core.PodRunning

	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"retained"},
		func(r *workercore.ModelDeploymentRetirementStatus) {
			r.Reason = "the withdrawal budget was exhausted; members, workload and capacity are retained"
		}))
	before := md.Status.Retirement.DeepCopy()

	cli := newModelDeploymentClient(md, newRenderInstanceType(), retained, other)
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	pods := []core.Pod{*retained, *other}

	// The abort holds the retained target but is no longer in flight, which is exactly the state
	// in which a second intent used to be admitted.
	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)
	require.False(t, plan.InFlight, "an abort is terminal until new intent, so nothing is in flight")
	require.True(t, plan.Hold, "and the retained target is still held")

	refused := r.refuseModelDeploymentRemoval(context.Background(), md, plan, other, pods, nil)

	assert.True(t, refused, "a retained reservation refuses a second removal intent")
	after := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, after, "the retained reservation is still on the object")
	assert.Equal(t, before.State, after.State, "and its state is untouched")
	assert.Equal(t, before.TargetMemberUIDs, after.TargetMemberUIDs,
		"the retained target's identity is not replaced by the second intent's target")
}

// TestAnExternallyTerminatedAbortStillAdmits pins the other side of the same guard.
//
// An abort that retained nothing has no reason to serialize anything: the target is gone, the hold
// was released with it, and a later removal is a new operation rather than a conflict.
func TestAnExternallyTerminatedAbortStillAdmits(t *testing.T) {
	md := retirementDeployment()
	md.Spec.Roles[0].Replicas = 1
	other := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	other.UID = "other"
	other.Status.Phase = core.PodRunning

	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAdmitted, "server", 1, []string{"vanished"},
		func(r *workercore.ModelDeploymentRetirementStatus) { r.Reason = "admitted" }))
	cli := newModelDeploymentClient(md, newRenderInstanceType(), other)
	r := holdReconciler(cli, &scriptedDrainReader{}, time.Now())
	pods := []core.Pod{*other}

	// The reservation's target no longer exists, so the pass records the external termination,
	// releases the hold, and leaves nothing in flight.
	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)
	require.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State)
	require.False(t, plan.Hold, "a target that cannot be retained must not be held")
	require.False(t, plan.InFlight)

	refused := r.refuseModelDeploymentRemoval(context.Background(), md, plan, other, pods, nil)
	assert.True(t, refused, "the pass that admits holds the removal itself; the next one deletes")
	assert.Equal(t, "other", getModelDeployment(t, cli).Status.Retirement.TargetMemberUIDs[0],
		"and the new target's identity is the one now reserved, because nothing was retained")
}

// TestANoRouterDeploymentHoldsUntilTheBudgetAbortsAndRetains pins the direct-Service contract.
//
// With no router there is no observer to ask, and "nobody to ask" is not a reading of the
// connections the target's own Service already established. The operation therefore holds where it
// is, keeps every member, issues no delete, and lets the withdrawal budget end it the way any other
// pre-delete expiry ends: aborted, with the members, the workload and the capacity retained.
func TestANoRouterDeploymentHoldsUntilTheBudgetAbortsAndRetains(t *testing.T) {
	md := retirementDeployment()
	require.Nil(t, md.Spec.Router, "this is the direct-Service case: the fixture declares no router")
	md.Spec.Roles[0].Replicas = 2
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	keeper.UID = "keeper"
	target.Status.Phase, keeper.Status.Phase = core.PodRunning, core.PodRunning

	started := time.Now()
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAdmitted, "server", 1, []string{"member-1"},
		func(r *workercore.ModelDeploymentRetirementStatus) {
			r.StartedAt = meta.NewTime(started)
			r.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			r.PhaseStartedAt = meta.NewTime(started)
		}))

	cli := newModelDeploymentClient(md, newRenderInstanceType(), target, keeper)
	now := started
	r := advancingReconciler(cli, &scriptedDrainReader{}, &now)
	pods := []core.Pod{*target, *keeper}

	// Each pass re-enters at the state the previous one persisted, which is what the controller
	// does between passes; the test carries the reservation forward itself because it is calling
	// the planner rather than the reconciler.
	pass := func() *modelDeploymentRetirementPlan {
		plan := r.planModelDeploymentRetirement(context.Background(), md, pods)
		md.Status.Retirement = plan.Reservation.DeepCopy()

		return plan
	}

	// The target is out of selection, so the withdrawal step is reached and has to answer.
	plan := pass()
	require.Equal(t, workercore.ModelDeploymentRetirementStateDisqualified, plan.Reservation.State,
		"leaving selection is observed; what is not observed is the established-connection hold")

	now = now.Add(10 * time.Second)
	plan = pass()
	assert.Equal(t, workercore.ModelDeploymentRetirementStateDisqualified, plan.Reservation.State,
		"with no quiescence observation the operation stays where it is rather than advancing")
	assert.Contains(t, plan.Reservation.Reason, "direct-Service",
		"and the reason names what could not be observed")
	assert.True(t, plan.Hold)
	assert.Empty(t, plan.Deletes, "no delete is authorized while the hold stands")

	// The budget runs out with the connections still unobserved, and the expiry is an ordinary
	// pre-delete abort: everything is retained and nothing was deleted.
	now = now.Add(modelDeploymentRetirementWithdrawalBudget)
	plan = pass()
	assert.Equal(t, workercore.ModelDeploymentRetirementStateAborted, plan.Reservation.State)
	assert.True(t, plan.Hold, "the retained target is held after the abort")
	assert.Contains(t, plan.Reservation.Reason, "retained")
	assert.Empty(t, plan.Deletes)

	survivors := replicaNames(t, cli)
	assert.Contains(t, survivors, "qwen-server-one",
		"the target is preserved: a direct-Service hold never deletes the replica")
	assert.Contains(t, survivors, "qwen-server-zero", "and the surviving replica is untouched")

	// THE NEGATIVE, STATED AS ONE. A deployment that declares no router must not have acquired a
	// Router Workload or quota on the way to being held: the hold is an absence of observation, and
	// the fixture corrections made beside it must not have invented the capability to satisfy it.
	workloadList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), workloadList, ctrlcli.InNamespace("team-a")))
	for _, workload := range workloadList.Items {
		assert.NotContains(t, workload.Name, "router",
			"holding a direct-Service deployment composes no Router Workload and claims no Router quota")
	}
	assert.Empty(t, routerPods(t, cli),
		"and no Router pod appears either: nothing here is answered by a Router that does not exist")
}

// retirementRouterBacked makes a deployment one whose ingress CAN be observed, by declaring a
// router and answering its view with the given served members.
//
// It exists because a deployment that declares no router now holds by design: there is no observer
// to ask, and the established direct-Service connections cannot be read from anything this operator
// sees. A test whose subject is the protocol -- the state sequence, the delete, the interception --
// therefore has to be a deployment whose ingress is actually observed, or it would be asserting
// against a path the contract deliberately stops. The no-Router case is not hidden by this: it has
// its own test, which holds, retains and times out.
func retirementRouterBacked(md *workercore.ModelDeployment) *workercore.ModelDeployment {
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}

	return md
}

// retirementRouterFixture makes a deployment one whose ingress is OBSERVED, returning the Router
// pod to seed alongside it.
//
// It is applied at the call sites whose subject is a removal being carried out, not inside every
// retiring helper: a deployment that declares no router holds by design, so a test about a removal
// that should happen has to say the removal can be observed. A test about the hold itself declares
// no router and must keep not declaring one.
func retirementRouterFixture(
	md *workercore.ModelDeployment,
) (*workercore.ModelDeployment, *core.Pod) {
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}

	return md, retirementRouterPod("router-zero", "10.0.9.1")
}

// retirementRouterReconciler wires a reconciler that answers the declared router's view with the
// given served members, resolving addresses against the same Pods the deployment's own listing sees.
func retirementRouterReconciler(
	cli ctrlcli.Client, reader modelDeploymentDrainReader, now time.Time, pods []core.Pod,
	served ...string,
) *ModelDeploymentReconciler {
	r := holdReconciler(cli, reader, now)
	// The Router process this fixture seeded carries its live controlling chain, because the
	// collection verifies every hop of it through the uncached reader before it reads a byte. A
	// fixture that seeded only the Pod measures the hold an unplaceable process produces, so every
	// "no residual" row would be asserting that hold rather than the absence it is about.
	if md := liveModelDeploymentFor(cli); md != nil && md.Spec.Router != nil {
		_ = seedRouterOwnership(cli, md)
	}
	r.servingViewFetch = func(_ context.Context, _ string) ([]byte, error) {
		return retirementRouterView(served, pods), nil
	}

	return r
}

// TestAReservedPodIsHeldEvenWhenItsOrdinalLabelIsGone pins that the reservation's own identity
// decides, not a mutable label on the Pod it holds.
//
// The wire names a target member by UID. The ordinal label is not part of that identity, and a Pod
// that loses it stops being a replica the wire can name while continuing to be the very Pod the
// operation is holding. Removing the ordinal before the reservation is consulted would hand that
// Pod to a path that deletes on the role, taking a member the protocol decided to retain.
func TestAReservedPodIsHeldEvenWhenItsOrdinalLabelIsGone(t *testing.T) {
	md := retirementRouterBacked(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	retained := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	retained.UID = "retained-uid"
	retained.Status.Phase = core.PodRunning
	retained.Status.PodIP = "10.0.5.1"
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	keeper.UID = "keeper-uid"
	keeper.Status.Phase = core.PodRunning
	keeper.Status.PodIP = "10.0.5.2"

	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"retained-uid"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			res.Reason = "the withdrawal budget was exhausted; members, workload and capacity are retained"
		}))
	before := md.Status.Retirement.DeepCopy()

	cli := newModelDeploymentClient(append(
		[]ctrlcli.Object{md, newRenderInstanceType(), retained, keeper}, ownedRouterObjects(md)...)...)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*retained, *keeper})

	plan := r.planModelDeploymentRetirement(context.Background(), md, []core.Pod{*retained, *keeper})
	require.True(t, plan.holds("retained-uid"), "the reservation still holds that Pod by UID")

	// THE LABEL IS REMOVED, not the Pod. Same UID, same object, one mutable label gone.
	stripped := *retained.DeepCopy()
	delete(stripped.Labels, modelDeploymentReplicaOrdinalLabel)
	_, seated := modelDeploymentPodOrdinal(&stripped)
	require.False(t, seated, "the Pod no longer claims an ordinal, so nothing downstream can name it")

	refused := r.refuseModelDeploymentRemoval(
		context.Background(), md, plan, &stripped, []core.Pod{stripped, *keeper}, nil)

	assert.True(t, refused, "a Pod the reservation holds is refused even with no ordinal to name it by")
	after := getModelDeployment(t, cli).Status.Retirement
	require.NotNil(t, after)
	assert.Equal(t, before.State, after.State, "and the retained reservation is untouched")
	assert.Equal(t, before.TargetMemberUIDs, after.TargetMemberUIDs,
		"the retained member UID set is still the one the operation bound")
}

// TestAnUnseatedPodTheReservationDoesNotHoldIsStillNotFrozen is the boundary of the guard above.
//
// The correction refuses a Pod the reservation holds. It must not refuse every Pod that happens to
// be unseated, or the rollout could no longer remove a Pod with no seat -- and a hold with no
// reservation to clear is a replica nothing would ever remove.
func TestAnUnseatedPodTheReservationDoesNotHoldIsStillNotFrozen(t *testing.T) {
	md := retirementRouterBacked(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	// An UNRELATED Pod with no ordinal and no place in the reservation: the rollout's business.
	unseated := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "stray", Namespace: "team-a", UID: "stray-uid",
			Labels: map[string]string{
				modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance: "qwen",
			},
		},
		Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.0.6.9"},
	}
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	keeper.UID = "keeper-uid"
	keeper.Status.Phase = core.PodRunning
	keeper.Status.PodIP = "10.0.6.1"

	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateAborted, "server", 1, []string{"gone-uid"},
		func(res *workercore.ModelDeploymentRetirementStatus) { res.Reason = "aborted and retained" }))

	cli := newModelDeploymentClient(append(
		[]ctrlcli.Object{md, newRenderInstanceType(), unseated, keeper}, ownedRouterObjects(md)...)...)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*unseated, *keeper})
	pods := []core.Pod{*unseated, *keeper}

	plan := r.planModelDeploymentRetirement(context.Background(), md, pods)
	require.False(t, plan.holds("stray-uid"), "the reservation does not hold this Pod")

	refused := r.refuseModelDeploymentRemoval(context.Background(), md, plan, unseated, pods, nil)

	assert.False(t, refused,
		"an unseated Pod outside the reservation is left to the rollout, not frozen by a narrow guard")
}

// TestH382SettlingRequiresReleaseObservation is the original C02 counterexample.
//
// Before the release predicate, Settling completed on the budget check alone: the operation read
// nothing, so a target whose Workload was still present and whose accelerator was still carved
// completed with the reason "its release has been observed". Every case below drives the operation
// to Settling with the target deleted and then checks what the completion actually observed.
func TestH382SettlingRequiresReleaseObservation(t *testing.T) {
	// A workload that is still present, owning the target's member. Nothing has released it, so
	// the operation must not complete.
	t.Run("a workload still present holds the operation", func(t *testing.T) {
		fixture := newRetirementReleaseFixture(t, retirementReleaseFacts{workloadStillPresent: true})

		plan := fixture.planAt(t, workercore.ModelDeploymentRetirementStateSettling)

		// THE STATE MACHINE STEP IS CALLED, NOT THE PREDICATE. Calling the predicate directly would
		// pass whether or not completion consults it, and the whole defect was that it did not.
		fixture.r.advanceModelDeploymentRetirementToCompleted(context.Background(), fixture.md, plan)

		assert.NotEqual(t, workercore.ModelDeploymentRetirementStateCompleted, plan.Reservation.State,
			"a captured workload that is still present has not released the quota, so the operation "+
				"does not complete")
		assert.False(t, plan.Clear, "and it is not cleared either")
		assert.Contains(t, plan.Reservation.Reason, "workload",
			"the reason names what is still holding it")
	})
}

// retirementReleaseFacts are the facts a release case needs to set up, so each case states the
// server state it is about rather than repeating the wiring.
type retirementReleaseFacts struct {
	// workloadStillPresent leaves the captured Workload on the server. The target pod itself is
	// always gone, because Settling is reached only once it is: without that, every case would be
	// asserting the member predicate and none would reach the quota question.
	workloadStillPresent bool
}

// newRetirementReleaseFixture builds a target that has reached the point where its release must be
// observed, and returns the object, the reconciler and the held set the predicates read.
func newRetirementReleaseFixture(
	t *testing.T, facts retirementReleaseFacts,
) *modelDeploymentRetirementReleaseFixture {
	t.Helper()

	md, router := retirementRouterFixture(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	target := surplusReplicaAt(t, md, "qwen-server-one", 1, "")
	target.UID = "member-1"
	target.Status.Phase = core.PodRunning
	target.Status.PodIP = "10.0.7.1"
	keeper := surplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	keeper.UID = "keeper"
	keeper.Status.Phase = core.PodRunning
	keeper.Status.PodIP = "10.0.7.2"

	workload := admittedReplicaWorkload(target, true)
	cli := newModelDeploymentClient(
		md, newRenderInstanceType(), target, keeper, router, workload)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*target, *keeper})

	// THE CAPTURE HAPPENS WHILE THE TARGET IS STILL THERE, which is the whole point of capturing in
	// Draining. The reservation is written first so the record can bind to a real operation, then
	// the target is deleted, which is the state the protocol is in by the time it settles. The
	// Workload is what is left un-released for this case to be about.
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			started := time.Now()
			res.StartedAt = meta.NewTime(started)
			res.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(started)
			res.TargetWorkloadUID = string(workload.UID)
		}))

	fixture := &modelDeploymentRetirementReleaseFixture{
		t: t, md: md, r: r, cli: cli, workloadUID: string(workload.UID),
		held: sets.New[types.UID]("member-1"),
	}
	capturePlan := &modelDeploymentRetirementPlan{Reservation: md.Status.Retirement, held: fixture.held}
	capturePlan.Hold = true
	record, err := r.captureModelDeploymentRetirementRelease(context.Background(), md, capturePlan)
	require.NoError(t, err, "the target is still present, so it can still be captured")
	require.NoError(t, r.persistModelDeploymentRetirementRelease(context.Background(), md, record))
	fixture.md = md

	require.NoError(t, cli.Delete(context.Background(), target))
	if !facts.workloadStillPresent {
		require.NoError(t, cli.Delete(context.Background(), workload))
	}

	return fixture
}

type modelDeploymentRetirementReleaseFixture struct {
	t           *testing.T
	md          *workercore.ModelDeployment
	r           *ModelDeploymentReconciler
	cli         ctrlcli.Client
	workloadUID string
	held        sets.Set[types.UID]
	// started and retry are the operation's identity. They are carried across phase changes
	// because a phase change is neither a new operation nor a new attempt.
	started time.Time
	retry   string
}

// planAt builds the plan the predicates read at one state, with the captured record already
// written, so each case is about the observation rather than about reaching the state.
func (f *modelDeploymentRetirementReleaseFixture) planAt(
	t *testing.T, state workercore.ModelDeploymentRetirementState,
) *modelDeploymentRetirementPlan {
	t.Helper()

	// The SAME start time is carried across phase changes. Restamping it here would make each phase
	// a different operation, and a record bound to the previous one would be refused for a reason
	// that has nothing to do with what the case is about. The two attempts can still share a
	// serialized second, which is what the retry token is bound with.
	started := f.started
	if started.IsZero() {
		started = time.Now()
		f.started = started
	}
	f.md = reserve(f.md, retirementReservation(
		state, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			res.StartedAt = meta.NewTime(started)
			res.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(time.Now())
			res.LastConsumedRetryToken = f.retry
			res.TargetWorkloadUID = f.workloadUID
		}))
	// The reservation is PERSISTED and read back. The release observation reads the operation from
	// the server, so a fixture that keeps it only in this object would be asserting against a
	// reservation the observation cannot see.
	require.NoError(t, f.cli.Status().Update(context.Background(), f.md))
	key := ctrlcli.ObjectKeyFromObject(f.md)
	f.md = new(workercore.ModelDeployment)
	require.NoError(t, f.cli.Get(context.Background(), key, f.md))

	pods := new(core.PodList)
	require.NoError(t, f.cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	members := make([]core.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].Labels[modelDeploymentReplicaOrdinalLabel] == "1" {
			members = append(members, pods.Items[i])
		}
	}

	plan := &modelDeploymentRetirementPlan{
		Reservation: f.md.Status.Retirement,
		held:        f.held,
		Deletes:     members,
	}
	plan.Hold = true

	return plan
}
