package webhook

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type (
	// SetupOptions is the options for setting up a webhook.
	SetupOptions struct {
		// Manager is the controller-runtime manager.
		Manager ctrl.Manager
	}

	// Setup is the interface for the webhook setup.
	Setup interface {
		// SetupWebhook returns the webhook affected object if successful.
		//
		// SetupWebhook is called before the Cache is started,
		// you should not do anything that requires the Cache to be started.
		// Instead, you can configure the Cache, like IndexField or something else.
		SetupWebhook(context.Context, SetupOptions) (runtime.Object, error)
	}
)

// DefaultValidator implements admission.Validator,
// which is used to combine and override the required methods.
type DefaultValidator struct{}

var _ ctrladmission.Validator[runtime.Object] = (*DefaultValidator)(nil)

func (DefaultValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (DefaultValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

func (DefaultValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}
