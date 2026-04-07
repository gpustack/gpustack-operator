package wellknown

import (
	"net/http"

	"github.com/gorilla/mux"

	"gpustack.ai/gpustack/pkg/extensionroute/openapi"
	"gpustack.ai/gpustack/pkg/peer"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

func Route(r *mux.Route, peerCp *peer.ControlPlane) openapi.Extender {
	p, _ := r.GetPathTemplate()
	sr := r.Subrouter()
	sr.Path("/peer").Methods(http.MethodGet).
		HandlerFunc(createPeerHandler(peerCp))
	return getOpenapiDecorate(p)
}

type (
	responsePeer struct {
		BootstrapPeerIDs []string `json:"bootstrapPeerIDs"`
		BootstrapPort    int      `json:"bootstrapPort"`
	}
)

// createPeerHandler is a factory to create a handler for peer information,
// which is used for bootstrapping the p2p network.
//
// GET: /peer
func createPeerHandler(peerCp *peer.ControlPlane) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := responsePeer{
			BootstrapPeerIDs: peerCp.GetBootstrapPeerIDs(),
			BootstrapPort:    peerCp.GetBootstrapPort(),
		}
		httpx.JSON(w, http.StatusOK, resp)
	}
}
