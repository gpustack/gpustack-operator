package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// The JSON contract the two observer patches serve (DESIGN §1.2/§1.3). One healthy
// payload every other test derives from.
const observerHealthyPayload = `{
  "router": {"boot_generation": 1700000000000, "now_ms": 1700000001000},
  "registry_revision": 7,
  "workers": [
    {
      "url": "http://10.0.0.5:8000",
      "port": "8000",
      "role": "regular",
      "pod_hint": "10.0.0.5",
      "worker_id": "wid-1",
      "selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true}
    },
    {
      "url": "http://10.0.0.6:8000",
      "port": "8000",
      "role": "prefill",
      "pod_hint": "10.0.0.6",
      "worker_id": "wid-2",
      "selection": {"registered": true, "healthy": false, "circuit_open": true, "in_selection": false}
    }
  ]
}`

// observationMemberPort is the engine port a bound worker URL names. It is a fixture constant
// because the payload and the Pod's own declared ports have to agree: the binder refuses a URL
// whose port the bound Pod never declared, which is the check that stops a row being counted
// against a listener that member is not serving.
const observationMemberPort int32 = 8000

// observationLivePod builds a member Pod the binder can place: the workload identity labels the
// render stamps on every member, and the declared listener its metrics URL names.
//
// THE IDENTITY IS NOT OPTIONAL IN A FIXTURE. The binder derives the role, the member rank and the
// listener from the bound Pod alone, and refuses an endpoint that carries none of them, so a Pod
// without the rendered labels exercises the refusal rather than the count.
func observationLivePod(name, ip string, uid types.UID, running bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			UID:       uid,
			Labels: map[string]string{
				modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:  "qwen",
				modelDeploymentLabelKeyComponent: "regular",
				modelDeploymentMemberIndexLabel:  "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "engine",
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: observationMemberPort}},
		}}},
		Status: corev1.PodStatus{PodIP: ip},
	}
	// The controlling owner is what makes this Pod one of the deployment's members, and the
	// collection's own fresh listing filters on exactly this reference. A member with no owner is
	// not counted by the cache either, so a fixture without it would measure an empty pool.
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: workercore.SchemeGroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       "qwen",
		UID:        "md-uid",
		Controller: ptr.To(true),
	}}
	if running {
		pod.Status.Phase = corev1.PodRunning
	} else {
		pod.Status.Phase = corev1.PodFailed
	}

	return pod
}

