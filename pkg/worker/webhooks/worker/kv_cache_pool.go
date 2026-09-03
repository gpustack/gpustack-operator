package worker

import (
	"context"
	"fmt"
	"slices"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// KVCachePoolWebhook validates a v1alpha1.KVCachePool.
//
// It is validating only, for the reason KVCacheBackendWebhook is: every default and every enum this
// API has is a CRD schema rule, and structural schema validation runs before the validating chain,
// so a value outside one never reaches this handler.
//
// What is left is what a schema cannot express: a count whose refusal needs a reason, a read of the
// backend this pool names, and immutability.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="kvcachepools",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type KVCachePoolWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *KVCachePoolWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.KVCachePool{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*KVCachePoolWebhook)(nil)

func (r *KVCachePoolWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	kvcp := obj.(*workercore.KVCachePool)

	errs := validateKVCachePoolSpec(kvcp)
	// The backend is read only when the object's own shape already names exactly one. Reading it
	// otherwise would answer "not found" for the empty name and bury the count rule that is the
	// actual fault.
	if len(errs) == 0 {
		errs = append(errs, r.validateKVCachePoolBackend(ctx, kvcp)...)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("KVCachePool"), kvcp.Name, errs)
	}

	return nil, nil
}

func (r *KVCachePoolWebhook) ValidateUpdate(
	_ context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldKvcp, newKvcp := oldObj.(*workercore.KVCachePool), newObj.(*workercore.KVCachePool)

	// The backend is NOT re-read here, and that is deliberate. spec.backends is immutable, so an
	// update cannot have changed which backend is named or whether this pool is entitled to it — but
	// the backend itself may since have been deleted or edited, and a rule that re-asked would make
	// an admitted pool stop being updatable because of a change to a different object. The
	// reconciler removing this pool's finalizer is such an update, so refusing it would strand the
	// pool undeletable for as long as its backend stayed gone.
	//
	// F5's precondition is not weakened by that: the reconciler treats the master's own 409 as
	// authoritative and surfaces a Condition, which is level-based and therefore the enforcement
	// that actually holds. Admission is the early refusal, not the guarantee.
	errs := validateKVCachePoolSpec(newKvcp)
	errs = append(errs, validateKVCachePoolImmutable(oldKvcp, newKvcp)...)
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("KVCachePool"), newKvcp.Name, errs)
	}

	return nil, nil
}

func (r *KVCachePoolWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Deletion is refused by the reconciler's finalizer while status.usedBy is non-empty, not here:
	// this handler sees only the object, while the decision needs the Bindings that hold it.
	return nil, nil
}

// validateKVCachePoolSpec holds every rule answerable from the object alone.
func validateKVCachePoolSpec(kvcp *workercore.KVCachePool) field.ErrorList {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	// A count the schema could have refused, refused here so the message can carry the reason. The
	// shape is a list because a pool over several backends is the obvious next question, and the
	// answer to it is a quota model rather than a wider bound.
	if len(kvcp.Spec.Backends) != 1 {
		errs = append(errs, field.Invalid(specPath.Child("backends"), kvcp.Spec.Backends,
			fmt.Sprintf("exactly 1 backend is supported, and %d were given: quota is enforced by one "+
				"master's per-tenant ledger, and no master can account for bytes held in another",
				len(kvcp.Spec.Backends))))
	}
	for i, name := range kvcp.Spec.Backends {
		if name == "" {
			errs = append(errs, field.Required(specPath.Child("backends").Index(i),
				"a backend is named by the name of a KVCacheBackend"))
		}
	}

	// A resource.Quantity is a STRING in the schema, so no numeric bound in a marker can reach it.
	// The rules are the policy file's own, from the one validator that renders it: a ceiling this
	// pool declares bounds every ceiling written under it, so the two cannot hold separate opinions
	// about which numbers are usable.
	errs = append(errs, mooncake.ValidateQuotaPolicyQuota(
		kvcp.Spec.Quota.Total, specPath.Child("quota", "total"))...)

	return errs
}

// validateKVCachePoolBackend refuses a pool whose backend cannot serve it.
//
// Two questions, and they fail differently: a backend that is not there yet is a reference to fix,
// while one running without its tenant ledger is a backend to change. The second is F5's
// precondition — with no ledger every quota write comes back UNAVAILABLE_IN_CURRENT_MODE, and worse
// than the failed write is what succeeds: every request falls into one default tenant, so two
// domains read each other's blocks with nothing reporting it.
func (r *KVCachePoolWebhook) validateKVCachePoolBackend(
	ctx context.Context, kvcp *workercore.KVCachePool,
) field.ErrorList {
	backendPath := field.NewPath("spec", "backends").Index(0)

	kvcb := &workercore.KVCacheBackend{}
	key := ctrlcli.ObjectKey{Name: kvcp.Spec.Backends[0]}
	if err := r.Client.Get(ctx, key, kvcb); err != nil {
		if !kerrors.IsNotFound(err) {
			return field.ErrorList{field.InternalError(backendPath,
				fmt.Errorf("get kv cache backend: %w", err))}
		}
		// The cache may simply not hold it yet — a pool created in the same breath as its backend is
		// the ordinary case — so a miss is re-asked of the API server before it becomes a refusal.
		//
		// Only a NotFound from that read is the refusal. A timeout, an RBAC denial or a 5xx says
		// nothing about whether the backend exists, and this webhook runs with failurePolicy Fail:
		// reporting one of them as "backend not found" makes a cluster outage read as a typo in the
		// object being submitted, which is the one place an author would not look.
		if err = r.APIReader.Get(ctx, key, kvcb, ctrlclix.WithoutQuorum); err != nil {
			if !kerrors.IsNotFound(err) {
				return field.ErrorList{field.InternalError(backendPath,
					fmt.Errorf("get kv cache backend: %w", err))}
			}
			return field.ErrorList{field.NotFound(backendPath, key.Name)}
		}
	}

	// Only a managed backend can be asked. An external one runs somebody else's master, and this
	// operator does not know how that process was started — so the question is answered where it can
	// be, by the reconciler reading the master's own 409.
	managed := kvcb.Spec.Connection.Managed
	if managed == nil || managed.Leader.MultiTenancy {
		return nil
	}

	return field.ErrorList{field.Invalid(backendPath, key.Name,
		fmt.Sprintf("backend %q runs without multi-tenancy, so it holds no tenant ledger: every "+
			`quota this pool wrote would be refused, and every request would fall into one default `+
			`tenant where two reuse domains read each other's blocks. Set `+
			`"spec.connection.managed.leader.multiTenancy" on that backend`, key.Name))}
}

// validateKVCachePoolImmutable freezes the reference a pool's whole identity rests on.
//
// Re-pointing a pool would leave every tenant quota it wrote on the old master's ledger, with
// nothing left here that names that master and could delete them — and each domain's warm cache
// would be stranded with them, while the pool reported itself Ready against a backend holding
// nothing.
func validateKVCachePoolImmutable(oldKvcp, newKvcp *workercore.KVCachePool) field.ErrorList {
	if slices.Equal(oldKvcp.Spec.Backends, newKvcp.Spec.Backends) {
		return nil
	}

	return field.ErrorList{field.Forbidden(field.NewPath("spec", "backends"),
		"backends is immutable: moving a pool to another backend strands every tenant quota it "+
			"wrote on the old master's ledger, with nothing left to delete them with")}
}
