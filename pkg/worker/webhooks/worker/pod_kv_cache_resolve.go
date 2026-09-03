package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemname"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// isolation is what the stamp records about a Pod's declared reuse domain.
//
// It carries no judgement about whether isolation results, and that omission is deliberate. Whether an
// injected tenant takes effect depends on the engine BUILD, which this project does not check, so a
// verdict here would have to guess - and guessing "isolated" for a build that ignores the variable is
// the one direction that misleads. What is recorded instead is the domain and the version the engine
// facts were read at; what was DONE about the tenant is recorded by the renderer, which knows.
type isolation struct {
	// Domain is the reuse identity the Binding declared. Empty only for an object that never went
	// through Binding admission, where the domain is required.
	Domain string

	// EngineVersion is the version the answer was measured at, carried so the stamp says why rather
	// than only what.
	EngineVersion string
}

// resolution is everything Default needs after reading the cluster: what to render, and what to say
// about the isolation it is not delivering.
type resolution struct {
	Input     inject.Input
	Isolation isolation
}

// resolve reads the Pod's annotations and the objects they name, and returns what to render.
//
// The reads are cached-first with a non-cached retry, because a Pod created in the same breath as its
// Binding is the ordinary case and the informer may simply not hold it yet. Only a NotFound from the
// second read is a refusal: a timeout, an RBAC denial or a 5xx says nothing about whether the object
// exists, and this webhook runs with failurePolicy Fail, so reporting one of them as "not found" would
// make a cluster outage read as a typo in the manifest.
func (r *PodKVCacheWebhook) resolve(ctx context.Context, pod *core.Pod) (*resolution, error) {
	if err := checkAnnotationVocabulary(pod); err != nil {
		return nil, err
	}

	// The message says what refusing this key does and does not buy, because the shape invites the
	// stronger reading. "The store accepts whatever tenant a client sends" was the earlier wording
	// and it is wrong HERE: injection only proceeds against a master reporting a tenant ledger
	// (checkQuotaLedger), and such a master refuses a name absent from it. Overstating the store's
	// permissiveness and overstating the Binding's authority are the same error in two directions.
	if _, ok := pod.Annotations[KVCacheDomainAnnotationKey]; ok {
		return nil, fmt.Errorf("annotation %q is not accepted: the reuse domain comes from the "+
			"KVCachePoolBinding and only from there, so that every domain this operator provisions has "+
			"an object accounting for it. This is the shape of the declarative contract, not an "+
			"enforcement boundary - a container setting MOONCAKE_TENANT_ID itself keeps that value, so "+
			"it can already name any domain some Binding has registered. What a Binding decides is "+
			"which names EXIST: the multi-tenant master this webhook injects against refuses a name "+
			"absent from its ledger. Remove it; a namespace needing two reuse boundaries gets two "+
			"Bindings", KVCacheDomainAnnotationKey)
	}

	engine, err := inject.ParseEngine(pod.Annotations[KVCacheEngineAnnotationKey])
	if err != nil {
		return nil, fmt.Errorf("annotation %q: %w", KVCacheEngineAnnotationKey, err)
	}
	role, err := inject.ParseRole(pod.Annotations[KVCacheRoleAnnotationKey])
	if err != nil {
		return nil, fmt.Errorf("annotation %q: %w", KVCacheRoleAnnotationKey, err)
	}

	binding, err := r.resolveBinding(ctx, pod)
	if err != nil {
		return nil, err
	}
	pool, err := r.resolvePool(ctx, binding)
	if err != nil {
		return nil, err
	}
	if err = r.checkQuotaLedger(pool); err != nil {
		return nil, err
	}
	backend, err := r.resolveBackend(ctx, pool)
	if err != nil {
		return nil, err
	}

	if pool.Status.ClientEndpoint == "" {
		return nil, fmt.Errorf("pool %q has not published a client endpoint yet, so there is no "+
			"address to point %q at; retry once the pool reports one", pool.Name, engine)
	}

	version, _ := inject.TenantSupportSource(engine)

	return &resolution{
		// The reuse domain goes to the renderer, which emits it only for an engine that reads one.
		// It is never a reason to refuse: an engine that ignores the variable is not a Pod anyone
		// should be stopped from creating.
		Input: inject.Input{
			Engine: engine,
			Role:   role,
			Domain: binding.Spec.Domain.Name,
			Connection: inject.Connection{
				MasterAddress: pool.Status.ClientEndpoint,
				Protocol:      mooncake.MemberProtocol(backend),
			},
		},
		Isolation: isolation{
			Domain:        binding.Spec.Domain.Name,
			EngineVersion: version,
		},
	}, nil
}

