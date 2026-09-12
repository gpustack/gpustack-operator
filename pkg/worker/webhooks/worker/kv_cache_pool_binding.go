package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// KVCachePoolBindingWebhook validates a v1alpha1.KVCachePoolBinding.
//
// It is validating only. What it holds is what a schema cannot: a shape rule on a name, a cross-object
// read for the ceiling this Binding may ask for, a per-master uniqueness check on the reuse domain,
// and the immutability that keeps a warm cache from being reinterpreted underneath itself.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="kvcachepoolbindings",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type KVCachePoolBindingWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *KVCachePoolBindingWebhook) SetupWebhook(
	_ context.Context, opts webhook.SetupOptions,
) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.KVCachePoolBinding{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*KVCachePoolBindingWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*KVCachePoolBindingWebhook)(nil)
)

// ReceiveDeletionUpdate keeps this webhook validating updates to a Binding that is being deleted.
//
// The freeze is worth most in exactly that window. The finalizer holds while status.usedBy is
// non-empty, so a Binding stays for as long as workloads run against it, and the pool publishes this
// object's spec.domain as its authoritative registry entry the whole time. Skipped there, a blockSize
// or dtype edit is admitted and then reported back as the truth about blocks written at the old
// shape.
//
// FORBIDDEN: adding a rule to ValidateUpdate that reads another object unconditionally. The guard
// this opts out of exists because update validation depending on state that may already be gone must
// not be able to reject a finalizer-clearing update, and a pool deleted before its Bindings is the
// ordinary teardown order. The one cross-object read here is gated on the ceiling having moved, which
// the update that releases this object does not do.
func (r *KVCachePoolBindingWebhook) ReceiveDeletionUpdate() {}

func (r *KVCachePoolBindingWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	kvcpb := obj.(*workercore.KVCachePoolBinding)

	var warnings ctrladmission.Warnings

	errs := validateKVCachePoolBindingSpec(kvcpb)
	// The cross-object questions are asked only once the object's own shape holds. A malformed
	// domain name would otherwise be reported alongside "no other Binding claims it", which is true
	// and useless.
	if len(errs) == 0 {
		errs = append(errs, r.validateKVCachePoolBindingDomainIsUnclaimed(ctx, kvcpb)...)
		errs = append(errs, r.validateKVCachePoolBindingCeilingFitsPool(ctx, kvcpb)...)

		separableWarnings, separableErrs := r.validateKVCachePoolBindingDomainIsSeparable(ctx, kvcpb)
		warnings, errs = separableWarnings, append(errs, separableErrs...)
	}
	if len(errs) > 0 {
		// A warning is dropped alongside a refusal on purpose. It describes what an ADMITTED object
		// would leave standing, and a reader whose object was rejected has no such object.
		return nil, kerrors.NewInvalid(workercore.Kind("KVCachePoolBinding"), kvcpb.Name, errs)
	}

	return warnings, nil
}

func (r *KVCachePoolBindingWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldKvcpb, newKvcpb := oldObj.(*workercore.KVCachePoolBinding), newObj.(*workercore.KVCachePoolBinding)

	errs := validateKVCachePoolBindingSpec(newKvcpb)
	errs = append(errs, validateKVCachePoolBindingImmutable(oldKvcpb, newKvcpb)...)

	// The pool is re-read only when this update MOVED the ceiling. spec.poolRef is immutable, so the
	// pool cannot have changed identity — but its total may have, and asking about it on every update
	// would put a different object in the path of removing this Binding's finalizer. A pool deleted
	// before its Bindings is the ordinary teardown order, and a Binding that could not then be
	// updated would be undeletable.
	//
	// A ceiling left where it was is therefore not re-checked against a pool total that shrank. That
	// is deliberate and not a hole: the reconciler observes the effective quota the master actually
	// grants, and a request above what the pool can serve is already reported as a shortfall there.
	// Admission refuses what could never work; a ceiling that stopped fitting is a state to report.
	if len(errs) == 0 && ceilingMoved(oldKvcpb, newKvcpb) {
		errs = append(errs, r.validateKVCachePoolBindingCeilingFitsPool(ctx, newKvcpb)...)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("KVCachePoolBinding"), newKvcpb.Name, errs)
	}

	return nil, nil
}

