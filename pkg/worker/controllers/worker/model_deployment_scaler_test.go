package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

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
			scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive), 2,
		),
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

func scalerTestOwnedPod(md *workercore.ModelDeployment, role string, ordinal int32, phase core.PodPhase) *core.Pod {
	controller := true
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: md.Namespace,
			Name:      fmt.Sprintf("%s-%s-%d", md.Name, role, ordinal),
			UID:       types.UID(fmt.Sprintf("%s-%s-%d-uid", md.Name, role, ordinal)),
			Labels: map[string]string{
				modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:    md.Name,
				modelDeploymentLabelKeyComponent:   role,
				modelDeploymentReplicaOrdinalLabel: fmt.Sprintf("%d", ordinal),
			},
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       md.Name,
				UID:        md.UID,
				Controller: &controller,
			}},
		},
		Status: core.PodStatus{Phase: phase},
	}
}

func TestModelDeploymentScalerMultiReplicaSerialRetirement(t *testing.T) {
	now := time.Unix(2000, 0).UTC()

	t.Run("surplus pods hold further scale-down when retirement status is nil or completed", func(t *testing.T) {
		// Deployment spec was already scaled down to 2P:6D, but decode-6 and prefill-2 Pods still exist.
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 2, "decode": 6})
		md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
			RoleName:       "decode",
			ReplicaOrdinal: 8,
			State:          workercore.ModelDeploymentRetirementStateCompleted,
		}
		scaler := scalerTestCoupledScaler()
		scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 1, MaxReplicas: 12},
		}
		scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
			NumeratorRole: "decode", DenominatorRole: "prefill",
			Min: quantityPointer("3"), Max: quantityPointer("3"), Target: quantityPointer("3"),
		}}
		// Seed stabilization history well below 2P:6D so only the live surplus Pod hold prevents scaling down to 1P:3D.
		old := meta.NewTime(now.Add(-10 * time.Minute))
		recent := meta.NewTime(now.Add(-30 * time.Second))
		scaler.Status.Roles = []workercore.ModelDeploymentScalerRoleStatus{
			{
				Name: "prefill", CurrentReplicas: 2,
				StabilizationHistory: []workercore.ModelDeploymentScalerStabilizationSample{
					{Timestamp: old, Replicas: 1},
					{Timestamp: recent, Replicas: 1},
				},
			},
			{
				Name: "decode", CurrentReplicas: 6,
				StabilizationHistory: []workercore.ModelDeploymentScalerStabilizationSample{
					{Timestamp: old, Replicas: 3},
					{Timestamp: recent, Replicas: 3},
				},
			},
		}
		scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
			Role:               "prefill",
			Ordinal:            2,
			PreScaleDownVector: map[string]int32{"prefill": 3, "decode": 9},
			Outcome:            workercore.ModelDeploymentScalerScaleDownOutcomeCommitted,
		}
		commitTS := meta.NewTime(now.Add(-2 * time.Minute))
		scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
			DecisionID: "dec-down",
			Term:       1,
			Vector:     map[string]int32{"prefill": 2, "decode": 6},
			Timestamp:  &commitTS,
		}
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
			TargetUID: "target-uid", TargetName: "target", Term: 1,
		}
		md.Annotations = map[string]string{
			workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 1),
		}

		objs := []ctrlcli.Object{
			md,
			scaler,
			scalerTestOwnedPod(md, "prefill", 0, core.PodRunning),
			scalerTestOwnedPod(md, "prefill", 1, core.PodRunning),
			scalerTestOwnedPod(md, "prefill", 2, core.PodRunning), // surplus Pod (ordinal >= current[prefill]=2)
			scalerTestOwnedPod(md, "decode", 0, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 1, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 2, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 3, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 4, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 5, core.PodRunning),
			scalerTestOwnedPod(md, "decode", 6, core.PodRunning), // surplus Pod (ordinal >= current[decode]=6)
		}
		cli := scalerTestClient(t, objs...)
		r := scalerTestReconciler(cli, now)
		pods, _, _, err := r.executionState(context.Background(), md)
		if err != nil {
			t.Fatalf("executionState error = %v", err)
		}
		if !hasSurplusDeploymentPod(pods, map[string]int32{"prefill": 2, "decode": 6}) {
			t.Fatalf("expected surplus pods to be detected")
		}

		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		if scaler.Status.LastScaleDown == nil || scaler.Status.LastScaleDown.Outcome != workercore.ModelDeploymentScalerScaleDownOutcomeCommitted {
			t.Fatalf("expected Committed LastScaleDown to remain intact while surplus pods exist, got %+v", scaler.Status.LastScaleDown)
		}

		// Delete decode-6 while prefill-2 remains and set Retirement=nil: still held by prefill-2!
		if err := cli.Delete(context.Background(), objs[len(objs)-1]); err != nil {
			t.Fatal(err)
		}
		md.Status.Retirement = nil
		if err := cli.Status().Update(context.Background(), md); err != nil {
			t.Fatal(err)
		}
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now.Add(10*time.Second), true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		if scaler.Status.LastScaleDown == nil {
			t.Fatalf("expected Committed LastScaleDown to remain intact while prefill-2 surplus pod exists")
		}

		// Delete prefill-2 (all surplus pods gone): Committed LastScaleDown is now cleared.
		if err := cli.Delete(context.Background(), objs[4]); err != nil {
			t.Fatal(err)
		}
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now.Add(20*time.Second), true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		if scaler.Status.LastScaleDown != nil {
			t.Fatalf("expected Committed LastScaleDown to clear once all surplus pods are gone, got %+v", scaler.Status.LastScaleDown)
		}
	})

	for _, tc := range []struct {
		name           string
		roleName       string
		replicaOrdinal int32
		restartCtrl    bool
	}{
		{name: "aborted first ordinal decode-5 restores 2P:6D", roleName: "decode", replicaOrdinal: 5},
		{name: "aborted middle ordinal decode-4 restores 2P:6D", roleName: "decode", replicaOrdinal: 4},
		{name: "aborted last ordinal prefill-1 restores 2P:6D after restart", roleName: "prefill", replicaOrdinal: 1, restartCtrl: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 1, "decode": 3})
			startedAt := meta.NewTime(now.Add(-30 * time.Second))
			md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
				RoleName:       tc.roleName,
				ReplicaOrdinal: tc.replicaOrdinal,
				State:          workercore.ModelDeploymentRetirementStateAborted,
				Reason:         "drain timed out",
				StartedAt:      startedAt,
				PhaseStartedAt: startedAt,
			}
			scaler := scalerTestCoupledScaler()
			scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
				TargetUID: "target-uid", TargetName: "target", Term: 1,
			}
			commitTS := meta.NewTime(now.Add(-1 * time.Minute))
			scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
				DecisionID: "dec-down",
				Term:       1,
				Vector:     map[string]int32{"prefill": 1, "decode": 3},
				Timestamp:  &commitTS,
			}
			scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
				Role:               "decode",
				Ordinal:            3,
				PreScaleDownVector: map[string]int32{"prefill": 2, "decode": 6},
				Outcome:            workercore.ModelDeploymentScalerScaleDownOutcomeCommitted,
			}
			md.Annotations = map[string]string{
				workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 1),
			}

			cli := scalerTestClient(t, md, scaler)
			r := scalerTestReconciler(cli, now)
			if tc.restartCtrl {
				// Simulate a freshly started controller instance reloading state strictly from objects.
				r = scalerTestReconciler(cli, now)
			}
			if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
				t.Fatalf("plan error = %v", err)
			}
			gotMD := scalerTestGetDeployment(t, cli)
			if got := scalerTestVector(gotMD); got["prefill"] != 2 || got["decode"] != 6 {
				t.Fatalf("restored vector = %#v, want 2P:6D", got)
			}
			if scaler.Status.LastScaleDown.Outcome != workercore.ModelDeploymentScalerScaleDownOutcomeAborted {
				t.Fatalf("LastScaleDown.Outcome = %q, want Aborted", scaler.Status.LastScaleDown.Outcome)
			}
			if cond := scalerCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked); cond.Status != meta.ConditionTrue || cond.Reason != workercore.ModelDeploymentScalerReasonScaleDownAborted {
				t.Fatalf("Blocked condition = %+v, want ScaleDownAborted", cond)
			}
		})
	}

	t.Run("stale aborted status before current commit does not restore old vector", func(t *testing.T) {
		md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 1, "decode": 3})
		staleTime := meta.NewTime(now.Add(-10 * time.Minute))
		md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
			RoleName:       "decode",
			ReplicaOrdinal: 4,
			State:          workercore.ModelDeploymentRetirementStateAborted,
			Reason:         "old abort",
			StartedAt:      staleTime,
			PhaseStartedAt: staleTime,
		}
		scaler := scalerTestCoupledScaler()
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
			TargetUID: "target-uid", TargetName: "target", Term: 1,
		}
		commitTS := meta.NewTime(now.Add(-1 * time.Minute))
		scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
			DecisionID: "dec-new-down",
			Term:       1,
			Vector:     map[string]int32{"prefill": 1, "decode": 3},
			Timestamp:  &commitTS,
		}
		scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
			Role:               "decode",
			Ordinal:            3,
			PreScaleDownVector: map[string]int32{"prefill": 2, "decode": 6},
			Outcome:            workercore.ModelDeploymentScalerScaleDownOutcomeCommitted,
		}
		md.Annotations = map[string]string{
			workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 1),
		}

		cli := scalerTestClient(t, md, scaler, scalerTestOwnedPod(md, "prefill", 0, core.PodRunning), scalerTestOwnedPod(md, "decode", 0, core.PodRunning))
		r := scalerTestReconciler(cli, now)
		if _, err := r.plan(context.Background(), scaler.DeepCopy(), scaler, md, now, true); err != nil {
			t.Fatalf("plan error = %v", err)
		}
		gotMD := scalerTestGetDeployment(t, cli)
		if got := scalerTestVector(gotMD); got["prefill"] != 1 || got["decode"] != 3 {
			t.Fatalf("stale abort unexpectedly modified vector to %#v", got)
		}
	})
}

