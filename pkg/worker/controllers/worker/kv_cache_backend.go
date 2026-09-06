package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// The phases a KVCacheBackend reports. They are derived from what the backend is observed to be
// doing, never from what was asked for: a leader Pod that is Running but whose process reports its
// service not ready is Provisioning, not Ready.
const (
	// KVCacheBackendPhaseProvisioning is reported while the backend is coming up and nothing has
	// been observed serving yet.
	KVCacheBackendPhaseProvisioning = "Provisioning"
	// KVCacheBackendPhaseReady is reported when the leader serves and at least one member is
	// mounted.
	KVCacheBackendPhaseReady = "Ready"
	// KVCacheBackendPhaseDegraded is reported when the leader serves but a member group is short
	// of the nodes its selector matches.
	KVCacheBackendPhaseDegraded = "Degraded"
	// KVCacheBackendPhaseError is reported when the leader cannot be reached or cannot be
	// scheduled.
	KVCacheBackendPhaseError = "Error"
	// KVCacheBackendPhaseDeleting is reported while the object is being torn down, including
	// while that teardown is refused.
	KVCacheBackendPhaseDeleting = "Deleting"
)

// The condition types a KVCacheBackend reports, one per axis. They are kubeapistatus.ConditionType
// so the package's accessors read and write them; those accessors are reflective and work on this
// group's own condition type as well as on an upstream one.
//
// Once more than one of these is observed, deriving Phase from them belongs to a
// kubeapistatus.StatusSummarizer rather than to a switch — that is what the package's walker is
// for, and it is how the ClusterQueue view already does it.
const (
	KVCacheBackendConditionLeaderAvailable  kubeapistatus.ConditionType = "LeaderAvailable"
	KVCacheBackendConditionMembersMounted   kubeapistatus.ConditionType = "MembersMounted"
	KVCacheBackendConditionCapacityObserved kubeapistatus.ConditionType = "CapacityObserved"
	// KVCacheBackendConditionDeletable is False exactly while status.usedBy names a consumer.
	// It is the condition the finalizer's refusal is explained by.
	KVCacheBackendConditionDeletable kubeapistatus.ConditionType = "Deletable"
)

// KVCacheBackendReconciler reconciles a KVCacheBackend.
//
// It owns the object's lifecycle and, from later tasks on, the workloads that serve it:
//
//   - A live object is locked with the repository's own finalizer, so nothing it renders is
//     orphaned by a delete that races the reconcile.
//   - On delete the finalizer is held while status.usedBy names a consumer. The refusal is not a
//     requeue: usedBy is written by the consumer's own controller onto this object's status, and
//     that write is the event that brings the teardown back. A timer would only re-ask a question
//     whose answer has not changed.
//   - Status is written wholesale under a DeepEqual guard, so a settled backend produces no write
//     and therefore no event of its own.
type KVCacheBackendReconciler struct {
	Client ctrlcli.Client

	// AdminHTTP reads the leader's admin surface. It carries its own timeout, because a leader that
	// accepts a connection and then stalls would otherwise hold this reconcile open for as long as
	// the transport allows.
	//
	// The timeout bounds one stall; it does not isolate the others from it. That is what
	// kvCacheBackendConcurrency is for, and the two are only useful together.
	AdminHTTP *http.Client
}

// adminReadTimeout bounds one read of the leader's admin surface. It is short: everything read there
// is served out of memory by a process in the same cluster, so a read that takes longer is a leader
// in trouble, and reporting that quickly is more useful than waiting for it.
const adminReadTimeout = 5 * time.Second

// newAdminHTTPClient builds the client every admin read goes through. One client for the
// controller's whole life, so its connection pool is reused across reconciles: building one per pass
// would reconnect to the same leader every time.
//
// Redirects are refused rather than followed. For an external backend the address is the spec
// author's, and the default client would follow a 302 from it to any host this operator can reach —
// reading it with the operator's network identity, and then copying an excerpt of the answer into a
// status that everyone who can read the object can read. The three reads are fixed paths on an
// address admission has already validated, so a redirect away from it is never something to honor;
// the response that carried it is what gets reported.
func newAdminHTTPClient() *http.Client {
	return &http.Client{
		Timeout: adminReadTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// adminRead gives ONE read of the leader's admin surface its own deadline.
//
// It exists because the three reads a pass makes are sequential, and one deadline around all of
// them is not the bound adminReadTimeout describes: a slow /health would leave the capacity scrape
// and the segment listing an almost-expired context, and both would be reported as failures of
// their own — a leader that is merely slow would read as a leader whose gauges and listing are
// broken.
func adminRead[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	readCtx, cancel := context.WithTimeout(ctx, adminReadTimeout)
	defer cancel()

	return read(readCtx)
}

// kvCacheBackendObserveInterval is how often a settled backend is read again.
//
// Everything status reports about a RUNNING store — the capacity gauges, the segment listing,
// whether an address still answers — is read over HTTP and produces no Kubernetes event. The watches
// wake this reconciler when a workload changes, which covers a leader restarting or a node joining a
// group, and covers nothing at all about a store whose Pods are steady while its contents move. An
// external backend has no workload of ours to watch in the first place, so without this its status
// would stay whatever the first pass happened to see.
//
// The interval is the one InstanceType re-summarizes on: soon enough that a capacity change shows up
// in seconds, rare enough that four reads a minute is not a load on a leader.
const kvCacheBackendObserveInterval = 15 * time.Second

// kvCacheBackendTeardownInterval is how often a backend waiting for its workloads to go looks again.
//
// The workload watch does not make this redundant: it dedups by request, and all three rendered
// kinds map to this one backend — so the deletion event for the last of them can be swallowed by
// the one before it, leaving nothing to wake the object again.
const kvCacheBackendTeardownInterval = 2 * time.Second

// kvCacheBackendMaxMembers caps how many segments status.members will carry.
//
// Every entry is republished on every pass, and the api server refuses an object past roughly 1.5
// MiB — so a listing beyond this would make every status write fail and retry forever while the
// observation that produced it reported success. Set above one member per node on the largest
// cluster Kubernetes supports, so reaching it means the listing is not what this API models.
const kvCacheBackendMaxMembers = 5000

// kvCacheBackendMaxMembersBytes bounds the identifiers that listing may carry, in total.
//
// The entry COUNT alone is not a bound. Every string in a segment — its name, its state, its
// protocol, its endpoint — is chosen by the admin endpoint rather than by this operator, and for an
// external backend that endpoint is somebody else's. A handful of entries carrying long identifiers
// outweighs thousands of ordinary ones, so a count-only guard admits a listing that cannot be
// written and then fails on every retry, including the retry that would have reported it.
//
// Well under the ~1.5 MiB an object may occupy: status also carries conditions, endpoints and
// capacity, and the object carries its spec and its managed-fields history beside all of that.
const kvCacheBackendMaxMembersBytes = 512 << 10

// segmentListingSize measures what a listing would contribute to status, without building it.
// Identifiers only: the per-entry JSON overhead is fixed and the count guard already bounds it.
func segmentListingSize(segments []mooncake.SegmentDetail) int {
	var n int
	for i := range segments {
		n += encodedStringSize(segments[i].Name) + encodedStringSize(segments[i].State) +
			encodedStringSize(segments[i].Protocol) + encodedStringSize(segments[i].TEEndpoint)
	}
	return n
}

// encodedStringSize is how many bytes a string occupies on the wire, not in memory.
//
// The two differ by up to six times and the difference is the far end's to choose: JSON escapes a
// control character, and `<`, `>` and `&`, as `\uXXXX` — six bytes for one. Measuring before that
// happens under-counts an identifier that came off an external endpoint by exactly the factor an
// adversary would pick, so a listing admitted as half a megabyte arrives as three.
//
// Marshaled rather than counted against a rule written here, because the rule is the encoder's:
// restating it is a second implementation that can drift from the one that will actually run.
func encodedStringSize(s string) int {
	encoded, err := json.Marshal(s)
	if err != nil {
		// Unreachable for a string, and a zero here would waive the bound for the one input that
		// reached it. The worst case the encoder can produce stands in instead.
		return len(s) * 6
	}
	return len(encoded)
}

// A condition message is capped at 32768 characters by the generated schema, and the API server
// rejects the WHOLE status write when one exceeds it — which would freeze status on exactly the pass
// that had a fault to report. Two shapes here can grow past that, and neither is this operator's to
// keep short.
//
// The first is a list of names, which grows with the cluster: a leader that lists no segment for the
// pods it should have accounts every one of them as short, so a few hundred nodes is enough, and a
// backend can be claimed by as many consumers as somebody creates. The counts around such a list
// already carry the magnitude, so a sample loses nothing a reader needs — and a message naming
// several hundred objects was never readable anyway.
//
// The second is a single string somebody else wrote: a kubelet's or scheduler's message, or a field
// off an HTTP body that an external backend served. Those are the actionable half of a fault and are
// passed through for that reason, but their length answers to the cluster or to the far end. One of
// them alone clears the limit — an admin response is bounded at 8 MiB, and nothing says how that is
// distributed across its fields.
const (
	kvCacheBackendMaxNames       = 20
	kvCacheBackendMaxDetailRunes = 2 << 10

	// kvCacheBackendMaxAmbiguityBytes bounds the ambiguity clause list, and it is DERIVED FROM THE
	// SCHEMA LIMIT rather than reused from the name cap above.
	//
	// Condition.message is capped at 32 KiB. Bounding the list by a COUNT cannot honor that cap,
	// because a clause has no bounded width: it carries a key and a nested name list, and both are
	// object names of up to 253 characters, so twenty clauses is anywhere from a few hundred bytes
	// to well past the limit. Counting bytes is the only bound that answers to the thing being
	// limited.
	//
	// 2 KiB of the 32 is held back for the sentence that frames this list and for the "and N more"
	// suffix, both of which are written outside the budget.
	//
	// One clause always fits, so the list can never be truncated to nothing: listBoundedNames caps
	// it at kvCacheBackendMaxNames names, which puts the widest possible clause near 5.4 KiB. That
	// relationship is asserted by a test rather than left to hold by luck, since raising the name
	// cap is what would break it.
	kvCacheBackendMaxAmbiguityBytes = 30 << 10
)

// listBoundedNames names a bounded sample and says how many it left out.
func listBoundedNames(names []string) string {
	if len(names) <= kvCacheBackendMaxNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(names[:kvCacheBackendMaxNames], ", "),
		len(names)-kvCacheBackendMaxNames)
}

// clipFaultDetail bounds a string this operator did not write, on its way into a condition message.
//
// Runes rather than display cells, which is what makes stringx.Truncate the wrong tool: a zero-width
// or combining character measures no cells, so a string of them is returned whole however long it
// is — and the limit being satisfied here counts characters.
func clipFaultDetail(detail string) string {
	return stringx.TruncateRunes(detail, kvCacheBackendMaxDetailRunes, "… (truncated)")
}

var _ ctrlreconcile.Reconciler = (*KVCacheBackendReconciler)(nil)

func (r *KVCacheBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	kvcb := new(workercore.KVCacheBackend)
	err := r.Client.Get(ctx, req.NamespacedName, kvcb, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch kv cache backend")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Deleting: run the finalizer teardown.
	if kvcb.DeletionTimestamp != nil {
		return r.teardownKVCacheBackend(ctx, kvcb)
	}

	// Lock.
	if !systemmeta.Lock(kvcb) {
		if err = r.Client.Update(ctx, kvcb); err != nil {
			logger.Error(err, "lock kv cache backend")
			return ctrl.Result{}, err
		}
	}

	// The condition accessors work on an object with a Status field, so the status is built inside
	// one. It carries the real ObjectMeta because a condition write reads the object it is given —
	// and it is a COPY of the observed status, so the caller's DeepEqual guard still compares two
	// independent values.
	holder := &workercore.KVCacheBackend{
		ObjectMeta: *kvcb.ObjectMeta.DeepCopy(),
		Status:     r.computeStatus(kvcb, r.liveKVCacheBackendConsumers(ctx, kvcb)),
	}
	// That status is a copy of the OBSERVED one, so it arrives carrying the addresses the previous
	// pass published. Every branch below derives its own, and a branch that derives none has to leave
	// none: keeping the old ones would go on advertising a Service that has since been deleted, and
	// would tell the phase below that something was rendered once when nothing was.
	holder.Status.Endpoints = nil

	// Set when this pass could not render, while the objects from an earlier one keep running. It
	// reaches the phase rather than the conditions: the conditions describe the STORE, which is
	// unaffected, and this describes what this operator can still do for it.
	var renderBlocked error

	// The two connection branches differ in what is RENDERED and agree on what is observed. Each
	// ends with the backend's addresses in status, and everything after that is read through them —
	// which is what lets one observation path serve a leader this operator started and one it was
	// merely pointed at.
	switch {
	case kvcb.Spec.Connection.Managed != nil:
		// Resolved once and handed to both sides: the leader and every member group run the same
		// backend-wide image unless a group names its own, and two resolutions could disagree if
		// the setting changed between them.
		image, imageErr := resolveKVCacheBackendImage(ctx, kvcb)
		if imageErr != nil {
			// NOT a return, because the workloads this backend already created are still running
			// and still serving. The image comes partly from an EDITABLE setting, so an admin
			// clearing it must not stop this operator from looking at a live store — that froze
			// status on a stale reading, with an exponential backoff behind it and nothing on the
			// object saying why. Rendering is skipped, since there is no image to render with, and
			// the phase below reports that a spec change would no longer be applied.
			logger.Error(imageErr, "resolve kv cache backend image")

			// The addresses are DERIVED from this object's name, so publishing them here would name
			// a Service that may never have been created — and then every read against it fails for
			// a reason that reads like a leader still coming up. Only a pass that rendered can have
			// created it, so ask instead of deriving.
			//
			// Existing is not enough: the name is derived, so an unrelated Service can already hold
			// it — and this branch runs precisely when nothing has been rendered to overwrite it.
			// Publishing then would point consumers at somebody else's workload and read this
			// backend's status out of it. The note is the same judge teardown uses, for the same
			// reason: a derived name is a query, not a proof.
			svc := new(core.Service)
			switch getErr := r.Client.Get(ctx, ctrlcli.ObjectKey{
				Name:      mooncake.LeaderObjectName(kvcb),
				Namespace: kuberess.SystemNamespaceName,
			}, svc); {
			case getErr == nil:
				if renderedForKVCacheBackend(svc, kvcb) {
					holder.Status.Endpoints = mooncake.LeaderEndpoints(kvcb)
				}
			case !kerrors.IsNotFound(getErr):
				return ctrl.Result{}, getErr
			}
		} else {
			if err = r.syncLeaderWorkload(ctx, kvcb, image); err != nil {
				return ctrl.Result{}, err
			}
			if err = r.syncMemberWorkloads(ctx, kvcb, image); err != nil {
				return ctrl.Result{}, err
			}
			// Published only after both objects converged, so an address in status always names
			// something that exists. Whether it yet ANSWERS is a different question, and the
			// conditions are where that one is reported.
			holder.Status.Endpoints = mooncake.LeaderEndpoints(kvcb)
		}
		renderBlocked = imageErr

	case kvcb.Spec.Connection.External != nil:
		// Nothing is created here, and that absence is the whole of the external mode: the
		// backend already runs, so there is no Deployment, no Service and no DaemonSet to converge,
		// and no image to resolve. The addresses are COPIED rather than derived — they name
		// somebody else's deployment, so this operator has nothing to compute them from and no
		// business changing them.
		holder.Status.Endpoints = slices.Clone(kvcb.Spec.Connection.External.Endpoints)

	default:
		// Admission refuses an object carrying neither branch, so reaching here means one got past
		// it. There is nothing to render and no address to read; the pass writes what it has rather
		// than dereferencing its way to a panic.
		return ctrl.Result{}, r.syncStatus(ctx, kvcb, holder.Status)
	}

	r.observeLeader(ctx, kvcb, holder)
	// Derived last, from the conditions the observation just wrote. A phase computed before them
	// would summarize the previous pass.
	deriveKVCacheBackendPhase(holder, renderBlocked)

	// Come back on a timer even when nothing failed. What was just read lives outside Kubernetes and
	// changes without telling it.
	return ctrl.Result{RequeueAfter: kvCacheBackendObserveInterval},
		r.syncStatus(ctx, kvcb, holder.Status)
}

// observeLeader reads everything this operator can observe about a running backend and folds it into
// the status being built: whether the leader serves, what capacity it reports, and which segments it
// lists.
//
// One pass, one read of each route. The three axes are derived together because they share a gate —
// service_ready — and reading /health twice could see two different answers within one reconcile,
// which would let the conditions contradict each other.
//
// The gate is service_ready and NOT the reads succeeding. /metrics and the listing are answered
// differently by a leader that is up but not serving: /metrics is ungated and hands back a
// well-formed exposition of zeroes, while the listing is gated and answers 503. Only the first is
// dangerous, because a zero is indistinguishable at the parser from a genuinely empty cache — so the
// health document decides for both, and neither read is treated as evidence on its own.
//
// It takes two objects because they answer different questions: kvcb is the observed object and is
// where the spec is read from, holder is the status under construction and is what the condition
// accessors write into.
func (r *KVCacheBackendReconciler) observeLeader(
	ctx context.Context, kvcb, holder *workercore.KVCacheBackend,
) {
	logger := ctrllog.FromContext(ctx)

	// Cleared to ABSENT up front, so every path below either publishes an observation or leaves no
	// field at all. Carrying the previous pass's figures forward would republish them as current,
	// and an empty struct here would serialize as "capacity": {} — a shape the API contract does
	// not have.
	holder.Status.Capacity = nil

	client := r.adminClientFor(holder.Status.Endpoints)
	if client == nil {
		reason, message := "NoAdminEndpoint", "the backend publishes no admin endpoint to read"
		KVCacheBackendConditionLeaderAvailable.False(holder, reason, message)
		KVCacheBackendConditionCapacityObserved.False(holder, reason, message)
		KVCacheBackendConditionMembersMounted.False(holder, reason, message)
		return
	}

	health, err := adminRead(ctx, client.Health)
	if err != nil {
		logger.V(2).Info("kv cache backend leader health unreadable", "error", err.Error())

		// Not being able to read a leader means two different things, and the difference is
		// whether its Pod is ready. A leader still pulling its image, or up but not yet serving,
		// is UNREACHABLE AND FINE — its readiness probe asks a gated route, so it is not ready
		// either, and reporting an error would make every install look broken for its first
		// minute. A leader whose Pod IS ready and still cannot be read is something else.
		//
		// This is a level-based reading of the same fact F6 describes as a failure threshold: the
		// Deployment has already applied the threshold, through the probe, and its readyReplicas
		// is the answer. Counting failures here would re-derive it edge-triggered and keep state
		// nothing else needs.
		reason := "LeaderStarting"
		message := fmt.Sprintf("the leader is not serving yet and cannot be read: %v", err)
		switch {
		case kvcb.Spec.Connection.Managed == nil:
			// An external backend gets no such grace, and the reason is that the excuse above
			// does not exist for it. Its address names something an operator has declared already
			// runs; nothing here is mid-creation, and there is no Pod of ours whose readiness could
			// say "not yet". Reporting a mistyped or firewalled endpoint as Provisioning would
			// leave it waiting forever for a start that is never going to come.
			reason = "LeaderUnreachable"
			message = fmt.Sprintf("the external backend at %s could not be read: %v",
				client.Address, err)
		case r.leaderPodIsReady(ctx, kvcb):
			reason = "LeaderUnreachable"
			message = fmt.Sprintf("the leader's pod is ready but its health could not be read: %v", err)
		case r.leaderPodIsUnschedulable(ctx, kvcb):
			reason = "LeaderUnschedulable"
			message = "the leader's pod cannot be scheduled onto any node, so it will not start " +
				"until the cluster or this object changes"
		default:
			// The other fault waiting does not resolve: the Pod was placed and its container will
			// not run — an image that cannot be pulled, or a process exiting as fast as it starts.
			// It is neither ready nor unschedulable, so without this it reads as a leader that is
			// still starting, forever.
			if podReason, podMessage, failed := r.leaderPodContainerFailure(ctx, kvcb); failed {
				reason, message = podReason, podMessage
			}
		}
		KVCacheBackendConditionLeaderAvailable.False(holder, reason, message)
		KVCacheBackendConditionCapacityObserved.False(holder, reason, message)
		// Membership keeps its last value: an empty list would read as "every member is gone",
		// which is a claim this pass cannot make.
		KVCacheBackendConditionMembersMounted.False(holder, reason, message)
		return
	}

	if !health.ServiceReady {
		// Not an error. The leader is answering and saying it is not serving yet, which is a phase.
		// Nothing it would report now means anything: its gauges read zero and its listing 503s.
		reason := "ServicePlaneNotActive"
		message := fmt.Sprintf(
			"the leader is up in role %q and reports ha_state %q, but its service plane is not active",
			clipFaultDetail(health.Role), clipFaultDetail(health.HAState))
		KVCacheBackendConditionLeaderAvailable.False(holder, reason, message)
		KVCacheBackendConditionCapacityObserved.False(holder, reason,
			"the leader is up but its service plane is not active, so it holds no capacity to report")
		KVCacheBackendConditionMembersMounted.False(holder, reason,
			"the leader is up but its service plane is not active, so it lists no segment")
		return
	}

	KVCacheBackendConditionLeaderAvailable.True(holder, "Serving",
		fmt.Sprintf("the leader serves in role %q", clipFaultDetail(health.Role)))

	r.observeCapacity(ctx, kvcb, holder, client)
	r.observeMembers(ctx, kvcb, holder, client)
}

// leaderPodIsReady reports whether the leader's Deployment has an available replica.
//
// For a MANAGED backend, a read failure is reported as a fault only when this is true — an external
// one has no Deployment to ask and is judged by a different rule. It is deliberately the DEPLOYMENT's
// own view rather than a probe of our own: the readiness probe already asks the leader a gated
// route, so readyReplicas is the leader's readiness as Kubernetes settled it, and re-deciding it
// here would be a second opinion that could disagree.
func (r *KVCacheBackendReconciler) leaderPodIsReady(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) bool {
	deploy := new(apps.Deployment)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}, deploy)
	if err != nil {
		// No Deployment to ask, so nothing is ready. A backend whose workload has not been created
		// is starting, not broken.
		return false
	}
	return deploy.Status.ReadyReplicas > 0
}