// TestParseRouterObservationView pins the schema-strict parse: a payload the observer
// patches serve parses fully, and anything else — an unknown field, a missing router
// block, an unsupported per-plugin dump — is an error the caller reports as an unusable
// view, never as an empty one.
func TestParseRouterObservationView(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		payload    string
		wantErr    string
		assertView func(t *testing.T, view RouterServingView)
	}{
		{
			name:    "the observer payload parses fully",
			payload: observerHealthyPayload,
			assertView: func(t *testing.T, view RouterServingView) {
				assert.Equal(t, uint64(1700000000000), view.RouterBootGeneration)
				assert.Equal(t, uint64(7), view.RegistryRevision)
				require.Len(t, view.Workers, 2)
				assert.Equal(t, "http://10.0.0.5:8000", view.Workers[0].URL)
				assert.Equal(t, "8000", view.Workers[0].Port)
				assert.True(t, view.Workers[0].Selection.InSelection)
				assert.False(t, view.Workers[1].Selection.Healthy)
				assert.True(t, view.Workers[1].Selection.CircuitOpen)
				assert.False(t, view.Workers[1].Selection.InSelection)
			},
		},
		{
			name: "an unknown field rejects the payload instead of ignoring it",
			payload: `{
				"router": {"boot_generation": 1, "now_ms": 2},
				"registry_revision": 1,
				"workers": [],
				"timestamp": "2026-10-02T00:00:00Z"
			}`,
			wantErr: "unknown field",
		},
		{
			name: "a per-plugin debug dump shape is not a membership view",
			payload: `{
				"timestamp": "2026-10-02T00:00:00Z",
				"plugins": {"picker": {"name": "picker", "message": "plugin does not support state collection"}}
			}`,
			wantErr: "unknown field",
		},
		{
			name: "a payload with no router block is rejected, never read as generation zero",
			payload: `{
				"registry_revision": 1,
				"workers": []
			}`,
			wantErr: "boot_generation",
		},
		{
			name: "the gateway view with its operational fields parses as served",
			payload: `{
				"router": {"boot_generation": 9, "now_ms": 10},
				"registry_revision": 3,
				"workers": [
					{
						"url": "http://10.0.0.7:8000",
						"port": "8000",
						"role": "regular",
						"pod_hint": "10.0.0.7",
						"worker_id": "wid-9",
						"selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true},
						"lane": {"busy": false, "waiting": 0},
						"last_job": {"job_type": "add", "worker_url": "http://10.0.0.7:8000", "status": "success", "message": null, "timestamp": 1700000000}
					}
				]
			}`,
			assertView: func(t *testing.T, view RouterServingView) {
				require.Len(t, view.Workers, 1)
				require.NotNil(t, view.Workers[0].Lane)
				assert.False(t, view.Workers[0].Lane.Busy)
				assert.Equal(t, 0, view.Workers[0].Lane.Waiting)
				assert.Contains(t, string(view.Workers[0].LastJob), `"status": "success"`,
					"the raw job status is carried as the gateway wrote it")
			},
		},
		{
			name: "an explicit empty worker array is a valid view of an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": []
			}`,
			assertView: func(t *testing.T, view RouterServingView) {
				assert.NotNil(t, view.Workers,
					"an explicit empty array is present membership, not absent membership")
				assert.Empty(t, view.Workers)
			},
		},
		{
			// The routers serialize a non-Option Vec, so the array is always on the wire.
			// Its absence is therefore a payload this operator cannot read, and an empty
			// membership claimed by a payload that never stated one is exactly the
			// fabricated zero the answer must never be.
			name: "an omitted workers array is rejected, never read as an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2
			}`,
			wantErr: "workers",
		},
		{
			name: "a null workers array is rejected, never read as an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": null
			}`,
			wantErr: "workers",
		},
		{
			name: "a workers value that is not an array is rejected as malformed",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": {"url": "http://10.0.0.5:8000"}
			}`,
			wantErr: "membership view",
		},
		{
			name: "a workers entry that is not an object is rejected as malformed",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": ["http://10.0.0.5:8000"]
			}`,
			wantErr: "membership view",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			view, err := parseRouterObservationView([]byte(tc.payload))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, strings.ToLower(err.Error()), tc.wantErr)

				return
			}
			require.NoError(t, err)
			tc.assertView(t, view)
		})
	}
}

// TestBindRouterObservationView pins collection-time identity binding: a serving worker is
// bound to the live Pod its address resolves to, the binding follows the live Pod object
// rather than the URL's history, and an address that resolves to nothing live makes the
// whole view indeterminate.
func TestBindRouterObservationView(t *testing.T) {
	t.Parallel()

	livePod := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)

	testCases := []struct {
		name      string
		view      RouterServingView
		pods      []*corev1.Pod
		wantBound []ServingEndpoint
		wantErr   string
	}{
		{
			name: "a serving worker binds to the live pod at its address",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true, Registered: true, Healthy: true},
				},
			}},
			pods: []*corev1.Pod{livePod},
			wantBound: []ServingEndpoint{{
				PodUID: "pod-uid-1", Member: "server-0", Role: "regular", Rank: "0",
				Listener: observationMemberPort,
				URL:      "http://10.0.0.5:8000", Port: "8000", Serving: true,
			}},
		},
		{
			name: "the binding follows the live pod, so a same-name replacement binds to the new UID",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods: []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-new", true)},
			wantBound: []ServingEndpoint{{
				PodUID: "pod-uid-new", Member: "server-0", Role: "regular", Rank: "0",
				Listener: observationMemberPort,
				URL:      "http://10.0.0.5:8000", Port: "8000", Serving: true,
			}},
		},
		{
			name: "an address no live pod holds makes the view indeterminate",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.9:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods:    []*corev1.Pod{livePod},
			wantErr: "indeterminate",
		},
		{
			name: "an address whose only pod is not running makes the view indeterminate",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.5:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: true},
				},
			}},
			pods:    []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-1", false)},
			wantErr: "indeterminate",
		},
		{
			name: "a worker outside the selection is never bound and never fails the view",
			view: RouterServingView{Workers: []RouterObservedWorker{
				{
					URL: "http://10.0.0.9:8000", Port: "8000", Role: "regular",
					Selection: RouterObservedSelection{InSelection: false},
				},
			}},
			pods:      []*corev1.Pod{livePod},
			wantBound: []ServingEndpoint{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bound, _, err := bindRouterObservationView(tc.view, tc.pods, false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBound, bound)
		})
	}
}

// TestAggregateModelDeploymentServing pins the Feature 2 state machine: the count is the
// deduplicated union across complete views, replicas that disagree are NotConverged, any
// missing/stale/indeterminate view is Unknown, an unconfigured router is NotConfigured, and
// a confirmed zero is a real explicit zero.
func TestAggregateModelDeploymentServing(t *testing.T) {
	t.Parallel()

	fresh := time.Now()
	freshness := 30 * time.Second

	// viewOf builds one Router process's observation. The router UID is explicit in every
	// case, because boot generation is a per-process property: two replicas started at
	// different times are expected to report different generations, and a case that means
	// "two independent routers" has to say so rather than leave it to a bare generation.
	viewOf := func(routerUID types.UID, gen uint64, at time.Time, serving bool, uids ...string) RouterServingObservation {
		view := RouterServingView{RouterPodUID: routerUID, RouterBootGeneration: gen}
		bound := make([]ServingEndpoint, 0, len(uids))
		for _, uid := range uids {
			bound = append(bound, ServingEndpoint{PodUID: types.UID(uid), Serving: serving})
		}

		return RouterServingObservation{View: view, CollectedAt: at, Bound: bound}
	}

	testCases := []struct {
		name         string
		configured   bool
		observations []RouterServingObservation
		freshness    time.Duration
		wantState    workercore.ModelDeploymentServingState
		wantValue    *int32
	}{
		{
			// The two views are two replicas, so they carry distinct identities; their
			// generations are held equal here so this case asserts the union rule alone.
			// The generation rule is asserted separately below.
			name:       "the union deduplicates endpoints two views both hold",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a", "pod-b"),
				viewOf("router-b", 1, fresh, true, "pod-b", "pod-c"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(3)),
		},
		{
			name:       "views that disagree about one endpoint are NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a"),
				viewOf("router-b", 1, fresh, false, "pod-a"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name:         "a configured router with no collected view is Unknown, never zero",
			configured:   true,
			observations: []RouterServingObservation{},
			freshness:    freshness,
			wantState:    workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "an unconfigured router is NotConfigured with no value",
			configured: false,
			wantState:  workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name:       "one unusable view makes the answer Unknown",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true, "pod-a"),
				{Err: errors.New("dial failed")},
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "a stale view makes the answer Unknown",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh.Add(-2*freshness), true, "pod-a"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			// Boot generation is the Router process's start time, so two replicas started
			// at different times are expected to differ. Comparing one scalar across
			// distinct processes made every multi-replica deployment permanently
			// NotConverged, so withdrawal could never confirm.
			name:       "independent router processes with their own boot generations converge",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-b", 1700000009000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(2)),
		},
		{
			name:       "two reads of one router process agreeing on its generation converge",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-a", 1700000000000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(2)),
		},
		{
			// A Router that rolled mid-observation still means the views describe two
			// different processes, so the same process identity reporting two generations
			// is the case that has to stay NotConverged.
			name:       "one router identity reporting two generations is NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1700000000000, fresh, true, "pod-a"),
				viewOf("router-a", 1700000009000, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			// Identity-less views cannot be placed in any process group, so they are
			// held to one another: a disagreement among them still means two processes,
			// and dropping the check would read a roll as convergence.
			name:       "views whose router identity is unknown must still agree with each other",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("", 1, fresh, true, "pod-a"),
				viewOf("", 2, fresh, true, "pod-b"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name:       "a gateway-shaped view parses, binds and aggregates like any other",
			configured: true,
			observations: func() []RouterServingObservation {
				view, err := parseRouterObservationView([]byte(`{
					"router": {"boot_generation": 9, "now_ms": 10},
					"registry_revision": 3,
					"workers": [
						{
							"url": "http://10.0.0.5:8000",
							"port": "8000",
							"role": "regular",
							"pod_hint": "10.0.0.5",
							"worker_id": "wid-9",
							"selection": {"registered": true, "healthy": true, "circuit_open": false, "in_selection": true},
							"lane": {"busy": false, "waiting": 0},
							"last_job": {"job_type": "add", "worker_url": "http://10.0.0.5:8000", "status": "success", "message": null, "timestamp": 1700000000}
						}
					]
				}`))
				if err != nil {
					panic(err)
				}

				return []RouterServingObservation{{
					View:        view,
					CollectedAt: fresh,
					Bound: []ServingEndpoint{{
						PodUID: "pod-a", Member: "server-0", Role: "regular",
						URL: "http://10.0.0.5:8000", Port: "8000", Serving: true,
					}},
				}}
			}(),
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(1)),
		},
		{
			name:       "a confirmed zero is the explicit empty set",
			configured: true,
			observations: []RouterServingObservation{
				viewOf("router-a", 1, fresh, true),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(0)),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := aggregateModelDeploymentServing(tc.configured, tc.observations, tc.freshness)
			assert.Equal(t, tc.wantState, answer.State)
			assert.Equal(t, tc.wantValue, answer.Value)
		})
	}
}

// TestCollectModelDeploymentServingView pins the collection boundary: the view is read
// directly from one router pod's observer endpoint over the passed fetch, a transport or
// schema failure is that view's error (never an empty success), and the collected view
// carries the router pod's UID as its identity.
func TestCollectModelDeploymentServingView(t *testing.T) {
	t.Parallel()

	t.Run("a served payload binds to the target pod", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(observerHealthyPayload))
		}))
		defer server.Close()

		// The test server stands in for the router pod's observer port; the fetch function
		// the production code passes resolves the pod IP to the server's address.
		view, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return []byte(observerHealthyPayload), nil
			},
			"router-pod-uid",
		)
		require.NoError(t, err)
		assert.Equal(t, types.UID("router-pod-uid"), view.View.RouterPodUID)
		assert.Len(t, view.View.Workers, 2)
		_ = server.URL
	})

	t.Run("a transport failure is the view's error, never an empty success", func(t *testing.T) {
		t.Parallel()

		view, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return nil, errors.New("connection refused")
			},
			"router-pod-uid",
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connection refused")
		assert.True(t, view.Empty())
	})

	t.Run("a served non-JSON payload is the view's error", func(t *testing.T) {
		t.Parallel()

		_, err := collectRouterServingView(
			context.Background(),
			func(_ context.Context, _ string) ([]byte, error) {
				return []byte("<html>not the observer</html>"), nil
			},
			"router-pod-uid",
		)
		require.Error(t, err)
	})
}

// TestOneCollectionBoundsEveryRouterUnderOneDeadline covers the budget itself, which is the part
// that cannot be checked by looking at a single call.
//
// EACH READ CARRIES THE SAME CONTEXT. The collection derives one deadline and every Router's fetch
// is made under it, so a second Router cannot start fresh and a slow first one cannot be paid for
// twice. A caller's own deadline is kept when it has one, because the collection is part of a
// larger pass and must not outlive it.
func TestOneCollectionBoundsEveryRouterUnderOneDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caller    func() (context.Context, context.CancelFunc)
		wantBound bool
	}{
		{
			name:      "an unlimited caller is still given a finite deadline",
			caller:    func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			wantBound: true,
		},
		{
			name: "a caller's own deadline is kept",
			caller: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 3*time.Second)
			},
			wantBound: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.caller()
			defer cancel()

			before := time.Now()
			readCtx, stop := modelDeploymentObservationContext(ctx)
			defer stop()

			_, bounded := readCtx.Deadline()
			require.Equal(t, tc.wantBound, bounded, "an observation read must never be unbounded")
			deadline, ok := readCtx.Deadline()
			require.True(t, ok)
			assert.True(t, deadline.After(before), "the deadline is in the future")
			assert.True(t, deadline.Before(before.Add(modelDeploymentServingFreshness)),
				"and inside the window the aggregator will still call fresh")
		})
	}
}

// TestAnUnlimitedCallerCannotOutliveItsReadBudget is the same rule from the other side: canceling
// what the caller passed has to stop the read, which is what makes a controller's own cancellation
// reach a collection that derived its own deadline.
func TestAnUnlimitedCallerCannotOutliveItsReadBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readCtx, stop := modelDeploymentObservationContext(ctx)
	defer stop()
	cancel()

	<-readCtx.Done()
	assert.ErrorIs(t, readCtx.Err(), context.Canceled)
}

// TestTheObserverTransportRefusesABodyItShouldNot carries covers the body bound with the real
// transport rather than a stubbed one.
//
// A Router that streams forever, or that answers with something enormous, is bounded here. The
// positive beside them is the ordinary case: a membership view is a few hundred workers and reads
// back unchanged.
func TestTheObserverTransportRefusesABodyItShouldNot(t *testing.T) {
	const view = `{"router":{"boot_generation":1700000000000,"now_ms":1700000001000},` +
		`"registry_revision":7,"workers":[]}`

	for _, tc := range []struct {
		name    string
		body    func(w http.ResponseWriter)
		wantErr string
	}{
		{
			name: "an ordinary membership view reads back unchanged",
			body: func(w http.ResponseWriter) { _, _ = w.Write([]byte(view)) },
		},
		{
			name: "a body past the cap is refused",
			body: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(view))
				chunk := make([]byte, 1<<20)
				for range 5 {
					_, _ = w.Write(chunk)
				}
			},
			wantErr: "byte bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.body(w)
			}))
			defer server.Close()

			ctx, cancel := modelDeploymentObservationContext(context.Background())
			defer cancel()

			raw, err := defaultServingViewFetch(ctx, server.URL)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, view, string(raw))
		})
	}
}

// observationDeadlineTransport answers like a Router and records the deadline of every request, so
// the collection's shared budget can be observed rather than inferred.
type observationDeadlineTransport struct {
	mu        sync.Mutex
	deadlines []time.Time
	view      string
}

func (transport *observationDeadlineTransport) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	deadline, bounded := request.Context().Deadline()
	transport.mu.Lock()
	if bounded {
		transport.deadlines = append(transport.deadlines, deadline)
	} else {
		transport.deadlines = append(transport.deadlines, time.Time{})
	}
	transport.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(transport.view)),
		Header:     make(http.Header),
		Request:    request,
	}, nil
}

// TestEveryRouterOfOneCollectionSharesOneDeadline is the shared budget measured where it matters:
// on the requests the Routers actually received.
//
// A HELPER that derives a deadline proves nothing about the collection, because the collection could
// still call the transport with the caller's own context. Here two Router Pods are read through the
// production transport, and both requests must carry the same bounded deadline. A second Router that
// started fresh would be a pass whose length is the sum of the Routers' choices.
func TestEveryRouterOfOneCollectionSharesOneDeadline(t *testing.T) {
	md, _ := retirementRouterFixture(newRenderDeployment())
	md.Generation = 1
	routers := []*corev1.Pod{
		retirementRouterPod("router-zero", "10.0.9.1"),
		retirementRouterPod("router-one", "10.0.9.2"),
	}
	for _, router := range routers {
		router.Status.PodIP = routerIP(t, router)
	}

	transport := &observationDeadlineTransport{view: observerHealthyPayload}
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = original })

	// The Router processes carry their full controlling chain, because the collection verifies it
	// before it dials anything: a fixture that seeded only the Pods would measure the hold an
	// unplaceable process produces and read as a pass that never reached the transport.
	chain := ownedRouterObjectsFor(md, routers[0], routers[1])
	seeded := append([]ctrlcli.Object{md}, chain...)
	seeded = append(seeded, observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true))
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(seeded...).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(ctrlcli.Object) []string { return nil }).
		Build()
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, []*corev1.Pod{observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)})
	require.Empty(t, failure, "both routers answer the same view: %s", failure)
	require.Len(t, observations, 2, "both routers are read")

	require.Len(t, transport.deadlines, 2, "each router is read once")
	for _, deadline := range transport.deadlines {
		require.False(t, deadline.IsZero(), "every outbound request carries a deadline")
	}
	assert.Equal(t, transport.deadlines[0], transport.deadlines[1],
		"both routers run under one derived deadline, not one each")
}

// routerIP reads a Router Pod's own address out of its fixture.
func routerIP(t *testing.T, pod *corev1.Pod) string {
	t.Helper()

	live := pod.DeepCopy()
	live.Status.PodIP = "10.0.9.1"
	if pod.Name == "router-one" {
		live.Status.PodIP = "10.0.9.2"
	}

	return live.Status.PodIP
}

// TestTheParserRefusesAPayloadThatDoesNotFinish covers the two malformed shapes a decoder would
// otherwise accept silently: a null worker row, and a second JSON value after the payload.
func TestTheParserRefusesAPayloadThatDoesNotFinish(t *testing.T) {
	const complete = `{"registered":true,"healthy":true,"circuit_open":false,"in_selection":true}`

	for _, tc := range []struct {
		name   string
		view   string
		wantOK bool
	}{
		{
			name:   "a complete view parses",
			view:   `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[{"url":"http://10.0.0.5:8000","selection":` + complete + `}]}`,
			wantOK: true,
		},
		{
			name: "a null worker row is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[null]}`,
		},
		{
			name: "a trailing JSON value is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}{"router":{"boot_generation":9}}`,
		},
		{
			name: "trailing content after the payload is refused",
			view: `{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]} and then some`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := parseRouterObservationView([]byte(tc.view))
			if tc.wantOK {
				require.NoError(t, err)
				require.Len(t, view.Workers, 1)

				return
			}
			require.Error(t, err, "a payload that does not fully state the view is not a view")
		})
	}
}