func (r *KVCachePoolBindingWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Deletion is refused by the reconciler's finalizer while status.usedBy is non-empty, and the
	// tenant's own objects are drained before the ledger entry goes: this handler sees neither.
	return nil, nil
}

// validateKVCachePoolBindingSpec holds every rule answerable from the object alone.
func validateKVCachePoolBindingSpec(kvcpb *workercore.KVCachePoolBinding) field.ErrorList {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	if kvcpb.Spec.PoolRef.Name == "" {
		errs = append(errs, field.Required(specPath.Child("poolRef", "name"),
			"a Binding grants exactly one pool, named here"))
	}

	errs = append(errs, validateKVCachePoolBindingDomain(
		&kvcpb.Spec.Domain, specPath.Child("domain"))...)

	// Only the shape, here. Whether it fits the pool is a cross-object question, asked where the pool
	// can be read.
	// Asked unconditionally: the schema guarantees the key is there, so what is left to judge is the
	// value, and the master's own rule is what judges it.
	errs = append(errs, mooncake.ValidateQuotaPolicyQuota(
		kvcpb.Spec.QuotaCeiling, specPath.Child("quotaCeiling"))...)

	return errs
}

// validateKVCachePoolBindingDomain judges the reuse identity this Binding registers.
//
// The name is held to TWO rules, and the redundancy is the point. A DNS-1123 label is the shape this
// API accepts, and it happens to satisfy every rule the master has — but the master's rules are the
// ones that decide whether the rendered policy file loads, so they are asked from the one place that
// states them rather than assumed to be covered.
func validateKVCachePoolBindingDomain(
	domain *workercore.KVCachePoolBindingDomain, fldPath *field.Path,
) field.ErrorList {
	namePath := fldPath.Child("name")

	errs := mooncake.ValidateQuotaPolicyTenantName(domain.Name, namePath)
	if domain.Name != "" {
		if msgs := validation.IsDNS1123Label(domain.Name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(namePath, domain.Name, fmt.Sprintf(
				"a reuse domain is named like a Kubernetes object, so nobody learns a second naming "+
					"rule, and it travels to the storage layer as the tenant id verbatim: %s",
				strings.Join(msgs, "; "))))
		}
	}

	// A block holding no tokens is not a smaller block, it is a cache shape nothing can write into.
	if domain.BlockSize <= 0 {
		errs = append(errs, field.Invalid(fldPath.Child("blockSize"), domain.BlockSize,
			"must be greater than 0: it is the number of tokens one cache block holds"))
	}

	// The syntactic form only. The exhaustive set belongs to whatever spec owns workloads, and
	// enumerating it here would make a new engine dtype an API change to this group.
	switch dtype := domain.Dtype; {
	case dtype == "":
		errs = append(errs, field.Required(fldPath.Child("dtype"),
			"the element type the cached tensors carry, in the engine's own spelling"))
	case dtype != strings.ToLower(dtype):
		errs = append(errs, field.Invalid(fldPath.Child("dtype"), dtype,
			"must be lowercase: engines spell it that way, and this value is compared rather than "+
				"interpreted, so two spellings of one type would read as two types"))
	}

	return errs
}

