package worker

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/utils/datax"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

func TestModelDeploymentScalerBindsOneOwner(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"server": 1})
	first := scalerTestScaler("first", "first-uid", workercore.ModelDeploymentScalerModeActive)
	second := scalerTestScaler("second", "second-uid", workercore.ModelDeploymentScalerModeActive)
	client := scalerTestClient(t, md, first, second)
	r := scalerTestReconciler(client, now)

	if _, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(first)}); err != nil {
		t.Fatal(err)
	}
	storedMD := scalerTestGetDeployment(t, client)
	var binding workercore.ModelDeploymentScalerBinding
	if err := json.Unmarshal([]byte(storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.ScalerUID != string(first.UID) || binding.Term != 1 {
		t.Fatalf("binding = %#v, want first scaler at term 1", binding)
	}

	if _, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(second)}); err != nil {
		t.Fatal(err)
	}
	storedSecond := scalerTestGetScaler(t, client, second.Name)
	if got := scalerCondition(storedSecond, workercore.ModelDeploymentScalerConditionTypeBound); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonAlreadyBound {
		t.Fatalf("Bound = %#v, want False/AlreadyBound", got)
	}
	storedMD = scalerTestGetDeployment(t, client)
	if err := json.Unmarshal([]byte(storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.ScalerUID != string(first.UID) {
		t.Fatalf("second scaler changed binding owner to %q", binding.ScalerUID)
	}
}

func TestModelDeploymentScalerModeTransitionsReleaseAndReacquire(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"server": 1})
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeShadow)
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	stored := scalerTestGetScaler(t, client, scaler.Name)
	if got := scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeActive); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonShadowMode {
		t.Fatalf("Shadow Active = %#v", got)
	}
	if got := scalerTestGetDeployment(t, client).Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "" {
		t.Fatalf("Shadow wrote target binding %q", got)
	}

	stored.Spec.Mode = workercore.ModelDeploymentScalerModeActive
	stored.Generation++
	if err := client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileScaler(t, r, scaler.Name)
	storedMD := scalerTestGetDeployment(t, client)
	firstTerm := scalerTestBinding(t, storedMD).Term
	if firstTerm != 1 {
		t.Fatalf("first active term = %d, want 1", firstTerm)
	}

	stored = scalerTestGetScaler(t, client, scaler.Name)
	stored.Spec.Mode = workercore.ModelDeploymentScalerModeShadow
	stored.Generation++
	if err := client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileScaler(t, r, scaler.Name)
	storedMD = scalerTestGetDeployment(t, client)
	if got := storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "" {
		t.Fatalf("Shadow transition retained binding %q", got)
	}
	if got := scalerCondition(scalerTestGetScaler(t, client, scaler.Name), workercore.ModelDeploymentScalerConditionTypeActive); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonShadowMode {
		t.Fatalf("Shadow transition Active = %#v", got)
	}

	stored = scalerTestGetScaler(t, client, scaler.Name)
	stored.Spec.Mode = workercore.ModelDeploymentScalerModeActive
	stored.Generation++
	if err := client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileScaler(t, r, scaler.Name)
	storedMD = scalerTestGetDeployment(t, client)
	if got := scalerTestBinding(t, storedMD).Term; got != firstTerm+1 {
		t.Fatalf("reacquired term = %d, want %d", got, firstTerm+1)
	}

	stored = scalerTestGetScaler(t, client, scaler.Name)
	stored.Spec.Suspend = true
	stored.Generation++
	if err := client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileScaler(t, r, scaler.Name)
	storedMD = scalerTestGetDeployment(t, client)
	if got := storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "" {
		t.Fatalf("suspend retained binding %q", got)
	}
	if got := scalerTestGetScaler(t, client, scaler.Name); scalerCondition(got, workercore.ModelDeploymentScalerConditionTypeActive).Reason != workercore.ModelDeploymentScalerReasonSuspended {
		t.Fatalf("suspend Active = %#v", scalerCondition(got, workercore.ModelDeploymentScalerConditionTypeActive))
	}
	if got := scalerTestGetDeployment(t, client).Spec.Roles[0].Replicas; got != 1 {
		t.Fatalf("suspend changed replicas to %d", got)
	}
}

