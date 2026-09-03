package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
)

// ModelDeploymentConditionCacheAttached reports whether the cache is observed to be in effect,
// which is a different question from whether it was configured.
//
// A flag being accepted proves nothing: measured on the shipped store, `--enable_kv_events=true` is
// accepted, the startup log echoes `enable_kv_events=1`, and `GET /kv_events/status` still answers
// `{"enabled":false}` with the socket never bound. In the same project another undeclared switch
// fails loudly instead. One switch's failure mode cannot be inferred from another's, so this
// condition is judged downstream of the engine and never on a render or a log line.
const ModelDeploymentConditionCacheAttached kubeapistatus.ConditionType = "CacheAttached"

const (
	modelDeploymentReasonCacheActive            = "CacheActive"
	modelDeploymentReasonCacheOperationsFailing = "CacheOperationsFailing"
	modelDeploymentReasonUnmanaged              = "Unmanaged"
	modelDeploymentReasonNoReplicaReady         = "NoReplicaReady"
	modelDeploymentReasonNoObservationAvailable = "NoObservationAvailable"
)

// ModelDeploymentCacheReading is one replica's own account of its KV cache client.
//
// THREE VALUES, AND THE THIRD IS NOT A NEGATIVE. What the three supported engines actually expose
// was measured, and none of them publishes anything that says "the connector initialized" before any
// traffic: vLLM's five `vllm:mooncake_store_operation_*` families are all labeled and only reach
// `.labels()` from an observation path that returns early on empty data; vllm-ascend declares no
// Prometheus metric of its own at all; SGLang's storage counters are labeled the same way, and its
// `hicache_host_*` gauges appear at startup but only prove the host tier is on. So "no account" and
// "attached and idle" are indistinguishable, and reading the first as a detachment would be a false
// alarm on the most common steady state there is.
//
// This type therefore CANNOT express a traffic-free "initialized" reading, and that is deliberate: a
// stand-in able to report a signal no engine provides would make the Unknown path untestable, and
// every test over it would be a test of a signal that does not exist.
type ModelDeploymentCacheReading int

const (
	// ModelDeploymentCacheUnreadable is no account at all: the endpoint did not answer, or it
	// answered and carried no store metric family. It is Unknown, never False.
	ModelDeploymentCacheUnreadable ModelDeploymentCacheReading = iota
	// ModelDeploymentCacheFailing is an account of store operations of which NONE succeeded. It is
	// positive evidence rather than an absence: the metric families carry a status label whose
	// measured values are "ok", "error" and "partial_failure", so a replica that tried and failed
	// publishes series nothing else publishes.
	ModelDeploymentCacheFailing
	// ModelDeploymentCacheActive is an account of store operations that succeeded.
	ModelDeploymentCacheActive
)

// ModelDeploymentCacheScraper reads one replica's own account of its KV cache client.
//
// It is an INTERFACE THE RECONCILER TAKES rather than a dial it makes, because every case that
// matters here is a failure and a real dial cannot be made to fail on demand.
//
// The account is per replica at the Pod's own address, and that is the property the whole condition
// rests on: the reuse domain is shared by every deployment on one Binding by design, so a
// domain-level figure cannot attribute — with a healthy deployment and a broken one on one Binding,
// the healthy one's writes would report the broken one as attached.
type ModelDeploymentCacheScraper interface {
	// ScrapeCache reads one replica's account.
	//
	// A non-nil error means the account could not be read at all, and the reading is then
	// Unreadable. Unreadable with a nil error is the other way to not have an account: the endpoint
	// answered, and carried no store metric family.
	ScrapeCache(ctx context.Context, pod *core.Pod) (ModelDeploymentCacheReading, error)
}