// validateKVCachePoolBindingDomainIsUnclaimed refuses a domain another Binding already registered
// ON A MASTER THAT ALSO SERVES THIS ONE.
//
// Two Bindings on one domain, WHERE ONE MASTER SERVES BOTH, would SHARE cache, which may well be what
// somebody wanted, and would collide on one quota ledger, which never is: the master holds a single
// entry per tenant, so the two ceilings become last-write-wins and each namespace sees a quota the
// other one set.
//
// Uniqueness is therefore PER MASTER — wider than per pool, because one master can serve several
// pools and the tenant space is master-global, and narrower than cluster-wide, because two masters
// hold two ledgers and two key spaces. The cluster-wide form of this check is what #166 measured: on
// two independent backends only one could ever register the "default" domain that no-tenant engines
// write under, so the other backend's injected Pods were admitted and then failed every write with
// TENANT_NOT_REGISTERED. "One master can serve several pools" argues for uniqueness across THOSE
// pools; it never argued for uniqueness across pools on a DIFFERENT master, and the check now reads
// both pools' backends and refuses only on a shared one.
//
// THE REFUSAL'S MESSAGE NAMES THE SHARED BACKENDS, and that is load-bearing rather than thorough:
// the check reads neither Binding's pool until it finds a name collision, so the message is the only
// place the operator learns which backend makes the two Bindings collide.
//
// It says the pools NAME the backend rather than that they are SERVED BY it, and the difference is
// what this check actually read. A pool naming several backends is served by NONE of them — that is
// the reconciler's BackendNotSingular refusal — so on that shape "served by" would assert something
// the intersection never established. What the refusal claims is therefore exactly what was
// computed, and the consequence keeps its condition: served by one master, the two collide.
//
// The wording is also independent of HOW MANY are shared, and no clause is inflected. Several is
// reachable, because BackendNotSingular is reported in status rather than kept out of the spec, so
// admission does see such a pool. A singular subject would be ungrammatical there, and a plural
// branch would be a second code path with no test that could fail on it.
//
// It races: two creates admitted against one cache state both pass. That is why F9's reconcile-time
// refusal exists, and why this check is the one that produces a good message rather than the one that
// guarantees the invariant.
func (r *KVCachePoolBindingWebhook) validateKVCachePoolBindingDomainIsUnclaimed(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding,
) field.ErrorList {
	namePath := field.NewPath("spec", "domain", "name")

	var holder *workercore.KVCachePoolBinding
	var shared []string

	// The FIRST match ends it: any Binding holding this name over a shared master already refutes
	// uniqueness, and a second one would change nothing about the verdict or the action.
	err := r.forEachSharedMasterClaimant(ctx, kvcpb,
		func(domain string) bool { return domain == kvcpb.Spec.Domain.Name },
		func(h *workercore.KVCachePoolBinding, s []string) (bool, error) {
			holder, shared = h, s
			return true, nil
		})
	if err != nil {
		return field.ErrorList{field.InternalError(namePath, err)}
	}
	if holder == nil {
		return nil
	}

	return field.ErrorList{field.Duplicate(namePath, fmt.Sprintf(
		"reuse domain %q is already registered by %s/%s, and both Bindings' pools name %s: "+
			"served by one master, the two would share cache and overwrite each other's "+
			"ceiling in the single ledger entry it keeps per tenant. Two masters hold two "+
			"ledgers, so a "+
			"Binding claiming this domain against a pool on another backend is fine — register "+
			"a domain no other Binding on a shared master holds. That does not rescue a needed "+
			"\"default\" domain here: the engines that forward no tenant write under that "+
			"literal name, so a Binding registering anything else registers a domain those "+
			"Pods never write to",
		kvcpb.Spec.Domain.Name, holder.Namespace, holder.Name,
		strings.Join(shared, " and ")))}
}

