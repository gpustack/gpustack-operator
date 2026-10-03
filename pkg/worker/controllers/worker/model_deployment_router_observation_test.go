package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

func observationLivePod(name, ip string, uid types.UID, running bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			UID:       uid,
		},
		Status: corev1.PodStatus{PodIP: ip},
	}
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
				PodUID: "pod-uid-1", Member: "server-0", Role: "regular",
				URL: "http://10.0.0.5:8000", Port: "8000", Serving: true,
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
				PodUID: "pod-uid-new", Member: "server-0", Role: "regular",
				URL: "http://10.0.0.5:8000", Port: "8000", Serving: true,
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

			bound, err := bindRouterObservationView(tc.view, tc.pods)
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

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(md, routers[0], routers[1], observationLivePod("server-0", "10.0.0.5", "pod-uid-1", true)).
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
			base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, router).Build()
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
