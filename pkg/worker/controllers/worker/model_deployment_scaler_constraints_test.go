package worker

import (
	"math"
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

func TestScalerConstraintsWholeUnit1P3DAnd1E1P2D(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	old := now.Add(-10 * time.Minute)
	recent := now.Add(-20 * time.Second)

	makeHistory := func(t1Val, t2Val int32) []v1alpha1.ModelDeploymentScalerStabilizationSample {
		return []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(old), Replicas: t1Val},
			{Timestamp: metav1Time(recent), Replicas: t2Val},
		}
	}

	t.Run("1P:3D scale up 1x to 2x", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		p := roleState("prefill", 1, 2)
		p.Kind = algorithm.RoleKindPrefill
		d := roleState("decode", 3, 6)
		d.Kind = algorithm.RoleKindDecode

		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if res.BlockingReason != "" {
			t.Fatalf("unexpected blocking reason: %q", res.BlockingReason)
		}
		if res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected 2P:6D, got %#v", res.Replicas)
		}
	})

	t.Run("1P:3D scale down 2x to 1x does not deadlock on u_D=3", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		p := roleState("prefill", 2, 1)
		p.Kind = algorithm.RoleKindPrefill
		p.StabilizationHistory = makeHistory(1, 1)
		d := roleState("decode", 6, 3)
		d.Kind = algorithm.RoleKindDecode
		d.StabilizationHistory = makeHistory(3, 3)

		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if res.BlockingReason != "" {
			t.Fatalf("unexpected blocking reason: %q", res.BlockingReason)
		}
		if res.Replicas["prefill"] != 1 || res.Replicas["decode"] != 3 {
			t.Fatalf("expected 1P:3D, got %#v", res.Replicas)
		}
	})

	t.Run("1E:1P:2D atomic scale up and scale down", func(t *testing.T) {
		spec := constraintSpec("encode", "prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "encode", MinReplicas: 1, MaxReplicas: 4},
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 2, MaxReplicas: 8},
		}
		e := roleState("encode", 1, 2)
		e.Kind = algorithm.RoleKindEncode
		p := roleState("prefill", 1, 2)
		p.Kind = algorithm.RoleKindPrefill
		d := roleState("decode", 2, 4)
		d.Kind = algorithm.RoleKindDecode

		upRes := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{e, p, d},
			WholeUnit: map[string]int32{"encode": 1, "prefill": 1, "decode": 2},
			Now:       now,
		})
		if upRes.Replicas["encode"] != 2 || upRes.Replicas["prefill"] != 2 || upRes.Replicas["decode"] != 4 {
			t.Fatalf("expected 2E:2P:4D, got %#v", upRes.Replicas)
		}

		eDown := roleState("encode", 2, 1)
		eDown.Kind = algorithm.RoleKindEncode
		eDown.StabilizationHistory = makeHistory(1, 1)
		pDown := roleState("prefill", 2, 1)
		pDown.Kind = algorithm.RoleKindPrefill
		pDown.StabilizationHistory = makeHistory(1, 1)
		dDown := roleState("decode", 4, 2)
		dDown.Kind = algorithm.RoleKindDecode
		dDown.StabilizationHistory = makeHistory(2, 2)

		downRes := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{eDown, pDown, dDown},
			WholeUnit: map[string]int32{"encode": 1, "prefill": 1, "decode": 2},
			Now:       now,
		})
		if downRes.Replicas["encode"] != 1 || downRes.Replicas["prefill"] != 1 || downRes.Replicas["decode"] != 2 {
			t.Fatalf("expected 1E:1P:2D, got %#v", downRes.Replicas)
		}
	})

	t.Run("explicit step policy smaller than unit holds and percent floors to units", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		// ScaleUp StepPolicy Pods=2 < u_D=3 -> holds at 1x.
		spec.Behavior.ScaleUp.StepPolicy = &v1alpha1.ModelDeploymentScalerStepPolicy{
			Type:  v1alpha1.ModelDeploymentScalerStepPolicyTypePods,
			Value: 2,
		}
		p := roleState("prefill", 1, 2)
		d := roleState("decode", 3, 6)
		d.Kind = algorithm.RoleKindDecode
		held := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if held.Replicas["prefill"] != 1 || held.Replicas["decode"] != 3 {
			t.Fatalf("expected hold when Pods step < u_D, got %#v", held.Replicas)
		}
		if held.BlockingReason != v1alpha1.ModelDeploymentScalerReasonPacingBounded {
			t.Fatalf("expected PacingBounded, got %q", held.BlockingReason)
		}

		// ScaleUp MaxStepPercent=25 at 2x (2P:6D): ceil(2*0.25)=1 for P (1 unit), ceil(6*0.25)=2 for D (0 units) -> holds.
		spec.Behavior.ScaleUp.StepPolicy = nil
		spec.Behavior.ScaleUp.MaxStepPercent = 25
		p2 := roleState("prefill", 2, 3)
		d2 := roleState("decode", 6, 9)
		d2.Kind = algorithm.RoleKindDecode
		pctHeld := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p2, d2},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if pctHeld.Replicas["prefill"] != 2 || pctHeld.Replicas["decode"] != 6 {
			t.Fatalf("expected percent step < 1 unit on D to hold at 2P:6D, got %#v", pctHeld.Replicas)
		}

		// ScaleUp MaxStepPercent=50 at 2x (2P:6D): ceil(2*0.5)=1 P (1 unit), ceil(6*0.5)=3 D (1 unit) -> +1 unit to 3P:9D.
		spec.Behavior.ScaleUp.MaxStepPercent = 50
		pctStep := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p2, d2},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if pctStep.Replicas["prefill"] != 3 || pctStep.Replicas["decode"] != 9 {
			t.Fatalf("expected +1 unit to 3P:9D, got %#v", pctStep.Replicas)
		}

		// Explicit ScaleDown StepPolicy Pods=2 < u_D=3 -> holds at 2P:6D.
		spec.Behavior.ScaleDown.StepPolicy = &v1alpha1.ModelDeploymentScalerStepPolicy{
			Type:  v1alpha1.ModelDeploymentScalerStepPolicyTypePods,
			Value: 2,
		}
		pDown := roleState("prefill", 2, 1)
		pDown.StabilizationHistory = makeHistory(1, 1)
		dDown := roleState("decode", 6, 3)
		dDown.Kind = algorithm.RoleKindDecode
		dDown.StabilizationHistory = makeHistory(3, 3)
		downHeld := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{pDown, dDown},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if downHeld.Replicas["prefill"] != 2 || downHeld.Replicas["decode"] != 6 {
			t.Fatalf("expected ScaleDown hold when Pods step < u_D, got %#v", downHeld.Replicas)
		}
	})

	t.Run("feasible bounds and RatioConflict", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 1},
			{Name: "decode", MinReplicas: 6, MaxReplicas: 12},
		}
		p := roleState("prefill", 1, 2)
		d := roleState("decode", 3, 6)
		d.Kind = algorithm.RoleKindDecode
		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if res.BlockingReason != v1alpha1.ModelDeploymentScalerReasonRatioConflict {
			t.Fatalf("expected RatioConflict, got %q", res.BlockingReason)
		}
	})

	t.Run("stabilization window and scale-down/scale-up holds", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		spec.Behavior.ScaleUp.StabilizationWindowSeconds = 60
		pUp := roleState("prefill", 1, 2)
		dUp := roleState("decode", 3, 6)
		dUp.Kind = algorithm.RoleKindDecode
		// Short history doesn't span upWindow -> hold scale-up.
		pUp.StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(recent), Replicas: 2},
		}
		dUp.StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(recent), Replicas: 6},
		}
		resUpHold := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{pUp, dUp},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if resUpHold.Replicas["prefill"] != 1 || resUpHold.Replicas["decode"] != 3 {
			t.Fatalf("expected scale-up hold under stabilization window, got %#v", resUpHold.Replicas)
		}

		// Scale-up blocked when pending workload exists.
		spec.Behavior.ScaleUp.StabilizationWindowSeconds = 0
		pUp.HasPendingUnadmittedWorkload = true
		pUp.StabilizationHistory = nil
		dUp.StabilizationHistory = nil
		resPending := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{pUp, dUp},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if resPending.Replicas["prefill"] != 1 || resPending.Replicas["decode"] != 3 {
			t.Fatalf("expected scale-up hold when pending workload exists, got %#v", resPending.Replicas)
		}

		// Scale-down held on DecodePostScaleUpGraceSeconds, HoldScaleDown, InAbortCooldown, HasActiveRetirement, and stabilization max >= kCur.
		baseDown := func() (ModelDeploymentScalerRoleState, ModelDeploymentScalerRoleState) {
			p := roleState("prefill", 2, 1)
			p.StabilizationHistory = makeHistory(1, 1)
			d := roleState("decode", 6, 3)
			d.Kind = algorithm.RoleKindDecode
			d.StabilizationHistory = makeHistory(3, 3)
			return p, d
		}

		p1, d1 := baseDown()
		d1.LastScaleUpTime = now.Add(-10 * time.Second)
		if res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec: spec, Roles: []ModelDeploymentScalerRoleState{p1, d1},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3}, Now: now,
		}); res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected Decode grace hold, got %#v", res.Replicas)
		}

		p2, d2 := baseDown()
		p2.HoldScaleDown = true
		if res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec: spec, Roles: []ModelDeploymentScalerRoleState{p2, d2},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3}, Now: now,
		}); res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected HoldScaleDown hold, got %#v", res.Replicas)
		}

		p3, d3 := baseDown()
		if res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec: spec, Roles: []ModelDeploymentScalerRoleState{p3, d3},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3}, InAbortCooldown: true, Now: now,
		}); res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected InAbortCooldown hold, got %#v", res.Replicas)
		}

		p4, d4 := baseDown()
		if res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec: spec, Roles: []ModelDeploymentScalerRoleState{p4, d4},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3}, HasActiveRetirement: true, Now: now,
		}); res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected HasActiveRetirement hold, got %#v", res.Replicas)
		}

		// History peak at 2x inside the 5-minute window blocks scale-down to 1x.
		p5, d5 := baseDown()
		d5.StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(old), Replicas: 3},
			{Timestamp: metav1Time(now.Add(-2 * time.Minute)), Replicas: 6},
			{Timestamp: metav1Time(recent), Replicas: 3},
		}
		if res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec: spec, Roles: []ModelDeploymentScalerRoleState{p5, d5},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3}, Now: now,
		}); res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 6 {
			t.Fatalf("expected stabilization max >= kCur hold, got %#v", res.Replicas)
		}
	})

	t.Run("non-multiple current falls back to ratio projection", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		spec.Ratios = []v1alpha1.ModelDeploymentScalerRoleRatio{
			scalerRatio("decode", "prefill", "3", "3", "3"),
		}
		// Current is skewed (2P:4D) instead of 2P:6D -> fallback restores exact 3:1 ratio.
		p := roleState("prefill", 2, 2)
		d := roleState("decode", 4, 6)
		d.Kind = algorithm.RoleKindDecode
		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if res.Replicas["prefill"] == 0 || res.Replicas["decode"] != 3*res.Replicas["prefill"] {
			t.Fatalf("expected fallback ratio projection to satisfy 3:1, got %#v", res.Replicas)
		}
	})
}

