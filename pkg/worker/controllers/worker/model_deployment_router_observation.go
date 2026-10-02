// Per-Router serving observation: the operator side of Feature 2.
//
// The two Rust routers expose a read-only membership view (the observer patches under
// pack/llm-router). This file is the consumer: it parses that view schema-strictly, binds
// each serving worker to the live Pod its address resolves to — at collection time, from
// Kubernetes state, never from anything the router claims about identity — and aggregates
// the bound views into the deployment's serving answer.
//
// THE THREE LAWS the spec puts on the answer, and where this file keeps them:
//
//   - Direct per-process access: a view is always read from one named Router Pod, never
//     sampled through the Router's Service; the caller passes the fetch, so who was asked
//     is decided before a byte is read.
//   - Identity at collection time: an endpoint's Pod UID comes from the live Pod object
//     its address resolves to, with the Pod running and not terminating. A URL that
//     resolves to nothing live is an indeterminate binding, and one indeterminate binding
//     makes the whole view unusable — a number built on a partial binding would be a guess
//     wearing a confirmation's clothes.
//   - Never a fake zero: an unusable, stale, missing or disagreeing set of views is
//     Unknown or NotConverged; NotConfigured means the deployment declares no Router at
//     all; Confirmed — including the explicit zero — requires every view fresh, complete,
//     generation-converged, and agreeing.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// RouterObservedWorker is one worker entry of the observer payload. The observer's
// selection fields are read from the same accessors the router's request path consults,
// so `in_selection` is the request path's own answer to "would this worker receive a new
// request".
// RouterObservedSelection is the router's own answer, per worker, to "would this worker
// receive a new request": the conjunction the request path evaluates.
type RouterObservedSelection struct {
	Registered  bool `json:"registered"`
	Healthy     bool `json:"healthy"`
	CircuitOpen bool `json:"circuit_open"`
	InSelection bool `json:"in_selection"`
}

type RouterObservedWorker struct {
	URL       string                  `json:"url"`
	Port      string                  `json:"port,omitempty"`
	Role      string                  `json:"role"`
	PodHint   string                  `json:"pod_hint"`
	WorkerID  string                  `json:"worker_id"`
	Selection RouterObservedSelection `json:"selection"`
	// Lane and LastJob are the gateway's control-plane operational reads (its per-URL
	// discovery lane and the last recorded control-plane job). They are PARSED so the
	// strict decoder accepts the gateway's view as it is served, and never read beyond
	// that: the binder and the aggregation take no input from them. LastJob stays raw
	// because its shape is the gateway's JobStatus, not this contract.
	Lane *struct {
		Busy    bool `json:"busy"`
		Waiting int  `json:"waiting"`
	} `json:"lane,omitempty"`
	LastJob json.RawMessage `json:"last_job,omitempty"`
}

// RouterServingView is one parsed, schema-validated observer payload. The schema is
// strict: an unknown field is a payload this operator does not understand, and a payload
// it does not understand is not a view of anything. A missing router block is rejected
// for the same reason — boot generation zero would make a restart indistinguishable from
// a first boot.
type RouterServingView struct {
	// RouterPodUID is the identity of the Router Pod the view was read from, bound by
	// the collector; it is not part of the wire payload the router serves.
	RouterPodUID         types.UID `json:"-"`
	RouterBootGeneration uint64
	RouterNowMS          uint64
	RegistryRevision     uint64
	Workers              []RouterObservedWorker
}

type routerObservationPayload struct {
	Router struct {
		BootGeneration uint64 `json:"boot_generation"`
		NowMS          uint64 `json:"now_ms"`
	} `json:"router"`
	RegistryRevision uint64                 `json:"registry_revision"`
	Workers          []RouterObservedWorker `json:"workers"`
}