// validateKVCachePoolBindingDomainIsSeparable judges a reuse domain that is DISTINCT from one already
// registered on a master this Binding's pool shares.
//
// Its sibling above refuses two Bindings on ONE name. This one is the opposite shape and needs a
// different answer: the two names are different, each is correctly claimed, and the question is
// whether anything downstream can keep them apart. Registering a second domain is a promise of
// separation, and the promise has two independent keepers.
//
// THE STORE IS THE KEEPER THIS CAN DECIDE, so this is the half that refuses. A master running without
// its tenant ledger puts every request into a single default tenant and degrades its shard index to a
// plain key hash, so two callers using different tenant names read and overwrite each other's blocks
// while both objects report themselves registered. No engine rescues that: a client that does forward
// its identity forwards it to a master with nowhere to put it. The FIRST domain on such a master is
// admitted and works, which is what makes the second one the object that introduces the harm — and
// the reason the refusal lands here rather than on whoever consumes it later.
//
// THE ENGINE IS THE KEEPER THIS CANNOT DECIDE, so that half is a warning and says so. Whether a
// workload's writes carry its domain depends on the inference engine in its container, and no object
// this webhook can read records which engine will consume a pool: it arrives as an annotation on a Pod
// that does not exist yet. Refusing on a fact nobody has would refuse every second domain, including
// the ones an engine does keep apart, and refusing the Pod instead puts the outage on an author who
// created no domain. So the Binding's author is told, at the moment they create the second domain,
// what it does and does not buy, and each injected Pod carries the stamp saying which happened for it.
//
// CREATE ONLY, and that is structural rather than a preference. spec.poolRef and spec.domain.name are
// both frozen by validateKVCachePoolBindingImmutable, so no update can produce a pairing of domain and
// master that did not exist at creation. A copy of this rule on the update path could therefore only
// fail an unrelated quotaCeiling edit against a collision admitted before this check existed, which is
// the hazard rather than the fix. Domains that already stand on a ledger-less master keep whatever the
// master gives them; admission refuses what has not happened yet.
func (r *KVCachePoolBindingWebhook) validateKVCachePoolBindingDomainIsSeparable(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding,
) (ctrladmission.Warnings, field.ErrorList) {
	namePath := field.NewPath("spec", "domain", "name")

	var first *workercore.KVCachePoolBinding
	var firstShared []string
	var refusal field.ErrorList

	// EVERY neighbor is examined, not only the first one found. The question this rule asks is whether
	// ANY master the two Bindings share fails to separate, and a first neighbor whose shared master is
	// fine says nothing about a second neighbor sharing a different one. Its sibling above may stop at
	// the first match because any duplicate already refutes it; the two questions are not the same
	// shape, and one early return serving both would admit exactly the case this exists to refuse.
	err := r.forEachSharedMasterClaimant(ctx, kvcpb,
		func(domain string) bool { return domain != kvcpb.Spec.Domain.Name },
		func(holder *workercore.KVCachePoolBinding, shared []string) (bool, error) {
			if first == nil {
				first, firstShared = holder, shared
			}

			for _, backend := range shared {
				separates, err := r.masterSeparatesDomains(
					ctx, backend, kvcpb.Spec.PoolRef.Name, holder.Spec.PoolRef.Name)
				if err != nil {
					return true, err
				}
				if separates {
					continue
				}

				refusal = field.ErrorList{field.Invalid(namePath, kvcpb.Spec.Domain.Name, fmt.Sprintf(
					"%s/%s already registers reuse domain %q on %s, and that backend holds no tenant "+
						"ledger: every request falls into one default tenant and the shard index "+
						"degrades to a plain key hash, so the two domains would read and overwrite "+
						"each other's blocks while both Bindings reported themselves registered. "+
						"Such a master serves one reuse domain and no more, whatever the engines do "+
						"— an engine that forwards its identity forwards it to a master with nowhere "+
						"to put it. Give that backend a ledger, by setting "+
						"spec.connection.managed.leader.multiTenancy on a managed one or by starting "+
						"an external one's master with multi-tenancy on, or point this Binding at a "+
						"pool on another backend",
					holder.Namespace, holder.Name, holder.Spec.Domain.Name, backend))}
				return true, nil
			}

			return false, nil
		})
	if err != nil {
		return nil, field.ErrorList{field.InternalError(namePath, err)}
	}
	if len(refusal) > 0 {
		return nil, refusal
	}
	if first == nil {
		return nil, nil
	}

	// Admitted, and the warning is what the admission does not cover. It names the other domain rather
	// than speaking generally, because "some other domain exists" is not something an operator can act
	// on and "team-b/batch holds beta" is.
	//
	// IT CLAIMS NOTHING ABOUT THE STORE, and that is a correction rather than brevity. Admitting here
	// means no master was OBSERVED OR DECLARED to be ledger-less, which includes an external master
	// nobody has scraped yet — so a sentence saying the master keeps the two apart would assert the very
	// thing the fall-through did not establish, in the one place an operator reads at that moment.
	return ctrladmission.Warnings{fmt.Sprintf(
		"reuse domain %q stands beside %q, which %s/%s registers on %s. Keeping two domains apart "+
			"needs both a master holding a tenant ledger and an engine that forwards the identity, "+
			"and neither is established here: an engine that forwards none writes under the store's "+
			"own \"default\" name, so its blocks and its usage land on whichever Binding registered "+
			"that name rather than on this one. Each injected Pod carries a stamp recording which "+
			"happened for it",
		kvcpb.Spec.Domain.Name, first.Spec.Domain.Name, first.Namespace, first.Name,
		strings.Join(firstShared, " and "))}, nil
}

