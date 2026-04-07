package extensionroute

import (
	"net/http"

	"github.com/gorilla/mux"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/extensionroute/openapi"
	"gpustack.ai/gpustack/pkg/extensionroute/swagger"
)

// Route registers OpenAPI and Swagger routes,
// and extends the OpenAPI spec with the given extenders.
func Route(gvs []meta.GroupVersion, r *mux.Router, extenders ...openapi.Extender) {
	// OpenAPI.
	openapi.Route(gvs, r.PathPrefix("/openapi").Methods(http.MethodGet), extenders...)

	// Swagger.
	r.Path("/swagger").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
	})
	swagger.Route(r.PathPrefix("/swagger").Methods(http.MethodGet))
}
