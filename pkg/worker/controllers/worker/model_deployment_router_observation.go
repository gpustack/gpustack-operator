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
//     generation-consistent per Router process, and agreeing.
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

// routerObservedSelectionWire carries the four selection booleans as pointers, so the decoder can
// tell an absent field from an explicit false and a null from a value. A missing boolean used to
// decode as false, which is the same value the router sends for a worker it has refused, so a
// truncated or partial payload was indistinguishable from a refusal and confirmed a zero.
type routerObservedSelectionWire struct {
	Registered  *bool `json:"registered"`
	Healthy     *bool `json:"healthy"`
	CircuitOpen *bool `json:"circuit_open"`
	InSelection *bool `json:"in_selection"`
}

// routerObservedWorkerWire is the on-the-wire worker. Selection is a pointer so a null or absent
// selection block stays distinguishable from a complete one.
type routerObservedWorkerWire struct {
	URL       string                       `json:"url"`
	Port      string                       `json:"port,omitempty"`
	Role      string                       `json:"role"`
	PodHint   string                       `json:"pod_hint"`
	WorkerID  string                       `json:"worker_id"`
	Selection *routerObservedSelectionWire `json:"selection"`
	Lane      *struct {
		Busy    bool `json:"busy"`
		Waiting int  `json:"waiting"`
	} `json:"lane,omitempty"`
	LastJob json.RawMessage `json:"last_job,omitempty"`
}

type routerObservationPayload struct {
	Router struct {
		BootGeneration uint64 `json:"boot_generation"`
		NowMS          uint64 `json:"now_ms"`
	} `json:"router"`
	RegistryRevision uint64 `json:"registry_revision"`
	// Workers is a pointer so an absent or null array stays distinguishable from an
	// explicit one. Both routers serialize a non-Option vector, so the array is always
	// on the wire; a payload without it states no membership, and reading that as an
	// empty registry would confirm a zero for a router that is serving.
	Workers *[]routerObservedWorkerWire `json:"workers"`
}

// parseRouterObservationView decodes one observer payload strictly. The decoder
// disallows unknown fields, so a service that answers the endpoint with some other
// document — the per-plugin debug-dump shapes the spec warns about — fails here instead
// of parsing into an empty, confident-looking view. A view must also carry its workers
// array explicitly: an explicit [] is a real empty registry and yields a valid zero,
// while an omitted or null array is a payload that never said what it was serving.
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
	if payload.Workers == nil {
		return RouterServingView{}, fmt.Errorf("observer payload carries no workers array")
	}

	// A document with a second JSON value after the payload is not the payload. The decoder reads
	// the first one and would otherwise ignore whatever followed.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return RouterServingView{}, fmt.Errorf("observer payload carries a trailing JSON value")
	}

	workers := make([]RouterObservedWorker, 0, len(*payload.Workers))
	for i, wire := range *payload.Workers {
		worker, err := routerObservedWorkerOf(wire)
		if err != nil {
			return RouterServingView{}, fmt.Errorf("observer payload worker %d: %w", i, err)
		}
		workers = append(workers, worker)
	}

	return RouterServingView{
		RouterBootGeneration: payload.Router.BootGeneration,
		RouterNowMS:          payload.Router.NowMS,
		RegistryRevision:     payload.RegistryRevision,
		Workers:              workers,
	}, nil
}

// routerObservedWorkerOf converts one wire row, refusing anything that does not state its
// selection completely.
//
// EVERY SELECTION BOOLEAN MUST BE PRESENT, NON-NULL AND BOOLEAN before a row means anything. The
// reason is that the answer the protocol acts on is a zero, and a zero is a claim that the router
// is serving nobody. A row that never said whether it is registered, healthy, open-circuited or in
// selection has not said it is serving nobody, and reading its silence as consent is how an
// incomplete payload confirms a zero for a router that is serving. An explicit false is the
// opposite case and stays a refusal to serve, which is the router's own answer.
func routerObservedWorkerOf(wire routerObservedWorkerWire) (RouterObservedWorker, error) {
	if wire.Selection == nil {
		return RouterObservedWorker{}, fmt.Errorf("carries no selection")
	}

	selection, err := routerObservedSelectionOf(*wire.Selection)
	if err != nil {
		return RouterObservedWorker{}, err
	}

	return RouterObservedWorker{
		URL:       wire.URL,
		Port:      wire.Port,
		Role:      wire.Role,
		PodHint:   wire.PodHint,
		WorkerID:  wire.WorkerID,
		Selection: selection,
		Lane:      wire.Lane,
		LastJob:   wire.LastJob,
	}, nil
}