// masterSeparatesDomains reports whether the master behind one backend can hold two reuse domains
// apart, reading the OBSERVATION before the DECLARATION.
//
// The pool's QuotaLedgerAvailable condition is what a controller measured against the running master,
// so it answers for a backend this operator did not start. It is consulted only on the one reason that
// is a configuration fact: False also carries LedgerUnreachable, which is a master that cannot be
// reached right now, and refusing an object because of an outage would send an operator to reconfigure
// something that is correct. Everything other than that one reason falls through.
//
// EVERY POOL NAMING THE MASTER IS ASKED, not only the one this Binding points at. One backend may be
// named by several pools, and the observation lives on whichever of them a controller has already
// reconciled — so reading this Binding's pool alone would miss a verdict that exists, purely because
// of which pool the new object happens to name. A Binding created in the same breath as its pool is
// the ordinary case, and that pool is the one with no conditions yet.
//
// The declaration answers for a managed backend no pool has converged for yet, and it is exact there:
// this operator renders that flag onto the leader's own command line.
//
// AN OBJECT THAT IS NOT THERE IS ANSWERED "separates", which is the side that claims less. An
// external backend nobody has scraped, a backend the pool names and nobody created, a pool that no
// longer exists — none of them is evidence that two domains collapse, and a refusal built on the
// absence of a reading would refuse a correct object for want of a controller pass. The admission
// warning on that path is worded to claim nothing the fall-through did not establish.
//
// AN OBJECT THAT CANNOT BE READ IS NOT THAT CASE, and the two are easy to fold together because both
// end in no data. Only a NotFound means absent; a timeout, an RBAC denial or a 5xx says nothing about
// whether the object exists, so it travels up and the create fails as an InternalError — the same
// answer every other cross-object read in this webhook gives, and the one this webhook's
// failurePolicy of Fail already implies.
func (r *KVCachePoolBindingWebhook) masterSeparatesDomains(
	ctx context.Context, backend string, pools ...string,
) (bool, error) {
	condition := workerctrl.KVCachePoolConditionQuotaLedgerAvailable
	seen := make(map[string]bool, len(pools))
	for _, pool := range pools {
		if pool == "" || seen[pool] {
			continue
		}
		seen[pool] = true

		kvcp, err := r.poolOrNil(ctx, pool)
		if err != nil {
			return false, err
		}
		// No membership test on the pool's backend list, and that is because the caller already made
		// one unnecessary: backend came out of the INTERSECTION of these two pools' lists, so both name
		// it by construction. A check here would be a branch no input can take.
		if kvcp == nil {
			continue
		}
		if condition.IsFalse(kvcp) &&
			condition.GetReason(kvcp) == workerctrl.KVCachePoolReasonMultiTenancyDisabled {
			return false, nil
		}
	}

	kvcb := &workercore.KVCacheBackend{}
	key := ctrlcli.ObjectKey{Name: backend}
	if err := r.Client.Get(ctx, key, kvcb); err != nil {
		if !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("get kv cache backend %q: %w", backend, err)
		}
		// A backend created in the same breath as the objects naming it is ordinary, so a cache miss is
		// re-asked of the API server before it decides anything.
		if err = r.APIReader.Get(ctx, key, kvcb); err != nil {
			if !kerrors.IsNotFound(err) {
				return false, fmt.Errorf("get kv cache backend %q: %w", backend, err)
			}
			return true, nil
		}
	}
	if managed := kvcb.Spec.Connection.Managed; managed != nil {
		return managed.Leader.MultiTenancy, nil
	}

	return true, nil
}

