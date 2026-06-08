package worker

import (
	"context"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
)

// InstancePersistentVolumeEventsHandler handles the "events" subresource of v1.InstancePersistentVolume objects.
//
// InstancePersistentVolumeEventsHandler lists Kubernetes Events that involve the v1.InstancePersistentVolume's backing PersistentVolumeClaim,
// which is named as the InstancePersistentVolume's name.
type InstancePersistentVolumeEventsHandler struct {
	extensionapi.GetOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newInstancePersistentVolumeEventsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstancePersistentVolumeEventsHandler {
	h := &InstancePersistentVolumeEventsHandler{}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return h
}

var (
	_ rest.Storage = (*InstancePersistentVolumeEventsHandler)(nil)
	_ rest.Getter  = (*InstancePersistentVolumeEventsHandler)(nil)
)

func (h *InstancePersistentVolumeEventsHandler) New() runtime.Object {
	return &worker.InstancePersistentVolumeEvents{}
}

func (h *InstancePersistentVolumeEventsHandler) Destroy() {
}

func (h *InstancePersistentVolumeEventsHandler) OnGet(
	ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions,
) (runtime.Object, error) {
	pvcMeta := &meta.PartialObjectMetadata{
		TypeMeta: meta.TypeMeta{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
		},
	}
	err := h.Client.Get(ctx, key, pvcMeta, &opts)
	if err != nil {
		return nil, err
	}

	coreEvtList := new(core.EventList)
	err = h.APIReader.List(ctx, coreEvtList,
		ctrlcli.MatchingFields{
			"involvedObject.name":      key.Name,
			"involvedObject.namespace": key.Namespace,
			"involvedObject.kind":      "PersistentVolumeClaim",
			"involvedObject.uid":       string(pvcMeta.UID),
		},
		ctrlclix.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy,
	)
	if err != nil {
		return nil, err
	}

	return &worker.InstancePersistentVolumeEvents{
		ObjectMeta: pvcMeta.ObjectMeta,
		Items:      coreEvtList.Items,
	}, nil
}