type discoveryDeadlineClient struct {
	ctrlcli.Client
	deadlines []time.Time
}

func (c *discoveryDeadlineClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	deadline, _ := ctx.Deadline()
	c.deadlines = append(c.deadlines, deadline)
	return c.Client.List(ctx, list, opts...)
}

func TestObservationBudgetCoversDiscoveryAndRemainingRetirement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		residual bool
		caller   time.Duration
		phaseAge time.Duration
		overall  time.Duration
		want     time.Duration
	}{
		{name: "serving discovery with unlimited caller", overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "residual discovery with unlimited caller", residual: true, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "serving generous caller is capped", caller: time.Hour, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "residual generous caller is capped", residual: true, caller: time.Hour, overall: time.Minute, want: modelDeploymentObservationBudget},
		{name: "serving keeps shorter caller", caller: 3 * time.Second, overall: time.Minute, want: 3 * time.Second},
		{name: "residual keeps shorter caller", residual: true, caller: 3 * time.Second, overall: time.Minute, want: 3 * time.Second},
		{name: "serving respects remaining withdrawal phase", phaseAge: 29 * time.Second, overall: time.Minute, want: time.Second},
		{name: "residual respects remaining withdrawal phase", residual: true, phaseAge: 29 * time.Second, overall: time.Minute, want: time.Second},
		{name: "serving respects remaining overall operation", overall: time.Second, want: time.Second},
		{name: "residual respects remaining overall operation", residual: true, overall: time.Second, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, _ := retirementRouterFixture(newRenderDeployment())
			now := time.Now()
			md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
				State:          workercore.ModelDeploymentRetirementStateWithdrawing,
				PhaseStartedAt: metav1.NewTime(now.Add(-tc.phaseAge)),
				Deadline:       metav1.NewTime(now.Add(tc.overall)),
			}
			router := retirementRouterPod("router-zero", "10.0.9.1")
			routerRS, routerDeployment := ownedRouterChain(router, md)
			base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(md, router, &routerRS, &routerDeployment).Build()
			recorder := &discoveryDeadlineClient{Client: base}
			reads := 0
			r := &ModelDeploymentReconciler{Client: recorder, APIReader: recorder, clock: func() time.Time { return now }, servingViewFetch: func(ctx context.Context, _ string) ([]byte, error) {
				reads++
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, deadline.Sub(now), tc.want+500*time.Millisecond)
				return []byte(`{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}`), nil
			}}
			ctx := context.Background()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.caller)
				defer cancel()
			}
			if tc.residual {
				reason, held := r.observeModelDeploymentRetirementResidual(ctx, md, sets.New[types.UID]("target"))
				require.False(t, held, reason)
			} else {
				answer := r.observeModelDeploymentServing(ctx, md)
				require.Equal(t, workercore.ModelDeploymentServingStateConfirmed, answer.State, answer.Reason)
				require.NotNil(t, answer.Value)
				require.EqualValues(t, 0, *answer.Value)
			}
			require.Equal(t, 1, reads, "actual Router fetch reached")
			require.GreaterOrEqual(t, len(recorder.deadlines), 2, "worker discovery and Router discovery both reached")
			for _, deadline := range recorder.deadlines {
				require.False(t, deadline.IsZero(), "every discovery is bounded")
				require.LessOrEqual(t, deadline.Sub(now), tc.want+500*time.Millisecond)
			}
		})
	}
}

