// Whole-group health qualification: the predicate table.
//
// One behavior per case, and the cases are declarative: each names the shape it builds, the
// verdict it expects and the acceptance it serves, and a single loop executes them. The table
// exists because the combination rule has a shape a prose rule describes badly -- three-valued
// legs where Failed and Unknown both refuse eligibility but only Failed may act -- and the only
// way that is credible is a reader can see each of those cases fail when its own rule is broken.
package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// healthPod builds one member of one replica, ready or not. A nil ready means the member has
// reported nothing at all, which is the state a just-created Pod is in and is not the same as
// having reported not-ready.
func healthPod(role string, ordinal, member int, ready *bool, uid string) core.Pod {
	pod := core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      role + "-" + strconvx.Itoa(ordinal) + "-" + strconvx.Itoa(member),
			Namespace: "team-a",
			UID:       types.UID(uid),
			Labels: map[string]string{
				modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:    "qwen",
				modelDeploymentLabelKeyComponent:   role,
				modelDeploymentReplicaOrdinalLabel: strconvx.Itoa(ordinal),
				modelDeploymentMemberIndexLabel:    strconvx.Itoa(member),
			},
		},
	}
	if ready != nil {
		status := core.ConditionFalse
		if *ready {
			status = core.ConditionTrue
		}
		pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: status}}
	}

	return pod
}

func healthBool(v bool) *bool { return &v }

// healthDeployment builds a deployment of one engine-shaped role of the given replica size, and
// optionally a second role, which is what the P/D shape needs.
func healthDeployment(replicaSize int32, extraRoles ...string) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{
				Name: workercore.ModelDeploymentEngineVLLM, Version: "0.25.1",
			},
			Roles: []workercore.ModelDeploymentRole{{
				Name: "server", Replicas: 1, ReplicaSize: replicaSize, InstanceType: "h20-8x",
			}},
		},
	}
	for _, name := range extraRoles {
		md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
			Name: name, Replicas: 1, ReplicaSize: replicaSize, InstanceType: "h20-8x",
		})
	}

	return md
}

// healthPending builds the replacement record the pass would carry for the named ordinals.
func healthPending(role string, ordinals ...int) modelDeploymentPendingReplacement {
	byOrdinal := make(map[int]bool, len(ordinals))
	for _, ordinal := range ordinals {
		byOrdinal[ordinal] = true
	}

	return modelDeploymentPendingReplacement{ordinals: map[string]map[int]bool{role: byOrdinal}}
}