// routerObservedSelectionOf requires all four booleans to be stated. The exported selection keeps
// plain bools because it is what the rest of the tree reads; presence is settled here, once, so
// nothing downstream has to re-ask.
func routerObservedSelectionOf(wire routerObservedSelectionWire) (RouterObservedSelection, error) {
	if wire.Registered == nil {
		return RouterObservedSelection{}, fmt.Errorf("selection states no registered")
	}
	if wire.Healthy == nil {
		return RouterObservedSelection{}, fmt.Errorf("selection states no healthy")
	}
	if wire.CircuitOpen == nil {
		return RouterObservedSelection{}, fmt.Errorf("selection states no circuit_open")
	}
	if wire.InSelection == nil {
		return RouterObservedSelection{}, fmt.Errorf("selection states no in_selection")
	}

	return RouterObservedSelection{
		Registered:  *wire.Registered,
		Healthy:     *wire.Healthy,
		CircuitOpen: *wire.CircuitOpen,
		InSelection: *wire.InSelection,
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

// modelDeploymentObserverMaxBodyBytes caps one observer body. It is generous for a membership view
// of a few hundred workers and small enough that a Router misbehaving cannot grow the response into
// the reconciler's memory.
const modelDeploymentObserverMaxBodyBytes = 4 << 20

// modelDeploymentObservationBudget is the total time one collection may take, across every Router it
// reads and the binding it does afterwards.
//
// IT IS ONE BUDGET AND NOT ONE PER ROUTER. Each read already carries the caller's own deadline, but
// a caller with no deadline gave none, and a deployment of several Routers multiplied that absence
// into a pass whose length is the sum of however long each of them chose to take. Deriving the
// total here means one collection is bounded whatever the caller passed and however many Routers
// answered.
//
// IT IS SHORTER THAN THE FRESHNESS IT HAS TO SURVIVE. A view collected right at the freshness limit
// is stale by the time the aggregation judges it, so a read budget equal to the freshness would
// collect answers the aggregator is obliged to refuse. The allowance below is the time discovery,
// the last read and the binding take out of the window.
const modelDeploymentObservationBudget = modelDeploymentServingFreshness - time.Second

// modelDeploymentObservationContext derives the single context every read of one collection runs
// under, so a caller that supplied its own deadline keeps it and an unlimited caller still gets a
// finite one.
func modelDeploymentObservationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, modelDeploymentObservationBudget)
}

// modelDeploymentRouterObservationContext keeps collection within the active retirement budget.
func (r *ModelDeploymentReconciler) modelDeploymentRouterObservationContext(
	ctx context.Context, md *workercore.ModelDeployment,
) (context.Context, context.CancelFunc) {
	budget := modelDeploymentObservationBudget
	if reservation := md.Status.Retirement; reservation != nil {
		phaseBudget := time.Duration(0)
		switch reservation.State {
		case workercore.ModelDeploymentRetirementStateAdmitted,
			workercore.ModelDeploymentRetirementStateDisqualified,
			workercore.ModelDeploymentRetirementStateWithdrawing:
			phaseBudget = modelDeploymentRetirementWithdrawalBudget
		case workercore.ModelDeploymentRetirementStateDraining:
			phaseBudget = modelDeploymentRetirementDrainBudget
		case workercore.ModelDeploymentRetirementStateSettling:
			phaseBudget = modelDeploymentRetirementSettleBudget
		case workercore.ModelDeploymentRetirementStateDeleting:
			phaseBudget = modelDeploymentRetirementOverallBudget
		}
		if phaseBudget != 0 {
			now := r.modelDeploymentNow()
			budget = min(budget, reservation.Deadline.Sub(now),
				reservation.PhaseStartedAt.Add(phaseBudget).Sub(now))
		}
	}

	return context.WithTimeout(ctx, budget)
}

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

	// BOOT GENERATION IS A PER-PROCESS VALUE. Both routers document it as their process
	// start time in milliseconds, so two independent replicas are EXPECTED to report
	// different ones, and comparing one scalar across distinct processes made every
	// multi-replica deployment permanently NotConverged — a stall that no amount of
	// waiting clears, because the disagreement is normal rather than transient.
	// Consistency is therefore asked one process at a time. The only disagreement that
	// makes the answer dishonest is ONE process reporting two generations, which means a
	// Router rolled mid-observation and the views describe different incarnations of it.
	// Views that carry no identity cannot be placed in any process, so they share one
	// group and are held against each other: dropping the check there would read a roll
	// as convergence, and treating each as its own process would never compare anything.
	generationByRouter := map[types.UID]uint64{}
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
		routerUID := observation.View.RouterPodUID
		generation := observation.View.RouterBootGeneration
		if seen, known := generationByRouter[routerUID]; known && seen != generation {
			return ServingAnswer{
				State: workercore.ModelDeploymentServingStateNotConverged,
				Reason: fmt.Sprintf(
					"a router process reports two boot generations (%d and %d): a router rolled during observation",
					seen, generation),
			}
		}
		generationByRouter[routerUID] = generation
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
	// AN UNLIMITED CALLER STILL GETS A FINITE REQUEST. The collection derives one budget for the
	// whole pass, but this transport is also reachable on its own, and a request that inherits a
	// context with no deadline has none of its own: it waits as long as the Router keeps the
	// connection open, which is unbounded and is exactly the shape of a Router that has wedged.
	requestCtx, cancel := modelDeploymentObservationContext(ctx)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
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

	// THE BODY IS READ UNDER A CAP, not to its end. An observer that never finishes writing would
	// otherwise hold a reconcile open for as long as it liked, and the deadline above is the only
	// thing standing between one Router and a pass that never returns.
	body := io.LimitReader(response.Body, modelDeploymentObserverMaxBodyBytes+1)
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read observer body: %w", err)
	}
	if len(raw) > modelDeploymentObserverMaxBodyBytes {
		return nil, fmt.Errorf("observer served more than the %d byte bound",
			modelDeploymentObserverMaxBodyBytes)
	}

	return raw, nil
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
	ctx, cancel := r.modelDeploymentRouterObservationContext(ctx, md)
	defer cancel()

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

	observations, failure := r.collectModelDeploymentRouterObservations(ctx, md, live)
	if failure != "" {
		return ServingAnswer{
			State:  workercore.ModelDeploymentServingStateUnknown,
			Reason: failure,
		}
	}

	return aggregateModelDeploymentServing(true, observations, modelDeploymentServingFreshness)
}

