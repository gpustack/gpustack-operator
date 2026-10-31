package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
)

func publishedSourceFixture() (*workercore.ModelArtifact, *workercore.ModelPrefetch) {
	artifact := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "weights", UID: "artifact", Generation: 1},
		Status:     workercore.ModelArtifactStatus{ObservedGeneration: 1, Resolved: &workercore.ModelArtifactResolved{ManifestDigest: modelTransferTestDigest}},
	}
	pf := &workercore.ModelPrefetch{
		ObjectMeta: meta.ObjectMeta{Namespace: artifact.Namespace, Name: "warm", UID: "prefetch", Generation: 2},
		Spec:       workercore.ModelPrefetchSpec{ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: artifact.Name}, Source: &workercore.ModelPrefetchSource{}},
		Status: workercore.ModelPrefetchStatus{
			Conditions: []gpustack.Condition{{Type: ModelPrefetchConditionSourceReady, Status: meta.ConditionTrue, ObservedGeneration: 2}},
			Transfer: &workercore.ModelPrefetchTransferStatus{Serving: &workercore.ModelPrefetchTransferServing{
				ArtifactUID: "artifact", ArtifactGeneration: 1, Digest: modelTransferTestDigest, ReadyLeaseExpiresAt: meta.NewTime(time.Now().Add(time.Hour)),
			}},
		},
	}
	return artifact, pf
}

func TestModelTransferSourceReadinessUsesPublishedPrefetchStatus(t *testing.T) {
	cases := []struct {
		name, change       string
		ready, alternative bool
	}{
		{name: "current proof", ready: true},
		{name: "expired lease", change: "expired"},
		{name: "stale prefetch revision", change: "prefetch revision"},
		{name: "stale artifact revision", change: "artifact revision"},
		{name: "artifact not observed", change: "artifact observation"},
		{name: "recreated artifact", change: "artifact uid"},
		{name: "changed digest", change: "digest"},
		{name: "disabled source", change: "disabled"},
		{name: "deleting source", change: "deleting"},
		{name: "no proof", change: "proof"},
		{name: "false readiness", change: "condition"},
		{name: "other artifact", change: "artifact name"},
		{name: "healthy alternative", change: "expired", alternative: true, ready: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			artifact, pf := publishedSourceFixture()
			alternative := pf.DeepCopy()
			alternative.Name = "alternative"
			switch tc.change {
			case "expired":
				pf.Status.Transfer.Serving.ReadyLeaseExpiresAt = meta.NewTime(time.Now().Add(-time.Minute))
			case "prefetch revision":
				pf.Generation++
			case "artifact revision":
				artifact.Generation++
				artifact.Status.ObservedGeneration++
			case "artifact observation":
				artifact.Status.ObservedGeneration = 0
			case "artifact uid":
				artifact.UID = "recreated"
			case "digest":
				artifact.Status.Resolved.ManifestDigest = "changed"
			case "disabled":
				pf.Spec.Source = nil
			case "deleting":
				now := meta.Now()
				pf.DeletionTimestamp = &now
				pf.Finalizers = []string{"hold"}
			case "proof":
				pf.Status.Transfer.Serving = nil
			case "condition":
				pf.Status.Conditions[0].Status = meta.ConditionFalse
			case "artifact name":
				pf.Spec.ArtifactRef.Name = "other"
			}
			objects := []ctrlcli.Object{pf}
			if tc.alternative {
				objects = append(objects, alternative)
			}
			lists := 0
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithIndex(&workercore.ModelPrefetch{}, modelPrefetchArtifactIndex, modelPrefetchArtifactIndexValues).WithObjects(objects...).
				WithInterceptorFuncs(ctrlinterceptor.Funcs{
					Get: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
						return fmt.Errorf("unexpected object read")
					},
					List: func(ctx context.Context, cli ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
						lists++
						options := new(ctrlcli.ListOptions).ApplyOptions(opts)
						require.IsType(t, &workercore.ModelPrefetchList{}, list)
						require.Equal(t, artifact.Namespace, options.Namespace)
						require.Equal(t, "spec.artifactRef.name=weights", options.FieldSelector.String())
						return cli.List(ctx, list, opts...)
					},
				}).Build()
			ready, until, err := modelDeploymentModelTransferSourceObserved(t.Context(), cli, artifact)
			require.NoError(t, err)
			require.Equal(t, tc.ready, ready)
			require.Equal(t, tc.ready, !until.IsZero())
			if tc.change != "artifact observation" {
				require.Equal(t, 1, lists)
			}
		})
	}
}

func TestModelPrefetchReadinessPredicateIgnoresProgressAndRenewal(t *testing.T) {
	cases := []struct {
		name, change string
		want         bool
	}{
		{name: "progress", change: "progress"},
		{name: "lease renewal", change: "renewal"},
		{name: "source gained", change: "gained", want: true},
		{name: "source lost", change: "lost", want: true},
		{name: "revision", change: "revision", want: true},
		{name: "artifact", change: "artifact", want: true},
	}
	predicate := modelPrefetchSourceReadinessChanged()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, old := publishedSourceFixture()
			next := old.DeepCopy()
			switch tc.change {
			case "progress":
				next.Status.Transfer.ReservedBytes++
			case "renewal":
				next.Status.Transfer.Serving.ReadyLeaseExpiresAt = meta.NewTime(time.Now().Add(2 * time.Hour))
			case "gained":
				old.Status.Transfer.Serving.ReadyLeaseExpiresAt = meta.NewTime(time.Now().Add(-time.Hour))
			case "lost":
				next.Status.Conditions[0].Status = meta.ConditionFalse
			case "revision":
				next.Generation++
			case "artifact":
				next.Spec.ArtifactRef.Name = "other"
			}
			require.Equal(t, tc.want, predicate.Update(ctrlevent.UpdateEvent{ObjectOld: old, ObjectNew: next}))
		})
	}
}

