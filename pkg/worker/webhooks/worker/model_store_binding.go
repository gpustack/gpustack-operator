package worker

import (
	"context"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/webhook"
)

// ModelStoreBindingWebhook validates a v1alpha1.ModelStoreBinding.
//
// It is validating only. What it holds is what a schema cannot: the existence of every store the
// grant names, the positivity of a quota written as a quantity string, and the immutability that
// keeps a grant from being re-pointed or shrunk under consumption that admission already waved
// through.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modelstorebindings",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelStoreBindingWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *ModelStoreBindingWebhook) SetupWebhook(
	_ context.Context, opts webhook.SetupOptions,
) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.ModelStoreBinding{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelStoreBindingWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelStoreBindingWebhook)(nil)
)

// ReceiveDeletionUpdate keeps this webhook validating updates to a Binding that is being deleted.
//
// The freeze is worth most in exactly that window: a grant edited while it is being taken away is
// the shape an audit reads afterwards.
func (r *ModelStoreBindingWebhook) ReceiveDeletionUpdate() {}

func (r *ModelStoreBindingWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	binding := obj.(*workercore.ModelStoreBinding)

	errs := validateModelStoreBindingSpec(binding)
	if _, exists := binding.Annotations[broker.AuthorityAnnotation]; exists {
		path := field.NewPath("metadata", "annotations").Key(broker.AuthorityAnnotation)
		errs = append(errs, field.Forbidden(path, "the authority pin is initialized after creation"))
	}
	if len(errs) == 0 {
		// The cross-object questions are asked only once the object's own shape holds.
		errs = append(errs, r.validateModelStoreRefsExist(ctx, binding)...)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("ModelStoreBinding"), binding.Name, errs)
	}

	return nil, nil
}

func (r *ModelStoreBindingWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldBinding, newBinding := oldObj.(*workercore.ModelStoreBinding), newObj.(*workercore.ModelStoreBinding)

	errs := validateModelStoreBindingSpec(newBinding)
	errs = append(errs, validateModelStoreBindingImmutable(oldBinding, newBinding)...)
	errs = append(errs, r.validateAuthorityPin(ctx, oldBinding, newBinding)...)
	if len(errs) == 0 && modelStoreRefsMoved(oldBinding, newBinding) {
		// Re-read only when the grant moved onto stores it had not named. storeRefs is immutable, so
		// this branch is unreachable for now — the guard exists so that if immutability ever
		// narrows, the existence check comes back with it rather than silently staying away.
		errs = append(errs, r.validateModelStoreRefsExist(ctx, newBinding)...)
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("ModelStoreBinding"), newBinding.Name, errs)
	}

	return nil, nil
}

func (r *ModelStoreBindingWebhook) validateAuthorityPin(ctx context.Context, oldBinding, newBinding *workercore.ModelStoreBinding) field.ErrorList {
	path := field.NewPath("metadata", "annotations").Key(broker.AuthorityAnnotation)
	oldValue, hadPin := oldBinding.Annotations[broker.AuthorityAnnotation]
	newValue, hasPin := newBinding.Annotations[broker.AuthorityAnnotation]
	if hadPin {
		if !hasPin || oldValue != newValue {
			return field.ErrorList{field.Forbidden(path, "the authority pin is immutable")}
		}
		return nil
	}
	if !hasPin {
		return nil
	}
	pin, err := broker.ParseAuthorityPin(newValue)
	if err != nil || newBinding.UID == "" {
		return field.ErrorList{field.Forbidden(path, "the authority pin is invalid")}
	}
	if r.APIReader == nil {
		return field.ErrorList{field.InternalError(path, fmt.Errorf("the live authority reader is unavailable"))}
	}
	cm := &core.ConfigMap{}
	key := ctrlcli.ObjectKey{Namespace: setting.DelegatedSecretNamespace, Name: broker.AuthorityName(newBinding.UID)}
	if err := r.APIReader.Get(ctx, key, cm); err != nil {
		if kerrors.IsNotFound(err) {
			return field.ErrorList{field.Forbidden(path, "the authority document does not exist")}
		}
		return field.ErrorList{field.InternalError(path, fmt.Errorf("read reservation authority: %w", err))}
	}
	if err := broker.ValidateAuthorityPin(cm, newBinding.UID, pin); err != nil {
		return field.ErrorList{field.Forbidden(path, "the authority pin does not match its document")}
	}
	return nil
}