// parseRouterObservationView decodes one observer payload strictly. The decoder
// disallows unknown fields, so a service that answers the endpoint with some other
// document — the per-plugin debug-dump shapes the spec warns about — fails here instead
// of parsing into an empty, confident-looking view.
func parseRouterObservationView(data []byte) (RouterServingView, error) {
	var payload routerObservationPayload
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return RouterServingView{}, fmt.Errorf("observer payload is not a membership view: %w", err)
	}
	if payload.Router.BootGeneration == 0 {
		return RouterServingView{}, fmt.Errorf("observer payload carries no boot_generation")
	}

	return RouterServingView{
		RouterBootGeneration: payload.Router.BootGeneration,
		RouterNowMS:          payload.Router.NowMS,
		RegistryRevision:     payload.RegistryRevision,
		Workers:              payload.Workers,
	}, nil
}

// ServingEndpoint is one bound serving worker: the view said it is in selection, and the
// operator resolved that worker's address to a live Pod at collection time.
type ServingEndpoint struct {
	PodUID  types.UID
	Member  string
	Role    string
	URL     string
	Port    string
	Serving bool
}

// RouterServingObservation is one Router process's answer: the parsed view plus the
// endpoints its serving workers bound to, or the error that makes the view unusable.
type RouterServingObservation struct {
	View RouterServingView
	// CollectedAt is when the operator read this view; freshness is judged against it,
	// on the operator's clock, not the router's.
	CollectedAt time.Time
	Bound       []ServingEndpoint
	Err         error
}

// Empty reports a view that was never read. A transport failure produces an Empty
// observation with an Err — the aggregation reads the Err, and Empty keeps an unread view
// from being mistaken for a view of an empty registry.
func (o RouterServingObservation) Empty() bool {
	return o.View.RouterBootGeneration == 0 && o.View.Workers == nil && o.Bound == nil
}

// observerURL is the endpoint the observer patches serve, on the router's existing
// listener.
const observerURLPath = "/observer/endpoints"

type routerViewFetch func(ctx context.Context, url string) ([]byte, error)

// collectRouterServingView reads one Router process's view through the passed fetch and
// carries the Router Pod's UID as the view's identity. The fetch is a parameter so the
// transport is the caller's decision — the production caller dials the Pod IP directly,
// and a test stands in without a network.
func collectRouterServingView(
	ctx context.Context, fetch routerViewFetch, routerPodUID types.UID,
) (RouterServingObservation, error) {
	view, err := fetch(ctx, observerURLPath)
	if err != nil {
		return RouterServingObservation{Err: fmt.Errorf("router view fetch failed: %w", err)}, err
	}
	parsed, err := parseRouterObservationView(view)
	if err != nil {
		return RouterServingObservation{Err: err}, err
	}
	parsed.RouterPodUID = routerPodUID

	return RouterServingObservation{View: parsed}, nil
}

// bindRouterObservationView resolves each serving worker's address to a live Pod and
// returns the bound endpoints. THE VIEW IS ALL-OR-NOTHING: a serving worker whose address
// resolves to nothing live, to more than one Pod, or only to a Pod that is terminating or
// not running, is an indeterminate binding — the operator cannot say the worker serves,
// and cannot say it does not, so the whole view is refused rather than counted with a gap.
// Workers outside the selection are never bound and never refuse the view: they are the
// workers the router itself already refused.
func bindRouterObservationView(
	view RouterServingView, pods []*corev1.Pod,
) ([]ServingEndpoint, error) {
	byIP := make(map[string][]*corev1.Pod, len(pods))
	for _, pod := range pods {
		if ip := pod.Status.PodIP; ip != "" {
			byIP[ip] = append(byIP[ip], pod)
		}
	}

	bound := make([]ServingEndpoint, 0)
	for _, worker := range view.Workers {
		if !worker.Selection.InSelection {
			continue
		}
		host := workerHost(worker.URL)
		if host == "" {
			return nil, fmt.Errorf("binding indeterminate: worker URL %q has no address", worker.URL)
		}
		candidates := byIP[host]
		if len(candidates) != 1 {
			return nil, fmt.Errorf(
				"binding indeterminate: %s resolves to %d live pods, not one", host, len(candidates))
		}
		pod := candidates[0]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			return nil, fmt.Errorf(
				"binding indeterminate: %s is pod %s which is not live", host, pod.Name)
		}
		bound = append(bound, ServingEndpoint{
			PodUID:  pod.UID,
			Member:  pod.Name,
			Role:    worker.Role,
			URL:     worker.URL,
			Port:    worker.Port,
			Serving: true,
		})
	}

	return bound, nil
}

