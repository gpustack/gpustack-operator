package worker

import (
	"sort"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// modelPrefetchTransferGenerationLimit matches the API status bound.
const modelPrefetchTransferGenerationLimit = 16

// summarizeModelPrefetchTransferGenerations keeps serving entries first, then
// orders by generation identity. The order implies no age. Accounting and
// readiness use the complete observation. The input remains unchanged.
func summarizeModelPrefetchTransferGenerations(
	generations []workercore.ModelPrefetchTransferGeneration,
) ([]workercore.ModelPrefetchTransferGeneration, bool) {
	summary := make([]workercore.ModelPrefetchTransferGeneration, len(generations))
	copy(summary, generations)
	sort.SliceStable(summary, func(i, j int) bool {
		servingI, servingJ := summary[i].ReadyNodes > 0, summary[j].ReadyNodes > 0
		if servingI != servingJ {
			return servingI
		}

		return modelstore.GenerationLess(summary[i].Generation, summary[j].Generation)
	})

	return summary[:min(len(summary), modelPrefetchTransferGenerationLimit)],
		len(generations) > modelPrefetchTransferGenerationLimit
}
