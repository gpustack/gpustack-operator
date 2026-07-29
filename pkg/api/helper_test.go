package api

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authz "k8s.io/api/authorization/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

const (
	testCRDName        = "devices.worker.gpustack.ai"
	testCRDResource    = "customresourcedefinitions"
	testAPIServiceName = "v1alpha1.worker.gpustack.ai"
	testAPIResource    = "apiservices"
)

// newTestCRD builds a custom resource definition distinguished by its short name.
func newTestCRD(shortName string) *apiext.CustomResourceDefinition {
	return &apiext.CustomResourceDefinition{
		ObjectMeta: meta.ObjectMeta{
			Name: testCRDName,
		},
		Spec: apiext.CustomResourceDefinitionSpec{
			Group: "worker.gpustack.ai",
			Names: apiext.CustomResourceDefinitionNames{
				Plural:     "devices",
				Singular:   "devices",
				Kind:       "Devices",
				ListKind:   "DevicesList",
				ShortNames: []string{shortName},
			},
			Scope: apiext.ClusterScoped,
			Versions: []apiext.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
				},
			},
		},
	}
}

// newTestAPIService builds an api service carrying the given service reference and CA bundle.
func newTestAPIService(svc apireg.ServiceReference, ca []byte) *apireg.APIService {
	return &apireg.APIService{
		ObjectMeta: meta.ObjectMeta{
			Name: testAPIServiceName,
		},
		Spec: apireg.APIServiceSpec{
			Group:                "worker.gpustack.ai",
			Version:              "v1alpha1",
			GroupPriorityMinimum: 100,
			VersionPriority:      100,
			Service: &apireg.ServiceReference{
				Namespace: svc.Namespace,
				Name:      svc.Name,
				Port:      svc.Port,
			},
			CABundle: ca,
		},
	}
}

// testServiceReference is the routing service every api service in these tests points at.
var testServiceReference = apireg.ServiceReference{
	Namespace: "gpustack-system",
	Name:      "gpustack-worker",
	Port:      ptr.To[int32](443),
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
			action.GetResource().Resource,
			errors.New("the object has been modified"))
	}

	return false, nil, nil
}

func (f *flakyUpdates) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// allowSelfSubjectAccessReviews makes the permission checks the installers run pass, which a
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

// Test_InstallCRDs drives the concurrent-boot path of the definition installer: N replicas run
// it before leader election, so a replica that loses the update must retry instead of failing
// and crashing the boot.
func Test_InstallCRDs(t *testing.T) {
	expected := newTestCRD("expected")
	getters := []CRDGetter{
		func() map[string]*apiext.CustomResourceDefinition {
			return map[string]*apiext.CustomResourceDefinition{
				"Devices": expected,
			}
		},
	}

	testCases := []struct {
		name            string
		seed            *apiext.CustomResourceDefinition
		conflicts       int
		wantUpdateCalls int
	}{
		{
			name:            "creates an absent definition",
			wantUpdateCalls: 0,
		},
		{
			name:            "retries a conflicting update",
			seed:            newTestCRD("stale"),
			conflicts:       1,
			wantUpdateCalls: 2,
		},
		{
			name:            "skips an aligned definition",
			seed:            newTestCRD("expected"),
			wantUpdateCalls: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.seed != nil {
				objs = append(objs, tc.seed)
			}
			cli := kubefake.NewSimpleClientset(objs...)
			allowSelfSubjectAccessReviews(cli)

			updates := &flakyUpdates{resource: testCRDResource, remain: tc.conflicts}
			cli.PrependReactor("update", testCRDResource, updates.react)

			err := InstallCRDs(t.Context(), cli, getters)
			require.NoError(t, err)

			assert.Equal(t, tc.wantUpdateCalls, updates.count(), "update calls")

			actual, err := cli.ApiextensionsV1().CustomResourceDefinitions().
				Get(t.Context(), testCRDName, meta.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, expected.Spec.Names.ShortNames, actual.Spec.Names.ShortNames)
		})
	}
}

// Test_InstallServices drives the same concurrent-boot path for the extension api services,
// which the installer applies with an align function of its own.
func Test_InstallServices(t *testing.T) {
	expectedCA := []byte("expected-ca")
	getters := []ServiceGetter{newTestAPIService}

	testCases := []struct {
		name            string
		seed            *apireg.APIService
		conflicts       int
		wantUpdateCalls int
	}{
		{
			name:            "creates an absent service",
			wantUpdateCalls: 0,
		},
		{
			name:            "retries a conflicting update",
			seed:            newTestAPIService(testServiceReference, []byte("stale-ca")),
			conflicts:       1,
			wantUpdateCalls: 2,
		},
		{
			name:            "skips an aligned service",
			seed:            newTestAPIService(testServiceReference, []byte("expected-ca")),
			wantUpdateCalls: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.seed != nil {
				objs = append(objs, tc.seed)
			}
			cli := kubefake.NewSimpleClientset(objs...)
			allowSelfSubjectAccessReviews(cli)

			updates := &flakyUpdates{resource: testAPIResource, remain: tc.conflicts}
			cli.PrependReactor("update", testAPIResource, updates.react)

			err := InstallServices(t.Context(), cli, testServiceReference, expectedCA, getters)
			require.NoError(t, err)

			assert.Equal(t, tc.wantUpdateCalls, updates.count(), "update calls")

			actual, err := cli.ApiregistrationV1().APIServices().
				Get(t.Context(), testAPIServiceName, meta.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, expectedCA, actual.Spec.CABundle)
		})
	}
}
