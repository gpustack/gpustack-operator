package nodefeature

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"gpustack.ai/gpustack/pkg/systemname"
)

// The fit labels publish, per node and per accelerator model, how much one card of that model can
// still give. They exist so a scheduler that reads only a node's summed capacity, as Kueue's
// topology-aware scheduling does, can still skip a node whose free room is spread over cards none of
// which fits a request. They filter only: nothing is ever charged against them.
//
// The model is the name part of the key, so the key is valid for any accelerated device key, which
// is at most 63 characters; a "<aKey>.<suffix>" name would not be. The domains are their own, apart
// from the "feature." and "acceleratable." prefixes other controllers watch, so a value that moves
// on every allocation wakes none of them.
const (
	// FitLabelDomainSuffix ends the domain of every fit label.
	FitLabelDomainSuffix = "fit." + systemname.LabelPrefix

	// _FitSlicedMaxFreeUnitsLabelPrefix prefixes the largest free units on any one card that can
	// serve a logical slice.
	_FitSlicedMaxFreeUnitsLabelPrefix = "sliced-max-free-units." + FitLabelDomainSuffix
	// _FitSharedFreeCardsLabelPrefix prefixes the number of cards that can serve a whole-card
	// family and still have a free ownership share.
	_FitSharedFreeCardsLabelPrefix = "shared-free-cards." + FitLabelDomainSuffix
)

// FitSlicedMaxFreeUnitsLabelKey returns the label key carrying the largest free units on any one
// card of the accelerator model aKey ("<manufacturer>-<id>") that can serve a logical slice.
func FitSlicedMaxFreeUnitsLabelKey(aKey string) string {
	return _FitSlicedMaxFreeUnitsLabelPrefix + aKey
}

// FitSharedFreeCardsLabelKey returns the label key carrying the number of cards of the accelerator
// model aKey ("<manufacturer>-<id>") that can serve a whole-card family and still have a free
// ownership share.
func FitSharedFreeCardsLabelKey(aKey string) string {
	return _FitSharedFreeCardsLabelPrefix + aKey
}

// IsFitLabelKey reports whether key is one of the two fit label keys, with a non-empty model, and
// is a valid label key. A model taken from an administrator's InstanceType or a Devices ledger is not
// checked against label-key grammar anywhere else, and a key built from a malformed one would make
// the Node patch or the node affinity carrying it invalid, so callers skip such a key instead.
func IsFitLabelKey(key string) bool {
	for _, prefix := range []string{_FitSlicedMaxFreeUnitsLabelPrefix, _FitSharedFreeCardsLabelPrefix} {
		if aKey, ok := strings.CutPrefix(key, prefix); ok && aKey != "" {
			return len(validation.IsQualifiedName(key)) == 0
		}
	}
	return false
}

// EqualIgnoringFitLabels reports whether two label maps are equal once every fit label is left out
// of both, so a watcher can ignore a Node update that moved only fit labels.
func EqualIgnoringFitLabels(a, b map[string]string) bool {
	for k, av := range a {
		if IsFitLabelKey(k) {
			continue
		}
		if bv, ok := b[k]; !ok || av != bv {
			return false
		}
	}
	for k := range b {
		if IsFitLabelKey(k) {
			continue
		}
		if _, ok := a[k]; !ok {
			return false
		}
	}
	return true
}

// FilterFitLabels returns the fit labels of a label map.
func FilterFitLabels(labels map[string]string) map[string]string {
	out := make(map[string]string)
	for k, v := range labels {
		if IsFitLabelKey(k) {
			out[k] = v
		}
	}
	return out
}
