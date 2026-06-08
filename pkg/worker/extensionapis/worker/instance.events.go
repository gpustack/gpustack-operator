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

// InstanceEventsHandler handles the "events" subresource of v1.Instance objects.
//
// InstanceEventsHandler lists Kubernetes Events that involve the v1.Instance's backing Pod,
// which is named as the Instance's name.
type InstanceEventsHandler struct {
	extensionapi.GetOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newInstanceEventsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstanceEventsHandler {
	h := &InstanceEventsHandler{}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return h
}

var (
	_ rest.Storage = (*InstanceEventsHandler)(nil)
	_ rest.Getter  = (*InstanceEventsHandler)(nil)
)

func (h *InstanceEventsHandler) New() runtime.Object {
	return &worker.InstanceEvents{}
}

func (h *InstanceEventsHandler) Destroy() {
}

func (h *InstanceEventsHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	podMeta := &meta.PartialObjectMetadata{
		TypeMeta: meta.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
	}
	err := h.Client.Get(ctx, key, podMeta, &opts)
	if err != nil {
		return nil, err
	}

	coreEvtList := new(core.EventList)
	err = h.APIReader.List(ctx, coreEvtList,
		ctrlcli.MatchingFields{
			"involvedObject.name":      key.Name,
			"involvedObject.namespace": key.Namespace,
			"involvedObject.kind":      "Pod",
			"involvedObject.uid":       string(podMeta.UID),
		},
		ctrlclix.WithoutQuorum,
		ctrlcli.UnsafeDisableDeepCopy,
	)
	if err != nil {
		return nil, err
	}

	return &worker.InstanceEvents{
		ObjectMeta: podMeta.ObjectMeta,
		Items:      coreEvtList.Items,
	}, nil
}
