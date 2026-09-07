package worker

import (
	"context"
	"fmt"
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
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// KVCachePoolBindingWebhook validates a v1alpha1.KVCachePoolBinding.
//
// It is validating only. What it holds is what a schema cannot: a shape rule on a name, a cross-object
// read for the ceiling this Binding may ask for, a cluster-wide uniqueness check on the reuse domain,
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

var _ ctrladmission.Validator[runtime.Object] = (*KVCachePoolBindingWebhook)(nil)

func (r *KVCachePoolBindingWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	kvcpb := obj.(*workercore.KVCachePoolBinding)

	errs := validateKVCachePoolBindingSpec(kvcpb)
	// The two cross-object questions are asked only once the object's own shape holds. A malformed
	// domain name would otherwise be reported alongside "no other Binding claims it", which is true
	// and useless.
	if len(errs) == 0 {
		errs = append(errs, r.validateKVCachePoolBindingDomainIsUnclaimed(ctx, kvcpb)...)
		errs = append(errs, r.validateKVCachePoolBindingCeilingFitsPool(ctx, kvcpb)...)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("KVCachePoolBinding"), kvcpb.Name, errs)
	}

	return nil, nil
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

// validateKVCachePoolBindingDomainIsUnclaimed refuses a domain another Binding already registered.
//
// Two Bindings on one domain, WHERE ONE MASTER SERVES BOTH, would SHARE cache, which may well be what
// somebody wanted, and would collide on one quota ledger, which never is: the master holds a single
// entry per tenant, so the two ceilings become last-write-wins and each namespace sees a quota the
// other one set.
//
// Uniqueness is CLUSTER-WIDE rather than per pool, because one master can serve several pools and the
// tenant space is master-global. The check is one unscoped List through the manager's cache — it
// walks no namespaces itself, and the cache is already holding every Binding for the reconciler.
//
// THAT REASON DOES NOT COVER SEVERAL MASTERS, and the gap is real: "one master can serve several
// pools" argues for uniqueness across THOSE pools, not across pools on a DIFFERENT master. With two
// independent backends, a domain every no-tenant engine writes under — "default" — can be registered
// only once cluster-wide, so injected Pods on the second backend are admitted and then fail every
// write with TENANT_NOT_REGISTERED. Tracked at
// https://github.com/gpustack/gpustack-operator/issues/166, which also records why the fix may never
// be needed. Left as written rather than narrowed here: changing the scope is an admission-semantics
// change to a merged API, and it is on a mechanism that a planned opt-in tenant would remove.
//
// THE REFUSAL'S MESSAGE THEREFORE STATES BOTH CASES, and that is load-bearing rather than thorough:
// an operator refused on two independent backends who is told the two would share cache goes looking
// for a collision two separate ledgers cannot have.
//
// It races: two creates admitted against one cache state both pass. That is why F9's reconcile-time
// refusal exists, and why this check is the one that produces a good message rather than the one that
// guarantees the invariant.
func (r *KVCachePoolBindingWebhook) validateKVCachePoolBindingDomainIsUnclaimed(
	ctx context.Context, kvcpb *workercore.KVCachePoolBinding,
) field.ErrorList {
	namePath := field.NewPath("spec", "domain", "name")

	list := &workercore.KVCachePoolBindingList{}
	if err := r.Client.List(ctx, list); err != nil {
		if err = r.APIReader.List(ctx, list, ctrlclix.WithoutQuorum); err != nil {
			return field.ErrorList{field.InternalError(namePath,
				fmt.Errorf("list kv cache pool bindings: %w", err))}
		}
	}

	for i := range list.Items {
		holder := &list.Items[i]
		if holder.Spec.Domain.Name != kvcpb.Spec.Domain.Name {
			continue
		}
		// The object under admission itself, seen through the cache on a re-admitted update.
		if holder.Namespace == kvcpb.Namespace && holder.Name == kvcpb.Name {
			continue
		}
		return field.ErrorList{field.Duplicate(namePath, fmt.Sprintf(
			"reuse domain %q is already registered by %s/%s. A domain is registered once "+
				"cluster-wide, and this check reads neither Binding's pool. Served by one master, "+
				"the two would share cache and overwrite each other's ceiling in its single "+
				"ledger entry for this tenant. Served by two independent backends, they share "+
				"nothing, and the refusal is this check's scope rather than a fault between them. "+
				"Register a domain no other Binding holds. That does not rescue a needed "+
				"\"default\" domain: the engines that forward no tenant write under that literal "+
				"name, so a Binding registering anything else registers a domain those Pods never "+
				"write to",
			kvcpb.Spec.Domain.Name, holder.Namespace, holder.Name))}
	}

	return nil
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
		if err = r.APIReader.Get(ctx, key, kvcp, ctrlclix.WithoutQuorum); err != nil {
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
