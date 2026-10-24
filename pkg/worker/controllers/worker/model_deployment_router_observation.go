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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/json"
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
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
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
	PodUID types.UID
	Member string
	// Role and Rank come from the bound Pod's own workload identity: the component label the
	// render stamps and the decimal member index. The observer's registry role and worker_id
	// are registry identities and are never copied here, because a router can call an endpoint
	// whatever it likes and the workload is the authority on what the endpoint is.
	Role string
	Rank string
	// Listener is the declared container port the bound URL targets. Virtual ranks that share
	// one physical endpoint share this listener and the PodUID above, so counting distinct
	// physical listeners by role folds them by construction.
	Listener int32
	URL      string
	Port     string
	Serving  bool
}

// RouterServingObservation is one Router process's answer: the parsed view plus the
// endpoints its serving workers bound to, or the error that makes the view unusable.
type RouterServingObservation struct {
	View RouterServingView
	// CollectedAt is when the operator read this view; freshness is judged against it,
	// on the operator's clock, not the router's.
	CollectedAt time.Time
	Bound       []ServingEndpoint
	// RecentDispatch is the journal of workers this router recently dispatched to, bound to the
	// same Pod identity as Bound and kept deliberately OUT of it. These workers are no longer in
	// the selection, so folding them into Bound would raise the serving count for workers the
	// router has already stopped selecting. The journal answers a different question, which only
	// the retirement residual asks: did a dispatch reach this target recently enough that
	// releasing it now would be releasing a member that may still be serving.
	RecentDispatch []ServingEndpoint
	Err            error
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

// modelDeploymentRouterObserverPort is the listener the owning profile serves the observer on.
//
// llm-d attaches the observer to the admin handler it already runs, which is the management
// listener where its metrics live. The other two profiles serve it on the request port their
// Service already targets, so their contract is unchanged. The value is read from the profile
// rather than discovered from a Pod, because a Pod port is what a container happens to have open
// and not something a caller may read a view from.
func modelDeploymentRouterObserverPort(profile string) int32 {
	if profile == workercore.ModelDeploymentRouterLLMD {
		return modelDeploymentRouterMetricsPort
	}

	return modelDeploymentRouterHTTPPort
}

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

// modelDeploymentServingFetchTimeout bounds ONE read of a Router's view.
//
// IT IS SHORTER THAN THE COLLECTION'S BUDGET ON PURPOSE. A collection is a sum of its reads, so a
// read that inherits only the collection's budget can spend all of it on one Router that never
// answers and leave the other Routers nothing to read with. The collection's own budget still caps
// the whole, and a caller's deadline that is earlier than this one still wins over both.
const modelDeploymentServingFetchTimeout = 2 * time.Second

// modelDeploymentObservationContext derives the context ONE read runs under, so a caller that
// supplied its own deadline keeps it and an unlimited caller still gets a finite one.
//
// THE DEADLINE IT ADDS IS THE PER-FETCH ONE, not the collection's budget: this transport is
// reachable on its own, and a read that waits for the whole collection's budget is a read that can
// take the whole pass with it.
func modelDeploymentObservationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, modelDeploymentServingFetchTimeout)
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
	view RouterServingView, pods []*corev1.Pod, journal bool,
) ([]ServingEndpoint, []ServingEndpoint, error) {
	byIP := make(map[string][]*corev1.Pod, len(pods))
	for _, pod := range pods {
		if ip := pod.Status.PodIP; ip != "" {
			byIP[ip] = append(byIP[ip], pod)
		}
	}

	bound := make([]ServingEndpoint, 0)
	dispatched := make([]ServingEndpoint, 0)
	for _, worker := range view.Workers {
		// A worker outside the selection is not counted as serving, whether or not it appears in
		// the journal. The journal is separate evidence, not a promotion back into Bound.
		inSelection := worker.Selection.InSelection

		// The journal is asked only of the profile that publishes it. The other two publish the
		// gateway's own last_job shape under that key, which this operator does not interpret, and
		// a block this operator cannot read is a block it must not read as "no recent dispatch".
		recent := false
		if !inSelection && journal {
			failure := error(nil)
			recent, failure = recentDispatchFromLastJob(worker.LastJob)
			if failure != nil {
				return nil, nil, failure
			}
		}

		endpoint, err := bindRouterObservationWorker(worker, byIP)
		if err != nil {
			// A SELECTABLE worker the operator cannot place is an indeterminate binding, and one
			// indeterminate binding makes the whole view unusable: a number built on a partial
			// binding would be a guess wearing a confirmation's clothes.
			//
			// A worker the router ITSELF already refused is different. It is not counted either
			// way, so an unplaceable one contributes no evidence rather than a reason to refuse
			// the view, and the view's own contradiction checks simply have nothing from it.
			if inSelection {
				return nil, nil, err
			}

			// A JOURNAL ROW THAT CANNOT BE PLACED IS UNAVAILABLE EVIDENCE, NOT NO EVIDENCE. The
			// dispatch journal is the only thing standing between a target with a recent dispatch
			// and a member that may still be serving it, so a row whose member cannot be resolved
			// has to fail closed: dropping it would read as a cleared journal and release a member
			// on the strength of a row this operator could not place.
			if !inSelection && recent {
				return nil, nil, err
			}

			continue
		}

		// BOTH SELECTIONS ARE KEPT, because the aggregate's only contradiction checks live on the
		// union: a physical endpoint one router selects and another refuses, or one view both
		// selects and refuses, is a disagreement that is invisible while the refused rows are
		// dropped on the way in. The Serving flag below is what separates the two.
		bound = append(bound, endpoint)
		if !inSelection && recent {
			dispatched = append(dispatched, endpoint)
		}
	}

	return bound, dispatched, nil
}

// bindRouterObservationWorker resolves one worker row to the live Pod its address names, and derives
// the workload identity it is counted under. The address and the live Pod decide what it is; nothing
// the registry says about the row contributes to that.
func bindRouterObservationWorker(
	worker RouterObservedWorker, byIP map[string][]*corev1.Pod,
) (ServingEndpoint, error) {
	host := workerHost(worker.URL)
	if host == "" {
		return ServingEndpoint{},
			fmt.Errorf("binding indeterminate: worker URL %q has no address", worker.URL)
	}
	candidates := byIP[host]
	if len(candidates) != 1 {
		return ServingEndpoint{}, fmt.Errorf(
			"binding indeterminate: %s resolves to %d live pods, not one", host, len(candidates))
	}
	pod := candidates[0]
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return ServingEndpoint{}, fmt.Errorf(
			"binding indeterminate: %s is pod %s which is not live", host, pod.Name)
	}
	identity, err := modelDeploymentEndpointIdentityOf(pod, worker.Port)
	if err != nil {
		return ServingEndpoint{}, err
	}

	return ServingEndpoint{
		PodUID:   pod.UID,
		Member:   pod.Name,
		Role:     identity.role,
		Rank:     identity.rank,
		Listener: identity.listener,
		URL:      worker.URL,
		Port:     worker.Port,
		Serving:  worker.Selection.InSelection,
	}, nil
}

// modelDeploymentEndpointIdentity is the workload identity one bound endpoint is counted under. All
// three fields come from the bound Pod's own rendered labels and ports, and every one of them is
// required, because a count built on a partly-identified endpoint cannot be attributed to a member.
type modelDeploymentEndpointIdentity struct {
	role     string
	rank     string
	listener int32
}

