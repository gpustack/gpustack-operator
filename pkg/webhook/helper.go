package webhook

import (
	"context"
	"fmt"
	"net/http"

	"github.com/davecgh/go-spew/spew"
	admreg "k8s.io/api/admissionregistration/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
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

		if d, ok := setups[i].(_DefaultWebhookHandler); ok {
			dh := ctrladmission.WithCustomDefaulter(scheme, obj, d).WithRecoverPanic(true)
			mux.Handle(d.DefaultPath(), dh)
		}
		if v, ok := setups[i].(_ValidatorWebhookHandler); ok {
			vh := ctrladmission.WithCustomValidator(scheme, obj, v).WithRecoverPanic(true)
			mux.Handle(v.ValidatePath(), vh)
		}
	}

	return nil
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
	if vwc != nil {
		_, err := kubeclientset.Update(ctx, vwCli, vwc,
			kubeclientset.WithCreateIfNotExisted[*admreg.ValidatingWebhookConfiguration]())
		if err != nil {
			return fmt.Errorf("install validating webhook configuration %q: %w",
				vwc.GetName(), err)
		}
	}
	if mwc != nil {
		_, err := kubeclientset.Update(ctx, mwCli, mwc,
			kubeclientset.WithCreateIfNotExisted[*admreg.MutatingWebhookConfiguration]())
		if err != nil {
			return fmt.Errorf("install mutating webhook configuration %q: %w",
				mwc.GetName(), err)
		}
	}

	return nil
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
