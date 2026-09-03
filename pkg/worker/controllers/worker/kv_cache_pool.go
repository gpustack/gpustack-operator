package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
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
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

const (
	// IndexingKVCachePoolBindingByPool indexes each KVCachePoolBinding by the pool it names. It is
	// what makes "the Bindings of this pool" one scoped query rather than a list of every Binding in
	// the cluster filtered in memory — a pass runs it on every reconcile, so the difference is per
	// pass rather than per startup.
	IndexingKVCachePoolBindingByPool = "kvcachepoolbindings.worker.gpustack.ai/pool"

	// IndexingKVCachePoolByBackend indexes each KVCachePool by the backends it draws from.
	//
	// It exists because the tenant ledger and the policy file converge per MASTER and not per pool
	// (F7): several pools may sit on one backend, and a pass that wrote only its own pool's tenants
	// would erase the others' on every reconcile. Finding those sibling pools has to be one query
	// for the same reason above — it happens on every pass, on both of them.
	IndexingKVCachePoolByBackend = "kvcachepools.worker.gpustack.ai/backend"
)

// indexKVCachePoolBindingByPool is the field-index extractor for IndexingKVCachePoolBindingByPool.
//
// A Binding whose pool does not exist is still indexed. That is deliberate: the pool may be created
// afterwards, and the index is what carries the Binding into the pass that follows — dropping it
// here would leave a Binding that named a pool early permanently invisible to it.
func indexKVCachePoolBindingByPool(obj ctrlcli.Object) []string {
	kvcpb, ok := obj.(*workercore.KVCachePoolBinding)
	if !ok || kvcpb == nil || kvcpb.Spec.PoolRef.Name == "" {
		return nil
	}
	return []string{kvcpb.Spec.PoolRef.Name}
}

// indexKVCachePoolByBackend is the field-index extractor for IndexingKVCachePoolByBackend.
//
// It indexes every entry of spec.backends rather than the first, although admission admits exactly
// one: the index has to describe what is STORED, and an object stored before that rule tightened, or
// written by a client that bypassed the webhook, would otherwise be missing from the very query its
// siblings use to find each other.
func indexKVCachePoolByBackend(obj ctrlcli.Object) []string {
	kvcp, ok := obj.(*workercore.KVCachePool)
	if !ok || kvcp == nil {
		return nil
	}

	names := make([]string, 0, len(kvcp.Spec.Backends))
	for _, name := range kvcp.Spec.Backends {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// The phases a KVCachePool reports, derived from what was observed rather than from what was asked
// for: a pool whose backend has published no address is Provisioning however complete its spec is.
const (
	KVCachePoolPhaseProvisioning = "Provisioning"
	KVCachePoolPhaseReady        = "Ready"
	KVCachePoolPhaseDegraded     = "Degraded"
	KVCachePoolPhaseError        = "Error"
	KVCachePoolPhaseDeleting     = "Deleting"
)

// The condition types a KVCachePool reports, one per axis.
//
// They are spelled POSITIVELY, as every condition in this repository is and as the Kubernetes API
// conventions ask: the type names the healthy state and False carries the fault. The spec names two
// faults — MultiTenancyDisabled and QuotaPolicyNotWritable — and those are REASONS on the two
// conditions below rather than condition types of their own, because a type that is True when
// something is wrong reads backwards from every other condition an operator sees on this cluster.
const (
	// KVCachePoolConditionBackendResolved is False while the backend cannot be read or has published
	// no address to reach it on.
	KVCachePoolConditionBackendResolved kubeapistatus.ConditionType = "BackendResolved"
	// KVCachePoolConditionQuotaLedgerAvailable is False when the master holds no tenant ledger to
	// write into — reason MultiTenancyDisabled, which is F5's precondition observed at runtime
	// rather than assumed from admission.
	KVCachePoolConditionQuotaLedgerAvailable kubeapistatus.ConditionType = "QuotaLedgerAvailable"
	// KVCachePoolConditionQuotaPolicyWritable is False when the master accepted no quota write —
	// reason QuotaPolicyNotWritable when its own policy source is what refused it.
	KVCachePoolConditionQuotaPolicyWritable kubeapistatus.ConditionType = "QuotaPolicyWritable"
	// KVCachePoolConditionCapacityAllocatable is False when the master reports nothing to allocate.
	// That is the startup-ordering trap: no member has mounted, so every effective quota is zero and
	// no write can succeed while every object still looks correctly configured.
	KVCachePoolConditionCapacityAllocatable kubeapistatus.ConditionType = "CapacityAllocatable"
	// KVCachePoolConditionReleasable is False only while a deletion is being HELD, and it is
	// deliberately absent from the axes summarizeKVCachePool reads: a live pool never carries it, and
	// an axis nobody has observed reads as Provisioning to that function.
	KVCachePoolConditionReleasable kubeapistatus.ConditionType = "Releasable"
)

// The reasons the spec names by name. They are constants because criterion 11 asserts them and an
// e2e case greps for them.
const (
	KVCachePoolReasonMultiTenancyDisabled   = "MultiTenancyDisabled"
	KVCachePoolReasonQuotaPolicyNotWritable = "QuotaPolicyNotWritable"
)

// The two reasons a pool's release is held.
const (
	// KVCachePoolReasonHeldByBindings is a grant that has not been withdrawn: releasing under a
	// Binding would leave a namespace holding a quota on a pool that no longer exists.
	KVCachePoolReasonHeldByBindings = "HeldByBindings"
	// KVCachePoolReasonLedgerNotReleased is capacity nobody could see afterwards. The master's ledger
	// carries no label saying whose an entry is, so once this object is gone nothing left in the
	// cluster knows to delete what it registered.
	KVCachePoolReasonLedgerNotReleased = "LedgerNotReleased"
)

// The kinds a usedBy entry in this pair names.
//
// They are constants because a claim and its release have to match on the same strings, and the two
// happen in different passes — one when a pool resolves its backend, the other when the pool is being
// torn down and the entry has to be found again by value.
const (
	KVCachePoolKind        = "KVCachePool"
	KVCachePoolBindingKind = "KVCachePoolBinding"
)

// kvCachePoolObserveInterval re-runs a pass against a settled pool. The master's figures — effective
// quota, usage, allocatable capacity — move without anything in the cluster changing, so a pool that
// only reconciled on Kubernetes events would report a snapshot from whenever the last one happened.
const kvCachePoolObserveInterval = 30 * time.Second

// KVCachePoolReconciler reconciles a KVCachePool.
//
// The pool is the ONLY reconciled object of this pair: a KVCachePoolBinding has no reconciler of its
// own, and every Binding event is mapped onto the pool it names. That is not a simplification, it is
// what makes the pass correct — the tenant ledger and the policy file are per master, so writing
// them from a per-Binding loop would have each Binding's pass erase what the others just wrote.
type KVCachePoolReconciler struct {
	Client ctrlcli.Client

	// AdminHTTP reads and writes the leader's admin surface. It carries its own timeout for the
	// reason the backend reconciler's does: a master that accepts a connection and then stalls would
	// otherwise hold this reconcile open for as long as the transport allows.
	AdminHTTP *http.Client
}

func (r *KVCachePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	kvcp := new(workercore.KVCachePool)
	err := r.Client.Get(ctx, req.NamespacedName, kvcp, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch kv cache pool")
			return ctrl.Result{}, err
		}
		// Gone, and its Bindings may not be: they are indexed by the name they themselves carry, so
		// this key still reaches them. What the pool wrote outside the cluster is released by its own
		// finalizer while the object still exists; what is left here is letting go of Bindings that
		// would otherwise wait for a reconciler with no object to run.
		return ctrl.Result{}, r.releaseOrphanKVCachePoolBindings(ctx, req.Name)
	}

	if kvcp.DeletionTimestamp != nil {
		return r.teardownKVCachePool(ctx, kvcp)
	}

	// Lock. Everything this pool registers on a master outlives the object, and the ledger carries no
	// label saying whose an entry is — so the finalizer is the only window in which what to delete is
	// still known.
	if !systemmeta.Lock(kvcp) {
		if err = r.Client.Update(ctx, kvcp); err != nil {
			logger.Error(err, "lock kv cache pool")
			return ctrl.Result{}, err
		}
	}

	// The condition accessors work on an object with a Status field, so the desired status is built
	// inside one. It carries the real ObjectMeta because a condition write reads the object it is
	// given, and it is a COPY of the observed status, so syncStatus still compares two independent
	// values.
	holder := &workercore.KVCachePool{
		ObjectMeta: *kvcp.ObjectMeta.DeepCopy(),
		Status:     *kvcp.Status.DeepCopy(),
	}
	// Derived afresh every pass. Carrying the previous one forward would go on advertising an
	// address whose backend has since been deleted, and would tell the phase that something was
	// resolved when nothing was.
	holder.Status.ClientEndpoint = ""

	kvcb, resolveErr := r.resolveKVCachePoolBackend(ctx, kvcp)
	if resolveErr != nil {
		if resolveErr.internal != nil {
			logger.Error(resolveErr.internal, "resolve kv cache pool backend")
			return ctrl.Result{}, resolveErr.internal
		}
		KVCachePoolConditionBackendResolved.False(holder, resolveErr.reason, resolveErr.message)
		// A Binding already on its way out is let go HERE, before this pass gives up. Its entry is on
		// a master that no longer exists, so there is nothing left to delete from and nothing owed —
		// and nothing else would ever reach it: a Binding event enqueues only the pool it names, and
		// this return is what that pool's every pass does while the backend stays gone. Without this
		// the finalizer holds forever and the object can only be freed by hand.
		if resolveErr.gone {
			if err = r.releaseOrphanKVCachePoolBindings(ctx, kvcp.Name); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Requeued rather than left waiting for an event: a backend coming up publishes its
		// addresses through its OWN status, and this reconciler does not watch that object.
		return ctrl.Result{RequeueAfter: kvCachePoolObserveInterval},
			r.syncKVCachePoolStatus(ctx, kvcp, r.summarizeKVCachePool(holder))
	}

	// Claimed as soon as the backend RESOLVES, before anything is asked of it. The claim is what holds
	// the backend's own teardown, and a backend that has published no address yet is exactly the one
	// somebody might delete while a pool is waiting for it to come up.
	if err = r.claimKVCacheBackend(ctx, kvcb, kvcp); err != nil {
		logger.Error(err, "claim kv cache backend", "backend", kvcb.Name)
		return ctrl.Result{}, err
	}

	clientAddress, adminAddress := kvCacheBackendAddresses(kvcb)
	if adminAddress == "" {
		KVCachePoolConditionBackendResolved.False(holder, "EndpointsNotPublished",
			fmt.Sprintf("backend %q publishes no %q address yet, so its quota ledger cannot be "+
				"reached; no address is derived from the backend's name, because one that happened "+
				"to resolve would drive the wrong master",
				kvcb.Name, workercore.KVCacheBackendEndpointNameAdmin))
		return ctrl.Result{RequeueAfter: kvCachePoolObserveInterval},
			r.syncKVCachePoolStatus(ctx, kvcp, r.summarizeKVCachePool(holder))
	}
	KVCachePoolConditionBackendResolved.True(holder, "Resolved",
		fmt.Sprintf("backend %q is reachable", kvcb.Name))
	// Only the Client address is republished. The Admin one is the write face of the quota ledger,
	// and this object is cluster-scoped and readable by anyone holding a pool RBAC rule.
	holder.Status.ClientEndpoint = clientAddress

	// Everything below converges the MASTER, not this pool: the ledger and the policy file are
	// shared by every pool that names this backend.
	master, err := r.observeKVCachePoolMaster(ctx, kvcb)
	if err != nil {
		logger.Error(err, "read the pools and bindings on one backend")
		return ctrl.Result{}, err
	}
	holder.Status.Domains = master.domainsOf(kvcp)
	holder.Status.UsedBy = master.usedByOf(kvcp)

	// Before the first external write, never after: what goes onto the master below can only be
	// removed while an object still names it, so a Binding written for must not be deletable without
	// one. Failing here costs a requeue; failing to do it costs an entry nothing can find again.
	if err = r.lockKVCachePoolBindings(ctx, master); err != nil {
		return ctrl.Result{}, err
	}

	if err = r.syncQuotaPolicyConfigMap(ctx, kvcb, master.tenants); err != nil {
		logger.Error(err, "sync kv cache quota policy configmap")
		return ctrl.Result{}, err
	}

	admin := &mooncake.AdminClient{Address: adminAddress, HTTP: r.AdminHTTP}
	ledger := r.convergeTenantLedger(ctx, admin, master, holder)
	observeKVCachePoolUsage(holder, ledger)

	// A domain the master REFUSED to drop goes back into the seed, and this second render is what puts
	// it there. The document above was written from the desired tenants, which exclude a releasing
	// Binding — correct once the entry is actually gone, and wrong while it is not: the seed is what
	// the leader's init container copies over the master's own policy on every container start, so a
	// restart in this window would take the live domain's policy away while its objects are still
	// there. Rendered only when something was retained, so a settled pass writes the ConfigMap once.
	//
	// A CONTESTED domain needs the same treatment for the same reason: the pass leaves its ledger
	// entry alone on purpose, so a seed that omits it would remove the quota on the next restart.
	if len(ledger.retained) > 0 || len(master.contested) > 0 {
		tenants := withContestedTenants(master, ledger.observed)
		if len(ledger.retained) > 0 {
			tenants = withRetainedTenants(master, ledger.retained, tenants)
		}
		if err = r.syncQuotaPolicyConfigMap(ctx, kvcb, tenants); err != nil {
			logger.Error(err, "re-sync kv cache quota policy configmap with the preserved tenants")
			return ctrl.Result{}, err
		}
	}

	// The Bindings are written from that one scrape, before the pool's own status: a Binding reports
	// figures the pool's phase is derived from, and writing the summary first would publish a Ready
	// pool whose Bindings still carried the previous pass's numbers.
	if err = r.syncKVCachePoolBindings(ctx, kvcp, master, ledger); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: kvCachePoolObserveInterval},
		r.syncKVCachePoolStatus(ctx, kvcp, r.summarizeKVCachePool(holder))
}

// kvCachePoolResolveError is a backend this pass could not use, split by whose problem it is.
//
// A reportable one becomes a Condition on the pool and the pass carries on to write it; an internal
// one is a failure of this operator's own reads and goes back to the queue unchanged. Collapsing the
// two would either retry a user's typo forever or bury a broken cache read in an object's status.
type kvCachePoolResolveError struct {
	reason  string
	message string
	// gone says the backend definitively does not exist, as opposed to one this pass could not settle
	// on. The difference decides whether a Binding already marked for deletion may be let go: its
	// ledger entry lives on a master that is not there, so nothing is owed to it and holding the
	// finalizer would strand the object forever. A pool naming the wrong NUMBER of backends is not
	// this — the master may be perfectly alive under one of the names — so it keeps holding.
	gone     bool
	internal error
}

func (e *kvCachePoolResolveError) Error() string {
	if e.internal != nil {
		return e.internal.Error()
	}
	return e.message
}

// resolveKVCachePoolBackend reads the one backend this pool draws from.
func (r *KVCachePoolReconciler) resolveKVCachePoolBackend(
	ctx context.Context, kvcp *workercore.KVCachePool,
) (*workercore.KVCacheBackend, *kvCachePoolResolveError) {
	// Admission takes exactly one, and this reads what is STORED: an object written before that rule
	// or around the webhook is reconciled rather than panicked over, and reported rather than
	// half-served.
	if len(kvcp.Spec.Backends) != 1 {
		return nil, &kvCachePoolResolveError{
			reason: "BackendNotSingular",
			message: fmt.Sprintf("this pool names %d backends and exactly 1 is supported: quota is "+
				"enforced by one master's ledger", len(kvcp.Spec.Backends)),
		}
	}

	name := kvcp.Spec.Backends[0]
	kvcb := new(workercore.KVCacheBackend)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, kvcb, ctrlclix.WithoutQuorum); err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, &kvCachePoolResolveError{internal: err}
		}
		// Admission required it to exist, and it may have been deleted since. Level-based: the
		// Condition says so and the next pass looks again.
		return nil, &kvCachePoolResolveError{
			reason:  "BackendNotFound",
			message: fmt.Sprintf("backend %q does not exist", name),
			gone:    true,
		}
	}

	return kvcb, nil
}

