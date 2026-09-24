package worker

import (
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// PodSetFitDemand returns what one Pod of the podset needs from one node's cards, read by the same
// parser the node-devices AdmissionCheck uses, so a node-affinity pin built from it and the check
// agree on the demand for every request shape.
//
//   - slicedUnits is the per-card units a logical slice needs, zero when the podset asks for no
//     slice or its template carries no folded units.
//   - sharedCards is the largest count of distinct cards any one container of the Pod needs, one
//     ownership share on each, zero when the podset asks for no share. The largest and not the sum,
//     because the check lets two containers of one Pod hold shares on the same card.
func PodSetFitDemand(ps *kueue.PodSet) (slicedUnits, sharedCards int32) {
	for _, d := range podSetFamilyDemands(ps, 1) {
		switch d.family {
		case nodefeature.ResourceFamilySliced:
			slicedUnits = d.unitsPerCard
		case nodefeature.ResourceFamilyShared:
			for _, need := range d.sharedNeeds {
				sharedCards = max(sharedCards, need.cards)
			}
		}
	}
	return slicedUnits, sharedCards
}