// leaderPodIsUnschedulable reports whether the scheduler has refused to place the leader's Pod.
//
// It is the one leader fault waiting never resolves: no node in the cluster satisfies the Pod, so
// something outside this reconcile — a node, a taint, the object's own resources — has to change
// before it can ever run. Through readyReplicas alone that is indistinguishable from a Pod still
// pulling its image, and reporting it as Provisioning would leave an operator waiting for a start
// that is not coming, so it is asked separately and reported as a fault.
//
// The question goes to the Pods rather than to the Deployment because the Deployment says so only
// through ProgressDeadlineExceeded, ten minutes later; PodScheduled is written by the scheduler on
// its first attempt.
func (r *KVCacheBackendReconciler) leaderPodIsUnschedulable(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) bool {
	for _, pod := range r.leaderPods(ctx, kvcb) {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == core.PodScheduled &&
				cond.Status == core.ConditionFalse &&
				cond.Reason == core.PodReasonUnschedulable {
				return true
			}
		}
	}

	return false
}

// leaderPodContainerFailure reports why the leader's container is not running, when the reason is
// one that waiting will not resolve.
//
// It is the second fault readyReplicas cannot see, and it arrives by the same route as the first: a
// Pod that is scheduled and not ready is normally a Pod still starting, so the caller's default is
// to say so — but an image that cannot be pulled, or a process that exits as fast as it is started,
// is not starting. Without this the two look identical and a backend that will never come up reports
// Provisioning for as long as it exists.
//
// The kubelet's own reason and message are passed through rather than restated. They name the image
// that could not be pulled or the loader symbol that was missing, which is the actionable half, and
// they are also the only description of a failure this operator cannot otherwise see inside.
func (r *KVCacheBackendReconciler) leaderPodContainerFailure(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (reason, message string, failed bool) {
	for _, pod := range r.leaderPods(ctx, kvcb) {
		for _, status := range pod.Status.ContainerStatuses {
			// The last termination goes FIRST, and the order is the whole point. A crash-looping
			// container is also waiting, and its waiting message is the kubelet's own boilerplate
			// — "back-off 5m0s restarting failed container=..." — which says nothing about why it
			// died. The exit code and the process's last words are here.
			if t := status.LastTerminationState.Terminated; t != nil && status.RestartCount > 0 {
				return "LeaderCrashLooping", fmt.Sprintf(
					"the leader's container has restarted %d time(s); it last exited with code %d: %s",
					status.RestartCount, t.ExitCode, clipFaultDetail(t.Message)), true
			}
			// Waiting with a reason is the kubelet saying it has stopped trying for now, and a
			// container that never ran has nothing above to report. ContainerCreating and
			// PodInitializing are the states that ARE progress, and they are excluded by name so a
			// reason nobody here has seen counts as a fault rather than as silence.
			if w := status.State.Waiting; w != nil && isTerminalWaitReason(w.Reason) {
				return w.Reason, fmt.Sprintf("the leader's container is not running: %s: %s",
					w.Reason, clipFaultDetail(w.Message)), true
			}
		}
	}

	return "", "", false
}

// isTerminalWaitReason reports whether a waiting container is stuck rather than starting.
//
// The two excluded reasons are the kubelet's own progress states: a container being created, and a
// Pod whose init containers have not finished. Everything else it reports while waiting — every
// image pull failure, every backoff — is a state that persists until something outside this reconcile
// changes.
func isTerminalWaitReason(reason string) bool {
	switch reason {
	case "", "ContainerCreating", "PodInitializing":
		return false
	}
	return true
}

// leaderPods lists the Pods the leader's Deployment owns, through the Deployment's own selector.
//
// Empty on any failure, and deliberately so: every caller asks whether a specific fault is visible,
// and "the Pods could not be read" is not evidence of one. Saying nothing leaves each caller's
// gentler default in place.
func (r *KVCacheBackendReconciler) leaderPods(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) []core.Pod {
	deploy := new(apps.Deployment)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}, deploy)
	if err != nil || deploy.Spec.Selector == nil {
		return nil
	}

	pods := new(core.PodList)
	err = r.Client.List(ctx, pods,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels(deploy.Spec.Selector.MatchLabels))
	if err != nil {
		return nil
	}

	// Labels select; the controller reference is what PROVES it. Every caller of this turns what it
	// finds into an Error phase for the backend, so a stranger carrying these derived labels would
	// make this operator publish a fault against a leader that is healthy.
	//
	// Ownership is TWO hops here, unlike the member path's one: a Deployment does not own its Pods,
	// its ReplicaSets do. So the ReplicaSets are resolved first, and by name rather than by UID —
	// the name is derived from the backend, and a Deployment deleted and recreated keeps it.
	owned := r.leaderReplicaSetNames(ctx, deploy)
	if len(owned) == 0 {
		return nil
	}

	live := make([]core.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		ref := kubemeta.GetControllerOfNoCopy(pod)
		if ref == nil || ref.Kind != "ReplicaSet" || !owned[ref.Name] {
			continue
		}
		live = append(live, *pod)
	}
	return live
}

