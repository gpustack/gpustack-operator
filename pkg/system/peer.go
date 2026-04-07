package system

import (
	"gpustack.ai/gpustack/pkg/peer"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

// Peer is the peer of the system.
var Peer varx.Once[peer.Peer]

// ConfigurePeer configures the peer of the system.
func ConfigurePeer(p peer.Peer) {
	Peer.Configure(p)
}
