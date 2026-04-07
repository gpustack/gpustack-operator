package extensionroutes

import (
	"net/http"

	"github.com/gorilla/mux"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionroute"
)

func Index() http.Handler {
	r := mux.NewRouter()

	openapiGVs := []meta.GroupVersion{
		worker.GroupVersion,
		gpustack.GroupVersion,
	}

	// Extend routes.
	extensionroute.Route(openapiGVs, r)

	return r
}
