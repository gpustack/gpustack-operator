// Whole-group health qualification: the predicate table.
//
// One behavior per case, and the cases are declarative: each names the shape it builds, the
// verdict it expects and the acceptance it serves, and a single loop executes them. The table
// exists because the combination rule has a shape a prose rule describes badly -- three-valued
// legs where Failed and Unknown both refuse eligibility but only Failed may act -- and the only
// way that is credible is a reader can see each of those cases fail when its own rule is broken.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

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
			// The address and the serving annotations the render stamps, because a member with no
			// address is a member the group-forward probe cannot ask, and a case about some OTHER
			// leg would then be reading a hold it never meant to arrange.
			Annotations: map[string]string{
				"prometheus.io/port":   "8000",
				"prometheus.io/scheme": "http",
			},
		},
		Status: core.PodStatus{
			PodIP: "10.0." + strconvx.Itoa(ordinal) + "." + strconvx.Itoa(1+member),
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

// healthRunningContainer builds a container status kubelet reports as running, carrying the
// Started answer it has made so far. A nil started is kubelet having made no answer, which is
// the state a container sits in while its startupProbe is still running.
func healthRunningContainer(started *bool) core.ContainerStatus {
	status := core.ContainerStatus{
		State: core.ContainerState{Running: &core.ContainerStateRunning{}},
	}
	if started != nil {
		status.Started = started
	}

	return status
}

// healthWaitingContainer builds a container status kubelet reports as stuck waiting in the
// named state.
func healthWaitingContainer(reason string) core.ContainerStatus {
	return core.ContainerStatus{
		State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: reason}},
	}
}

// healthStartingPod stamps a running phase and one main container onto a member, so a case can
// place the replica inside its startup window. Which container kubelet reports is data: a
// running one kubelet has not marked Started is the state a large model compiles graphs in.
func healthStartingPod(
	role string, ordinal, member int, ready *bool, container core.ContainerStatus, uid string,
) core.Pod {
	pod := healthPod(role, ordinal, member, ready, uid)
	pod.Status.Phase = core.PodRunning
	pod.Status.ContainerStatuses = []core.ContainerStatus{container}

	return pod
}

// stampHealthGroupTotals writes onto every member the Kueue group total a rendered replica always
// carries, taken from the size of the role it belongs to.
//
// THE STAMP IS WHAT MAKES THESE FIXTURES HONEST RATHER THAN MERELY PASSING. The completeness leg
// reads a replica's deployed size from its members rather than from the role, so a Pod without the
// annotation reads as a replica of one -- and every case here below would then be judging a
// multi-member replica against a total of one. healthPod is given the total it was rendered with by
// this call rather than by an argument, because the size belongs to the deployment and a case that
// wants a replica built at a DIFFERENT size than the role declares -- which is the state this whole
// change exists for -- stamps it by hand and is explicit about doing so.
func stampHealthGroupTotals(md *workercore.ModelDeployment, pods []core.Pod) []core.Pod {
	sizes := make(map[string]int, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		sizes[md.Spec.Roles[i].Name] = modelDeploymentRoleSize(&md.Spec.Roles[i])
	}

	stamped := make([]core.Pod, len(pods))
	for i := range pods {
		stamped[i] = *pods[i].DeepCopy()
		total := sizes[modelDeploymentPodRole(&stamped[i])]
		if total < 1 {
			total = 1
		}
		if stamped[i].Annotations == nil {
			stamped[i].Annotations = map[string]string{}
		}
		stamped[i].Annotations[kueuepodconst.GroupTotalCountAnnotation] = strconvx.Itoa(total)
	}

	return stamped
}

