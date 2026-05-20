package workergateway

import (
	"net/http"

	"github.com/gorilla/mux"

	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

func (wg *WorkerGateway) getHandleApis() http.Handler {
	r := mux.NewRouter()

	r.Path("/workers").Methods(http.MethodPost).
		HandlerFunc(wg.handleSubscribeWorker)
	r.Path("/workers").Methods(http.MethodDelete).
		HandlerFunc(wg.handleUnsubscribeWorker)
	r.Path("/instancetypes").Methods(http.MethodGet).
		HandlerFunc(wg.handleListInstanceTypes)

	return r
}

// handleSubscribeWorker handles the worker subscribe request.
//
// POST /workers?cluster=cluster1&force=true
func (wg *WorkerGateway) handleSubscribeWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Cluster string `query:"cluster" json:"cluster"`
		Force   bool   `query:"force" json:"force"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery, httpx.BindJSON)

	if req.Cluster == "" {
		httpx.Error(w, http.StatusBadRequest)
		return
	}

	err := wg.Manager.SubscribeWorker(ctx, req.Cluster, req.Force)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError)
		return
	}
}

// handleUnsubscribeWorker handles the worker unsubscribe request.
//
// DELETE /workers?cluster=cluster1
func (wg *WorkerGateway) handleUnsubscribeWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Cluster string `query:"cluster" json:"cluster"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery, httpx.BindJSON)

	if req.Cluster == "" {
		httpx.Error(w, http.StatusBadRequest)
		return
	}

	wg.Manager.UnsubscribeWorker(ctx, req.Cluster)
}

// handleListInstanceTypes handles the list instance types request.
//
// GET /instancetypes?cluster=cluster1&cluster=cluster2
func (wg *WorkerGateway) handleListInstanceTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Clusters []string `query:"cluster"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	list, err := manager.AggregateInstanceTypes(ctx, req.Clusters, wg.Manager)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError)
		return
	}

	httpx.JSON(w, http.StatusOK, list)
}