// leaderReplicaSetNames is the set of ReplicaSets the leader's Deployment controls, which is the
// hop between the Deployment and the Pods that carry its faults.
func (r *KVCacheBackendReconciler) leaderReplicaSetNames(
	ctx context.Context, deploy *apps.Deployment,
) map[string]bool {
	sets := new(apps.ReplicaSetList)
	err := r.Client.List(ctx, sets,
		ctrlcli.InNamespace(deploy.Namespace),
		ctrlcli.MatchingLabels(deploy.Spec.Selector.MatchLabels))
	if err != nil {
		return nil
	}

	names := make(map[string]bool, len(sets.Items))
	for i := range sets.Items {
		ref := kubemeta.GetControllerOfNoCopy(&sets.Items[i])
		if ref == nil || ref.Kind != "Deployment" || ref.Name != deploy.Name {
			continue
		}
		names[sets.Items[i].Name] = true
	}
	return names
}

// observeCapacity publishes what the leader's counters report, or nothing.
func (r *KVCacheBackendReconciler) observeCapacity(
	ctx context.Context,
	kvcb *workercore.KVCacheBackend,
	holder *workercore.KVCacheBackend,
	client *mooncake.AdminClient,
) {
	capacity, err := adminRead(ctx, client.Capacity)
	if err != nil {
		KVCacheBackendConditionCapacityObserved.False(holder, "ScrapeFailed",
			fmt.Sprintf("the leader's metrics could not be read: %v", err))
		return
	}

	total, used := selectKVCacheBackendCapacity(kvcb, capacity)
	if total == nil || used == nil {
		KVCacheBackendConditionCapacityObserved.False(holder, "FamilyMissing",
			"the leader's exposition does not carry the capacity families for this backend's medium")
		return
	}

	holder.Status.Capacity = &workercore.KVCacheBackendCapacity{
		Total: resource.NewQuantity(*total, resource.BinarySI),
		Used:  resource.NewQuantity(*used, resource.BinarySI),
	}
	KVCacheBackendConditionCapacityObserved.True(holder, "Observed",
		"capacity is read from the leader's own counters")
}

// observeMembers publishes the leader's segment listing as status.members[].
//
// The listing is authoritative: the leader is what allocation goes through, so a member Pod it does
// not list holds nothing, however healthy that Pod looks. Two fields the listing cannot supply —
// which node a member runs on and which medium it contributes — are joined in from the member Pod
// whose address matches, and are left EMPTY when no Pod matches rather than guessed at.
func (r *KVCacheBackendReconciler) observeMembers(
	ctx context.Context,
	kvcb *workercore.KVCacheBackend,
	holder *workercore.KVCacheBackend,
	client *mooncake.AdminClient,
) {
	logger := ctrllog.FromContext(ctx)

	segments, err := adminRead(ctx, client.Segments)
	if err != nil {
		// The previous listing is KEPT, which is the opposite of what a failed capacity scrape
		// does, and the difference is in the types. Capacity is two pointers, so it has an "absent"
		// meaning "not observed" and it uses it. A list has no such state: an empty list is a
		// legible value meaning "the leader lists no segment", so clearing it here would publish a
		// falsehood. The stale list plus a False condition says what actually happened.
		logger.V(2).Info("kv cache backend segment listing unreadable", "error", err.Error())
		KVCacheBackendConditionMembersMounted.False(holder, "ListingFailed",
			fmt.Sprintf("the leader's segment listing could not be read, so membership is as of "+
				"the last successful read: %v", err))
		return
	}

	if size := segmentListingSize(segments); len(segments) > kvCacheBackendMaxMembers ||
		size > kvCacheBackendMaxMembersBytes {
		// Withheld rather than truncated, and the previous listing is kept for the same reason a
		// failed read keeps it: a silently shortened list reads as a backend that lost members.
		KVCacheBackendConditionMembersMounted.False(holder, "ListingTooLarge",
			fmt.Sprintf("the leader lists %d segment(s) carrying %d byte(s) of identifiers, past "+
				"the %d entries or %d bytes this status can hold; publishing them would push the "+
				"object past the size the api server accepts, and every status write would fail "+
				"from then on — including this one",
				len(segments), size, kvCacheBackendMaxMembers, kvCacheBackendMaxMembersBytes))
		return
	}

	pods, ready, joinErr := r.listMemberPods(ctx, kvcb)
	if joinErr != nil {
		// The listing was read; only the join failed. Publishing the segments without their node
		// and medium is better than publishing nothing.
		logger.Error(joinErr, "list kv cache backend member pods")
	}

	// Keys that several ready Pods answer to AND that a segment actually arrived on. Both halves
	// matter. A segment on such a key cannot be traced to a Pod, so it is published without a node
	// or a medium rather than with a guessed one, and the condition below reports the ambiguity
	// instead of a number derived from it.
	//
	// Collected inside this loop rather than over the whole index, because a shared key that no
	// segment uses is not a problem: two TCP groups on one node share that node's name, and the
	// leader reports each of their segments by its own distinct pod IP, so every segment resolves.
	// Judging the index instead of the listing would move that healthy backend to Degraded and tell
	// the operator to split two groups that never collided.
	ambiguous := map[string][]string{}

	members := make([]workercore.KVCacheBackendMemberStatus, 0, len(segments))
	accounted := make(map[string]struct{}, len(segments))
	for _, segment := range segments {
		member := workercore.KVCacheBackendMemberStatus{
			SegmentName: segment.Name,
			Protocol:    segment.Protocol,
			State:       mooncake.SegmentState(segment.State),
		}
		host := hostOf(segment.TEEndpoint)
		switch candidates := readyMemberPods(pods[host]); {
		case len(candidates) > 1:
			ambiguous[host] = memberPodNames(candidates)
		case len(candidates) == 1:
			member.NodeName = candidates[0].nodeName
			member.Medium = candidates[0].medium
			accounted[candidates[0].podName] = struct{}{}
		case len(pods[host]) == 1:
			// Nothing ready answers to this key, and exactly one Pod does. A member on its way up or
			// down still supplies the two fields the listing cannot, which is worth more than a blank
			// row. It is NOT accounted for: the shortfall holds ready Pods to the listing, and this
			// one is not one of them.
			member.NodeName = pods[host][0].nodeName
			member.Medium = pods[host][0].medium
		}
		members = append(members, member)
	}

	// Sorted by the one field that identifies a segment, so two passes over one listing produce
	// byte-identical status and the DeepEqual guard holds. The leader's own order is not promised.
	slices.SortFunc(members, func(a, b workercore.KVCacheBackendMemberStatus) int {
		return strings.Compare(a.SegmentName, b.SegmentName)
	})
	holder.Status.Members = members

	// The shortfall is a SET DIFFERENCE, not a difference of counts. Comparing cardinalities lets a
	// stale segment stand in for a missing member: one ready Pod the leader does not list, plus one
	// listed segment whose Pod has already gone, is one against one — and a backend that has lost a
	// member reads Mounted. Named rather than counted, because the message is the whole reason this
	// condition is a message and not a number.
	var short []string
	for _, name := range ready {
		if _, ok := accounted[name]; !ok {
			short = append(short, name)
		}
	}

	// Read once, used by two of the branches below. A member that never became ready is in neither
	// the listing nor the ready count, so without this it is invisible to both.
	faults := r.memberPodFaults(ctx, kvcb)

	switch {
	case joinErr != nil:
		// Before the shortfall, because the shortfall is the thing that failed. With no Pods
		// listed there is nothing to hold the leader to, the difference is empty, and a non-empty
		// listing would fall through to Mounted — reporting every member as accounted for on the
		// strength of a comparison that never happened. The segments above are still published;
		// only the verdict is withheld.
		KVCacheBackendConditionMembersMounted.False(holder, "PodsUnreadable",
			fmt.Sprintf("the leader lists %d segment(s), but this operator's own member pods could "+
				"not be read, so none of them has been accounted for: %v", len(members), joinErr))
	case len(members) == 0:
		// The Pod count belongs in the message only where the Pods are this operator's own, because
		// there it is the actionable half of the answer: no segment AND no Pod is a placement
		// problem, no segment WITH Pods is a mount problem. An external backend's members belong to
		// somebody else, so a count of zero would describe this operator rather than the backend.
		reason, message := "NoSegments", "the leader lists no segment"
		if kvcb.Spec.Connection.Managed != nil {
			message = fmt.Sprintf("%s, with %d member pod(s) running", message, len(ready))
			// A member that never became ready is not in that count, and its own container is the
			// only place that says why. Without this the message reads "no segment, with 0 member
			// pod(s) running" — true, and useless.
			if len(faults) > 0 {
				reason, message = faults[0].reason, message+"; "+faults[0].detail
			}
		}
		KVCacheBackendConditionMembersMounted.False(holder, reason, message)
	case len(ambiguous) > 0:
		// Before the shortfall, because the shortfall cannot be computed: the Pods behind an
		// ambiguous key are unaccounted for by construction, so they would all be reported missing
		// and the number would describe this index rather than the cluster.
		//
		// The message names the key and the Pods, because the operator's next step is to make them
		// stop sharing it — a nodeSelector that keeps the two groups off one node — and that step
		// is only obvious if the message says which groups and which node.
		KVCacheBackendConditionMembersMounted.False(holder, "AmbiguousMemberIdentity",
			fmt.Sprintf("the leader lists %d segment(s), and %s, so no segment can be traced to a "+
				"member: the leader reports a segment by address and both of its fields carry a "+
				"transfer port bound at random, which no pod carries. Give the groups node "+
				"selectors that keep them on different nodes to make each member addressable",
				len(members), describeAmbiguousKeys(ambiguous)))
	case len(short) > 0:
		KVCacheBackendConditionMembersMounted.False(holder, "SegmentsShort",
			fmt.Sprintf("the leader lists %d segment(s), and %d of %d ready member pod(s) match "+
				"none of them (%s); a pod the leader does not list holds nothing",
				len(members), len(short), len(ready), listBoundedNames(short)))
	case len(faults) > 0:
		// Segments exist and every ready Pod is accounted for, so the shortfall above finds
		// nothing — and yet a selected node is holding no cache and never will. Reporting Ready
		// here would mean a group that has lost half its members looks identical to one that has
		// not, which is the case the phase exists to tell apart.
		KVCacheBackendConditionMembersMounted.False(holder, faults[0].reason,
			fmt.Sprintf("the leader lists %d segment(s), and %d selected member pod(s) will not "+
				"start; %s", len(members), len(faults), faults[0].detail))
	default:
		KVCacheBackendConditionMembersMounted.True(holder, "Mounted",
			fmt.Sprintf("the leader lists %d segment(s)", len(members)))
	}
}

// podIsReady reports the Pod's own readiness, which for a member is the store client having mounted
// its segment and not merely started: the readiness probe connects to a port the entrypoint serves
// only after the mount. Without that probe this would be true the moment the container ran, and
// every member would spend its startup counted as a shortfall.
func podIsReady(pod *core.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == core.PodReady {
			return cond.Status == core.ConditionTrue
		}
	}
	return false
}

// memberPodFault is a member Pod that has stopped making progress, and why.
type memberPodFault struct {
	reason string
	detail string
}

// memberPodFaults returns every member Pod that has stopped making progress — a container that will
// not start, or a Pod no node will take. It is what leaderPodContainerFailure and
// leaderPodIsUnschedulable together are for the leader.
//
// The line it draws is between a STARTING Pod and a STUCK one, and that line is the whole reason it
// exists. A Pod that is pulling or waiting to be scheduled is deliberately not a shortfall: counting
// it would hold a healthy backend at Degraded for the length of every rollout. But one whose
// container will not start is never going to arrive, and a backend reporting Ready while a selected
// node holds nothing is reporting a fiction that no amount of waiting resolves.
//
// Empty on a failed read: an unreadable Pod list is not evidence of a fault.
func (r *KVCacheBackendReconciler) memberPodFaults(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) []memberPodFault {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil {
		return nil
	}

	var faults []memberPodFault
	for group := range managed.Members {
		pods := new(core.PodList)
		err := r.Client.List(ctx, pods,
			ctrlcli.InNamespace(kuberess.SystemNamespaceName),
			ctrlcli.MatchingLabels(mooncake.MemberSelectorLabels(kvcb, group)))
		if err != nil {
			return nil
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil || podIsReady(pod) {
				continue
			}
			if !memberPodIsOurs(pod, mooncake.MemberObjectName(kvcb, group)) {
				continue
			}
			if fault, stuck := memberPodStuck(pod); stuck {
				faults = append(faults, fault)
			}
		}
	}

	return faults
}