// healthDeployment builds a deployment of one engine-shaped role of the given replica size, and
// optionally a second role, which is what the P/D shape needs.
func healthDeployment(replicaSize int32, extraRoles ...string) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{
				Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:v0.25.1",
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

		// kinds sets the role kinds by role name, so a case can be a genuine prefill/decode shape
		// rather than two roles that merely happen to be named after one.
		kinds map[string]workercore.ModelDeploymentRoleKind
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
			name: "one member not ready fails the whole replica, and no probe is spent on it",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(false), "uid-b"),
			},
			// The readiness fault alone condemns the replica, so no live probe is fired for it:
			// the group leg reads Unknown as an absence of evidence, and the definite fault is
			// the only thing standing between this replica and selection.
			wantEligible: false, wantActivated: false,
			wantLeg: modelDeploymentLegMembersReady, wantVect: modelDeploymentLegFailed,
		},
		{
			name: "a multi-member replica whose members all carried the probe qualifies",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: true, wantActivated: true,
			wantLeg: modelDeploymentLegGroupForward, wantVect: modelDeploymentLegVerified,
		},
		{
			// The hold is now a statement about a shape no probe can reach, not about every
			// multi-member replica, and the leg is still named so a reader is told WHICH
			// condition the group failed.
			name:  "a held instance names the leg that is unverified",
			md:    healthDeployment(2),
			kinds: map[string]workercore.ModelDeploymentRoleKind{"server": workercore.ModelDeploymentRoleKindPrefill},
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
			name: "an external data parallel replica of two is verified like any other",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: true, wantActivated: true,
			wantLeg: modelDeploymentLegGroupForward, wantVect: modelDeploymentLegVerified,
		},
		{
			// A prefill/decode pair is judged as its own replicas and is held on the group leg,
			// because a probe of one role cannot reach across to the other.
			name: "a prefill and decode pair are judged as their own replicas and held",
			md:   healthDeployment(2, "decode"),
			kinds: map[string]workercore.ModelDeploymentRoleKind{
				"prefill": workercore.ModelDeploymentRoleKindPrefill,
				"decode":  workercore.ModelDeploymentRoleKindDecode,
			},
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
			// Eligible now that the group leg is verified: the case is about the GENERATION, and a
			// replaced member changes the generation without changing the answer, which is the
			// property that makes the generation worth deriving.
			wantEligible: true, wantActivated: true,
			wantGeneration: "uid-a/uid-replacement",
		},
		{
			name: "the same members in another order are the same generation",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
			},
			wantEligible: true, wantActivated: true,
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
			kinds: map[string]workercore.ModelDeploymentRoleKind{
				"server": workercore.ModelDeploymentRoleKindDecode,
			},
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(false), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			},
			wantEligible: false, wantActivated: false,
			// The replica carries a Failed readiness leg AND an Unknown group-forward leg at the
			// same time, which is the only way to tell the two apart by what the predicate reports.
			// The group leg is Unknown because a disaggregated role has no observation, not because
			// a probe was left out: the fixture states which.
			wantFailure: healthBool(true),
			wantLeg:     modelDeploymentLegMembersReady,
			wantVect:    modelDeploymentLegFailed,
		},
		{
			// The replica is inside its startupProbe window: the container is up, kubelet has
			// made no Started answer, and readiness is still false. That is an open question, not
			// a fault, so the replica holds instead of revoking.
			name: "a replica mid-startup holds instead of revoking",
			pods: []core.Pod{
				healthStartingPod("server", 0, 0,
					healthBool(false), healthRunningContainer(healthBool(false)), "uid-a"),
			},
			wantEligible: false, wantActivated: true,
			wantFailure:  healthBool(false),
			wantLeg:      modelDeploymentLegMembersReady,
			wantVect:     modelDeploymentLegUnknown,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegMembersReady},
			wantObserved: healthBool(false),
		},
		{
			// Once kubelet has marked the container Started, a false readiness answer is the
			// definite fault it always was, so the startup hold must not swallow it.
			name: "a replica whose startup succeeded but readiness is false is revoked as before",
			pods: []core.Pod{
				healthStartingPod("server", 0, 0,
					healthBool(false), healthRunningContainer(healthBool(true)), "uid-a"),
			},
			wantEligible: false, wantActivated: true,
			wantFailure: healthBool(true),
			wantLeg:     modelDeploymentLegMembersReady,
			wantVect:    modelDeploymentLegFailed,
		},
		{
			name: "a member stuck in CrashLoopBackOff fails the replica during startup",
			pods: []core.Pod{
				healthStartingPod("server", 0, 0,
					nil, healthWaitingContainer("CrashLoopBackOff"), "uid-a"),
			},
			wantEligible: false, wantActivated: true,
			wantFailure: healthBool(true),
			wantLeg:     modelDeploymentLegMembersReady,
			wantVect:    modelDeploymentLegFailed,
		},
		{
			// The multi-member rule: one ready member is not the whole group while a sibling is
			// still starting, so the readiness leg holds rather than reporting every member ready.
			name: "a replica with one member ready and a sibling still starting holds",
			md:   healthDeployment(2),
			pods: []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthStartingPod("server", 0, 1,
					healthBool(false), healthRunningContainer(healthBool(false)), "uid-b"),
			},
			wantEligible: false, wantActivated: true,
			wantFailure:  healthBool(false),
			wantLeg:      modelDeploymentLegMembersReady,
			wantVect:     modelDeploymentLegUnknown,
			wantHeldLegs: []modelDeploymentQualificationLegName{modelDeploymentLegMembersReady},
			wantObserved: healthBool(true),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := tc.md
			if md == nil {
				md = healthDeployment(1)
			}
			for name, kind := range tc.kinds {
				for i := range md.Spec.Roles {
					if md.Spec.Roles[i].Name == name {
						md.Spec.Roles[i].Kind = kind
					}
				}
			}
			if tc.retire != nil {
				md.Status.Retirement = tc.retire
			}
			for name, kind := range tc.kinds {
				for i := range md.Spec.Roles {
					if md.Spec.Roles[i].Name == name {
						md.Spec.Roles[i].Kind = kind
					}
				}
			}
			pods := tc.pods
			if pods == nil {
				pods = []core.Pod{healthPod("server", 0, 0, healthBool(true), "uid-a")}
			}
			pods = stampHealthGroupTotals(md, pods)

			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, tc.pending, boundProbeFetch,
			)
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