// TestWholeGroupHealthPredicate is the case table for the whole-group predicate. Each case states
// one behavior and the acceptance it serves; the loop below is the only execution.
func TestWholeGroupHealthPredicate(t *testing.T) {
	testCases := []struct {
		name string
		// md is the deployment under test; nil builds the default single-member shape.
		md *workercore.ModelDeployment
		// pods are the live members; nil builds one ready single-member replica of "server".
		pods    []core.Pod
		held    []core.Pod
		pending modelDeploymentPendingReplacement
		retire  *workercore.ModelDeploymentRetirementStatus

		// wantNoQualification states that the predicate declines to answer about this replica at
		// all, which is what a departing role earns.
		wantNoQualification bool
		// wantEligible is the whole-group answer the eligibility write asks.
		wantEligible bool
		// wantFailure, when set, pins whether a DEFINITE fault is reported. It is what separates
		// a revocation from a hold on a replica carrying both kinds of leg, so it is asserted
		// separately from wantEligible: a replica that is not eligible for either reason looks
		// identical without it.
		wantFailure   *bool
		wantActivated bool
		// wantLeg, when set, must be this leg's verdict.
		wantLeg  modelDeploymentQualificationLegName
		wantVect modelDeploymentLegVerdict
		// wantHeldLegs, when non-nil, is the exact sorted set of unverified legs.
		wantHeldLegs []modelDeploymentQualificationLegName
		// wantObserved, when set, pins whether the deployment has made an observation.
		wantObserved *bool
		// wantGeneration, when set, must equal the derived membership generation.
		wantGeneration modelDeploymentMembershipGeneration
	}{
		{
			name:         "a complete ready single-member replica qualifies",
			pods:         []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			wantEligible: true, wantActivated: true,
		},
		{
			name:         "a replica short of its declared members fails the completeness leg",
			md:           healthDeployment(2),
			pods:         []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			wantEligible: false, wantActivated: true,
			wantLeg: modelDeploymentLegMemberSetComplete, wantVect: modelDeploymentLegFailed,
		},
		{
			name: "one member not ready fails the whole replica",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(false), "uid-b"),
			},
			// Not activated, and not because of the fault: a replica of two members has no group
			// observation at all, so the restore half is closed whatever its members report. The
			// fault is still recorded, and withdrawal does not need activation to act.
			wantEligible: false, wantActivated: false,
			wantLeg: modelDeploymentLegMembersReady, wantVect: modelDeploymentLegFailed,
		},
		{
			name: "a multi-member replica is held unsupported with no group observation",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: false, wantActivated: false,
			wantLeg: modelDeploymentLegGroupForward, wantVect: modelDeploymentLegUnknown,
		},
		{
			name: "a held instance names the leg that is unverified",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: false, wantActivated: false,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegGroupForward},
		},
		{
			name:         "a single-member replica has no group predicate to hold it",
			pods:         []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			wantEligible: true, wantActivated: true,
			wantLeg: modelDeploymentLegGroupForward, wantVect: modelDeploymentLegVerified,
		},
		{
			name: "an external data parallel replica of two is held like any other",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: false, wantActivated: false,
			wantLeg: modelDeploymentLegGroupForward, wantVect: modelDeploymentLegUnknown,
		},
		{
			name: "a prefill and decode pair are judged as their own replicas",
			md:   healthDeployment(2, "decode"),
			pods: []core.Pod{
				healthPod("prefill", 0, 0, healthBool(true), "uid-p0"),
				healthPod("prefill", 0, 1, healthBool(true), "uid-p1"),
				healthPod("decode", 0, 0, healthBool(true), "uid-d0"),
				healthPod("decode", 0, 1, healthBool(true), "uid-d1"),
			},
			wantEligible: false, wantActivated: false,
		},
		{
			name: "a member replaced with a new uid changes the generation",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-replacement"),
			},
			wantEligible: false, wantActivated: false,
			wantGeneration: "uid-a/uid-replacement",
		},
		{
			name: "the same members in another order are the same generation",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
			},
			wantEligible: false, wantActivated: false,
			wantGeneration: "uid-a/uid-b",
		},
		{
			name: "a reservation naming this replica fails the retirement leg",
			md:   healthDeployment(1),
			pods: []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			retire: &workercore.ModelDeploymentRetirementStatus{
				RoleName: "server", ReplicaOrdinal: 0,
				State: workercore.ModelDeploymentRetirementStateAdmitted,
			},
			wantEligible: false, wantActivated: true,
			wantLeg:      modelDeploymentLegNoPlannedRetirement,
			wantVect:     modelDeploymentLegFailed,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegNoPlannedRetirement},
		},
		{
			name: "a reservation for another role does not block this one",
			md:   healthDeployment(1),
			pods: []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			retire: &workercore.ModelDeploymentRetirementStatus{
				RoleName: "other", ReplicaOrdinal: 3,
				State: workercore.ModelDeploymentRetirementStateAdmitted,
			},
			wantEligible: true, wantActivated: true,
			wantLeg: modelDeploymentLegNoPlannedRetirement, wantVect: modelDeploymentLegVerified,
		},
		{
			name:         "a replica condemned for replacement fails that leg",
			md:           healthDeployment(1),
			pods:         []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			pending:      healthPending("server", 0),
			wantEligible: false, wantActivated: true,
			wantLeg:      modelDeploymentLegNoPendingReplacement,
			wantVect:     modelDeploymentLegFailed,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegNoPendingReplacement},
		},
		{
			name:         "a replica this pass created has observed nothing",
			md:           healthDeployment(1),
			pods:         []core.Pod{healthPod("server", 0, 0, nil, "uid-a")},
			wantEligible: false, wantActivated: true,
			wantLeg:      modelDeploymentLegMembersReady,
			wantVect:     modelDeploymentLegUnknown,
			wantObserved: healthBool(false),
		},
		{
			name:         "a replica whose members reported nothing is not a health fault",
			md:           healthDeployment(1),
			pods:         []core.Pod{healthPod("server", 0, 0, nil, "uid-a")},
			wantEligible: false, wantActivated: true,
			wantLeg:      modelDeploymentLegMembersReady,
			wantVect:     modelDeploymentLegUnknown,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegMembersReady},
		},
		{
			name:         "a member explicitly reporting not ready is a fault, unlike silence",
			md:           healthDeployment(1),
			pods:         []core.Pod{healthPod("server", 0, 0, healthBool(false), "uid-a")},
			wantEligible: false, wantActivated: true,
			wantLeg:      modelDeploymentLegMembersReady,
			wantVect:     modelDeploymentLegFailed,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegMembersReady},
		},
		{
			name:                "a replica of a role the spec no longer names qualifies for nothing",
			md:                  healthDeployment(1),
			pods:                []core.Pod{healthPod("departed", 0, 0, healthBool(true), "uid-a")},
			wantNoQualification: true,
		},
		{
			name: "a failed leg outranks an unknown one and still revokes",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(false), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: false, wantActivated: false,
			// The replica carries a Failed readiness leg AND an Unknown group-forward leg at the
			// same time, which is the only way to tell the two apart by what the predicate reports.
			wantFailure: healthBool(true),
			wantLeg:     modelDeploymentLegMembersReady,
			wantVect:    modelDeploymentLegFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := tc.md
			if md == nil {
				md = healthDeployment(1)
			}
			if tc.retire != nil {
				md.Status.Retirement = tc.retire
			}
			pods := tc.pods
			if pods == nil {
				pods = []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")}
			}

			qualifications := qualifyModelDeploymentInstances(md, pods, tc.pending)
			if tc.wantNoQualification {
				assert.Empty(t, qualifications,
					"a replica the predicate declines to answer about produces no qualification")

				return
			}
			require.NotEmpty(t, qualifications, "every named replica produced a qualification")

			for _, q := range qualifications {
				assert.Equal(t, tc.wantEligible, q.Eligible(),
					"the whole-group answer the eligibility write asks")
				if tc.wantFailure != nil {
					assert.Equal(t, *tc.wantFailure, q.HasFailure(),
						"a definite fault is reported even when an unverified leg sits alongside it")
				}
				assert.Equal(t, tc.wantActivated, q.Activated(),
					"whether the restore half may run")

				if tc.wantLeg != "" {
					assert.Equal(t, tc.wantVect, q.Leg(tc.wantLeg).Verdict,
						"leg %s", tc.wantLeg)
				}
				if tc.wantHeldLegs != nil {
					assert.Equal(t, tc.wantHeldLegs, q.HeldLegs(),
						"the reported unverified legs, in a stable order")
				}
				if tc.wantObserved != nil {
					assert.Equal(t, *tc.wantObserved, q.Observed,
						"whether this replica has made any readiness observation")
				}
				if tc.wantGeneration != "" {
					assert.Equal(t, tc.wantGeneration, q.Generation,
						"the derived membership generation")
				}
			}
		})
	}
}