func TestModelTransferSourceLeaseRequeue(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		result ctrl.Result
		until  time.Time
		want   ctrl.Result
	}{
		{name: "no proof", result: ctrl.Result{RequeueAfter: time.Minute}, want: ctrl.Result{RequeueAfter: time.Minute}},
		{name: "lease expires sooner", result: ctrl.Result{RequeueAfter: time.Minute}, until: now.Add(time.Second), want: ctrl.Result{RequeueAfter: time.Second}},
		{name: "existing sooner", result: ctrl.Result{RequeueAfter: time.Second}, until: now.Add(time.Minute), want: ctrl.Result{RequeueAfter: time.Second}},
		{name: "proof expired during reconcile", until: now.Add(-time.Second), want: ctrl.Result{RequeueAfter: time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, modelTransferSourceLeaseRequeue(tc.result, tc.until, now))
		})
	}
}

func TestModelDeploymentSourcePrefetchMapsOnlyArtifactConsumers(t *testing.T) {
	_, pf := publishedSourceFixture()
	objects := make([]ctrlcli.Object, 0, 3)
	for _, item := range []struct{ name, namespace, artifact string }{
		{"matching", "team-a", "weights"}, {"other-artifact", "team-a", "elsewhere"}, {"other-namespace", "team-b", "weights"},
	} {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Name = item.name
			md.Namespace = item.namespace
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: item.artifact}
		})
		objects = append(objects, md)
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithIndex(&workercore.ModelDeployment{}, modelDeploymentArtifactIndex, modelDeploymentArtifactIndexValues).WithObjects(objects...).Build()
	r := &ModelDeploymentReconciler{Client: cli}
	requests, err := r.mapModelDeploymentSourcePrefetch(t.Context(), pf)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, "matching", requests[0].Name)
}

func TestModelPrefetchPublishesBoundedSummaryWithFullReadiness(t *testing.T) {
	_, pf := publishedSourceFixture()
	proof := pf.Status.Transfer.Serving
	source := &sourceObservation{admitted: true, charge: 123, serving: proof}
	for i := range 20 {
		source.generations = append(source.generations, workercore.ModelPrefetchTransferGeneration{Generation: fmt.Sprint(i), ReadyNodes: 1})
	}
	r := &ModelPrefetchReconciler{Now: time.Now}
	r.writeStatus(pf, prefetchProgress{}, nil, false, source, false)
	require.Len(t, pf.Status.Transfer.Generations, 16)
	require.Equal(t, int64(123), pf.Status.Transfer.ReservedBytes)
	require.Equal(t, proof, pf.Status.Transfer.Serving)
	condition := testCondition(pf.Status.Conditions, "SourceSummaryTruncated")
	require.NotNil(t, condition)
	require.Equal(t, meta.ConditionTrue, condition.Status)
	ready := testCondition(pf.Status.Conditions, ModelPrefetchConditionSourceReady)
	require.NotNil(t, ready)
	require.Equal(t, meta.ConditionTrue, ready.Status)
	source.authorityUnknown = true
	r.writeStatus(pf, prefetchProgress{}, nil, false, source, false)
	require.Nil(t, pf.Status.Transfer.Serving)
	ready = testCondition(pf.Status.Conditions, ModelPrefetchConditionSourceReady)
	require.Equal(t, meta.ConditionFalse, ready.Status)
}

type sourceInventoryClient struct {
	ctrlcli.Client
	lists map[string]int
}

func (c *sourceInventoryClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	c.lists[fmt.Sprintf("%T", list)]++
	return c.Client.List(ctx, list, opts...)
}

func TestModelPrefetchSourceObservationReusesDaemonSetInventory(t *testing.T) {
	r, cli, pf, _, _, _ := testSourceAuthorityFixture(t)
	counter := &sourceInventoryClient{Client: cli, lists: map[string]int{}}
	r.Client = counter
	reconcileModelPrefetch(t, r, pf.Namespace, pf.Name)
	require.Equal(t, 1, counter.lists["*v1.DaemonSetList"])
	got := getPrefetch(t, cli, pf.Namespace, pf.Name)
	require.NotNil(t, got.Status.Transfer.Serving)
}

func TestModelTransferSourceReadFailureDoesNotDecideDelivery(t *testing.T) {
	for _, policy := range []string{"Prefer", "Require"} {
		t.Run(policy, func(t *testing.T) {
			modelTransferPolicyStub(t, policy)
			artifact, _ := publishedSourceFixture()
			failure := fmt.Errorf("prefetch cache unavailable")
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}).
				WithInterceptorFuncs(ctrlinterceptor.Funcs{List: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectList, ...ctrlcli.ListOption) error {
					return failure
				}}).Build()
			render := &ModelDeploymentArtifactRender{Delivery: workercore.ModelDeploymentModelDeliveryNode}
			weights := &modelArtifactWeights{Render: render}
			err := applyModelDeploymentModelTransferPolicy(t.Context(), cli, weights, artifact, nil)
			require.ErrorIs(t, err, failure)
			require.Same(t, render, weights.Render)
			require.Zero(t, weights.sourceReadyUntil)
		})
	}
}