// TestModelDeploymentMemberReadinessStartupHold is the case table for one member's readiness
// around startup. The distinctions it pins: a running container kubelet has not marked Started
// is a hold and not a fault, a container stuck in a bad waiting reason is a fault even before
// any readiness report, and a Started container whose readiness is still false remains the
// fault it always was.
func TestModelDeploymentMemberReadinessStartupHold(t *testing.T) {
	testCases := []struct {
		name string
		// phase is the pod phase; the zero value is the phase a just-created Pod carries.
		phase core.PodPhase
		// ready is the PodReady condition answer; nil means the condition is absent.
		ready *bool
		// containers and initContainers are the statuses kubelet has reported.
		containers     []core.ContainerStatus
		initContainers []core.ContainerStatus
		want           modelDeploymentLegVerdict
	}{
		{
			name:  "a running container kubelet has not marked started holds instead of failing",
			phase: core.PodRunning, ready: healthBool(false),
			containers: []core.ContainerStatus{healthRunningContainer(healthBool(false))},
			want:       modelDeploymentLegUnknown,
		},
		{
			name:  "a running container with no started answer holds too",
			phase: core.PodRunning, ready: healthBool(false),
			containers: []core.ContainerStatus{healthRunningContainer(nil)},
			want:       modelDeploymentLegUnknown,
		},
		{
			name:  "a started container whose readiness is false is a fault as before",
			phase: core.PodRunning, ready: healthBool(false),
			containers: []core.ContainerStatus{healthRunningContainer(healthBool(true))},
			want:       modelDeploymentLegFailed,
		},
		{
			name:  "a container in CrashLoopBackOff is a fault before any readiness report",
			phase: core.PodRunning,
			containers: []core.ContainerStatus{
				healthWaitingContainer("CrashLoopBackOff"),
			},
			want: modelDeploymentLegFailed,
		},
		{
			name:  "an image pull backoff is a fault too",
			phase: core.PodPending,
			containers: []core.ContainerStatus{
				healthWaitingContainer("ImagePullBackOff"),
			},
			want: modelDeploymentLegFailed,
		},
		{
			name:  "a stuck init container is a fault whatever the main container is doing",
			phase: core.PodRunning,
			initContainers: []core.ContainerStatus{
				healthWaitingContainer("CreateContainerConfigError"),
			},
			containers: []core.ContainerStatus{healthRunningContainer(healthBool(true))},
			want:       modelDeploymentLegFailed,
		},
		{
			// A ready pod carries the authoritative answer, so a waiting entry in its status
			// arrays must not overturn it.
			name:  "a ready pod is verified whatever its container history says",
			phase: core.PodRunning, ready: healthBool(true),
			containers: []core.ContainerStatus{healthWaitingContainer("CrashLoopBackOff")},
			want:       modelDeploymentLegVerified,
		},
		{
			// A waiting state that is not a definite fault is the ordinary pull-and-create
			// window, and stays a hold.
			name:  "a container still being created is not a fault",
			phase: core.PodPending,
			containers: []core.ContainerStatus{
				healthWaitingContainer("ContainerCreating"),
			},
			want: modelDeploymentLegUnknown,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := core.Pod{Status: core.PodStatus{
				Phase:                 tc.phase,
				InitContainerStatuses: tc.initContainers,
				ContainerStatuses:     tc.containers,
			}}
			if tc.ready != nil {
				status := core.ConditionFalse
				if *tc.ready {
					status = core.ConditionTrue
				}
				pod.Status.Conditions = []core.PodCondition{
					{Type: core.PodReady, Status: status},
				}
			}

			assert.Equal(t, tc.want, modelDeploymentMemberReadiness(&pod), tc.name)
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
		context.Background(), md, []core.Pod{ready}, modelDeploymentPendingReplacement{}, nil,
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
		context.Background(), md, []core.Pod{ready, unobserved}, modelDeploymentPendingReplacement{}, nil,
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
		// kinds sets the role kinds by role name, so the held case is a shape that has no
		// observation at all rather than one that merely had nobody to ask.
		kinds map[string]workercore.ModelDeploymentRoleKind

		wantStatus string
		wantReason string
		// wantEligible is what the role's count field must read; nil means the field is absent.
		wantEligible *int32
		// wantServing is the serving state T7 must not disturb.
		wantServing workercore.ModelDeploymentServingState
	}{
		{
			// A disaggregated role is held for want of any observation, which is the one hold whose
			// count must be absent rather than a partial one: nothing here knows whether either
			// replica would qualify, so it publishes nothing it can stand behind.
			name: "a held multi-member replica reports unknown unsupported and no count",
			md:   healthDeployment(2),
			kinds: map[string]workercore.ModelDeploymentRoleKind{
				"server": workercore.ModelDeploymentRoleKindPrefill,
			},
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
		{
			// A replica mid-startup has made no readiness answer this operator may call a fault,
			// so the deployment reports an open question at both levels and never a revocation.
			name: "a replica mid-startup is never reported revoked",
			pods: []core.Pod{
				healthStartingPod("server", 0, 0,
					healthBool(false), healthRunningContainer(healthBool(false)), "uid-a"),
			},
			wantStatus: "Unknown", wantReason: modelDeploymentReasonNotObserved,
			wantEligible: nil, wantServing: workercore.ModelDeploymentServingStateNotConfigured,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := tc.md
			if md == nil {
				md = healthDeployment(1)
			}
			for name, kind := range tc.kinds {
				for i := range md.Spec.Roles {
					if md.Spec.Roles[i].Name == name {
						md.Spec.Roles[i].Kind = kind
					}
				}
			}

			pods := stampHealthGroupTotals(md, tc.pods)
			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, tc.pending, boundProbeFetch,
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
				md, pods, nil, modelDeploymentRoleQualified(qualifications),
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

// TestACondemnedReplicaIsNeverProbed pins the cheap-legs-first ordering at the real call site:
// a replica already condemned by a definite cache-carried leg must reach the probe transport
// zero times, on any engine shape the probe would otherwise serve. A one-token live request
// against a replica whose fault is already certain spends engine tokens on an answer that
// cannot soften the fault.
func TestACondemnedReplicaIsNeverProbed(t *testing.T) {
	calls := 0
	countingFetch := func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		calls++
		return boundProbeFetch(context.Background(), "", nil)
	}

	t.Run("an unready member condemns the replica before any probe", func(t *testing.T) {
		calls = 0
		md := healthDeployment(2)
		pods := []core.Pod{
			healthPod("server", 0, 0, healthBool(true), "uid-a"),
			healthPod("server", 0, 1, healthBool(false), "uid-b"),
		}
		qualifications := qualifyModelDeploymentInstances(
			context.Background(), md, pods,
			modelDeploymentPendingReplacement{}, countingFetch,
		)
		require.Len(t, qualifications, 1)
		assert.Zero(t, calls, "a replica condemned by the readiness leg must not be probed")
		assert.False(t, qualifications[0].Activated())
		assert.True(t, qualifications[0].HasFailure())
	})

	t.Run("an incomplete replica condemns it before any probe", func(t *testing.T) {
		calls = 0
		md := healthDeployment(2)
		// One member of a replica declared at two. The group total is stamped from the role,
		// because these members were rendered from it; a replica short of a DIFFERENT size than
		// the role declares is the edit case and is asserted in the table above instead.
		pods := stampHealthGroupTotals(md, []core.Pod{
			healthPod("server", 0, 0, healthBool(true), "uid-a"),
		})
		qualifications := qualifyModelDeploymentInstances(
			context.Background(), md, pods,
			modelDeploymentPendingReplacement{}, countingFetch,
		)
		// One member of two: the completeness leg fails AND the shape answers without the
		// network (NotApplicable), so zero calls is true twice over.
		require.Len(t, qualifications, 1)
		assert.Zero(t, calls)
		assert.False(t, qualifications[0].Eligible())
	})
}

// healthPairDeployment builds a deployment with a Prefill role and a Decode role, each declaring its
// own data-parallel width, on the engine named.
//
// THE WIDTHS ARE DECLARED BECAUSE THEY ARE THE ONLY PLACE THE ENGINE'S DATA-PARALLEL WIDTH IS
// WRITTEN DOWN, and a probe that guessed them would ask about ranks the engine does not have.
func healthPairDeployment(engine string, prefillWidth, decodeWidth int) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: engine, Image: "engine:latest"},
			Model:  workercore.ModelDeploymentModel{Name: "Qwen/Qwen3-30B-A3B"},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Replicas: 1, ReplicaSize: 1, InstanceType: "h20-8x",
					Kind:      workercore.ModelDeploymentRoleKindPrefill,
					ExtraArgs: []string{"--dp-size", strconvx.Itoa(prefillWidth)},
				},
				{
					Name: "decode", Replicas: 1, ReplicaSize: 1, InstanceType: "h20-8x",
					Kind:      workercore.ModelDeploymentRoleKindDecode,
					ExtraArgs: []string{"--dp-size", strconvx.Itoa(decodeWidth)},
				},
			},
		},
	}

	return md
}