// journalWorker is one observed worker, stated as facts rather than as a handler. The maps are the
// literal JSON the shipped observer serves, so a row cannot drift into a shape the producer does
// not emit.
type journalWorker map[string]any

// journalWorkerFact builds one observed worker at a live Pod's own address. The address is the Pod
// list's, never a literal here, because the collection binds a worker's address to the Pod it
// resolves to at read time and refuses a view it cannot bind.
func journalWorkerFact(ip, uid string, inSelection bool, lastJob map[string]any) journalWorker {
	worker := journalWorker{
		"url":  "http://" + ip + ":8000",
		"port": "8000", "role": "server",
		"pod_hint": uid, "worker_id": uid,
		"selection": map[string]any{
			"registered": inSelection, "healthy": inSelection,
			"circuit_open": false, "in_selection": inSelection,
		},
	}
	if lastJob != nil {
		worker["last_job"] = lastJob
	}

	return worker
}

// journalView renders the view the shipped llm-d observer serves for a set of workers. A failure to
// encode is a broken fixture rather than a case under test, so it stops the test where it is
// reported instead of carrying a panic into the collection.
func journalView(t *testing.T, workers []journalWorker) []byte {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"router":            map[string]any{"boot_generation": 1, "now_ms": 1},
		"registry_revision": 1,
		"workers":           workers,
	})
	require.NoError(t, err)

	return payload
}

// ownedRouterChain gives a Router process the live controlling chain the render produces —
// Pod, ReplicaSet, workload Deployment, ModelDeployment — and returns the two apps objects the
// fixture must seed so the collection's ownership verification finds the real rendered shape.
//
// THE DEPLOYMENT IS ONE OBJECT PER MODELDEPLOYMENT, NOT ONE PER POD. Every Router process of a
// deployment is scaled by the same workload Deployment, so that Deployment has one name and one
// UID whatever else varies. Deriving its UID from the Pod made two Router fixtures build the same
// name with different identities, which the API cannot hold: the second write is rejected and a
// fixture that only means to place two routers cannot be built at all.
func ownedRouterChain(pod *corev1.Pod, md *workercore.ModelDeployment) (appsv1.ReplicaSet, appsv1.Deployment) {
	rsUID := types.UID("rs-uid-" + pod.Name)
	deploymentUID := types.UID("deployment-uid-router-" + md.Namespace + "-" + md.Name)
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: appsv1.SchemeGroupVersion.String(),
		Kind:       "ReplicaSet",
		Name:       pod.Name + "-rs",
		UID:        rsUID,
		Controller: &controller,
	}}
	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name + "-rs",
			Namespace: pod.Namespace,
			UID:       rsUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(),
				Kind:       "Deployment",
				Name:       md.Name + "-router",
				UID:        deploymentUID,
				Controller: &controller,
			}},
		},
	}
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      md.Name + "-router",
			Namespace: pod.Namespace,
			UID:       deploymentUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeployment",
				Name:       md.Name,
				UID:        md.UID,
				Controller: &controller,
			}},
		},
	}

	return replicaSet, deployment
}

// ownedRouterObjects returns the one Router process a fixture needs together with every live object
// its controlling chain names, ready to spread into an object list.
//
// A fixture that seeds only the Pod is not a faithful positive path: the collection verifies every
// hop of the chain through the uncached reader and holds a process it cannot place, so a fixture
// that omits the ReplicaSet and Deployment measures the hold rather than the answer.
func ownedRouterObjects(md *workercore.ModelDeployment) []ctrlcli.Object {
	pod := retirementRouterPod("router-zero", "10.0.9.1")
	replicaSet, deployment := ownedRouterChain(pod, md)

	return []ctrlcli.Object{pod, &replicaSet, &deployment}
}

// ownedRouterObjectsFor is ownedRouterObjects for a fixture that places more than one Router process.
// Each process gets its own ReplicaSet, because a ReplicaSet's name is derived from its Deployment
// and a hash and two processes of one Deployment are two different sets; the Deployment they share
// is seeded once, because it is one object.
func ownedRouterObjectsFor(
	md *workercore.ModelDeployment, pods ...*corev1.Pod,
) []ctrlcli.Object {
	objects := make([]ctrlcli.Object, 0, 2*len(pods)+1)
	var shared *appsv1.Deployment
	for _, pod := range pods {
		replicaSet, deployment := ownedRouterChain(pod, md)
		objects = append(objects, pod, &replicaSet)
		if shared == nil {
			shared = &deployment
		}
	}

	return append(objects, shared)
}

// TestRetirementResidualReadsTheDispatchJournal asks the real residual function, with the real
// collector, about a target the router removed from its selection.
//
// The shipped observer keeps a removed worker in the view with in_selection false and marks the
// dispatches it recently sent to it under last_job.recent_dispatch. The binder refuses to count a
// worker outside the selection as serving, which is right, so the journal is the only remaining
// evidence that a dispatch reached the target. Before it was read, a target with a recent dispatch
// read as a target with nothing left serving it, and the operation released it.
func TestRetirementResidualReadsTheDispatchJournal(t *testing.T) {
	recent := map[string]any{"recent_dispatch": true}
	notRecent := map[string]any{"recent_dispatch": false}

	for _, tc := range []struct {
		name     string
		workers  []journalWorker
		status   int
		wantHeld bool
	}{
		{
			name:     "a removed captured target with a recent dispatch holds the release",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, recent)},
			wantHeld: true,
		},
		{
			name:     "a complete view with no target selection and no dispatch clears this check",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, nil)},
			wantHeld: false,
		},
		{
			name:     "a journal that says the dispatch was not recent does not hold",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, notRecent)},
			wantHeld: false,
		},
		{
			name: "recent dispatch for an unrelated survivor does not hold the captured target",
			workers: []journalWorker{
				journalWorkerFact("10.0.1.1", "member-1", false, nil),
				journalWorkerFact("10.0.1.2", "other-1", false, recent),
			},
			wantHeld: false,
		},
		{
			// A journal block the producer does not emit is a block this operator cannot read.
			// Reading it as false would be a decoder default turning unreadable evidence into
			// confirmed absence of a dispatch, which is the one thing the journal must never be.
			name:     "a journal without the boolean holds rather than confirming absence",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", false, map[string]any{"job_type": "add"})},
			wantHeld: true,
		},
		{
			name:     "a selectable captured target is still held by the serving count",
			workers:  []journalWorker{journalWorkerFact("10.0.1.1", "member-1", true, nil)},
			wantHeld: true,
		},
		{
			// The observer answers 503 while a selection or send is pending, which is exactly the
			// window in which a dispatch may be in flight and unrecorded. No supported shape, no
			// fresh timestamp and no absent journal can stand in for that answer.
			name:     "an actual native 503 remains unknown and holds",
			workers:  nil,
			status:   http.StatusServiceUnavailable,
			wantHeld: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, _ := retirementRouterFixture(newRenderDeployment())
			pods := []corev1.Pod{
				*surplusReplicaAt(t, md, "qwen-server-one", 1, ""),
				*surplusReplicaAt(t, md, "qwen-server-zero", 0, ""),
			}
			pods[0].UID = "member-1"
			pods[1].UID = "other-1"
			ips := []string{"10.0.1.1", "10.0.1.2"}
			objects := make([]ctrlcli.Object, 0, 3+len(pods))
			routerProcess := retirementRouterPod("router-zero", "10.0.9.1")
			routerRS, routerDeployment := ownedRouterChain(routerProcess, md)
			objects = append(objects, md, routerProcess, &routerRS, &routerDeployment)
			for i := range pods {
				pods[i].Status.Phase = corev1.PodRunning
				pods[i].Status.PodIP = ips[i]
				objects = append(objects, pods[i].DeepCopy())
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			r := &ModelDeploymentReconciler{
				Client: client, APIReader: client, clock: time.Now,
				servingViewFetch: func(context.Context, string) ([]byte, error) {
					if status != http.StatusOK {
						return nil, fmt.Errorf("observer answered %d", status)
					}

					return journalView(t, tc.workers), nil
				},
			}

			reason, held := r.observeModelDeploymentRetirementResidual(
				context.Background(), md, sets.New[types.UID]("member-1"))

			assert.Equal(t, tc.wantHeld, held, reason)
		})
	}
}

