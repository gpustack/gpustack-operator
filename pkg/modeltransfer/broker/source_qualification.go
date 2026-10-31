package broker

import (
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func isSupportedTransferBackend(backend string) bool {
	switch backend {
	case "nixl", "hixl":
		return true
	default:
		return false
	}
}

// sourceGate refuses new remote work when the source gate is closed or the
// backend is unsupported. It answers with a typed refusal that names the
// reason, so a caller can tell a qualification refusal from an entitlement,
// capacity or generation refusal.
//
// The broker lock is held by the caller.
func (b *Broker) sourceGate(where, backend string) *protocol.Error {
	if b.sourceQualified && (backend == "" || isSupportedTransferBackend(backend)) {
		return nil
	}
	if backend == "" {
		return protocol.New(protocol.KindSourceUnavailable, where,
			"the generation names no transfer backend, so the source cannot prove an accepted backend")
	}
	return protocol.New(protocol.KindSourceUnavailable, where,
		"the source has no safety acceptance record for the %q transfer backend, so it refuses new remote work",
		backend)
}