func TestScalerConstraintsWholeUnitOverflowSaturation(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	old := now.Add(-10 * time.Minute)
	recent := now.Add(-20 * time.Second)

	t.Run("wholeUnitRawTarget handles math.MaxInt32 without negative wrap", func(t *testing.T) {
		p := roleState("prefill", 6, math.MaxInt32)
		p.Kind = algorithm.RoleKindPrefill
		d := roleState("decode", 8, math.MaxInt32)
		d.Kind = algorithm.RoleKindDecode
		wholeUnit := map[string]int32{"prefill": 3, "decode": 4}
		kRaw := wholeUnitRawTarget([]ModelDeploymentScalerRoleState{p, d}, wholeUnit, 2)
		want := int32((int64(math.MaxInt32) + 2) / 3)
		if kRaw != want {
			t.Fatalf("wholeUnitRawTarget = %d, want %d", kRaw, want)
		}
	})

	t.Run("solveWholeUnitConstraints scales up with RawRecommendedReplicas=MaxInt32 and StepPolicy Pods=MaxInt32", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 3, MaxReplicas: 12},
			{Name: "decode", MinReplicas: 4, MaxReplicas: 16},
		}
		spec.Behavior.ScaleUp.StepPolicy = &v1alpha1.ModelDeploymentScalerStepPolicy{
			Type:  v1alpha1.ModelDeploymentScalerStepPolicyTypePods,
			Value: math.MaxInt32,
		}
		p := roleState("prefill", 6, math.MaxInt32)
		p.Kind = algorithm.RoleKindPrefill
		p.StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(old), Replicas: math.MaxInt32},
			{Timestamp: metav1Time(recent), Replicas: math.MaxInt32},
		}
		d := roleState("decode", 8, math.MaxInt32)
		d.Kind = algorithm.RoleKindDecode
		d.StabilizationHistory = []v1alpha1.ModelDeploymentScalerStabilizationSample{
			{Timestamp: metav1Time(old), Replicas: math.MaxInt32},
			{Timestamp: metav1Time(recent), Replicas: math.MaxInt32},
		}

		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 3, "decode": 4},
			Now:       now,
		})
		if res.Replicas["prefill"] != 12 || res.Replicas["decode"] != 16 {
			t.Fatalf("expected scale up to 12P:16D, got %#v (blocking=%q)", res.Replicas, res.BlockingReason)
		}
	})

	t.Run("solveWholeUnitConstraints handles large MinReplicas without int32 overflow", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		// Choose MinReplicas near MaxInt32 where minimum+unit-1 would overflow int32.
		unit := int32(3)
		maxUnits := int32(math.MaxInt32) / 4
		minP := maxUnits * unit
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: math.MaxInt32 - 1, MaxReplicas: math.MaxInt32},
			{Name: "decode", MinReplicas: 4, MaxReplicas: math.MaxInt32},
		}
		// (math.MaxInt32 - 1 + 2) / 3 = 715827883 > maxUnits (536870911), so kMin > kMax -> RatioConflict cleanly.
		p := roleState("prefill", minP, minP)
		p.Kind = algorithm.RoleKindPrefill
		d := roleState("decode", maxUnits*4, maxUnits*4)
		d.Kind = algorithm.RoleKindDecode

		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 3, "decode": 4},
			Now:       now,
		})
		if res.BlockingReason != v1alpha1.ModelDeploymentScalerReasonRatioConflict {
			t.Fatalf("expected RatioConflict, got %q (replicas=%#v)", res.BlockingReason, res.Replicas)
		}
	})

	t.Run("non-divisible current vector with WholeUnit blocks as RatioConflict instead of per-role fallback", func(t *testing.T) {
		spec := constraintSpec("prefill", "decode")
		spec.Roles = []v1alpha1.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 12},
			{Name: "decode", MinReplicas: 1, MaxReplicas: 16},
		}
		p := roleState("prefill", 2, 6)
		p.Kind = algorithm.RoleKindPrefill
		d := roleState("decode", 5, 10)
		d.Kind = algorithm.RoleKindDecode

		res := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
			Spec:      spec,
			Roles:     []ModelDeploymentScalerRoleState{p, d},
			WholeUnit: map[string]int32{"prefill": 1, "decode": 3},
			Now:       now,
		})
		if res.BlockingReason != v1alpha1.ModelDeploymentScalerReasonRatioConflict {
			t.Fatalf("expected RatioConflict, got %q", res.BlockingReason)
		}
		if res.Replicas["prefill"] != 2 || res.Replicas["decode"] != 5 {
			t.Fatalf("expected current vector held at 2P:5D, got %#v", res.Replicas)
		}
		if res.Reasons["prefill"] != v1alpha1.ModelDeploymentScalerReasonRatioConflict ||
			res.Reasons["decode"] != v1alpha1.ModelDeploymentScalerReasonRatioConflict {
			t.Fatalf("expected RatioConflict reason on every role, got %#v", res.Reasons)
		}
	})
}