// TestTheObserverIsReadFromTheOwningProfileListener pins the listener each profile serves the
// observer on. The llm-d patches attach it to the management listener the router already runs;
// the other two serve it on the request port their Service already targets, and their contract is
// unchanged.
func TestTheObserverIsReadFromTheOwningProfileListener(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    int32
	}{
		{profile: "llm-d-router", want: modelDeploymentRouterMetricsPort},
		{profile: "vllm-router", want: modelDeploymentRouterHTTPPort},
		{profile: "sglang-gateway", want: modelDeploymentRouterHTTPPort},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentRouterObserverPort(tc.profile))
		})
	}
}

func TestObserverTransportCancelsStalledHeadersAndBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flush bool
	}{{"headers never arrive", false}, {"body never finishes", true}} {
		t.Run(tc.name, func(t *testing.T) {
			reached := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.flush {
					_, _ = w.Write([]byte("{"))
					w.(http.Flusher).Flush()
				}
				close(reached)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := defaultServingViewFetch(ctx, server.URL)
			require.True(t, errors.Is(err, context.DeadlineExceeded), "actual transport must stop on caller deadline: %v", err)
			select {
			case <-reached:
			default:
				t.Fatal("server stall was never reached")
			}
		})
	}
}

// TestRouterViewCollectedAtIsStampedAfterItsOwnRead pins the per-view stamp (I10) without any
// timing guess: the reconciler's injected clock advances once per read, so a slow first read is
// visible as a strictly earlier stamp on the first view than on the second. A stamp taken once
// before the loop would make the two views equal here, and a slow first fetch would pre-age the
// last view toward the freshness edge in production.
func TestRouterViewCollectedAtIsStampedAfterItsOwnRead(t *testing.T) {
	md, router := retirementRouterFixture(newRenderDeployment())
	routerTwo := retirementRouterPod("router-one", "10.0.9.2")
	chain := ownedRouterObjectsFor(md, router, routerTwo)
	base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(append([]ctrlcli.Object{md}, chain...)...).Build()

	tick := 0
	baseNow := time.Now()
	r := &ModelDeploymentReconciler{
		Client: base, APIReader: base,
		clock: func() time.Time {
			tick++
			return baseNow.Add(time.Duration(tick) * time.Minute)
		},
		servingViewFetch: func(_ context.Context, _ string) ([]byte, error) {
			return []byte(`{"router":{"boot_generation":1,"now_ms":2},"registry_revision":1,"workers":[]}`), nil
		},
	}

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md,
		[]*corev1.Pod{retirementRouterPod("router-zero", "10.0.9.1"), retirementRouterPod("router-one", "10.0.9.2")},
	)
	require.Empty(t, failure)
	require.Len(t, observations, 2)
	assert.True(t, observations[0].CollectedAt.Before(observations[1].CollectedAt),
		"each view is stamped after its own read, not from one pre-loop timestamp: %v vs %v",
		observations[0].CollectedAt, observations[1].CollectedAt)
}

// seedRouterOwnership gives every Router process of a deployment its live controlling chain — Pod,
// ReplicaSet, workload Deployment, ModelDeployment — and seeds the apps objects that chain names,
// so a collection that verifies ownership finds the real rendered shape.
func seedRouterOwnership(client ctrlcli.Client, md *workercore.ModelDeployment) error {
	routers := new(corev1.PodList)
	if err := client.List(context.Background(), routers,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{modelDeploymentRouterLabelKey: md.Spec.Router.Name}); err != nil {
		return err
	}
	for i := range routers.Items {
		routerPod := &routers.Items[i]
		// A PROCESS THE RECONCILER HAS ALREADY RENDERED IS LEFT ALONE. The chain objects carry the
		// names the real render gives them, so seeding a second chain here would put a foreign
		// object under the reconciler's own names; the sync that maintains those names would then
		// find it different from its own render on every pass and rewrite it forever. A Pod that
		// already carries a controlling owner is one whose chain the reconciler owns.
		if metav1.GetControllerOf(routerPod) != nil {
			continue
		}
		replicaSet, deployment := ownedRouterChain(routerPod, md)
		if err := client.Update(context.Background(), routerPod); err != nil {
			return err
		}
		if err := client.Create(context.Background(), &replicaSet); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := client.Create(context.Background(), &deployment); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}

// seedHealthyRouterOwnership is seedRouterOwnership for the convergence-only positives whose
// fixture path seeds no Router objects itself.
func seedHealthyRouterOwnership(t *testing.T, client ctrlcli.Client, md *workercore.ModelDeployment) {
	t.Helper()
	// A deployment that declares no router has declared its absence, and there is nothing to seed:
	// writing a chain here would invent a Router the spec does not have.
	if md.Spec.Router == nil {
		return
	}
	require.NoError(t, seedRouterOwnership(client, md))
}

// liveModelDeploymentFor reads the deployment a fixture seeded, without a *testing.T, so a fixture
// builder can complete its own seeding before it hands a reconciler back to the test.
func liveModelDeploymentFor(cli ctrlcli.Client) *workercore.ModelDeployment {
	list := new(workercore.ModelDeploymentList)
	if err := cli.List(context.Background(), list); err != nil || len(list.Items) == 0 {
		return nil
	}

	return &list.Items[0]
}

// ownedRouterFixture returns a Router process carrying its live controlling chain together with
// the ReplicaSet and Deployment objects that chain names, ready for a fixture's objects list.
func ownedRouterFixture(md *workercore.ModelDeployment) (*corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment) {
	router := retirementRouterPod("router-zero", "10.0.9.1")
	replicaSet, deployment := ownedRouterChain(router, md)

	return router, &replicaSet, &deployment
}

// TestABoundEndpointNeedsTheIdentityTheWorkloadStates pins the binding acceptance: a bound
// endpoint is counted under the role, rank and listener the bound Pod itself declares, and a row
// whose Pod cannot state all three is refused rather than counted with a gap.
//
// The observer's own worker_id is never a source. It is a native registry identity that enumerates
// virtual ranks on a data-parallel member, and reading it as a member rank would report the
// router's registry size rather than the pool's serving members.
func TestABoundEndpointNeedsTheIdentityTheWorkloadStates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		port    string
		mutate  func(*corev1.Pod)
		wantErr string
	}{
		{
			name: "a member with its rendered role, rank and declared listener binds",
			port: "8000",
		},
		{
			name: "a pod that declares no workload role is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, modelDeploymentLabelKeyComponent)
			},
			wantErr: "carries no workload role",
		},
		{
			name: "a pod that declares no member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				delete(pod.Labels, modelDeploymentMemberIndexLabel)
			},
			wantErr: "carries no member rank",
		},
		{
			// The render writes a decimal index counting from zero. A label a user edited into
			// something else is not an ordinal, and reading it as one would name a member that
			// does not exist.
			name: "a non-decimal member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				pod.Labels[modelDeploymentMemberIndexLabel] = "leader"
			},
			wantErr: "rather than a decimal index",
		},
		{
			name: "a signed member rank is refused",
			port: "8000",
			mutate: func(pod *corev1.Pod) {
				pod.Labels[modelDeploymentMemberIndexLabel] = "-1"
			},
			wantErr: "rather than a decimal index",
		},
		{
			// A port the Pod never declared is not a listener it can be serving on, so a row
			// naming one is a row about nothing this member owns.
			name: "a port the member never declared is refused",
			port: "9999",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Ports = nil
			},
			wantErr: "declares no container port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			view := RouterServingView{Workers: []RouterObservedWorker{{
				URL:       "http://10.0.0.5:" + tc.port,
				Port:      tc.port,
				Selection: RouterObservedSelection{InSelection: true, Registered: true, Healthy: true},
			}}}

			bound, _, err := bindRouterObservationView(view, []*corev1.Pod{pod}, false)
			if tc.wantErr != "" {
				require.Error(t, err, "an endpoint the workload cannot identify is not an endpoint")
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}
			require.NoError(t, err)
			require.Len(t, bound, 1)
			assert.Equal(t, "regular", bound[0].Role)
			assert.Equal(t, "0", bound[0].Rank)
			assert.Equal(t, observationMemberPort, bound[0].Listener)
		})
	}
}