func TestModelDeploymentScalerPendingCohortRetractsAtomically(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 1, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"prefill": 2, "decode": 2})
	scaler := scalerTestCoupledScaler()
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: 3}
	started := meta.NewTime(now.Add(-time.Minute))
	scaler.Status.PendingCohort = &workercore.ModelDeploymentScalerPendingCohortStatus{
		DecisionID: "decision-1", Term: 3,
		BaselineVector: map[string]int32{"prefill": 1, "decode": 1},
		TargetVector:   map[string]int32{"prefill": 2, "decode": 2},
		StartedAt:      &started,
	}
	scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
		DecisionID: "decision-1", Term: 3, Vector: map[string]int32{"prefill": 2, "decode": 2},
	}
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey:  scalerTestBindingJSON(scaler, 3),
		workercore.ModelDeploymentScalerDecisionAnnotationKey: scalerTestDecisionJSON(scaler, 3, "decision-1", map[string]int32{"prefill": 2, "decode": 2}),
	}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	result := reconcileScaler(t, r, scaler.Name)

	storedMD := scalerTestGetDeployment(t, client)
	if got := scalerTestVector(storedMD); !scalerTestVectorsEqual(got, map[string]int32{"prefill": 1, "decode": 1}) {
		t.Fatalf("replica vector = %v, want atomic baseline", got)
	}
	if got := scalerTestGetScaler(t, client, scaler.Name); got.Status.PendingCohort != nil {
		t.Fatalf("PendingCohort remains: %#v", got.Status.PendingCohort)
	} else if condition := scalerCondition(got, workercore.ModelDeploymentScalerConditionTypeBlocked); condition.Status != meta.ConditionTrue || condition.Reason != workercore.ModelDeploymentScalerReasonResourceExhausted {
		t.Fatalf("Blocked = %#v, want True/ResourceExhausted", condition)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %s, want 10s after retraction", result.RequeueAfter)
	}
}

func TestModelDeploymentScalerShadowDoesNotRetractPendingCohort(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 1, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"prefill": 2, "decode": 2})
	scaler := scalerTestCoupledScaler()
	scaler.Spec.Mode = workercore.ModelDeploymentScalerModeShadow
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: 3}
	started := meta.NewTime(now.Add(-time.Minute))
	scaler.Status.PendingCohort = &workercore.ModelDeploymentScalerPendingCohortStatus{
		DecisionID: "decision-1", Term: 3,
		BaselineVector: map[string]int32{"prefill": 1, "decode": 1},
		TargetVector:   map[string]int32{"prefill": 2, "decode": 2},
		StartedAt:      &started,
	}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	storedMD := scalerTestGetDeployment(t, client)
	if got := scalerTestVector(storedMD); !scalerTestVectorsEqual(got, map[string]int32{"prefill": 2, "decode": 2}) {
		t.Fatalf("Shadow changed replica vector: %v", got)
	}
	if got := storedMD.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey]; got != "" {
		t.Fatalf("Shadow wrote decision annotation %q", got)
	}
}

func TestModelDeploymentScalerRestoresAbortedRetirement(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 2, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"prefill": 1, "decode": 1})
	md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
		RoleName: "prefill", ReplicaOrdinal: 1,
		State:  workercore.ModelDeploymentRetirementStateAborted,
		Reason: "DrainTimeout",
	}
	scaler := scalerTestCoupledScaler()
	scaler.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds = 90
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: 4}
	scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
		DecisionID: "decision-2", Term: 4, Vector: map[string]int32{"prefill": 1, "decode": 1},
	}
	scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
		Role: "prefill", Ordinal: 1, Outcome: "Committed",
		PreScaleDownVector: map[string]int32{"prefill": 2, "decode": 2},
	}
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey:  scalerTestBindingJSON(scaler, 4),
		workercore.ModelDeploymentScalerDecisionAnnotationKey: scalerTestDecisionJSON(scaler, 4, "decision-2", map[string]int32{"prefill": 1, "decode": 1}),
	}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	storedMD := scalerTestGetDeployment(t, client)
	if got := scalerTestVector(storedMD); !scalerTestVectorsEqual(got, map[string]int32{"prefill": 2, "decode": 2}) {
		t.Fatalf("replica vector = %v, want pre-scale-down vector", got)
	}
	storedScaler := scalerTestGetScaler(t, client, scaler.Name)
	if got := storedScaler.Status.LastScaleDown; got.Outcome != "Aborted" || got.CooldownUntil == nil || !got.CooldownUntil.Time.Equal(now.Add(90*time.Second)) {
		t.Fatalf("LastScaleDown = %#v, want aborted with cooldown", got)
	}
	if got := scalerCondition(storedScaler, workercore.ModelDeploymentScalerConditionTypeBlocked); got.Status != meta.ConditionTrue || got.Reason != workercore.ModelDeploymentScalerReasonScaleDownAborted {
		t.Fatalf("Blocked = %#v, want True/ScaleDownAborted", got)
	}
}

