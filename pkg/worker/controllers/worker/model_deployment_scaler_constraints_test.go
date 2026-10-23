package worker

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

func scalerRatio(num, den, minRatio, target, maxRatio string) v1alpha1.ModelDeploymentScalerRoleRatio {
	return v1alpha1.ModelDeploymentScalerRoleRatio{
		NumeratorRole: num, DenominatorRole: den,
		Min: resourcePtr(minRatio), Target: resourcePtr(target), Max: resourcePtr(maxRatio),
	}
}

func resourcePtr(value string) *resource.Quantity {
	quantity := resource.MustParse(value)
	return &quantity
}

func constraintSpec(names ...string) v1alpha1.ModelDeploymentScalerSpec {
	roles := make([]v1alpha1.ModelDeploymentScalerRoleBounds, 0, len(names))
	for _, name := range names {
		roles = append(roles, v1alpha1.ModelDeploymentScalerRoleBounds{Name: name, MinReplicas: 1, MaxReplicas: 8})
	}
	return v1alpha1.ModelDeploymentScalerSpec{Roles: roles}
}

func roleState(name string, current, raw int32) ModelDeploymentScalerRoleState {
	return ModelDeploymentScalerRoleState{
		Name: name, CurrentReplicas: current, RawRecommendedReplicas: raw,
		CompleteCoverage: true,
		StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{
			Timestamp: metav1Time(time.Unix(0, 0)), Replicas: raw,
		}},
	}
}

func TestScalerConstraintsJointPacingAndRatioProjection(t *testing.T) {
	spec := constraintSpec("prefill", "decode")
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{scalerRatio("prefill", "decode", "2", "2", "2")}
	spec.Roles[1].MaxReplicas = 3
	input := ModelDeploymentScalerConstraintInput{
		Spec: spec, Now: time.Unix(20, 0),
		Roles: []ModelDeploymentScalerRoleState{roleState("prefill", 4, 8), roleState("decode", 2, 2)},
	}
	input.Roles[1].StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: metav1Time(time.Unix(0, 0)), Replicas: 2}}
	result := SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["prefill"] != 6 || result.Replicas["decode"] != 3 {
		t.Fatalf("joint projection = %#v, intervals=%#v, reason=%q", result.Replicas, result.Intervals, result.BlockingReason)
	}
}

func TestScalerConstraintsFixedRatioScaleDownAndDirection(t *testing.T) {
	spec := constraintSpec("prefill", "decode")
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{scalerRatio("prefill", "decode", "1", "1", "1")}
	spec.Behavior.ScaleDown.StabilizationWindowSeconds = 10
	old := metav1Time(time.Unix(5, 0))
	input := ModelDeploymentScalerConstraintInput{Spec: spec, Now: time.Unix(20, 0), Roles: []ModelDeploymentScalerRoleState{
		{Name: "prefill", CurrentReplicas: 2, RawRecommendedReplicas: 1, CompleteCoverage: true, StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: old, Replicas: 1}}},
		{Name: "decode", CurrentReplicas: 2, RawRecommendedReplicas: 1, CompleteCoverage: true, StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: old, Replicas: 1}}},
	}}
	result := SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["prefill"] != 1 || result.Replicas["decode"] != 1 {
		t.Fatalf("fixed-ratio down = %#v", result.Replicas)
	}

	input.Roles[0].RawRecommendedReplicas = 4
	input.Roles[1].RawRecommendedReplicas = 2
	result = SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["prefill"] < 2 || result.Replicas["decode"] < 2 {
		t.Fatalf("scale-up direction = %#v", result.Replicas)
	}
}

func TestScalerConstraintsThreeRoleCouplingAndGuards(t *testing.T) {
	spec := constraintSpec("encode", "prefill", "decode")
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{
		scalerRatio("encode", "prefill", "0.5", "0.5", "0.5"),
		scalerRatio("prefill", "decode", "2", "2", "2"),
	}
	now := time.Unix(20, 0)
	input := ModelDeploymentScalerConstraintInput{Spec: spec, Now: now, Roles: []ModelDeploymentScalerRoleState{
		{Name: "encode", CurrentReplicas: 1, RawRecommendedReplicas: 4, CompleteCoverage: true, StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: metav1Time(time.Unix(0, 0)), Replicas: 4}}},
		{Name: "prefill", CurrentReplicas: 2, RawRecommendedReplicas: 8, CompleteCoverage: true, StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: metav1Time(time.Unix(0, 0)), Replicas: 8}}},
		{Name: "decode", CurrentReplicas: 1, RawRecommendedReplicas: 4, CompleteCoverage: true, StabilizationHistory: []v1alpha1.ModelDeploymentScalerStabilizationSample{{Timestamp: metav1Time(time.Unix(0, 0)), Replicas: 4}}},
	}}
	result := SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["encode"] != 3 || result.Replicas["prefill"] != 6 || result.Replicas["decode"] != 3 {
		t.Fatalf("three-role projection = %#v", result.Replicas)
	}

	input.Roles[1].HasPendingUnadmittedWorkload = true
	result = SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["prefill"] != 2 || result.Replicas["decode"] != 1 || result.BlockingReason != v1alpha1.ModelDeploymentScalerReasonPacingBounded {
		t.Fatalf("pending guard = %#v reason=%q", result.Replicas, result.BlockingReason)
	}
	input.HasActiveRetirement = true
	input.Roles[0].RawRecommendedReplicas = 1
	input.Roles[1].RawRecommendedReplicas = 1
	input.Roles[2].RawRecommendedReplicas = 1
	result = SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["encode"] != 1 || result.Replicas["prefill"] != 2 || result.Replicas["decode"] != 1 {
		t.Fatalf("retirement guard = %#v", result.Replicas)
	}

	input.HasActiveRetirement = false
	input.Roles[2].Kind = algorithm.RoleKindDecode
	input.Roles[2].LastScaleUpTime = now.Add(-30 * time.Second)
	input.Roles[2].RawRecommendedReplicas = 1
	result = SolveModelDeploymentScalerConstraints(input)
	if result.Replicas["decode"] != 1 {
		t.Fatalf("decode grace = %#v", result.Replicas)
	}
}