// kvCacheBackendAddresses reads a backend's two published addresses BY NAME.
//
// By name and not by position: status.endpoints is a list keyed on name, so a reader that took the
// first entry would dial whichever role the writer happened to order first. An address this pass
// does not find comes back empty rather than derived from the backend's own name — a guessed address
// that happens to resolve is how a pool would silently drive the wrong master.
func kvCacheBackendAddresses(kvcb *workercore.KVCacheBackend) (client, admin string) {
	for i := range kvcb.Status.Endpoints {
		endpoint := &kvcb.Status.Endpoints[i]
		switch endpoint.Name {
		case workercore.KVCacheBackendEndpointNameClient:
			client = endpoint.Address
		case workercore.KVCacheBackendEndpointNameAdmin:
			admin = endpoint.Address
		}
	}
	return client, admin
}

// kvCachePoolPredicate decides which events about the pool itself are worth a pass.
//
// It triggers on a create, on a generation (spec) change, and on the object being marked for
// deletion. It refuses the final removal — there is nothing left to converge — and, load-bearingly,
// it refuses this operator's OWN writes: a status update and a finalizer edit change neither the
// generation nor the deletion timestamp, so without this every pass would schedule the next one and
// the loop would never settle.
func kvCachePoolPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.Funcs{
		DeleteFunc: func(ctrlevent.DeleteEvent) bool { return false },
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			oldKvcp, ok := e.ObjectOld.(*workercore.KVCachePool)
			if !ok {
				return true
			}
			newKvcp, ok := e.ObjectNew.(*workercore.KVCachePool)
			if !ok {
				return true
			}
			if !oldKvcp.DeletionTimestamp.Equal(newKvcp.DeletionTimestamp) {
				return true
			}
			return oldKvcp.Generation != newKvcp.Generation
		},
	}
}

func (r *KVCachePoolReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexers. Both are registered here rather than where they are read, because
	// an index has to exist before the cache starts and a List against a missing one fails at
	// runtime rather than at compile time.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &workercore.KVCachePoolBinding{},
		IndexingKVCachePoolBindingByPool, indexKVCachePoolBindingByPool)
	if err != nil {
		return fmt.Errorf("index kv cache pool binding '%s': %w", IndexingKVCachePoolBindingByPool, err)
	}
	err = fi.IndexField(ctx, &workercore.KVCachePool{},
		IndexingKVCachePoolByBackend, indexKVCachePoolByBackend)
	if err != nil {
		return fmt.Errorf("index kv cache pool '%s': %w", IndexingKVCachePoolByBackend, err)
	}

	r.Client = opts.Manager.GetClient()
	if r.AdminHTTP == nil {
		r.AdminHTTP = newAdminHTTPClient()
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("kvcachepool").
		For(
			// Reconcile each KVCachePool by its own name.
			//
			// Trigger on create, on a generation (spec) change, and on the object being marked for
			// deletion. Never on the final removal — there is nothing left to converge — and never
			// on this operator's own status or finalizer writes, which change neither the generation
			// nor the deletion timestamp and would otherwise make every pass schedule the next one.
			&workercore.KVCachePool{},
			ctrlbuilder.WithPredicates(kvCachePoolPredicate()),
		).
		Watches(
			// Every Binding event reaches the pool it names. A Binding is where a reuse domain and a
			// ceiling are declared, so its create, its edit and its deletion are all changes to what
			// the pool has to write into the master's ledger.
			//
			// Deletion is NOT filtered out here, unlike on the pool itself: a Binding that goes away
			// is a tenant whose entry has to be removed, and no other event says so.
			&workercore.KVCachePoolBinding{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueKVCachePoolWhenBindingChanged),
		).
		Complete(r)
}

// kvCachePoolMaster is everything about ONE master that a pass needs, gathered once.
//
// It is master-wide and not pool-wide, and that is the point of the type existing: the tenant ledger
// and the policy file are shared by every pool bound to a backend, so a pass that only knew its own
// pool would write a document describing a fraction of the master and erase the rest.
type kvCachePoolMaster struct {
	// tenants is the desired ledger: one entry per Binding that asked for a ceiling, across every
	// pool on this master, in a deterministic order so two renders of one state are byte-identical.
	tenants []mooncake.QuotaPolicyTenant

	// domains is every reuse domain claimed on this master, keyed by domain name, whether or not a
	// ceiling was asked for. A domain with no ceiling is claimed and writes no ledger entry, which
	// are two different facts.
	domains map[string]kvCachePoolDomainClaim

	// registered is every domain a pool on this master has ALREADY published in its status. It is
	// what makes a delete safe: an entry in the master's ledger that this operator never registered
	// belongs to somebody else — an external backend may well be serving tenants nobody here
	// created — and deleting it would be data loss with no trace of who asked for it.
	registered map[string]struct{}

	// contested is every domain more than one Binding claims, with EVERY claimant — including the
	// ones removed from domains above. The claims are kept rather than counted because both sides
	// have to name the other in their own status, and a count cannot say who.
	contested map[string][]workercore.KVCachePoolBindingReference

	// bindings is every Binding of every pool on this master, keyed by pool name. The Binding pass
	// writes status onto these, and they come from the same List the claims were built from: a
	// second query would let a Binding created in between be reported against a ledger converged
	// without it.
	bindings map[string][]workercore.KVCachePoolBinding
}

// kvCachePoolDomainClaim is one Binding's claim on a reuse domain.
type kvCachePoolDomainClaim struct {
	pool    string
	binding workercore.KVCachePoolBindingReference
	domain  workercore.KVCachePoolBindingDomain
}

