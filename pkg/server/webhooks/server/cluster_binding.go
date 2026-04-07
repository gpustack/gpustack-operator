package server

import (
	"context"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/webhook"
)

// ClusterBindingWebhook hooks a v1alpha1.ClusterBinding object.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="server.gpustack.ai",version="v1alpha1",resource="clusterbindings",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ClusterBindingWebhook struct {
	webhook.DefaultValidator

	Client ctrlcli.Client
}

func (r *ClusterBindingWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()

	return &servercore.ClusterBinding{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*ClusterBindingWebhook)(nil)

func (r *ClusterBindingWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	cb := obj.(*servercore.ClusterBinding)

	// Validate if the name is same as the cluster name.
	if cb.Name != cb.ClusterRef.Name {
		return nil, field.Invalid(
			field.NewPath("metadata.name"), cb.Name,
			"the name of cluster binding must be same as the cluster name")
	}

	// Validate if the cluster binding be applicable to the project.
	{
		cls := &servercore.Cluster{
			ObjectMeta: meta.ObjectMeta{
				Name:      cb.ClusterRef.Name,
				Namespace: cb.ClusterRef.Namespace,
			},
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(cls), cls)
		if err != nil {
			return nil, field.Invalid(
				field.NewPath("clusterRef"), cb.ClusterRef, "cluster not found")
		}
		proj := new(server.Project)
		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cb.Namespace, Namespace: cls.Namespace}, proj)
		if err != nil {
			return nil, field.Invalid(
				field.NewPath("metadata.namespace"), cb.Namespace, "project not found")
		}
	}

	return nil, nil
}

func (r *ClusterBindingWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	oldCb, newCb := oldObj.(*servercore.ClusterBinding), newObj.(*servercore.ClusterBinding)

	// Validate immutable fields.
	if !kubemeta.DeepEqual(oldCb.ClusterRef, newCb.ClusterRef) {
		return nil, field.Invalid(
			field.NewPath("clusterRef"), oldCb.ClusterRef, "immutable field")
	}

	return nil, nil
}