// healthPairAddress is the address one member of a pair's group answers at.
//
// THE TWO ROLES OF ONE DEPLOYMENT NEVER SHARE AN ADDRESS, because the ordinary fixture derives an
// address from the ordinal and the member index alone, and two roles of one deployment would then
// answer at the same one, which is a shape no cluster has.
func healthPairAddress(role string, ordinal, member int) string {
	side := 0
	if role == "decode" {
		side = 1
	}

	return "10.0." + strconvx.Itoa(side) + "." + strconvx.Itoa(ordinal) + "." + strconvx.Itoa(1+member)
}

// healthPairHost is the authority one member of a pair's group is asked at.
func healthPairHost(role string, ordinal, member int) string {
	return healthPairAddress(role, ordinal, member) + ":8000"
}

// healthPairPod is one member of a pair's group, with the prefiller's bootstrap port published on it
// when the prefiller is the one being built.
//
// THE BOOTSTRAP PORT IS PUBLISHED ONLY ON THE PREFILLER, because it is the prefiller that serves the
// transfer endpoints the decode side has to be told about.
func healthPairPod(role string, ordinal, member int, ready *bool, uid, bootstrapPort string) core.Pod {
	pod := healthPod(role, ordinal, member, ready, uid)
	pod.Status.PodIP = healthPairAddress(role, ordinal, member)
	if bootstrapPort != "" {
		pod.Annotations[modelDeploymentGroupForwardPairBootstrapPortAnnotation] = bootstrapPort
	}

	return pod
}