func (r *ModelStoreBindingWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Taking a grant away is always allowed: the namespace's prefetches lose their basis and the
	// accounting controller reports them, which is the honest answer to a revoked grant. Deleting
	// the Binding is also how a source-memory grant is revoked outright — an edit may remove the
	// section, which makes source preparation follow the node's configured capacity — and the
	// delete is what makes the revocation an audited event.
	return nil, nil
}

// validateModelStoreBindingSpec holds every rule answerable from the object alone.
func validateModelStoreBindingSpec(binding *workercore.ModelStoreBinding) field.ErrorList {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	for i, ref := range binding.Spec.StoreRefs {
		if ref.Name == "" {
			errs = append(errs, field.Required(specPath.Child("storeRefs").Index(i).Child("name"),
				"a grant names the store it covers"))
		}
	}

	// A budget of zero would grant a cache and refuse every prefetch with one message: the state
	// it would allow does not work, so it does not pass.
	if binding.Spec.Quota.Bytes.Sign() <= 0 {
		errs = append(errs, field.Invalid(specPath.Child("quota", "bytes"), binding.Spec.Quota.Bytes.String(),
			"must be greater than 0: it is the byte budget every prefetch of this namespace draws on"))
	}

	// The source-memory grant is a second grant, and its own shape rules apply to it alone: no
	// limit of it is inferred from quota.bytes or allowPinned, and a limit nobody set would be a
	// default pretending to be a capacity answer.
	if grant := binding.Spec.SourceMemory; grant != nil {
		if grant.Bytes.Sign() <= 0 {
			errs = append(errs, field.Invalid(specPath.Child("sourceMemory", "bytes"),
				grant.Bytes.String(),
				"must be greater than 0: it is the registered-memory ceiling the namespace's source "+
					"reservations draw on"))
		}
		if grant.Registrations < 1 {
			errs = append(errs, field.Invalid(specPath.Child("sourceMemory", "registrations"),
				grant.Registrations,
				"must be at least 1: it is the live registration count the namespace's source "+
					"generations may hold"))
		}
		if grant.Sessions < 1 {
			errs = append(errs, field.Invalid(specPath.Child("sourceMemory", "sessions"),
				grant.Sessions,
				"must be at least 1: it is the read sessions the namespace may hold admitted, "+
					"never inferred from prefetch retention"))
		}
	}

	return errs
}

// validateModelStoreBindingImmutable holds the grant still. The exceptions are the growth paths:
// allowPinned false→true widens a grant without moving it, a quota increase funds more of the
// same, and a source-memory grant may be granted for the first time, grow, or be removed — a
// removed grant makes the namespace's source preparation follow the node's own configured
// capacity, which is a coherent state the broker resolves per field, not an entitlement nobody
// admitted. A quota DECREASE is refused even though no byte has moved yet, because the
// accounting counts what prefetches are PROJECTED to hold, and retroactively making an admitted
// prefetch over budget reads as an operator error the operator did not make. The source-memory
// grant holds its bytes and registrations (they may only grow). Its session cap alone may move
// either way: sessions admitted above a lowered cap finish and drain naturally, and the lowered
// cap then governs the new ones.
func validateModelStoreBindingImmutable(old, next *workercore.ModelStoreBinding) field.ErrorList {
	specPath := field.NewPath("spec")

	var errs field.ErrorList

	if !slices.EqualFunc(old.Spec.StoreRefs, next.Spec.StoreRefs,
		func(a, b workercore.ModelStoreBindingStoreReference) bool { return a.Name == b.Name }) {
		errs = append(errs, field.Invalid(specPath.Child("storeRefs"), next.Spec.StoreRefs,
			"is immutable: the grant is what a namespace's prefetch admission is checked against, and "+
				"re-pointing it would strand the old accounting while admitting against the new"))
	}

	switch cmp := next.Spec.Quota.Bytes.Cmp(old.Spec.Quota.Bytes); {
	case cmp < 0:
		errs = append(errs, field.Invalid(specPath.Child("quota", "bytes"), next.Spec.Quota.Bytes.String(),
			fmt.Sprintf("cannot shrink: prefetches admitted against %s would turn over budget by an "+
				"edit; grow the budget instead when the namespace needs more", old.Spec.Quota.Bytes.String())))
	case cmp == 0:
		// Unmoved, which is the ordinary update (a status write or a no-op resubmit).
	}

	if modelStoreBindingAllowsPinned(old) && !modelStoreBindingAllowsPinned(next) {
		errs = append(errs, field.Invalid(specPath.Child("allowPinned"), next.Spec.AllowPinned,
			"cannot go back to false: prefetches pinned under this grant would lose the pin they were "+
				"admitted on; take the grant away by deleting the Binding, which is audited"))
	}

	errs = append(errs, validateModelStoreBindingSourceMemoryImmutable(
		specPath.Child("sourceMemory"), old.Spec.SourceMemory, next.Spec.SourceMemory,
	)...)

	return errs
}

