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
// actual cause, so a caller can tell a qualification refusal from an
// entitlement, capacity or generation refusal, and an operator can act on
// the right remedy: a disabled admission policy reads differently from an
// unsupported backend, and a refusal over both names both. The kind, the
// admission policy, and the supported backends are unchanged.
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
	policyDisabled := !b.sourceQualified
	backendUnsupported := !isSupportedTransferBackend(backend)
	switch {
	case policyDisabled && backendUnsupported:
		return protocol.New(protocol.KindSourceUnavailable, where,
			"the source-transfer admission policy is disabled and the %q transfer backend is unsupported, so the source refuses new remote work",
			backend)
	case policyDisabled:
		return protocol.New(protocol.KindSourceUnavailable, where,
			"the source-transfer admission policy is disabled, so the source refuses new remote work")
	default:
		return protocol.New(protocol.KindSourceUnavailable, where,
			"the %q transfer backend is unsupported, so the source refuses new remote work",
			backend)
	}
}
