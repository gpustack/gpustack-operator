package worker

import (
	"context"
	"reflect"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/openapitest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/component-base/compatibility"

	"gpustack.ai/gpustack/pkg/worker/extensionapis"
)

type admissionTestClient struct {
	*kubefake.Clientset
}

func (c admissionTestClient) Discovery() discovery.DiscoveryInterface {
	return admissionTestDiscovery{DiscoveryInterface: c.Clientset.Discovery()}
}

type admissionTestDiscovery struct {
	discovery.DiscoveryInterface
}

func (admissionTestDiscovery) OpenAPIV3() openapi.Client {
	return openapitest.NewFakeClient()
}

type admissionStopInitializer struct {
	stop <-chan struct{}
}

func (i admissionStopInitializer) Initialize(plugin admission.Interface) {
	if plugin, ok := plugin.(initializer.WantsDrainedNotification); ok {
		plugin.SetDrainedNotification(i.stop)
	}
}

func TestExtensionAdmissionSyncWithoutPolicyAPIs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	client := admissionTestClient{Clientset: kubefake.NewClientset()}
	// Older hosting clusters serve webhook configuration but lack policy APIs.
	for _, resource := range []string{
		"mutatingadmissionpolicies", "mutatingadmissionpolicybindings",
		"validatingadmissionpolicies", "validatingadmissionpolicybindings",
	} {
		client.PrependReactor("list", resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
		})
	}
	factory := informers.NewSharedInformerFactory(client, 0)
	defer factory.Shutdown()
	// Cancel before waiting for informer shutdown, including after a failed assertion.
	defer cancel()
	config := genericapiserver.NewRecommendedConfig(extensionapis.Codecs)
	config.Authorization.Authorizer = authorizerfactory.NewAlwaysAllowAuthorizer()
	config.EffectiveVersion = compatibility.NewEffectiveVersionFromString("1.36", "", "")
	options := newExtensionAPIServerAdmissionOptions()
	if err := options.ApplyTo(&config.Config, factory, client,
		fake.NewSimpleDynamicClient(runtime.NewScheme()), feature.DefaultFeatureGate,
		config.EffectiveVersion, admissionStopInitializer{stop: ctx.Done()}); err != nil {
		t.Fatalf("apply admission options: %v", err)
	}
	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for informer, ready := range synced {
		if !ready {
			t.Errorf("admission informer %v could not sync without policy APIs", informer)
		}
	}
	for _, webhook := range []runtime.Object{
		&admissionv1.MutatingWebhookConfiguration{},
		&admissionv1.ValidatingWebhookConfiguration{},
	} {
		if !synced[reflect.TypeOf(webhook)] {
			t.Errorf("webhook admission informer %T must remain active and synced", webhook)
		}
	}
}