// domainsOf builds one pool's registry entries out of the master-wide claims.
//
// It is AUTHORITATIVE rather than advisory: an entry exists because a Binding declares it. The
// observed figures each entry can also carry are not written here — a domain nobody scraped yet is
// reported without them rather than with zeroes.
func (m *kvCachePoolMaster) domainsOf(kvcp *workercore.KVCachePool) []workercore.KVCachePoolDomain {
	names := make([]string, 0, len(m.domains))
	for name, claim := range m.domains {
		if claim.pool == kvcp.Name {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)

	domains := make([]workercore.KVCachePoolDomain, 0, len(names))
	for _, name := range names {
		claim := m.domains[name]
		domains = append(domains, workercore.KVCachePoolDomain{
			Name:      name,
			Binding:   claim.binding,
			BlockSize: claim.domain.BlockSize,
			Dtype:     claim.domain.Dtype,
		})
	}
	return domains
}

// usedByOf lists the Bindings holding one pool, from the same List the claims were built from.
//
// It is a single-scope query in the direction a cluster-scoped object is allowed to look: the pool
// reads its Bindings through the index, and no Binding ever reads across a namespace to find it. The
// entries carry a REAL namespace, unlike the one this pool writes into its backend, because a Binding
// is namespaced and the namespace is half of its identity.
func (m *kvCachePoolMaster) usedByOf(kvcp *workercore.KVCachePool) []workercore.KVCacheObjectReference {
	bindings := m.bindings[kvcp.Name]
	if len(bindings) == 0 {
		return nil
	}

	usedBy := make([]workercore.KVCacheObjectReference, 0, len(bindings))
	for i := range bindings {
		// A Binding this pass is about to release is already not a holder: the same pass takes its
		// lock off, so listing it would publish a holder that no longer exists by the time the status
		// lands. A deleting one something still HOLDS stays on the list, because it does still exist
		// and its own deletion is going nowhere until that holder lets go.
		if kvCachePoolBindingIsReleasing(&bindings[i]) {
			continue
		}
		usedBy = append(usedBy, workercore.KVCacheObjectReference{
			Kind:      KVCachePoolBindingKind,
			Namespace: bindings[i].Namespace,
			Name:      bindings[i].Name,
		})
	}
	if len(usedBy) == 0 {
		return nil
	}
	// One order for one state, so a re-derivation does not rewrite the status because a List came
	// back differently.
	slices.SortFunc(usedBy, compareKVCacheObjectReferences)
	return usedBy
}

// compareKVCacheObjectReferences orders a usedBy list. Kind first, because one list may hold several,
// then the namespace and the name that identify an object within it.
func compareKVCacheObjectReferences(a, b workercore.KVCacheObjectReference) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// kvCachePoolBindingIsReleasing reports a Binding that is being deleted and that nothing holds any
// more.
//
// Such a Binding is dropped from the pass's claims — the ledger entry it owned has to go BEFORE its
// finalizer comes off, and converging a ledger nobody asks that entry of is what deletes it. A
// deleting Binding a workload still holds keeps both its claim and its quota: the workload is still
// writing, and withdrawing the quota under it would refuse writes the deletion has not authorized
// stopping.
func kvCachePoolBindingIsReleasing(kvcpb *workercore.KVCachePoolBinding) bool {
	return kvcpb.DeletionTimestamp != nil && len(kvcpb.Status.UsedBy) == 0
}

// observeKVCachePoolMaster gathers every pool on one backend and every Binding of those pools.
//
// Two indexed queries and no namespace walk: one for the sibling pools, then one per pool for its
// Bindings. The sibling query is what F7 is about — a pass that skipped it would render a policy
// file holding only its own tenants, and the other pool's would vanish on every reconcile.
func (r *KVCachePoolReconciler) observeKVCachePoolMaster(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (*kvCachePoolMaster, error) {
	pools := new(workercore.KVCachePoolList)
	err := r.Client.List(ctx, pools,
		ctrlcli.MatchingFields{IndexingKVCachePoolByBackend: kvcb.Name})
	if err != nil {
		return nil, fmt.Errorf("list the pools on backend %q: %w", kvcb.Name, err)
	}

	master := &kvCachePoolMaster{
		domains:    map[string]kvCachePoolDomainClaim{},
		registered: map[string]struct{}{},
		contested:  map[string][]workercore.KVCachePoolBindingReference{},
		bindings:   map[string][]workercore.KVCachePoolBinding{},
	}
	claimants := map[string][]workercore.KVCachePoolBindingReference{}

	for i := range pools.Items {
		pool := &pools.Items[i]

		// The index carries EVERY entry of spec.backends, so a pool naming two of them is listed here
		// under both. resolveKVCachePoolBackend refuses to serve such a pool at all — it reports
		// BackendNotSingular and claims neither backend — and this pass has to agree with it, or the
		// pool's ceilings are written into two masters' ledgers by two reconciles while the pool
		// itself owns a record on neither.
		//
		// Refusing to SERVE it is not refusing to know about it: its domains still go into registered
		// and its Bindings still into the snapshot below, because an entry a previous pass wrote can
		// only be removed while its name is still known, and the finalizers still have to come off.
		// Registered but not desired is exactly what convergeTenantLedger deletes, which is the right
		// outcome for a pool no backend serves.
		served := len(pool.Spec.Backends) == 1

		// What this pool said last pass. Read before the Bindings, because it describes what is in
		// the master's ledger now rather than what should be.
		for j := range pool.Status.Domains {
			master.registered[pool.Status.Domains[j].Name] = struct{}{}
		}

		bindings := new(workercore.KVCachePoolBindingList)
		err = r.Client.List(ctx, bindings,
			ctrlcli.MatchingFields{IndexingKVCachePoolBindingByPool: pool.Name})
		if err != nil {
			return nil, fmt.Errorf("list the bindings of pool %q: %w", pool.Name, err)
		}

		master.bindings[pool.Name] = bindings.Items

		for j := range bindings.Items {
			kvcpb := &bindings.Items[j]
			name := kvcpb.Spec.Domain.Name
			if name == "" {
				continue
			}
			// Registered whether or not this pass goes on to ask for it. What may be deleted is what
			// this operator CREATED, and the pool's status is one pass behind the ledger: an entry
			// written by a pass whose status write then failed would otherwise be invisible to the
			// delete path — and permanently so once its Binding was gone.
			master.registered[name] = struct{}{}

			if kvCachePoolBindingIsReleasing(kvcpb) || !served {
				// Its claim is over, or the pool holding it is one no backend serves. Either way it
				// stays in master.bindings, because its finalizer still has to come off, but it
				// contributes neither a domain nor a tenant: converging the ledger without it is what
				// DELETEs the entry it owned, and re-rendering the policy without it is what drops it
				// from the document.
				continue
			}

			ref := workercore.KVCachePoolBindingReference{
				Namespace: kvcpb.Namespace,
				Name:      kvcpb.Name,
			}
			claimants[name] = append(claimants[name], ref)
			master.domains[name] = kvCachePoolDomainClaim{
				pool:    pool.Name,
				binding: ref,
				domain:  kvcpb.Spec.Domain,
			}

			master.tenants = append(master.tenants, mooncake.QuotaPolicyTenant{
				Name:  name,
				Quota: kvcpb.Spec.QuotaCeiling,
			})
		}
	}

	// A domain two Bindings claim is managed for NEITHER of them, and its ledger entry is left
	// exactly as it is. Admission refuses the second claim, so reaching this means two creates raced
	// one cache — and picking a winner here would hand one namespace a ceiling the other one set.
	// The claimants are kept so both Bindings can name each other; only the acting stops here.
	for name, refs := range claimants {
		if len(refs) > 1 {
			master.contested[name] = refs
			delete(master.domains, name)
			// Dropped from registered as well, and that is what makes "left exactly as it is" true.
			// Convergence deletes an entry that is registered but not desired; removing the name from
			// tenants alone would take it out of desired while leaving it registered, so the entry
			// two Bindings are arguing over would be DELETED by the pass that claims to leave it
			// alone — taking the cache with it, for both of them.
			delete(master.registered, name)
		}
	}
	master.tenants = slices.DeleteFunc(master.tenants, func(t mooncake.QuotaPolicyTenant) bool {
		_, contested := master.contested[t.Name]
		return contested
	})

	// One order for one state, so a re-render diffs cleanly against the last and the ConfigMap is
	// not rewritten because a List came back in another order.
	slices.SortFunc(master.tenants, func(a, b mooncake.QuotaPolicyTenant) int {
		return strings.Compare(a.Name, b.Name)
	})

	return master, nil
}

// withContestedTenants puts a domain two Bindings are arguing over back into the desired document.
//
// The contested branch drops it from the desired tenants so neither claimant's ceiling is written,
// and deliberately leaves its LEDGER entry exactly as it is. The seed has to say the same thing: it
// is copied over the master's own policy on every container start, so a document that omits the
// domain would take its explicit quota away on the next leader restart and start refusing the writes
// of a cache both namespaces are still using — a data-plane outcome from a conflict the API already
// refuses at admission.
//
// The ceiling written back is the one already ON the master, not either claimant's: the point is to
// preserve what is there, and choosing a claimant's figure here is the pick the contested branch
// exists to avoid.
func withContestedTenants(
	master *kvCachePoolMaster, observed []mooncake.TenantQuota,
) []mooncake.QuotaPolicyTenant {
	if len(master.contested) == 0 {
		return master.tenants
	}

	tenants := slices.Clone(master.tenants)
	for i := range observed {
		entry := &observed[i]
		if _, contested := master.contested[entry.TenantID]; !contested {
			continue
		}
		if !entry.HasExplicitPolicy {
			// Nothing of ours to preserve: the domain runs under the master's default, and writing a
			// figure for it here would create the explicit policy this pass has no claimant for.
			continue
		}
		tenants = append(tenants, mooncake.QuotaPolicyTenant{
			Name: entry.TenantID,
			// The REQUESTED figure, not the effective one: the policy file carries ceilings, and the
			// grant is what the master computes from them. Seeding it with a grant would ratchet the
			// ceiling down to whatever the last division happened to yield.
			Quota: *resource.NewQuantity(entry.RequestedBytes, resource.BinarySI),
		})
	}

	slices.SortFunc(tenants, func(a, b mooncake.QuotaPolicyTenant) int {
		return strings.Compare(a.Name, b.Name)
	})
	return tenants
}

// withRetainedTenants puts the entries the master would not remove back into the desired document.
//
// Their Bindings are releasing, so observeKVCachePoolMaster left them out — which is right for the
// LEDGER, where the removal is exactly what this pass is trying to do, and wrong for the SEED, which
// the leader copies over its own policy on every container start. Between the refusal and the drain
// the domain is still live, and a restart in that window would take its policy with it.
//
// A ceiling comes from the releasing Binding itself, which is still in the snapshot: it is the figure
// the entry was written with, so putting it back reproduces the document rather than inventing one.
func withRetainedTenants(
	master *kvCachePoolMaster, retained []string, base []mooncake.QuotaPolicyTenant,
) []mooncake.QuotaPolicyTenant {
	tenants := slices.Clone(base)
	for _, domain := range retained {
		if slices.ContainsFunc(tenants, func(t mooncake.QuotaPolicyTenant) bool {
			return t.Name == domain
		}) {
			continue
		}
		if ceiling, ok := retainedTenantCeiling(master, domain); ok {
			tenants = append(tenants, mooncake.QuotaPolicyTenant{Name: domain, Quota: ceiling})
		}
	}

	// The same order the snapshot uses, so a document rebuilt here diffs cleanly against the one
	// written before the ledger pass.
	slices.SortFunc(tenants, func(a, b mooncake.QuotaPolicyTenant) int {
		return strings.Compare(a.Name, b.Name)
	})
	return tenants
}

// retainedTenantCeiling finds the ceiling a domain was registered with, across every pool on this
// master. An entry no Binding claims any more is left out: nothing here knows what it asked for.
func retainedTenantCeiling(
	master *kvCachePoolMaster, domain string,
) (ceiling resource.Quantity, found bool) {
	for _, bindings := range master.bindings {
		for i := range bindings {
			if bindings[i].Spec.Domain.Name == domain {
				return bindings[i].Spec.QuotaCeiling, true
			}
		}
	}
	return resource.Quantity{}, false
}

// lockKVCachePoolBindings takes the finalizer on every live Binding on this master, and must run
// BEFORE anything is written outside the cluster.
//
// The ledger entry a Binding owns can only ever be deleted by name, and the name is only known while
// some object still carries it: master.registered is built from the pools' published domains and
// from the live Bindings, so a Binding that disappears before either records its domain takes the
// only record of that entry with it. The entry then survives every later pass — convergeTenantLedger
// refuses to delete what this operator cannot prove it registered, which is the right refusal and
// exactly what makes the loss permanent.
//
// Locking inside the per-pool Binding pass was too late: the policy document and the ledger are
// converged first, so a crash between the external write and that pass left a Binding deletable with
// no finalizer and its entry already on the master.
//
// It walks EVERY pool's Bindings, not this pool's, because the writes it guards are themselves
// master-wide — master.tenants is "one entry per Binding that asked for a ceiling, across every pool
// on this master", and convergeTenantLedger PUTs each one. Guarding only this pool's Bindings would
// leave the siblings this pass is about to write for unprotected. The reconciler already owns those
// master-wide writes; this closes them, it does not widen them.
//
// A Binding already marked for deletion is skipped rather than locked: putting a finalizer back on
// an object the API server is waiting to remove would strand it.
func (r *KVCachePoolReconciler) lockKVCachePoolBindings(
	ctx context.Context, master *kvCachePoolMaster,
) error {
	logger := ctrllog.FromContext(ctx)

	for pool := range master.bindings {
		for i := range master.bindings[pool] {
			kvcpb := &master.bindings[pool][i]

			if kvcpb.DeletionTimestamp != nil || systemmeta.Lock(kvcpb) {
				continue
			}
			if err := r.Client.Update(ctx, kvcpb); err != nil {
				logger.Error(err, "lock kv cache pool binding before converging the master",
					"kv cache pool binding", ctrlcli.ObjectKeyFromObject(kvcpb))
				return err
			}
		}
	}

	return nil
}

// syncQuotaPolicyConfigMap writes the whole policy document for one master.
//
// It renders WHOLE and overwrites, never merges: the ConfigMap is the desired state, and the master
// rewrites its own copy of the file on every admin-API change. A pass that merged into what it found
// would be merging into that rewrite.
//
// A document the renderer refuses STOPS the pass, and it has to. The refusal means at least one
// tenant in this set failed the validator F6 requires everything to clear before it can reach the
// master — and the very next thing the caller does is PUT that same set into the ledger, one entry at
// a time, with no second validation. Carrying on would send the refused tenant to the master and
// leave the ledger holding entries the seed document does not, which is the one divergence the whole
// render-wholly-from-the-operator design exists to prevent.
//
// Retrying it forever is the lesser cost. Every input here came through a webhook running the same
// validator, so a refusal is either an object written around admission or a bug in this operator;
// both are conditions to surface loudly, and neither is one to converge past.
func (r *KVCachePoolReconciler) syncQuotaPolicyConfigMap(
	ctx context.Context, kvcb *workercore.KVCacheBackend, tenants []mooncake.QuotaPolicyTenant,
) error {
	logger := ctrllog.FromContext(ctx)

	document, err := mooncake.RenderQuotaPolicy(tenants)
	if err != nil {
		logger.Error(err, "render the tenant quota policy",
			"backend", kvcb.Name, "tenants", len(tenants))
		return fmt.Errorf("render the tenant quota policy of backend %q: %w", kvcb.Name, err)
	}

	eCM := mooncake.RenderQuotaPolicyConfigMap(kvcb, document)
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eCM,
		kubeclientset.WithUpdateIfExisted(alignQuotaPolicyConfigMapFn(kvcb, eCM)))
	if err != nil {
		return err
	}

	logger.V(2).Info("synced the tenant quota policy", "backend", kvcb.Name, "tenants", len(tenants))
	return nil
}

// alignQuotaPolicyConfigMapFn converges an existing policy ConfigMap onto the rendered one.
//
// Only what this operator RENDERS is compared: the document, the resource note and the owner
// reference. A ConfigMap picks up labels and annotations from whatever else touches the cluster, and
// rewriting it for those would make every pass a write — which is exactly the churn the status guard
// avoids one object over.
//
// The second return value is SKIP, not "changed". Returning false with nothing changed rewrites the
// object on every pass; returning a nil object with it panics inside the caller.
func alignQuotaPolicyConfigMapFn(
	kvcb *workercore.KVCacheBackend, eCM *core.ConfigMap,
) func(*core.ConfigMap) (*core.ConfigMap, bool, error) {
	return func(aCM *core.ConfigMap) (*core.ConfigMap, bool, error) {
		skip := true

		if !kubemeta.DeepEqual(aCM.Data, eCM.Data) {
			aCM.Data = eCM.Data
			skip = false
		}

		if !systemmeta.EqualResourceTypeAndNotes(eCM, aCM) {
			systemmeta.SyncResourceTypeAndNotes(eCM, aCM)
			skip = false
		}

		// Without this, a ConfigMap that predates the owner reference — or one somebody edited it
		// off — outlives the backend it belongs to, and nothing else would ever delete it: the
		// backend's own teardown does not know this object exists.
		if !kubemeta.IsControlledBy(aCM, kvcb) {
			kubemeta.ControlOnWithoutBlock(aCM, kvcb,
				workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))
			skip = false
		}

		return aCM, skip, nil
	}
}

