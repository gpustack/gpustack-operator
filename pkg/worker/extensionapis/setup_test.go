package extensionapis

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apiserver/pkg/registry/rest"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// TestEveryCRDViewServesDelete pins the rule on setups: a v1 view of a kind the CRDs also serve
// serves delete, list and watch, or the garbage collector, which watches the preferred version, never
// collects that kind's objects whose owner is gone.
func TestEveryCRDViewServesDelete(t *testing.T) {
	checked := 0
	for _, s := range setups {
		st, ok := s.(rest.Storage)
		if !ok {
			continue
		}
		kind := reflect.Indirect(reflect.ValueOf(st.New())).Type().Name()
		if !scheme.Scheme.Recognizes(workercore.SchemeGroupVersion.WithKind(kind)) {
			continue
		}
		checked++
		_, deletes := s.(rest.GracefulDeleter)
		_, lists := s.(rest.Lister)
		_, watches := s.(rest.Watcher)
		assert.True(t, deletes && lists && watches, "the v1 view of %s serves delete, list and watch", kind)
	}
	assert.GreaterOrEqual(t, checked, 13, "every primary storage kind is served by a view that is checked here")
}

// TestEveryStorageKindHasPublicProxy pins the registry coverage: every primary storage kind has a
// public v1 type, a handler in setups, and a view whose scope matches the storage scope. The kinds
// walked here are the ones the v1alpha1 scheme actually registers; a list answers meta.ListAccessor
// and a helper kind has no generated CRD, so both are excluded by matching the objects themselves.
func TestEveryStorageKindHasPublicProxy(t *testing.T) {
	crds := workercore.GetCustomResourceDefinitions()
	checked := 0
	for gvk := range scheme.Scheme.AllKnownTypes() {
		if gvk.Group != workercore.SchemeGroupVersion.Group ||
			gvk.Version != workercore.SchemeGroupVersion.Version {
			continue
		}
		obj, err := scheme.Scheme.New(gvk)
		require.NoError(t, err, "the scheme registers %s", gvk)
		_, listErr := kmeta.ListAccessor(obj)
		if listErr == nil {
			continue
		}
		crd, primary := crds[gvk.Kind]
		if !primary {
			continue
		}
		checked++
		t.Run(gvk.Kind, func(t *testing.T) {
			assert.True(t, scheme.Scheme.Recognizes(worker.SchemeGroupVersionKind(gvk.Kind)),
				"the storage kind %s has a public v1 type", gvk.Kind)

			var handler extensionapi.Setup
			for _, s := range setups {
				st, ok := s.(rest.Storage)
				if !ok {
					continue
				}
				if reflect.Indirect(reflect.ValueOf(st.New())).Type().Name() == gvk.Kind {
					handler = s
					break
				}
			}
			require.NotNil(t, handler, "the storage kind %s has a public v1 handler", gvk.Kind)

			scoper, ok := handler.New().(rest.Scoper)
			require.True(t, ok, "the public type of %s is scoped", gvk.Kind)
			assert.Equal(t, crd.Spec.Scope == apiext.NamespaceScoped, scoper.NamespaceScoped(),
				"the v1 view of %s serves the storage scope", gvk.Kind)
		})
	}
	assert.GreaterOrEqual(t, checked, 13, "every primary storage kind is enumerated here")
}