// memberPodStuck reads whether a not-ready member Pod has stopped, and reports nothing for one that
// is still on its way.
func memberPodStuck(pod *core.Pod) (memberPodFault, bool) {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == core.PodScheduled && cond.Status == core.ConditionFalse &&
			cond.Reason == core.PodReasonUnschedulable {
			return memberPodFault{
				reason: core.PodReasonUnschedulable,
				detail: fmt.Sprintf("member pod %s has no node to run on: %s",
					pod.Name, clipFaultDetail(cond.Message)),
			}, true
		}
	}

	for _, status := range pod.Status.ContainerStatuses {
		// The last termination goes first: a crash-looping container is also waiting, and its
		// waiting message is the kubelet's own backoff boilerplate rather than a cause.
		if t := status.LastTerminationState.Terminated; t != nil && status.RestartCount > 0 {
			return memberPodFault{
				reason: "MemberCrashLooping",
				detail: fmt.Sprintf(
					"member pod %s has restarted %d time(s); it last exited with code %d: %s",
					pod.Name, status.RestartCount, t.ExitCode, clipFaultDetail(t.Message)),
			}, true
		}
		if w := status.State.Waiting; w != nil && isTerminalWaitReason(w.Reason) {
			return memberPodFault{
				reason: w.Reason,
				detail: fmt.Sprintf("member pod %s is not running: %s: %s",
					pod.Name, w.Reason, clipFaultDetail(w.Message)),
			}, true
		}
	}

	return memberPodFault{}, false
}

// memberPodFacts is what a member Pod contributes to a listing entry: the two fields the leader
// cannot report because they are Kubernetes facts and not store facts, plus the Pod's own name.
//
// The name is not published anywhere. It is the identity the shortfall check accounts against, and
// it has to be the Pod's rather than either index key, because one Pod is reachable under two of
// those and would otherwise be accounted for twice.
// Several Pods can answer to ONE key, which is why the index holds a slice of these rather than one:
// two member groups can select one node, and then both of their Pods answer to that node's name; on
// the RDMA path both also hold the host's network namespace, so both answer to its address too.
//
// When more than one READY Pod answers to the key a segment arrived on, the identity cannot be
// recovered, and that is a property of the data rather than of this code: the leader reports a
// segment as "<host>:<transfer port>", and BOTH of the fields it offers — segment_name and
// te_endpoint — are that shape. The transfer port is bound at random and is not a fact any Pod
// carries (four observed values, none configured: 15002, 15995, 16566, 16655), so the join must
// strip it. Two Pods behind one host are therefore indistinguishable in every observable field.
//
// So no assignment is attempted there. Three earlier versions of this tried — credit the surviving
// Pod, credit the whole set, credit by multiplicity — and each had a defect the next review found,
// because each was producing an approximation for a problem whose input does not determine its
// output. What is reported instead is that the identity is ambiguous.
type memberPodFacts struct {
	podName  string
	nodeName string
	medium   string
	// ready is carried here rather than looked up beside the join, because readiness is what decides
	// whether a Pod is a candidate for a segment at all. Kept out of it, a key shared by one ready
	// and one unready Pod resolves to whichever was filed last: the segment takes the unready Pod's
	// node and medium, and the ready member — the only one that could have produced it — is reported
	// missing.
	ready bool
}

// listMemberPods indexes this backend's member Pods by every name the leader could report one under,
// which is how a listing entry is joined back to the Pod that produced it.
//
// A Pod is indexed under BOTH its address and its node name, and the ADDRESS is the one that hits.
// The leader reports a segment's te_endpoint using whatever the member set as its local hostname,
// and this renderer sets that from the downward API's status.podIP — so on a rendered backend the
// listing carries "<pod IP>:<port>".
//
// The node-name key is what it used to carry. A member still running from a template rendered before
// that change reports "<node>:<port>", and it is joined rather than silently left blank — which is
// the failure this whole comment exists for: indexing on one key alone produced an empty node and
// medium on every member, and that reads exactly like the legitimate "no Pod matched" case.
// A second key costs one map write per Pod; the failure it guards against costs a diagnosis.
//
// An external backend's members are somebody else's Pods — this operator neither renders nor selects
// them — so the index is empty and every listing entry keeps the blank node and medium. That is the
// same rule a managed entry follows when no Pod matches it, reached by a different route.
// It returns the index and the NAMES of the ready Pods, not a count of them. One Pod occupies two
// keys, so neither the index's size nor a running total says which Pods the leader accounted for —
// and that is the question the shortfall has to answer.
func (r *KVCacheBackendReconciler) listMemberPods(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (map[string][]memberPodFacts, []string, error) {
	facts := map[string][]memberPodFacts{}
	var ready []string

	managed := kvcb.Spec.Connection.Managed
	if managed == nil {
		return facts, ready, nil
	}

	for group := range managed.Members {
		pods := new(core.PodList)
		err := r.Client.List(ctx, pods,
			ctrlcli.InNamespace(kuberess.SystemNamespaceName),
			ctrlcli.MatchingLabels(mooncake.MemberSelectorLabels(kvcb, group)))
		if err != nil {
			return facts, ready, err
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			// A stranger carrying these labels would otherwise be indexed by ITS node and address —
			// so a segment could be joined to the wrong node and medium — and counted as ready,
			// inventing a shortfall against a listing that was never going to name it.
			if !memberPodIsOurs(pod, mooncake.MemberObjectName(kvcb, group)) {
				continue
			}

			// Indexed whatever its state, but ACCOUNTED FOR only when ready. The index answers
			// "which Pod produced this segment", which a Pod answers as soon as it has a node —
			// and the accounting answers "which Pods should the leader be listing", which only a
			// ready member can be held to. Holding a Pod that is pulling, pending or crash-looping
			// to it would invent a shortfall and move a healthy backend to Degraded for the
			// duration of a rollout.
			fact := memberPodFacts{
				podName:  pod.Name,
				nodeName: pod.Spec.NodeName,
				medium:   managed.Members[group].Medium,
				ready:    podIsReady(pod),
			}
			if fact.ready {
				ready = append(ready, pod.Name)
			}

			// Indexed under BOTH, and a Pod carrying neither has mounted nothing so nothing can
			// match it. The pod IP is what a member this operator renders advertises, and the node
			// name is what one rendered before that did — an external backend, or a member still
			// running from an older template, is still joined rather than silently left blank.
			//
			// Written through indexMemberPod rather than assigned, because both keys can already be
			// held by another group's Pod, and both can be the same string for this one.
			if pod.Spec.NodeName != "" {
				indexMemberPod(facts, pod.Spec.NodeName, fact)
			}
			if pod.Status.PodIP != "" {
				indexMemberPod(facts, pod.Status.PodIP, fact)
			}
		}
	}

	// Sorted so a message built from a key's Pods is byte-identical across reconciles: the API
	// server's list order is not promised, and a condition message that reorders rewrites the object
	// on every pass.
	for key := range facts {
		slices.SortFunc(facts[key], func(a, b memberPodFacts) int {
			return strings.Compare(a.podName, b.podName)
		})
	}

	return facts, ready, nil
}

// readyMemberPods keeps the Pods behind one index key that could actually be holding a segment.
//
// Readiness is what decides an ambiguity, because only a ready member is held to the leader's
// listing: two Pods sharing a key while one is still pulling its image is a rollout, not an
// operator error, and the ready one is then the only candidate rather than one of two.
//
// The names are sorted by listMemberPods, so a message built from this is byte-identical across
// reconciles and does not churn the object.
func readyMemberPods(facts []memberPodFacts) []memberPodFacts {
	ready := make([]memberPodFacts, 0, len(facts))
	for _, fact := range facts {
		if fact.ready {
			ready = append(ready, fact)
		}
	}

	return ready
}

// memberPodNames is the names of a key's Pods, which is what a condition message can carry — the
// facts themselves say nothing an operator can act on.
func memberPodNames(facts []memberPodFacts) []string {
	names := make([]string, 0, len(facts))
	for _, fact := range facts {
		names = append(names, fact.podName)
	}

	return names
}

// describeAmbiguousKeys renders the sharing as one clause, sorted so the condition message is
// byte-identical across reconciles and does not churn the object.
// LIMITED, on both axes, and the reason is that this message can be the largest thing this
// controller writes. One ambiguous key per node is what a two-group RDMA backend produces, so the
// number of clauses answers to the cluster's size rather than to anything here — a thousand nodes
// renders past `Condition.message`'s 32 KiB schema limit, every status write is then rejected, and the
// reconcile retries forever WITHOUT ever publishing the ambiguity it was trying to report. A fault
// report that grows with the fault is the one shape that fails exactly when it is needed.
//
// The outer bound is therefore BYTES and not clauses. Clauses vary in width by more than an order of
// magnitude, so a count that is safe for short node names overruns the limit for long ones, and the
// overrun lands on the path that is already failing.
func describeAmbiguousKeys(ambiguous map[string][]string) string {
	clauses := make([]string, 0, len(ambiguous))
	for key, sharing := range ambiguous {
		clauses = append(clauses, fmt.Sprintf("%d ready member pod(s) answer to %q (%s)",
			len(sharing), key, listBoundedNames(sharing)))
	}
	slices.Sort(clauses)

	const separator = "; "

	kept, used := 0, 0
	for _, clause := range clauses {
		next := used + len(clause)
		if kept > 0 {
			next += len(separator)
		}
		if next > kvCacheBackendMaxAmbiguityBytes {
			break
		}
		used, kept = next, kept+1
	}

	if kept == len(clauses) {
		return strings.Join(clauses, separator)
	}

	return fmt.Sprintf("%s; and %d more shared key(s)",
		strings.Join(clauses[:kept], separator), len(clauses)-kept)
}

// indexMemberPod files one Pod under one key, KEEPING the Pods already there.
//
// A plain map assignment is what this replaces, and it dropped a Pod every time two groups met on a
// node: the dropped one was ready, unlisted, and reported as a shortfall forever. Holding every Pod
// per key is what lets the join ask "how many could have produced this segment" instead of reading
// whichever one a map happened to keep.
//
// A Pod filed under the SAME key twice is not two Pods. Both of its keys are the same string when
// the node is named by its address, which is legal and happens with --hostname-override and on some
// managed platforms. Appending it again would make one member read as two and manufacture an
// ambiguity out of a single Pod; returning early rather than overwriting also keeps a genuine
// collision recorded by an earlier call, which an overwrite would erase.
func indexMemberPod(facts map[string][]memberPodFacts, key string, fact memberPodFacts) {
	for _, filed := range facts[key] {
		if filed.podName == fact.podName {
			return
		}
	}
	facts[key] = append(facts[key], fact)
}

// hostOf strips the port from an address the leader reports. The listing carries host:port and the
// Pod carries a bare address, so one of the two has to give.
//
// Split by the standard parser rather than at the first colon, which an IPv6 literal has several of:
// "[2001:db8::1]:15002" cuts to "[2001" and matches nothing. That is a LIVE path, not a defensive
// one: a member advertises its pod IP, so on an IPv6 cluster every entry in the listing is a
// bracketed literal. The index this feeds fails silently — an empty node and medium on every member,
// indistinguishable from no Pod matching — so getting it wrong would be invisible.
func hostOf(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	// No port to strip, or a shape the parser does not accept. Either way the address is its own
	// host: a listing this operator cannot parse is still a listing entry, and the join simply
	// misses rather than matching something else.
	return address
}

// selectKVCacheBackendCapacity picks the gauges this backend's capacity is reported from.
//
// The leader keeps two independent pairs — one for segments it holds in memory, one for the disk a
// member offloads to — and it serializes BOTH unconditionally, so a pool a backend does not use is
// present in the exposition reading zero rather than being absent from it.
//
// The question is therefore what the backend is MADE of, and there are two answers:
//
//   - No disk tier: the memory pair alone. The file pair would be a zero, and while adding it would
//     give the same figure today, it would start meaning something the object does not have on the
//     day this operator renders a second file-backed thing.
//   - A disk tier, or an EXTERNAL backend: both pairs, added. A backend with a tier is genuinely
//     made of both, and an external one names nothing this operator can ask — it says how to REACH
//     a backend, not what it is built from — so summing is the only honest reading there too.
//
// This used to select by the group's MEDIUM, which stopped being the question when a disk tier
// became a layer on a group rather than a medium of its own: one group now contributes to both
// pairs at once, and no single medium name can say that.
func selectKVCacheBackendCapacity(
	kvcb *workercore.KVCacheBackend, capacity mooncake.LeaderCapacity,
) (total, used *int64) {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil {
		return sumCapacityGauges(capacity.TotalBytes, capacity.TotalFileBytes),
			sumCapacityGauges(capacity.AllocatedBytes, capacity.AllocatedFileBytes)
	}

	if len(managed.Members) == 0 {
		return nil, nil
	}

	if kvCacheBackendHasDiskTier(managed) {
		return sumCapacityGauges(capacity.TotalBytes, capacity.TotalFileBytes),
			sumCapacityGauges(capacity.AllocatedBytes, capacity.AllocatedFileBytes)
	}
	return capacity.TotalBytes, capacity.AllocatedBytes
}

// kvCacheBackendHasDiskTier reports whether any member group offloads to a local disk.
//
// Admission allows at most one group to carry a tier, and that bound exists for this function's
// sake: the leader reports every tier through one pair of gauges, so a second one would land in the
// same figure with no way to tell the two apart.
func kvCacheBackendHasDiskTier(managed *workercore.KVCacheBackendManaged) bool {
	for i := range managed.Members {
		if managed.Members[i].LocalDisk != nil {
			return true
		}
	}
	return false
}

// sumCapacityGauges adds two gauges each of which may be missing from the exposition, and stays
// absent only when BOTH are. An exposition carrying one family and not the other still reported the
// pool it has, and calling that "not observed" would discard a figure that was read.
func sumCapacityGauges(memory, file *int64) *int64 {
	switch {
	case memory == nil:
		return file
	case file == nil:
		return memory
	}

	// Saturated rather than wrapped. Each gauge is separately bounded to a non-negative int64 by
	// the decoder, but their sum need not fit one — and a wrapped sum is NEGATIVE, which reads as a
	// capacity and is published as one.
	//
	// This used to say only an external backend reaches it, which stopped being true when a managed
	// backend with a disk tier began summing both pairs as well. The saturation is still right for
	// both, and for the same reason in each: an external backend's /metrics is whatever address an
	// administrator wrote, and a managed backend's file gauge is whatever ceiling its members
	// declared — neither pair is this operator's to assume anything about.
	if *file > math.MaxInt64-*memory {
		sum := int64(math.MaxInt64)
		return &sum
	}

	sum := *memory + *file
	return &sum
}

// adminClientFor builds a reader for the admin endpoint this pass published, and returns nil when
// there is none to read. It uses the controller's own HTTP client, which SetupController installs;
// a caller that constructs this reconciler itself supplies one, which is also how a test keeps the
// suite off the network. The address comes from status rather than from a second derivation, so what
// is read is exactly what a consumer of this object would read.
func (r *KVCacheBackendReconciler) adminClientFor(
	endpoints []workercore.KVCacheBackendEndpoint,
) *mooncake.AdminClient {
	for _, endpoint := range endpoints {
		if endpoint.Name != workercore.KVCacheBackendEndpointNameAdmin || endpoint.Address == "" {
			continue
		}
		return &mooncake.AdminClient{Address: endpoint.Address, HTTP: r.AdminHTTP}
	}
	return nil
}

// syncLeaderWorkload converges the leader's Deployment and the Service in front of it.
func (r *KVCacheBackendReconciler) syncLeaderWorkload(
	ctx context.Context, kvcb *workercore.KVCacheBackend, image string,
) error {
	logger := ctrllog.FromContext(ctx)

	eDeploy := mooncake.RenderLeaderDeployment(kvcb, image)
	_, err := kubeclientset.CreateWithCtrlClient(ctx, r.Client, eDeploy,
		kubeclientset.WithUpdateIfExisted(alignLeaderDeploymentFn(kvcb, eDeploy)))
	if err != nil {
		logger.Error(err, "sync kv cache backend leader deployment")
		return err
	}

	eSvc := mooncake.RenderLeaderService(kvcb)
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eSvc,
		kubeclientset.WithUpdateIfExisted(alignLeaderServiceFn(kvcb, eSvc)))
	if err != nil {
		logger.Error(err, "sync kv cache backend leader service")
		return err
	}

	logger.V(2).Info("synced kv cache backend leader workload")
	return nil
}