// collectModelDeploymentRouterObservations is the ONE collection of Router views, and both callers
// of a Router view run it: the reconcile-time serving answer and the retirement residual. They
// each carried their own copy of discovery, fetch, parse and binding, which is how the two drifted
// and how a residual could be collected under a budget the serving answer knew nothing about.
//
// DISCOVERY, EVERY READ AND THE BINDING ALL RUN INSIDE ONE CONTEXT. A caller that passed a deadline
// keeps it; an unlimited caller gets the observation budget; and a single Router cannot renew the
// total by being slow, because every read after the first inherits whatever the first left. A read
// that runs out of time, fails, or comes back unusable is recorded as an error on its own
// observation, so the aggregate answers Unknown and the residual holds, rather than a read
// silently becoming an empty registry.
func (r *ModelDeploymentReconciler) collectModelDeploymentRouterObservations(
	ctx context.Context, md *workercore.ModelDeployment, live []*corev1.Pod,
) ([]RouterServingObservation, string) {
	collectCtx, cancel := r.modelDeploymentRouterObservationContext(ctx, md)
	defer cancel()
	if err := collectCtx.Err(); err != nil {
		return nil, fmt.Sprintf("the router observation budget is exhausted: %v", err)
	}

	routerPods := &corev1.PodList{}
	if err := r.Client.List(collectCtx, routerPods,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
			modelDeploymentRouterLabelKey:   md.Spec.Router.Name,
		},
	); err != nil {
		return nil, fmt.Sprintf("the router's pods could not be listed: %v", err)
	}

	fetch := r.servingViewFetch
	if fetch == nil {
		fetch = defaultServingViewFetch
	}

	observations := make([]RouterServingObservation, 0, len(routerPods.Items))
	for i := range routerPods.Items {
		routerPod := &routerPods.Items[i]
		if routerPod.Status.PodIP == "" || routerPod.DeletionTimestamp != nil {
			continue
		}
		observation, err := collectRouterServingView(collectCtx, func(
			callCtx context.Context, path string,
		) ([]byte, error) {
			return fetch(callCtx, fmt.Sprintf("http://%s:%d%s",
				routerPod.Status.PodIP, modelDeploymentRouterHTTPPort, path))
		}, routerPod.UID)
		if err != nil {
			observations = append(observations, observation)

			continue
		}
		// Stamped per view, after that view's own read: one timestamp taken before the loop
		// would age every later view by however long the earlier reads ran, and a slow first
		// fetch could push the last view's recorded age to the freshness edge before the
		// aggregate ever judged it. The freshness gate in the aggregate is unchanged.
		observation.CollectedAt = r.modelDeploymentNow()
		observation.Bound, err = bindRouterObservationView(observation.View, live)
		if err != nil {
			observation.Err = err
		}
		observations = append(observations, observation)
	}

	if err := collectCtx.Err(); err != nil {
		return nil, fmt.Sprintf("the router observation budget is exhausted: %v", err)
	}

	return observations, ""
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