// TestTheServingCountFoldsVirtualRowsOnOnePhysicalListener pins the union rule: several rows a
// router registers for one data-parallel member are ONE physical listener, and the count reports
// the members serving rather than the rows received. It also pins that a contradiction refuses
// convergence while agreeing views and an explicit empty registry stay accepted.
func TestTheServingCountFoldsVirtualRowsOnOnePhysicalListener(t *testing.T) {
	t.Parallel()

	// One member, four virtual ranks: the same Pod and the same declared listener, which is the
	// shape a data-parallel engine registers.
	virtuals := func() []RouterObservedWorker {
		workers := make([]RouterObservedWorker, 0, 4)
		for i := 0; i < 4; i++ {
			workers = append(workers, RouterObservedWorker{
				URL: "http://10.0.0.5:8000", Port: "8000", WorkerID: string(rune('a' + i)),
				Selection: RouterObservedSelection{InSelection: true},
			})
		}

		return workers
	}
	selected := func(workers []RouterObservedWorker, inSelection bool) RouterServingObservation {
		return RouterServingObservation{
			View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1, Workers: workers},
			CollectedAt: time.Now(),
			Bound: []ServingEndpoint{{
				PodUID: "pod-uid-1", Role: "regular", Rank: "0",
				Listener: observationMemberPort, Serving: inSelection,
			}},
		}
	}
	bound := func(workers []RouterObservedWorker) RouterServingObservation {
		observation := selected(workers, true)
		bound, _, err := bindRouterObservationView(observation.View, []*corev1.Pod{
			observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true),
		}, false)
		require.NoError(t, err)
		observation.Bound = bound

		return observation
	}

	for _, tc := range []struct {
		name         string
		observations []RouterServingObservation
		wantState    workercore.ModelDeploymentServingState
		wantValue    int32
		wantReason   string
	}{
		{
			name:         "four virtual rows on one listener are one serving member",
			observations: []RouterServingObservation{bound(virtuals())},
			wantState:    workercore.ModelDeploymentServingStateConfirmed,
			wantValue:    1,
		},
		{
			name:         "two routers that agree on the same physical member count it once",
			observations: []RouterServingObservation{bound(virtuals()), bound(virtuals())},
			wantState:    workercore.ModelDeploymentServingStateConfirmed,
			wantValue:    1,
		},
		{
			// A router that selects a listener another refuses is not a lagging view; it is two
			// answers about the same member, and a count built on either one is a guess.
			name: "routers that disagree about one physical member refuse convergence",
			observations: []RouterServingObservation{
				bound(virtuals()), selected(virtuals(), false),
			},
			wantState:  workercore.ModelDeploymentServingStateNotConverged,
			wantReason: "disagree",
		},
		{
			// The same contradiction inside one view is caught by the same fold, because the union
			// is walked row by row and the refused rows are carried rather than dropped.
			name: "one view that both selects and refuses a listener refuses convergence",
			observations: []RouterServingObservation{
				func() RouterServingObservation {
					observation := bound(virtuals())
					observation.Bound = append(observation.Bound, ServingEndpoint{
						PodUID: "pod-uid-1", Role: "regular", Rank: "0",
						Listener: observationMemberPort, Serving: false,
					})

					return observation
				}(),
			},
			wantState:  workercore.ModelDeploymentServingStateNotConverged,
			wantReason: "disagree",
		},
		{
			name: "an explicit empty registry is a confirmed zero",
			observations: []RouterServingObservation{{
				View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1},
				CollectedAt: time.Now(),
			}},
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := aggregateModelDeploymentServing(
				true, tc.observations, modelDeploymentServingFreshness)
			assert.Equal(t, tc.wantState, answer.State, answer.Reason)
			if tc.wantValue == 0 && tc.wantState != workercore.ModelDeploymentServingStateConfirmed {
				return
			}
			if answer.Value != nil {
				assert.Equal(t, tc.wantValue, *answer.Value, answer.Reason)
			}
			if tc.wantReason != "" {
				assert.Contains(t, answer.Reason, tc.wantReason)
			}
		})
	}
}

// TestARouterProcessIsHeldUnlessEveryChainHopIsTheRenderedOne pins the ownership chain: each
// live hop of Pod, ReplicaSet, Deployment and ModelDeployment is checked by kind, API version,
// name and identity, and a hop that is missing, unreadable or different holds the process rather
// than narrowing the answer.
func TestARouterProcessIsHeldUnlessEveryChainHopIsTheRenderedOne(t *testing.T) {
	t.Parallel()

	md := newRenderDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	faithful := func(t *testing.T) *ModelDeploymentReconciler {
		pod, replicaSet, deployment := ownedRouterFixture(md)
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(md.DeepCopy(), pod, replicaSet, deployment).Build()

		return &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, cli ctrlcli.Client)
		replace *chainReplacement
		wantErr string
	}{
		{
			name: "the rendered chain verifies",
		},
		{
			name: "a pod with no controlling owner is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				dropOwner(t, cli, "router-zero")
			},
			wantErr: "no controlling owner",
		},
		{
			name: "a pod controlled by something other than a ReplicaSet is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				rewriteOwner(t, cli, "router-zero", "StatefulSet", appsv1.SchemeGroupVersion.String(),
					"router-zero-rs", "rs-uid-router-zero")
			},
			wantErr: "rather than a ReplicaSet",
		},
		{
			// A ReplicaSet of a different API version is not the chain this operator verifies,
			// and reading it as one would verify a shape the reconciler never renders.
			name: "a ReplicaSet of another API version is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				rewriteOwner(t, cli, "router-zero", "ReplicaSet", "apps/v1beta2",
					"router-zero-rs", "rs-uid-router-zero")
			},
			wantErr: "rather than a ReplicaSet",
		},
		{
			name: "a replica set that no longer exists is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				require.NoError(t, cli.Delete(context.Background(),
					&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
						Name: "router-zero-rs", Namespace: md.Namespace,
					}}))
			},
			wantErr: "could not be read uncached",
		},
		{
			// A ReplicaSet deleted and recreated under its own name is what a rollout or a
			// retirement leaves behind: the read still succeeds, and the identity the Pod's
			// controlling reference carries is the only thing left to tell the two apart.
			name: "a replica set that changed identity is held",
			replace: &chainReplacement{
				object:   func() ctrlcli.Object { return new(appsv1.ReplicaSet) },
				key:      ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-zero-rs"},
				freshUID: types.UID("rs-uid-router-zero-replacement"),
			},
			wantErr: "ReplicaSet changed identity during observation",
		},
		{
			// The workload Deployment is the far end of the apps chain, and it is replaced the
			// same way: the same name, a read that still succeeds, and an identity comparison
			// that is the only thing left holding the process.
			name: "a Deployment that changed identity is held",
			replace: &chainReplacement{
				object:   func() ctrlcli.Object { return new(appsv1.Deployment) },
				key:      ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"},
				freshUID: types.UID("deployment-uid-router-replacement"),
			},
			wantErr: "Deployment changed identity during observation",
		},
		{
			name: "a replica set with no controlling Deployment is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				replicaSet := new(appsv1.ReplicaSet)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "router-zero-rs"}, replicaSet))
				replicaSet.OwnerReferences = nil
				require.NoError(t, cli.Update(context.Background(), replicaSet))
			},
			wantErr: "no controlling Deployment",
		},
		{
			name: "a Deployment owned by another deployment is held",
			mutate: func(_ *testing.T, cli ctrlcli.Client) {
				deployment := new(appsv1.Deployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name + "-router"}, deployment))
				deployment.OwnerReferences[0].UID = "someone-else"
				require.NoError(t, cli.Update(context.Background(), deployment))
			},
			wantErr: "rather than this deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := faithful(t)
			if tc.mutate != nil {
				tc.mutate(t, r.Client)
			}
			if tc.replace != nil {
				replaceChainHop(t, r.Client, *tc.replace)
			}
			// The pod is read back from the client, because what the row mutated is the object the
			// collection would find, not the fixture's own copy of it.
			pod := new(corev1.Pod)
			require.NoError(t, r.Client.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: "team-a", Name: "router-zero"}, pod))

			err := r.verifyRouterPodOwnership(context.Background(), md, pod)
			if tc.wantErr == "" {
				require.NoError(t, err, "the rendered chain is the one the render produces")

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// dropOwner removes a Pod's controlling owner reference, leaving the Pod otherwise untouched.
func dropOwner(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = nil
	require.NoError(t, cli.Update(context.Background(), pod))
}

// rewriteOwner replaces a Pod's controlling owner reference with a named one.
func rewriteOwner(t *testing.T, cli ctrlcli.Client, name, kind, apiVersion, ownerName string, uid types.UID) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: apiVersion, Kind: kind, Name: ownerName, UID: uid,
		Controller: ptr.To(true),
	}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// chainReplacement states that one object of the rendered chain is replaced in place: the object