func TestModelDeploymentScalerWaitsOnReplacedTarget(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 3, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "new-md-uid", map[string]int32{"server": 2})
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Generation = 5
	scaler.Status.ObservedGeneration = 5
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: "old-md-uid", TargetName: md.Name, Term: 6}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	storedMD := scalerTestGetDeployment(t, client)
	if got := storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "" {
		t.Fatalf("replacement target was bound: %q", got)
	}
	storedScaler := scalerTestGetScaler(t, client, scaler.Name)
	if got := scalerCondition(storedScaler, workercore.ModelDeploymentScalerConditionTypeBound); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonTargetReplaced {
		t.Fatalf("Bound = %#v, want False/TargetReplaced", got)
	}
	if got := storedScaler.Status.Binding; got == nil || got.TargetUID != "old-md-uid" || got.Term != 6 {
		t.Fatalf("Binding = %#v, want old target retained until spec edit", got)
	}
	if storedScaler.Status.LastCommit != nil {
		t.Fatalf("LastCommit = %#v, want no recovery from replacement target", storedScaler.Status.LastCommit)
	}
}

func TestModelDeploymentScalerRecoversDecisionAfterRestart(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 4, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"server": 2})
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 4),
		workercore.ModelDeploymentScalerDecisionAnnotationKey: scalerTestDecisionJSONWithStatus(
			scaler, 4, "recovered", map[string]int32{"server": 2}, map[string]int32{"server": 1}, timePointer(now), 1,
		),
	}
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)
	reconcileScaler(t, r, scaler.Name)

	storedScaler := scalerTestGetScaler(t, client, scaler.Name)
	got := storedScaler.Status.LastCommit
	if got == nil || got.DecisionID != "recovered" || got.Term != 4 || !scalerTestVectorsEqual(got.Vector, map[string]int32{"server": 2}) {
		t.Fatalf("LastCommit = %#v, want recovered decision", got)
	}
	if !scalerTestVectorsEqual(got.BaselineVector, map[string]int32{"server": 1}) || got.Timestamp == nil || got.TargetGeneration != 1 {
		t.Fatalf("recovered commit metadata = %#v, want baseline, timestamp, and generation", got)
	}
	if storedMD := scalerTestGetDeployment(t, client); !scalerTestVectorsEqual(scalerTestVector(storedMD), map[string]int32{"server": 2}) {
		t.Fatalf("recovery changed vector: %v", scalerTestVector(storedMD))
	}
}

func TestModelDeploymentScalerDeletionClearsBindingAndPreservesVector(t *testing.T) {
	now := meta.NewTime(time.Date(2026, 10, 21, 12, 5, 0, 0, time.UTC))
	md := scalerTestDeployment("target", "md-uid", map[string]int32{"server": 2})
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(
			scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive), 2),
		"preserve": "annotation",
	}
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.DeletionTimestamp = &now
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now.Time)
	reconcileScaler(t, r, scaler.Name)

	storedMD := scalerTestGetDeployment(t, client)
	if got := storedMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "" {
		t.Fatalf("deletion retained binding %q", got)
	}
	if got := storedMD.Annotations["preserve"]; got != "annotation" {
		t.Fatalf("deletion changed unrelated annotation to %q", got)
	}
	if got := scalerTestVector(storedMD); !scalerTestVectorsEqual(got, map[string]int32{"server": 2}) {
		t.Fatalf("deletion changed replica vector: %v", got)
	}
}

func scalerTestClient(t *testing.T, objects ...ctrlcli.Object) ctrlcli.Client {
	t.Helper()
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}).
		WithObjects(objects...).Build()
}

func scalerTestReconciler(client ctrlcli.Client, now time.Time) *ModelDeploymentScalerReconciler {
	return &ModelDeploymentScalerReconciler{
		Client:  client,
		clock:   func() time.Time { return now },
		sampler: &modelDeploymentScalerSampler{Clock: func() time.Time { return now }},
	}
}

