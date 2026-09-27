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

func (r *ModelStoreBindingWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Taking a grant away is always allowed: the namespace's prefetches lose their basis and the
	// accounting controller reports them, which is the honest answer to a revoked grant.
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

	return errs
}

// validateModelStoreBindingImmutable holds the grant still. The two exceptions are the growth
// paths: allowPinned false→true widens a grant without moving it, and a quota increase funds more
// of the same. A quota DECREASE is refused even though no byte has moved yet, because the
// accounting counts what prefetches are PROJECTED to hold, and retroactively making an admitted
// prefetch over budget reads as an operator error the operator did not make.
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