// validateModelStoreBindingSourceMemoryImmutable holds the memory grant to grant-for-the-first-
// time, monotone growth, and removal. Bytes and registrations may not shrink below what live
// reservations already hold against while the grant stands. Sessions is the one limit an admin
// may raise or lower online — sessions admitted above a lowered cap finish and drain naturally,
// and the lowered cap then governs the new ones (the spec's own shape check keeps it at one or
// more). Removal is allowed: a Binding without the section follows the node's configured
// capacity per field, so the source preparation keeps working without pretending the absent
// grant is a number.
func validateModelStoreBindingSourceMemoryImmutable(
	path *field.Path, old, next *workercore.ModelStoreBindingSourceMemory,
) field.ErrorList {
	switch {
	case old == nil && next == nil:
		return nil
	case old == nil && next != nil:
		return nil // granting for the first time is the admin's own widening, like allowPinned false→true
	case old != nil && next == nil:
		return nil // removal hands the namespace to the node's configured capacity
	}

	var errs field.ErrorList

	if cmp := next.Bytes.Cmp(old.Bytes); cmp < 0 {
		errs = append(errs, field.Invalid(path.Child("bytes"), next.Bytes.String(),
			fmt.Sprintf("cannot shrink below the %s already granted: admitted reservations draw on it",
				old.Bytes.String())))
	}
	if next.Registrations < old.Registrations {
		errs = append(errs, field.Invalid(path.Child("registrations"), next.Registrations,
			fmt.Sprintf("cannot shrink below the %d already granted: live registrations hold against it",
				old.Registrations)))
	}

	return errs
}

// modelStoreBindingAllowsPinned is the effective allowPinned, nil reading as the false default.
func modelStoreBindingAllowsPinned(binding *workercore.ModelStoreBinding) bool {
	return binding.Spec.AllowPinned != nil && *binding.Spec.AllowPinned
}

// validateModelStoreRefsExist refuses a grant naming a store that does not exist: a Binding over a
// ghost store would report Ready and refuse every prefetch with a message about the ghost.
func (r *ModelStoreBindingWebhook) validateModelStoreRefsExist(
	ctx context.Context, binding *workercore.ModelStoreBinding,
) field.ErrorList {
	path := field.NewPath("spec", "storeRefs")

	var errs field.ErrorList

	for i, ref := range binding.Spec.StoreRefs {
		if ref.Name == "" {
			continue // already reported by the shape check
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, new(workercore.ModelStore))
		if kerrors.IsNotFound(err) {
			// storeRefs is immutable and the check is failurePolicy=Fail, so a cache a beat behind
			// would permanently reject a grant over a store that exists. Re-read uncached before
			// refusing.
			err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, new(workercore.ModelStore))
		}
		switch {
		case err == nil:
		case kerrors.IsNotFound(err):
			errs = append(errs, field.NotFound(path.Index(i).Child("name"), ref.Name))
		default:
			errs = append(errs, field.InternalError(path.Index(i).Child("name"),
				fmt.Errorf("get model store %q: %w", ref.Name, err)))
		}
	}

	return errs
}

// modelStoreRefsMoved reports whether the update named a store list it had not named before.
func modelStoreRefsMoved(old, next *workercore.ModelStoreBinding) bool {
	return !slices.EqualFunc(old.Spec.StoreRefs, next.Spec.StoreRefs,
		func(a, b workercore.ModelStoreBindingStoreReference) bool { return a.Name == b.Name })
}
