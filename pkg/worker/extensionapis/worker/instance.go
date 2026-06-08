package worker

import (
	"context"
	"fmt"
	"strings"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	_InstanceResource = "instances"
	_InstanceKind     = "Instance"
)

// InstanceHandler handles v1.Instance objects.
//
// InstanceHandler maps the v1.Instance to a Kubernetes Pod resource,
// which is named as the Instance's name.
type InstanceHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *InstanceHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &workercore.Instance{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, srs, err
	}

	// Declare GVR.
	gvr = worker.SchemeGroupVersionResource(_InstanceResource)

	// Create table converter to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Type",
				Type: "string",
			},
			Template: "{.spec.type}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Access",
				Type: "string",
			},
			Template: "{.status.accessAddresses[0]}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Port(s)",
				Type: "string",
			},
			Render: func(obj runtime.Object) string {
				ports := obj.(*worker.Instance).Status.Ports
				parts := make([]string, 0, len(ports))
				for i := range ports {
					p := &ports[i]
					parts = append(parts, fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol))
				}
				return strings.Join(parts, ",")
			},
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			Template: "{.status.phase}",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.Instance{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.Instance, *worker.InstanceList, *workercore.Instance, *workercore.InstanceList,
	](tc, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"log":    newInstanceLogHandler(h.ObjectInfo, opts),
		"events": newInstanceEventsHandler(h.ObjectInfo, opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstanceHandler)(nil)
	_ rest.Creater           = (*InstanceHandler)(nil)
	_ rest.Lister            = (*InstanceHandler)(nil)
	_ rest.Watcher           = (*InstanceHandler)(nil)
	_ rest.Getter            = (*InstanceHandler)(nil)
	_ rest.Updater           = (*InstanceHandler)(nil)
	_ rest.Patcher           = (*InstanceHandler)(nil)
	_ rest.GracefulDeleter   = (*InstanceHandler)(nil)
	_ rest.CollectionDeleter = (*InstanceHandler)(nil)
)

func (h *InstanceHandler) New() runtime.Object {
	return &worker.Instance{}
}

func (h *InstanceHandler) Destroy() {}

func (h *InstanceHandler) NewList() runtime.Object {
	return &worker.InstanceList{}
}

func (h *InstanceHandler) NewListForProxy() runtime.Object {
	return &workercore.InstanceList{}
}

func (h *InstanceHandler) CastObjectTo(do *worker.Instance) (uo *workercore.Instance) {
	return (*workercore.Instance)(do)
}

func (h *InstanceHandler) CastObjectFrom(ctx context.Context, uo *workercore.Instance) (do *worker.Instance) {
	return decorateObject(ctx, (*worker.Instance)(uo))
}

func decorateObject(ctx context.Context, inst *worker.Instance) *worker.Instance {
	staticAddress := settings.InstanceAccessStaticAddress.ShouldValueFromRemote(ctx)
	switch {
	case staticAddress != "":
		inst.Status.AccessAddresses = []string{
			staticAddress,
		}
	case len(inst.Status.HostIPs) > 0:
		address := inst.Status.HostIPs[0].IP
		wildcardDNS := settings.InstanceAccessWildcardDNS.ShouldValueFromRemote(ctx)
		if wildcardDNS != "" {
			address = string(slicex.Transform([]rune(inst.Status.HostIPs[0].IP), func(r rune) rune {
				if r == '.' || r == ':' {
					return '-'
				}
				return r
			}))
			address = fmt.Sprintf("%s.%s", address, wildcardDNS)
		}
		inst.Status.AccessAddresses = []string{address}
	}
	return inst
}