// resolveKVCacheBackendImage picks the image the backend runs: the one named on the object, else the
// cluster-wide Setting.
//
// Admission refuses an object where both are empty, but a Setting can be blanked AFTER an object is
// admitted, and this runs on every reconcile. So the case is handled here as an error rather than
// assumed away: rendering an empty image would produce a Deployment the API server rejects, with a
// message pointing at the container instead of at the Setting somebody cleared.
func resolveKVCacheBackendImage(ctx context.Context, kvcb *workercore.KVCacheBackend) (string, error) {
	if image := strings.TrimSpace(kvcb.Spec.Image); image != "" {
		return image, nil
	}
	if image := strings.TrimSpace(settings.KVCacheBackendImage.ShouldValue(ctx)); image != "" {
		return image, nil
	}
	return "", fmt.Errorf("no image: neither spec.image nor the %q setting names one",
		settings.KVCacheBackendImage.Name())
}

// alignLeaderDeploymentFn converges a running leader Deployment onto the rendered one.
//
// It compares the fields this operator RENDERS rather than the whole object, and that is not an
// optimisation. The API server defaults a Pod template on write — terminationMessagePath,
// imagePullPolicy, dnsPolicy, schedulerName and a dozen more — so a DeepEqual against the rendered
// template differs on every single pass. The Deployment would be rewritten forever, and each rewrite
// rolls the leader.
//
// spec.selector is never touched: it is immutable, and an update carrying a different one is
// rejected outright, leaving the object stuck until somebody deletes it by hand.
func alignLeaderDeploymentFn(
	kvcb *workercore.KVCacheBackend, eDeploy *apps.Deployment,
) func(*apps.Deployment) (*apps.Deployment, bool, error) {
	return func(aDeploy *apps.Deployment) (*apps.Deployment, bool, error) {
		skip := true

		if !kubemeta.DeepEqual(aDeploy.Spec.Replicas, eDeploy.Spec.Replicas) {
			aDeploy.Spec.Replicas = eDeploy.Spec.Replicas
			skip = false
		}

		// The strategy is converged for the same reason the replica count is: both say there is
		// exactly one master. An edit back to RollingUpdate would surge a second one on the next
		// update, and the API server defaults that strategy's own fields on write — so only the
		// TYPE is compared, and the fields underneath it are cleared with it rather than left to
		// describe a strategy no longer in use.
		if aDeploy.Spec.Strategy.Type != eDeploy.Spec.Strategy.Type {
			aDeploy.Spec.Strategy = eDeploy.Spec.Strategy
			skip = false
		}

		// Converged even though the renderer only ever leaves it false, because that is exactly
		// what makes it worth converging: hostNetwork is a privilege the leader is designed not to
		// have, and the API server does not default it to anything this could be confused with. An
		// edit that turns it on would otherwise stand forever, since nothing else here would ever
		// look at it — a privilege granted by hand and kept by omission.
		if aDeploy.Spec.Template.Spec.HostNetwork != eDeploy.Spec.Template.Spec.HostNetwork {
			aDeploy.Spec.Template.Spec.HostNetwork = eDeploy.Spec.Template.Spec.HostNetwork
			skip = false
		}
		// The same shape, and it is what carries an existing workload to the safer state: a leader
		// rendered before this field was set holds a mounted service-account token until something
		// looks at it.
		if !kubemeta.DeepEqual(aDeploy.Spec.Template.Spec.AutomountServiceAccountToken,
			eDeploy.Spec.Template.Spec.AutomountServiceAccountToken) {
			aDeploy.Spec.Template.Spec.AutomountServiceAccountToken = eDeploy.Spec.Template.Spec.AutomountServiceAccountToken
			skip = false
		}

		// Credentials, so an edit that drops them has to be undone rather than tolerated. Unlike
		// the pull policy the API server defaults nothing here, so an empty expectation legibly
		// means "no secrets" and comparing it is safe.
		if !kubemeta.DeepEqual(
			aDeploy.Spec.Template.Spec.ImagePullSecrets, eDeploy.Spec.Template.Spec.ImagePullSecrets,
		) {
			aDeploy.Spec.Template.Spec.ImagePullSecrets = eDeploy.Spec.Template.Spec.ImagePullSecrets
			skip = false
		}

		// Converged because the container pass below moves VolumeMounts. A pass that carried a mount
		// across without the volume it names hands the API server a pod template it refuses outright
		// — `volumeMounts[0].name: Not found` — and goes on refusing it every pass afterwards. What
		// makes that worse than a loud failure is how it presents: the Deployment stays at the
		// generation it had, so this object keeps reporting Ready and its conditions stay True while
		// the master runs on without the flag the edit turned on. Multi-tenancy is exactly such an
		// edit, and it is one the pool webhook TELLS an operator to make.
		//
		// The whole slice is compared, which is what also takes the volumes away again when the
		// switch goes back off. That is only safe because the renderer spells out the fields the API
		// server would otherwise default — see quotaPolicyVolumes.
		if !kubemeta.DeepEqual(aDeploy.Spec.Template.Spec.Volumes, eDeploy.Spec.Template.Spec.Volumes) {
			aDeploy.Spec.Template.Spec.Volumes = eDeploy.Spec.Template.Spec.Volumes
			skip = false
		}

		// The init containers go field by field, through the same function the containers use, for
		// the same reason it exists: the server defaults fields inside a container, so comparing one
		// whole would make every pass a write.
		if len(aDeploy.Spec.Template.Spec.InitContainers) != len(eDeploy.Spec.Template.Spec.InitContainers) {
			aDeploy.Spec.Template.Spec.InitContainers = eDeploy.Spec.Template.Spec.InitContainers
			skip = false
		} else {
			for i := range eDeploy.Spec.Template.Spec.InitContainers {
				if alignRenderedContainer(
					&aDeploy.Spec.Template.Spec.InitContainers[i],
					eDeploy.Spec.Template.Spec.InitContainers[i],
				) {
					skip = false
				}
			}
		}

		if len(aDeploy.Spec.Template.Spec.Containers) != len(eDeploy.Spec.Template.Spec.Containers) {
			aDeploy.Spec.Template.Spec.Containers = eDeploy.Spec.Template.Spec.Containers
			skip = false
		} else {
			for i := range eDeploy.Spec.Template.Spec.Containers {
				if alignRenderedContainer(
					&aDeploy.Spec.Template.Spec.Containers[i],
					eDeploy.Spec.Template.Spec.Containers[i],
				) {
					skip = false
				}
			}
		}

		if !kubemeta.DeepEqual(aDeploy.Spec.Template.Labels, eDeploy.Spec.Template.Labels) {
			aDeploy.Spec.Template.Labels = eDeploy.Spec.Template.Labels
			skip = false
		}

		if restoreRenderedLabels(aDeploy, eDeploy) {
			skip = false
		}

		if !systemmeta.EqualResourceTypeAndNotes(eDeploy, aDeploy) {
			systemmeta.SyncResourceTypeAndNotes(eDeploy, aDeploy)
			skip = false
		}

		if !kubemeta.IsControlledBy(aDeploy, kvcb) {
			kubemeta.ControlOnWithoutBlock(aDeploy, kvcb,
				workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))
			skip = false
		}

		return aDeploy, skip, nil
	}
}

// alignRenderedContainer copies the rendered fields onto a running container and reports whether
// anything moved. The fields are named one by one because the ones NOT named here are the server's
// to default; overwriting them is what would make every pass a write.
//
// One function serves the leader and the members. A field only one of them uses compares equal on
// the other, and keeping the list in one place is what stops a field added to a renderer from being
// rendered but never converged.
func alignRenderedContainer(actual *core.Container, expected core.Container) (changed bool) {
	if actual.Image != expected.Image {
		actual.Image = expected.Image
		changed = true
	}
	// Unconditional, because the renderer resolves the policy rather than leaving it empty. An
	// empty expectation was the problem: the API server fills the field in on write, so comparing
	// against its default rolls the workload forever, and skipping the comparison to avoid that
	// left a stale policy standing — a value that was removed from the spec, or one derived from an
	// image tag the spec has since moved off. kvcache.EffectivePullPolicy owns that rule now.
	if actual.ImagePullPolicy != expected.ImagePullPolicy {
		actual.ImagePullPolicy = expected.ImagePullPolicy
		changed = true
	}
	// Safe to compare, unlike the pull policy: the renderer names a non-empty value, and the API
	// server only defaults an empty one.
	if actual.TerminationMessagePolicy != expected.TerminationMessagePolicy {
		actual.TerminationMessagePolicy = expected.TerminationMessagePolicy
		changed = true
	}
	if !kubemeta.DeepEqual(actual.Command, expected.Command) {
		actual.Command = expected.Command
		changed = true
	}
	if !kubemeta.DeepEqual(actual.Args, expected.Args) {
		actual.Args = expected.Args
		changed = true
	}
	if !kubemeta.DeepEqual(actual.Env, expected.Env) {
		actual.Env = expected.Env
		changed = true
	}
	if !kubemeta.DeepEqual(actual.Ports, expected.Ports) {
		actual.Ports = expected.Ports
		changed = true
	}
	if !kubemeta.DeepEqual(actual.ReadinessProbe, expected.ReadinessProbe) {
		actual.ReadinessProbe = expected.ReadinessProbe
		changed = true
	}
	if !kubemeta.DeepEqual(actual.LivenessProbe, expected.LivenessProbe) {
		actual.LivenessProbe = expected.LivenessProbe
		changed = true
	}
	// The whole value, not only Requests. The renderers set no limits at all, so a limit is a field
	// they leave at its zero value — and comparing only what a renderer fills in is exactly how an
	// injected one survives: a low memory limit OOM-kills the container on a loop, and nothing in
	// this object says why. The backend's own claim is a REQUEST by design, since a member sizes its
	// segment from the same figure and a limit under it would kill the member that fits.
	if !kubemeta.DeepEqual(actual.Resources, expected.Resources) {
		actual.Resources = expected.Resources
		changed = true
	}
	if !kubemeta.DeepEqual(actual.VolumeMounts, expected.VolumeMounts) {
		actual.VolumeMounts = expected.VolumeMounts
		changed = true
	}
	// The security context is compared as a whole rather than capability by capability: it is
	// rendered as a whole, and a running container that acquired one out of band must lose it.
	if !kubemeta.DeepEqual(actual.SecurityContext, expected.SecurityContext) {
		actual.SecurityContext = expected.SecurityContext
		changed = true
	}
	// The shutdown hook carries the scale-in grace INSIDE its argv, and that grace is editable. Left
	// out of this comparison the hook is written once and never again: the pod fingerprint moves
	// with it, so the members are deleted and recreated — from a template whose hook still holds the
	// old grace. The result is worse than not converging, because everything else says the change
	// took effect.
	if !kubemeta.DeepEqual(actual.Lifecycle, expected.Lifecycle) {
		actual.Lifecycle = expected.Lifecycle
		changed = true
	}
	return changed
}

