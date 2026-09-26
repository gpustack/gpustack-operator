package extensionapis

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apiserver/pkg/registry/rest"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
	assert.GreaterOrEqual(t, checked, 5, "the views of Devices, Instance, InstanceType, ModelDeployment, ModelArtifact and NodeModelStore are checked")
}