// resolveBinding reads the Binding the Pod names, in the Pod's OWN namespace. There is no
// cross-namespace form: a Binding is where a namespace's grant is provisioned and accounted, so
// honoring a namespaced value would let a Pod draw on another namespace's grant by naming it.
//
// Provisioned and accounted, NOT enforced. The store is reached over a Service any Pod can dial, and
// nothing derives a credential from this object - so a workload that knows another domain's name can
// still reach it, and a container setting MOONCAKE_TENANT_ID itself keeps that value. Calling a
// Binding an authorization point would claim a boundary this stack does not have. What it does bound
// is which names exist at all: a multi-tenant master refuses one absent from its ledger, so an
// unregistered name is not a way around a ceiling. Tracked as #168.
func (r *PodKVCacheWebhook) resolveBinding(
	ctx context.Context, pod *core.Pod,
) (*workercore.KVCachePoolBinding, error) {
	name := pod.Annotations[KVCacheBindingAnnotationKey]
	switch {
	case name == "":
		return nil, fmt.Errorf("annotation %q is required: it names the KVCachePoolBinding in this "+
			"namespace that provisions this Pod's grant on a pool", KVCacheBindingAnnotationKey)
	case strings.Contains(name, "/"):
		return nil, fmt.Errorf("annotation %q must be a plain name, not %q: a Binding is resolved in "+
			"the Pod's own namespace and there is no cross-namespace form",
			KVCacheBindingAnnotationKey, name)
	}

	// Existence is all this checks, and the Binding's readiness deliberately is not. A Binding whose
	// reconcile has not finished has not registered its tenant with the master yet, so an engine that
	// forwards one gets TENANT_NOT_REGISTERED - and then stops getting it, because the reconciler
	// converges and the same Pod's later writes succeed. The cost is an interval without caching that
	// heals itself.
	//
	// A refusal does not heal. It is not retried, so a deployment created in the same breath as its
	// Binding - the ordinary case - would simply fail. Trading a permanent failure for a temporary
	// degradation is the wrong direction.
	//
	// The division of labor that follows: PERMANENT loss is refused, TEMPORARY non-convergence is
	// admitted and left to heal. F4b is the permanent side - a pool whose ledger is gone stays gone
	// until somebody acts, and no amount of waiting fixes it.
	// Read live rather than through r.get. That helper falls back to the APIReader only on NotFound, so
	// it repairs a cache that is missing an object but not one that is holding a stale copy of it - and
	// the deletion check below reads exactly the field a stale copy is most likely to be behind on.
	// Informer lag is not a knife-edge: after a watch reconnect it is seconds, which is long enough for
	// a Binding deleted moments ago to still read as live and admit a Pod that can never write.
	// The pool and the backend keep using r.get: nothing about them is judged on a field this young.
	binding := &workercore.KVCachePoolBinding{}
	key := ctrlcli.ObjectKey{Namespace: pod.Namespace, Name: name}
	if err := r.APIReader.Get(ctx, key, binding); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("annotation %q names KVCachePoolBinding %q, which does not exist in "+
				"namespace %q: a Pod draws on a pool through a Binding its own namespace holds, so "+
				"either the name is wrong or the Binding has not been created",
				KVCacheBindingAnnotationKey, name, pod.Namespace)
		}
		return nil, fmt.Errorf("get kv cache pool binding %q in namespace %q: %w",
			name, pod.Namespace, err)
	}
	// A Binding under deletion is still returned by Get, and admitting against one is the permanent
	// side of the division of labor above rather than the temporary one: the pool reconciler is
	// removing that domain from the ledger, and once it is gone every write from this Pod fails with
	// TENANT_NOT_REGISTERED. Waiting does not heal it, and nothing walks it back - a plain Pod does
	// not appear in the Binding's status.usedBy, so the finalizer that protects declared consumers
	// does not see this one at all.
	if !binding.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("annotation %q names KVCachePoolBinding %q, which is being deleted: "+
			"its reuse domain is being withdrawn from the pool's ledger, so a Pod admitted against "+
			"it would be injected and then fail every cache write. Wait for the deletion to finish "+
			"and create a new Binding, or point this Pod at one that is not terminating",
			KVCacheBindingAnnotationKey, name)
	}
	return binding, nil
}

// resolvePool follows the Binding to its pool. The pool is cluster-scoped, so the reference carries no
// namespace and there is none to check.
func (r *PodKVCacheWebhook) resolvePool(
	ctx context.Context, binding *workercore.KVCachePoolBinding,
) (*workercore.KVCachePool, error) {
	pool := &workercore.KVCachePool{}
	key := ctrlcli.ObjectKey{Name: binding.Spec.PoolRef.Name}
	if err := r.get(ctx, key, pool); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("KVCachePoolBinding %q in namespace %q references pool %q, which "+
				"does not exist", binding.Name, binding.Namespace, key.Name)
		}
		return nil, fmt.Errorf("get kv cache pool %q: %w", key.Name, err)
	}
	return pool, nil
}