// syncMemberWorkloads converges one DaemonSet per member group.
//
// Admission permits exactly one group in this scope, but the loop is written over the list anyway:
// the index is what names the objects, so a second group added later becomes a second DaemonSet
// rather than a rewrite of the first.
func (r *KVCacheBackendReconciler) syncMemberWorkloads(
	ctx context.Context, kvcb *workercore.KVCacheBackend, image string,
) error {
	logger := ctrllog.FromContext(ctx)

	for group := range kvcb.Spec.Connection.Managed.Members {
		eDs := mooncake.RenderMemberDaemonSet(kvcb, group, image)
		// The LIVE object, not the rendered one: it is what carries the UID, and the restart below
		// proves a Pod is this DaemonSet's by that UID rather than by a selector.
		aDs, err := kubeclientset.CreateWithCtrlClient(ctx, r.Client, eDs,
			kubeclientset.WithUpdateIfExisted(alignMemberDaemonSetFn(kvcb, eDs)))
		if err != nil {
			logger.Error(err, "sync kv cache backend member daemonset", "group", group)
			return err
		}

		if err = r.restartOutdatedMemberPods(ctx, aDs); err != nil {
			logger.Error(err, "restart outdated kv cache backend member pods", "group", group)
			return err
		}
	}

	if err := r.pruneRemovedMemberWorkloads(ctx, kvcb); err != nil {
		logger.Error(err, "prune removed kv cache backend member workloads")
		return err
	}

	logger.V(2).Info("synced kv cache backend member workloads",
		"groups", len(kvcb.Spec.Connection.Managed.Members))
	return nil
}

// pruneRemovedMemberWorkloads deletes the DaemonSets of member groups the spec no longer has.
//
// The loop above walks the CURRENT groups, so on its own it can only ever create and update: a
// group dropped from the list leaves a DaemonSet nothing addresses, and its members keep serving
// segments the backend no longer declares. That was unreachable while exactly one group was
// allowed — dropping the only group means deleting the object, which is teardown's path — and
// became reachable the moment several groups did.
//
// It runs AFTER the sync rather than before it, so a spec that both adds and removes a group is
// never momentarily short of members.
//
// Ownership is proven the way teardown proves it: list on the resource-type label, then keep only
// what carries this backend's own note. The per-group names are DERIVED from the backend's name, so
// an unrelated object can already hold one, and deleting on a name alone would delete somebody
// else's.
func (r *KVCacheBackendReconciler) pruneRemovedMemberWorkloads(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) error {
	logger := ctrllog.FromContext(ctx)

	expected := make(map[string]struct{}, len(kvcb.Spec.Connection.Managed.Members))
	for group := range kvcb.Spec.Connection.Managed.Members {
		expected[mooncake.MemberObjectName(kvcb, group)] = struct{}{}
	}

	dss := new(apps.DaemonSetList)
	err := r.Client.List(ctx, dss,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels(
			systemmeta.GetResourcesLabelSetOfType[map[string]string](kvcache.ResourceType)))
	if err != nil {
		return err
	}

	for i := range dss.Items {
		ds := &dss.Items[i]
		if !renderedForKVCacheBackend(ds, kvcb) {
			continue
		}
		if _, ok := expected[ds.Name]; ok {
			continue
		}
		if ds.DeletionTimestamp != nil {
			continue
		}
		// Foreground, so the Pods are gone before the DaemonSet is: they hold the node memory the
		// departing group claimed, and releasing the object first would leave them running with
		// nothing accounting for them.
		if err = r.Client.Delete(ctx, ds,
			ctrlcli.PropagationPolicy(meta.DeletePropagationForeground)); err != nil &&
			!kerrors.IsNotFound(err) {
			return err
		}
		logger.V(1).Info("deleted the daemonset of a member group the spec no longer declares",
			"daemonset", ds.Name)
	}

	return nil
}

// restartOutdatedMemberPods deletes exactly the member Pods built from a template that has since
// changed in a way that matters.
//
// This is the operator half of the OnDelete strategy, and the two halves only work together. The
// DaemonSet is left on OnDelete because its default would roll every member whenever the node
// selector moved — and widening a group, which is how members are ADDED, moves exactly that. But
// OnDelete alone would leave a changed image written and never applied, so the restart decision is
// made here instead, against a fingerprint that covers the whole template except the node selector.
//
// The Pods are deleted rather than patched: the DaemonSet recreates each one from the current
// template, which is the only way a Pod's spec changes at all.
// memberPodIsOurs reports whether a Pod the member selector matched is actually the named
// DaemonSet's. Labels only SELECT — a selector is a query, and any Pod in the namespace carrying the
// three identity labels matches it, whoever built it. Every use of a member Pod (deleting it,
// counting it against the leader's listing, reading a fault off it) publishes a decision about this
// backend, so each needs the controller reference as proof and not the labels.
//
// Compared by kind and NAME rather than by UID: the name is derived from the backend, so a DaemonSet
// deleted and recreated keeps it while its Pods are orphaned and re-adopted across the gap. A UID
// comparison would disown them for the length of that adoption — the very window a template change
// lands in. No Pod can carry a controller reference to a DaemonSet of this name without being its.
func memberPodIsOurs(pod *core.Pod, daemonSetName string) bool {
	ref := kubemeta.GetControllerOfNoCopy(pod)
	return ref != nil && ref.Kind == "DaemonSet" && ref.Name == daemonSetName
}

func (r *KVCacheBackendReconciler) restartOutdatedMemberPods(
	ctx context.Context, ds *apps.DaemonSet,
) error {
	logger := ctrllog.FromContext(ctx)

	want := ds.Spec.Template.Annotations[mooncake.MemberPodSpecHashAnnotation]
	if want == "" {
		// Nothing to compare against. Deleting every Pod on an unknown fingerprint would turn a
		// rendering bug into a cluster-wide restart, so this does nothing instead.
		return nil
	}

	pods := new(core.PodList)
	err := r.Client.List(ctx, pods,
		ctrlcli.InNamespace(ds.Namespace),
		ctrlcli.MatchingLabels(ds.Spec.Selector.MatchLabels))
	if err != nil {
		return err
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Annotations[mooncake.MemberPodSpecHashAnnotation] == want {
			continue
		}
		if !memberPodIsOurs(pod, ds.Name) {
			logger.V(2).Info("skipping a pod that matches the selector and is not this daemonset's",
				"pod", pod.Name)
			continue
		}

		logger.V(2).Info("restarting a member pod whose spec changed",
			"pod", pod.Name, "node", pod.Spec.NodeName)
		if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// alignMemberDaemonSetFn converges a running member DaemonSet onto the rendered one.
//
// It aligns the same way the leader's does and for the same reason — the API server defaults a Pod
// template on write — with one addition: the fabric fields. hostNetwork, the DNS policy and the
// device volume are all decided by spec.transport.protocol, which admission permits an update to,
// so a backend switched from TCP to RDMA has to acquire them and one switched back has to lose them.
func alignMemberDaemonSetFn(
	kvcb *workercore.KVCacheBackend, eDs *apps.DaemonSet,
) func(*apps.DaemonSet) (*apps.DaemonSet, bool, error) {
	return func(aDs *apps.DaemonSet) (*apps.DaemonSet, bool, error) {
		skip := true
		aPod, ePod := &aDs.Spec.Template.Spec, eDs.Spec.Template.Spec

		// Widening a group's selector is the supported way to add members, so this one moves.
		if !kubemeta.DeepEqual(aPod.NodeSelector, ePod.NodeSelector) {
			aPod.NodeSelector = ePod.NodeSelector
			skip = false
		}

		if aPod.HostNetwork != ePod.HostNetwork {
			aPod.HostNetwork = ePod.HostNetwork
			skip = false
		}
		if aPod.DNSPolicy != ePod.DNSPolicy {
			aPod.DNSPolicy = ePod.DNSPolicy
			skip = false
		}
		if !kubemeta.DeepEqual(aPod.AutomountServiceAccountToken, ePod.AutomountServiceAccountToken) {
			aPod.AutomountServiceAccountToken = ePod.AutomountServiceAccountToken
			skip = false
		}
		if !kubemeta.DeepEqual(aPod.Volumes, ePod.Volumes) {
			aPod.Volumes = ePod.Volumes
			skip = false
		}
		if !kubemeta.DeepEqual(aPod.ImagePullSecrets, ePod.ImagePullSecrets) {
			aPod.ImagePullSecrets = ePod.ImagePullSecrets
			skip = false
		}

		if len(aPod.Containers) != len(ePod.Containers) {
			aPod.Containers = ePod.Containers
			skip = false
		} else {
			for i := range ePod.Containers {
				if alignRenderedContainer(&aPod.Containers[i], ePod.Containers[i]) {
					skip = false
				}
			}
		}

		if !kubemeta.DeepEqual(aDs.Spec.Template.Labels, eDs.Spec.Template.Labels) {
			aDs.Spec.Template.Labels = eDs.Spec.Template.Labels
			skip = false
		}

		// The fingerprint lives here, so a template change is not converged until this moves with
		// it — and a Pod created afterwards inherits the new value, which is what makes a Pod
		// built before the change distinguishable from one built after.
		if !kubemeta.DeepEqual(aDs.Spec.Template.Annotations, eDs.Spec.Template.Annotations) {
			aDs.Spec.Template.Annotations = eDs.Spec.Template.Annotations
			skip = false
		}

		if aDs.Spec.UpdateStrategy.Type != eDs.Spec.UpdateStrategy.Type {
			aDs.Spec.UpdateStrategy = eDs.Spec.UpdateStrategy
			skip = false
		}

		if !kubemeta.DeepEqual(aPod.TerminationGracePeriodSeconds, ePod.TerminationGracePeriodSeconds) {
			aPod.TerminationGracePeriodSeconds = ePod.TerminationGracePeriodSeconds
			skip = false
		}

		if restoreRenderedLabels(aDs, eDs) {
			skip = false
		}

		if !systemmeta.EqualResourceTypeAndNotes(eDs, aDs) {
			systemmeta.SyncResourceTypeAndNotes(eDs, aDs)
			skip = false
		}

		if !kubemeta.IsControlledBy(aDs, kvcb) {
			kubemeta.ControlOnWithoutBlock(aDs, kvcb,
				workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))
			skip = false
		}

		return aDs, skip, nil
	}
}

// alignLeaderServiceFn converges a running Service onto the rendered one.
//
// spec.clusterIP is never touched — it is assigned by the API server and immutable — which is the
// whole reason this aligns named fields instead of assigning the rendered spec wholesale.
func alignLeaderServiceFn(
	kvcb *workercore.KVCacheBackend, eSvc *core.Service,
) func(*core.Service) (*core.Service, bool, error) {
	return func(aSvc *core.Service) (*core.Service, bool, error) {
		skip := true

		if !kubemeta.DeepEqual(aSvc.Spec.Selector, eSvc.Spec.Selector) {
			aSvc.Spec.Selector = eSvc.Spec.Selector
			skip = false
		}

		// The type is converged, and the fields only another type may carry are cleared with it.
		// This one matters more than it looks: the leader serves an admin API on 9003 with no
		// authentication of its own, relying on being a ClusterIP. Flipped to NodePort or
		// LoadBalancer it is published to the outside — and nothing else here would notice, because
		// the port comparison deliberately ignores the assigned nodePort, so the ports keep
		// comparing equal while the exposure stands.
		// Independent of the type, because this one does not need the type to move. A ClusterIP
		// Service carrying spec.externalIPs routes those addresses to every port it declares — 9003
		// among them — while `kubectl get svc` still says ClusterIP and the type comparison above
		// sees nothing to converge. It is the same unauthenticated exposure as a NodePort flip,
		// reached without changing the field that flip is watched by.
		if len(aSvc.Spec.ExternalIPs) > 0 {
			aSvc.Spec.ExternalIPs = nil
			skip = false
		}

		// The third way to reach the same place, and the one that publishes something the other two
		// cannot: an endpoint the readiness gate is deliberately withholding. The leader's probe is
		// gated on its service plane being active, so a client resolving this name gets no address
		// until the store can serve — unless this field says to publish the address anyway. Then
		// the first thing an engine connects to is a leader that answers and cannot serve.
		//
		// Comparing the zero value is safe here, unlike a pull policy: the API server defaults this
		// to false, which is what the renderer leaves it at.
		if aSvc.Spec.PublishNotReadyAddresses != eSvc.Spec.PublishNotReadyAddresses {
			aSvc.Spec.PublishNotReadyAddresses = eSvc.Spec.PublishNotReadyAddresses
			skip = false
		}

		if aSvc.Spec.Type != eSvc.Spec.Type {
			aSvc.Spec.Type = eSvc.Spec.Type
			aSvc.Spec.ExternalTrafficPolicy = ""
			aSvc.Spec.HealthCheckNodePort = 0
			aSvc.Spec.AllocateLoadBalancerNodePorts = nil
			aSvc.Spec.LoadBalancerClass = nil
			for i := range aSvc.Spec.Ports {
				aSvc.Spec.Ports[i].NodePort = 0
			}
			skip = false
		}

		if !equalLeaderServicePorts(aSvc.Spec.Ports, eSvc.Spec.Ports) {
			aSvc.Spec.Ports = eSvc.Spec.Ports
			skip = false
		}

		if restoreRenderedLabels(aSvc, eSvc) {
			skip = false
		}

		if !systemmeta.EqualResourceTypeAndNotes(eSvc, aSvc) {
			systemmeta.SyncResourceTypeAndNotes(eSvc, aSvc)
			skip = false
		}

		if !kubemeta.IsControlledBy(aSvc, kvcb) {
			kubemeta.ControlOnWithoutBlock(aSvc, kvcb,
				workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))
			skip = false
		}

		return aSvc, skip, nil
	}
}