// modelDeploymentEndpointIdentityOf reads that identity off the live Pod.
//
// THE OBSERVER'S OWN worker_id IS NOT A SOURCE HERE. It is an opaque native registry identity the
// router mints for its own bookkeeping, and on a data-parallel member it enumerates VIRTUAL ranks
// that all share ONE physical listener. Reading it as a member rank would turn one engine into as
// many endpoints as it has shards, which is a number about the router's registry rather than about
// the pool. The rank comes from the member-index label the render stamps, which is this operator's
// own count of which member of its replica a Pod is.
//
// EACH FIELD IS VALIDATED RATHER THAN STORED, and an endpoint that carries none of them is refused
// instead of counted. A missing role names no workload, a non-decimal rank is not the ordinal the
// render writes, and a port the Pod never declared is not a listener it can be serving on: a count
// that included any of them would be reporting something the Pod does not say about itself.
func modelDeploymentEndpointIdentityOf(pod *corev1.Pod, urlPort string) (modelDeploymentEndpointIdentity, error) {
	role := modelDeploymentPodRole(pod)
	if role == "" {
		return modelDeploymentEndpointIdentity{}, fmt.Errorf(
			"binding indeterminate: member %s carries no workload role", pod.Name)
	}

	rank := pod.Labels[modelDeploymentMemberIndexLabel]
	if rank == "" {
		return modelDeploymentEndpointIdentity{}, fmt.Errorf(
			"binding indeterminate: member %s carries no member rank", pod.Name)
	}
	// A MEMBER INDEX COUNTS FROM ZERO, so it is read as an unsigned decimal. ParseUint refuses a
	// sign, a base prefix and any surrounding space, all of which are shapes the render never
	// writes and all of which would be a different meaning read as this one.
	if _, err := strconv.ParseUint(rank, 10, 32); err != nil {
		return modelDeploymentEndpointIdentity{}, fmt.Errorf(
			"binding indeterminate: member %s carries member rank %q rather than a decimal index",
			pod.Name, rank)
	}

	listener := modelDeploymentDeclaredListenerOf(pod, urlPort)
	if listener == 0 {
		return modelDeploymentEndpointIdentity{}, fmt.Errorf(
			"binding indeterminate: member %s declares no container port for %q", pod.Name, urlPort)
	}

	return modelDeploymentEndpointIdentity{role: role, rank: rank, listener: listener}, nil
}

// modelDeploymentDeclaredListenerOf returns the declared container port the bound URL targets,
// or 0 when the pod declares no such port. The declared port is the listener contract the render
// wrote; a port the pod never declared is not a listener this operator can count.
func modelDeploymentDeclaredListenerOf(pod *corev1.Pod, urlPort string) int32 {
	port, err := strconv.Atoi(urlPort)
	if err != nil {
		return 0
	}
	for i := range pod.Spec.Containers {
		for _, declared := range pod.Spec.Containers[i].Ports {
			if int(declared.ContainerPort) == port {
				return declared.ContainerPort
			}
		}
	}

	return 0
}

