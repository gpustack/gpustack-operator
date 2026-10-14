package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The table is shared policy: admission refuses a combination it lacks and the render reads the
// same row to learn which connector to configure. These cases pin the table's own shape, so a row
// cannot be added or edited without something here saying so -- and the refusal message a user
// reads is built from the same rows.
// TestModelDeploymentEPDMatrix pins the table's own shape, so a row cannot be added or edited without
// something here saying so. The rows are the whole admission answer, and the message a refusal
// carries is built from them.
func TestModelDeploymentEPDMatrix(t *testing.T) {
	t.Run("every row names a router, an engine and a platform the API has", func(t *testing.T) {
		routers := map[string]bool{
			workercore.ModelDeploymentRouterLLMD:   true,
			workercore.ModelDeploymentRouterVLLM:   true,
			workercore.ModelDeploymentRouterSGLang: true,
		}
		engines := map[string]bool{
			workercore.ModelDeploymentEngineVLLM:   true,
			workercore.ModelDeploymentEngineSGLang: true,
		}

		for _, row := range modelDeploymentEPDMatrix {
			assert.True(t, routers[row.Router], "router %q", row.Router)
			assert.True(t, engines[row.Engine], "engine %q", row.Engine)
			assert.NotEmpty(t, row.Platform, "row %+v names no platform", row)
			assert.NotEmpty(t, row.Connector, "row %+v names no connector", row)
			assert.NotEmpty(t, row.EngineFloor, "row %+v names no floor", row)
		}
	})

	t.Run("no row names the same combination twice", func(t *testing.T) {
		seen := map[string]bool{}
		for _, row := range modelDeploymentEPDMatrix {
			key := strings.Join([]string{row.Router, row.Engine, row.Platform}, "+")
			assert.False(t, seen[key], "%s is admitted twice", key)
			seen[key] = true
		}
	})

	t.Run("no NVIDIA row is admitted", func(t *testing.T) {
		_, admitted := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerNVIDIA)
		assert.False(t, admitted,
			"an NVIDIA combination is admitted only after its proof of concept passes")
	})

	t.Run("the two admitted rows are the ones the design names", func(t *testing.T) {
		ascend, admitted := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, admitted, "the Ascend row is admitted")
		assert.Equal(t, ModelDeploymentECConnectorExample, ascend.Connector)
		assert.True(t, ascend.SharedVolume, "the Ascend row carries the encoder leg through a claim")

		rocm, admitted := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAMD)
		require.True(t, admitted, "the ROCm row is admitted")
		assert.Equal(t, ModelDeploymentECConnectorCPU, rocm.Connector)
		assert.False(t, rocm.SharedVolume, "the ROCm row carries the encoder leg over the network")
	})

	// THE RETURNED ROW IS A COPY, so a caller holding one cannot reach the table behind it. The
	// table is unexported precisely so that no package can rewrite what is admitted, and a pointer
	// would have undone that for every package that could name this function.
	t.Run("the returned row is not the table", func(t *testing.T) {
		admitted, found := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, found)

		admitted.Connector = "SomethingElseEntirely"
		again, found := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, found)
		assert.Equal(t, ModelDeploymentECConnectorExample, again.Connector,
			"writing to a returned row must not reach the admitted table")
	})
}