// healthPairFetch records every request and answers the directed pair the way the engine does.
//
// THE PREFILLERS ARE NAMED RATHER THAN GUESSED, because a request says which side of the pair it
// is only to the engine: this operator sends the same chat route to both heads.
type healthPairFetch struct {
	mutex sync.Mutex
	// asked records every request, in the order the legs were sent.
	asked []modelDeploymentGroupForwardPairRequest
	// prefillerHosts are the authorities the prefillers answer at.
	prefillHosts map[string]bool
	// failures maps an authority to the error that authority answers with.
	failures map[string]error
}

func (f *healthPairFetch) fetch(
	_ context.Context, request modelDeploymentGroupForwardPairRequest,
) ([]byte, error) {
	f.mutex.Lock()
	f.asked = append(f.asked, request)
	f.mutex.Unlock()

	authority := request.URL[strings.Index(request.URL, "://")+3:]
	if slash := strings.Index(authority, "/"); slash >= 0 {
		authority = authority[:slash]
	}
	// THE METRICS SCRAPE IS ANSWERED BY THE STUB ITSELF, minimally: the observation reads the
	// mooncake series of both halves, and a GET that fell through to the chat answer would hand it
	// a document that reads as no series at all.
	if request.Method == "GET" {
		return []byte("vllm:mooncake_bytes_transferred_count 0.0\n" +
			"vllm:mooncake_num_failed_recvs 0.0\n"), nil
	}
	if err, failed := f.failures[authority]; failed {
		return nil, err
	}

	return probePairAnswer(request.Body, f.prefillHosts[authority]), nil
}

// healthPairPairs returns the distinct pairs that were asked, each one named by the prefiller it
// was asked for and the authority it was asked at.
//
// THE PAIR IS READ OUT OF THE REQUEST rather than out of the order the requests arrived in,
// because a pair is two requests and the order of the two legs is not the identity of the pair.
func healthPairPairs(t *testing.T, fetch *healthPairFetch) []string {
	t.Helper()

	seen := map[string]bool{}
	var pairs []string
	for _, request := range fetch.asked {
		if request.Method == "GET" {
			// THE METRICS SCRAPES ARE NOT PAIRS. The observation reads both halves' mooncake
			// exposition after the legs answer, and a scrape names no prefiller and no room.
			continue
		}
		authority := request.URL[strings.Index(request.URL, "://")+3:]
		if slash := strings.Index(authority, "/"); slash >= 0 {
			authority = authority[:slash]
		}
		var body struct {
			BootstrapHost string `json:"bootstrap_host"`
		}
		require.NoError(t, json.Unmarshal(request.Body, &body))
		pair := body.BootstrapHost + "->" + authority
		if seen[pair] {
			continue
		}
		seen[pair] = true
		pairs = append(pairs, pair)
	}

	return pairs
}

// healthPairQualification finds the qualification of the replica of one role.
func healthPairQualification(
	t *testing.T, qualifications []modelDeploymentInstanceQualification, role string,
) modelDeploymentInstanceQualification {
	t.Helper()

	for _, q := range qualifications {
		if q.View.Role == role {
			return q
		}
	}
	t.Fatalf("no qualification for role %q", role)

	return modelDeploymentInstanceQualification{}
}

// TestGroupQualificationAsksTheDirectedPair is the positive contract of the pair wiring: a Prefill
// group and a Decode group are observed as one pair, both answers are Verified, and each side keeps
// the generation of its own group.
func TestGroupQualificationAsksTheDirectedPair(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true}}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	require.Len(t, qualifications, 2)
	for _, q := range qualifications {
		assert.Equal(t, modelDeploymentGroupForwardVerified, q.GroupForward.State,
			"%s answered the pair", q.View.Role)
		assert.True(t, q.Eligible(), "%s qualifies on a verified pair", q.View.Role)
		assert.Equal(t, modelDeploymentLegVerified,
			q.Leg(modelDeploymentLegMembersReady).Verdict,
			"%s keeps its own readiness", q.View.Role)
	}
	assert.Len(t, fetch.asked, 4, "one rank pair is two requests, and the two halves' metrics scrapes follow it")
}

// TestGroupQualificationNeverReadsNotApplicableForAPair pins the difference between a pair and an
// ordinary group: a Prefill or a Decode replica is served by a pair whatever its member count is, so
// it never reads NotApplicable, which is the answer for a group with no cross-member path at all.
func TestGroupQualificationNeverReadsNotApplicableForAPair(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true}}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	for _, q := range qualifications {
		assert.NotEqual(t, modelDeploymentGroupForwardNotApplicable, q.GroupForward.State,
			"%s is served by a pair, so there is no cross-member path of its own to not apply",
			q.View.Role)
	}
}