// TestMembershipGenerationIsDerivedNotStored pins the two properties the generation is chosen
// for: it is a pure function of the member UID set, and it does not depend on the order the
// members happen to be listed in.
func TestMembershipGenerationIsDerivedNotStored(t *testing.T) {
	first := modelDeploymentReplicaView{Members: []*core.Pod{
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a")}},
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-b")}},
	}}
	second := modelDeploymentReplicaView{Members: []*core.Pod{
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-b")}},
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a")}},
	}}
	replaced := modelDeploymentReplicaView{Members: []*core.Pod{
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a")}},
		{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-c")}},
	}}

	assert.Equal(t, modelDeploymentGenerationOf(first), modelDeploymentGenerationOf(second),
		"the same members in another order are the same generation")
	assert.NotEqual(t, modelDeploymentGenerationOf(first), modelDeploymentGenerationOf(replaced),
		"a replaced member is a different membership, so a prior verification does not carry over")
}

// TestQualifiedCountIsNilUnlessTheListIsComplete pins the one distinction the wire makes: a count
// this operator cannot stand behind is absent, and an observed empty set is an explicit zero.
func TestQualifiedCountIsNilUnlessTheListIsComplete(t *testing.T) {
	md := healthDeployment(1)
	ready := healthPod("server", 0, 0, healthBool(true), "uid-a")
	qualified := qualifyModelDeploymentInstances(
		md, []core.Pod{ready}, modelDeploymentPendingReplacement{},
	)
	counts := modelDeploymentRoleQualified(qualified)
	require.Contains(t, counts, "server",
		"a complete qualified list carries its count")
	require.NotNil(t, counts["server"])
	assert.Equal(t, int32(1), *counts["server"])

	// A second replica of the same role that is held makes the role's list incomplete, so the
	// count goes absent rather than reporting the one replica that did qualify.
	unobserved := healthPod("server", 1, 0, nil, "uid-b")
	partial := qualifyModelDeploymentInstances(
		md, []core.Pod{ready, unobserved}, modelDeploymentPendingReplacement{},
	)
	partialCounts := modelDeploymentRoleQualified(partial)
	assert.NotContains(t, partialCounts, "server",
		"a role with a held replica reports no count at all rather than a partial one")
}

// TestEndpointEligibilityStatusMapping is the case table for what the predicate publishes. Every
// case states the whole-group situation and the exact status it must produce, because the wire
// carries one distinction twice -- a nil count and an Unknown condition against an explicit zero
// and a False one -- and publishing the wrong member of that pair is how a healthy deployment
// reads as a blackholed one.
func TestEndpointEligibilityStatusMapping(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		pods []core.Pod
		// pending is the replacement record the pass carries, for the held-by-replacement case.
		pending modelDeploymentPendingReplacement

		wantStatus string
		wantReason string
		// wantEligible is what the role's count field must read; nil means the field is absent.
		wantEligible *int32
		// wantServing is the serving state T7 must not disturb.
		wantServing workercore.ModelDeploymentServingState
	}{
		{
			name: "a held multi-member replica reports unknown unsupported and no count",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantStatus: "Unknown", wantReason: modelDeploymentReasonEndpointsUnsupported,
			wantEligible: nil, wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name:       "a revoked replica reports false revoked and an explicit zero",
			md:         healthDeployment(1),
			pods:       []core.Pod{healthPod("server", 0, 0, healthBool(false), "uid-a")},
			wantStatus: "False", wantReason: modelDeploymentReasonEndpointsRevoked,
			wantEligible: healthCount(int32(0)), wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name:       "a qualified single-member replica reports true and its count",
			md:         healthDeployment(1),
			pods:       []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")},
			wantStatus: "True", wantReason: modelDeploymentReasonEndpointsQualified,
			wantEligible: healthCount(int32(1)), wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name:       "a deployment whose replicas have reported nothing is not observed at all",
			md:         healthDeployment(1),
			pods:       []core.Pod{healthPod("server", 0, 0, nil, "uid-a")},
			wantStatus: "Unknown", wantReason: modelDeploymentReasonNotObserved,
			wantEligible: nil, wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name: "one revoked replica does not hide a qualified sibling from the count",
			md:   healthDeployment(1),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 1, 0, healthBool(false), "uid-b"),
			},
			wantStatus: "False", wantReason: modelDeploymentReasonEndpointsRevoked,
			wantEligible: healthCount(int32(1)), wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := tc.md
			if md == nil {
				md = healthDeployment(1)
			}

			qualifications := qualifyModelDeploymentInstances(
				md, tc.pods, tc.pending,
			)
			holder := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: md.Name}}
			observeModelDeploymentEndpointEligibility(holder, qualifications)

			assert.Equal(t, tc.wantStatus,
				ModelDeploymentConditionEndpointEligibility.GetStatus(holder),
				"the deployment-level verdict")
			assert.Equal(t, tc.wantReason,
				ModelDeploymentConditionEndpointEligibility.GetReason(holder),
				"the machine-readable class of that verdict")

			roles := modelDeploymentRoleStatuses(
				md, tc.pods, nil, modelDeploymentRoleQualified(qualifications),
			)
			require.Len(t, roles, 1)
			assert.Equal(t, tc.wantEligible, roles[0].Endpoints.Eligible,
				"nil is unobserved and an explicit zero is an observed empty set")
			assert.Equal(t, tc.wantServing, roles[0].Endpoints.Serving.State,
				"T7 leaves the serving state exactly as the router answer set it")
		})
	}
}

