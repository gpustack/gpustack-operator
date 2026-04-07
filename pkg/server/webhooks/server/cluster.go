package server

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/server/apistatus"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/webhook"
)

// ClusterWebhook hooks a v1alpha1.Cluster object.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="server.gpustack.ai",version="v1alpha1",resource="clusters",scope="Namespaced",subResources=["status"]
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE","DELETE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ClusterWebhook struct {
	Client ctrlcli.Client
}

func (r *ClusterWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()

	return &servercore.Cluster{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*ClusterWebhook)(nil)

func (r *ClusterWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	cls := obj.(*servercore.Cluster)

	// Validate cluster name.
	if stringx.StringWidth(cls.Name) > 30 {
		return nil, field.TooLong(
			field.NewPath("name"), stringx.StringWidth(cls.Name), 30)
	}

	// Validate reserved cluster name and type.
	if kuberess.IsLocalCluster(cls) {
		if !settings.ImportLocalCluster.ShouldValueBool(ctx) {
			return nil, field.Invalid(
				field.NewPath("metadata.name"), cls.Name, "the cluster name is reserved when enabled ImportLocalCluster setting")
		}
	} else if cls.Spec.Type == servercore.ClusterTypeLoopback {
		return nil, field.Invalid(
			field.NewPath("spec.type"), cls.Spec.Type, "loopback cluster type is reserved for the local cluster")
	}

	// Validate if the cluster be applicable to the team.
	{
		team := &server.Team{
			ObjectMeta: meta.ObjectMeta{
				Name:      cls.Namespace,
				Namespace: kuberess.SystemNamespaceName,
			},
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(team), team)
		if err != nil {
			return nil, field.Invalid(
				field.NewPath("metadata.namespace"), cls.Namespace, "team not found")
		}
	}

	return nil, nil
}

func (r *ClusterWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	oldCls, newCls := oldObj.(*servercore.Cluster), newObj.(*servercore.Cluster)

	// Validate cluster type.
	if oldCls.Spec.Type != newCls.Spec.Type {
		return nil, field.Invalid(
			field.NewPath("spec.type"), oldCls.Spec.Type, "immutable field")
	}

	// Validate cluster condition.
	if apistatus.ClusterConditionDeleting.Exists(newCls) && newCls.DeletionTimestamp == nil {
		return nil, field.Invalid(
			field.NewPath("status.conditions"), newCls.Status.Conditions, "deletion has not been requested yet")
	}

	return nil, nil
}

func (r *ClusterWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	cls := obj.(*servercore.Cluster)

	// Cannot delete the cluster if it is in use.
	cbList := new(servercore.ClusterBindingList)
	err := r.Client.List(ctx, cbList,
		ctrlcli.MatchingFields{
			"spec.clusterRef.namespace": cls.Namespace,
			"spec.clusterRef.name":      cls.Name,
		},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("list related cluster binding: %w", err))
	}
	if len(cbList.Items) > 0 {
		return nil, field.Invalid(
			field.NewPath("metadata.name"), cls.GetName(), "blocked by in-used cluster bindings")
	}

	// Cannot delete the local cluster if ImportLocalCluster is enabled.
	if kuberess.IsLocalCluster(cls) && settings.ImportLocalCluster.ShouldValueBool(ctx) {
		return nil, field.Forbidden(
			field.NewPath("metadata.name"), "deletion of local cluster is forbidden when enabled ImportLocalCluster setting")
	}

	return nil, nil
}