// TestGroupQualificationAggregatesPartnersIndependently is the any-success rule, read as behavior
// rather than as a call count: a replica is Verified when at least one of its ready partners answers,
// a later unsuccessful partner never displaces that answer, and the partner's own fault never
// reaches the other side's cheap legs.
func TestGroupQualificationAggregatesPartnersIndependently(t *testing.T) {
	testCases := []struct {
		name string
		// failing names the authorities that answer with an error.
		failing map[string]bool
		// wantVerified names the replicas that end Verified, by role and ordinal.
		wantVerified map[string]bool
	}{
		{
			name: "the first decode fails and the second one answers",
			failing: map[string]bool{
				healthPairHost("decode", 0, 0): true,
			},
			wantVerified: map[string]bool{
				"prefill/0": true, "prefill/1": true, "decode/1": true,
			},
		},
		{
			name: "the first prefiller fails and the second one answers",
			failing: map[string]bool{
				healthPairHost("prefill", 0, 0): true,
			},
			wantVerified: map[string]bool{
				"prefill/1": true, "decode/0": true, "decode/1": true,
			},
		},
		{
			name: "every partner fails",
			failing: map[string]bool{
				healthPairHost("prefill", 0, 0): true,
				healthPairHost("prefill", 1, 0): true,
				healthPairHost("decode", 0, 0):  true,
				healthPairHost("decode", 1, 0):  true,
			},
			wantVerified: map[string]bool{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
			md.Spec.Roles[0].Replicas = 2
			md.Spec.Roles[1].Replicas = 2
			pods := stampHealthGroupTotals(md, []core.Pod{
				healthPairPod("prefill", 0, 0, healthBool(true), "uid-p00", "8998"),
				healthPairPod("prefill", 1, 0, healthBool(true), "uid-p10", "8998"),
				healthPairPod("decode", 0, 0, healthBool(true), "uid-d00", ""),
				healthPairPod("decode", 1, 0, healthBool(true), "uid-d10", ""),
			})
			fetch := &healthPairFetch{
				prefillHosts: map[string]bool{
					healthPairHost("prefill", 0, 0): true,
					healthPairHost("prefill", 1, 0): true,
				},
				failures: map[string]error{},
			}
			for authority := range tc.failing {
				fetch.failures[authority] = errors.New("no route to host")
			}

			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
			)

			require.Len(t, qualifications, 4)
			for _, q := range qualifications {
				name := q.View.Role + "/" + strconvx.Itoa(q.View.Ordinal)
				want := tc.wantVerified[name]
				state := modelDeploymentGroupForwardVerified
				if !want {
					state = modelDeploymentGroupForwardUnknown
				}
				assert.Equal(t, state, q.GroupForward.State,
					"%s ends %s", name, state)
				assert.Equal(t, modelDeploymentLegVerified,
					q.Leg(modelDeploymentLegMembersReady).Verdict,
					"%s keeps its own readiness whatever its partner did", name)
				assert.Equal(t, modelDeploymentLegVerified,
					q.Leg(modelDeploymentLegMemberSetComplete).Verdict,
					"%s keeps its own completeness whatever its partner did", name)
			}

			pairs := healthPairPairs(t, fetch)
			assert.Len(t, pairs, len(pairs), "every pair is asked at most once")
			seen := map[string]bool{}
			for _, pair := range pairs {
				assert.False(t, seen[pair], "the pair %s is asked at most once", pair)
				seen[pair] = true
			}
		})
	}
}

// TestGroupQualificationHoldsAPairThatCouldNotBeAnswered is the fail-closed contract of the pair
// wiring: a pair that could not be observed holds, and a hold is not a revocation.
func TestGroupQualificationHoldsAPairThatCouldNotBeAnswered(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{
		prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
		failures: map[string]error{
			healthPairHost("decode", 0, 0): errors.New("no route to host"),
		},
	}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	require.Len(t, qualifications, 2)
	for _, q := range qualifications {
		assert.Equal(t, modelDeploymentGroupForwardUnknown, q.GroupForward.State,
			"%s holds a pair that could not be observed", q.View.Role)
		assert.False(t, q.HasFailure(),
			"%s holds rather than revokes: a pair that could not be read blames neither side",
			q.View.Role)
		assert.False(t, q.Eligible(), "%s is not eligible", q.View.Role)
	}
}

