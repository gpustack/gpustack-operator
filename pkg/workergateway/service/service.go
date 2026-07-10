package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/util/flushwriter"
	klog "k8s.io/klog/v2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/utils/contextx"
	"gpustack.ai/gpustack/pkg/utils/events/topic"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

type Service struct {
	Context context.Context
	Logger  klog.Logger
	Manager manager.Manager
}

func New(ctx context.Context, manager manager.Manager) (*Service, error) {
	return &Service{
		Context: ctx,
		Logger:  klog.FromContext(ctx).WithName("service"),
		Manager: manager,
	}, nil
}

func (s *Service) Index() http.Handler {
	r := mux.NewRouter()
	r.Use(httpx.AccessLog(s.Logger, true))

	r.Path("/workers").Methods(http.MethodPost).
		HandlerFunc(s.handleSubscribeWorker)
	r.Path("/workers").Methods(http.MethodDelete).
		HandlerFunc(s.handleUnsubscribeWorker)
	r.Path("/instancetypeflavors").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstanceTypeFlavors)
	r.Path("/instancetypes").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstanceTypes)
	r.Path("/instances").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstances)
	r.Path("/instancepersistentvolumetypes").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstancePersistentVolumeTypes)
	r.Path("/instancepersistentvolumes").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstancePersistentVolumes)
	r.Path("/instanceimagepullsecrets").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstanceImagePullSecrets)
	r.Path("/instancesshpublickeys").Methods(http.MethodGet).
		HandlerFunc(s.handleListInstanceSSHPublicKeys)

	return r
}

// parseGVKString parses a GVK string in the form "group/version/kind".
// A leading empty group (core group) is accepted, e.g. "/v1/Pod" or "v1/Pod".
func parseGVKString(s string) (schema.GroupVersionKind, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("empty gvk")
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return schema.GroupVersionKind{Version: parts[0], Kind: parts[1]}, nil
	case 3:
		return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}, nil
	default:
		return schema.GroupVersionKind{}, fmt.Errorf("invalid gvk format %q, expected group/version/kind", s)
	}
}

// handleSubscribeWorker handles the worker subscribe request.
//
// POST /workers?cluster=cluster1[&force=true&gvk=worker.gpustack.ai/v1/Devices&gvk=...]
// Body (optional): {"token":"...","force":true,"clusters":"...","gvks":["worker.gpustack.ai/v1/Devices"]}
func (s *Service) handleSubscribeWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Token   string   `query:"token" json:"token"`
		Force   bool     `query:"force,omitempty" json:"force,omitempty"`
		Cluster string   `query:"cluster" json:"clusters"`
		GVKs    []string `query:"gvk,omitempty" json:"gvks,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery, httpx.BindJSON)

	if req.Cluster == "" {
		logger.Error(nil, "cluster is required")
		httpx.Error(w, http.StatusBadRequest)
		return
	}

	gvks := make([]schema.GroupVersionKind, 0, len(req.GVKs))
	for _, raw := range req.GVKs {
		gvk, err := parseGVKString(raw)
		if err != nil {
			logger.Error(err, "parse gvk", "raw", raw)
			httpx.Error(w, http.StatusBadRequest)
			return
		}
		gvks = append(gvks, gvk)
	}

	err := s.Manager.SubscribeWorker(ctx, req.Cluster, req.Token, gvks, req.Force)
	if err != nil {
		logger.Error(err, "subscribe worker failed")
		httpx.Error(w, http.StatusInternalServerError)
		return
	}
}

// handleUnsubscribeWorker handles the worker unsubscribe request.
//
// DELETE /workers?cluster=cluster1
func (s *Service) handleUnsubscribeWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Cluster string `query:"cluster"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery, httpx.BindJSON)

	if req.Cluster == "" {
		logger.Error(nil, "cluster is required")
		httpx.Error(w, http.StatusBadRequest)
		return
	}

	logger.Info("unsubscribe worker", "cluster", req.Cluster)

	s.Manager.UnsubscribeWorker(ctx, req.Cluster)
}

