package router

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestMetricsForEngine_CoversEveryAPIEngine(t *testing.T) {
	crds := workercore.GetCustomResourceDefinitions()
	var engines []string
	for _, crd := range crds {
		if !strings.Contains(strings.ToLower(crd.Name), "modeldeployment") {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
			engine := spec.Properties["engine"]
			for _, value := range engine.Enum {
				engines = append(engines, strings.Trim(string(value.Raw), `"`))
			}
		}
	}
	require.NotEmpty(t, engines, "the served ModelDeployment engine enum must be readable")

	for _, engine := range engines {
		metrics, err := MetricsForEngine(engine)
		require.NoError(t, err, engine)
		assert.Positive(t, metrics.Port, engine)
		assert.NotEmpty(t, metrics.QueuedRequests, engine)
		assert.NotEmpty(t, metrics.RunningRequests, engine)
		assert.NotEmpty(t, metrics.KVCacheUtilization, engine)
	}
}

func TestMetricsForEngine_RefusalNamesMetricAndEngine(t *testing.T) {
	_, err := MetricsForEngine("engine-without-metrics")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "queued requests")
	assert.Contains(t, err.Error(), "engine-without-metrics")
}