// TestGroupQualificationHoldsWhenNoReadyPartnerIsAvailable covers the three ways a partner can be
// absent, and pins that none of them reaches the ready side's own cheap legs.
func TestGroupQualificationHoldsWhenNoReadyPartnerIsAvailable(t *testing.T) {
	testCases := []struct {
		name string
		// pods builds the live members of the deployment.
		pods func(md *workercore.ModelDeployment) []core.Pod
		// wantPartnerLeg pins the partner's own completeness verdict.
		wantPartnerComplete modelDeploymentLegVerdict
	}{
		{
			name: "the opposite role has no pods at all",
			pods: func(md *workercore.ModelDeployment) []core.Pod {
				return []core.Pod{
					healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
				}
			},
		},
		{
			name: "the opposite group is short of a member",
			pods: func(md *workercore.ModelDeployment) []core.Pod {
				md.Spec.Roles[1].ReplicaSize = 2

				return []core.Pod{
					healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
					healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
				}
			},
			wantPartnerComplete: modelDeploymentLegFailed,
		},
		{
			name: "the opposite member is not ready",
			pods: func(md *workercore.ModelDeployment) []core.Pod {
				return []core.Pod{
					healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
					healthPairPod("decode", 0, 0, healthBool(false), "uid-d0", ""),
				}
			},
			wantPartnerComplete: modelDeploymentLegVerified,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
			pods := stampHealthGroupTotals(md, tc.pods(md))
			fetch := &healthPairFetch{
				prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
			}

			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
			)

			require.NotEmpty(t, qualifications)
			prefill := healthPairQualification(t, qualifications, "prefill")
			assert.Equal(t, modelDeploymentGroupForwardUnknown, prefill.GroupForward.State,
				"the prefiller holds: no ready partner could complete the pair")
			assert.Equal(t, modelDeploymentLegVerified,
				prefill.Leg(modelDeploymentLegMembersReady).Verdict,
				"the prefiller keeps its own readiness")
			assert.Equal(t, modelDeploymentLegVerified,
				prefill.Leg(modelDeploymentLegMemberSetComplete).Verdict,
				"the prefiller keeps its own completeness")
			assert.Empty(t, fetch.asked, "no partner means no request")

			if tc.wantPartnerComplete != modelDeploymentLegVerified {
				decode := healthPairQualification(t, qualifications, "decode")
				assert.Equal(t, tc.wantPartnerComplete,
					decode.Leg(modelDeploymentLegMemberSetComplete).Verdict,
					"the partner's own shortfall is the partner's own leg")
			}
		})
	}
}

// TestGroupQualificationHoldsAPairWhoseMembersAreNotReady pins that a replica which is not all
// Ready=True and complete is never asked about on the network.
func TestGroupQualificationHoldsAPairWhoseMembersAreNotReady(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(false), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{
		prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
	}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	require.Len(t, qualifications, 2)
	for _, q := range qualifications {
		assert.Equal(t, modelDeploymentGroupForwardUnknown, q.GroupForward.State,
			"%s holds: its own members are not ready", q.View.Role)
	}
	assert.Empty(t, fetch.asked, "a replica that is not ready is never asked about")
}

// TestGroupQualificationIsUnsupportedOnAnEngineWithNoPairObservation pins the standing capability
// gap: an engine this operator has no verified pair observation for is Unsupported, and nothing is
// asked of it.
func TestGroupQualificationIsUnsupportedOnAnEngineWithNoPairObservation(t *testing.T) {
	md := healthPairDeployment("TensorRT", 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{
		prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
	}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	require.Len(t, qualifications, 2)
	for _, q := range qualifications {
		assert.Equal(t, modelDeploymentGroupForwardUnsupported, q.GroupForward.State,
			"%s is unsupported on an engine with no pair observation", q.View.Role)
	}
	assert.Empty(t, fetch.asked, "an unsupported shape is never asked about")
}

// TestGroupQualificationIsUnsupportedWithoutAPairTransport pins the unwired capability: a caller
// that hands over no pair transport gets no observation of a pair at all, which is Unsupported
// rather than a question this operator cannot answer.
func TestGroupQualificationIsUnsupportedWithoutAPairTransport(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch,
	)

	require.Len(t, qualifications, 2)
	for _, q := range qualifications {
		assert.Equal(t, modelDeploymentGroupForwardUnsupported, q.GroupForward.State,
			"%s has no observation of a pair without a transport", q.View.Role)
		assert.Contains(t, q.GroupForward.Reason, "transport",
			"the reason names the unwired capability")
	}
}

// TestGroupQualificationIsUnsupportedOnARoleThatTookItsCommandLine pins that a role which took
// over its command line serves a route this operator does not know, and that neither side of the
// pair may be verified through it.
func TestGroupQualificationIsUnsupportedOnARoleThatTookItsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		// commandRole names the role that took over its command line.
		commandRole string
	}{
		{name: "the prefiller took its command line", commandRole: "prefill"},
		{name: "the decoder took its command line", commandRole: "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
			for i := range md.Spec.Roles {
				if md.Spec.Roles[i].Name == tc.commandRole {
					md.Spec.Roles[i].Command = []string{"python3", "-m", "custom"}
				}
			}
			pods := stampHealthGroupTotals(md, []core.Pod{
				healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
				healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
			})
			fetch := &healthPairFetch{
				prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
			}

			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
			)

			require.Len(t, qualifications, 2)
			for _, q := range qualifications {
				assert.Equal(t, modelDeploymentGroupForwardUnsupported, q.GroupForward.State,
					"%s is unsupported: a role whose route this operator does not know cannot be "+
						"paired with anything", q.View.Role)
			}
			assert.Empty(t, fetch.asked, "an unsupported shape is never asked about")
		})
	}
}