func scalerTestDeployment(name, uid string, replicas map[string]int32) *workercore.ModelDeployment {
	roles := make([]workercore.ModelDeploymentRole, 0, len(replicas))
	for _, name := range []string{"server", "prefill", "decode"} {
		if count, ok := replicas[name]; ok {
			roles = append(roles, workercore.ModelDeploymentRole{Name: name, Replicas: count})
		}
	}
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "ns", Name: name, UID: types.UID(uid), Generation: 1},
		Spec:       workercore.ModelDeploymentSpec{Roles: roles},
	}
}

func scalerTestScaler(name, uid string, mode workercore.ModelDeploymentScalerMode) *workercore.ModelDeploymentScaler {
	return &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "ns", Name: name, UID: types.UID(uid), Generation: 1,
			Finalizers: []string{workercore.ModelDeploymentScalerFinalizerKey},
		},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "target"},
			Mode:      mode,
			Roles:     []workercore.ModelDeploymentScalerRoleBounds{{Name: "server", MinReplicas: 1, MaxReplicas: 4}},
			Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeProportional,
				Proportional: &workercore.ModelDeploymentScalerProportionalSpec{
					Metric:      workercore.ModelDeploymentScalerProportionalMetricRunningRequests,
					RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{{Name: "server", Target: resource.MustParse("1")}},
				},
			},
		},
	}
}

func scalerTestCoupledScaler() *workercore.ModelDeploymentScaler {
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
		{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
		{Name: "decode", MinReplicas: 1, MaxReplicas: 4},
	}
	scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
		NumeratorRole: "prefill", DenominatorRole: "decode",
		Min: quantityPointer("1"), Max: quantityPointer("1"), Target: quantityPointer("1"),
	}}
	scaler.Spec.Sampling = workercore.ModelDeploymentScalerSamplingSpec{IntervalSeconds: 10, PendingTimeoutSeconds: 60}
	scaler.Spec.Algorithm.Proportional.RoleTargets = []workercore.ModelDeploymentScalerRoleTarget{
		{Name: "prefill", Target: resource.MustParse("1")},
		{Name: "decode", Target: resource.MustParse("1")},
	}
	return scaler
}

func quantityPointer(value string) *resource.Quantity {
	quantity := resource.MustParse(value)
	return &quantity
}

func reconcileScaler(t *testing.T, r *ModelDeploymentScalerReconciler, name string) ctrlreconcile.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "ns", Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func scalerTestGetDeployment(t *testing.T, client ctrlcli.Client) *workercore.ModelDeployment {
	t.Helper()
	md := new(workercore.ModelDeployment)
	if err := client.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "ns", Name: "target"}, md); err != nil {
		t.Fatal(err)
	}
	return md
}

func scalerTestGetScaler(t *testing.T, client ctrlcli.Client, name string) *workercore.ModelDeploymentScaler {
	t.Helper()
	scaler := new(workercore.ModelDeploymentScaler)
	if err := client.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "ns", Name: name}, scaler); err != nil {
		t.Fatal(err)
	}
	return scaler
}