func TestScalerConstraintsRatioConflict(t *testing.T) {
	spec := constraintSpec("prefill", "decode")
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{scalerRatio("prefill", "decode", "1.5", "1.5", "1.5")}
	spec.Roles[0].MaxReplicas = 1
	spec.Roles[1].MaxReplicas = 1
	result := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
		Spec: spec, Now: time.Unix(20, 0), Roles: []ModelDeploymentScalerRoleState{
			roleState("prefill", 1, 1), roleState("decode", 1, 1),
		},
	})
	if result.Replicas["prefill"] != 1 || result.Replicas["decode"] != 1 || result.BlockingReason != v1alpha1.ModelDeploymentScalerReasonRatioConflict {
		t.Fatalf("ratio conflict = %#v reason=%q", result.Replicas, result.BlockingReason)
	}
}

func TestScalerConstraintsPrunesStabilizationHistoryAndCapsMaxItems(t *testing.T) {
	spec := constraintSpec("server")
	spec.Behavior.ScaleDown.StabilizationWindowSeconds = 60
	var history []v1alpha1.ModelDeploymentScalerStabilizationSample
	base := time.Unix(1000, 0)
	for i := 0; i < 400; i++ {
		history = append(history, v1alpha1.ModelDeploymentScalerStabilizationSample{
			Timestamp: metav1Time(base.Add(time.Duration(i) * time.Second)),
			Replicas:  2,
		})
	}
	now := base.Add(400 * time.Second)
	result := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
		Spec: spec, Now: now,
		Roles: []ModelDeploymentScalerRoleState{{
			Name: "server", CurrentReplicas: 2, RawRecommendedReplicas: 1,
			CompleteCoverage: true, StabilizationHistory: history,
		}},
	})
	pruned := result.StabilizationHistory["server"]
	if len(pruned) == 0 || len(pruned) > modelDeploymentScalerMaxStabilizationSamples {
		t.Fatalf("pruned history len = %d, want 1..%d", len(pruned), modelDeploymentScalerMaxStabilizationSamples)
	}
	if !historySpans(pruned, now, 60*time.Second) {
		t.Fatalf("pruned history must still span the stabilization window")
	}
}

func TestScalerConstraintsLooseMultiRoleComponentRemainsBounded(t *testing.T) {
	spec := constraintSpec("encode", "prefill", "decode")
	for i := range spec.Roles {
		spec.Roles[i].MaxReplicas = 1024
	}
	spec.Behavior.ScaleUp.StepPolicy = &v1alpha1.ModelDeploymentScalerStepPolicy{
		Type:  v1alpha1.ModelDeploymentScalerStepPolicyTypePods,
		Value: 1024,
	}
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{
		{NumeratorRole: "encode", DenominatorRole: "prefill", Target: resourcePtr("0.5")},
		{NumeratorRole: "prefill", DenominatorRole: "decode", Target: resourcePtr("2")},
	}
	result := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
		Spec: spec, Now: time.Unix(20, 0),
		Roles: []ModelDeploymentScalerRoleState{
			roleState("encode", 1, 200),
			roleState("prefill", 2, 400),
			roleState("decode", 1, 200),
		},
	})
	if result.Replicas["encode"] != 200 || result.Replicas["prefill"] != 400 || result.Replicas["decode"] != 200 {
		t.Fatalf("loose component projection = %#v", result.Replicas)
	}
}

func TestScalerConstraintsStarGraphMultipleNeighbors(t *testing.T) {
	spec := constraintSpec("center", "leaf1", "leaf2")
	spec.Behavior.ScaleUp.StepPolicy = &v1alpha1.ModelDeploymentScalerStepPolicy{
		Type:  v1alpha1.ModelDeploymentScalerStepPolicyTypePods,
		Value: 1024,
	}
	spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{
		{NumeratorRole: "center", DenominatorRole: "leaf1", Target: resourcePtr("2")},
		{NumeratorRole: "center", DenominatorRole: "leaf2", Target: resourcePtr("2")},
	}
	result := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
		Spec: spec, Now: time.Unix(20, 0),
		Roles: []ModelDeploymentScalerRoleState{
			roleState("center", 2, 4),
			roleState("leaf1", 1, 2),
			roleState("leaf2", 1, 2),
		},
	})
	if result.Replicas["center"] != 4 || result.Replicas["leaf1"] != 2 || result.Replicas["leaf2"] != 2 {
		t.Fatalf("star graph projection = %#v", result.Replicas)
	}
}