// TestGroupQualificationIsUnsupportedOnAVLLMPairWithoutTheRoutingProxy pins the one known vLLM gap:
// the prefiller's engine id is held in the prefiller's own transfer registry, which this operator
// does not read, so a vLLM pair that no routing proxy drives cannot be observed. The answer is the
// same with and without a partner Pod, and nothing is asked either way.
func TestGroupQualificationIsUnsupportedOnAVLLMPairWithoutTheRoutingProxy(t *testing.T) {
	for _, tc := range []struct {
		name string
		// withPartner says whether the decode group has a member at all.
		withPartner bool
	}{
		{name: "with a decode group to pair with", withPartner: true},
		{name: "with no decode group to pair with", withPartner: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := healthPairDeployment(workercore.ModelDeploymentEngineVLLM, 1, 1)
			pods := []core.Pod{
				healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
			}
			if tc.withPartner {
				pods = append(pods, healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""))
			}
			pods = stampHealthGroupTotals(md, pods)
			fetch := &healthPairFetch{
				prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
			}

			qualifications := qualifyModelDeploymentInstances(
				context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
			)

			require.NotEmpty(t, qualifications)
			for _, q := range qualifications {
				assert.Equal(t, modelDeploymentGroupForwardUnsupported, q.GroupForward.State,
					"%s is unsupported: the prefiller's engine id is in the prefiller's own registry",
					q.View.Role)
				assert.Equal(t, modelDeploymentLegVerified,
					q.Leg(modelDeploymentLegMembersReady).Verdict,
					"%s keeps its own readiness whatever the configuration says", q.View.Role)
			}
			assert.Empty(t, fetch.asked, "an unsupported shape is never asked about")
		})
	}
}

// TestGroupQualificationRetainsTheWinningAttempt pins the evidence of a verified pair: the attempt
// that verified the replica is retained with its shape and its request identifiers, so a reader can
// tell a proxy-shape answer from a rank-covering one without re-deriving it.
func TestGroupQualificationRetainsTheWinningAttempt(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := stampHealthGroupTotals(md, []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	})
	fetch := &healthPairFetch{
		prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true},
	}

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch, fetch.fetch,
	)

	for _, q := range qualifications {
		require.NotNil(t, q.PairAttempt,
			"%s keeps the attempt that verified it", q.View.Role)
		assert.Equal(t, modelDeploymentGroupForwardPairShapeHeadToHead, q.PairAttempt.Shape,
			"%s keeps the shape the attempt took", q.View.Role)
		assert.NotEmpty(t, q.PairAttempt.RequestIDs,
			"%s keeps the requests the answer was bound to", q.View.Role)
		assert.Equal(t, q.Generation, q.GroupForward.Generation,
			"%s carries the generation of the group the attempt bound", q.View.Role)
	}
}

// TestGroupQualificationStatesTheGroup is the table of the group states, each with the legs that
// produce it and the reason it carries.
func TestGroupQualificationStatesTheGroup(t *testing.T) {
	md := healthDeployment(1)
	ready := healthPod("server", 0, 0, healthBool(true), "uid-a")
	held := healthPod("server", 0, 0, nil, "uid-a")
	unready := healthPod("server", 0, 0, healthBool(false), "uid-a")

	qualified := qualifyModelDeploymentInstances(
		context.Background(), md, []core.Pod{ready}, modelDeploymentPendingReplacement{}, boundProbeFetch,
	)
	require.Len(t, qualified, 1)
	assert.Equal(t, modelDeploymentGroupStateEligible, qualified[0].GroupState(),
		"a complete ready group qualifies")
	assert.NotEmpty(t, qualified[0].Reason(), "an eligible group still says why")

	starting := qualifyModelDeploymentInstances(
		context.Background(), md, []core.Pod{held}, modelDeploymentPendingReplacement{}, boundProbeFetch,
	)
	require.Len(t, starting, 1)
	assert.Equal(t, modelDeploymentGroupStateAwaitingReady, starting[0].GroupState(),
		"a group whose members have not reported yet is awaiting its members")
	assert.NotEmpty(t, starting[0].Reason(), "a held group names the leg that holds it")

	revoked := qualifyModelDeploymentInstances(
		context.Background(), md, []core.Pod{unready}, modelDeploymentPendingReplacement{}, boundProbeFetch,
	)
	require.Len(t, revoked, 1)
	assert.Equal(t, modelDeploymentGroupStateWithdrawn, revoked[0].GroupState(),
		"a member that reports not ready withdraws the group")
	assert.NotEmpty(t, revoked[0].Reason(), "a withdrawn group names the fault")

	unsupported := qualifyModelDeploymentInstances(
		context.Background(), healthPairDeployment("TensorRT", 1, 1),
		stampHealthGroupTotals(healthPairDeployment("TensorRT", 1, 1), []core.Pod{
			healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		}),
		modelDeploymentPendingReplacement{}, boundProbeFetch,
	)
	require.Len(t, unsupported, 1)
	assert.Equal(t, modelDeploymentGroupStateUnsupported, unsupported[0].GroupState(),
		"a shape no observation exists for is unsupported")
}
