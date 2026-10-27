package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

func TestModelDeploymentScalerCapacityPriorBindingAndRecovery(t *testing.T) {
	tests := []struct {
		name         string
		broken       bool
		readError    bool
		recovery     string
		wantReplicas int32
		wantBinding  bool
	}{
		{name: "unresolved cannot acquire binding", broken: true, wantReplicas: 1},
		{name: "resolved binds without inventing metrics", wantReplicas: 1, wantBinding: true},
		{name: "unresolved keeps cohort rollback", broken: true, recovery: "cohort", wantReplicas: 1, wantBinding: true},
		{name: "unresolved keeps retirement abort restore", broken: true, recovery: "retirement", wantReplicas: 2, wantBinding: true},
		{name: "cohort rollback preserves API error", readError: true, recovery: "cohort", wantReplicas: 1, wantBinding: true},
		{name: "retirement restore preserves API error", readError: true, recovery: "retirement", wantReplicas: 2, wantBinding: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			now := time.Unix(1000, 0)
			if tt.broken {
				cm.Data[priors.DefaultCatalogConfigMapKey] = "{"
			}
			if tt.recovery != "" {
				md.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey] = scalerTestBindingJSON(scaler, 1)
				scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: 1}
			}
			switch tt.recovery {
			case "cohort":
				md.Spec.Roles[0].Replicas = 3
				scaler.Status.PendingCohort = &workercore.ModelDeploymentScalerPendingCohortStatus{Term: 1, BaselineVector: map[string]int32{"server": 1}, TargetVector: map[string]int32{"server": 3}, StartedAt: timePointer(now.Add(-400 * time.Second))}
			case "retirement":
				md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{RoleName: "server", ReplicaOrdinal: 1, State: workercore.ModelDeploymentRetirementStateAborted}
				scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{Role: "server", Ordinal: 1, PreScaleDownVector: map[string]int32{"server": 2}, Outcome: workercore.ModelDeploymentScalerScaleDownOutcomeCommitted}
			}
			client := scalerTestClient(t, scaler, md, cm, instance)
			r := scalerTestReconciler(client, now)
			forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("denied"))
			if tt.readError {
				r.APIReader = capacityPriorErrorReader{Reader: client, kind: "*v1.ConfigMap", err: forbidden}
			}
			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(scaler)})
			if tt.readError {
				if !apierrors.IsForbidden(err) || !errors.Is(err, forbidden) {
					t.Errorf("recovery lost original API error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			stored := scalerTestGetScaler(t, client, scaler.Name)
			target := scalerTestGetDeployment(t, client)
			if bound := target.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey] != ""; bound != tt.wantBinding {
				t.Fatalf("binding exists = %v, want %v", bound, tt.wantBinding)
			}
			if target.Spec.Roles[0].Replicas != tt.wantReplicas {
				t.Fatalf("replicas = %d, want %d", target.Spec.Roles[0].Replicas, tt.wantReplicas)
			}
			ready := scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady)
			if (tt.broken || tt.readError) && (ready.Status != meta.ConditionFalse || ready.Reason != workercore.ModelDeploymentScalerReasonCapacityPriorUnresolved) {
				t.Fatalf("prior readiness = %#v", ready)
			}
			if !tt.broken && !tt.readError {
				if ready.Status != meta.ConditionTrue || len(stored.Status.ResolvedCapacityPriors) != 1 {
					t.Fatalf("missing resolution evidence: %#v", stored.Status)
				}
				if scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeMetricsReady).Status == meta.ConditionTrue {
					t.Fatal("catalog readiness invented live metrics")
				}
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorResetsRestoredState(t *testing.T) {
	tests := []struct {
		name, event string
		restart     bool
	}{
		{name: "catalog deletion clears memory", event: "catalog"},
		{name: "catalog deletion clears restored checkpoint", event: "catalog", restart: true},
		{name: "identity change clears restored checkpoint", event: "identity", restart: true},
		{name: "suspension clears both stores", event: "suspend"},
		{name: "target replacement clears both stores", event: "replace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			client := scalerTestClient(t, scaler, md, cm, instance)
			now := time.Unix(1000, 0)
			r := scalerTestReconciler(client, now)
			reconcileScaler(t, r, scaler.Name)
			stored := scalerTestGetScaler(t, client, scaler.Name)
			stored.Status.Predictors = []workercore.ModelDeploymentScalerPredictorStatus{{Name: "server", SampleCount: 32}}
			if err := client.Status().Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			r.sampler.RecordPredictorStates(stored, map[string]*algorithm.PredictorState{"server": {SampleCount: 32}})
			switch tt.event {
			case "catalog":
				if err := client.Delete(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			case "identity":
				live := scalerTestGetDeployment(t, client)
				live.Spec.Model.Name = "another/model"
				if err := client.Update(context.Background(), live); err != nil {
					t.Fatal(err)
				}
			case "suspend":
				stored.Spec.Suspend = true
				stored.Generation++
				if err := client.Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			case "replace":
				live := scalerTestGetDeployment(t, client)
				live.UID = "replacement"
				if err := client.Update(context.Background(), live); err != nil {
					t.Fatal(err)
				}
			}
			if tt.restart {
				r = scalerTestReconciler(client, now)
			}
			reconcileScaler(t, r, scaler.Name)
			stored = scalerTestGetScaler(t, client, scaler.Name)
			if len(stored.Status.Predictors) != 0 || len(stored.Status.ResolvedCapacityPriors) != 0 {
				t.Fatalf("stale persisted prior state survived %s: %#v", tt.event, stored.Status)
			}
			if scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady).Status == meta.ConditionTrue {
				t.Fatal("invalidated capacity evidence still reports ready")
			}
			if state := r.sampler.PredictorState(stored, "server"); state != nil {
				t.Fatalf("stale memory predictor survived: %#v", state)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorPrewriteRefresh(t *testing.T) {
	tests := []struct {
		name, change string
		wantError    bool
	}{
		{name: "ordinary replicas keep compatibility", change: "replicas"},
		{name: "catalog content changes stop stale plan", change: "catalog", wantError: true},
		{name: "catalog deletion stops stale plan", change: "delete", wantError: true},
		{name: "identity annotations stop stale plan", change: "identity", wantError: true},
		{name: "concurrent suspension stops stale plan", change: "suspend", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			cached := scalerTestClient(t, scaler, md, cm, instance)
			live := scalerTestClient(t, scaler.DeepCopy(), md.DeepCopy(), cm.DeepCopy(), instance.DeepCopy())
			planned, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), cached, scaler, md)
			if err != nil {
				t.Fatal(err)
			}
			switch tt.change {
			case "replicas", "identity":
				target := scalerTestGetDeployment(t, live)
				if tt.change == "replicas" {
					target.Spec.Roles[0].Replicas++
				} else {
					delete(target.Annotations, workercore.ModelDeploymentScalerModelIDAnnotationKey)
				}
				if err := live.Update(context.Background(), target); err != nil {
					t.Fatal(err)
				}
			case "suspend":
				stored := scalerTestGetScaler(t, live, scaler.Name)
				stored.Spec.Suspend = true
				if err := live.Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			case "catalog":
				document := capacityPriorReadCatalog(t, cm)
				document.Profiles[0].Points[1].TotalTokensPerSecond = 1800
				stored := new(core.ConfigMap)
				if err := live.Get(context.Background(), ctrlcli.ObjectKeyFromObject(cm), stored); err != nil {
					t.Fatal(err)
				}
				capacityPriorWriteCatalog(t, stored, document)
				if err := live.Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := live.Delete(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			}
			r := scalerTestReconciler(cached, time.Unix(1000, 0))
			r.APIReader = live
			err = r.recheckCapacityPriors(context.Background(), scaler, md, planned)
			if (err != nil) != tt.wantError {
				t.Fatalf("refresh error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorCatalogWatch(t *testing.T) {
	scaler, md, cm, instance := capacityPriorCompleteFixture(t)
	unrelated := scaler.DeepCopy()
	unrelated.Name, unrelated.UID = "unrelated", "unrelated"
	unrelated.Spec.Algorithm.CapacityPrior.CatalogRef.Name = "different-catalog"
	manual := scaler.DeepCopy()
	manual.Name, manual.UID = "manual", "manual"
	manual.Spec.Algorithm.CapacityPrior = nil
	client := scalerTestClient(t, scaler, unrelated, manual, md, cm, instance)
	r := scalerTestReconciler(client, time.Unix(1000, 0))
	got := r.mapCapacityPriorCatalog(context.Background(), cm)
	if len(got) != 1 || got[0].Name != scaler.Name || got[0].Namespace != scaler.Namespace {
		t.Fatalf("catalog requests = %#v", got)
	}
}

func TestModelDeploymentScalerCapacityPriorStaleFailureInvalidation(t *testing.T) {
	tests := []struct {
		name, event, reason string
		removePrior         bool
	}{
		{name: "removed then suspended", event: "suspend", removePrior: true, reason: workercore.ModelDeploymentScalerReasonSuspended},
		{name: "removed with missing target", event: "missing", removePrior: true, reason: workercore.ModelDeploymentScalerReasonTargetNotFound},
		{name: "removed with replaced target", event: "replaced", removePrior: true, reason: workercore.ModelDeploymentScalerReasonTargetReplaced},
		{name: "requested then suspended", event: "suspend", reason: workercore.ModelDeploymentScalerReasonSuspended},
		{name: "requested with missing target", event: "missing", reason: workercore.ModelDeploymentScalerReasonTargetNotFound},
		{name: "requested with replaced target", event: "replaced", reason: workercore.ModelDeploymentScalerReasonTargetReplaced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			cm.Data[priors.DefaultCatalogConfigMapKey] = "{"
			client := scalerTestClient(t, scaler, md, cm, instance)
			r := scalerTestReconciler(client, time.Unix(1000, 0))
			reconcileScaler(t, r, scaler.Name)
			stored := scalerTestGetScaler(t, client, scaler.Name)
			if scalerCondition(stored, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady).Status != meta.ConditionFalse {
				t.Fatal("setup must record a prior resolution failure")
			}
			if tt.removePrior {
				stored.Spec.Algorithm.CapacityPrior = nil
			}
			stored.Generation++
			stored.Spec.Suspend = tt.event == "suspend"
			if err := client.Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			switch tt.event {
			case "missing":
				if err := client.Delete(context.Background(), md); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				stored.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: "old-target", TargetName: md.Name, Term: 1}
				stored.Status.ObservedGeneration = stored.Generation
				if err := client.Status().Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			}
			reconcileScaler(t, r, scaler.Name)
			ready := scalerCondition(scalerTestGetScaler(t, client, scaler.Name), workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady)
			if ready.Status != meta.ConditionUnknown || ready.Reason != tt.reason {
				t.Fatalf("stale prior failure after %s: %#v", tt.event, ready)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorSkipsSteadyStatusWrites(t *testing.T) {
	scaler, md, cm, instance := capacityPriorCompleteFixture(t)
	pod := scalerStatusTestEnginePod(md, "server", "engine-uid")
	client := scalerTestClient(t, scaler, md, cm, instance, pod)
	now := time.Unix(1000, 0)
	r := scalerTestReconciler(client, now)
	var served, increment float64
	r.sampler.Scrape = func(context.Context, *core.Pod) ([]byte, error) {
		increment += 100
		served += increment
		return scalerStatusTestExposition(served, served/2), nil
	}
	for range 4 {
		reconcileScaler(t, r, scaler.Name)
		now = now.Add(15 * time.Second)
		r.clock = func() time.Time { return now }
	}
	first := scalerTestGetScaler(t, client, scaler.Name)
	if len(first.Status.ResolvedCapacityPriors) != 1 {
		t.Fatalf("expected a resolved catalog capacity: %#v", first.Status)
	}
	fingerprint := first.Status.ResolvedCapacityPriors[0].CompatibilityFingerprint
	priorSample := r.sampler.RoleSamples(first)["server"].Observation
	historyLength := len(r.sampler.StabilizationHistory(first, "server"))
	for range 4 {
		reconcileScaler(t, r, scaler.Name)
		stored := scalerTestGetScaler(t, client, scaler.Name)
		if stored.ResourceVersion != first.ResourceVersion {
			t.Fatalf("steady catalog scaler wrote status: version %s became %s", first.ResourceVersion, stored.ResourceVersion)
		}
		if stored.Status.ResolvedCapacityPriors[0].CompatibilityFingerprint != fingerprint {
			t.Fatal("live observations changed capacity compatibility")
		}
		now = now.Add(15 * time.Second)
		r.clock = func() time.Time { return now }
	}
	latestSample := r.sampler.RoleSamples(first)["server"].Observation
	if priorSample.PromptTokensPerSecond == latestSample.PromptTokensPerSecond {
		t.Fatal("live throughput must change during the steady-state check")
	}
	if len(r.sampler.StabilizationHistory(first, "server")) <= historyLength {
		t.Fatal("in-memory history did not grow while status writes were suppressed")
	}
	after := first.DeepCopy()
	after.Status.ResolvedCapacityPriors[0].Fields[0].EffectiveTokensPerSecond = resource.MustParse("1.5k")
	if err := r.updateStatus(context.Background(), first, after); err != nil {
		t.Fatal(err)
	}
	if stored := scalerTestGetScaler(t, client, scaler.Name); stored.ResourceVersion != first.ResourceVersion {
		t.Fatal("semantically equal capacity quantities triggered a status write")
	}
	after.Status.ResolvedCapacityPriors[0].Fields[0].EffectiveTokensPerSecond = resource.MustParse("1501")
	if err := r.updateStatus(context.Background(), first, after); err != nil {
		t.Fatal(err)
	}
	if stored := scalerTestGetScaler(t, client, scaler.Name); stored.ResourceVersion == first.ResourceVersion {
		t.Fatal("a changed effective capacity must write status")
	}
}