// convergeTenantLedger makes the master's ledger match what the Bindings ask for.
//
// Three rules, and the third is the one worth stating: a PUT only where the observed request differs
// so a settled master takes no write at all; a DELETE only for an entry this operator itself
// registered and no Binding still asks for; and NOTHING for an entry it never registered. That last
// one is what keeps an external master's own tenants — created by whoever runs it — from being
// deleted by a pool that happens to point at it.
//
// It writes Conditions rather than returning an error. Every failure here is the master's answer,
// and an answer is an observation to report: a requeue would re-ask a question whose answer will not
// change until somebody edits the backend.
func (r *KVCachePoolReconciler) convergeTenantLedger(
	ctx context.Context,
	admin *mooncake.AdminClient,
	master *kvCachePoolMaster,
	holder *workercore.KVCachePool,
) kvCachePoolLedgerPass {
	logger := ctrllog.FromContext(ctx)

	observed, err := admin.ListTenantQuotas(ctx)
	if err != nil {
		r.reportTenantLedgerFailure(holder, err, "reading the tenant ledger")
		logger.Error(err, "list tenant quotas")
		return kvCachePoolLedgerPass{}
	}

	held := make(map[string]mooncake.TenantQuota, len(observed))
	for _, entry := range observed {
		held[entry.TenantID] = entry
	}

	desired := make(map[string]struct{}, len(master.tenants))
	for _, tenant := range master.tenants {
		desired[tenant.Name] = struct{}{}

		// Value() is safe here: the webhook and the renderer both refuse a quantity that would not
		// survive the conversion, through the same validator.
		want := tenant.Quota.Value()
		if entry, ok := held[tenant.Name]; ok && entry.HasExplicitPolicy && entry.RequestedBytes == want {
			continue
		}
		if err = admin.PutTenantQuota(ctx, tenant.Name, want); err != nil {
			r.reportTenantLedgerFailure(holder, err,
				fmt.Sprintf("writing the quota of reuse domain %q", tenant.Name))
			logger.Error(err, "put tenant quota", "tenant", tenant.Name)
			return kvCachePoolLedgerPass{}
		}
	}

	// The entries this pass asked the master to remove and did not get, because the domain still holds
	// objects. Collected rather than counted: a releasing Binding reports which of them is ITS domain.
	var retained []string

	for _, entry := range observed {
		if _, wanted := desired[entry.TenantID]; wanted {
			continue
		}
		if _, ours := master.registered[entry.TenantID]; !ours {
			// Somebody else's tenant on a master this pool merely points at.
			logger.V(2).Info("leaving a ledger entry this operator never registered",
				"tenant", entry.TenantID)
			continue
		}
		if !entry.HasExplicitPolicy {
			// This operator only ever writes explicit policies, so only an explicit policy is its to
			// remove. The PUT side above already reads this same field; deleting without reading it
			// would make the two halves disagree about what an entry of ours looks like.
			logger.V(2).Info("leaving a ledger entry that carries no explicit policy",
				"tenant", entry.TenantID)
			continue
		}
		if err = admin.DeleteTenantQuota(ctx, entry.TenantID); err != nil {
			if errors.Is(err, mooncake.ErrTenantNotEmpty) {
				// The domain still holds objects. Not a fault — the entry goes once it has drained —
				// but not converged either: this DELETE is one the pass WANTED and did not get, and
				// converged is what lets a releasing Binding's finalizer come off. Reporting it as
				// converged would remove the object that owns this entry while the entry is still on
				// the master, and the ledger carries no label saying whose it was.
				logger.V(2).Info("a reuse domain still holds objects, so its quota stays",
					"tenant", entry.TenantID)
				retained = append(retained, entry.TenantID)
				continue
			}
			r.reportTenantLedgerFailure(holder, err,
				fmt.Sprintf("removing the quota of reuse domain %q", entry.TenantID))
			logger.Error(err, "delete tenant quota", "tenant", entry.TenantID)
			return kvCachePoolLedgerPass{}
		}
	}

	KVCachePoolConditionQuotaLedgerAvailable.True(holder, "Available",
		"the master holds a tenant ledger and it is writable")
	KVCachePoolConditionQuotaPolicyWritable.True(holder, "Writable",
		fmt.Sprintf("the master's ledger holds the %d reuse domain(s) asked for on this backend, "+
			"across every pool bound to it", len(master.tenants)))

	metrics, scraped := r.observeAllocatableCapacity(ctx, admin, holder)
	return kvCachePoolLedgerPass{
		converged: len(retained) == 0,
		retained:  retained,
		observed:  observed,
		metrics:   metrics,
		scraped:   scraped,
	}
}

// kvCachePoolLedgerPass is what one pass over the master's ledger came back with.
//
// The two flags look alike and are not. Converged says every PUT and DELETE this pass wanted was
// accepted, and it is what gates releasing a Binding: the entry has to be gone from the master before
// the object that owned it stops existing, or the capacity it held becomes unreclaimable — the ledger
// carries no label saying whose an entry is. Scraped says the exposition answered, which is a
// different surface that fails on its own; a ledger that converged and a scrape that did not is an
// ordinary state, and the Bindings then keep the figures they had.
type kvCachePoolLedgerPass struct {
	converged bool
	// retained names the entries the master would not remove because their domain still holds
	// objects. It is the one NON-converged outcome that is not a failure — the next pass asks again —
	// so it is carried rather than folded into converged alone: a Binding whose own domain is in here
	// can say why its deletion is waiting, instead of reporting a ledger that would not answer.
	retained []string
	// observed is the ledger as it was READ this pass. It is carried so a re-render can reproduce an
	// entry the pass deliberately did not write — a contested domain's, whose figure must come from
	// the master rather than from either claimant.
	observed []mooncake.TenantQuota
	metrics  mooncake.TenantQuotaMetrics
	scraped  bool
}

// reportTenantLedgerFailure turns the master's refusal into the two conditions criterion 11 names.
//
// The distinction is the point. Multi-tenancy off is a BACKEND to reconfigure and nothing this pool
// can converge; a policy source that cannot be rewritten is a MOUNT to fix and the quota would
// otherwise read as one that simply will not apply. Everything else is neither, and says so rather
// than borrowing one of their names.
//
// BOTH axes are written on every outcome, and that is what makes the report level-based. The holder
// starts from the status of the last pass, so an axis this pass does not touch keeps whatever the
// previous one concluded — and these two faults exclude each other. A pass that saw the policy source
// refuse a write, on top of a pass that had seen no ledger at all, would leave MultiTenancyDisabled
// standing next to it: summarizeKVCachePool scans the axes in order, so the pool would report a
// backend to reconfigure when this pass's own answer proves the ledger is there. The reverse ordering
// leaves the mirror image. An axis this pass could not observe is set Unknown rather than left alone,
// because "not observed by this pass" and "observed to be fine" are not the same sentence.
func (r *KVCachePoolReconciler) reportTenantLedgerFailure(
	holder *workercore.KVCachePool, err error, doing string,
) {
	switch {
	case errors.Is(err, mooncake.ErrMultiTenancyDisabled):
		KVCachePoolConditionQuotaLedgerAvailable.False(holder,
			KVCachePoolReasonMultiTenancyDisabled,
			fmt.Sprintf("the master refused %s because it runs without multi-tenancy: it holds no "+
				"tenant ledger, and every request falls into one default tenant where two reuse "+
				"domains read each other's blocks. Set "+
				`"spec.connection.managed.leader.multiTenancy" on the backend`, doing))
		KVCachePoolConditionQuotaPolicyWritable.Unknown(holder,
			KVCachePoolReasonMultiTenancyDisabled,
			"a master with no tenant ledger accepts no quota to persist, so whether its quota policy "+
				"source is writable was not observed. Turn multi-tenancy on and this pass answers it")
	case errors.Is(err, mooncake.ErrQuotaPolicyNotWritable):
		// This refusal is itself the ledger's answer: the master took the request, found the tenant
		// ledger to write it into, and failed on the policy source underneath. Reporting the ledger
		// as anything but available here would contradict the very response being handled.
		KVCachePoolConditionQuotaLedgerAvailable.True(holder, "Available",
			"the master holds a tenant ledger; what it could not do was persist the policy")
		KVCachePoolConditionQuotaPolicyWritable.False(holder,
			KVCachePoolReasonQuotaPolicyNotWritable,
			fmt.Sprintf("the master could not persist %s and so did not apply it: its tenant quota "+
				"policy source is not writable. The quota is NOT in force — this master writes the "+
				"policy before applying it, so nothing was accepted. A restart will not help; the "+
				"policy source has to be made writable", doing))
	default:
		// Neither named fault. The ledger is the surface that did not answer, so the failure is
		// reported on ITS axis, and the policy source — which is only ever reached through a ledger
		// write that got a response — is left unobserved rather than blamed.
		KVCachePoolConditionQuotaLedgerAvailable.False(holder, "LedgerUnreachable",
			fmt.Sprintf("%s failed: %v", doing, err))
		KVCachePoolConditionQuotaPolicyWritable.Unknown(holder, "LedgerUnreachable",
			"the master's ledger did not answer, so whether it can persist a quota policy was not "+
				"observed")
	}
}

// observeKVCachePoolUsage sums what THIS pool's own tenants hold, out of the one scrape the pass
// already took.
//
// Never the master's whole ledger. A backend serves several pools (F7), and its exposition carries
// every tenant on it — publishing that here would make a pool's usage grow because somebody else's
// namespace wrote, which is the one reading this figure must never support.
//
// ABSENT when the scrape did not answer, and absent is not zero: a total serialized as 0 says the
// cache is empty, while what happened is that nobody could look. A domain the scrape does not mention
// contributes nothing rather than blocking the sum — its own Binding reports that gap on its own
// condition, and a pool total withheld because one of ten domains was missing would hide the nine.
func observeKVCachePoolUsage(holder *workercore.KVCachePool, ledger kvCachePoolLedgerPass) {
	if !ledger.scraped {
		holder.Status.Usage = nil
		return
	}

	// Zero is published rather than withheld once the scrape answered AND at least one of this pool's
	// domains was in it: the master was asked and said that domain holds nothing, which is a
	// measurement. A domain the exposition did not carry is not that — it is the same absent-is-not-
	// zero distinction the Binding keeps with TenantNotInLedger, and a pool that published 0 for it
	// would report "this pool holds nothing" for the ordinary state right after a master restart.
	var total int64
	var resolved int
	for i := range holder.Status.Domains {
		sample, known := ledger.metrics.Tenant(holder.Status.Domains[i].Name)
		if !known {
			continue
		}
		resolved++
		if occupancy, _ := sample.Occupancy(); occupancy != nil {
			total += *occupancy
		}
	}

	// A pool with no domains at all is the one case where zero is the answer rather than the absence
	// of one: nothing was asked for, so nothing is missing.
	if resolved == 0 && len(holder.Status.Domains) > 0 {
		holder.Status.Usage = nil
		return
	}

	holder.Status.Usage = &workercore.KVCachePoolUsage{Total: quantityFromBytes(&total)}
}

