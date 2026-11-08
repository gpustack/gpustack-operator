package worker

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// summaryGeneration names one generation the way the source's numeric
// identifiers name them, which is the shape GenerationLess orders numerically.
func summaryGeneration(name string, ready int32) workercore.ModelPrefetchTransferGeneration {
	return workercore.ModelPrefetchTransferGeneration{Generation: name, ReadyNodes: ready}
}

func generationName(n int) string {
	return strconv.Itoa(n)
}

// ascendingGenerations builds non-serving generations whose names already sit in the
// GenerationLess order the aggregator returns.
func ascendingGenerations(count int) []workercore.ModelPrefetchTransferGeneration {
	generations := make([]workercore.ModelPrefetchTransferGeneration, 0, count)
	for i := range count {
		generations = append(generations, summaryGeneration(generationName(i), 0))
	}

	return generations
}

func generationNames(from, to int) []string {
	names := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		names = append(names, generationName(i))
	}

	return names
}

func TestSummarizeModelPrefetchTransferGenerations(t *testing.T) {
	cases := []struct {
		name        string
		generations []workercore.ModelPrefetchTransferGeneration
		wantNames   []string
		wantCut     bool
	}{
		{
			name:        "an empty observation summarizes to nothing and cuts nothing",
			generations: nil,
			wantNames:   nil,
			wantCut:     false,
		},
		{
			name:        "a list at the cap passes whole and cuts nothing",
			generations: ascendingGenerations(modelPrefetchTransferGenerationLimit),
			wantNames:   generationNames(0, modelPrefetchTransferGenerationLimit),
			wantCut:     false,
		},
		{
			name:        "one over the cap cuts the later names, keeping the generation order",
			generations: ascendingGenerations(modelPrefetchTransferGenerationLimit + 1),
			wantNames:   generationNames(0, modelPrefetchTransferGenerationLimit),
			wantCut:     true,
		},
		{
			name: "serving generations go before non-serving ones that name earlier",
			generations: func() []workercore.ModelPrefetchTransferGeneration {
				// Three non-serving generations name first, two serving ones name last; the five
				// entries are under the cap, and the summary still serves first.
				generations := ascendingGenerations(3)
				generations = append(generations,
					summaryGeneration(generationName(10), 2),
					summaryGeneration(generationName(11), 1),
				)

				return generations
			}(),
			wantNames: []string{
				generationName(10), generationName(11),
				generationName(0), generationName(1), generationName(2),
			},
			wantCut: false,
		},
		{
			name: "at an over-cap size the serving entries are kept before the names cut",
			generations: func() []workercore.ModelPrefetchTransferGeneration {
				generations := ascendingGenerations(modelPrefetchTransferGenerationLimit - 1)
				generations = append(generations,
					summaryGeneration(generationName(100), 4),
					summaryGeneration(generationName(101), 1),
				)

				return generations
			}(),
			wantNames: append([]string{
				generationName(100), generationName(101),
			}, generationNames(0, modelPrefetchTransferGenerationLimit-2)...),
			wantCut: true,
		},
		{
			name: "repeated generation names keep their own order; nothing deduplicates here",
			generations: []workercore.ModelPrefetchTransferGeneration{
				summaryGeneration(generationName(1), 1),
				summaryGeneration(generationName(1), 1),
			},
			wantNames: []string{generationName(1), generationName(1)},
			wantCut:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summary, truncated := summarizeModelPrefetchTransferGenerations(c.generations)
			require.Len(t, summary, len(c.wantNames), "the summary holds exactly the kept entries")
			for i, want := range c.wantNames {
				assert.Equal(t, want, summary[i].Generation)
			}
			assert.Equal(t, c.wantCut, truncated)
		})
	}
}

func TestSummarizeModelPrefetchTransferGenerationsKeepsTheCallerObservation(t *testing.T) {
	generations := ascendingGenerations(modelPrefetchTransferGenerationLimit + 2)
	generations[0].ReadyNodes = 3
	before := append([]workercore.ModelPrefetchTransferGeneration(nil), generations...)

	summary, truncated := summarizeModelPrefetchTransferGenerations(generations)
	require.True(t, truncated)
	assert.Equal(t, before, generations, "the observation the caller owns is not reordered or edited")

	summary[0].ReadyNodes = 0
	assert.Equal(t, before, generations,
		"the summary the caller keeps is its own copy, not a view over the observation")
}

func TestSummarizeModelPrefetchTransferGenerationsIsDeterministic(t *testing.T) {
	generations := ascendingGenerations(modelPrefetchTransferGenerationLimit + 3)
	generations[4].ReadyNodes = 2
	generations[9].ReadyNodes = 1

	first, firstCut := summarizeModelPrefetchTransferGenerations(generations)
	for range 20 {
		again, againCut := summarizeModelPrefetchTransferGenerations(generations)
		assert.Equal(t, firstCut, againCut)
		assert.Equal(t, first, again, "the same observation summarizes the same way every time")
	}
}