func scalerTestBinding(t *testing.T, md *workercore.ModelDeployment) workercore.ModelDeploymentScalerBinding {
	t.Helper()
	var binding workercore.ModelDeploymentScalerBinding
	if err := json.Unmarshal([]byte(md.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]), &binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func scalerTestBindingJSON(scaler *workercore.ModelDeploymentScaler, term int64) string {
	value, err := json.Marshal(workercore.ModelDeploymentScalerBinding{ScalerName: scaler.Name, ScalerUID: string(scaler.UID), Term: term})
	if err != nil {
		panic(err)
	}
	return string(value)
}

func scalerTestDecisionJSON(scaler *workercore.ModelDeploymentScaler, term int64, id string, vector map[string]int32) string {
	value, err := json.Marshal(modelDeploymentScalerDecisionAnnotation{
		ScalerUID: string(scaler.UID), DecisionID: id, Term: term, Vector: vector,
	})
	if err != nil {
		panic(err)
	}
	return string(value)
}

func scalerTestDecisionJSONWithStatus(
	scaler *workercore.ModelDeploymentScaler,
	term int64,
	id string,
	vector, baseline map[string]int32,
	timestamp *meta.Time,
	targetGeneration int64,
) string {
	value, err := json.Marshal(modelDeploymentScalerDecisionAnnotation{
		ScalerUID: string(scaler.UID), DecisionID: id, Term: term, Vector: vector,
		BaselineVector: baseline, Timestamp: timestamp, TargetGeneration: targetGeneration,
	})
	if err != nil {
		panic(err)
	}
	return string(value)
}

func scalerTestVector(md *workercore.ModelDeployment) map[string]int32 {
	vector := make(map[string]int32, len(md.Spec.Roles))
	for _, role := range md.Spec.Roles {
		vector[role.Name] = role.Replicas
	}
	return vector
}

func scalerTestVectorsEqual(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for role, count := range a {
		if b[role] != count {
			return false
		}
	}
	return true
}

func scalerCondition(scaler *workercore.ModelDeploymentScaler, conditionType string) *gpustack.Condition {
	for i := range scaler.Status.Conditions {
		if scaler.Status.Conditions[i].Type == conditionType {
			return &scaler.Status.Conditions[i]
		}
	}
	return &gpustack.Condition{Type: conditionType}
}

// scalerTestDemandHistory returns a demand history holding the given samples.
func scalerTestDemandHistory(samples ...algorithm.DemandSample) *datax.RingBuffer[algorithm.DemandSample] {
	history := algorithm.NewDemandHistory()
	for _, sample := range samples {
		history.Push(sample)
	}
	return history
}

func TestModelDeploymentScalerPredictorStatusSkipsNonFiniteDemandSamples(t *testing.T) {
	now := time.Unix(100, 0)
	statuses := scalerPredictorStatuses(map[string]*algorithm.PredictorState{
		"server": {
			Level:       math.NaN(),
			Trend:       12.5,
			SampleCount: 2,
			History: scalerTestDemandHistory(
				algorithm.DemandSample{Timestamp: now, TokensPerSecond: math.NaN()},
				algorithm.DemandSample{Timestamp: now.Add(time.Second), TokensPerSecond: math.Inf(1)},
				algorithm.DemandSample{Timestamp: now.Add(2 * time.Second), TokensPerSecond: 42},
			),
		},
	})
	if len(statuses) != 1 || len(statuses[0].History) != 1 {
		t.Fatalf("statuses = %#v, want 1 finite sample", statuses)
	}
}

func TestModelDeploymentScalerDeletionCleansUpDecisionAndRefusesCorruptBinding(t *testing.T) {
	now := time.Unix(200, 0)
	delTime := meta.NewTime(now)

	md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 2})
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.DeletionTimestamp = &delTime
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey:  scalerTestBindingJSON(scaler, 1),
		workercore.ModelDeploymentScalerDecisionAnnotationKey: scalerTestDecisionJSON(scaler, 1, "dec-1", map[string]int32{"server": 2}),
	}

	cli := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(cli, now)
	reconcileScaler(t, r, scaler.Name)

	gotMD := scalerTestGetDeployment(t, cli)
	if _, exists := gotMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; exists {
		t.Fatalf("expected binding annotation to be removed on deletion, got %v", gotMD.Annotations)
	}
	if _, exists := gotMD.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey]; exists {
		t.Fatalf("expected decision annotation to be removed on deletion, got %v", gotMD.Annotations)
	}

	corruptMD := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 2})
	corruptScaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	corruptScaler.DeletionTimestamp = &delTime
	corruptMD.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey: "{corrupt-json",
	}
	corruptCli := scalerTestClient(t, corruptMD, corruptScaler)
	reconcileScaler(t, scalerTestReconciler(corruptCli, now), corruptScaler.Name)

	liveCorruptMD := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 2})
	liveCorruptMD.Annotations = map[string]string{
		workercore.ModelDeploymentScalerBindingAnnotationKey: "{corrupt-json",
	}
	liveScaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	liveCli := scalerTestClient(t, liveCorruptMD, liveScaler)
	reconcileScaler(t, scalerTestReconciler(liveCli, now), liveScaler.Name)

	storedLiveMD := scalerTestGetDeployment(t, liveCli)
	if got := storedLiveMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey]; got != "{corrupt-json" {
		t.Fatalf("corrupt binding annotation = %q, want it left as it is", got)
	}
	storedLiveScaler := scalerTestGetScaler(t, liveCli, liveScaler.Name)
	if got := scalerCondition(storedLiveScaler, workercore.ModelDeploymentScalerConditionTypeBound); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonInvalidBinding {
		t.Fatalf("Bound = %#v, want False/InvalidBinding", got)
	}
	if got := scalerCondition(storedLiveScaler, workercore.ModelDeploymentScalerConditionTypeActive); got.Status != meta.ConditionFalse || got.Reason != workercore.ModelDeploymentScalerReasonInvalidBinding {
		t.Fatalf("Active = %#v, want False/InvalidBinding", got)
	}
}

