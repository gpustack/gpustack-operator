package modelstore

import (
	"sort"
	"strconv"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ModelTransferGenerationAggregate is one source generation's standing across the stores that report it.
// The figures are the nodes' own, summed; the namespace's reservation and the policy it was
// admitted under are the Binding's answers, not the nodes', so the caller carries them from there
// rather than finding them here.
type ModelTransferGenerationAggregate struct {
	// Generation is the source generation the figures describe.
	Generation string

	// ReadyNodes counts the stores whose entry for the digest is Serving. It is the transfer
	// readiness, and it is deliberately not the disk readyNodes: a store that stopped serving its
	// generation leaves this count while its published tree stays on disk.
	ReadyNodes int32

	// DrainingNodes counts the stores whose entry has stopped admitting — draining or
	// quarantined. Their memory is still held, which the bytes below include.
	DrainingNodes int32

	// ResidentBytes sums what the stores hold registered for the generation, serving and draining
	// alike.
	ResidentBytes int64

	// Registrations is the live registration count the stores hold.
	Registrations int32

	// ActiveSessions is the read sessions the stores have admitted.
	ActiveSessions int32

	// RejectedAdmissions is the admission refusals the stores have reported since the generation
	// started.
	RejectedAdmissions int64
}

// AggregateModelTransferGenerations returns each generation's standing for one digest, ordered by
// generation name, from the stores that report it. Two entries for one digest are the
// drain-and-refill window across stores, not a duplicate: each carries its own figures, and
// neither borrows the other's.
//
// An empty result means no store reports the digest serving — the transfer readiness the caller
// reports is none, and it says nothing about the disk trees those same stores may hold.
func AggregateModelTransferGenerations(
	stores []workercore.NodeModelStore, digest string,
) []ModelTransferGenerationAggregate {
	byGeneration := map[string]*ModelTransferGenerationAggregate{}

	for i := range stores {
		if stores[i].Status.ModelTransfer == nil {
			continue
		}
		for _, g := range stores[i].Status.ModelTransfer.Generations {
			if g.Digest != digest {
				continue
			}
			agg, ok := byGeneration[g.Generation]
			if !ok {
				agg = &ModelTransferGenerationAggregate{Generation: g.Generation}
				byGeneration[g.Generation] = agg
			}
			switch g.State {
			case workercore.NodeModelStoreModelTransferGenerationStateServing:
				agg.ReadyNodes++
			case workercore.NodeModelStoreModelTransferGenerationStateDraining,
				workercore.NodeModelStoreModelTransferGenerationStateQuarantined:
				agg.DrainingNodes++
			}
			agg.ResidentBytes += g.ResidentBytes
			agg.Registrations += g.Registrations
			agg.ActiveSessions += g.ActiveSessions
			agg.RejectedAdmissions += g.RejectedAdmissions
		}
	}

	out := make([]ModelTransferGenerationAggregate, 0, len(byGeneration))
	for _, agg := range byGeneration {
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool {
		return GenerationLess(out[i].Generation, out[j].Generation)
	})

	return out
}

// GenerationLess orders generation identifiers deterministically: two
// numeric identifiers sort before nonnumeric identifiers and compare as numbers.
// Equal numbers and nonnumeric identifiers use the plain name order.
func GenerationLess(a, b string) bool {
	av, aerr := strconv.ParseUint(a, 10, 64)
	bv, berr := strconv.ParseUint(b, 10, 64)
	if aerr == nil && berr == nil {
		if av != bv {
			return av < bv
		}
	} else if (aerr == nil) != (berr == nil) {
		return aerr == nil
	}

	return a < b
}

// ModelTransferAdmittedOnNode says whether one store's report holds an admitted
// generation for the digest — Serving, Draining, or Quarantined: each of the
// three holds its reservation.
func ModelTransferAdmittedOnNode(store *workercore.NodeModelStore, digest string) bool {
	if store == nil || store.Status.ModelTransfer == nil {
		return false
	}
	for _, g := range store.Status.ModelTransfer.Generations {
		if g.Digest == digest {
			return true
		}
	}

	return false
}

// ModelTransferAdmittedDigests narrows the candidate digests to those at least one
// store reports with an admitted generation. It is the reservation ledger's
// raw material: a namespace is charged for the digests it holds admitted, at
// their resolved sizes, whatever the physical sharing beneath them.
func ModelTransferAdmittedDigests(
	stores []workercore.NodeModelStore, candidates map[string]bool,
) map[string]bool {
	admitted := map[string]bool{}
	for digest := range candidates {
		for i := range stores {
			if ModelTransferAdmittedOnNode(&stores[i], digest) {
				admitted[digest] = true
				break
			}
		}
	}

	return admitted
}

// TransferLimits echoes a memory grant as the applied policy a reader
// sees beside the figures it bounded. A nil grant echoes none: no grant, no
// policy to restate.
func TransferLimits(
	grant *workercore.ModelStoreBindingSourceMemory,
) *workercore.ModelPrefetchTransferLimits {
	if grant == nil {
		return nil
	}

	return &workercore.ModelPrefetchTransferLimits{
		Bytes:         grant.Bytes,
		Registrations: grant.Registrations,
		Sessions:      grant.Sessions,
	}
}