// forEachSharedMasterClaimant walks the OTHER Bindings whose reuse domain satisfies matchDomain and
// whose pool is served by a master this Binding's pool also names, handing each to visit together with
// the masters the two pools share. A visit returning true stops the walk.
//
// It is ONE scan serving two rules that ask opposite questions of the same neighbors — whether a
// domain is already claimed, and whether a DIFFERENT domain stands beside it on a master that cannot
// tell the two apart. A second copy of the walk would have to keep agreeing about which Bindings are
// neighbors, and the two rules would drift in the ADMITTING direction, which is the one nothing
// reports.
//
// WHERE THEY DIFFER IS WHEN TO STOP, which is why that is the caller's and not this function's: one
// duplicate refutes uniqueness, while "can every shared master separate these domains" is only
// answered by asking all of them. A shared early return would have been correct for one rule and
// silently wrong for the other.
//
// IT RACES, exactly as the uniqueness rule it shares does: the list is read from the informer cache,
// so two creates admitted against one cache state both pass, and the API server is asked only when
// the cache ERRORS rather than when it merely lags. For the uniqueness rule the reconciler's
// contested-domain pass is the backstop. THE SEPARATION RULE HAS NO SUCH BACKSTOP — a pair admitted
// together stays admitted — so this is an early refusal with a good message rather than an invariant,
// and saying so here is what keeps the next reader from taking it for one.
func (r *KVCachePoolBindingWebhook) forEachSharedMasterClaimant(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding, matchDomain func(domain string) bool,
	visit func(holder *workercore.KVCachePoolBinding, shared []string) (stop bool, err error),
) error {
	list := &workercore.KVCachePoolBindingList{}
	if err := r.Client.List(ctx, list); err != nil {
		if err = r.APIReader.List(ctx, list, ctrlclix.WithoutQuorum); err != nil {
			return fmt.Errorf("list kv cache pool bindings: %w", err)
		}
	}

	var ownBackends []string
	ownBackendsRead := false

	for i := range list.Items {
		holder := &list.Items[i]
		if !matchDomain(holder.Spec.Domain.Name) {
			continue
		}
		// The object under admission itself, seen through the cache on a re-admitted update.
		if holder.Namespace == kvcpb.Namespace && holder.Name == kvcpb.Name {
			continue
		}

		shared, err := r.poolsSharedMasters(ctx, kvcpb.Spec.PoolRef.Name, holder.Spec.PoolRef.Name,
			&ownBackends, &ownBackendsRead)
		if err != nil {
			return err
		}
		if len(shared) == 0 {
			continue
		}

		stop, err := visit(holder, shared)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}

	return nil
}

