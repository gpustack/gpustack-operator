package server

import (
	"context"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/server/apistatus"
)

// ClusterConfigHandler handles v1.ClusterConfig objects,
// which is a subresource of v1.Cluster objects.
type ClusterConfigHandler struct {
	extensionapi.GetOperation
	extensionapi.UpdateOperation

	Client ctrlcli.Client
}

func newClusterConfigHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *ClusterConfigHandler {
	h := &ClusterConfigHandler{}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)
	h.UpdateOperation = extensionapi.WithSubResourceUpdate(parent, h)

	// Set client.
	h.Client = opts.Manager.GetClient()

	return h
}

var (
	_ rest.Storage = (*ClusterConfigHandler)(nil)
	_ rest.Getter  = (*ClusterConfigHandler)(nil)
	_ rest.Updater = (*ClusterConfigHandler)(nil)
	_ rest.Patcher = (*ClusterConfigHandler)(nil)
)

func (h *ClusterConfigHandler) New() runtime.Object {
	return &server.ClusterConfig{}
}

func (h *ClusterConfigHandler) Destroy() {}

const (
	_kubeconfig = "kubeconfig"
)

func (h *ClusterConfigHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get cluster.
	cls := new(servercore.Cluster)
	err := h.Client.Get(ctx, key, cls, &opts)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	// Fill cluster config.
	clsCfg := &server.ClusterConfig{
		ObjectMeta: cls.ObjectMeta,
		Status: server.ClusterConfigStatus{
			Type: cls.Spec.Type,
		},
	}

	// Get the kubeconfig from the referenced secret.
	if cls.Spec.Type != servercore.ClusterTypeLoopback && cls.Status.ConfigSecretName != "" {
		sec := &core.Secret{
			ObjectMeta: meta.ObjectMeta{
				Namespace: key.Namespace,
				Name:      cls.Status.ConfigSecretName,
			},
		}
		err = h.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec)
		if err == nil {
			if _, ok := sec.Data[_kubeconfig]; ok {
				clsCfg.Status.Config = string(sec.Data[_kubeconfig])
			}
		}
	}

	return clsCfg, nil
}

func (h *ClusterConfigHandler) OnUpdate(ctx context.Context, obj, objOld runtime.Object, _ ctrlcli.UpdateOptions) (runtime.Object, error) {
	clsCfg, clsCfgOld := obj.(*server.ClusterConfig), objOld.(*server.ClusterConfig)

	if clsCfg.Spec.Config == clsCfgOld.Status.Config {
		return clsCfg, nil
	}

	// Get cluster.
	cls := new(servercore.Cluster)
	err := h.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(clsCfg), cls)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	// Create the referenced secret with the kubeconfig in cluster config.
	eSec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace:    cls.Namespace,
			GenerateName: "gpustack-cluster-config-",
		},
		Data: map[string][]byte{
			_kubeconfig: []byte(clsCfg.Spec.Config),
		},
	}
	kubemeta.ControlOnWithoutBlock(eSec, cls, servercore.SchemeGroupVersionKind("Cluster"))
	aSec, err := kubeclientset.CreateWithCtrlClient(ctx, h.Client, eSec)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	// Update cluster status with the secret name.
	if cls.Spec.Type == servercore.ClusterTypeProxy {
		msg := "Installing worker components, please wait..."
		apistatus.ClusterConditionImported.Unknown(cls, apistatus.ClusterConditionImportedReasonApplyingConfig, msg)
	} else {
		apistatus.ClusterConditionImported.True(cls, "", "")
		apistatus.ClusterConditionConnected.Unknown(cls, "", "")
	}
	cls.Status.ConfigSecretName = aSec.Name

	err = h.Client.Status().Update(ctx, cls)
	if err != nil {
		// Clean up the created secret if failed to update cluster status.
		_ = kubeclientset.DeleteWithCtrlClient(ctx, h.Client, aSec)
		return nil, kerrors.NewInternalError(err)
	}

	return h.OnGet(ctx, ctrlcli.ObjectKeyFromObject(clsCfg), ctrlcli.GetOptions{
		Raw: &meta.GetOptions{ResourceVersion: "0"},
	})
}
