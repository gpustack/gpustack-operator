package extensionroutes

import (
	"net/http"

	"github.com/gorilla/mux"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	gpustack "gpustack.ai/gpustack/api/v1"
	"gpustack.ai/gpustack/pkg/extensionroute"
	"gpustack.ai/gpustack/pkg/peer"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/identify"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/loopback"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/ui"
	"gpustack.ai/gpustack/pkg/server/extensionroutes/wellknown"
)

func Index(peerCp *peer.ControlPlane) http.Handler {
	r := mux.NewRouter()

	openapiGVs := []meta.GroupVersion{
		server.GroupVersion,
		gpustack.GroupVersion,
	}

	// Extend routes.
	wellKnownOpenapi := wellknown.Route(r.PathPrefix("/.well-known"), peerCp)
	identifyOpenapi := identify.Route(r.PathPrefix("/identify"))
	loopbackOpenapi := loopback.Route(r.PathPrefix("/loopback"))
	extensionroute.Route(openapiGVs, r, wellKnownOpenapi, identifyOpenapi, loopbackOpenapi)

	// UI route.
	r.NotFoundHandler = ui.Index()

	return r
}
