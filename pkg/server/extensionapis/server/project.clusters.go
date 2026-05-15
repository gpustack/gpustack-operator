package server

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const _ProjectClustersResource = "projectclusters"

type ProjectClustersHandler struct {
	extensionapi.GetOperation
	extensionapi.UpdateOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newProjectClustersHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *ProjectClustersHandler {
	h := &ProjectClustersHandler{}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)
	h.UpdateOperation = extensionapi.WithSubResourceUpdate(parent, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return h
}

var (
	_ rest.Storage = (*ProjectClustersHandler)(nil)
	_ rest.Getter  = (*ProjectClustersHandler)(nil)
	_ rest.Updater = (*ProjectClustersHandler)(nil)
	_ rest.Patcher = (*ProjectClustersHandler)(nil)
)

func (h *ProjectClustersHandler) New() runtime.Object {
	return &server.ProjectClusters{}
}

func (h *ProjectClustersHandler) Destroy() {}

func (h *ProjectClustersHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// List.
	cbList := new(servercore.ClusterBindingList)
	err := h.APIReader.List(ctx, cbList,
		ctrlcli.InNamespace(key.Name))
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	// Convert.
	projclss := convertProjectClustersFromClusterBindingList(cbList)
	if projclss == nil {
		return nil, kerrors.NewNotFound(server.Resource(_ProjectClustersResource), key.Name)
	}

	// Get and refill.
	proj := new(server.Project)
	err = h.Client.Get(ctx, key, proj, &opts)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}
	projclss.ObjectMeta = proj.ObjectMeta

	return projclss, nil
}

func (h *ProjectClustersHandler) OnUpdate(ctx context.Context, obj, objOld runtime.Object, _ ctrlcli.UpdateOptions) (runtime.Object, error) {
	projclss, projclssOld := obj.(*server.ProjectClusters), objOld.(*server.ProjectClusters)

	// Figure out delta.
	projclssSet := sets.New[server.ProjectCluster](projclss.Items...)
	projclssOldSet := sets.New[server.ProjectCluster](projclssOld.Items...)
	needBinding := projclssSet.Difference(projclssOldSet)
	needUnbinding := projclssOldSet.Difference(projclssSet)

	// Unbind.
	for _, projcls := range needUnbinding.UnsortedList() {
		cb := &servercore.ClusterBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: projclss.Name,
				Name:      projcls.Name,
			},
		}

		// Delete.
		err := kubeclientset.DeleteWithCtrlClient(ctx, h.Client, cb)
		if err != nil {
			return nil, kerrors.NewInternalError(fmt.Errorf("delete cluster binding: %w", err))
		}
	}

	// Bind.
	for _, projcls := range needBinding.UnsortedList() {
		cb := &servercore.ClusterBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: projclss.Name,
				Name:      projcls.Name,
			},
			ClusterRef: projcls.ClusterReference,
		}
		systemmeta.NoteResource(cb, "", map[string]string{
			"scope":   "project",
			"project": kubemeta.GetNamespacedNameKey(projclss),
		})

		// Create.
		_, err := kubeclientset.CreateWithCtrlClient(ctx, h.Client, cb)
		if err != nil {
			return nil, kerrors.NewInternalError(fmt.Errorf("create cluster binding: %w", err))
		}
	}

	// Get.
	return h.OnGet(ctx, ctrlcli.ObjectKeyFromObject(projclss),
		ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
}

func convertProjectClusterFromClusterBinding(cb *servercore.ClusterBinding) *server.ProjectCluster {
	if cb == nil {
		return nil
	}

	if cb.DeletionTimestamp != nil {
		return nil
	}

	projcls := &server.ProjectCluster{
		ClusterReference: cb.ClusterRef,
	}

	return projcls
}

func convertProjectClustersFromClusterBindingList(cbList *servercore.ClusterBindingList) *server.ProjectClusters {
	if cbList == nil {
		return nil
	}

	projclss := &server.ProjectClusters{
		Items: make([]server.ProjectCluster, 0, len(cbList.Items)),
	}

	for i := range cbList.Items {
		projcls := convertProjectClusterFromClusterBinding(&cbList.Items[i])
		if projcls == nil {
			continue
		}
		projclss.Items = append(projclss.Items, *projcls)
	}

	return projclss
}