// workerHost extracts the address portion of a worker URL: scheme-stripped, path-stripped.
// The registry's URLs are `scheme://host:port`; a URL in an unexpected shape yields an
// empty host, which the binder refuses rather than guesses past.
func workerHost(url string) string {
	withoutScheme := url
	if scheme, rest, found := strings.Cut(url, "://"); found {
		_ = scheme
		withoutScheme = rest
	}
	host := withoutScheme
	if cut := strings.IndexAny(host, "/?"); cut >= 0 {
		host = host[:cut]
	}
	if hostPort, _, err := net.SplitHostPort(host); err == nil {
		return hostPort
	}

	// A host without a port is still an address; SplitHostPort's error for that shape is
	// expected and the host itself is what the caller wants.
	return host
}

// ServingAnswer is the Feature 2 answer in the Feature 5 wire's shape: a state always,
// a value only when the state is Confirmed.
type ServingAnswer struct {
	State  workercore.ModelDeploymentServingState
	Value  *int32
	Reason string
}

// aggregateModelDeploymentServing is the Feature 2 state machine. `configured` is whether
// the deployment declares a Router at all; `observations` are the collected per-process
// answers; `freshness` is how long a collected view may keep informing the answer.
func aggregateModelDeploymentServing(
	configured bool, observations []RouterServingObservation, freshness time.Duration,
) ServingAnswer {
	if !configured {
		return ServingAnswer{State: workercore.ModelDeploymentServingStateNotConfigured}
	}
	if len(observations) == 0 {
		return ServingAnswer{
			State:  workercore.ModelDeploymentServingStateUnknown,
			Reason: "no router view was collected",
		}
	}

	generations := map[uint64]struct{}{}
	for _, observation := range observations {
		if observation.Err != nil {
			return ServingAnswer{
				State:  workercore.ModelDeploymentServingStateUnknown,
				Reason: fmt.Sprintf("a router view is unusable: %v", observation.Err),
			}
		}
		if age := time.Since(observation.CollectedAt); age > freshness {
			return ServingAnswer{
				State:  workercore.ModelDeploymentServingStateUnknown,
				Reason: fmt.Sprintf("a router view is stale by %s", age.Round(time.Second)),
			}
		}
		generations[observation.View.RouterBootGeneration] = struct{}{}
	}
	// A router that rolled mid-observation means the views describe two different
	// processes; no single number is honest until they converge on the surviving one.
	if len(generations) > 1 {
		return ServingAnswer{
			State:  workercore.ModelDeploymentServingStateNotConverged,
			Reason: "router generations disagree: a router rolled during observation",
		}
	}

	// The deduplicated union across views, with per-endpoint agreement checked as it
	// forms. Views are never intersected (one lagging replica would zero the answer and
	// hide exactly the leak the union exists to show) and never summed (the same endpoint
	// seen by two replicas is one endpoint).
	serving := map[types.UID]ServingEndpoint{}
	for _, observation := range observations {
		for _, endpoint := range observation.Bound {
			previous, seen := serving[endpoint.PodUID]
			if seen && previous.Serving != endpoint.Serving {
				return ServingAnswer{
					State:  workercore.ModelDeploymentServingStateNotConverged,
					Reason: fmt.Sprintf("views disagree about pod %s", endpoint.PodUID),
				}
			}
			serving[endpoint.PodUID] = endpoint
		}
	}

	count := int32(0)
	for uid := range serving {
		if serving[uid].Serving {
			count++
		}
	}

	return ServingAnswer{
		State: workercore.ModelDeploymentServingStateConfirmed,
		Value: &count,
	}
}

