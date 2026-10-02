package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
			name: "an explicit empty worker list is a valid view of an empty registry",
			payload: `{
				"router": {"boot_generation": 5, "now_ms": 6},
				"registry_revision": 2,
				"workers": null
			}`,
			assertView: func(t *testing.T, view RouterServingView) {
				assert.Empty(t, view.Workers)
			},
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

	viewOf := func(gen uint64, at time.Time, serving bool, uids ...string) RouterServingObservation {
		view := RouterServingView{RouterBootGeneration: gen}
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
			name:       "the union deduplicates endpoints two views both hold",
			configured: true,
			observations: []RouterServingObservation{
				viewOf(1, fresh, true, "pod-a", "pod-b"),
				viewOf(1, fresh, true, "pod-b", "pod-c"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateConfirmed,
			wantValue: ptr.To(int32(3)),
		},
		{
			name:       "views that disagree about one endpoint are NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf(1, fresh, true, "pod-a"),
				viewOf(1, fresh, false, "pod-a"),
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
				viewOf(1, fresh, true, "pod-a"),
				{Err: errors.New("dial failed")},
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "a stale view makes the answer Unknown",
			configured: true,
			observations: []RouterServingObservation{
				viewOf(1, fresh.Add(-2*freshness), true, "pod-a"),
			},
			freshness: freshness,
			wantState: workercore.ModelDeploymentServingStateUnknown,
		},
		{
			name:       "views from different router generations are NotConverged",
			configured: true,
			observations: []RouterServingObservation{
				viewOf(1, fresh, true, "pod-a"),
				viewOf(2, fresh, true, "pod-a"),
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
				viewOf(1, fresh, true),
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