// observeAllocatableCapacity reads what the master has to divide between its tenants.
//
// Zero is the startup-ordering trap this exists for: no member has mounted, so every tenant's
// effective quota is zero and no write can succeed, while every object in the cluster still looks
// correctly configured and every quota this pool wrote was accepted.
// The scrape it took is RETURNED rather than dropped, because it is the same document every Binding
// of the pool reads its own tenant out of. One read, however many Bindings — a per-Binding scrape
// would put the pass's request count under the control of how many namespaces bound to the pool.
func (r *KVCachePoolReconciler) observeAllocatableCapacity(
	ctx context.Context, admin *mooncake.AdminClient, holder *workercore.KVCachePool,
) (mooncake.TenantQuotaMetrics, bool) {
	metrics, err := admin.TenantQuotaMetrics(ctx)
	if err != nil {
		KVCachePoolConditionCapacityAllocatable.False(holder, "CapacityNotObserved",
			fmt.Sprintf("the master's tenant metrics could not be read: %v", err))
		return mooncake.TenantQuotaMetrics{}, false
	}

	switch capacity := metrics.AllocatableCapacityBytes; {
	case capacity == nil:
		KVCachePoolConditionCapacityAllocatable.False(holder, "CapacityNotObserved",
			"the master's exposition carries no allocatable capacity")
	case *capacity == 0:
		// The reason states the reading, not a cause. Two states produce it and this scrape cannot
		// tell them apart: members that have not mounted their segments yet, and a leader that has
		// restarted and is answering before its own view has reconverged — measured at roughly 30 s
		// on a single-master restart. Naming either one would send an operator to the wrong place
		// half the time.
		KVCachePoolConditionCapacityAllocatable.False(holder, "NothingToAllocate",
			"the master reports nothing to allocate, so every reuse domain's effective quota is "+
				"zero and no write can succeed. Its members have either not mounted their segments "+
				"yet, or not finished remounting them after the master restarted")
	default:
		KVCachePoolConditionCapacityAllocatable.True(holder, "Allocatable",
			fmt.Sprintf("the master has %d bytes to divide between its reuse domains", *capacity))
	}

	// Returned even when the capacity gauge was missing or zero. Those say something about the
	// MASTER; the per-tenant series in the same document are still what they are, and a Binding whose
	// own figures arrived should report them rather than inherit the pool's verdict.
	return metrics, true
}

// The condition types a KVCachePoolBinding reports. Spelled positively, like the pool's.
const (
	// KVCachePoolBindingConditionDomainExclusive is False when another Binding claims the same reuse
	// domain — reason DomainClaimedByMultipleBindings, naming the other claimant.
	KVCachePoolBindingConditionDomainExclusive kubeapistatus.ConditionType = "DomainExclusive"
	// KVCachePoolBindingConditionQuotaObserved is False when the master's figures for this domain
	// could not be read, and True even when the master has never heard of the domain — that is an
	// observation, not a failure to observe.
	KVCachePoolBindingConditionQuotaObserved kubeapistatus.ConditionType = "QuotaObserved"
	// KVCachePoolBindingConditionQuotaGranted is False when the observed grant cannot serve a write:
	// no ledger entry, no exported figure, or a grant of zero. It is separate from QuotaObserved
	// because a grant of zero is a successful observation — the pair distinguishes "we could not
	// look" from "we looked, and there is nothing to write into".
	//
	// Without it a Binding reached Ready on observation alone, so a domain whose grant was zero — the
	// ordinary state for tens of seconds after its master restarts — reported Ready while no write
	// could succeed. That is precisely the reading a Binding exists to prevent.
	KVCachePoolBindingConditionQuotaGranted kubeapistatus.ConditionType = "QuotaGranted"
	// KVCachePoolBindingConditionReleasable is False while a workload in this namespace still holds
	// the pool through this Binding. Absent from the axes summarizeKVCachePoolBinding reads, for the
	// reason the pool's own is.
	KVCachePoolBindingConditionReleasable kubeapistatus.ConditionType = "Releasable"
)

const (
	// KVCachePoolBindingReasonDomainClaimedByMultipleBindings is the F9 race, named by the spec and
	// asserted by an e2e case.
	KVCachePoolBindingReasonDomainClaimedByMultipleBindings = "DomainClaimedByMultipleBindings"
	// KVCachePoolBindingReasonHeldByWorkloads is a Binding whose deletion cannot proceed: it is the
	// grant, and releasing it under a workload would leave that workload writing into a pool
	// nothing records it as using.
	KVCachePoolBindingReasonHeldByWorkloads = "HeldByWorkloads"
	// KVCachePoolBindingReasonLedgerNotReleased is a Binding nothing in the cluster holds any more,
	// waiting on the MASTER: it refuses to drop a quota whose domain still holds objects. It is
	// separate from HeldByWorkloads because the action is different — drain the domain, rather than
	// stop the workloads — and because this one is the only hold no object in the cluster explains.
	KVCachePoolBindingReasonLedgerNotReleased = "LedgerNotReleased"
)

// syncKVCachePoolBindings writes each Binding's own figures from the ONE scrape the pass took.
//
// Nothing here is summed. Every figure comes from the single series bearing that Binding's own
// spec.domain.name, because the tenant IS the reuse domain — a Binding that added two series
// together would be reporting another namespace's consumption as its own.
//
// A scrape that did not happen is a pass that could not read the master. The previous figures are
// then left exactly where they are and only the Condition moves: zeroing them would turn "we could
// not look" into "you are using nothing", which is the reading that sends somebody hunting for a
// cache that evaporated.
//
// This is also where each Binding's finalizer goes on and comes off. The Binding has no reconciler of
// its own — every event about one is mapped to the pool it names — so the lock is taken and released
// on the pool's pass, which is the same pass that has just converged the ledger those two acts have
// to be ordered against.
func (r *KVCachePoolReconciler) syncKVCachePoolBindings(
	ctx context.Context,
	kvcp *workercore.KVCachePool,
	master *kvCachePoolMaster,
	ledger kvCachePoolLedgerPass,
) error {
	logger := ctrllog.FromContext(ctx)

	for i := range master.bindings[kvcp.Name] {
		kvcpb := &master.bindings[kvcp.Name][i]

		if kvCachePoolBindingIsReleasing(kvcpb) {
			if err := r.releaseKVCachePoolBinding(ctx, kvcpb, ledger); err != nil {
				return err
			}
			continue
		}

		// Already locked, by lockKVCachePoolBindings, before this pass wrote anything to the master.
		// Locking here instead was the defect: the ledger and the policy file are converged above, so
		// a crash in between left a Binding with an entry on the master and no finalizer to delete it.

		holder := &workercore.KVCachePoolBinding{
			ObjectMeta: *kvcpb.ObjectMeta.DeepCopy(),
			Status:     *kvcpb.Status.DeepCopy(),
		}
		r.observeKVCachePoolBinding(holder, kvcpb, master, ledger.metrics, ledger.scraped)

		desired := summarizeKVCachePoolBinding(holder)
		if kubemeta.DeepEqual(desired, kvcpb.Status) {
			continue
		}
		kvcpb.Status = desired
		if err := r.Client.Status().Update(ctx, kvcpb); err != nil {
			// The whole pass gives up, rather than carrying on to the siblings. A failure here is
			// almost always a conflict, which says this Binding was changed underneath the snapshot
			// every OTHER Binding in this loop is also being written from — so continuing would be
			// writing the rest from a listing already known to be stale. The requeue re-lists and
			// re-scrapes, and the siblings get their figures a resync later from data that is
			// current. Delayed, not lost.
			logger.Error(err, "update kv cache pool binding status",
				"kv cache pool binding", ctrlcli.ObjectKeyFromObject(kvcpb))
			return err
		}
		logger.V(2).Info("refreshed kv cache pool binding status",
			"kv cache pool binding", ctrlcli.ObjectKeyFromObject(kvcpb), "phase", desired.Phase)
	}

	return nil
}

// observeKVCachePoolBinding fills one Binding's figures in place.
func (r *KVCachePoolReconciler) observeKVCachePoolBinding(
	holder, kvcpb *workercore.KVCachePoolBinding,
	master *kvCachePoolMaster,
	metrics mooncake.TenantQuotaMetrics,
	scraped bool,
) {
	domain := kvcpb.Spec.Domain.Name

	if kvcpb.DeletionTimestamp != nil {
		// Reached only for a Binding something still holds — a released one never gets this far. The
		// figures below are still written for it: the workload is still reading and writing through
		// this domain, and a deletion nobody has been able to act on does not make its quota stale.
		KVCachePoolBindingConditionReleasable.False(holder,
			KVCachePoolBindingReasonHeldByWorkloads,
			fmt.Sprintf("deletion is held: %s in this namespace still hold(s) the pool through this "+
				"binding. Releasing it would leave a workload writing into a pool nothing records it "+
				"as using", formatKVCacheConsumers(kvcpb.Status.UsedBy)))
	}

	if others := otherClaimants(master.contested[domain], kvcpb); len(others) > 0 {
		// Managed for NEITHER Binding, so neither reports figures it is not the owner of. The other
		// claimant is named because the two are in different namespaces: without the name, each side
		// sees a domain it cannot use and no way to find out who has it.
		KVCachePoolBindingConditionDomainExclusive.False(holder,
			KVCachePoolBindingReasonDomainClaimedByMultipleBindings,
			fmt.Sprintf("reuse domain %q is also claimed by %s, so it is managed for neither: one "+
				"ledger entry cannot carry two namespaces' quotas. Admission refuses a second claim, "+
				"so this is two creates that raced it — delete one of them", domain,
				strings.Join(others, ", ")))
		return
	}
	KVCachePoolBindingConditionDomainExclusive.True(holder, "Exclusive",
		fmt.Sprintf("reuse domain %q is claimed by this binding alone", domain))

	if !scraped {
		// Every figure already on the object stays. See this function's caller.
		KVCachePoolBindingConditionQuotaObserved.False(holder, "MasterNotScraped",
			fmt.Sprintf("the master's figures for reuse domain %q could not be read this pass; the "+
				"ones below are from the last pass that could", domain))
		return
	}

	sample, known := metrics.Tenant(domain)
	if !known {
		// The master has never heard of this domain. It is an OBSERVATION — so the axis is True and
		// the figures are cleared rather than left stale.
		holder.Status.RequestedQuota, holder.Status.EffectiveQuota = nil, nil
		holder.Status.Usage, holder.Status.OverQuota = nil, nil

		KVCachePoolBindingConditionQuotaObserved.True(holder, "TenantNotInLedger",
			fmt.Sprintf("the master reports no figures for reuse domain %q: it carries no entry for "+
				"it yet. This is not a quota of zero — no quota has been observed at all", domain))
		KVCachePoolBindingConditionQuotaGranted.False(holder, "NoLedgerEntry",
			fmt.Sprintf("the master carries no ledger entry for reuse domain %q, so nothing is "+
				"granted to it and no write can succeed", domain))
		return
	}

	holder.Status.RequestedQuota = quantityFromBytes(sample.RequestedBytes)
	holder.Status.EffectiveQuota = quantityFromBytes(sample.EffectiveBytes)
	holder.Status.OverQuota = sample.OverQuota

	occupancy, inflight := sample.Occupancy()
	holder.Status.Usage = quantityFromBytes(occupancy)

	KVCachePoolBindingConditionQuotaObserved.True(holder, "Observed",
		quotaObservedMessage(domain, sample.ExplicitPolicy, inflight))

	// Read off the figure rather than off the pool's capacity: a Binding reports its own tenant, and
	// the two can differ. nil and zero are kept apart because they are different readings — the
	// master not exporting the series at all is not the master granting nothing.
	switch {
	case sample.EffectiveBytes == nil:
		// DEFENSIVE, and no master version is known to produce it: a truncated exposition is refused
		// whole by the decoder, and every version seen exports a tenant's requested and effective
		// series from one loop. The nil check is still required — the figure is a pointer and the
		// comparison below would panic — so the branch carries its own reason rather than being
		// folded into the one below, which would report a grant of zero that was never read.
		KVCachePoolBindingConditionQuotaGranted.False(holder, "GrantNotExported",
			fmt.Sprintf("the master's figures for reuse domain %q carry no effective quota, so "+
				"whether anything is granted to it is unknown", domain))
	case *sample.EffectiveBytes == 0:
		// Says what was read and where the cause would be visible, without claiming to have seen
		// one: this reads the per-tenant series and never the pool's capacity gauge, so it cannot
		// tell a master with nothing mounted from a share that a proportional recut took to zero.
		KVCachePoolBindingConditionQuotaGranted.False(holder, "ZeroGranted",
			fmt.Sprintf("the master grants reuse domain %q zero bytes, so no write can succeed. "+
				"Whether its pool has nothing to allocate, or divided what it has and left this "+
				"domain nothing, is on the pool's own CapacityAllocatable", domain))
	default:
		KVCachePoolBindingConditionQuotaGranted.True(holder, "Granted",
			fmt.Sprintf("the master grants reuse domain %q %d bytes", domain, *sample.EffectiveBytes))
	}
}