// recentDispatchFromLastJob reads the journal field the shipped llm-d observer writes.
//
// The producer omits last_job entirely for a worker it never dispatched to, so an absent block is
// the absence of a recent dispatch rather than a missing required field. Requiring it would make
// every untouched worker an error. A block that is present must carry recent_dispatch as a boolean,
// because the producer emits it only to mark a dispatch. A block without it, or with it as anything
// else, is a shape this operator does not understand, and it is refused rather than read as false:
// a decoder default would turn a journal it could not read into a confirmed absence of a dispatch,
// which is the one reading this field must never get.
//
// A worker outside the selection with no journal block is left to the caller. The other two
// routers publish the gateway's own last_job shape there, which this operator does not interpret.
func recentDispatchFromLastJob(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var job struct {
		Recent *bool `json:"recent_dispatch"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return false, fmt.Errorf("the dispatch journal is not readable: %w", err)
	}
	if job.Recent == nil {
		return false, fmt.Errorf(
			"the dispatch journal carries no recent_dispatch boolean, so it cannot be read")
	}

	return *job.Recent, nil
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
	// ByRole is the same union counted per workload role, over the very endpoints Value sums.
	// It exists so a role's own status field is not filled with the deployment-wide number: a
	// prefill role reporting "two" because a decode role has two serving endpoints is a status
	// that reads as its own pool and is not. A role with no endpoint is an explicit zero, which
	// is why the map is built for every role the union names and the reader supplies the rest.
	ByRole map[string]int32
	// Groups is the same collection read per GROUP rather than per deployment. The deployment-wide
	// answer cannot tell two groups of one role apart, and a group whose view is partial has to be
	// distinguishable from a group that is merely empty.
	Groups map[modelDeploymentGroupKey]modelDeploymentGroupServing
	// Members is each member's carried facts as this collection read them, which is the fresh
	// read and not the caller's cached Pod list.
	Members map[types.UID]modelDeploymentMemberServing
	// CollectedAt is when the OLDEST evidence in this answer was read. The freshness of the whole
	// answer is the freshness of its oldest evidence, so this is the instant the restore decision
	// compares against the freshness window.
	CollectedAt time.Time
}

// modelDeploymentGroupKey identifies one group: the role it belongs to, the replica ordinal it
// occupies, and the CURRENT membership generation of that group.
//
// THE GENERATION IS THE GROUP'S CURRENT MEMBERSHIP AND NEVER THE MEMBERS THE ROUTER HAPPENED TO
// LIST. A key built from the observed members would not match the group it belongs to on a partial
// view, and the group would silently never get an answer -- the worst outcome here, because a
// partial view is exactly where a confirmed serving count must not be published.
type modelDeploymentGroupKey struct {
	Role       string
	Ordinal    int
	Generation modelDeploymentMembershipGeneration
}

// modelDeploymentGroupServing is what the operator knows about ONE group from the Router's view.
//
// THE COUNTS ARE TWO KINDS OF THING. Endpoints counts the ROUTER endpoints in selection for this
// group, while Members and Expected count MEMBERS: an ordinary group of three members has one
// expected endpoint and three members, and a count that conflated the two would publish a group of
// three as three endpoints.
type modelDeploymentGroupServing struct {
	// SnapshotValid is whether this operator holds a fresh, complete and coherent observation of
	// this group. It is the restoration gate's input, and it is NOT the confirmation.
	SnapshotValid bool
	// InvalidReason names why the snapshot is not valid, in words a reader can act on.
	InvalidReason string
	// State is the serving state of this group.
	State workercore.ModelDeploymentServingState
	// Endpoints counts the Router endpoints in selection for this group.
	Endpoints int32
	// Members counts the group's current membership.
	Members int32
	// Expected counts the group's expected member count.
	Expected int32
	// Retained is whether the group is ELIGIBLE AND UNCHANGED GENERATION: every answering member
	// already carries the eligibility label, and the eligibility annotation on those members names
	// the group's current complete membership generation.
	//
	// IT IS THE ONE FACT THAT SEPARATES THE TWO CASES A STATUS HAS TO TELL APART. A retained group
	// publishes Eligible while the observation is lost, and a group the barrier holds publishes
	// AwaitingObservation, and a composite flag would collapse exactly that distinction.
	Retained bool
	// MemberNotReady says the fresh membership found a member of this group that is not ready.
	//
	// IT IS A KNOWN FACT AND NOT A MISSING ONE, so a later uncertainty does not erase it: a
	// collection that could not read the Router's view afterwards still knows that this member was
	// not ready. The state has to name the leg that holds the group, and the reason that names the
	// member travels with this fact.
	MemberNotReady bool
}

// modelDeploymentMemberServing is one member's carried facts as the collection read them.
//
// THEY ARE READ FRESH, AT COLLECTION TIME, AND NOT FROM THE CALLER'S POD LIST. The caller's list is
// the informer cache's view of the deployment, and a cached value that has fallen behind is not new
// proof of anything: a retention decision built on it would keep a group eligible on the strength
// of a label the API server no longer carries.
type modelDeploymentMemberServing struct {
	// Eligible is whether the member carried the eligibility label.
	Eligible bool
	// Generation is the membership generation the eligibility annotation named, empty when the
	// member carries no annotation.
	Generation modelDeploymentMembershipGeneration
	// Ready is whether the member reported ready in the fresh read.
	//
	// IT IS READ FRESH AND NOT FROM THE CALLER'S LIST, for the same reason the label is: the
	// qualification's own readiness legs read the caller's list, and a list that is behind the API
	// server cannot say that a member is ready. The fresh word is the one the collection is
	// defined against, so it is the one that decides whether the group may be served.
	Ready bool
}

// modelDeploymentPassObservation is what ONE pass observed about the deployment's groups, and it is
// the carrier every reader of a pass's answers shares.
//
// THE RESTORATION DECISION AND THE STATUS MUST READ THE SAME ONE. A second collection would be a
// second answer about a second moment, and a fresh qualification would be a second verdict about a
// second grouping of the same Pods, so a status could publish a verdict the labels were never
// written from.
type modelDeploymentPassObservation struct {
	// Serving is the collected Router answer, with the per-group facts and the freshness instant.
	Serving ServingAnswer
	// Qualifications is the whole-group answer the eligibility write acted on.
	Qualifications []modelDeploymentInstanceQualification
	// CollectiveMembers is the current membership of the deployment's logical group when the shape
	// has one that is not a replica: the elastic collective's whole worker set.
	//
	// IT IS EMPTY FOR EVERY OTHER SHAPE, and the status then publishes the replica groups alone.
	// The consumer identifies the elastic role from the deployment, so this carries the members and
	// no group of its own.
	//
	// IT IS THE ALREADY-FETCHED, OWNER-CHECKED LIST, so populating it costs no second fetch and no
	// probe.
	CollectiveMembers []corev1.Pod
}

// modelDeploymentServingUnobserved is the answer a path that made no observation publishes.
//
// IT IS NOT THE ZERO VALUE, because the zero value's state is the empty string and that is not a
// serving state at all: publishing it would tell a reader that the operator knows something it does
// not. A deployment that declares no Router says so, and a deployment that declares one says that
// this pass did not read it.
func modelDeploymentServingUnobserved(md *workercore.ModelDeployment, reason string) ServingAnswer {
	if md.Spec.Router == nil {
		return ServingAnswer{State: workercore.ModelDeploymentServingStateNotConfigured}
	}

	return ServingAnswer{
		State:  workercore.ModelDeploymentServingStateUnknown,
		Reason: reason,
	}
}

// modelDeploymentAnnotationKeyEndpointEligibleGeneration names the group's membership generation
// under which a member's eligibility was established.
//
// IT IS AN ANNOTATION AND NOT A LABEL, because the generation is the sorted member UID set joined
// with slashes and a label value cannot hold a slash. Its absence means the member was never made
// eligible, and a value that differs from the group's current generation means the generation
// changed after the eligibility was established.
//
// IT IS WRITTEN IN THE SAME PATCH AS THE ELIGIBILITY LABEL, so the two cannot disagree, and it is
// per-Pod, so it is gone with the Pod that carried it.
const modelDeploymentAnnotationKeyEndpointEligibleGeneration = "modeldeployment." + systemname.LabelPrefix +
	"endpoint-eligible-generation"

// modelDeploymentServingFreshNow reports whether a collected answer is still inside the freshness
// window at the moment of the decision that reads it.
//
// THE RECHECK IS A COMPARISON AGAINST THE EXISTING CLOCK AND THE EXISTING WINDOW, and never a
// second fetch: the answer was collected once, and reading the routers again would be a second
// collection with a second budget and a second answer.
//
// IT EXISTS BECAUSE THE WINDOW IS JUDGED ONCE, AT THE AGGREGATION, AND THE DECISION THAT USES THE
// ANSWER MAY COME LATER. The label write that follows the collection is a sequence of API round
// trips, one per changed member, and nothing bounds it: an answer judged fresh when it was
// aggregated can be stale by the time the last member is written.
func (r *ModelDeploymentReconciler) modelDeploymentServingFreshNow(answer ServingAnswer) bool {
	if answer.CollectedAt.IsZero() {
		return false
	}

	return r.modelDeploymentNow().Sub(answer.CollectedAt) <= modelDeploymentServingFreshness
}

// modelDeploymentPhysicalEndpoint is what the union counts: one declared listener on one Pod. Every
// virtual row the router registers for that listener folds onto this one key.
type modelDeploymentPhysicalEndpoint struct {
	podUID   types.UID
	listener int32
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
	//
	// THE FOLD KEY IS THE PHYSICAL LISTENER, NOT THE ROW. One member's engine can appear in a
	// view as several rows, because a data-parallel member registers one entry per virtual rank
	// and they all share the single listener that member actually serves. Counting rows would
	// report the router's registry size rather than the pool's serving members, so the rows that
	// name one Pod and one declared port fold into one endpoint. The role is checked on the folded
	// endpoint rather than added to the key, so two views that seat the same listener in
	// different roles are caught as a contradiction instead of counted as two.
	physical := map[modelDeploymentPhysicalEndpoint]ServingEndpoint{}
	for _, observation := range observations {
		for _, endpoint := range observation.Bound {
			key := modelDeploymentPhysicalEndpoint{
				podUID:   endpoint.PodUID,
				listener: endpoint.Listener,
			}
			previous, seen := physical[key]
			if seen {
				if previous.Serving != endpoint.Serving {
					return ServingAnswer{
						State: workercore.ModelDeploymentServingStateNotConverged,
						Reason: fmt.Sprintf(
							"views disagree about whether pod %s listener %d is selected",
							endpoint.PodUID, endpoint.Listener),
					}
				}
				if previous.Role != endpoint.Role {
					return ServingAnswer{
						State: workercore.ModelDeploymentServingStateNotConverged,
						Reason: fmt.Sprintf("views seat pod %s listener %d as both %q and %q",
							endpoint.PodUID, endpoint.Listener, previous.Role, endpoint.Role),
					}
				}
			}
			physical[key] = endpoint
		}
	}

	// THE GLOBAL COUNT AND THE PER-ROLE COUNTS COME FROM THE SAME FOLD, in one pass over it, so
	// they cannot describe different pools. Counting the union twice would be two answers to one
	// question, and they would drift the first time a role's endpoints changed.
	count := int32(0)
	byRole := make(map[string]int32, len(physical))
	for _, endpoint := range physical {
		if !endpoint.Serving {
			continue
		}
		count++
		byRole[endpoint.Role]++
	}

	return ServingAnswer{
		State:  workercore.ModelDeploymentServingStateConfirmed,
		Value:  &count,
		ByRole: byRole,
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
	// ONE READ IS BOUNDED ON ITS OWN, at the per-fetch deadline, and the caller's own context still
	// governs it: the earlier of the two deadlines wins, and a canceled caller ends the request
	// with it. Without that bound a request that inherits only the collection's budget waits for
	// that whole budget on one Router that never answers, which is exactly the shape of a Router
	// that has wedged.
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
//
// IT IS THE ONE COLLECTION OF A PASS. The status and the restoration gate read this answer and
// nothing else, so the two cannot answer about different moments.
func (r *ModelDeploymentReconciler) observeModelDeploymentServing(
	ctx context.Context, md *workercore.ModelDeployment,
) ServingAnswer {
	if md.Spec.Router == nil {
		return aggregateModelDeploymentServing(false, nil, 0)
	}
	ctx, cancel := r.modelDeploymentRouterObservationContext(ctx, md)
	defer cancel()

	observations, live, failure := r.collectModelDeploymentRouterObservationsWithMembers(ctx, md)
	if failure.reason != "" {
		answer := ServingAnswer{
			State:  workercore.ModelDeploymentServingStateUnknown,
			Reason: failure.reason,
		}
		if !failure.retainable || len(live) == 0 {
			// NO FRESH MEMBERSHIP, OR THE FAILURE PROVED THAT SOMETHING MOVED. There is no fact to
			// publish, and a fact is the only thing that can hold a group open.
			return answer
		}

		// THE GROUP FACTS ARE STILL PUBLISHED, because they are computed from the fresh membership
		// rather than from the view, and a view that was never read is not what a group is judged
		// on. The facts say Unknown and carry no confirmed count, and the retention they carry is
		// the existing one: every answering member holds the eligibility label and the annotation
		// on it names the group's current complete membership generation.
		answer.CollectedAt = failure.readAt
		answer.Groups, answer.Members = modelDeploymentServingGroups(md, live, nil, answer)

		return answer
	}

	answer := aggregateModelDeploymentServing(true, observations, modelDeploymentServingFreshness)
	answer.CollectedAt = modelDeploymentServingOldest(observations)
	answer.Groups, answer.Members = modelDeploymentServingGroups(md, live, observations, answer)

	// THE PUBLIC CONFIRMATION IS FILTERED BY THE GROUP FACTS, AND THE RAW EVIDENCE IS NOT TOUCHED.
	// The aggregate keeps its own semantics, because the retirement path reads them, and a group
	// seen half is not a deployment confirmed.
	if answer.State == workercore.ModelDeploymentServingStateConfirmed {
		for _, group := range answer.Groups {
			if group.SnapshotValid {
				continue
			}
			answer.State = workercore.ModelDeploymentServingStateUnknown
			answer.Value = nil
			answer.Reason = group.InvalidReason

			break
		}
	}

	return answer
}

// modelDeploymentServingOldest is when the oldest evidence in the collection was read.
//
// THE FRESHNESS OF AN ANSWER IS THE FRESHNESS OF ITS OLDEST EVIDENCE, so this is the instant the
// restoration decision compares against the freshness window.
func modelDeploymentServingOldest(observations []RouterServingObservation) time.Time {
	oldest := time.Time{}
	for _, observation := range observations {
		if observation.CollectedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || observation.CollectedAt.Before(oldest) {
			oldest = observation.CollectedAt
		}
	}

	return oldest
}

// modelDeploymentServingGroups computes the per-group serving facts and the per-member carried
// facts from the fresh membership and the collected views.
//
// THE MEMBERSHIP IS THE FRESH READ THE COLLECTION MADE, and the carried facts come with it: they
// are read from the same objects the views were bound to, so a member that was relabelled between
// the two reads cannot be counted as one that carried the label.
func modelDeploymentServingGroups(
	md *workercore.ModelDeployment,
	live []*corev1.Pod,
	observations []RouterServingObservation,
	answer ServingAnswer,
) (map[modelDeploymentGroupKey]modelDeploymentGroupServing, map[types.UID]modelDeploymentMemberServing) {
	members := make(map[types.UID]modelDeploymentMemberServing, len(live))
	for _, pod := range live {
		members[pod.UID] = modelDeploymentMemberServing{
			Eligible: pod.Labels[modelDeploymentLabelKeyEndpointEligible] ==
				modelDeploymentEndpointEligibleValue,
			Generation: modelDeploymentMembershipGeneration(
				pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration]),
			Ready: podIsReady(pod),
		}
	}

	// THE ENDPOINTS THE ROUTER HOLDS, PER MEMBER AND PER LISTENER. The fold is the physical
	// listener for the reason the deployment-wide union uses it: one member's engine registers a
	// row per virtual rank and they all share the one listener that member serves.
	held := map[types.UID]map[int32]bool{}
	for _, observation := range observations {
		for _, endpoint := range observation.Bound {
			if held[endpoint.PodUID] == nil {
				held[endpoint.PodUID] = map[int32]bool{}
			}
			held[endpoint.PodUID][endpoint.Listener] = endpoint.Serving
		}
	}

	pods := make([]corev1.Pod, 0, len(live))
	for _, pod := range live {
		pods = append(pods, *pod)
	}

	// THE COLLECTION'S OWN VERDICT GATES THE GROUP FACTS.
	//
	// AN ANSWER THAT IS NOT CONFIRMED SAYS THE COLLECTION WAS INCOMPLETE: no view was read, one
	// was unusable, one was stale, or two views disagreed. An incomplete collection proves NOTHING
	// ABOUT WHAT THE ROUTER HOLDS, and the group facts are computed from the endpoints that were
	// bound, so without this gate a Router that did not answer would read as a Router that holds
	// nothing, and the group would restore on the strength of a read that never happened.
	//
	// THE RETENTION FACT IS NOT GATED BY IT, because the retention is proved by the fresh
	// membership rather than by the view: an already eligible group of the unchanged generation
	// keeps what it has while the answer is lost, which is exactly what an unusable answer is.
	unsound := answer.State != workercore.ModelDeploymentServingStateConfirmed
	unsoundReason := answer.Reason

	groups := make(map[modelDeploymentGroupKey]modelDeploymentGroupServing)
	for _, view := range modelDeploymentGroupPods(pods, true) {
		generation := modelDeploymentGenerationOf(view)
		key := modelDeploymentGroupKey{Role: view.Role, Ordinal: view.Ordinal, Generation: generation}
		fact := modelDeploymentGroupFact(md, view, generation, members, held)
		if unsound {
			fact.SnapshotValid = false
			fact.State = workercore.ModelDeploymentServingStateUnknown
			// A KNOWN FACT IS NOT ERASED BY A LATER UNCERTAINTY. The collection read the fresh
			// membership and found a member that is not ready, and that word is a fact about the
			// group whatever the Router's view did afterwards. The collection's own reason keeps
			// its own place: it is the answer's reason, and it names the collection.
			if !fact.MemberNotReady {
				fact.InvalidReason = unsoundReason
			}
		}
		groups[key] = fact
	}

	return groups, members
}

// modelDeploymentGroupFact is what one group looks like from the collected views.
//
// THE EXPECTED ENDPOINTS ARE THE ANSWERING SET, read from the shape helpers rather than
// re-derived here: an ordinary group of three members has ONE expected endpoint and three members,
// and a count that conflated the two would publish a group of three as three endpoints.
func modelDeploymentGroupFact(
	md *workercore.ModelDeployment,
	view modelDeploymentReplicaView,
	generation modelDeploymentMembershipGeneration,
	members map[types.UID]modelDeploymentMemberServing,
	held map[types.UID]map[int32]bool,
) modelDeploymentGroupServing {
	fact := modelDeploymentGroupServing{Members: int32(len(view.Members))}

	shape := modelDeploymentDeployedReplicaShape(view)
	fact.Expected = int32(shape.Total)
	if shape.State == modelDeploymentReplicaShapeUnreadable {
		fact.State = workercore.ModelDeploymentServingStateUnknown
		fact.InvalidReason = shape.Reason

		return fact
	}

	// A GROUP WITH A MEMBER THAT IS NOT READY IS NOT A GROUP THAT MAY BE SERVED, whatever the
	// Router holds. The Router's view answers whether the Router is serving the group; readiness
	// answers whether the group may be served at all, and the fresh membership is the freshest
	// word on that.
	//
	// NO ACCEPTANCE PATH OPENS FOR AN UNSAFE GROUP. Not the retention, and not the fresh restore
	// either: a collection that read the whole answering set confirms the Router's view of the
	// group, and a confirmation about a group that may not be served is not a confirmation that
	// restores it. The count stays unconfirmed, because only a confirmation carries one.
	if reason, unsafe := modelDeploymentGroupUnsafe(view, members); unsafe {
		fact.SnapshotValid = false
		fact.State = workercore.ModelDeploymentServingStateUnknown
		fact.InvalidReason = reason
		fact.MemberNotReady = true

		return fact
	}

	answering, reason := modelDeploymentAnsweringMembers(
		modelDeploymentDeployedAnsweringShape(view, md.Spec.Engine.Name), view.Members)
	if reason != "" {
		fact.State = workercore.ModelDeploymentServingStateUnknown
		fact.InvalidReason = reason

		return fact
	}

	expected := make(map[types.UID]bool, len(answering))
	for _, member := range answering {
		expected[member.UID] = true
	}

	// THE ENDPOINTS THE ROUTER HOLDS FOR THIS GROUP, against the ones it is expected to hold.
	observed := make(map[types.UID]bool, len(view.Members))
	endpoints := int32(0)
	for _, member := range view.Members {
		listeners, bound := held[member.UID]
		if !bound {
			continue
		}
		observed[member.UID] = true
		for _, serving := range listeners {
			if serving && expected[member.UID] {
				endpoints++
			}
		}
	}

	// A GROUP WHOSE EXPECTED ENDPOINTS CANNOT BE JUDGED IS A GROUP WHOSE COMPLETENESS CANNOT BE
	// JUDGED. The Router holds either every expected endpoint of the group or none of them: a
	// partial view is a contradiction, and a group seen half is not a group seen.
	switch {
	case len(observed) == 0:
		// A COHERENT ZERO IS NOT A FAULT. It says the Router is not serving this group, which is a
		// fact about the Router and not a verdict on the group, and it does not block a restore.
		fact.SnapshotValid = true
		fact.State = workercore.ModelDeploymentServingStateConfirmed
	case modelDeploymentSameMembers(observed, expected):
		fact.SnapshotValid = true
		fact.State = workercore.ModelDeploymentServingStateConfirmed
		fact.Endpoints = endpoints
	default:
		heldExpected := 0
		for uid := range observed {
			if expected[uid] {
				heldExpected++
			}
		}
		fact.State = workercore.ModelDeploymentServingStateUnknown
		fact.InvalidReason = fmt.Sprintf(
			"the router's view of the group is partial: it holds %d of the group's %d expected "+
				"endpoints, so the group's serving cannot be confirmed",
			heldExpected, len(expected))

		return fact
	}

	// THE RETENTION FACT: every answering member carries the eligibility label, and the eligibility
	// annotation on those members names the group's CURRENT complete membership generation.
	fact.Retained = true
	for _, member := range answering {
		carried, known := members[member.UID]
		if !known || !carried.Eligible || carried.Generation != generation {
			fact.Retained = false

			break
		}
	}

	return fact
}

// modelDeploymentGroupUnsafe reports whether a member of the group is not ready, and why.
//
// A GROUP WITH AN UNREADY MEMBER IS UNSAFE. A member that has not reported ready is not a member
// the group may be served by, and its healthy sibling does not make it one.
//
// THE FRESH MEMBERSHIP DECIDES, because the qualification's own readiness legs read the caller's
// list. A list that is behind the API server cannot say that a member is ready, so it cannot close
// a group the fresh read has found unsafe, and the fresh word is the one the collection is defined
// against.
func modelDeploymentGroupUnsafe(
	view modelDeploymentReplicaView, members map[types.UID]modelDeploymentMemberServing,
) (string, bool) {
	for _, member := range view.Members {
		carried, known := members[member.UID]
		if !known {
			continue
		}
		if !carried.Ready {
			return fmt.Sprintf(
				"the group's member %s is not ready, so the group may not be served",
				member.Name), true
		}
	}

	return "", false
}

// modelDeploymentSameMembers reports whether two member sets hold the same members.
//
// THE SETS ARE COMPARED AND NOT THE COUNTS, because a group of three whose Router holds one
// follower and not the leader holds as many members as a group whose Router holds the leader
// alone, and only the sets tell them apart.
func modelDeploymentSameMembers(a, b map[types.UID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for uid := range a {
		if !b[uid] {
			return false
		}
	}

	return true
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
	ctx context.Context, md *workercore.ModelDeployment, _ []*corev1.Pod,
) ([]RouterServingObservation, string) {
	observations, _, failure := r.collectModelDeploymentRouterObservationsWithMembers(ctx, md)

	return observations, failure.reason
}

// modelDeploymentCollectionFailure is why a collection produced no answer, and what that failure
// leaves the operator allowed to conclude.
//
// THE CLASS IS THE POINT, AND THE REASON STRING CANNOT CARRY IT. A failure that proved nothing
// about the group's identity leaves a group that was already qualified at its current generation
// with what it has, and the serving answer says Unknown. A failure that proved that something
// moved closes the group, because the group is no longer the group that was qualified.
//
// A FAILURE THAT IS NOT RETAINABLE IS A CLOSED ONE. There is no third state, and a fact is the
// only thing that can hold a group open: a group fact is published on a failure path only when it
// is computed from a membership read that actually happened.
type modelDeploymentCollectionFailure struct {
	reason string
	// retainable says the failure proved nothing about the group's identity.
	retainable bool
	// readAt is when the fresh membership was read. It is the instant a failure-path answer
	// carries, because that membership is the only evidence such an answer holds.
	readAt time.Time
}

// modelDeploymentGuardError says WHICH read of the guard failed, because the opening and the
// closing guard do not treat their failures alike.
//
// THE OPENING GUARD BINDS THE PASS TO THE OBJECT IT WAS ASKED ABOUT, so a failure to read the
// deployment's own identity closes every group. THE CLOSING GUARD IS A BOOKEND: its failure means
// it proved nothing, which is not the same as proving that something moved.
type modelDeploymentGuardError struct {
	reason string
	// identity says the deployment's own identity could not be read.
	identity bool
	// routers says the Router processes could not be listed.
	routers bool
	// members says the membership could not be listed.
	members bool
}

func (f modelDeploymentGuardError) Error() string { return f.reason }

// collectModelDeploymentRouterObservationsWithMembers is the collection and the fresh membership it
// read.
//
// THE MEMBERSHIP COMES BACK BECAUSE THE GROUP FACTS ARE COMPUTED AGAINST IT. The per-group facts
// read the answering set and each member's carried label and annotation, and reading them from the
// caller's Pod list instead would read the informer cache's view of the deployment: a cached value
// that has fallen behind is not new proof that a member was ever eligible.
func (r *ModelDeploymentReconciler) collectModelDeploymentRouterObservationsWithMembers(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]RouterServingObservation, []*corev1.Pod, modelDeploymentCollectionFailure) {
	collectCtx, cancel := r.modelDeploymentRouterObservationContext(ctx, md)
	defer cancel()
	if err := collectCtx.Err(); err != nil {
		return nil, nil, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf("the router observation budget is exhausted: %v", err),
		}
	}

	// THE FRESH MEMBERSHIP IS READ FIRST, AND READ ONCE.
	//
	// IT IS THE EVIDENCE THE RETENTION RESTS ON, so it is read before anything that can fail, and
	// the opening guard takes it rather than listing the members a second time. The closing guard
	// still lists them, because its whole purpose is to see whether they moved.
	//
	// THE PASS IS STILL BOUNDED BY ONE BUDGET AND ONE ROUND TRIP: no read was added, and one was
	// removed.
	live, err := r.listFreshModelDeploymentPods(collectCtx, md)
	if err != nil {
		return nil, nil, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf("the deployment's pods could not be listed uncached: %v", err),
		}
	}
	readAt := r.modelDeploymentNow()

	// THE COLLECTION IS BOUNDED BY THE LIVE OBJECTS, not by the snapshot this pass started
	// from. The deployment and its membership are read through the uncached reader before and
	// after the views are collected; a changed UID, generation, owner or membership means the
	// views describe a pool this pass no longer sees, and confirmed absence from an earlier
	// snapshot would be a lie about the current one.
	// EVERY FRESH READ SHARES THE COLLECTION BUDGET. The bookends are part of this pass, and a
	// guard read on the caller's own context could outlive the pass it is bounding.
	//
	// THE FIRST BOOKEND IS TAKEN BEFORE ANY DISCOVERY. It is the state the pass is defined
	// against, so a Router that appears or a member that leaves after it is a change underneath
	// this collection, which is exactly what the closing bookend is there to catch.
	before, guardFailure := r.snapshotModelDeploymentObservationGuard(collectCtx, md, live)
	if guardFailure.reason != "" {
		if guardFailure.identity {
			// THE PASS COULD NOT BIND ITSELF TO THE OBJECT IT WAS ASKED ABOUT, so it can conclude
			// nothing about any group in it.
			return nil, nil, modelDeploymentCollectionFailure{reason: guardFailure.reason}
		}

		// THE ROUTERS COULD NOT BE LISTED. The group is not the Router, and the fresh membership
		// is in hand, so the groups keep what they had.
		return nil, live, modelDeploymentCollectionFailure{
			reason:     guardFailure.reason,
			retainable: true,
			readAt:     readAt,
		}
	}
	// THE FIRST BOOKEND IS ALSO COMPARED WITH THE CALLER. Two equal fresh snapshots are a fact
	// about the two reads alone, so a caller holding a REPLACED deployment would see the same
	// before and after and read its own stale object as an unchanged one. The caller's UID and
	// generation are the claim this collection has to be about.
	if !before.matchesCaller(md) {
		return nil, nil, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf(
				"the caller read deployment %s at generation %d but the deployment is now %s at "+
					"generation %d; the views would describe an object this pass was not asked about",
				md.UID, md.Generation, before.deploymentUID, before.generation),
		}
	}

	// DISCOVERY READS LIVE. The Router processes this pass will bind are the ones the API server
	// still holds, not the ones the watch last delivered: a Router that rolled between the cache
	// and this read would otherwise be read at an address the collection can no longer prove.
	routerPods := &corev1.PodList{}
	if err := r.APIReader.List(collectCtx, routerPods,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
			modelDeploymentRouterLabelKey:   md.Spec.Router.Name,
		},
	); err != nil {
		return nil, live, modelDeploymentCollectionFailure{
			reason:     fmt.Sprintf("the router's pods could not be listed: %v", err),
			retainable: true,
			readAt:     readAt,
		}
	}

	// BINDING USES THE SAME FRESH OBJECTS, not the caller's list. The endpoints a view binds to
	// are resolved against Pods this pass re-read, so a member that was replaced, relabelled or
	// replaced in place since the caller listed them cannot be counted as the member it claims.
	fetch := r.servingViewFetch
	if fetch == nil {
		fetch = defaultServingViewFetch
	}

	// The observer is served on the listener the owning profile already runs. llm-d serves it on
	// its management listener, which is where its metrics and admin endpoints already are; the
	// other two serve it on the request port their Service targets. The port comes from the
	// profile, never from an arbitrary Pod port, because a Pod port is not a contract.
	port := modelDeploymentRouterObserverPort(md.Spec.Router.Name)
	// The dispatch journal is read only from the profile that publishes it. The other two publish
	// the gateway's own last_job shape under that key, which this operator does not interpret.
	journal := md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD
	// The chain behind the Router processes is read once per collection, not once per process.
	// It lives and dies with this pass, so the next one reads it again from the API server.
	owners := newRouterOwnerChain(r.APIReader)
	observations := make([]RouterServingObservation, 0, len(routerPods.Items))
	for i := range routerPods.Items {
		routerPod := &routerPods.Items[i]
		// A terminating Router process is still a running Router process: it holds an address
		// and its observer still answers, and dropping it here would read a shutdown as a
		// converged empty registry. Coverage ends only when the process loses its address.
		if routerPod.Status.PodIP == "" {
			continue
		}
		// The view is only about this deployment when the Pod's owning chain is: each Router
		// process is verified live, through the uncached reader, to be controlled by a
		// ReplicaSet owned by this deployment. A process whose chain is missing or unreadable
		// holds the observation rather than narrowing it.
		if err := r.verifyRouterPodOwnership(collectCtx, md, routerPod, owners); err != nil {
			observations = append(observations, RouterServingObservation{
				Err: fmt.Errorf("router process %s is not verified for this deployment: %w",
					routerPod.Name, err),
			})

			continue
		}
		observation, err := collectRouterServingView(collectCtx, func(
			callCtx context.Context, path string,
		) ([]byte, error) {
			return fetch(callCtx, fmt.Sprintf("http://%s:%d%s",
				routerPod.Status.PodIP, port, path))
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
		observation.Bound, observation.RecentDispatch, err = bindRouterObservationView(observation.View, live, journal)
		if err != nil {
			observation.Err = err
		}
		observations = append(observations, observation)
	}

	if err := collectCtx.Err(); err != nil {
		return nil, live, modelDeploymentCollectionFailure{
			reason:     fmt.Sprintf("the router observation budget is exhausted: %v", err),
			retainable: true,
			readAt:     readAt,
		}
	}

	// THE CHAIN IS BOOKENDED, like the deployment's own identity above. The owners were read
	// once and shared by every process, so this pass re-reads each of them and refuses the whole
	// collection if one moved while the views were being read. Without it, sharing the read would
	// have meant a reparent after the first process went unnoticed by every later one.
	//
	// THE ROUTERS MOVING IS NOT THE GROUP MOVING, so this failure retains: the membership the
	// groups are judged on did not move, and the serving answer says Unknown.
	if err := owners.verify(collectCtx); err != nil {
		return nil, live, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf("the routers' owning chain changed while the routers were being "+
				"observed; the collected views describe an owner this pass can no longer stand behind: %v", err),
			retainable: true,
			readAt:     readAt,
		}
	}

	after, guardFailure := r.snapshotModelDeploymentObservationGuard(collectCtx, md, nil)
	if guardFailure.reason != "" {
		// A KNOWN CHANGE DOMINATES A LATER UNCERTAINTY. The closing bookend may have read enough
		// to prove that the deployment or the membership moved before a later read failed, and a
		// read failure must not erase a change the same snapshot already proved.
		//
		// ONLY THE FACTS THE CLOSING SNAPSHOT ACTUALLY READ ARE COMPARED. A snapshot that failed
		// before it read the deployment knows nothing about it, and a snapshot that failed before it
		// listed the members knows nothing about them.
		if before.deploymentMoved(after) {
			return nil, live, modelDeploymentCollectionFailure{
				reason: fmt.Sprintf(
					"the deployment changed while the routers were being observed; the collected " +
						"views describe an earlier snapshot and cannot confirm absence"),
				readAt: readAt,
			}
		}
		if before.membersMoved(after) {
			return nil, live, modelDeploymentCollectionFailure{
				reason: fmt.Sprintf(
					"the membership changed while the routers were being observed; the collected " +
						"views describe an earlier snapshot and cannot confirm absence"),
				readAt: readAt,
			}
		}

		// THE CLOSING BOOKEND COULD NOT BE READ. It exists to prove that something moved, and it
		// proved nothing.
		return nil, live, modelDeploymentCollectionFailure{
			reason:     guardFailure.reason,
			retainable: true,
			readAt:     readAt,
		}
	}
	if before.identityMoved(after) {
		return nil, live, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf(
				"the deployment or its membership changed while the routers were being observed; " +
					"the collected views describe an earlier snapshot and cannot confirm absence"),
			readAt: readAt,
		}
	}
	if before.routerFacts != after.routerFacts {
		return nil, live, modelDeploymentCollectionFailure{
			reason: fmt.Sprintf(
				"the routers' processes changed while the routers were being observed; the group's " +
					"members did not move, so the group keeps what it has and the serving answer " +
					"stays unknown"),
			retainable: true,
			readAt:     readAt,
		}
	}

	return observations, live, modelDeploymentCollectionFailure{}
}

// listFreshModelDeploymentPods reads the deployment's own Pods through the uncached reader.
//
// IT IS THE CACHED LISTING'S SHAPE WITH THE UNCACHED READER, and that is deliberate: the ownership
// filter the cache applies is the one that decides which Pods this deployment may count at all, and
// re-deriving it here would be a second answer to "which Pods are mine" that could drift from it.
func (r *ModelDeploymentReconciler) listFreshModelDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]*corev1.Pod, error) {
	pods := new(corev1.PodList)
	if err := r.APIReader.List(ctx, pods,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
	); err != nil {
		return nil, err
	}

	owned := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if !modelDeploymentOwns(&pods.Items[i], md) {
			continue
		}
		owned = append(owned, &pods.Items[i])
	}

	return owned, nil
}

// modelDeploymentObservationGuard is the uncached identity this collection is bounded by: the
// deployment's UID, generation and controlling owner, plus the member UIDs it currently has.
type modelDeploymentObservationGuard struct {
	deploymentUID string
	generation    int64
	ownerUIDs     string
	memberUIDs    string
	// memberFacts and routerFacts are the per-Pod identities the bindings acted on. They are
	// separate strings rather than folded into memberUIDs so a failure names which of the two
	// changed, and memberUIDs keeps answering the plain membership question.
	memberFacts string
	routerFacts string
	// read records which of the facts above this snapshot actually read.
	read modelDeploymentGuardRead
}

// modelDeploymentGuardRead records which parts of a guard snapshot were read.
//
// A SNAPSHOT THAT FAILED PART WAY THROUGH KNOWS SOME OF THEM. The facts it did read are facts, and
// a snapshot that proved the deployment changed must not lose that proof because the membership
// listing failed afterwards. The facts it never reached are unknown, and an unknown is not a value
// that can be compared: a field that was never read is not a change.
type modelDeploymentGuardRead struct {
	deployment bool
	members    bool
	routers    bool
}

// snapshotModelDeploymentObservationGuard reads that identity through the uncached reader.
//
// THE MEMBERS ARE PASSED IN when the caller has already read them, and listed here when it has
// not. THE OPENING GUARD TAKES THE HOISTED MEMBERSHIP READ, so the membership is listed once per
// collection and not twice; THE CLOSING GUARD LISTS THEM AGAIN, because its whole purpose is to
// see whether they moved.
func (r *ModelDeploymentReconciler) snapshotModelDeploymentObservationGuard(
	ctx context.Context, md *workercore.ModelDeployment, members []*corev1.Pod,
) (modelDeploymentObservationGuard, modelDeploymentGuardError) {
	guard := modelDeploymentObservationGuard{}

	live := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), live); err != nil {
		return guard, modelDeploymentGuardError{
			reason: fmt.Errorf(
				"the deployment could not be re-read uncached during observation: %w", err).Error(),
			identity: true,
		}
	}
	ownerUIDs := make([]string, 0, len(live.OwnerReferences))
	for _, owner := range live.OwnerReferences {
		ownerUIDs = append(ownerUIDs, string(owner.UID))
	}
	sort.Strings(ownerUIDs)

	// THE DEPLOYMENT'S OWN FACTS ARE KNOWN FROM HERE ON, and they stay known when a later read
	// fails. A snapshot that proved the deployment changed must not lose that proof because the
	// membership or the Router listing failed afterwards.
	guard.deploymentUID = string(live.UID)
	guard.generation = live.Generation
	guard.ownerUIDs = strings.Join(ownerUIDs, ",")
	guard.read.deployment = true

	owned := make([]corev1.Pod, 0, len(members))
	if members == nil {
		listed := new(corev1.PodList)
		if err := r.APIReader.List(ctx, listed,
			ctrlcli.InNamespace(md.Namespace),
			ctrlcli.MatchingLabels{
				modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance: md.Name,
			},
		); err != nil {
			return guard, modelDeploymentGuardError{
				reason: fmt.Errorf(
					"the membership could not be re-read uncached during observation: %w", err).Error(),
				members: true,
			}
		}
		for i := range listed.Items {
			if modelDeploymentOwns(&listed.Items[i], md) {
				owned = append(owned, listed.Items[i])
			}
		}
	} else {
		for i := range members {
			owned = append(owned, *members[i])
		}
	}
	// EVERY FACT A BINDING ACTED ON IS IN THE BOOKEND, not only the membership. A member that kept
	// its UID while its controlling owner, its role, its member rank or the listener it declares
	// changed is a different member wearing the same name, and a UID-only comparison would read it
	// as unchanged and confirm absence about it.
	//
	// The identity labels above are a prefilter, so the controller reference is confirmed here the
	// same way listFreshModelDeploymentPods confirms it. A Pod that carries the labels but is not
	// owned by this deployment is not a fact any binding can act on, and keeping it out is what makes
	// the guard describe exactly the set the bindings read: otherwise one such Pod's churn refuses the
	// whole collection over a member this deployment does not have.
	memberUIDs := make([]string, 0, len(owned))
	memberFacts := make([]string, 0, len(owned))
	for i := range owned {
		memberUIDs = append(memberUIDs, string(owned[i].UID))
		memberFacts = append(memberFacts, modelDeploymentMemberFact(&owned[i]))
	}
	sort.Strings(memberUIDs)
	sort.Strings(memberFacts)

	// THE MEMBERSHIP'S FACTS ARE KNOWN FROM HERE ON, and they stay known when the Router listing
	// fails. A member that left under the collection is a change the collection already proved.
	guard.memberUIDs = strings.Join(memberUIDs, ",")
	guard.memberFacts = strings.Join(memberFacts, ";")
	guard.read.members = true

	// The Router processes are read under the same snapshot for the same reason: a process that
	// lost its address or changed owner is no longer the process this pass read.
	routers := &corev1.PodList{}
	if err := r.APIReader.List(ctx, routers,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
			modelDeploymentRouterLabelKey:   md.Spec.Router.Name,
		},
	); err != nil {
		return guard, modelDeploymentGuardError{
			reason: fmt.Errorf(
				"the routers could not be re-read uncached during observation: %w", err).Error(),
			routers: true,
		}
	}
	routerFacts := make([]string, 0, len(routers.Items))
	for i := range routers.Items {
		routerFacts = append(routerFacts, modelDeploymentMemberFact(&routers.Items[i]))
	}
	sort.Strings(routerFacts)

	guard.routerFacts = strings.Join(routerFacts, ";")
	guard.read.routers = true

	return guard, modelDeploymentGuardError{}
}

// identityMoved reports whether the deployment's own identity or the membership moved between two
// bookends.
//
// THE ROUTER PROCESSES ARE NOT PART OF IT. A collection whose routers were reparented or replaced
// describes a different set of processes, but the group's members did not move, and the retention
// is proved by the fresh membership rather than by the view.
func (g modelDeploymentObservationGuard) identityMoved(other modelDeploymentObservationGuard) bool {
	return g.deploymentMoved(other) || g.membersMoved(other)
}

// deploymentMoved reports whether the deployment's own identity moved between two snapshots.
//
// ONLY THE FACTS BOTH SNAPSHOTS READ. A snapshot that failed before the deployment was read knows
// nothing about it, and an unknown is not a change: it is the absence of one.
func (g modelDeploymentObservationGuard) deploymentMoved(other modelDeploymentObservationGuard) bool {
	if !g.read.deployment || !other.read.deployment {
		return false
	}

	return g.deploymentUID != other.deploymentUID ||
		g.generation != other.generation ||
		g.ownerUIDs != other.ownerUIDs
}

// membersMoved reports whether the membership moved between two snapshots.
//
// ONLY THE FACTS BOTH SNAPSHOTS READ, for the same reason as deploymentMoved. A closing snapshot
// that failed before it listed the members knows nothing about them, and a closing snapshot that
// read them knows them even if the Router listing failed afterwards.
func (g modelDeploymentObservationGuard) membersMoved(other modelDeploymentObservationGuard) bool {
	if !g.read.members || !other.read.members {
		return false
	}

	return g.memberUIDs != other.memberUIDs || g.memberFacts != other.memberFacts
}

// matchesCaller reports whether the first uncached read is still the object the caller passed in.
//
// A CACHED OBJECT IS A CLAIM ABOUT THE PAST, and this collection is defined against what the
// deployment is now. The caller's copy can be from a pass that started before a replacement, and
// comparing only the two fresh snapshots would accept it, because both reads would agree on the
// replacement and the disagreement is between the caller and both of them.
func (g modelDeploymentObservationGuard) matchesCaller(md *workercore.ModelDeployment) bool {
	return g.deploymentUID == string(md.UID) && g.generation == md.Generation
}

// modelDeploymentMemberFact is the identity a binding about this Pod would have read: its own UID,
// its controlling owner as an identity, the workload role and member rank it declares, the
// listeners it declares, whether it is running, and whether it is terminating. Two snapshots that
// carry the same fact describe the same member; two that differ describe a member that moved.
//
// THE READINESS IS PART OF THE FACT, because a member that stopped reporting ready is a member
// that moved: it is no longer a member the group may be served by, and a member the group's
// qualification admitted on an earlier readiness is not the member it admitted. The phase alone
// does not say it, because a member whose readiness probe fails stays Running.
//
// THE ELIGIBILITY LABEL AND THE GENERATION ANNOTATION ARE PART OF THE FACT, because the collection
// now consumes them: the retention rule reads them to decide whether a group is eligible and
// unchanged generation, and a fact that left them out would let a label or an annotation change
// under the collection without the collection noticing. The member index above is the ordinal, and
// it is here for the same reason.
func modelDeploymentMemberFact(pod *corev1.Pod) string {
	owner := "none"
	if ref := meta.GetControllerOf(pod); ref != nil {
		owner = ref.APIVersion + "/" + ref.Kind + "/" + ref.Name + "/" + string(ref.UID)
	}
	listeners := make([]string, 0, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		for _, declared := range pod.Spec.Containers[i].Ports {
			listeners = append(listeners, strconv.Itoa(int(declared.ContainerPort)))
		}
	}
	sort.Strings(listeners)
	terminating := pod.DeletionTimestamp != nil

	return strings.Join([]string{
		string(pod.UID),
		owner,
		pod.Status.PodIP,
		modelDeploymentPodRole(pod),
		pod.Labels[modelDeploymentMemberIndexLabel],
		strings.Join(listeners, "+"),
		string(pod.Status.Phase),
		strconv.FormatBool(terminating),
		strconv.FormatBool(podIsReady(pod)),
		pod.Labels[modelDeploymentLabelKeyEndpointEligible],
		pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
	}, "|")
}

// routerOwnerChain holds the OWNING objects one collection verified, and only that
// collection's.
//
// A Pod's controlling chain is immutable for the lifetime of that Pod, so every Router process
// seated in one ReplicaSet verifies the same two objects. Reading them once per collection
// instead of once per process turns a cost that grew with the replica count into a constant
// one, and it is safe to do so HERE and nowhere else: verify re-reads every one of them before
// the collection ends, so the answers were given against a chain that provably did not move
// while the collection was running.
//
// IT IS NOT A PROCESS-LIFETIME CACHE. It is built by the collection, used by it and discarded
// with it. A reparented owner, a replaced owner and a same-name replacement are all read afresh
// by the next collection, and a same-name replacement is refused inside this one.
type routerOwnerChain struct {
	reader      ctrlcli.Reader
	replicaSets map[ctrlcli.ObjectKey]*appsv1.ReplicaSet
	deployments map[ctrlcli.ObjectKey]*appsv1.Deployment
}

// newRouterOwnerChain opens the per-collection record of what the chain's objects are, read
// through the uncached reader because a cached answer is the snapshot this collection is
// defined against.
func newRouterOwnerChain(reader ctrlcli.Reader) routerOwnerChain {
	return routerOwnerChain{
		reader:      reader,
		replicaSets: map[ctrlcli.ObjectKey]*appsv1.ReplicaSet{},
		deployments: map[ctrlcli.ObjectKey]*appsv1.Deployment{},
	}
}

// replicaSet reads one ReplicaSet through the uncached reader, once per collection. Every
// process asking for the same ReplicaSet gets the object this collection verified.
func (c routerOwnerChain) replicaSet(
	ctx context.Context, key ctrlcli.ObjectKey,
) (*appsv1.ReplicaSet, error) {
	if verified, seen := c.replicaSets[key]; seen {
		return verified, nil
	}
	replicaSet := new(appsv1.ReplicaSet)
	if err := c.reader.Get(ctx, key, replicaSet); err != nil {
		return nil, err
	}
	c.replicaSets[key] = replicaSet

	return replicaSet, nil
}

// deployment reads one Deployment through the uncached reader, once per collection, under the
// same rule as replicaSet.
func (c routerOwnerChain) deployment(
	ctx context.Context, key ctrlcli.ObjectKey,
) (*appsv1.Deployment, error) {
	if verified, seen := c.deployments[key]; seen {
		return verified, nil
	}
	deployment := new(appsv1.Deployment)
	if err := c.reader.Get(ctx, key, deployment); err != nil {
		return nil, err
	}
	c.deployments[key] = deployment

	return deployment, nil
}

// verify re-reads every object this collection judged a chain by, and reports the first one that
// moved underneath it. A ReplicaSet or Deployment replaced, recreated or reparented while the
// collection was running describes a chain the processes behind it were not verified against,
// and confirming an observation from it would be confirming about an owner nobody re-checked.
//
// THE COST IS CONSTANT IN THE PROCESS COUNT: two reads to share the chain and two to prove it
// did not move, whatever the number of Router processes, where each process verified its own
// chain and spent two reads on the owners.
func (c routerOwnerChain) verify(ctx context.Context) error {
	for key, verified := range c.replicaSets {
		fresh := new(appsv1.ReplicaSet)
		if err := c.reader.Get(ctx, key, fresh); err != nil {
			return fmt.Errorf("the router process's ReplicaSet %q could not be re-read uncached: %w",
				key.Name, err)
		}
		if fresh.UID != verified.UID {
			return fmt.Errorf("the router process's ReplicaSet %q was replaced while the routers "+
				"were being observed", key.Name)
		}
		// IDENTITY AND OWNER, as for the Deployment below: the two move independently, and a
		// ReplicaSet reparented to another ModelDeployment keeps its UID while the chain every
		// process was verified against names the old owner.
		if ownerIdentity(fresh) != ownerIdentity(verified) {
			return fmt.Errorf("the router process's ReplicaSet %q changed owner while the routers "+
				"were being observed", key.Name)
		}
	}
	for key, verified := range c.deployments {
		fresh := new(appsv1.Deployment)
		if err := c.reader.Get(ctx, key, fresh); err != nil {
			return fmt.Errorf("the router process's Deployment %q could not be re-read uncached: %w",
				key.Name, err)
		}
		// IDENTITY AND OWNER, because they move independently: a Deployment deleted and recreated
		// under the same name keeps its name and loses its UID, and one reparented to another
		// ModelDeployment keeps its UID and loses the owner this collection verified.
		if fresh.UID != verified.UID {
			return fmt.Errorf("the router process's Deployment %q was replaced while the routers "+
				"were being observed", key.Name)
		}
		if ownerIdentity(fresh) != ownerIdentity(verified) {
			return fmt.Errorf("the router process's Deployment %q changed owner while the routers "+
				"were being observed", key.Name)
		}
	}

	return nil
}

// ownerIdentity is one object's controlling owner at full identity, or the word none when it has
// no controller. Two reads carrying the same owner identity are the same owner.
func ownerIdentity(obj ctrlcli.Object) string {
	ref := meta.GetControllerOf(obj)
	if ref == nil {
		return "none"
	}

	return strings.Join([]string{
		ref.APIVersion, ref.Kind, ref.Name, string(ref.UID),
	}, "/")
}

// verifyRouterPodOwnership checks the live controlling-owner chain of one Router process: the Pod
// is controlled by a ReplicaSet that still exists with that identity, and that ReplicaSet is
// controlled by this deployment. The read goes through the uncached reader, because a cached
// answer is the snapshot this collection is defined against.
//
// THE POD IS READ FOR EVERY PROCESS, WHILE THE OWNERS ARE READ ONCE PER COLLECTION. That split
// is deliberate. The Pod is the object this collection is about, so a replaced one has to be
// seen here and now; the owners behind it are one shared chain whose identity each process then
// checks against its own Pod's claim, so one read answers every process asking.
func (r *ModelDeploymentReconciler) verifyRouterPodOwnership(
	ctx context.Context, md *workercore.ModelDeployment, pod *corev1.Pod, owners routerOwnerChain,
) error {
	// THE POD ITSELF IS RE-READ. The object handed in came from a listing, and a listing is a
	// snapshot; this check is the one that says the chain is live, so it may not be answered from
	// a copy of the Pod taken before the collection started. A Router that was adopted or
	// replaced since that listing is only visible here.
	live := new(corev1.Pod)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), live); err != nil {
		return fmt.Errorf("the router process could not be re-read uncached: %w", err)
	}
	if live.UID != pod.UID {
		return fmt.Errorf("the router process changed identity during observation")
	}

	ref := meta.GetControllerOf(live)
	if ref == nil {
		return fmt.Errorf("the router process has no controlling owner")
	}
	if ref.Kind != "ReplicaSet" || ref.APIVersion != appsv1.SchemeGroupVersion.String() {
		return fmt.Errorf("the router process is controlled by %s (%s) rather than a ReplicaSet",
			ref.Kind, ref.APIVersion)
	}
	replicaSet, err := owners.replicaSet(ctx,
		ctrlcli.ObjectKey{Namespace: live.Namespace, Name: ref.Name})
	if err != nil {
		return fmt.Errorf("the router process's ReplicaSet could not be read uncached: %w", err)
	}
	// THE IDENTITY CHECK IS NOT THE READ. The Pod's own owner reference is this process's claim
	// about its chain, and it is compared to the owner this collection verified for every
	// process, so a Pod pointing at a chain this collection never read is refused here.
	if replicaSet.UID != ref.UID {
		return fmt.Errorf("the router process's ReplicaSet changed identity during observation")
	}
	// The chain continues through the workload Deployment the ReplicaSet scales, and ends at
	// this deployment: kind, API version and identity at every hop.
	deploymentRef := meta.GetControllerOf(replicaSet)
	if deploymentRef == nil {
		return fmt.Errorf("the router process's ReplicaSet has no controlling Deployment")
	}
	if deploymentRef.Kind != "Deployment" ||
		deploymentRef.APIVersion != appsv1.SchemeGroupVersion.String() {
		return fmt.Errorf("the router process's ReplicaSet is controlled by %s (%s) rather than a Deployment",
			deploymentRef.Kind, deploymentRef.APIVersion)
	}
	deployment, err := owners.deployment(ctx,
		ctrlcli.ObjectKey{Namespace: live.Namespace, Name: deploymentRef.Name})
	if err != nil {
		return fmt.Errorf("the router process's Deployment could not be read uncached: %w", err)
	}
	if deployment.UID != deploymentRef.UID {
		return fmt.Errorf("the router process's Deployment changed identity during observation")
	}
	modelOwner := meta.GetControllerOf(deployment)
	if modelOwner == nil {
		return fmt.Errorf("the router process's Deployment has no controlling ModelDeployment")
	}
	if modelOwner.Kind != workercore.SchemeGroupVersionKind("ModelDeployment").Kind ||
		modelOwner.APIVersion != workercore.SchemeGroupVersion.String() || modelOwner.UID != md.UID {
		return fmt.Errorf("the router process's Deployment is owned by %s (%s) rather than this deployment",
			modelOwner.Kind, modelOwner.APIVersion)
	}

	return nil
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
		}
		// ONLY A CONFIRMED ANSWER CARRIES A NUMBER. Unknown, NotConverged and NotConfigured all
		// state that no count was established, and the API carries that as an absent value: a
		// zero written into a state that never measured anything is a measurement of zero, which
		// is the false zero this whole path exists to refuse.
		if answer.State != workercore.ModelDeploymentServingStateConfirmed {
			continue
		}
		// A ROLE WITH NO ENDPOINT IN THE UNION IS ZERO, not the deployment's total. The count is
		// read from the answer's own per-role map, and a role the map does not name has none, so
		// the field says what this role is serving rather than what the deployment is. The value
		// is per role and is a copy, so two roles never share one addressable count.
		roleValue := answer.ByRole[status.Roles[i].Name]
		status.Roles[i].Endpoints.Serving.Value = &roleValue
	}
}
