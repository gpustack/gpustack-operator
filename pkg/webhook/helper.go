package webhook

import (
	"context"
	"fmt"
	"net/http"

	"github.com/davecgh/go-spew/spew"
	"go.uber.org/multierr"
	admreg "k8s.io/api/admissionregistration/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

type (
	_DefaultWebhookHandler interface {
		ctrladmission.Defaulter[runtime.Object]
		DefaultPath() string
	}
	_ValidatorWebhookHandler interface {
		ctrladmission.Validator[runtime.Object]
		ValidatePath() string
	}
	// ReceiveDeletionUpdate opts a webhook out of the deletion guard. By default a
	// handler's Default and ValidateUpdate are skipped for an object that carries a
	// deletion timestamp, so a finalizer-clearing update is never rejected; a handler
	// that implements this marker keeps receiving those calls during deletion.
	ReceiveDeletionUpdate interface {
		ReceiveDeletionUpdate()
	}
	ConfigurationsGetter func(string, admreg.WebhookClientConfig) (
		*admreg.ValidatingWebhookConfiguration,
		*admreg.MutatingWebhookConfiguration,
	)
	HTTPServeMux interface {
		Handle(string, http.Handler)
	}
)

// ExecuteSetup executes the given setup to register the webhook API.
func ExecuteSetup(ctx context.Context, mgr ctrl.Manager, mux HTTPServeMux, setups []Setup) error {
	scheme := mgr.GetScheme()
	for i := range setups {
		switch setups[i].(type) {
		default:
			continue
		case _DefaultWebhookHandler:
		case _ValidatorWebhookHandler:
		}

		opts := SetupOptions{Manager: mgr}
		obj, err := setups[i].SetupWebhook(ctx, opts)
		if err != nil {
			return fmt.Errorf("webhook setup: %s: %w", spew.Sdump(setups[i]), err)
		}

		// Guard defaulting/update-validation against deletion unless the handler opts out.
		_, keepDeletionUpdate := setups[i].(ReceiveDeletionUpdate)
		if d, ok := setups[i].(_DefaultWebhookHandler); ok {
			var defaulter ctrladmission.Defaulter[runtime.Object] = d
			if !keepDeletionUpdate {
				defaulter = deletionGuardedDefaulter{delegate: d}
			}
			dh := ctrladmission.WithCustomDefaulter(scheme, obj, defaulter).WithRecoverPanic(true)
			mux.Handle(d.DefaultPath(), dh)
		}
		if v, ok := setups[i].(_ValidatorWebhookHandler); ok {
			var validator ctrladmission.Validator[runtime.Object] = v
			if !keepDeletionUpdate {
				validator = deletionGuardedValidator{delegate: v}
			}
			vh := ctrladmission.WithCustomValidator(scheme, obj, validator).WithRecoverPanic(true)
			mux.Handle(v.ValidatePath(), vh)
		}
	}

	return nil
}

// objectBeingDeleted reports whether obj carries a deletion timestamp.
func objectBeingDeleted(obj runtime.Object) bool {
	accessor, err := apimeta.Accessor(obj)
	if err != nil {
		return false
	}
	return accessor.GetDeletionTimestamp() != nil
}

// deletionGuardedDefaulter skips defaulting once the object is being deleted, so an
// update that only clears a finalizer is never rejected by defaulting that depends on
// state which may already be gone.
type deletionGuardedDefaulter struct {
	delegate ctrladmission.Defaulter[runtime.Object]
}

func (g deletionGuardedDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	if objectBeingDeleted(obj) {
		return nil
	}
	return g.delegate.Default(ctx, obj)
}

// deletionGuardedValidator skips update validation once the object is being deleted, so
// an update that only clears a finalizer is never rejected by update validation. Create
// and delete validation are delegated unchanged.
type deletionGuardedValidator struct {
	delegate ctrladmission.Validator[runtime.Object]
}

func (g deletionGuardedValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return g.delegate.ValidateCreate(ctx, obj)
}

func (g deletionGuardedValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	if objectBeingDeleted(newObj) {
		return nil, nil
	}
	return g.delegate.ValidateUpdate(ctx, oldObj, newObj)
}

func (g deletionGuardedValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return g.delegate.ValidateDelete(ctx, obj)
}

// InstallConfigurations installs the webhook configurations.
func InstallConfigurations(
	ctx context.Context,
	prefix string,
	cli kubernetes.Interface,
	cc admreg.WebhookClientConfig,
	getters []ConfigurationsGetter,
) error {
	err := review.CanDoUpdate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    admreg.SchemeGroupVersion.Group,
				Version:  admreg.SchemeGroupVersion.Version,
				Resource: "validatingwebhookconfigurations",
			},
			{
				Group:    admreg.SchemeGroupVersion.Group,
				Version:  admreg.SchemeGroupVersion.Version,
				Resource: "mutatingwebhookconfigurations",
			},
		},
		review.WithCreateIfNotExisted(),
	)
	if err != nil {
		return err
	}

	vwCli := cli.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	mwCli := cli.AdmissionregistrationV1().MutatingWebhookConfigurations()

	vwc, mwc := MergeConfigurations(cc, prefix, getters)
	// The align functions are what make a losing writer retry instead of returning the
	// conflict: every replica installs the configurations before leader election, so
	// without them a concurrent boot fails one of them.
	if vwc != nil {
		vwcAlignFn := func(aVwc *admreg.ValidatingWebhookConfiguration) (_ *admreg.ValidatingWebhookConfiguration, skip bool, err error) {
			skip = true
			// Align webhooks.
			if !kubemeta.DeepEqual(aVwc.Webhooks, vwc.Webhooks) {
				aVwc.Webhooks = vwc.DeepCopy().Webhooks
				skip = false
			}
			return aVwc, skip, err
		}
		_, err := kubeclientset.Update(ctx, vwCli, vwc,
			kubeclientset.WithCreateIfNotExisted[*admreg.ValidatingWebhookConfiguration](),
			kubeclientset.WithUpdateAlign(vwcAlignFn))
		if err != nil {
			return fmt.Errorf("install validating webhook configuration %q: %w",
				vwc.GetName(), err)
		}
	}
	if mwc != nil {
		mwcAlignFn := func(aMwc *admreg.MutatingWebhookConfiguration) (_ *admreg.MutatingWebhookConfiguration, skip bool, err error) {
			skip = true
			// Align webhooks.
			if !kubemeta.DeepEqual(aMwc.Webhooks, mwc.Webhooks) {
				aMwc.Webhooks = mwc.DeepCopy().Webhooks
				skip = false
			}
			return aMwc, skip, err
		}
		_, err := kubeclientset.Update(ctx, mwCli, mwc,
			kubeclientset.WithCreateIfNotExisted[*admreg.MutatingWebhookConfiguration](),
			kubeclientset.WithUpdateAlign(mwcAlignFn))
		if err != nil {
			return fmt.Errorf("install mutating webhook configuration %q: %w",
				mwc.GetName(), err)
		}
	}

	return nil
}