// equalLeaderServicePorts compares the ports on what this operator renders, ignoring nodePort, which
// the API server assigns and which a plain DeepEqual would therefore see as a difference forever.
func equalLeaderServicePorts(actual, expected []core.ServicePort) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range expected {
		if actual[i].Name != expected[i].Name ||
			actual[i].Port != expected[i].Port ||
			actual[i].Protocol != expected[i].Protocol ||
			actual[i].TargetPort != expected[i].TargetPort {
			return false
		}
	}
	return true
}

// teardownKVCacheBackend releases the lock once nothing claims the backend, and refuses to release
// it while something does.
//
// The refusal is a status write and nothing else: no requeue, no timer. status.usedBy is maintained
// by the controllers of the objects that consume this backend, and their write onto this status is
// what wakes this reconcile again. That is the level-based reading — the teardown resumes when the
// fact it is waiting on changes, rather than on a schedule that guesses when it might have.
//
// Waiting for the rendered workloads to GO is the one exception, and it is timed. The difference is
// what the two waits are watching: usedBy is a field on this object, which cannot change without
// waking this reconcile, while the workloads are three other kinds whose deletion events all dedup
// down to this one request.
//
// What it refuses on is the claims that still name something — not the raw list. A consumer that
// vanished without clearing its entry cannot come back to clear it, so refusing on the raw list would
// hold this teardown forever with no event able to end it.
func (r *KVCacheBackendReconciler) teardownKVCacheBackend(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	live := r.liveKVCacheBackendConsumers(ctx, kvcb)
	if len(live) > 0 {
		logger.V(2).Info("kv cache backend is in use; holding the lock",
			"usedBy", len(live))
		return ctrl.Result{}, r.syncStatus(ctx, kvcb, r.computeStatus(kvcb, live))
	}

	// The rendered objects go FIRST, and the lock is what makes that ordering mean anything.
	//
	// They are namespaced dependents of a cluster-scoped owner, which the collector honors — so it
	// would reach them on its own. But between the finalizer coming off and that happening, the
	// leader is still serving on an address no object accounts for any more, and the members still
	// hold the node memory they claimed. Ownership is the safety net, not the mechanism.
	remaining, err := r.deleteRenderedWorkloads(ctx, kvcb)
	if err != nil {
		logger.Error(err, "delete kv cache backend workloads")
		return ctrl.Result{}, err
	}
	if remaining > 0 {
		// The lock stays on until they are GONE, not until the deletes were accepted: the leader
		// keeps serving and the members keep holding the node memory they claimed for as long as
		// their own termination takes.
		//
		// The cost is that a dependent which will not terminate holds this backend with it, so the
		// log names what is left rather than hanging silently.
		logger.V(2).Info("waiting for the backend's workloads to go", "remaining", remaining)
		// Published on the way out, not only on the in-use branch above. Teardown of an unused
		// backend takes as long as its workloads take to terminate, and without this the object
		// keeps whatever phase it had — usually Ready — for that whole window, while the API
		// contract says Deleting is reported until the teardown finishes.
		if err = r.syncStatus(ctx, kvcb, r.computeStatus(kvcb, live)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: kvCacheBackendTeardownInterval}, nil
	}

	systemmeta.Unlock(kvcb)
	if err := r.Client.Update(ctx, kvcb); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	logger.V(2).Info("released kv cache backend")
	return ctrl.Result{}, nil
}

// deleteRenderedWorkloads removes everything a managed backend renders, and is a no-op for an
// external one, which renders nothing. It reports how many of those objects are still there, so the
// caller can hold the finalizer until they are actually gone rather than until the deletes were
// accepted.
//
// It is level-based: a NotFound is the state this is trying to reach, so a partially-deleted backend
// converges on a repeat rather than failing on what somebody else already removed. The member
// DaemonSets are found by the label every group carries rather than by walking a spec that may no
// longer describe them, so a group deleted from the spec before the object was is not left behind.
//
// EVERY delete is guarded by the resource note, and the guard is not ceremony: the names here are
// DERIVED, so an object of that name can predate this backend, and the align path cannot be relied
// on to have adopted it. That path never writes spec.selector, because the field is immutable — so
// a same-name workload whose selector differs has its every update rejected and carries no note,
// however many passes have run. Deleting by derived name alone removes it, and nothing recreates it.
//
// Deletion is FOREGROUND, so that "still there" keeps meaning it for as long as the dependents run.
func (r *KVCacheBackendReconciler) deleteRenderedWorkloads(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (remaining int, err error) {
	if kvcb.Spec.Connection.Managed == nil {
		return 0, nil
	}

	logger := ctrllog.FromContext(ctx)

	leaderKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}
	for _, obj := range []ctrlcli.Object{&apps.Deployment{}, &core.Service{}} {
		present, err := r.deleteOwnedWorkload(ctx, kvcb, obj, leaderKey)
		if err != nil {
			return remaining, err
		}
		if present {
			remaining++
		}
	}

	// Listed on the RESOURCE-TYPE label rather than on this backend's own two, so that what finds
	// the objects and what proves they are ours are the same mechanism. The identity labels are a
	// rendering detail an edit can drop; the resource type is systemmeta's, and the note filtered on
	// below travels with it. Discovering on one key and judging on another is how an object becomes
	// invisible to its own teardown.
	dss := new(apps.DaemonSetList)
	err = r.Client.List(ctx, dss,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels(
			systemmeta.GetResourcesLabelSetOfType[map[string]string](kvcache.ResourceType)))
	if err != nil {
		return remaining, err
	}
	for i := range dss.Items {
		ds := &dss.Items[i]
		if !renderedForKVCacheBackend(ds, kvcb) {
			logger.V(2).Info("skipping a daemonset that matches the labels and carries no note of this backend",
				"daemonset", ds.Name)
			continue
		}
		remaining++
		if ds.DeletionTimestamp != nil {
			continue
		}
		if err = r.Client.Delete(ctx, ds, ctrlcli.PropagationPolicy(meta.DeletePropagationForeground)); err != nil {
			if kerrors.IsNotFound(err) {
				remaining--
				continue
			}
			return remaining, err
		}
	}

	return remaining, nil
}

// deleteOwnedWorkload deletes one object this backend renders under a derived name, and reports
// whether it is still present when the call returns.
//
// The object is READ before it is deleted, because the name alone proves nothing about who it
// belongs to. Present-and-not-ours reports absent: it is not this backend's to delete, and it is
// not this backend's to wait for either.
func (r *KVCacheBackendReconciler) deleteOwnedWorkload(
	ctx context.Context, kvcb *workercore.KVCacheBackend, obj ctrlcli.Object, key ctrlcli.ObjectKey,
) (present bool, err error) {
	if err = r.Client.Get(ctx, key, obj); err != nil {
		return false, ctrlcli.IgnoreNotFound(err)
	}

	if !renderedForKVCacheBackend(obj, kvcb) {
		ctrllog.FromContext(ctx).V(2).Info(
			"skipping an object that carries this backend's derived name and no note of it",
			"object", fmt.Sprintf("%T", obj), "name", key.Name)
		return false, nil
	}

	if obj.GetDeletionTimestamp() != nil {
		return true, nil
	}

	err = r.Client.Delete(ctx, obj, ctrlcli.PropagationPolicy(meta.DeletePropagationForeground))
	if err != nil {
		return !kerrors.IsNotFound(err), ctrlcli.IgnoreNotFound(err)
	}
	return true, nil
}

// restoreRenderedLabels puts the rendered labels back on a live object and reports whether anything
// moved.
//
// An object's OWN labels are not decoration here, they are discovery keys — the member sweep finds a
// backend's DaemonSets by listing on them. Left unconverged, an edit that drops one hides the object
// from the reconciler that owns it, and the teardown runs on a path where this align never does, so
// nothing would put it back in time to matter.
//
// Restored key by key rather than wholesale: a workload in a real cluster carries labels put there by
// things that are not this operator, and removing those is not this reconciler's business.
func restoreRenderedLabels(actual, expected ctrlcli.Object) (changed bool) {
	for k, v := range expected.GetLabels() {
		if !kubemeta.IsLabeled(actual, k, v) {
			kubemeta.SetLabel(actual, k, v)
			changed = true
		}
	}
	return changed
}

// renderedForKVCacheBackend reports whether an object is one this backend's renderers produced.
//
// The resource note is the judge and the labels are not, because the labels are a QUERY: anything in
// this namespace can carry them, and a delete has to be surer than a list. The controller reference
// is not consulted either — it would answer the same question a second time, since the align path
// writes the note and the reference together on every pass.
func renderedForKVCacheBackend(obj ctrlcli.Object, kvcb *workercore.KVCacheBackend) bool {
	return systemmeta.MatchResource(obj, kvcache.ResourceType, kvcache.ResourceNoteBackend, kvcb.Name)
}

// syncStatus writes the desired status only when it differs, so a settled object produces no write
// and no self-triggered event.
func (r *KVCacheBackendReconciler) syncStatus(
	ctx context.Context, kvcb *workercore.KVCacheBackend, desired workercore.KVCacheBackendStatus,
) error {
	if kubemeta.DeepEqual(desired, kvcb.Status) {
		return nil
	}

	logger := ctrllog.FromContext(ctx)

	kvcb.Status = desired
	if err := r.Client.Status().Update(ctx, kvcb); err != nil {
		logger.Error(err, "update kv cache backend status")
		return err
	}
	logger.V(2).Info("refreshed kv cache backend status", "phase", desired.Phase)
	return nil
}

// computeStatus builds the whole status from what is observed.
//
// It observes one thing so far: whether anything claims the backend, which is what Deletable
// answers and what the teardown refuses on. The three workload conditions arrive with the tasks
// that can observe them — there is no leader to ask and no member to count yet — and inventing them
// here would report a state nothing measured. Everything already in status that this pass does not
// derive is carried forward, so a later task's figures are not erased by this one.
//
// The claims arrive as an argument rather than being read off the object, because deciding which of
// them still name something is a lookup and this stays a pure function of what was observed.
func (r *KVCacheBackendReconciler) computeStatus(
	kvcb *workercore.KVCacheBackend, live []workercore.KVCacheObjectReference,
) workercore.KVCacheBackendStatus {
	// The condition accessors mutate the object they are given, so they work on a copy: the caller
	// compares this result against the observed status and writes only on a difference.
	desired := &workercore.KVCacheBackend{Status: *kvcb.Status.DeepCopy()}

	inUseBy := formatKVCacheBackendConsumers(live)
	switch {
	case inUseBy != "":
		KVCacheBackendConditionDeletable.False(desired, "InUse",
			fmt.Sprintf("in use by %s", inUseBy))
	case len(kvcb.Status.UsedBy) > 0:
		// Every claim on this backend names something that is gone. Saying so, rather than reporting
		// the plain "no object claims this backend", is what tells a reader why usedBy and Deletable
		// disagree — the alternative is a status that looks self-contradictory and explains nothing.
		KVCacheBackendConditionDeletable.True(desired, "Unused",
			fmt.Sprintf("no object claims this backend; status.usedBy still names %s, which no longer "+
				"exists", formatKVCacheBackendConsumers(kvcb.Status.UsedBy)))
	default:
		KVCacheBackendConditionDeletable.True(desired, "Unused", "no object claims this backend")
	}

	switch {
	case kvcb.DeletionTimestamp != nil:
		desired.Status.Phase = KVCacheBackendPhaseDeleting
		desired.Status.PhaseMessage = KVCacheBackendConditionDeletable.GetMessage(desired)
		if inUseBy == "" {
			desired.Status.PhaseMessage = ""
		}
	default:
		desired.Status.Phase = KVCacheBackendPhaseProvisioning
		desired.Status.PhaseMessage = "waiting for the backend's workloads"
	}

	return desired.Status
}