// keeps its namespace and name and is given another UID, the shape a rollout or a retirement
// leaves behind.
type chainReplacement struct {
	object func() ctrlcli.Object
	key    ctrlcli.ObjectKey
	// freshUID is the identity the replacement carries. It has to differ from the identity the
	// chain's controlling reference still names, or the row would verify nothing.
	freshUID types.UID
}

// replaceChainHop deletes the named object of the rendered chain and recreates it under the same
// namespace and name with another UID, changing nothing else about it, and then asserts that the
// object the reconciler will read is that replacement. The read therefore succeeds, so the row is
// only ever held by the identity comparison, and the assertion is what proves the row reached it
// rather than a missing-object path.
func replaceChainHop(t *testing.T, cli ctrlcli.Client, hop chainReplacement) {
	t.Helper()

	live := hop.object()
	require.NoError(t, cli.Get(context.Background(), hop.key, live))
	assert.NotEqual(t, live.GetUID(), hop.freshUID,
		"a replacement reusing the rendered identity would verify nothing")

	removed := hop.object()
	removed.SetNamespace(hop.key.Namespace)
	removed.SetName(hop.key.Name)
	require.NoError(t, cli.Delete(context.Background(), removed))

	live.SetUID(hop.freshUID)
	// A READ OBJECT CARRIES THE VERSION OF THE ONE THAT WAS DELETED. A create names no version,
	// so the replacement is written without one, exactly as the API server would store it.
	live.SetResourceVersion("")
	require.NoError(t, cli.Create(context.Background(), live))

	// THE REPLACEMENT IS THE OBJECT THE CHAIN READS. This reads it back rather than trusting the
	// write, so a row that failed to establish a fresh identity is reported as a broken fixture
	// instead of passing against the read failure it did not mean to exercise.
	replaced := hop.object()
	require.NoError(t, cli.Get(context.Background(), hop.key, replaced))
	assert.Equal(t, hop.freshUID, replaced.GetUID(),
		"the chain must read the replacement, so the identity comparison is what holds it")
}

// TestATerminatingRouterProcessIsStillAProcess pins the coverage rule: a Router Pod that is
// terminating still holds an address and still answers, so it is read like any other, and only a
// process that has lost its address ends the coverage.
func TestATerminatingRouterProcessIsStillAProcess(t *testing.T) {
	t.Parallel()

	md, router := retirementRouterFixture(newRenderDeployment())
	// A deleting object the API accepts still carries a finalizer, and a fake client that refuses
	// one without is modeling the API rather than the operator, so the fixture is a real one.
	router.DeletionTimestamp = ptr.To(metav1.Now())
	router.Finalizers = []string{"gpustack.ai/test"}
	// The chain belongs to THIS Pod, the one the collection discovers by label.
	replicaSet, deployment := ownedRouterChain(router, md)
	member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
	member.Labels[modelDeploymentLabelKeyInstance] = md.Name
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(md, router, member, &replicaSet, &deployment).Build()
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, clock: time.Now,
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, []*corev1.Pod{member})
	require.Empty(t, failure)
	require.Len(t, observations, 1,
		"a terminating process is a running process: it holds an address and answers")
	assert.NoError(t, observations[0].Err,
		"a terminating process is read, not held: dropping it would read a shutdown as a converged empty registry")
}

// TestTheCollectionIsBoundedByTheDeploymentItStartedFrom pins the guard: the deployment and
// its membership are re-read uncached after the views, and a deployment that changed underneath
// the collection holds the whole answer rather than confirming absence from an earlier snapshot.
//
// THE CHANGE IS MADE FROM INSIDE THE FETCH, between the two reads, because that is the window the
// guard exists to close. A change made before the collection is simply what both snapshots see, and
// a guard that cannot tell that apart from a change underneath it is not the guard.
func TestTheCollectionIsBoundedByTheDeploymentItStartedFrom(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment)
		wantMsg string
	}{
		{
			name: "an unchanged deployment and membership confirm",
		},
		{
			name: "a deployment whose generation moved holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				live := new(workercore.ModelDeployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKeyFromObject(md), live))
				live.Generation++
				require.NoError(t, cli.Update(context.Background(), live))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a deployment that was replaced by another object holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				live := new(workercore.ModelDeployment)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKeyFromObject(md), live))
				live.UID = "a-different-deployment"
				require.NoError(t, cli.Update(context.Background(), live))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a membership that lost a member holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				require.NoError(t, cli.Delete(context.Background(),
					&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: "server-0", Namespace: "team-a",
					}}))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			// The member keeps its UID and changes its controlling owner. The two bookends still
			// agree on the membership, so a UID-only comparison would read this member as the same
			// one and confirm absence about a Pod this pass never bound.
			name: "a member that changed owner while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				writeStatus(t, cli, "server-0", func(pod *corev1.Pod) {
					_ = pod
				})
				adopt(t, cli, "server-0", mdForMember(t, cli))
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a member that was relabelled into another role while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				relabel(t, cli, "server-0", modelDeploymentLabelKeyComponent, "prefill")
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			name: "a member whose declared listener moved while keeping its UID holds",
			mutate: func(t *testing.T, cli ctrlcli.Client, _ *workercore.ModelDeployment) {
				moveListener(t, cli, "server-0", 9000)
			},
			wantMsg: "changed while the routers were being observed",
		},
		{
			// A Pod that wears this deployment's identity labels but is owned by another object is
			// not a fact any binding can act on, so the guard must not bookend it. Refusing the
			// collection over it would make the guard describe a wider set than the bindings read,
			// and one foreign Pod's churn would hold a member this deployment does have.
			name: "a labelled pod this deployment does not own does not hold",
			mutate: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				impostor := observationLivePod("server-9", "10.0.0.9", "pod-uid-9", true)
				impostor.Labels[modelDeploymentLabelKeyInstance] = md.Name
				impostor.OwnerReferences[0].UID = "another-deployment"
				require.NoError(t, cli.Create(context.Background(), impostor))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md, router := retirementRouterFixture(newRenderDeployment())
			member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
			member.Labels[modelDeploymentLabelKeyInstance] = md.Name
			replicaSet, deployment := ownedRouterChain(router, md)
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(md, router, member, &replicaSet, &deployment).Build()
			r := &ModelDeploymentReconciler{
				Client: cli, APIReader: cli, clock: time.Now,
				servingViewFetch: func(context.Context, string) ([]byte, error) {
					if tc.mutate != nil {
						tc.mutate(t, cli, md)
					}

					return []byte(observerHealthyPayload), nil
				},
			}

			observations, failure := r.collectModelDeploymentRouterObservations(
				context.Background(), md, []*corev1.Pod{member})
			if tc.wantMsg == "" {
				require.Empty(t, failure)
				require.Len(t, observations, 1)

				return
			}
			require.NotEmpty(t, failure,
				"confirmed absence from an earlier snapshot would be a lie about the current one")
			assert.Contains(t, failure, tc.wantMsg)
		})
	}
}