// DeleteConfigurations deletes the webhook configurations the prefix names,
// "<prefix>-validation" and "<prefix>-mutation". An absent configuration is already deleted,
// and both are attempted even after one fails.
func DeleteConfigurations(ctx context.Context, prefix string, cli kubernetes.Interface) error {
	var errs []error
	if err := cli.AdmissionregistrationV1().ValidatingWebhookConfigurations().
		Delete(ctx, prefix+"-validation", meta.DeleteOptions{}); err != nil && !kerrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete validating webhook configuration %q: %w",
			prefix+"-validation", err))
	}
	if err := cli.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Delete(ctx, prefix+"-mutation", meta.DeleteOptions{}); err != nil && !kerrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete mutating webhook configuration %q: %w",
			prefix+"-mutation", err))
	}
	return multierr.Combine(errs...)
}

// MergeConfigurations merges the webhook configurations from the getters and returns the validating and mutating webhook configurations.
func MergeConfigurations(
	cc admreg.WebhookClientConfig,
	prefix string,
	getters []ConfigurationsGetter,
) (
	*admreg.ValidatingWebhookConfiguration,
	*admreg.MutatingWebhookConfiguration,
) {
	// NB(thxCode): add more webhook configurations getters here.
	// Merge all the webhook configurations from the getters.
	var (
		vret = make([]*admreg.ValidatingWebhookConfiguration, len(getters))
		vwsc int
		mret = make([]*admreg.MutatingWebhookConfiguration, len(getters))
		mwsc int
	)
	for i := range getters {
		vwc, mwc := getters[i](prefix, cc)
		if vwc != nil {
			vret[i] = vwc
			vwsc += len(vwc.Webhooks)
		}
		if mwc != nil {
			mret[i] = mwc
			mwsc += len(mwc.Webhooks)
		}
	}

	var (
		vwc *admreg.ValidatingWebhookConfiguration
		mwc *admreg.MutatingWebhookConfiguration
	)
	if vwsc != 0 {
		vwc = &admreg.ValidatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{
				Name: prefix + "-validation",
			},
		}
		for i := range vret {
			if vret[i] == nil {
				continue
			}
			vwc.Webhooks = append(vwc.Webhooks, vret[i].Webhooks...)
		}
	}
	if mwsc != 0 {
		mwc = &admreg.MutatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{
				Name: prefix + "-mutation",
			},
		}
		for i := range mret {
			if mret[i] == nil {
				continue
			}
			mwc.Webhooks = append(mwc.Webhooks, mret[i].Webhooks...)
		}
	}

	return vwc, mwc
}