// quotaObservedMessage says what the figures mean, which varies by more than their values.
func quotaObservedMessage(domain string, explicit *bool, inflight bool) string {
	parts := make([]string, 0, 3)

	switch {
	case explicit != nil && !*explicit:
		// Asked for, and the master says it is running without one. The write has not landed yet, or
		// it was refused — the pool's own conditions carry which.
		parts = append(parts, fmt.Sprintf("a quotaCeiling is set, but the master reports reuse "+
			"domain %q as running without an explicit policy; see the pool's conditions", domain))
	default:
		parts = append(parts, fmt.Sprintf("the master's figures for reuse domain %q were read", domain))
	}

	if inflight {
		parts = append(parts, "usage includes writes that have not committed: this master reports one "+
			"occupancy figure charged at the start of a write, with nothing to subtract")
	}

	return strings.Join(parts, "; ")
}

// otherClaimants names every claimant of a contested domain except the Binding being reported on.
func otherClaimants(
	claims []workercore.KVCachePoolBindingReference, kvcpb *workercore.KVCachePoolBinding,
) []string {
	others := make([]string, 0, len(claims))
	for _, claim := range claims {
		if claim.Namespace == kvcpb.Namespace && claim.Name == kvcpb.Name {
			continue
		}
		others = append(others, claim.Namespace+"/"+claim.Name)
	}
	slices.Sort(others)
	return others
}

// quantityFromBytes turns an observed byte count into the quantity status carries, preserving the
// distinction the pointer exists for: nothing observed stays nothing published.
func quantityFromBytes(bytes *int64) *resource.Quantity {
	if bytes == nil {
		return nil
	}
	// BinarySI so a figure that is a whole number of MiB reads as one. The master's counts are byte
	// counts, and DecimalSI would render 1073741824 rather than 1Gi.
	return resource.NewQuantity(*bytes, resource.BinarySI)
}

// summarizeKVCachePoolBinding derives the Binding's phase from its conditions, the same way the
// pool's is derived from its own: a fault first, then an axis nobody has looked at yet.
func summarizeKVCachePoolBinding(
	holder *workercore.KVCachePoolBinding,
) workercore.KVCachePoolBindingStatus {
	status := holder.Status

	// A deletion decides the phase before any axis does, and Releasable is deliberately not one of
	// them: an axis that only exists on the way out would read as unobserved — Provisioning — on every
	// live object.
	if holder.DeletionTimestamp != nil {
		status.Phase = KVCachePoolPhaseDeleting
		status.PhaseMessage = KVCachePoolBindingConditionReleasable.GetMessage(holder)
		return status
	}

	// QuotaGranted comes after QuotaObserved so a pass that could not read the master reports that,
	// rather than reporting the grant it therefore could not see.
	axes := []kubeapistatus.ConditionType{
		KVCachePoolBindingConditionDomainExclusive,
		KVCachePoolBindingConditionQuotaObserved,
		KVCachePoolBindingConditionQuotaGranted,
	}

	for _, axis := range axes {
		if axis.Exists(holder) && !axis.IsTrue(holder) {
			status.Phase, status.PhaseMessage = KVCachePoolPhaseError, axis.GetMessage(holder)
			return status
		}
	}
	for _, axis := range axes {
		if !axis.Exists(holder) {
			status.Phase, status.PhaseMessage = KVCachePoolPhaseProvisioning,
				fmt.Sprintf("%s has not been observed yet", axis)
			return status
		}
	}

	status.Phase, status.PhaseMessage = KVCachePoolPhaseReady,
		KVCachePoolBindingConditionQuotaObserved.GetMessage(holder)
	return status
}

// summarizeKVCachePool derives the phase from the conditions.
//
// Everything that holds a pool away from Ready is a condition first, so this reads them rather than
// re-deriving anything: a fault that reports one way in a condition and another way in the phase is
// how an operator learns to trust neither.
func (r *KVCachePoolReconciler) summarizeKVCachePool(
	holder *workercore.KVCachePool,
) workercore.KVCachePoolStatus {
	status := holder.Status

	axes := []kubeapistatus.ConditionType{
		KVCachePoolConditionBackendResolved,
		KVCachePoolConditionQuotaLedgerAvailable,
		KVCachePoolConditionQuotaPolicyWritable,
		KVCachePoolConditionCapacityAllocatable,
	}

	// A fault is looked for FIRST, across every axis, and an unobserved axis only after that. The
	// order matters because one fault leaves the axes after it unobserved — a pass that stopped at
	// the first ledger refusal never reached the capacity read — and scanning in axis order would
	// then report "capacity has not been observed yet" for a pool whose actual problem is a master
	// with no ledger, which is the sentence an operator needs.
	for _, axis := range axes {
		if axis.Exists(holder) && !axis.IsTrue(holder) {
			status.Phase, status.PhaseMessage = KVCachePoolPhaseError, axis.GetMessage(holder)
			return status
		}
	}
	for _, axis := range axes {
		if !axis.Exists(holder) {
			status.Phase, status.PhaseMessage = KVCachePoolPhaseProvisioning,
				fmt.Sprintf("%s has not been observed yet", axis)
			return status
		}
	}

	status.Phase, status.PhaseMessage = KVCachePoolPhaseReady,
		"the master holds every quota this pool asked for"
	return status
}

// syncKVCachePoolStatus writes the status wholesale, and only on a difference.
//
// The guard is what keeps the loop from feeding itself: this reconciler's predicate ignores its own
// status writes, and a settled pool writes nothing at all on top of that.
func (r *KVCachePoolReconciler) syncKVCachePoolStatus(
	ctx context.Context, kvcp *workercore.KVCachePool, desired workercore.KVCachePoolStatus,
) error {
	if kubemeta.DeepEqual(desired, kvcp.Status) {
		return nil
	}

	logger := ctrllog.FromContext(ctx)

	kvcp.Status = desired
	if err := r.Client.Status().Update(ctx, kvcp); err != nil {
		logger.Error(err, "update kv cache pool status")
		return err
	}
	logger.V(2).Info("refreshed kv cache pool status", "phase", desired.Phase)
	return nil
}