// poolsSharedMasters returns the backends serving BOTH pools two Bindings name, which is the only
// configuration in which their claims on one domain collide: a master holds one ledger entry per
// tenant, so two Bindings on one domain over one master overwrite each other's ceiling, while two
// masters hold two ledgers and collide on nothing. A pool's masters are its spec.backends, so the
// answer is an intersection; a pool naming several is served by NONE of them (the reconciler's
// BackendNotSingular refusal), but the intersection is still the safe answer — admission refuses
// rather than reasoning about a shape its sibling webhook is already reporting.
//
// A pool with no masters is answered as sharing NOTHING, and that covers a pool which cannot be
// read as well as one which names no backend, including an explicitly empty list — poolBackends
// normalizes all of them to nil. For this Binding's own pool the
// refusal is the ceiling check's to make, one call later, and a domain verdict computed against a
// missing pool would be noise ahead of it; for the holder's, a gone pool means no master serves the
// holder, so its claim collides with nothing — the reconciler's per-master contested set agrees.
// ownBackends caches this Binding's own pool across claimants, so a cluster holding N Bindings on
// one domain costs N+1 pool reads, not 2N.
func (r *KVCachePoolBindingWebhook) poolsSharedMasters(
	ctx context.Context, ownPool, holderPool string,
	ownBackends *[]string, ownBackendsRead *bool,
) ([]string, error) {
	if !*ownBackendsRead {
		backends, err := r.poolBackends(ctx, ownPool)
		if err != nil {
			return nil, err
		}
		*ownBackends = backends
		*ownBackendsRead = true
	}
	if *ownBackends == nil {
		return nil, nil
	}

	holderBackends, err := r.poolBackends(ctx, holderPool)
	if err != nil {
		return nil, err
	}
	if holderBackends == nil {
		return nil, nil
	}

	var shared []string
	for _, b := range *ownBackends {
		if slices.Contains(holderBackends, b) {
			shared = append(shared, b)
		}
	}
	return shared, nil
}

// poolBackends answers WHICH MASTERS SERVE ONE POOL. A nil slice with no error means NONE DO, and
// every way that happens is normalized to it: the pool is gone, or it exists carrying no backend.
//
// The normalization is load-bearing rather than tidy. spec.backends is required but carries no
// minItems, so an explicit empty list is a shape the API server accepts, and returning it verbatim
// would make the caller's `== nil` early return a NEAR equivalence instead of a real one — the
// verdict would still come out right, by way of an intersection against an empty set, after a
// second pool read that answers nothing.
//
// The read falls back to the API server for the reason the ceiling check's does: a Binding created
// in the same breath as its pool is ordinary, and a cache miss must not become a wrong scope verdict.
func (r *KVCachePoolBindingWebhook) poolBackends(
	ctx context.Context, name string,
) ([]string, error) {
	kvcp, err := r.poolOrNil(ctx, name)
	if err != nil || kvcp == nil || len(kvcp.Spec.Backends) == 0 {
		return nil, err
	}
	return kvcp.Spec.Backends, nil
}

// poolOrNil reads one pool, cache first and API server second, and answers a nil pool with no error
// when it genuinely does not exist.
//
// It is one function rather than a read at each site that wants a pool, because the two-step is the
// part that is easy to get subtly wrong: only a NotFound from the SECOND read means absent, and a
// timeout or an RBAC denial reported as absent turns a cluster fault into a verdict about the object
// being admitted. Two copies would agree on the day they were written.
func (r *KVCachePoolBindingWebhook) poolOrNil(
	ctx context.Context, name string,
) (*workercore.KVCachePool, error) {
	kvcp := &workercore.KVCachePool{}
	key := ctrlcli.ObjectKey{Name: name}
	if err := r.Client.Get(ctx, key, kvcp); err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("get kv cache pool %q: %w", name, err)
		}
		if err = r.APIReader.Get(ctx, key, kvcp); err != nil {
			if !kerrors.IsNotFound(err) {
				return nil, fmt.Errorf("get kv cache pool %q: %w", name, err)
			}
			return nil, nil
		}
	}
	return kvcp, nil
}

