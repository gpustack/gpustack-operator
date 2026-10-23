package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestModelDeploymentScalerPredicateIgnoresStatusOnlyUpdates states which changes of a
// scaler wake its reconciler: the ones that change what the scaler is asked to do, and not
// the ones the reconciler itself writes.
func TestModelDeploymentScalerPredicateIgnoresStatusOnlyUpdates(t *testing.T) {
	predicate := modelDeploymentScalerPredicate()
	base := &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "ns",
			Name:       "scaler",
			UID:        types.UID("scaler-uid"),
			Generation: 3,
			Finalizers: []string{workercore.ModelDeploymentScalerFinalizerKey},
		},
	}
	statusOnly := base.DeepCopy()
	statusOnly.ResourceVersion = "2"
	statusOnly.Status.ObservedGeneration = 3
	statusOnly.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{
		TargetUID: "target-uid", TargetName: "target", Term: 1,
	}
	changedSpec := base.DeepCopy()
	changedSpec.Generation = 4
	changedSpec.Spec.Mode = workercore.ModelDeploymentScalerModeActive
	beingDeleted := base.DeepCopy()
	beingDeleted.DeletionTimestamp = &meta.Time{Time: time.Now()}
	changedFinalizer := base.DeepCopy()
	changedFinalizer.Finalizers = nil

	tests := []struct {
		name string
		old  *workercore.ModelDeploymentScaler
		new  *workercore.ModelDeploymentScaler
		want bool
	}{
		{
			name: "a status written by the reconciler is not a change of the scaler",
			old:  base,
			new:  statusOnly,
			want: false,
		},
		{
			name: "a changed spec is a change of the scaler",
			old:  base,
			new:  changedSpec,
			want: true,
		},
		{
			name: "a scaler on its way out is a change of the scaler",
			old:  base,
			new:  beingDeleted,
			want: true,
		},
		{
			name: "a changed finalizer is a change of the scaler",
			old:  base,
			new:  changedFinalizer,
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := predicate.Update(ctrlevent.UpdateEvent{ObjectOld: test.old, ObjectNew: test.new})
			if got != test.want {
				t.Fatalf("update = %v, want %v", got, test.want)
			}
		})
	}

	if !predicate.Create(ctrlevent.CreateEvent{Object: base}) {
		t.Fatal("a created scaler must wake the reconciler")
	}
	if !predicate.Delete(ctrlevent.DeleteEvent{Object: base}) {
		t.Fatal("a deleted scaler must wake the reconciler")
	}
	if !predicate.Generic(ctrlevent.GenericEvent{Object: base}) {
		t.Fatal("a generic event must wake the reconciler")
	}
}

// TestModelDeploymentScalerSkipsSteadyStateStatusWrites states that a scaler whose control
// plane does not move does not write its status on every tick, and that the window it needs
// between ticks is kept in memory rather than in the apiserver.
func TestModelDeploymentScalerSkipsSteadyStateStatusWrites(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	client := scalerTestClient(t, md, scaler)
	r := scalerTestReconciler(client, now)

	reconcileScaler(t, r, scaler.Name)
	first := scalerTestGetScaler(t, client, scaler.Name)
	if first.ResourceVersion == "" {
		t.Fatal("the first reconcile must write the status of the scaler")
	}

	for tick := 1; tick <= 4; tick++ {
		now = now.Add(15 * time.Second)
		r.clock = func() time.Time { return now }
		reconcileScaler(t, r, scaler.Name)
	}

	steady := scalerTestGetScaler(t, client, scaler.Name)
	if steady.ResourceVersion != first.ResourceVersion {
		t.Fatalf("resource version = %q, want %q: a steady scaler must not write its status",
			steady.ResourceVersion, first.ResourceVersion)
	}

	// The window the scaler needs between ticks is kept in memory, so it grows on every tick
	// whether the status was written or not.
	history := r.sampler.StabilizationHistory(steady, "server")
	if len(history) != 5 {
		t.Fatalf("stabilization history holds %d samples, want 5", len(history))
	}
}