// TestACallerHoldingAReplacedDeploymentIsRefused pins the caller half of the bookend: the
// first uncached read is compared with the object the caller passed in, not only with the second
// uncached read.
//
// TWO EQUAL FRESH SNAPSHOTS SAY NOTHING ABOUT THE CALLER. A reconcile that started before a
// replacement carries the old object for its whole pass, and both of its fresh reads agree on the
// replacement, so a guard comparing only the two reads accepts the caller's stale object as
// current. This is a control for exactly that: the deployment is replaced before the collection
// starts, so the two bookends agree and only the caller disagrees.
func TestACallerHoldingAReplacedDeploymentIsRefused(t *testing.T) {
	t.Parallel()

	md, router := retirementRouterFixture(newRenderDeployment())
	member := observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)
	member.Labels[modelDeploymentLabelKeyInstance] = md.Name
	replicaSet, deployment := ownedRouterChain(router, md)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(md, router, member, &replicaSet, &deployment).Build()
	r := &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, clock: time.Now,
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	// The replacement happens FIRST, so both uncached bookends see the same new object and the
	// only disagreeing party is the caller still holding the old one.
	live := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), live))
	live.UID = "a-replacement-deployment"
	require.NoError(t, cli.Update(context.Background(), live))

	observations, failure := r.collectModelDeploymentRouterObservations(
		context.Background(), md, []*corev1.Pod{member})
	require.NotEmpty(t, failure,
		"two agreeing fresh reads are a fact about the two reads, not about the caller")
	assert.Contains(t, failure, "the caller read deployment")
	assert.Empty(t, observations)
}

// adopt re-points a Pod's controlling owner at another object, as an adoption would.
func adopt(t *testing.T, cli ctrlcli.Client, name string, md *workercore.ModelDeployment) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: workercore.SchemeGroupVersion.String(),
		Kind:       "ModelDeployment",
		Name:       md.Name,
		UID:        "someone-else",
		Controller: ptr.To(true),
	}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// relabel changes one label on a Pod in place, keeping its UID and everything else.
func relabel(t *testing.T, cli ctrlcli.Client, name, key, value string) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.Labels[key] = value
	require.NoError(t, cli.Update(context.Background(), pod))
}

// moveListener re-declares the member's container ports, keeping its UID and every label.
func moveListener(t *testing.T, cli ctrlcli.Client, name string, port int32) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port}}
	require.NoError(t, cli.Update(context.Background(), pod))
}

// writeStatus changes a Pod's status in place, the way the kubelet writes it.
func writeStatus(t *testing.T, cli ctrlcli.Client, name string, mutate func(*corev1.Pod)) {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, pod))
	mutate(pod)
	require.NoError(t, cli.Status().Update(context.Background(), pod))
}

// mdForMember reads the seeded deployment, for a row that mutates ownership mid-collection.
func mdForMember(t *testing.T, cli ctrlcli.Client) *workercore.ModelDeployment {
	t.Helper()
	live := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, live))

	return live
}

// TestTheCountIsPerRoleOverTheSameUnion pins the per-role derivation: the union of three physical
// endpoints is counted once globally and per role, and the global total is never copied onto a
// role that does not hold it.
//
// THE FIXTURE IS THE SHAPE THE REVIEW NAMED: two endpoints in custom-a, one in custom-b and a
// fourth that every router has already refused. A role with no serving endpoint is an explicit
// zero, which is the only reading under which a role's own field is not reporting its neighbors.
func TestTheCountIsPerRoleOverTheSameUnion(t *testing.T) {
	t.Parallel()

	endpoints := []ServingEndpoint{
		{PodUID: "pod-a-0", Role: "custom-a", Rank: "0", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-a-1", Role: "custom-a", Rank: "1", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-b-0", Role: "custom-b", Rank: "0", Listener: observationMemberPort, Serving: true},
		{PodUID: "pod-z-0", Role: "unselected", Rank: "0", Listener: observationMemberPort, Serving: false},
	}
	answer := aggregateModelDeploymentServing(true, []RouterServingObservation{{
		View:        RouterServingView{RouterPodUID: "router-uid-one", RouterBootGeneration: 1},
		CollectedAt: time.Now(),
		Bound:       endpoints,
	}}, modelDeploymentServingFreshness)

	require.Equal(t, workercore.ModelDeploymentServingStateConfirmed, answer.State, answer.Reason)
	require.NotNil(t, answer.Value)
	assert.Equal(t, int32(3), *answer.Value, "three physical endpoints serve")
	assert.Equal(t, map[string]int32{"custom-a": 2, "custom-b": 1}, answer.ByRole,
		"each role is counted from the same union, and a refused endpoint counts for nobody")

	status := &workercore.ModelDeploymentStatus{Roles: []workercore.ModelDeploymentRoleStatus{
		{Name: "custom-a"}, {Name: "custom-b"}, {Name: "unselected"},
	}}
	applyModelDeploymentServing(status, answer)
	for i, want := range []int32{2, 1, 0} {
		require.NotNil(t, status.Roles[i].Endpoints.Serving.Value)
		assert.Equal(t, want, *status.Roles[i].Endpoints.Serving.Value,
			"role %q reports its own count, not the deployment's", status.Roles[i].Name)
	}
}

// TestOnlyAConfirmedAnswerCarriesACount pins the nil contract: Unknown, NotConverged and
// NotConfigured all leave every role's serving value absent, and only Confirmed allocates one.
//
// THE ABSENCE IS THE CONTRACT, not a missing field. A zero written into a state that never
// established a count is a measurement of zero, which is the false zero this whole path exists to
// refuse, and a reader cannot tell it from a role that genuinely serves nothing.
func TestOnlyAConfirmedAnswerCarriesACount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		answer    ServingAnswer
		wantState workercore.ModelDeploymentServingState
		// wantValue is per role, in the order the status declares them.
		wantValue []int32
	}{
		{
			name:      "not configured carries no count",
			answer:    ServingAnswer{State: workercore.ModelDeploymentServingStateNotConfigured},
			wantState: workercore.ModelDeploymentServingStateNotConfigured,
		},
		{
			name: "unknown carries no count even when a per-role map was built",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateUnknown,
				ByRole: map[string]int32{"custom-a": 2},
			},
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name: "not converged carries no count",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateNotConverged,
				ByRole: map[string]int32{"custom-a": 2},
			},
			wantState: workercore.ModelDeploymentServingStateNotConverged,
		},
		{
			name: "confirmed carries the role's own count",
			answer: ServingAnswer{
				State:  workercore.ModelDeploymentServingStateConfirmed,
				Value:  ptr.To(int32(3)),
				ByRole: map[string]int32{"custom-a": 2, "custom-b": 1},
			},
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: []int32{2, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status := &workercore.ModelDeploymentStatus{Roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "custom-a"}, {Name: "custom-b"},
			}}
			applyModelDeploymentServing(status, tc.answer)

			for i := range status.Roles {
				got := status.Roles[i].Endpoints.Serving
				assert.Equal(t, tc.wantState, got.State)
				if tc.wantValue == nil {
					assert.Nil(t, got.Value,
						"a state that established no count must carry no number")

					continue
				}
				require.NotNil(t, got.Value)
				assert.Equal(t, tc.wantValue[i], *got.Value,
					"role %q reports its own count", status.Roles[i].Name)
			}
			// Each role owns its value; two roles must never alias one count.
			if tc.wantValue != nil {
				require.NotNil(t, status.Roles[0].Endpoints.Serving.Value)
				require.NotNil(t, status.Roles[1].Endpoints.Serving.Value)
				assert.NotSame(t, status.Roles[0].Endpoints.Serving.Value,
					status.Roles[1].Endpoints.Serving.Value)
			}
		})
	}
}
