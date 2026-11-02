package worker

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

// DevicesRDMASelectionPolicyHandler proxies a v1alpha1.DevicesRDMASelectionPolicy as the
// public DevicesRDMASelectionPolicy resource.
//
// The policy has no status on either version: per-node resolution and application state is
// Devices-owned, so no status route is exposed and main-resource writes replace the whole
// object.
type DevicesRDMASelectionPolicyHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *DevicesRDMASelectionPolicyHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("devicesrdmaselectionpolicies")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor()
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.DevicesRDMASelectionPolicy{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.DevicesRDMASelectionPolicy, *worker.DevicesRDMASelectionPolicyList,
		*workercore.DevicesRDMASelectionPolicy, *workercore.DevicesRDMASelectionPolicyList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *DevicesRDMASelectionPolicyHandler) New() runtime.Object {
	return &worker.DevicesRDMASelectionPolicy{}
}

func (h *DevicesRDMASelectionPolicyHandler) Destroy() {}

func (h *DevicesRDMASelectionPolicyHandler) NewList() runtime.Object {
	return &worker.DevicesRDMASelectionPolicyList{}
}

func (h *DevicesRDMASelectionPolicyHandler) NewListForProxy() runtime.Object {
	return &workercore.DevicesRDMASelectionPolicyList{}
}

func (h *DevicesRDMASelectionPolicyHandler) CastObjectTo(obj *worker.DevicesRDMASelectionPolicy) *workercore.DevicesRDMASelectionPolicy {
	return (*workercore.DevicesRDMASelectionPolicy)(obj)
}

func (h *DevicesRDMASelectionPolicyHandler) CastObjectFrom(
	_ context.Context, obj *workercore.DevicesRDMASelectionPolicy,
) *worker.DevicesRDMASelectionPolicy {
	return (*worker.DevicesRDMASelectionPolicy)(obj)
}