// teardownKVCachePool releases the pool once nothing holds it, and refuses while something does.
//
// Two holders, refused for different reasons and reported differently because of it.
//
// A Binding still referencing the pool is a grant that has not been withdrawn: releasing under one
// would leave a namespace holding a quota on a pool that no longer exists. That refusal is a
// status write and nothing else — a Binding going away wakes this reconcile through the watch, so a
// timer would only be guessing at when.
//
// A master that will not give up an entry this pool registered is capacity nobody could see
// afterwards: the ledger carries no label saying whose an entry is, so once this object is gone
// nothing left in the cluster knows to delete it. That refusal IS timed, because the master is not an
// object this reconciler watches and nothing in the cluster changes when it recovers.
//
// The Bindings this pass can already let go of are released BEFORE either refusal is considered, and
// that ordering is load-bearing rather than tidy. A Binding's lock comes off in the serving pass, and
// a pool marked for deletion never reaches one — so deleting a pool and its Bindings together, which
// is the ordinary way a stack goes, would otherwise deadlock: the pool waiting for Bindings whose
// only release path it has just stopped taking.
//
// What a deleting pool does NOT do is keep observing. The Bindings it is still held by stop having
// their figures refreshed, which is the accepted cost of the teardown being a path of its own: the
// pool has been asked to go, and the Condition below names every Binding standing in the way.
func (r *KVCachePoolReconciler) teardownKVCachePool(
	ctx context.Context, kvcp *workercore.KVCachePool,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	holder := &workercore.KVCachePool{
		ObjectMeta: *kvcp.ObjectMeta.DeepCopy(),
		Status:     *kvcp.Status.DeepCopy(),
	}
	deleting := func(result ctrl.Result) (ctrl.Result, error) {
		return result, r.syncKVCachePoolStatus(ctx, kvcp, deletingKVCachePoolStatus(holder))
	}

	bindings := new(workercore.KVCachePoolBindingList)
	err := r.Client.List(ctx, bindings,
		ctrlcli.MatchingFields{IndexingKVCachePoolBindingByPool: kvcp.Name})
	if err != nil {
		logger.Error(err, "list the bindings of a pool being deleted")
		return ctrl.Result{}, err
	}

	// Resolved once, for every removal below. A nil client is a master that is GONE rather than one
	// that could not be reached, and the difference decides whether anything is still owed to it.
	admin, kvcb, unreachable, err := r.resolveKVCachePoolAdmin(ctx, kvcp, holder)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Observed once, for the ownership rule below. Taken only when there is a master to ask: a nil
	// client is one that is gone, and an unreachable one has already turned every removal below into
	// a hold. A snapshot that cannot be TAKEN is an error rather than a pass that proceeds without
	// one, on the same terms as the ledger listing inside deleteTenantQuotas — deleting an entry
	// without knowing whose it is is the loss this rule exists to prevent.
	var master *kvCachePoolMaster
	if admin != nil {
		if master, err = r.observeKVCachePoolMaster(ctx, kvcb); err != nil {
			logger.Error(err, "observe the master while tearing down a pool")
			return ctrl.Result{}, err
		}
	}

	// removable narrows a list of domain names to the ones this pool may actually remove.
	//
	// The names a teardown gathers are the ones this pool NAMED, and one master serves several
	// pools. The serving path already refuses to touch an entry that is not this pool's — the ledger
	// convergence takes a contested domain out of what it converges, and releaseQuotaPolicyOfPool
	// keeps a sibling's tenant in the document it re-renders — but THIS path deleted by name. A
	// domain two Bindings raced, or one a sibling pool owns, was therefore removed from the ledger by
	// whichever pool happened to be deleted first, taking the live claimant's quota and the cache
	// under it. Three surfaces, and they have to decide ownership by one rule or the pool that goes
	// first decides for the others.
	//
	// A domain claimed by NOBODY stays removable, and that is not an oversight: it is a Binding
	// force-deleted past its own release, where this pool's status.domains is the last record in the
	// cluster that the entry exists.
	removable := func(names []string) []string {
		if master == nil {
			return names
		}
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if _, contested := master.contested[name]; contested {
				logger.V(2).Info("leaving a contested reuse domain to the claimant that keeps it",
					"domain", name)
				continue
			}
			if claim, claimed := master.domains[name]; claimed && claim.pool != kvcp.Name {
				logger.V(2).Info("leaving a reuse domain a sibling pool owns",
					"domain", name, "owner", claim.pool)
				continue
			}
			kept = append(kept, name)
		}
		return kept
	}

	var releasing, holding []*workercore.KVCachePoolBinding
	for i := range bindings.Items {
		kvcpb := &bindings.Items[i]
		if kvCachePoolBindingIsReleasing(kvcpb) {
			releasing = append(releasing, kvcpb)
			continue
		}
		holding = append(holding, kvcpb)
	}

	if len(releasing) > 0 {
		if unreachable {
			return deleting(ctrl.Result{RequeueAfter: kvCachePoolObserveInterval})
		}
		names := make([]string, 0, len(releasing))
		for _, kvcpb := range releasing {
			names = append(names, kvcpb.Spec.Domain.Name)
		}
		if r.deleteTenantQuotas(ctx, admin, removable(names), holder) {
			return deleting(ctrl.Result{RequeueAfter: kvCachePoolObserveInterval})
		}
		for _, kvcpb := range releasing {
			if err = r.unlockKVCachePoolBinding(ctx, kvcpb); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if len(holding) > 0 {
		names := make([]string, 0, len(holding))
		for _, kvcpb := range holding {
			names = append(names, kvcpb.Namespace+"/"+kvcpb.Name)
		}
		KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonHeldByBindings,
			fmt.Sprintf("deletion is held: %s still reference(s) this pool. A binding is a namespace's "+
				"grant, and releasing the pool under one would leave that namespace holding a quota "+
				"on a pool that no longer exists", listBoundedNames(names)))
		return deleting(ctrl.Result{})
	}

	// Whatever the Bindings did not take with them. In the ordinary case this is empty; what reaches
	// it is a Binding force-deleted past its own release, leaving this pool the last object that knows
	// its entry exists.
	if len(kvcp.Status.Domains) > 0 {
		if unreachable {
			return deleting(ctrl.Result{RequeueAfter: kvCachePoolObserveInterval})
		}
		names := make([]string, 0, len(kvcp.Status.Domains))
		for i := range kvcp.Status.Domains {
			names = append(names, kvcp.Status.Domains[i].Name)
		}
		if r.deleteTenantQuotas(ctx, admin, removable(names), holder) {
			return deleting(ctrl.Result{RequeueAfter: kvCachePoolObserveInterval})
		}
	}

	// The rendered document goes with the entries, and for the same reason they do: a tenant this
	// pool created has to stop being written down anywhere before the pool that owns it is gone. It
	// is skipped only when the backend itself is gone, which takes the ConfigMap with it through the
	// owner reference.
	//
	// A master that cannot be reached HOLDS instead. The re-render is whole-document and master-wide,
	// so writing one without reading the ledger would publish a seed built from a guess about what
	// the siblings on this backend still hold — the same reason deleteTenantQuotas refuses to remove
	// entries it could not list first.
	if kvcb != nil {
		if unreachable {
			return deleting(ctrl.Result{RequeueAfter: kvCachePoolObserveInterval})
		}
		if err = r.releaseQuotaPolicyOfPool(ctx, admin, kvcb, kvcp); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The claim goes AFTER the entries and BEFORE this pool's own lock. After, because a backend
	// released while an entry of this pool's is still on its master could be deleted with the entry on
	// it. Before, because a pool that dropped its own lock first would leave the claim behind with
	// nothing in the cluster left to remove it, and the backend held forever by a pool that is gone.
	if err = r.releaseKVCachePoolBackendClaim(ctx, kvcp); err != nil {
		return ctrl.Result{}, err
	}

	if systemmeta.Unlock(kvcp) {
		return ctrl.Result{}, nil
	}
	if err = r.Client.Update(ctx, kvcp); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	logger.V(2).Info("released kv cache pool")
	return ctrl.Result{}, nil
}

// deletingKVCachePoolStatus is the status of a pool on its way out.
//
// The phase is decided here rather than by summarizeKVCachePool for the reason Releasable is not one
// of that function's axes: the four axes describe a pool that is being SERVED, and a teardown does not
// re-observe any of them. Left to the summary, a pool held by a Binding would report whatever fault
// the last serving pass happened to see.
func deletingKVCachePoolStatus(holder *workercore.KVCachePool) workercore.KVCachePoolStatus {
	status := holder.Status
	status.Phase = KVCachePoolPhaseDeleting
	status.PhaseMessage = KVCachePoolConditionReleasable.GetMessage(holder)
	return status
}

// resolveKVCachePoolAdmin finds the master a pool being torn down still owes entries to.
//
// A nil client with unreachable FALSE is a master that is GONE, and nothing is owed: the backend took
// its ledger with it, and a pool that waited for one would be undeletable for as long as the backend
// stayed gone — which is the ordinary order a stack comes down in. The same answer covers a pool that
// never named a usable backend, because it registered nothing.
//
// Unreachable TRUE is a backend that exists and publishes no address. That one holds, with the
// Condition already written: the entries may well still be there, and an entry left behind is
// capacity nothing can reclaim — the ledger does not record which pool created it.
// The backend is returned alongside the client because the teardown owes it two different removals:
// the ledger entries, which need the address, and the rendered policy document, which needs only the
// object. A backend that is GONE returns nil for both — its ConfigMap went with it, through the
// owner reference.
func (r *KVCachePoolReconciler) resolveKVCachePoolAdmin(
	ctx context.Context, kvcp, holder *workercore.KVCachePool,
) (admin *mooncake.AdminClient, backend *workercore.KVCacheBackend, unreachable bool, err error) {
	logger := ctrllog.FromContext(ctx)

	kvcb, resolveErr := r.resolveKVCachePoolBackend(ctx, kvcp)
	if resolveErr != nil {
		if resolveErr.internal != nil {
			return nil, nil, false, resolveErr.internal
		}
		// Only a backend that is GONE owes nothing. The other way to fail this resolve is
		// BackendNotSingular, and a pool naming two backends is not a pool whose master vanished —
		// one of the names can be a live master still holding the entries this pool registered
		// while the field was singular, or before a write went around the webhook. Treated as gone,
		// the teardown finds a nil client, calls the ledger settled, drops the backend claim and
		// takes the finalizer off: the entries stay, and the ledger does not record which pool
		// created them, so nothing can ever reclaim that capacity. Held here instead, on the same
		// terms as an unreachable admin address below, because it is the same loss.
		if !resolveErr.gone {
			KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonLedgerNotReleased,
				fmt.Sprintf("deletion is held: this pool's backend cannot be resolved (%s), so the "+
					"reuse domains it registered cannot be removed from any ledger. %s. An entry "+
					"left behind is capacity nothing can reclaim, because the ledger does not "+
					"record which pool created it",
					resolveErr.reason, resolveErr.message))
			return nil, nil, true, nil
		}
		logger.V(2).Info("tearing down a pool with no master left to ask",
			"reason", resolveErr.reason)
		return nil, nil, false, nil
	}

	_, adminAddress := kvCacheBackendAddresses(kvcb)
	if adminAddress == "" {
		KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonLedgerNotReleased,
			fmt.Sprintf("deletion is held: backend %q publishes no %q address, so the reuse domains "+
				"registered on it cannot be removed from its ledger. An entry left behind is capacity "+
				"nothing can reclaim, because the ledger does not record which pool created it",
				kvcb.Name, workercore.KVCacheBackendEndpointNameAdmin))
		return nil, kvcb, true, nil
	}

	return &mooncake.AdminClient{Address: adminAddress, HTTP: r.AdminHTTP}, kvcb, false, nil
}

// releaseQuotaPolicyOfPool re-renders one master's policy document without the pool that is going.
//
// The ledger is not the only place a pool's tenants are written. This ConfigMap is rendered by THIS
// controller but OWNED by the backend, so nothing collects it when the pool goes — and the leader's
// seed container copies it back over the master's own file on every container start. A pool released
// without this step leaves entries for reuse domains no object in the cluster claims, and the master
// rebuilds their usage from an empty index on restart, so each one comes back as a quota with zero
// usage: the shape of a perfectly healthy tenant. Nothing observable distinguishes them afterwards,
// which is why preventing them here is the only defense rather than a tidy-up.
//
// The document is rebuilt from the LEDGER as well as from the snapshot, because the snapshot alone
// is not the set of tenants that should stay. observeKVCachePoolMaster deliberately leaves two kinds
// out of master.tenants — a contested domain, and one whose Binding is releasing — and the serving
// path puts them back through withContestedTenants/withRetainedTenants before it writes. This path
// owes them the same treatment for the same reason, and it is not this pool's own teardown that makes
// them fragile: they belong to SIBLING pools on the same backend, whose reconcile is not running here.
// Rendering from the snapshot alone drops a live sibling's entry from a whole-document write, and the
// next leader restart seeds the master without its quota.
func (r *KVCachePoolReconciler) releaseQuotaPolicyOfPool(
	ctx context.Context,
	admin *mooncake.AdminClient,
	kvcb *workercore.KVCacheBackend,
	kvcp *workercore.KVCachePool,
) error {
	master, err := r.observeKVCachePoolMaster(ctx, kvcb)
	if err != nil {
		return err
	}

	tenants := make([]mooncake.QuotaPolicyTenant, 0, len(master.tenants))
	kept := make(map[string]struct{}, len(master.tenants))
	for _, tenant := range master.tenants {
		// A domain THIS pool registered goes with it; one a sibling registered stays. That is the
		// same ownership rule the ledger removal follows, and it has to be, or the two surfaces
		// would disagree about who a tenant belongs to. A domain claimed by nobody in the map is
		// left alone, which is the conservative side of the choice.
		if claim, ok := master.domains[tenant.Name]; ok && claim.pool == kvcp.Name {
			continue
		}
		tenants = append(tenants, tenant)
		kept[tenant.Name] = struct{}{}
	}

	// A listing that fails HOLDS the teardown rather than writing a document built from half of what
	// the master holds, which is the same refusal deleteTenantQuotas makes for the same reason.
	observed, err := admin.ListTenantQuotas(ctx)
	if err != nil {
		return fmt.Errorf("list tenant quotas before re-rendering the quota policy: %w", err)
	}
	for i := range observed {
		entry := &observed[i]
		if _, already := kept[entry.TenantID]; already {
			continue
		}
		// Only an EXPLICIT policy is a line in this document. One running under the master's default
		// is a tenant this operator never wrote, and writing a figure for it here would create the
		// explicit policy it does not have — the same gate the ledger's own PUT and DELETE apply.
		if !entry.HasExplicitPolicy {
			continue
		}
		// Everything this pool registered is on its way out, whether or not the DELETE above reached
		// it: an entry still listed because it is draining must not be seeded back by the pool that
		// is leaving.
		if claim, ok := master.domains[entry.TenantID]; ok && claim.pool == kvcp.Name {
			continue
		}
		if kvCachePoolRegisteredBy(master, entry.TenantID, kvcp.Name) {
			continue
		}
		tenants = append(tenants, mooncake.QuotaPolicyTenant{
			Name: entry.TenantID,
			// The REQUESTED figure the master itself holds, never a claimant's: a contested domain
			// has two claimants and neither one's ceiling is the one to seed, and a releasing
			// Binding's entry is reproduced rather than recomputed.
			Quota: *resource.NewQuantity(entry.RequestedBytes, resource.BinarySI),
		})
	}

	// One order for one state, so a document rebuilt here diffs cleanly against the one the serving
	// path writes.
	slices.SortFunc(tenants, func(a, b mooncake.QuotaPolicyTenant) int {
		return strings.Compare(a.Name, b.Name)
	})

	// Written even when nothing is left, rather than deleted. An empty document is exactly what the
	// seed container falls back to when the ConfigMap is absent, so the two paths leave the master
	// in the same state — and deleting an object this controller does not own is a wider act than
	// this one needs.
	return r.syncQuotaPolicyConfigMap(ctx, kvcb, tenants)
}

// kvCachePoolRegisteredBy reports whether one of the named pool's Bindings carries this domain.
//
// master.domains answers that for a domain with a single live claimant and for nobody else: a
// contested one is deleted from the map, and a releasing Binding never enters it. Both are exactly
// the cases the re-render has to decide, so the Bindings are read directly.
func kvCachePoolRegisteredBy(master *kvCachePoolMaster, domain, pool string) bool {
	for i := range master.bindings[pool] {
		if master.bindings[pool][i].Spec.Domain.Name == domain {
			return true
		}
	}
	return false
}