// validateKVCachePoolBindingCeilingFitsPool refuses a request the pool could not have granted.
//
// The ceiling is a REQUEST rather than a grant — the master reduces every tenant in proportion when
// the requests together exceed capacity — so this is not a reservation check and no arithmetic across
// the pool's other Bindings happens here. It refuses only the one thing that is meaningless on its
// face: asking for more than the whole pool declares.
func (r *KVCachePoolBindingWebhook) validateKVCachePoolBindingCeilingFitsPool(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding,
) field.ErrorList {
	poolPath := field.NewPath("spec", "poolRef", "name")

	kvcp := &workercore.KVCachePool{}
	key := ctrlcli.ObjectKey{Name: kvcpb.Spec.PoolRef.Name}
	if err := r.Client.Get(ctx, key, kvcp); err != nil {
		if !kerrors.IsNotFound(err) {
			return field.ErrorList{field.InternalError(poolPath,
				fmt.Errorf("get kv cache pool: %w", err))}
		}
		// A Binding created in the same breath as its pool is ordinary, so a cache miss is re-asked
		// of the API server before it becomes a refusal.
		//
		// Only a NotFound from that read is the refusal, for the reason the pool webhook gives: under
		// failurePolicy Fail, reporting a timeout or an RBAC denial as "pool not found" sends the
		// author looking for a typo in a name that is correct.
		if err = r.APIReader.Get(ctx, key, kvcp); err != nil {
			if !kerrors.IsNotFound(err) {
				return field.ErrorList{field.InternalError(poolPath,
					fmt.Errorf("get kv cache pool: %w", err))}
			}
			return field.ErrorList{field.NotFound(poolPath, key.Name)}
		}
	}

	ceiling := kvcpb.Spec.QuotaCeiling
	if ceiling.Cmp(kvcp.Spec.Quota.Total) <= 0 {
		return nil
	}

	return field.ErrorList{field.Invalid(field.NewPath("spec", "quotaCeiling"), ceiling.String(),
		fmt.Sprintf("must not exceed the pool's own ceiling of %s: a request larger than everything "+
			"pool %q declares can never be granted, whatever the other Bindings ask for",
			kvcp.Spec.Quota.Total.String(), kvcp.Name))}
}

// ceilingMoved reports whether an update changed what this Binding asks for.
//
// Cmp and not equality of the string: 1Ti and 1099511627776 are the same request written two ways,
// and re-reading the pool for a rewritten spelling would put it back in the path of every apply from
// a templating tool that normalises quantities.
func ceilingMoved(oldKvcpb, newKvcpb *workercore.KVCachePoolBinding) bool {
	return oldKvcpb.Spec.QuotaCeiling.Cmp(newKvcpb.Spec.QuotaCeiling) != 0
}

// validateKVCachePoolBindingImmutable freezes what a warm cache cannot survive being told again.
//
// The pool is frozen because re-pointing moves a namespace's grant silently and leaves its
// bytes on the old master. The domain is frozen field by field, and the last two are the dangerous
// ones: a blockSize or a dtype changed under blocks already written is not an error anywhere. The
// writes succeed, the reads succeed, and the tensors come back wrong.
func validateKVCachePoolBindingImmutable(
	oldKvcpb, newKvcpb *workercore.KVCachePoolBinding,
) field.ErrorList {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	if oldKvcpb.Spec.PoolRef.Name != newKvcpb.Spec.PoolRef.Name {
		errs = append(errs, field.Forbidden(specPath.Child("poolRef", "name"),
			"poolRef is immutable: re-pointing a Binding moves this namespace's grant "+
				"without anything recording that it moved, and strands its bytes on the old master"))
	}

	domainPath := specPath.Child("domain")
	oldDomain, newDomain := oldKvcpb.Spec.Domain, newKvcpb.Spec.Domain
	if oldDomain.Name != newDomain.Name {
		errs = append(errs, field.Forbidden(domainPath.Child("name"),
			"name is immutable: it is the tenant this namespace's cache and quota both live under, "+
				"so renaming it strands the ledger entry and abandons every block already written"))
	}
	if oldDomain.BlockSize != newDomain.BlockSize {
		errs = append(errs, field.Forbidden(domainPath.Child("blockSize"),
			"blockSize is immutable: blocks already in the cache were written at the old size, and "+
				"reading them back at a new one is silent corruption rather than a failure"))
	}
	if oldDomain.Dtype != newDomain.Dtype {
		errs = append(errs, field.Forbidden(domainPath.Child("dtype"),
			"dtype is immutable: tensors already in the cache carry the old element type, and "+
				"reading them back as another one is silent corruption rather than a failure"))
	}

	return errs
}