// modelDeploymentServingFreshness is how long a collected view keeps informing the
// answer. It bounds how stale a router's picture of the pool may be while still being a
// picture of the pool; past it the answer is Unknown rather than a number that outlived
// its evidence.
const modelDeploymentServingFreshness = 15 * time.Second

// defaultServingViewFetch is the production transport: a plain GET with a short timeout,
// against the URL the caller built from the Router Pod's own IP — never the Router's
// Service, which would sample one replica and silently drop the rest.
func defaultServingViewFetch(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("observer answered %s", response.Status)
	}

	return io.ReadAll(response.Body)
}

// observeModelDeploymentServing is the reconcile-time collection: discover the Router
// Pods the deployment renders, read each one's view directly, bind, and aggregate. Every
// failure on the way is a property of the answer, not a reconcile error — a Router that
// cannot be read is an Unknown in the wire, which is what the spec asks the field to say.
func (r *ModelDeploymentReconciler) observeModelDeploymentServing(
	ctx context.Context, md *workercore.ModelDeployment,
) ServingAnswer {
	if md.Spec.Router == nil {
		return aggregateModelDeploymentServing(false, nil, 0)
	}

	fetch := r.servingViewFetch
	if fetch == nil {
		fetch = defaultServingViewFetch
	}

	endpointPods, err := r.listModelDeploymentPods(ctx, md)
	if err != nil {
		return ServingAnswer{
			State:  workercore.ModelDeploymentServingStateUnknown,
			Reason: fmt.Sprintf("the deployment's pods could not be listed: %v", err),
		}
	}
	live := make([]*corev1.Pod, 0, len(endpointPods))
	for i := range endpointPods {
		live = append(live, &endpointPods[i])
	}

	routerPods := &corev1.PodList{}
	if err := r.Client.List(ctx, routerPods,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
			modelDeploymentRouterLabelKey:   md.Spec.Router.Name,
		},
	); err != nil {
		return ServingAnswer{
			State:  workercore.ModelDeploymentServingStateUnknown,
			Reason: fmt.Sprintf("the router's pods could not be listed: %v", err),
		}
	}

	now := time.Now()
	observations := make([]RouterServingObservation, 0, len(routerPods.Items))
	for i := range routerPods.Items {
		routerPod := &routerPods.Items[i]
		if routerPod.Status.PodIP == "" || routerPod.DeletionTimestamp != nil {
			continue
		}
		observation, err := collectRouterServingView(ctx, func(
			callCtx context.Context, path string,
		) ([]byte, error) {
			return fetch(callCtx, fmt.Sprintf("http://%s:%d%s",
				routerPod.Status.PodIP, modelDeploymentRouterHTTPPort, path))
		}, routerPod.UID)
		if err != nil {
			observations = append(observations, observation)

			continue
		}
		observation.CollectedAt = now
		observation.Bound, err = bindRouterObservationView(observation.View, live)
		if err != nil {
			observation.Err = err
		}
		observations = append(observations, observation)
	}

	return aggregateModelDeploymentServing(true, observations, modelDeploymentServingFreshness)
}

// applyModelDeploymentServing writes the observed answer onto every role's serving field.
// An empty answer is the unobserved default and leaves the status exactly as the status
// builder wrote it, which keeps the status derivation itself free of observation concerns.
func applyModelDeploymentServing(
	status *workercore.ModelDeploymentStatus, answer ServingAnswer,
) {
	if answer.State == "" {
		return
	}
	for i := range status.Roles {
		status.Roles[i].Endpoints.Serving = workercore.ModelDeploymentServingStatus{
			State: answer.State,
			Value: answer.Value,
		}
	}
}