// handleListInstanceTypeFlavors handles the list instance type flavors request.
//
// GET /instancetypeflavors?cluster=cluster1&cluster=cluster2[&aggregated=true]
func (s *Service) handleListInstanceTypeFlavors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters   []string `query:"cluster,omitempty"`
		Aggregated bool     `query:"aggregated,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstanceTypeFlavor")

	if req.Aggregated {
		listOp := OpListAggregateInstanceTypeFlavors()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, manager.IteratorOptions{}, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result(true))
		return
	}

	listOp := OpListClusterInstanceTypeFlavors()
	err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, manager.IteratorOptions{}, listOp.Next)
	if err != nil {
		logger.Error(err, "iterate workers failed")
		httpx.Error(w, http.StatusInternalServerError)
		return
	}

	httpx.JSON(w, http.StatusOK, listOp.Result())
}

// handleListInstanceTypes handles the list instance types request.
//
// GET /instancetypes?cluster=cluster1&cluster=cluster2[&watch=true&aggregated=true]
func (s *Service) handleListInstanceTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters   []string `query:"cluster,omitempty"`
		Watch      bool     `query:"watch,omitempty"`
		Aggregated bool     `query:"aggregated,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstanceType")

	if req.Aggregated {
		listOp := OpListAggregateInstanceTypes()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, manager.IteratorOptions{}, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		if !req.Watch {
			httpx.JSON(w, http.StatusOK, listOp.Result(true))
			return
		}

		// The result of listOp has transferred the ownership to watchOp,
		// the result must not be retained or read after this point.
		watchOp := OpHandleAggregatedInstanceType(listOp.Result(false))
		s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)

		return
	}

	if !req.Watch {
		listOp := OpListClusterInstanceTypes()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, manager.IteratorOptions{}, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstanceType()
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// handleListInstances handles the list instances request.
//
// GET /instances?cluster=cluster1&cluster=cluster2[&namespace=foo&watch=true]
func (s *Service) handleListInstances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters  []string `query:"cluster,omitempty"`
		Namespace string   `query:"namespace,omitempty"`
		Watch     bool     `query:"watch,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("Instance")
	iterOpts := manager.IteratorOptions{Namespace: req.Namespace}

	if !req.Watch {
		listOp := OpListClusterInstances()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, iterOpts, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstance(req.Namespace)
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// handleListInstancePersistentVolumeTypes handles the list instance persistent volume types request.
//
// GET /instancepersistentvolumetypes?cluster=cluster1&cluster=cluster2[&watch=true]
func (s *Service) handleListInstancePersistentVolumeTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters []string `query:"cluster,omitempty"`
		Watch    bool     `query:"watch,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstancePersistentVolumeType")

	if !req.Watch {
		listOp := OpListClusterInstancePersistentVolumeTypes()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, manager.IteratorOptions{}, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstancePersistentVolumeType()
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// handleListInstancePersistentVolumes handles the list instance persistent volumes request.
//
// GET /instancepersistentvolumes?cluster=cluster1&cluster=cluster2[&namespace=foo&watch=true]
func (s *Service) handleListInstancePersistentVolumes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters  []string `query:"cluster,omitempty"`
		Namespace string   `query:"namespace,omitempty"`
		Watch     bool     `query:"watch,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstancePersistentVolume")
	iterOpts := manager.IteratorOptions{Namespace: req.Namespace}

	if !req.Watch {
		listOp := OpListClusterInstancePersistentVolumes()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, iterOpts, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstancePersistentVolume(req.Namespace)
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// handleListInstanceImagePullSecrets handles the list instance image pull secrets request.
//
// GET /instanceimagepullsecrets?cluster=cluster1&cluster=cluster2[&namespace=foo&watch=true]
func (s *Service) handleListInstanceImagePullSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters  []string `query:"cluster,omitempty"`
		Namespace string   `query:"namespace,omitempty"`
		Watch     bool     `query:"watch,omitempty"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstanceImagePullSecret")
	iterOpts := manager.IteratorOptions{Namespace: req.Namespace}

	if !req.Watch {
		listOp := OpListClusterInstanceImagePullSecrets()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, iterOpts, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstanceImagePullSecret(req.Namespace)
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// handleListInstanceSSHPublicKeys handles the list instance SSH public keys request.
//
// GET /instancesshpublickeys?cluster=cluster1&cluster=cluster2[&namespace=foo&watch=true]
func (s *Service) handleListInstanceSSHPublicKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	var req struct {
		Clusters  []string `query:"cluster"`
		Namespace string   `query:"namespace"`
		Watch     bool     `query:"watch"`
	}
	_ = httpx.BindWith(r, &req, httpx.BindQuery)

	gvk := worker.SchemeGroupVersionKind("InstanceSSHPublicKey")
	iterOpts := manager.IteratorOptions{Namespace: req.Namespace}

	if !req.Watch {
		listOp := OpListClusterInstanceSSHPublicKeys()
		err := s.Manager.IterateWorkers(ctx, req.Clusters, gvk, iterOpts, listOp.Next)
		if err != nil {
			logger.Error(err, "iterate workers failed")
			httpx.Error(w, http.StatusInternalServerError)
			return
		}

		httpx.JSON(w, http.StatusOK, listOp.Result())
		return
	}

	watchOp := OpHandleClusterInstanceSSHPublicKey(req.Namespace)
	s.streamResponse(w, r, req.Clusters, gvk, watchOp.Handle)
}

// streamResponse streams the worker events to the client.
// It subscribes to the worker events of the given gvk and filters the events by the given clusters.
// The handler is used to transform the worker event before sending it to the client.
// If the handler returns nil, the event will be filtered out.
func (s *Service) streamResponse(
	w http.ResponseWriter,
	r *http.Request,
	clusters []string,
	gvk schema.GroupVersionKind,
	handler func(*manager.WorkerEvent) []*manager.WorkerEvent,
) {
	ctx := r.Context()
	logger := klog.FromContext(ctx)

	sub, err := topic.Subscribe[*manager.WorkerEvent](manager.WorkerEventTopic(gvk))
	if err != nil {
		logger.Error(err, "subscribe worker event failed")
		httpx.Error(w, http.StatusInternalServerError)
		return
	}
	defer sub.Unsubscribe()

	filters := sets.New[string](clusters...)

	if handler == nil {
		handler = func(wEvt *manager.WorkerEvent) []*manager.WorkerEvent {
			return []*manager.WorkerEvent{wEvt}
		}
	}

	sctx := contextx.WithoutCancel(ctx, s.Context)

	pr, pw := io.Pipe()
	gox.Go(func() {
		defer func() { _ = pw.Close() }()
		enc := json.NewEncoder(pw)
		for {
			tEvt, err := sub.Receive(sctx)
			if err != nil {
				return
			}
			if tEvt.Data == nil {
				continue
			}
			if filters.Len() > 0 && !filters.Has(tEvt.Data.Cluster) {
				continue
			}
			wEvts := handler(tEvt.Data.DeepCopy())
			for i := range wEvts {
				err = enc.Encode(wEvts[i])
				if err != nil {
					logger.Error(err, "encode worker event failed")
				}
			}
		}
	})

	if wsstream.IsWebSocketRequest(r) {
		reader := wsstream.NewReader(pr, true, wsstream.NewDefaultReaderProtocols())
		if err := reader.Copy(w, r); err != nil {
			logger.Error(err, "stream via websocket")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	_, _ = io.Copy(flushwriter.Wrap(w), pr)
}