// observeModelDeploymentCache folds the cache-attachment reading into the status.
//
// Level-based, evaluated every pass, and never True by assumption.
func (r *ModelDeploymentReconciler) observeModelDeploymentCache(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
	domain *modelDeploymentDomain, holder *workercore.ModelDeployment,
) {
	if role := modelDeploymentUnmanagedRole(md); role != "" {
		// The operator synthesized no argument and no client environment for that role, so it does
		// not claim the observation is about its own doing. Whatever the pool reports.
		ModelDeploymentConditionCacheAttached.Unknown(holder, modelDeploymentReasonUnmanaged,
			fmt.Sprintf("role %q replaced the whole command line, so the operator configured no "+
				"cache client for it and does not report on one it did not render", role))

		return
	}

	ready := modelDeploymentReadyReplicas(pods)
	if len(ready) == 0 {
		ModelDeploymentConditionCacheAttached.Unknown(holder, modelDeploymentReasonNoReplicaReady,
			"no replica has become ready yet, so no engine has an account to give")

		return
	}

	active, failing, unreadable := r.readModelDeploymentCache(ctx, ready)

	switch {
	case active > 0:
		ModelDeploymentConditionCacheAttached.True(holder, modelDeploymentReasonCacheActive,
			fmt.Sprintf("%d of %d ready replicas report succeeding store operations",
				active, len(ready)))

	case failing > 0:
		// NOT an absence, and this is the one state with no nearer observer. A connector that
		// cannot come up takes its replica with it — vLLM raises without its config path — so that
		// failure is already reported as a replica that never becomes Ready. This one is different:
		// the engine is Ready and serving, and every store operation it attempts fails. Nothing else
		// in the status says so, which is why it is False here rather than Unknown.
		ModelDeploymentConditionCacheAttached.False(holder,
			modelDeploymentReasonCacheOperationsFailing,
			fmt.Sprintf("%d of %d ready replicas report store operations of which none succeeded; "+
				"the engine is serving without the cache", failing, len(ready)))

	case domain != nil && modelDeploymentDomainHoldsData(domain):
		// The corroborating signal, used only where no replica gave an account. It is the master's
		// own account of bytes that landed, which is stronger evidence of a working cache and weaker
		// attribution — it is per reuse domain, and one domain is shared by design.
		ModelDeploymentConditionCacheAttached.True(holder, modelDeploymentReasonCacheActive,
			fmt.Sprintf("no replica gave an account, and reuse domain %q holds data; this attributes "+
				"to the domain rather than to this deployment, because one domain is shared by "+
				"every deployment on its binding", domain.KVCache.Domain.Name))

	default:
		// Unknown because THE STATE IT WOULD REPORT HAS A NEARER OBSERVER, not merely because the
		// signal is missing. A connector that failed to come up killed its replica, and a replica
		// that is not Ready is already reported by status.roles[].ready. What is left here is a
		// deployment that is attached and simply idle, which is indistinguishable from an unread
		// one — and the two must not be collapsed into a detachment report.
		//
		// Absent is not zero: an absent blocks/usage on the Binding means the scrape did not carry
		// this domain, while an observed zero is a domain holding nothing, and an idle attached
		// domain holds nothing either.
		ModelDeploymentConditionCacheAttached.Unknown(holder,
			modelDeploymentReasonNoObservationAvailable,
			fmt.Sprintf("none of the %d ready replicas gave an account of its cache client and the "+
				"reuse domain reports nothing held; an attached deployment that is idle looks "+
				"exactly like this, so it is not reported as detached", unreadable))
	}
}

// readModelDeploymentCache scrapes every ready replica and counts what came back.
//
// A scraper the reconciler was built without is every replica unreadable rather than an error: the
// condition's whole contract is that an unread signal is Unknown.
func (r *ModelDeploymentReconciler) readModelDeploymentCache(
	ctx context.Context, ready []*core.Pod,
) (active, failing, unreadable int) {
	if r.CacheScraper == nil {
		return 0, 0, len(ready)
	}

	for _, pod := range ready {
		reading, err := r.CacheScraper.ScrapeCache(ctx, pod)
		if err != nil {
			unreadable++

			continue
		}
		switch reading {
		case ModelDeploymentCacheActive:
			active++
		case ModelDeploymentCacheFailing:
			failing++
		case ModelDeploymentCacheUnreadable:
			unreadable++
		}
	}

	return active, failing, unreadable
}

// modelDeploymentUnmanagedRole names the first role that took over its command line, or "".
func modelDeploymentUnmanagedRole(md *workercore.ModelDeployment) string {
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.Template != nil && len(role.Template.Command) > 0 {
			return role.Name
		}
	}

	return ""
}

// modelDeploymentReadyReplicas selects the replicas an engine can be asked about: Ready, and not on
// their way out. A terminating replica may still answer, and what it says is about a process that is
// leaving.
func modelDeploymentReadyReplicas(pods []core.Pod) []*core.Pod {
	ready := make([]*core.Pod, 0, len(pods))
	for i := range pods {
		if pods[i].DeletionTimestamp != nil || !podIsReady(&pods[i]) {
			continue
		}
		ready = append(ready, &pods[i])
	}

	return ready
}

// modelDeploymentDomainHoldsData reports whether the master's account of the reuse domain shows
// anything held.
//
// nil is not zero on either figure. Both are pointers with omitempty on the Binding, so absent
// means the scrape did not carry this domain; treating that as zero would turn every unscraped pool
// into a positive detachment report.
func modelDeploymentDomainHoldsData(domain *modelDeploymentDomain) bool {
	if domain.KVCache == nil {
		return false
	}
	if domain.Blocks != nil && *domain.Blocks > 0 {
		return true
	}

	return domain.Usage != nil && !domain.Usage.IsZero()
}