// TestModelDeploymentScalerPredictorWarmsUpInMemoryWithoutPerTickStatusWrites states that a
// predictor warms up across ticks without a status write per tick, and that the status is
// written when the scaler leaves the warming up.
func TestModelDeploymentScalerPredictorWarmsUpInMemoryWithoutPerTickStatusWrites(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
	md.Spec.Engine = workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM}
	scaler := scalerTestScaler("scaler", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
		Type: workercore.ModelDeploymentScalerAlgorithmTypePredictive,
		Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{
			TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
				ServerTargetTokensPerSecond: quantityPointer("100"),
			},
			MinPoints: 3,
		},
	}
	pod := scalerStatusTestEnginePod(md, "server", types.UID("pod-uid"))
	client := scalerTestClient(t, md, scaler, pod)
	r := scalerTestReconciler(client, now)

	// The engine's counters grow on every tick, so the sampler reads a rate every tick.
	var served float64
	r.sampler.Scrape = func(context.Context, *core.Pod) ([]byte, error) {
		served += 1000
		return scalerStatusTestExposition(served, served/2), nil
	}

	versions := make([]string, 0, 6)
	for tick := 0; tick < 6; tick++ {
		if tick > 0 {
			now = now.Add(15 * time.Second)
			r.clock = func() time.Time { return now }
		}
		reconcileScaler(t, r, scaler.Name)
		versions = append(versions, scalerTestGetScaler(t, client, scaler.Name).ResourceVersion)
	}

	// The predictor counted every tick it saw a rate for, in memory.
	state := r.sampler.PredictorState(scalerTestGetScaler(t, client, scaler.Name), "server")
	if state == nil {
		t.Fatal("the sampler must keep the predictor state of the role")
	}
	if state.SampleCount < 3 {
		t.Fatalf("sample count = %d, want at least 3", state.SampleCount)
	}

	// The status was written when the scaler left the warming up, and not on every tick
	// after it: the last ticks carry the same resource version.
	if versions[len(versions)-1] != versions[len(versions)-2] {
		t.Fatalf("resource version = %q, want %q: a warm scaler must not write its status",
			versions[len(versions)-1], versions[len(versions)-2])
	}
	writes := 0
	for i := 1; i < len(versions); i++ {
		if versions[i] != versions[i-1] {
			writes++
		}
	}
	if writes == 0 {
		t.Fatal("the status of a scaler that warms up must be written at least once")
	}

	warmedVersion := versions[len(versions)-1]
	for tick := 0; tick < int(modelDeploymentScalerPredictorCheckpointStep); tick++ {
		now = now.Add(15 * time.Second)
		r.clock = func() time.Time { return now }
		reconcileScaler(t, r, scaler.Name)
	}
	checkpointed := scalerTestGetScaler(t, client, scaler.Name)
	if checkpointed.ResourceVersion == warmedVersion {
		t.Fatal("steady predictor must checkpoint status after modelDeploymentScalerPredictorCheckpointStep samples")
	}
	if len(checkpointed.Status.Predictors) != 1 || checkpointed.Status.Predictors[0].SampleCount < 3+modelDeploymentScalerPredictorCheckpointStep {
		t.Fatalf("checkpointed predictors = %+v, want SampleCount >= %d",
			checkpointed.Status.Predictors, 3+modelDeploymentScalerPredictorCheckpointStep)
	}
}

// scalerStatusTestEnginePod builds a ready engine Pod the reconciler finds through the
// identity labels and the owner reference of the deployment.
func scalerStatusTestEnginePod(
	md *workercore.ModelDeployment, role string, uid types.UID,
) *core.Pod {
	pod := scalerSamplerPod("engine-0", role, uid, 0)
	pod.Namespace = md.Namespace
	pod.Labels[modelDeploymentLabelKeyName] = modelDeploymentLabelValueName
	pod.Labels[modelDeploymentLabelKeyInstance] = md.Name
	pod.OwnerReferences = []meta.OwnerReference{{
		APIVersion: workercore.SchemeGroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        md.UID,
		Controller: ptr.To(true),
	}}
	return &pod
}

// scalerStatusTestExposition returns an engine exposition whose token counters grew since
// the last reading, so that the sampler reads a rate over the window between them.
func scalerStatusTestExposition(prompt, generation float64) []byte {
	return []byte(fmt.Sprintf(`# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="test"} 4.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="test"} 2.0
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="test"} %v
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="test"} %v
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_sum{engine="0",model_name="test"} 20.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="test"} 56.0
# TYPE vllm:inter_token_latency_seconds histogram
vllm:inter_token_latency_seconds_sum{engine="0",model_name="test"} 31.0
vllm:inter_token_latency_seconds_count{engine="0",model_name="test"} 5428.0
`, prompt, generation))
}
