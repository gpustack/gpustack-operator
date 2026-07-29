package webhook

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	authz "k8s.io/api/authorization/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

const (
	testPrefix       = "gpustack-worker"
	testVwcName      = testPrefix + "-validation"
	testMwcName      = testPrefix + "-mutation"
	testVwcResource  = "validatingwebhookconfigurations"
	testMwcResource  = "mutatingwebhookconfigurations"
	testWebhookName  = "instance.worker.gpustack.ai"
	testExpectedPath = "/validate-worker-gpustack-ai-v1alpha1-instance"
	testStalePath    = "/validate-stale"
)

// newTestConfigurationsGetter returns a getter shaped like the generated one: one validating
// and one mutating webhook, both pointing at the given client config.
func newTestConfigurationsGetter() ConfigurationsGetter {
	return func(prefix string, cc admreg.WebhookClientConfig) (
		*admreg.ValidatingWebhookConfiguration,
		*admreg.MutatingWebhookConfiguration,
	) {
		return &admreg.ValidatingWebhookConfiguration{
				Webhooks: []admreg.ValidatingWebhook{
					newTestValidatingWebhook(cc),
				},
			}, &admreg.MutatingWebhookConfiguration{
				Webhooks: []admreg.MutatingWebhook{
					newTestMutatingWebhook(cc),
				},
			}
	}
}

func newTestValidatingWebhook(cc admreg.WebhookClientConfig) admreg.ValidatingWebhook {
	return admreg.ValidatingWebhook{
		Name:                    testWebhookName,
		ClientConfig:            cc,
		SideEffects:             ptr.To(admreg.SideEffectClassNone),
		AdmissionReviewVersions: []string{"v1"},
	}
}

func newTestMutatingWebhook(cc admreg.WebhookClientConfig) admreg.MutatingWebhook {
	return admreg.MutatingWebhook{
		Name:                    testWebhookName,
		ClientConfig:            cc,
		SideEffects:             ptr.To(admreg.SideEffectClassNone),
		AdmissionReviewVersions: []string{"v1"},
	}
}

// newTestClientConfig builds a webhook client config addressing the given path.
func newTestClientConfig(path string) admreg.WebhookClientConfig {
	return admreg.WebhookClientConfig{
		Service: &admreg.ServiceReference{
			Namespace: "gpustack-system",
			Name:      "gpustack-worker",
			Port:      ptr.To[int32](443),
			Path:      ptr.To(path),
		},
		CABundle: []byte("ca"),
	}
}

// flakyUpdates rejects the first n update calls of the given resource with a conflict error,
// which is the only way to drive the losing writer: the fake object tracker implements no
// optimistic concurrency, so a conflict never arises on its own.
type flakyUpdates struct {
	mu       sync.Mutex
	resource string
	remain   int
	calls    int
}

func (f *flakyUpdates) react(action k8stesting.Action) (bool, runtime.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.remain > 0 {
		f.remain--
		return true, nil, kerrors.NewConflict(
			schema.GroupResource{
				Group:    action.GetResource().Group,
				Resource: f.resource,
			},
			f.resource,
			errors.New("the object has been modified"))
	}

	return false, nil, nil
}

func (f *flakyUpdates) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// allowSelfSubjectAccessReviews makes the permission checks the installer runs pass, which a
// fake client denies by default.
func allowSelfSubjectAccessReviews(cli *kubefake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, ok := action.(k8stesting.CreateAction).GetObject().(*authz.SelfSubjectAccessReview)
			if !ok {
				return false, nil, nil
			}
			sar = sar.DeepCopy()
			sar.Status.Allowed = true
			return true, sar, nil
		})
}

// seedConfigurations builds the pair of webhook configurations a previous boot left behind,
// carrying the given webhook path.
func seedConfigurations(path string) []runtime.Object {
	cc := newTestClientConfig(path)
	return []runtime.Object{
		&admreg.ValidatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{Name: testVwcName},
			Webhooks:   []admreg.ValidatingWebhook{newTestValidatingWebhook(cc)},
		},
		&admreg.MutatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{Name: testMwcName},
			Webhooks:   []admreg.MutatingWebhook{newTestMutatingWebhook(cc)},
		},
	}
}

// Test_InstallConfigurations drives the concurrent-boot path of the webhook configuration
// installer: N replicas run it before leader election, so a replica that loses the update must
// retry instead of failing and crashing the boot.
func Test_InstallConfigurations(t *testing.T) {
	expected := newTestClientConfig(testExpectedPath)
	getters := []ConfigurationsGetter{newTestConfigurationsGetter()}

	testCases := []struct {
		name            string
		seed            []runtime.Object
		conflicts       int
		wantUpdateCalls int
	}{
		{
			name:            "creates absent configurations",
			wantUpdateCalls: 0,
		},
		{
			name:            "retries a conflicting update",
			seed:            seedConfigurations(testStalePath),
			conflicts:       1,
			wantUpdateCalls: 2,
		},
		{
			name:            "skips aligned configurations",
			seed:            seedConfigurations(testExpectedPath),
			wantUpdateCalls: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := kubefake.NewSimpleClientset(tc.seed...)
			allowSelfSubjectAccessReviews(cli)

			// Only the validating configuration is made to conflict, so the counters stay
			// unambiguous; both go through the same call.
			updates := &flakyUpdates{resource: testVwcResource, remain: tc.conflicts}
			cli.PrependReactor("update", testVwcResource, updates.react)

			err := InstallConfigurations(t.Context(), testPrefix, cli, expected, getters)
			require.NoError(t, err)

			assert.Equal(t, tc.wantUpdateCalls, updates.count(), "validating update calls")

			vwc, err := cli.AdmissionregistrationV1().ValidatingWebhookConfigurations().
				Get(t.Context(), testVwcName, meta.GetOptions{})
			require.NoError(t, err)
			require.Len(t, vwc.Webhooks, 1)
			assert.Equal(t, testExpectedPath, *vwc.Webhooks[0].ClientConfig.Service.Path)

			mwc, err := cli.AdmissionregistrationV1().MutatingWebhookConfigurations().
				Get(t.Context(), testMwcName, meta.GetOptions{})
			require.NoError(t, err)
			require.Len(t, mwc.Webhooks, 1)
			assert.Equal(t, testExpectedPath, *mwc.Webhooks[0].ClientConfig.Service.Path)
		})
	}
}