func TestModelDeploymentScalerSpecialRestorePathsRespectBindingOwnership(t *testing.T) {
	now := time.Unix(1000, 0)
	started := meta.NewTime(now.Add(-400 * time.Second))
	otherScaler := scalerTestScaler("other", "other-uid", workercore.ModelDeploymentScalerModeActive)

	t.Run("pending cohort timeout retraction stops when binding changed", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 3})
		md.Annotations = map[string]string{
			workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(otherScaler, 2),
		}
		scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
			TargetUID: "target-uid", TargetName: "target", Term: 1,
		}
		scaler.Status.PendingCohort = &workercore.ModelDeploymentScalerPendingCohortStatus{
			DecisionID:     "dec-1",
			Term:           1,
			BaselineVector: map[string]int32{"server": 1},
			TargetVector:   map[string]int32{"server": 3},
			StartedAt:      &started,
		}
		cli := scalerTestClient(t, md, scaler)
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		gotMD := scalerTestGetDeployment(t, cli)
		if gotMD.Spec.Roles[0].Replicas != 3 {
			t.Fatalf("replicas = %d, want 3 (unmodified when binding is not owned)", gotMD.Spec.Roles[0].Replicas)
		}
		if got := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound); got.Reason != workercore.ModelDeploymentScalerReasonAlreadyBound {
			t.Fatalf("Bound condition = %+v, want AlreadyBound", got)
		}
	})

	t.Run("retirement abort restore stops when binding changed", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
		md.Annotations = map[string]string{
			workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(otherScaler, 2),
		}
		md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
			RoleName:       "server",
			ReplicaOrdinal: 1,
			State:          workercore.ModelDeploymentRetirementStateAborted,
			Reason:         "drain timed out",
		}
		scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
			TargetUID: "target-uid", TargetName: "target", Term: 1,
		}
		scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
			Role:               "server",
			Ordinal:            1,
			PreScaleDownVector: map[string]int32{"server": 2},
			Outcome:            "Committed",
		}
		cli := scalerTestClient(t, md, scaler)
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		gotMD := scalerTestGetDeployment(t, cli)
		if gotMD.Spec.Roles[0].Replicas != 1 {
			t.Fatalf("replicas = %d, want 1 (unmodified when binding is not owned)", gotMD.Spec.Roles[0].Replicas)
		}
		if got := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound); got.Reason != workercore.ModelDeploymentScalerReasonAlreadyBound {
			t.Fatalf("Bound condition = %+v, want AlreadyBound", got)
		}
	})
}

func TestModelDeploymentScalerTargetRolesEligibility(t *testing.T) {
	now := time.Unix(1000, 0)

	t.Run("missing target role blocks planning", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
		scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
		scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
			{Name: "nonexistent", MinReplicas: 1, MaxReplicas: 4},
		}
		cli := scalerTestClient(t, md, scaler)
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		cond := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked)
		if cond.Status != meta.ConditionTrue || cond.Reason != workercore.ModelDeploymentScalerReasonBoundsConflict {
			t.Fatalf("Blocked condition = %+v, want True with BoundsConflict", cond)
		}
	})

	t.Run("custom command role blocks planning", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
		md.Spec.Roles[0].Command = []string{"/bin/bash", "-c", "echo custom"}
		scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
		cli := scalerTestClient(t, md, scaler)
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		cond := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked)
		if cond.Status != meta.ConditionTrue || cond.Reason != workercore.ModelDeploymentScalerReasonBoundsConflict {
			t.Fatalf("Blocked condition = %+v, want True with BoundsConflict", cond)
		}
	})

	t.Run("elastic-ep role blocks planning", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
		md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2}
		scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
		cli := scalerTestClient(t, md, scaler)
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		cond := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked)
		if cond.Status != meta.ConditionTrue || cond.Reason != workercore.ModelDeploymentScalerReasonBoundsConflict {
			t.Fatalf("Blocked condition = %+v, want True with BoundsConflict", cond)
		}
	})
}