// deriveKVCacheBackendPhase summarizes the observed conditions into the one field a human reads
// first.
//
// It runs only over conditions this pass actually wrote, so the phase never claims more than was
// observed. A deletion in progress is decided earlier, in computeStatus, and is not revisited here:
// a backend on its way out is Deleting whatever its leader happens to be doing.
//
// There are FIVE phases and no separate Pending. To a reader, a workload that has not been scheduled
// and a leader that has not finished starting are the same state — the backend is not usable yet —
// and phaseMessage is what tells them apart.
// leaderIsStillStarting reports whether a LeaderAvailable=False reason describes a start in
// progress rather than a fault.
//
// LeaderStarting is a leader whose Pod is not ready yet, which is what every fresh install looks
// like for its first minute. ServicePlaneNotActive is a leader that is answering and says its
// service plane has not come up — it reported that itself, so it is running.
//
// Everything else is a fault, including the reasons the kubelet writes. That is the direction the
// list has to run: this operator owns the two progress states and does not own the fault vocabulary,
// so a reason nobody here anticipated reports as Error and gets looked at.
func leaderIsStillStarting(reason string) bool {
	return reason == "LeaderStarting" || reason == "ServicePlaneNotActive"
}

func deriveKVCacheBackendPhase(holder *workercore.KVCacheBackend, renderBlocked error) {
	if holder.Status.Phase == KVCacheBackendPhaseDeleting {
		return
	}

	switch {
	case renderBlocked != nil && len(holder.Status.Endpoints) == 0:
		// Nothing was ever rendered, so there is no address to read and LeaderAvailable is False for
		// the trivial reason that there is nothing to ask. Ranked ABOVE the leader check for exactly
		// that: its reason would otherwise be read as "still coming up", and a backend that can
		// never start would sit at Provisioning indefinitely instead of naming what is missing.
		holder.Status.Phase = KVCacheBackendPhaseDegraded
		holder.Status.PhaseMessage = fmt.Sprintf("nothing has been rendered for this backend and "+
			"nothing can be, so it cannot start: %v", renderBlocked)

	case KVCacheBackendConditionLeaderAvailable.IsFalse(holder):
		// The leader not serving is the difference between "coming up" and "broken", and only the
		// condition's own reason knows which. An unreachable leader is an Error; one that answers
		// and says it is not active yet is still Provisioning.
		// Named by what is still STARTING, not by what is broken. The faults include the kubelet's
		// own reasons — an image that will not pull, a container that will not stay up — and this
		// operator does not own that vocabulary: a list of them would have to grow every time the
		// kubelet learns a new one, and until it did, a new fault would report as a start in
		// progress. Two reasons describe progress, and everything else is a fault.
		holder.Status.Phase = KVCacheBackendPhaseError
		if leaderIsStillStarting(KVCacheBackendConditionLeaderAvailable.GetReason(holder)) {
			holder.Status.Phase = KVCacheBackendPhaseProvisioning
		}
		holder.Status.PhaseMessage = KVCacheBackendConditionLeaderAvailable.GetMessage(holder)

	case renderBlocked != nil:
		// Ranked under a leader that is not serving — the store being broken is the more urgent
		// fact — and OVER a shortfall, because a shortfall is often this problem's symptom: a member
		// group added to the spec is simply never rendered. Reporting the shortfall would name what
		// is missing and hide why.
		//
		// Not a condition, because every condition here describes the STORE and the store may be
		// perfectly healthy; what is degraded is this operator's hold on it.
		holder.Status.Phase = KVCacheBackendPhaseDegraded
		holder.Status.PhaseMessage = fmt.Sprintf("the backend is running, but its workloads can no "+
			"longer be rendered, so a change to this object would not reach them: %v", renderBlocked)

	case KVCacheBackendConditionMembersMounted.IsFalse(holder):
		// The leader serves, so the backend exists — it just holds less than it should.
		holder.Status.Phase = KVCacheBackendPhaseDegraded
		holder.Status.PhaseMessage = KVCacheBackendConditionMembersMounted.GetMessage(holder)

	case KVCacheBackendConditionLeaderAvailable.IsTrue(holder):
		holder.Status.Phase = KVCacheBackendPhaseReady
		holder.Status.PhaseMessage = ""
	}
}

// formatKVCacheBackendConsumers names the claimants in the refusal message, because an operator
// whose delete is refused needs to know what to go and remove.
//
// Sampled, because usedBy has no item bound: it is written by whoever consumes the backend, one
// entry per consumer, and this message is what carries Deletable=False and the Deleting phase. An
// unbounded join would take the refusal down with it — the delete would be refused and the object
// would not say so.
func formatKVCacheBackendConsumers(refs []workercore.KVCacheObjectReference) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Kind+"/"+ref.Name)
	}
	return listBoundedNames(names)
}

// liveKVCacheBackendConsumers keeps the entries of status.usedBy that still name an object which
// exists.
//
// The list is written by the consumers themselves and cleared by them on the way out, so ordinarily
// every entry is live and this returns all of them. It exists for the case where a consumer went away
// WITHOUT clearing its entry — an operator forcing a wedged KVCachePool's finalizer off, say. Nothing
// can clean up after that consumer, because the consumer is what would have done the cleaning, so the
// reader has to work without the cleanup ever happening. Otherwise an entry naming a pool that is
// gone holds this backend's teardown for good, and the only sign of it is a Deletable=False naming an
// object nobody can find.
//
// An entry is dropped only on a definite NOT FOUND. A kind this reconciler cannot look up, or a read
// that failed, is KEPT: turning "cannot verify" into "does not exist" is the wrong direction for a
// claim whose whole purpose is to hold a deletion.
//
// That rule is only as good as the read behind it, so the read is stated rather than assumed. It goes
// through the manager's informer cache, and the cache's error is ONE-DIRECTIONAL: an entry enters the
// indexer on an ADD and leaves on a DELETE, so a cache running behind still holds a pool it has not
// yet seen deleted. It errs towards refusing a teardown, never towards allowing one.
//
// Erring the other way takes a cache that has never seen the pool, and there are two ways to get one.
// A cache that has not started is closed by controller-runtime, which syncs every informer before it
// starts a controller. A pool created moments ago, its ADD still in flight, is closed by what must
// already have happened for this loop to be reading the entry at all: that entry was written by the
// POOL's own reconciler, which runs on the same manager and therefore out of the same cache, so that
// cache had seen the pool before the entry existed. "This cache does not hold the pool" and "usedBy
// names the pool" cannot both be true.
//
// Deliberately NO ResourceVersion option. A cached Get ignores it — CacheReader.Get reads only
// UnsafeDisableDeepCopy off the options — so passing one would advertise a staleness trade-off this
// call does not actually make, on the one read in this file where getting staleness wrong would
// release a lock that cannot be taken back.
func (r *KVCacheBackendReconciler) liveKVCacheBackendConsumers(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) []workercore.KVCacheObjectReference {
	if len(kvcb.Status.UsedBy) == 0 {
		return nil
	}

	live := make([]workercore.KVCacheObjectReference, 0, len(kvcb.Status.UsedBy))
	for _, ref := range kvcb.Status.UsedBy {
		if ref.Kind != KVCachePoolKind {
			live = append(live, ref)
			continue
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, new(workercore.KVCachePool))
		if err == nil || !kerrors.IsNotFound(err) {
			live = append(live, ref)
		}
	}
	return live
}

// enqueueKVCacheBackendWhenPoolChanged maps a pool back to every backend it draws from.
//
// It reads the pool's spec rather than the backends' usedBy, because the event that matters most is
// the pool's DELETE — and a delete carries the object's last known state, which still names them.
func (r *KVCacheBackendReconciler) enqueueKVCacheBackendWhenPoolChanged(
	_ context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	kvcp, ok := obj.(*workercore.KVCachePool)
	if !ok {
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(kvcp.Spec.Backends))
	for _, name := range kvcp.Spec.Backends {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	}
	return reqs
}

// enqueueKVCacheBackendWhenWorkloadChanged maps a rendered object back to the backend that owns it.
//
// It reads the backend's name from the object's own resource note rather than walking its owner
// references: the note is what the predicate has already filtered on, so the two agree by
// construction, and a delete event carries the note as readily as an update does.
func (r *KVCacheBackendReconciler) enqueueKVCacheBackendWhenWorkloadChanged(
	_ context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	name := systemmeta.DescribeResourceNote(obj, kvcache.ResourceNoteBackend)
	if name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: name}}}
}

// kvCacheBackendWorkloadPredicate keeps the watches to this operator's own objects. Without it every
// Deployment and Service in the cluster would wake this reconciler.
//
// The namespace is half of the test, and it is the half that is not about noise. A resource note is
// an annotation, so anyone who can create an object in ANY namespace can write one that matches —
// and each match costs the named backend a reconcile, which is three HTTP reads against its admin
// surface. Rendering only ever happens in the system namespace, so nothing outside it can be one of
// this operator's objects, whatever it says about itself.
func kvCacheBackendWorkloadPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
		return obj.GetNamespace() == kuberess.SystemNamespaceName &&
			systemmeta.MatchResource(obj, kvcache.ResourceType)
	})
}

func (r *KVCacheBackendReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	if r.AdminHTTP == nil {
		r.AdminHTTP = newAdminHTTPClient()
	}

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("kvcachebackend").
		For(&workercore.KVCacheBackend{}).
		Watches(
			// A pool claims this backend by writing itself into status.usedBy, and its own reconciler
			// clears that entry on the way out — so ordinarily this backend is woken by its own status
			// changing and this watch adds nothing. What it covers is the pool going away WITHOUT
			// clearing the entry, which nothing else can wake: the claim would name an object that no
			// longer exists and hold the teardown with no event ever arriving to re-examine it.
			//
			// Not deduped, unlike the workload watches. Those fire per rendered object and collapse
			// onto one backend; this one fires per pool, and a pool names each of its backends once.
			&workercore.KVCachePool{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueKVCacheBackendWhenPoolChanged),
		).
		Watches(
			// Watch the leader's Deployment and enqueue the backend that owns it. Without this the
			// reconciler converges the rendered objects only when the BACKEND changes, so a hand
			// edit to the Deployment would stand until something else happened to the object —
			// which is not continuous reconciliation, it is reconciliation on a coincidence.
			&apps.Deployment{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueKVCacheBackendWhenWorkloadChanged,
			),
			ctrlbuilder.WithPredicates(kvCacheBackendWorkloadPredicate()),
		).
		Watches(
			// Same for the Service in front of it, which carries the published endpoints.
			&core.Service{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueKVCacheBackendWhenWorkloadChanged,
			),
			ctrlbuilder.WithPredicates(kvCacheBackendWorkloadPredicate()),
		).
		Watches(
			// And the member groups' DaemonSets, which is where a node joining or leaving a group
			// first becomes visible.
			&apps.DaemonSet{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueKVCacheBackendWhenWorkloadChanged,
			),
			ctrlbuilder.WithPredicates(kvCacheBackendWorkloadPredicate()),
		).
		Watches(
			// And the leader's own Pods, for the one fault that is only ever written there. The
			// scheduler records "no node will take this" on the POD, as PodScheduled=False, and that
			// write moves nothing on the Deployment — whose own account of it arrives ten minutes
			// later as ProgressDeadlineExceeded. Without this watch the pass that reports an
			// unschedulable leader would have to be the one some other event happened to trigger.
			&core.Pod{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueKVCacheBackendWhenLeaderPodChanged,
			),
			ctrlbuilder.WithPredicates(kvCacheBackendLeaderPodPredicate()),
		).
		WithOptions(ctrlcontroller.Options{
			MaxConcurrentReconciles: kvCacheBackendConcurrency,
		}).
		Complete(r)
}

// kvCacheBackendConcurrency is how many backends this controller reconciles at once.
//
// It is set because this reconciler is the only one here that BLOCKS on something outside the
// cluster: every backend re-enqueues itself on a timer and each pass makes three sequential reads
// against an address the object names — for an external backend, an address this operator has no
// control over at all. On the default single worker, one endpoint that accepts a connection and then
// stalls holds the queue for its timeout on every pass, and every other backend waits behind it —
// including one being deleted.
//
// A per-request timeout does not substitute for this. It bounds how long one stall lasts and says
// nothing about who else is blocked by it.
//
// Four rather than more: the work is one HTTP read at a time against distinct hosts, so this is
// about not letting a few bad addresses monopolize the queue, not about throughput.
const kvCacheBackendConcurrency = 4

// kvCacheBackendLeaderPodPredicate keeps the Pod watch to this operator's leaders.
//
// It cannot use the resource note the other three watches filter on: a Pod is made by the
// Deployment's controller from a template, and the note is on the Deployment rather than inside that
// template. The identity labels are what a leader's Pod does carry — and a label is cheaper to
// forge than a note, so the namespace matters here for the same reason it does there.
func kvCacheBackendLeaderPodPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
		return obj.GetNamespace() == kuberess.SystemNamespaceName &&
			mooncake.LeaderPodBackendName(obj.GetLabels()) != ""
	})
}

// enqueueKVCacheBackendWhenLeaderPodChanged maps a leader's Pod back to the backend that owns it.
func (r *KVCacheBackendReconciler) enqueueKVCacheBackendWhenLeaderPodChanged(
	_ context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	name := mooncake.LeaderPodBackendName(obj.GetLabels())
	if name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: name}}}
}