// deleteTenantQuotas removes the named entries and reports whether any of them held.
//
// The names are always a list this pool REGISTERED, never everything the master holds: one master
// serves several pools (F7) and its ledger says nothing about whose an entry is, so deleting by
// anything wider would take a sibling pool's tenants with it.
//
// A domain that still holds objects is the third meaning of 409 and gets its own sentence: it is not
// a fault, the entry goes once the domain has drained, and reading it as the multi-tenancy refusal
// would put a false Condition on the one call that decides whether an object can be released.
//
// The EXPLICIT-POLICY gate is the same one convergence applies, and it has to be: this operator only
// ever writes explicit policies, so only an explicit policy is its to remove. Without it a teardown
// removes an entry running under the master's default — a tenant this operator adopted by name but
// never created — where the serving path would have left it alone. Two paths deleting by different
// rules is the defect, not the rule itself.
func (r *KVCachePoolReconciler) deleteTenantQuotas(
	ctx context.Context, admin *mooncake.AdminClient, domains []string, holder *workercore.KVCachePool,
) (held bool) {
	logger := ctrllog.FromContext(ctx)

	// A nil client is a master that is GONE, not one that could not be reached, and nothing is owed to
	// a ledger that no longer exists. resolveKVCachePoolAdmin's contract already says so; enforcing it
	// HERE rather than at each call site is what makes it true for every caller, because the callers
	// distinguish only `unreachable`, which is FALSE on exactly this path.
	if admin == nil {
		return false
	}

	// Read once, for the gate below. A listing that fails HOLDS the teardown rather than proceeding
	// blind: deleting without knowing which entries carry an explicit policy is exactly what this
	// gate exists to prevent, so an unanswered question must not be taken as permission.
	observed, err := admin.ListTenantQuotas(ctx)
	if err != nil {
		KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonLedgerNotReleased,
			fmt.Sprintf("deletion is held: the master's tenant ledger could not be read, so whether "+
				"its entries are this operator's to remove is unknown: %v", err))
		logger.Error(err, "list tenant quotas while tearing down a pool")
		return true
	}
	explicit := make(map[string]bool, len(observed))
	for i := range observed {
		explicit[observed[i].TenantID] = observed[i].HasExplicitPolicy
	}

	for _, domain := range domains {
		if present, ok := explicit[domain]; !ok || !present {
			// Already gone, or running under the master's default and never written by this operator.
			logger.V(2).Info("leaving a ledger entry that carries no explicit policy of ours",
				"tenant", domain)
			continue
		}
		err := admin.DeleteTenantQuota(ctx, domain)
		if err == nil {
			continue
		}
		if errors.Is(err, mooncake.ErrTenantNotEmpty) {
			KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonLedgerNotReleased,
				fmt.Sprintf("deletion is held: reuse domain %q still holds objects, so the master "+
					"refuses to remove its quota. The entry goes once the domain has drained", domain))
			return true
		}
		KVCachePoolConditionReleasable.False(holder, KVCachePoolReasonLedgerNotReleased,
			fmt.Sprintf("deletion is held: the master would not remove the quota of reuse domain "+
				"%q: %v", domain, err))
		logger.Error(err, "delete tenant quota while tearing down a pool", "tenant", domain)
		return true
	}

	return false
}

// releaseKVCachePoolBackendClaim drops this pool from its backend's usedBy, if the backend is still
// there to drop it from. A backend already gone needs nothing removed from it.
func (r *KVCachePoolReconciler) releaseKVCachePoolBackendClaim(
	ctx context.Context, kvcp *workercore.KVCachePool,
) error {
	kvcb, resolveErr := r.resolveKVCachePoolBackend(ctx, kvcp)
	if resolveErr != nil {
		if resolveErr.internal != nil {
			return resolveErr.internal
		}
		return nil
	}
	return r.releaseKVCacheBackend(ctx, kvcb, kvcp)
}

// releaseKVCachePoolBinding deletes a Binding's finalizer once the entry it owned is gone.
//
// The ORDER is the whole of it. convergeTenantLedger ran before this and found no Binding asking for
// that tenant, so the entry has already been deleted from the master; the finalizer comes off after,
// never before. A ledger that did not converge holds the release instead of forcing it — a Binding
// released over an entry still on the master leaves capacity nothing can reclaim, because the ledger
// records no owner and the object that knew is gone.
//
// No status is written on the way out. The object is about to stop existing, and a status write onto
// one whose last finalizer is coming off races the API server's own removal.
func (r *KVCachePoolReconciler) releaseKVCachePoolBinding(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding, ledger kvCachePoolLedgerPass,
) error {
	logger := ctrllog.FromContext(ctx).
		WithValues("kv cache pool binding", ctrlcli.ObjectKeyFromObject(kvcpb))

	if !ledger.converged {
		domain := kvcpb.Spec.Domain.Name

		// THIS domain is the one the master would not drop. A status is written, unlike on the release
		// below: the object is not about to stop existing — it waits for as long as the workload's
		// objects live — and without a reason it shows as a Binding stuck in Deleting with a blank
		// message, because the Deleting phase takes its message from this very condition.
		if slices.Contains(ledger.retained, domain) {
			logger.V(2).Info("holding a binding's release: its domain still holds objects")
			return r.holdKVCachePoolBindingRelease(ctx, kvcpb,
				fmt.Sprintf("deletion is held: the master will not remove the quota of reuse domain "+
					"%q while it still holds objects. Nothing in the cluster references this binding "+
					"any more — drain the domain and the release completes on the next pass", domain))
		}

		// A NON-EMPTY retained list means the removal loop ran to the end, so every entry it wanted
		// gone except those is gone — including this one. Holding it here would make one undrained
		// domain block the release of every sibling that drained cleanly, which is a deadlock built
		// out of two unrelated namespaces.
		if len(ledger.retained) > 0 {
			logger.V(2).Info("releasing a binding whose own entry is gone, on a pass another " +
				"domain held")
			return r.unlockKVCachePoolBinding(ctx, kvcpb)
		}

		// An EMPTY retained list on a non-converged pass is a ledger that failed outright — the
		// listing, a write, or a removal that was not the drain refusal. Whether this domain's entry
		// is gone is then unknown, so the finalizer holds; and it says so, because the alternative
		// is the same blank Deleting message for the far more common transient failure.
		logger.V(2).Info("holding a binding's release: the ledger did not converge this pass")
		return r.holdKVCachePoolBindingRelease(ctx, kvcpb,
			fmt.Sprintf("deletion is held: the master's tenant ledger did not answer this pass, so "+
				"whether the quota of reuse domain %q is gone cannot be established. The pool's own "+
				"conditions carry what the master said; the release retries every pass", domain))
	}

	return r.unlockKVCachePoolBinding(ctx, kvcpb)
}

// holdKVCachePoolBindingRelease says why a Binding nothing holds is still not going.
func (r *KVCachePoolReconciler) holdKVCachePoolBindingRelease(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding, message string,
) error {
	holder := &workercore.KVCachePoolBinding{
		ObjectMeta: *kvcpb.ObjectMeta.DeepCopy(),
		Status:     *kvcpb.Status.DeepCopy(),
	}
	KVCachePoolBindingConditionReleasable.False(holder,
		KVCachePoolBindingReasonLedgerNotReleased, message)

	desired := summarizeKVCachePoolBinding(holder)
	if kubemeta.DeepEqual(desired, kvcpb.Status) {
		return nil
	}
	kvcpb.Status = desired
	if err := r.Client.Status().Update(ctx, kvcpb); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// unlockKVCachePoolBinding takes a Binding's finalizer off, which is what finally lets it go.
//
// It writes no status. The object is about to stop existing, and a status write onto one whose last
// finalizer is coming off races the API server's own removal. A not-found means somebody else got
// there first, which is the outcome this was after.
func (r *KVCachePoolReconciler) unlockKVCachePoolBinding(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding,
) error {
	if systemmeta.Unlock(kvcpb) {
		return nil
	}

	logger := ctrllog.FromContext(ctx).
		WithValues("kv cache pool binding", ctrlcli.ObjectKeyFromObject(kvcpb))

	if err := r.Client.Update(ctx, kvcpb); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		logger.Error(err, "release kv cache pool binding")
		return err
	}
	logger.V(2).Info("released kv cache pool binding")
	return nil
}

// releaseOrphanKVCachePoolBindings lets go of the Bindings of a pool that no longer exists.
//
// Only a Binding already marked for deletion is touched. One that is not is left exactly as it is:
// the pool it names may be created a moment later, and this Binding's lock is what will make that
// pool's own teardown wait for it.
//
// No ledger entry is deleted, and none can be. The pool named the backend, so with the pool gone
// nothing says which master held the entry. The pool's own finalizer covers that path and refuses
// while any Binding remains, so reaching this means the pool was force-deleted — and what its
// Bindings registered is then the master's to reclaim.
func (r *KVCachePoolReconciler) releaseOrphanKVCachePoolBindings(ctx context.Context, pool string) error {
	logger := ctrllog.FromContext(ctx)

	bindings := new(workercore.KVCachePoolBindingList)
	err := r.Client.List(ctx, bindings, ctrlcli.MatchingFields{IndexingKVCachePoolBindingByPool: pool})
	if err != nil {
		logger.Error(err, "list the bindings of an absent pool", "pool", pool)
		return err
	}

	for i := range bindings.Items {
		kvcpb := &bindings.Items[i]
		if kvcpb.DeletionTimestamp == nil {
			continue
		}
		if err = r.unlockKVCachePoolBinding(ctx, kvcpb); err != nil {
			return err
		}
	}

	return nil
}

// claimKVCacheBackend records this pool in the backend's usedBy, which is what holds the backend's
// own teardown while a pool still draws on it.
//
// The entry carries an EMPTY namespace, and that is a value rather than an absence: both objects are
// cluster-scoped, so there is no namespace to name. This is the one list in this family that writes
// one, and an empty string has to survive being a list map key for the write to land at all.
//
// A write happens only when the list does not already say what it should. That guard is what keeps a
// settled pool from rewriting another controller's object on every pass — and, with the backend's own
// reconciler writing the same status, from turning every pass into a conflict.
func (r *KVCachePoolReconciler) claimKVCacheBackend(
	ctx context.Context, kvcb *workercore.KVCacheBackend, kvcp *workercore.KVCachePool,
) error {
	return r.syncKVCacheBackendClaim(ctx, kvcb, kvcp, true)
}

// releaseKVCacheBackend drops this pool's claim, so a backend deleted after its pools can finish.
func (r *KVCachePoolReconciler) releaseKVCacheBackend(
	ctx context.Context, kvcb *workercore.KVCacheBackend, kvcp *workercore.KVCachePool,
) error {
	return r.syncKVCacheBackendClaim(ctx, kvcb, kvcp, false)
}

func (r *KVCachePoolReconciler) syncKVCacheBackendClaim(
	ctx context.Context, kvcb *workercore.KVCacheBackend, kvcp *workercore.KVCachePool, claim bool,
) error {
	ref := workercore.KVCacheObjectReference{
		Kind:      KVCachePoolKind,
		Namespace: "",
		Name:      kvcp.Name,
	}

	at := slices.IndexFunc(kvcb.Status.UsedBy,
		func(e workercore.KVCacheObjectReference) bool { return e == ref })
	if claim == (at >= 0) {
		return nil
	}

	if claim {
		kvcb.Status.UsedBy = append(kvcb.Status.UsedBy, ref)
		// One order for one state, so a backend read by two pools does not have its status rewritten
		// because they claimed it in a different order than the last time.
		slices.SortFunc(kvcb.Status.UsedBy, compareKVCacheObjectReferences)
	} else {
		kvcb.Status.UsedBy = slices.Delete(kvcb.Status.UsedBy, at, at+1)
	}

	// A conflict is left to the queue rather than retried in place. The backend's reconciler writes
	// this status too, so the losing side's whole view of the backend is stale by then — retrying just
	// the list would write a claim derived from an object that has since moved.
	return r.Client.Status().Update(ctx, kvcb)
}

// formatKVCacheConsumers names the holders in a refusal message, because an operator whose delete is
// refused needs to know what to go and remove.
//
// Bounded, for the reason the backend's own is: usedBy has no item limit, it is written by whoever
// consumes the object, and this message is what carries the refusal. An unbounded join would take the
// refusal down with it — the delete would be held and the object would not say why.
func formatKVCacheConsumers(refs []workercore.KVCacheObjectReference) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Kind+"/"+ref.Name)
	}
	return listBoundedNames(names)
}

// enqueueKVCachePoolWhenBindingChanged maps a Binding onto the pool it names.
//
// The pool is cluster-scoped, so the request carries a bare name and no namespace — a Binding from
// any namespace enqueues the same pool, which is the whole point of the pool being the single
// reconciled object.
//
// A Binding naming a pool that does not exist still enqueues it. The pass then finds nothing and
// returns, which costs one no-op reconcile and keeps the Binding from being invisible if the pool is
// created a moment later.
func (r *KVCachePoolReconciler) enqueueKVCachePoolWhenBindingChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("kv cache pool binding", ctrlcli.ObjectKeyFromObject(obj))

	kvcpb, ok := obj.(*workercore.KVCachePoolBinding)
	if !ok || kvcpb.Spec.PoolRef.Name == "" {
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: kvcpb.Spec.PoolRef.Name}},
	}

	logger.V(2).Info("enqueued kv cache pool from binding", "requests", reqs)
	return reqs
}