func TestModelDeploymentScalerWholeUnitObservationAndPanicExpiry(t *testing.T) {
	const unitTargetTPS = 5250.0
	t0 := time.Unix(200000, 0).UTC()

	newWholeUnitScaler := func(predictive bool) *workercore.ModelDeploymentScaler {
		scaler := scalerTestCoupledScaler()
		scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
			{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
			{Name: "decode", MinReplicas: 3, MaxReplicas: 12},
		}
		scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
			NumeratorRole:   "decode",
			DenominatorRole: "prefill",
			Min:             quantityPointer("3"),
			Max:             quantityPointer("3"),
		}}
		scaler.Spec.Behavior = workercore.ModelDeploymentScalerBehaviorSpec{
			ScaleUp: workercore.ModelDeploymentScalerScaleUpRules{
				StabilizationWindowSeconds: 0,
			},
			ScaleDown: workercore.ModelDeploymentScalerScaleDownRules{
				StabilizationWindowSeconds:    60,
				DecodePostScaleUpGraceSeconds: 10,
			},
		}
		ttSpec := workercore.ModelDeploymentScalerTokenThroughputSpec{
			TTFTTargetMilliseconds: quantityPointer("500"),
			ITLTargetMilliseconds:  quantityPointer("50"),
			StableWindowSeconds:    180,
			PanicWindowSeconds:     60,
			PanicThreshold:         quantityPointer("2"),
		}
		if predictive {
			scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypePredictive,
				Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: ttSpec,
					HorizonSeconds:  60,
					MinPoints:       2,
				},
			}
		} else {
			scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
				Type:            workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &ttSpec,
			}
		}
		scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
			TargetUID: "target-uid", TargetName: "target", Term: 1,
		}
		return scaler
	}

	newWholeUnitPriors := func(scaler *workercore.ModelDeploymentScaler, includeEncode bool) *ModelDeploymentScalerCapacityPriors {
		unitVector := map[string]int32{"prefill": 1, "decode": 3}
		encodeRole := ""
		algs := map[string]workercore.ModelDeploymentScalerAlgorithmSpec{
			"prefill": {},
			"decode":  scaler.Spec.Algorithm,
		}
		if includeEncode {
			unitVector = map[string]int32{"encode": 1, "prefill": 1, "decode": 2}
			encodeRole = "encode"
			algs["encode"] = workercore.ModelDeploymentScalerAlgorithmSpec{}
		}
		return &ModelDeploymentScalerCapacityPriors{
			Mode:       ModelDeploymentScalerPriorModeWholeUnitServerFallback,
			Algorithms: algs,
			WholeUnit: &ModelDeploymentScalerWholeUnitPrior{
				UnitVector:                unitVector,
				EncodeRole:                encodeRole,
				PrefillRole:               "prefill",
				DecodeRole:                "decode",
				UnitTargetTokensPerSecond: unitTargetTPS,
				MinUnits:                  1,
				MaxUnits:                  4,
			},
		}
	}

	ensureAdmittedUnitPods := func(
		t *testing.T,
		cli ctrlcli.Client,
		md *workercore.ModelDeployment,
		counts map[string]int32,
	) {
		t.Helper()
		ctx := context.Background()
		controller := true
		for role, count := range counts {
			for ordinal := int32(0); ordinal < count; ordinal++ {
				podName := fmt.Sprintf("%s-%s-%d", md.Name, role, ordinal)
				existing := new(core.Pod)
				if err := cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: podName}, existing); err == nil {
					continue
				}
				pod := scalerSamplerPod(podName, role, types.UID(podName+"-uid"), int(ordinal))
				pod.Namespace = md.Namespace
				pod.Labels[modelDeploymentLabelKeyName] = modelDeploymentLabelValueName
				pod.Labels[modelDeploymentLabelKeyInstance] = md.Name
				group := modelDeploymentReplicaGroupName(md, role, int(ordinal))
				pod.Labels[kueuepodconst.GroupNameLabel] = group
				pod.OwnerReferences = []meta.OwnerReference{{
					APIVersion: workercore.SchemeGroupVersion.String(),
					Kind:       "ModelDeployment",
					Name:       md.Name,
					UID:        md.UID,
					Controller: &controller,
				}}
				if err := cli.Create(ctx, &pod); err != nil {
					t.Fatal(err)
				}
				wl := &kueue.Workload{
					ObjectMeta: meta.ObjectMeta{
						Namespace:       md.Namespace,
						Name:            group,
						Labels:          pod.Labels,
						OwnerReferences: []meta.OwnerReference{{Kind: "Pod", Name: pod.Name, UID: pod.UID}},
					},
					Status: kueue.WorkloadStatus{
						Conditions: []meta.Condition{
							{Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue, Reason: "Admitted"},
							{Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue, Reason: "Admitted"},
						},
					},
				}
				if err := cli.Create(ctx, wl); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	t.Run("observation clock, history retention, and panic expiry", func(t *testing.T) {
		for _, predictive := range []bool{false, true} {
			name := "TokenThroughput"
			if predictive {
				name = "Predictive"
			}
			t.Run(name, func(t *testing.T) {
				md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 1, "decode": 3})
				md.Spec.Engine = workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode

				scaler := newWholeUnitScaler(predictive)
				md.Annotations = map[string]string{
					workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 1),
				}
				resolved := newWholeUnitPriors(scaler, false)

				cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
					WithObjects(md, scaler).Build()
				ensureAdmittedUnitPods(t, cli, md, map[string]int32{"prefill": 1, "decode": 3})

				type podCounters struct {
					prompt int64
					gen    int64
					hist   int64
				}
				var mu sync.Mutex
				counters := map[string]*podCounters{}
				clockNow := t0.Add(-10 * time.Second)
				promptRatePerPod := 100.0
				genRatePerPod := 100.0
				lastScrapeAt := map[string]time.Time{}

				r := &ModelDeploymentScalerReconciler{
					Client:  cli,
					clock:   func() time.Time { return clockNow },
					sampler: &modelDeploymentScalerSampler{Clock: func() time.Time { return clockNow }},
				}
				r.sampler.Scrape = func(_ context.Context, pod *core.Pod) ([]byte, error) {
					mu.Lock()
					defer mu.Unlock()
					c, ok := counters[pod.Name]
					if !ok {
						c = &podCounters{hist: 10}
						counters[pod.Name] = c
						lastScrapeAt[pod.Name] = clockNow
					}
					if dt := clockNow.Sub(lastScrapeAt[pod.Name]).Seconds(); dt > 0 {
						role := modelDeploymentPodRole(pod)
						if role == "prefill" {
							c.prompt += int64(math.Round(promptRatePerPod * dt))
						} else {
							c.gen += int64(math.Round(genRatePerPod * dt))
						}
						c.hist += 10
						lastScrapeAt[pod.Name] = clockNow
					}
					var b strings.Builder
					fmt.Fprintf(&b, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 1\n")
					fmt.Fprintf(&b, "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting 0\n")
					fmt.Fprintf(&b, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc 0.1\n")
					fmt.Fprintf(&b, "# TYPE vllm:prompt_tokens_total counter\nvllm:prompt_tokens_total %d\n", c.prompt)
					fmt.Fprintf(&b, "# TYPE vllm:generation_tokens_total counter\nvllm:generation_tokens_total %d\n", c.gen)
					fmt.Fprintf(&b, "# TYPE vllm:prefix_cache_queries_total counter\nvllm:prefix_cache_queries_total %d\n", c.prompt)
					fmt.Fprintf(&b, "# TYPE vllm:prefix_cache_hits_total counter\nvllm:prefix_cache_hits_total 0\n")
					fmt.Fprintf(&b, "# TYPE vllm:time_to_first_token_seconds histogram\n")
					fmt.Fprintf(&b, "vllm:time_to_first_token_seconds_bucket{le=\"0.1\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:time_to_first_token_seconds_bucket{le=\"0.5\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:time_to_first_token_seconds_bucket{le=\"+Inf\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:time_to_first_token_seconds_count %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:time_to_first_token_seconds_sum %.1f\n", float64(c.hist)*0.1)
					fmt.Fprintf(&b, "# TYPE vllm:inter_token_latency_seconds histogram\n")
					fmt.Fprintf(&b, "vllm:inter_token_latency_seconds_bucket{le=\"0.01\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:inter_token_latency_seconds_bucket{le=\"0.05\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:inter_token_latency_seconds_bucket{le=\"+Inf\"} %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:inter_token_latency_seconds_count %d\n", c.hist)
					fmt.Fprintf(&b, "vllm:inter_token_latency_seconds_sum %.2f\n", float64(c.hist)*0.01)
					return []byte(b.String()), nil
				}

				step := func(at time.Time) {
					t.Helper()
					clockNow = at
					md = scalerTestGetDeployment(t, cli)
					scaler = scalerTestGetScaler(t, cli, scaler.Name)
					if _, err := r.planWithPriors(context.Background(), scaler.DeepCopy(), scaler, md, resolved, at, true); err != nil {
						t.Fatalf("planWithPriors(%s) error = %v", at, err)
					}
				}

				// Baseline pass at t0-10s initializes counter snapshots.
				step(t0.Add(-10 * time.Second))

				// Drive a 3 * C_unit panic spike at t0 (C_unit=5250 -> 15750 tok/s across 1P:3D).
				promptRatePerPod = 8000
				genRatePerPod = 8000.0 / 3.0
				step(t0)

				state := r.sampler.PredictorState(scaler, "decode")
				if state == nil || state.History == nil || state.History.Len() == 0 {
					t.Fatalf("expected non-empty state.History at panic spike, got %+v", state)
				}
				lastSample, ok := state.History.At(state.History.Len() - 1)
				if !ok || !lastSample.Timestamp.Equal(t0) {
					t.Fatalf("latest history sample = %+v (ok=%v), want timestamp %v", lastSample, ok, t0)
				}
				if wantExit := t0.Add(180 * time.Second); !state.PanicExitAt.Equal(wantExit) {
					t.Fatalf("PanicExitAt = %v, want %v", state.PanicExitAt, wantExit)
				}
				gotMD := scalerTestGetDeployment(t, cli)
				if gotVec := scalerTestVector(gotMD); gotVec["prefill"] < 2 || gotVec["decode"] < 6 {
					t.Fatalf("expected scale-up on panic spike, got vector %v", gotVec)
				}
				if gotScaler := scalerTestGetScaler(t, cli, scaler.Name); gotScaler.Status.Roles[1].Reason != algorithm.ReasonPanic {
					t.Fatalf("decode role reason = %q, want %q", gotScaler.Status.Roles[1].Reason, algorithm.ReasonPanic)
				}

				// Admit scaled-up pods and settle low demand well past stableWindow (t0 + 10m).
				scaledVec := scalerTestVector(gotMD)
				ensureAdmittedUnitPods(t, cli, gotMD, scaledVec)
				promptRatePerPod = 100
				genRatePerPod = 100
				step(t0.Add(20 * time.Second))

				for tick := 8*time.Minute + 50*time.Second; tick <= 10*time.Minute; tick += 10 * time.Second {
					step(t0.Add(tick))
				}

				afterState := r.sampler.PredictorState(scaler, "decode")
				if afterState == nil || afterState.History == nil || afterState.History.Len() == 0 {
					t.Fatalf("expected non-empty state.History after panic expiry")
				}
				if !afterState.PanicExitAt.IsZero() {
					t.Fatalf("expected PanicExitAt to expire, got %v", afterState.PanicExitAt)
				}
				finalVec := scalerTestVector(scalerTestGetDeployment(t, cli))
				if finalVec["prefill"] != 1 || finalVec["decode"] != 3 {
					t.Fatalf("expected scale-down to 1P:3D after panic expiry, got %v", finalVec)
				}
				finalScaler := scalerTestGetScaler(t, cli, scaler.Name)
				if gotReason := finalScaler.Status.Roles[1].Reason; gotReason != algorithm.ReasonScaleDown {
					t.Fatalf("decode role reason after panic expiry = %q, want %q", gotReason, algorithm.ReasonScaleDown)
				}

				// Verify planWholeUnitStates directly clears InPanic and HoldScaleDown after stableWindow.
				settledObs := map[string]algorithm.RoleObservation{
					"prefill": {
						Role: "prefill", Kind: algorithm.RoleKindPrefill,
						CurrentReplicas: 2, ReadyReplicas: 2,
						PromptTokensPerSecond: 100, TTFTMilliseconds: 100,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: true, ObservedAt: t0.Add(10*time.Minute + 10*time.Second),
					},
					"decode": {
						Role: "decode", Kind: algorithm.RoleKindDecode,
						CurrentReplicas: 6, ReadyReplicas: 6,
						GenerationTokensPerSecond: 100, ITLMilliseconds: 10,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: true, ObservedAt: t0.Add(10*time.Minute + 10*time.Second),
					},
				}
				twoUnitMD := scalerTestGetDeployment(t, cli)
				twoUnitMD.Spec.Roles[0].Replicas = 2
				twoUnitMD.Spec.Roles[1].Replicas = 6
				algByRole, _, roleStates, _, _, _, err := r.planWholeUnitStates(
					finalScaler, twoUnitMD, resolved, settledObs, nil, 2, t0.Add(10*time.Minute+10*time.Second),
				)
				if err != nil {
					t.Fatalf("planWholeUnitStates error = %v", err)
				}
				if algByRole["decode"].InPanic || algByRole["decode"].HoldScaleDown {
					t.Fatalf("expected InPanic=false and HoldScaleDown=false after expiry, got %+v", algByRole["decode"])
				}
				if roleStates[1].HoldScaleDown {
					t.Fatalf("expected role state HoldScaleDown=false after expiry, got %+v", roleStates[1])
				}
			})
		}
	})

	t.Run("observation timestamp fallback and max selection", func(t *testing.T) {
		pTime := t0.Add(-5 * time.Second)
		dTime := t0.Add(-2 * time.Second)
		cases := []struct {
			name   string
			pObsAt time.Time
			dObsAt time.Time
			wantAt time.Time
		}{
			{name: "both zero falls back to now", wantAt: t0},
			{name: "prefill only uses prefill timestamp", pObsAt: pTime, wantAt: pTime},
			{name: "decode only uses decode timestamp", dObsAt: dTime, wantAt: dTime},
			{name: "decode newer uses decode timestamp", pObsAt: pTime, dObsAt: dTime, wantAt: dTime},
			{name: "prefill newer uses prefill timestamp", pObsAt: dTime, dObsAt: pTime, wantAt: dTime},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 1, "decode": 3})
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
				scaler := newWholeUnitScaler(false)
				resolved := newWholeUnitPriors(scaler, false)
				cli := scalerTestClient(t, md, scaler)
				r := scalerTestReconciler(cli, t0)

				obs := map[string]algorithm.RoleObservation{
					"prefill": {
						Role: "prefill", Kind: algorithm.RoleKindPrefill,
						CurrentReplicas: 1, ReadyReplicas: 1,
						PromptTokensPerSecond: 1000, TTFTMilliseconds: 100,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: true, ObservedAt: tc.pObsAt,
					},
					"decode": {
						Role: "decode", Kind: algorithm.RoleKindDecode,
						CurrentReplicas: 3, ReadyReplicas: 3,
						GenerationTokensPerSecond: 1000, ITLMilliseconds: 10,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: true, ObservedAt: tc.dObsAt,
					},
				}
				_, unitObs, _ := buildWholeUnitObservation(md, resolved.WholeUnit, obs, 1, t0)
				if !unitObs.ObservedAt.Equal(tc.wantAt) {
					t.Fatalf("unitObs.ObservedAt = %v, want %v", unitObs.ObservedAt, tc.wantAt)
				}
				_, predictorByRole, _, _, _, _, err := r.planWholeUnitStates(
					scaler, md, resolved, obs, nil, 1, t0,
				)
				if err != nil {
					t.Fatalf("planWholeUnitStates error = %v", err)
				}
				pred := predictorByRole["decode"]
				if pred == nil || pred.History == nil || pred.History.Len() == 0 {
					t.Fatalf("expected recorded history sample, got %+v", pred)
				}
				sample, ok := pred.History.At(pred.History.Len() - 1)
				if !ok || !sample.Timestamp.Equal(tc.wantAt) {
					t.Fatalf("history sample timestamp = %v (ok=%v), want %v", sample.Timestamp, ok, tc.wantAt)
				}
			})
		}
	})

	t.Run("cross-role signal matrix", func(t *testing.T) {
		cases := []struct {
			name               string
			pThroughput        algorithm.SignalState
			dThroughput        algorithm.SignalState
			pLatency           algorithm.SignalState
			dLatency           algorithm.SignalState
			pTTFT              float64
			pTTFTUnmeasured    bool
			dITL               float64
			dITLUnmeasured     bool
			pComplete          bool
			dComplete          bool
			wantThroughput     algorithm.SignalState
			wantLatency        algorithm.SignalState
			wantTTFTUnmeasured bool
			wantITLUnmeasured  bool
			wantMetricsReady   bool
			wantReason         string
		}{
			{
				name:               "Fresh and Fresh",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalFresh,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalFresh,
				pTTFT:              100,
				dITL:               10,
				pComplete:          true,
				dComplete:          true,
				wantThroughput:     algorithm.SignalFresh,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: false,
				wantITLUnmeasured:  false,
				wantMetricsReady:   true,
				wantReason:         algorithm.ReasonTokenThroughput,
			},
			{
				name:               "Fresh and Idle with normal TTFT",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalIdle,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalIdle,
				pTTFT:              100,
				dITLUnmeasured:     true,
				pComplete:          true,
				dComplete:          true,
				wantThroughput:     algorithm.SignalFresh,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: false,
				wantITLUnmeasured:  true,
				wantMetricsReady:   false,
				wantReason:         algorithm.ReasonWaitingForMetrics,
			},
			{
				name:               "Idle and Idle",
				pThroughput:        algorithm.SignalIdle,
				dThroughput:        algorithm.SignalIdle,
				pLatency:           algorithm.SignalIdle,
				dLatency:           algorithm.SignalIdle,
				pTTFTUnmeasured:    true,
				dITLUnmeasured:     true,
				pComplete:          true,
				dComplete:          true,
				wantThroughput:     algorithm.SignalIdle,
				wantLatency:        algorithm.SignalIdle,
				wantTTFTUnmeasured: true,
				wantITLUnmeasured:  true,
				wantMetricsReady:   true,
				wantReason:         algorithm.ReasonTokenThroughput,
			},
			{
				name:               "Fresh and Missing",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalMissing,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalMissing,
				pTTFT:              100,
				dITLUnmeasured:     true,
				pComplete:          true,
				dComplete:          false,
				wantThroughput:     algorithm.SignalMissing,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: false,
				wantITLUnmeasured:  true,
				wantMetricsReady:   false,
				wantReason:         algorithm.ReasonWaitingForMetrics,
			},
			{
				name:               "Fresh and Stale",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalStale,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalStale,
				pTTFT:              100,
				dITLUnmeasured:     true,
				pComplete:          true,
				dComplete:          false,
				wantThroughput:     algorithm.SignalStale,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: false,
				wantITLUnmeasured:  true,
				wantMetricsReady:   false,
				wantReason:         algorithm.ReasonWaitingForMetrics,
			},
			{
				name:               "Fresh and Missing without any fresh latency",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalMissing,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalMissing,
				pTTFTUnmeasured:    true,
				dITLUnmeasured:     true,
				pComplete:          false,
				dComplete:          false,
				wantThroughput:     algorithm.SignalMissing,
				wantLatency:        algorithm.SignalMissing,
				wantTTFTUnmeasured: true,
				wantITLUnmeasured:  true,
				wantMetricsReady:   false,
				wantReason:         algorithm.ReasonWaitingForMetrics,
			},
			{
				name:               "Fresh and Stale without any fresh latency",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalStale,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalStale,
				pTTFTUnmeasured:    true,
				dITLUnmeasured:     true,
				pComplete:          false,
				dComplete:          false,
				wantThroughput:     algorithm.SignalStale,
				wantLatency:        algorithm.SignalStale,
				wantTTFTUnmeasured: true,
				wantITLUnmeasured:  true,
				wantMetricsReady:   false,
				wantReason:         algorithm.ReasonWaitingForMetrics,
			},
			{
				name:               "single-side TTFT fresh breach with ITL missing",
				pThroughput:        algorithm.SignalFresh,
				dThroughput:        algorithm.SignalMissing,
				pLatency:           algorithm.SignalFresh,
				dLatency:           algorithm.SignalMissing,
				pTTFT:              700,
				dITLUnmeasured:     true,
				pComplete:          true,
				dComplete:          true,
				wantThroughput:     algorithm.SignalMissing,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: false,
				wantITLUnmeasured:  true,
				wantMetricsReady:   true,
				wantReason:         algorithm.ReasonLatencyGuardScaleUp,
			},
			{
				name:               "single-side ITL fresh breach with TTFT missing",
				pThroughput:        algorithm.SignalMissing,
				dThroughput:        algorithm.SignalFresh,
				pLatency:           algorithm.SignalMissing,
				dLatency:           algorithm.SignalFresh,
				pTTFTUnmeasured:    true,
				dITL:               75,
				pComplete:          true,
				dComplete:          true,
				wantThroughput:     algorithm.SignalMissing,
				wantLatency:        algorithm.SignalFresh,
				wantTTFTUnmeasured: true,
				wantITLUnmeasured:  false,
				wantMetricsReady:   true,
				wantReason:         algorithm.ReasonLatencyGuardScaleUp,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				pHasTTFT := !tc.pTTFTUnmeasured && tc.pLatency == algorithm.SignalFresh
				dHasITL := !tc.dITLUnmeasured && tc.dLatency == algorithm.SignalFresh
				if got := combineWholeUnitThroughputState(tc.pThroughput, tc.dThroughput); got != tc.wantThroughput {
					t.Fatalf("combineWholeUnitThroughputState(%q, %q) = %q, want %q",
						tc.pThroughput, tc.dThroughput, got, tc.wantThroughput)
				}
				if got := combineWholeUnitLatencyState(tc.pLatency, tc.dLatency, pHasTTFT, dHasITL); got != tc.wantLatency {
					t.Fatalf("combineWholeUnitLatencyState(%q, %q, %v, %v) = %q, want %q",
						tc.pLatency, tc.dLatency, pHasTTFT, dHasITL, got, tc.wantLatency)
				}

				md := scalerTestDeployment("target", "target-uid", map[string]int32{"prefill": 1, "decode": 3})
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
				scaler := newWholeUnitScaler(false)
				resolved := newWholeUnitPriors(scaler, false)
				cli := scalerTestClient(t, md, scaler)
				r := scalerTestReconciler(cli, t0)

				obs := map[string]algorithm.RoleObservation{
					"prefill": {
						Role: "prefill", Kind: algorithm.RoleKindPrefill,
						CurrentReplicas: 1, ReadyReplicas: 1,
						PromptTokensPerSecond: 500, TTFTMilliseconds: tc.pTTFT, TTFTUnmeasured: tc.pTTFTUnmeasured,
						ThroughputState: tc.pThroughput, LatencyState: tc.pLatency,
						CompleteCoverage: tc.pComplete, ObservedAt: t0,
					},
					"decode": {
						Role: "decode", Kind: algorithm.RoleKindDecode,
						CurrentReplicas: 3, ReadyReplicas: 3,
						GenerationTokensPerSecond: 500, ITLMilliseconds: tc.dITL, ITLUnmeasured: tc.dITLUnmeasured,
						ThroughputState: tc.dThroughput, LatencyState: tc.dLatency,
						CompleteCoverage: tc.dComplete, ObservedAt: t0,
					},
				}
				_, unitObs, _ := buildWholeUnitObservation(md, resolved.WholeUnit, obs, 1, t0)
				if unitObs.ThroughputState != tc.wantThroughput || unitObs.LatencyState != tc.wantLatency {
					t.Fatalf("unitObs states = (%q, %q), want (%q, %q)",
						unitObs.ThroughputState, unitObs.LatencyState, tc.wantThroughput, tc.wantLatency)
				}
				if unitObs.TTFTUnmeasured != tc.wantTTFTUnmeasured || unitObs.ITLUnmeasured != tc.wantITLUnmeasured {
					t.Fatalf("unitObs unmeasured = (%v, %v), want (%v, %v)",
						unitObs.TTFTUnmeasured, unitObs.ITLUnmeasured, tc.wantTTFTUnmeasured, tc.wantITLUnmeasured)
				}

				algByRole, _, _, _, metricsReady, _, err := r.planWholeUnitStates(
					scaler, md, resolved, obs, nil, 1, t0,
				)
				if err != nil {
					t.Fatalf("planWholeUnitStates error = %v", err)
				}
				if metricsReady != tc.wantMetricsReady {
					t.Fatalf("metricsReady = %v, want %v", metricsReady, tc.wantMetricsReady)
				}
				if gotReason := algByRole["decode"].Reason; gotReason != tc.wantReason {
					t.Fatalf("decode reason = %q, want %q", gotReason, tc.wantReason)
				}
			})
		}
	})

	t.Run("partial unit readiness", func(t *testing.T) {
		cases := []struct {
			name                string
			includeEncode       bool
			current             map[string]int32
			ready               map[string]int32
			completeByRole      map[string]bool
			kCur                int32
			wantUnitReady       int32
			wantComplete        bool
			wantMetricsReady    bool
			wantDecodeReason    string
			wantHoldScaleDown   bool
			checkPlanWithPriors bool
		}{
			{
				name:              "1P:3D current 2P:6D ready (2,5) gives unitReady=1 and incomplete coverage",
				current:           map[string]int32{"prefill": 2, "decode": 6},
				ready:             map[string]int32{"prefill": 2, "decode": 5},
				completeByRole:    map[string]bool{"prefill": true, "decode": false},
				kCur:              2,
				wantUnitReady:     1,
				wantComplete:      false,
				wantMetricsReady:  false,
				wantDecodeReason:  algorithm.ReasonScaleDown,
				wantHoldScaleDown: true,
			},
			{
				name:              "1P:3D current 1P:3D ready (1,2) gives unitReady=0 and NoReadyReplicas",
				current:           map[string]int32{"prefill": 1, "decode": 3},
				ready:             map[string]int32{"prefill": 1, "decode": 2},
				completeByRole:    map[string]bool{"prefill": true, "decode": false},
				kCur:              1,
				wantUnitReady:     0,
				wantComplete:      false,
				wantMetricsReady:  false,
				wantDecodeReason:  algorithm.ReasonNoReadyReplicas,
				wantHoldScaleDown: true,
			},
			{
				name:                "1E:1P:2D with Encode unready gives unitReady=0 and MetricsReady=False",
				includeEncode:       true,
				current:             map[string]int32{"encode": 1, "prefill": 1, "decode": 2},
				ready:               map[string]int32{"encode": 0, "prefill": 1, "decode": 2},
				completeByRole:      map[string]bool{"encode": false, "prefill": true, "decode": true},
				kCur:                1,
				wantUnitReady:       0,
				wantComplete:        false,
				wantMetricsReady:    false,
				wantDecodeReason:    algorithm.ReasonNoReadyReplicas,
				wantHoldScaleDown:   true,
				checkPlanWithPriors: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				scaler := newWholeUnitScaler(false)
				roles := []workercore.ModelDeploymentRole{
					{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill, Replicas: tc.current["prefill"]},
					{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode, Replicas: tc.current["decode"]},
				}
				if tc.includeEncode {
					roles = append([]workercore.ModelDeploymentRole{{
						Name:     "encode",
						Kind:     workercore.ModelDeploymentRoleKindEncode,
						Replicas: tc.current["encode"],
					}}, roles...)
					scaler.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
						{Name: "encode", MinReplicas: 1, MaxReplicas: 4},
						{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
						{Name: "decode", MinReplicas: 2, MaxReplicas: 8},
					}
					scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{
						{NumeratorRole: "encode", DenominatorRole: "prefill", Min: quantityPointer("1"), Max: quantityPointer("1")},
						{NumeratorRole: "decode", DenominatorRole: "prefill", Min: quantityPointer("2"), Max: quantityPointer("2")},
					}
				}
				md := &workercore.ModelDeployment{
					ObjectMeta: meta.ObjectMeta{Namespace: "ns", Name: "target", UID: "target-uid", Generation: 1},
					Spec: workercore.ModelDeploymentSpec{
						Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
						Roles:  roles,
					},
				}
				md.Annotations = map[string]string{
					workercore.ModelDeploymentScalerBindingAnnotationKey: scalerTestBindingJSON(scaler, 1),
				}
				resolved := newWholeUnitPriors(scaler, tc.includeEncode)
				cli := scalerTestClient(t, md, scaler)
				r := scalerTestReconciler(cli, t0)

				obs := map[string]algorithm.RoleObservation{
					"prefill": {
						Role: "prefill", Kind: algorithm.RoleKindPrefill,
						CurrentReplicas: tc.current["prefill"], ReadyReplicas: tc.ready["prefill"],
						PromptTokensPerSecond: 500, TTFTMilliseconds: 100,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: tc.completeByRole["prefill"], ObservedAt: t0,
					},
					"decode": {
						Role: "decode", Kind: algorithm.RoleKindDecode,
						CurrentReplicas: tc.current["decode"], ReadyReplicas: tc.ready["decode"],
						GenerationTokensPerSecond: 500, ITLMilliseconds: 10,
						ThroughputState: algorithm.SignalFresh, LatencyState: algorithm.SignalFresh,
						CompleteCoverage: tc.completeByRole["decode"], ObservedAt: t0,
					},
				}
				if tc.includeEncode {
					obs["encode"] = algorithm.RoleObservation{
						Role: "encode", Kind: algorithm.RoleKindEncode,
						CurrentReplicas: tc.current["encode"], ReadyReplicas: tc.ready["encode"],
						CompleteCoverage: tc.completeByRole["encode"], ObservedAt: t0,
					}
				}

				_, unitObs, _ := buildWholeUnitObservation(md, resolved.WholeUnit, obs, tc.kCur, t0)
				if unitObs.ReadyReplicas != tc.wantUnitReady {
					t.Fatalf("unitObs.ReadyReplicas = %d, want %d", unitObs.ReadyReplicas, tc.wantUnitReady)
				}
				if unitObs.CompleteCoverage != tc.wantComplete {
					t.Fatalf("unitObs.CompleteCoverage = %v, want %v", unitObs.CompleteCoverage, tc.wantComplete)
				}

				algByRole, _, roleStates, _, metricsReady, _, err := r.planWholeUnitStates(
					scaler, md, resolved, obs, nil, tc.kCur, t0,
				)
				if err != nil {
					t.Fatalf("planWholeUnitStates error = %v", err)
				}
				if metricsReady != tc.wantMetricsReady {
					t.Fatalf("metricsReady = %v, want %v", metricsReady, tc.wantMetricsReady)
				}
				if got := algByRole["decode"]; got.Reason != tc.wantDecodeReason || got.HoldScaleDown != tc.wantHoldScaleDown {
					t.Fatalf("decode recommendation = %+v, want reason=%q holdScaleDown=%v",
						got, tc.wantDecodeReason, tc.wantHoldScaleDown)
				}
				for _, st := range roleStates {
					if st.CompleteCoverage != tc.wantComplete || st.HoldScaleDown != tc.wantHoldScaleDown {
						t.Fatalf("role state %s = %+v, want complete=%v hold=%v",
							st.Name, st, tc.wantComplete, tc.wantHoldScaleDown)
					}
				}

				if tc.checkPlanWithPriors {
					e2eCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
						WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.ModelDeploymentScaler{}, &core.Pod{}, &kueue.Workload{}).
						WithObjects(md.DeepCopy(), scaler.DeepCopy()).Build()
					ensureAdmittedUnitPods(t, e2eCli, md, map[string]int32{"prefill": 1, "decode": 2})
					e2eR := &ModelDeploymentScalerReconciler{
						Client:  e2eCli,
						clock:   func() time.Time { return t0 },
						sampler: &modelDeploymentScalerSampler{Clock: func() time.Time { return t0 }},
					}
					e2eR.sampler.Scrape = func(_ context.Context, _ *core.Pod) ([]byte, error) {
						return []byte("# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 0\n"), nil
					}
					e2eScaler := scalerTestGetScaler(t, e2eCli, scaler.Name)
					e2eMD := scalerTestGetDeployment(t, e2eCli)
					if _, err := e2eR.planWithPriors(
						context.Background(), e2eScaler.DeepCopy(), e2eScaler, e2eMD, resolved, t0, true,
					); err != nil {
						t.Fatalf("planWithPriors error = %v", err)
					}
					stored := scalerTestGetScaler(t, e2eCli, scaler.Name)
					cond := scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeMetricsReady)
					if cond.Status != meta.ConditionFalse {
						t.Fatalf("MetricsReady condition = %+v, want False", cond)
					}
				}
			})
		}
	})
}