func healthCount[T any](v T) *T { return &v }

// TestAnUnverifiedLegIsAHoldAndNotARevocation pins the mapping from an unverified leg to the
// HeldUnverified reason.
//
// The qualification is built here rather than by the evaluator because no engine shape produces
// this state today: a multi-member replica is Unsupported, which has its own reason, and a
// single-member one is NotApplicable. The branch is the landing place for an engine group-forward
// observation that cannot be verified, so its mapping is pinned while it has no producer, and the
// pending-replacement fixture it replaces is left to assert revocation, which is a Failed leg.
func TestAnUnverifiedLegIsAHoldAndNotARevocation(t *testing.T) {
	qualifications := []modelDeploymentInstanceQualification{
		{
			View: modelDeploymentReplicaView{Role: "server", Ordinal: 0},
			GroupForward: modelDeploymentGroupForward{
				State: modelDeploymentGroupForwardUnknown,
			},
			Observed: true,
			Legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegUnknown},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
		},
	}

	holder := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: "held"}}
	observeModelDeploymentEndpointEligibility(holder, qualifications)

	assert.Equal(t, "Unknown",
		ModelDeploymentConditionEndpointEligibility.GetStatus(holder),
		"an unverified leg is an open question, not a fault")
	assert.Equal(t, modelDeploymentReasonEndpointsHeldUnverified,
		ModelDeploymentConditionEndpointEligibility.GetReason(holder),
		"the hold names its class and is not reported as a revocation")
	assert.NotContains(t, modelDeploymentRoleQualified(qualifications), "server",
		"a held replica contributes no count, so the role's count stays nil")
}