// resolveBackend follows the pool to the backend that supplies the transport.
//
// The pool's webhook admits exactly one backend name, so the first entry is the one. An empty list is
// still handled: an object that never went through that webhook would otherwise panic here, and a
// mutating webhook is the wrong place to find out.
func (r *PodKVCacheWebhook) resolveBackend(
	ctx context.Context, pool *workercore.KVCachePool,
) (*workercore.KVCacheBackend, error) {
	if len(pool.Spec.Backends) == 0 {
		return nil, fmt.Errorf("pool %q names no backend, so its transport cannot be resolved",
			pool.Name)
	}

	backend := &workercore.KVCacheBackend{}
	key := ctrlcli.ObjectKey{Name: pool.Spec.Backends[0]}
	if err := r.get(ctx, key, backend); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("pool %q references backend %q, which does not exist",
				pool.Name, key.Name)
		}
		return nil, fmt.Errorf("get kv cache backend %q: %w", key.Name, err)
	}
	return backend, nil
}

// checkQuotaLedger is F4b: refuse a pool whose master holds no tenant ledger.
//
// The three failing shapes get three messages, because they call for different actions. Reported off
// is a configuration to change; not reported yet is a wait; Unknown is also a wait, and treating it as
// "on" would be reading an admission of ignorance as an answer.
//
// This gate stays a refusal where F4a became a stamp, and the difference is whose action introduced the
// fault: the pool is the thing the Pod asked to join, and its answer is observed continuously by a
// controller rather than inferred from an upstream version.
func (r *PodKVCacheWebhook) checkQuotaLedger(pool *workercore.KVCachePool) error {
	condition := workerctrl.KVCachePoolConditionQuotaLedgerAvailable

	switch {
	case !condition.Exists(pool):
		return fmt.Errorf("pool %q does not report %s yet, so whether its master holds a tenant "+
			"ledger is unknown; retry once the pool reports it",
			pool.Name, condition)
	case condition.IsUnknown(pool):
		return fmt.Errorf("pool %q reports %s as Unknown, which is a wait rather than an answer; "+
			"retry once it settles", pool.Name, condition)
	case condition.IsFalse(pool):
		// The controller's own message is carried through rather than restated. False has more than
		// one cause: MultiTenancyDisabled is a configuration fact, LedgerUnreachable is the master
		// being temporarily unreachable. An error asserting "its master holds no tenant ledger" sends
		// an operator to reconfigure multi-tenancy during what may be an outage - and a message
		// naming the wrong cause is worse than one naming none.
		return fmt.Errorf("pool %q reports %s as False (%s): %s",
			pool.Name, condition, condition.GetReason(pool), condition.GetMessage(pool))
	}
	return nil
}

// get reads one object, cache first and API server second.
//
// The second read is CONSISTENT, deliberately. Its whole purpose is to tell an informer that has not
// caught up from an object that truly does not exist, and only a NotFound from it becomes a refusal -
// so a stale answer would produce exactly the outcome the fallback exists to prevent. An earlier
// revision passed WithoutQuorum, which sets resourceVersion=0 and explicitly permits an answer from
// behind: a Binding created moments before its Pod could still read as absent, and that Pod would be
// rejected permanently, since admission is never retried.
//
// The cost is a quorum read on the miss path only, which is already the exceptional one.
func (r *PodKVCacheWebhook) get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object) error {
	err := r.Client.Get(ctx, key, obj)
	if err == nil || !kerrors.IsNotFound(err) {
		return err
	}
	return r.APIReader.Get(ctx, key, obj)
}

// acceptedAnnotations is every kvcache.gpustack.ai/ key a Pod's author may set. The domain key is
// deliberately absent: it has its own refusal, with its own reason.
var acceptedAnnotations = []string{
	KVCacheBindingAnnotationKey,
	KVCacheEngineAnnotationKey,
	KVCacheRoleAnnotationKey,
	KVCacheContainerAnnotationKey,
}

// checkAnnotationVocabulary refuses a key in this webhook's namespace that it does not accept.
//
// A typo is the case this exists for. Silently ignoring "kvcache.gpustack.ai/bindng" would leave the
// Pod with no Binding at all, and the symptom - a container that starts fine and caches nothing - is
// invisible from outside. The two keys this webhook WRITES are refused by the same rule for a
// different reason: they are the record of what was decided, and a submitted value would be a forged
// one.
func checkAnnotationVocabulary(pod *core.Pod) error {
	prefix := "kvcache." + systemname.LabelPrefix
	for key := range pod.Annotations {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		switch {
		case slices.Contains(acceptedAnnotations, key), key == KVCacheDomainAnnotationKey:
			continue
		case key == inject.ClientConfigAnnotationKey, key == KVCacheInjectedAnnotationKey:
			return fmt.Errorf("annotation %q is written by this webhook and may not be supplied: it "+
				"records what was decided, and a submitted value would be a record of a decision "+
				"nobody made", key)
		default:
			return fmt.Errorf("annotation %q is not one this webhook accepts; the accepted keys are "+
				"%s. A key it ignored would leave the Pod configured differently from the manifest, "+
				"with nothing reporting it", key, strings.Join(acceptedAnnotations, ", "))
		}
	}
	return nil
}
