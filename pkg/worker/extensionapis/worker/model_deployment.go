package worker

import (
	"context"
	"errors"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	modelDeploymentForgerProxyFenceRuleKernelArtifact = "the kernelArtifactRef of a bound deployment belongs to the forger that owns it"
	modelDeploymentForgerProxyFenceRuleRoleSet        = "the roles of a bound deployment are managed by the forger that owns it, " +
		"so they cannot be changed while a binding holds"
	modelDeploymentForgerProxyFenceRuleAnnotation = "the forger binding and decision annotations of a bound deployment belong to the forger that owns it"
	modelDeploymentForgerProxyFenceRuleOrphan     = "the forger that owns this deployment still exists"
)

var (
	_ extensionapi.CurdProxyOnCreateBeforeAdvice[*worker.ModelDeployment] = (*ModelDeploymentHandler)(nil)
	_ extensionapi.CurdProxyOnUpdateBeforeAdvice[*worker.ModelDeployment] = (*ModelDeploymentHandler)(nil)
)

// ModelDeploymentHandler exposes the v1 view of a v1alpha1.ModelDeployment.
type ModelDeploymentHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
	apiReader ctrlcli.Reader
}

func (h *ModelDeploymentHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modeldeployments")
	table, err := modelDeploymentTable()
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelDeployment{}
	h.apiReader = opts.Manager.GetAPIReader()
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelDeployment, *worker.ModelDeploymentList,
		*workercore.ModelDeployment, *workercore.ModelDeploymentList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), h.apiReader)
	return gvr, map[string]rest.Storage{
		"metrics": newModelDeploymentMetricsHandler(h.ObjectInfo, opts),
	}, nil
}

func modelDeploymentTable() (rest.TableConvertor, error) {
	return extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Engine", "{.spec.engine.name}"),
		extensionapi.JSONPathTemplateColumn("Roles", "{.status.roleSummary}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"),
		extensionapi.JSONPathTemplateColumn("Endpoint", "{.status.endpoint}"),
	)
}

func (h *ModelDeploymentHandler) New() runtime.Object     { return &worker.ModelDeployment{} }
func (h *ModelDeploymentHandler) Destroy()                {}
func (h *ModelDeploymentHandler) NewList() runtime.Object { return &worker.ModelDeploymentList{} }
func (h *ModelDeploymentHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelDeploymentList{}
}

func (h *ModelDeploymentHandler) CastObjectTo(obj *worker.ModelDeployment) *workercore.ModelDeployment {
	return (*workercore.ModelDeployment)(obj)
}

func (h *ModelDeploymentHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelDeployment,
) *worker.ModelDeployment {
	return (*worker.ModelDeployment)(obj)
}

// BeforeOnCreate refuses forger-binding and forger-decision annotations on the public v1 proxy
// before forwarding with the operator worker's identity.
func (h *ModelDeploymentHandler) BeforeOnCreate(
	_ context.Context, obj *worker.ModelDeployment, _ *ctrlcli.CreateOptions,
) error {
	if obj == nil {
		return nil
	}
	gr := worker.SchemeGroupVersionResource("modeldeployments").GroupResource()
	if _, ok := obj.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey]; ok {
		return kerrors.NewForbidden(gr, obj.Name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
	}
	if _, ok := obj.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey]; ok {
		return kerrors.NewForbidden(gr, obj.Name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
	}
	return nil
}

// BeforeOnUpdate enforces the user-side ModelDeploymentForger single-writer fence on the public v1
// proxy before forwarding writes under the operator worker's identity.
func (h *ModelDeploymentHandler) BeforeOnUpdate(
	ctx context.Context, obj, oldObj *worker.ModelDeployment, _ *ctrlcli.UpdateOptions,
) error {
	if obj == nil {
		return nil
	}
	gr := worker.SchemeGroupVersionResource("modeldeployments").GroupResource()
	ns := obj.Namespace
	if ns == "" && oldObj != nil {
		ns = oldObj.Namespace
	}
	if ns == "" {
		ns = genericapirequest.NamespaceValue(ctx)
	}
	name := obj.Name
	if name == "" && oldObj != nil {
		name = oldObj.Name
	}

	if h.apiReader == nil {
		return kerrors.NewInternalError(errors.New("the deployment cannot be read, because this handler has no reader"))
	}

	liveOld := new(workercore.ModelDeployment)
	if err := h.apiReader.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: name}, liveOld); err != nil {
		if kerrors.IsNotFound(err) {
			return kerrors.NewNotFound(gr, name)
		}
		return kerrors.NewInternalError(fmt.Errorf("read live ModelDeployment %q: %w", name, err))
	}

	md := (*workercore.ModelDeployment)(obj)
	oldBinding, oldHasBinding, oldErr := parseModelDeploymentForgerBinding(liveOld)
	if oldErr != nil {
		return kerrors.NewForbidden(gr, name, fmt.Errorf(
			"the forger binding of this deployment cannot be read, so nothing may be written under it: %w",
			oldErr,
		))
	}
	_, newHasBinding, newErr := parseModelDeploymentForgerBinding(md)
	if newErr != nil {
		return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
	}

	bindingChanged := liveOld.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] !=
		md.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey]
	decisionChanged := liveOld.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey] !=
		md.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey]
	decisionRemoved := liveOld.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey] != "" &&
		md.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey] == ""
	roleSetChanged := modelDeploymentProxyRoleSetChanged(liveOld, md)
	kernelRefsChanged := modelDeploymentProxyKernelArtifactRefsChanged(liveOld, md)

	if !oldHasBinding {
		if bindingChanged {
			return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
		}
		if decisionChanged && !decisionRemoved {
			return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
		}
		return nil
	}

	if !newHasBinding {
		if roleSetChanged {
			return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleRoleSet))
		}
		if kernelRefsChanged {
			return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleKernelArtifact))
		}
		if decisionChanged && !decisionRemoved {
			return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
		}
		return h.verifyForgerBindingIsOrphan(ctx, gr, ns, name, oldBinding)
	}

	if bindingChanged || decisionChanged {
		return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleAnnotation))
	}
	if roleSetChanged {
		return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleRoleSet))
	}
	if kernelRefsChanged {
		return kerrors.NewForbidden(gr, name, errors.New(modelDeploymentForgerProxyFenceRuleKernelArtifact))
	}
	return nil
}

func (h *ModelDeploymentHandler) verifyForgerBindingIsOrphan(
	ctx context.Context, gr schema.GroupResource, namespace, mdName string, binding workercore.ModelDeploymentForgerBinding,
) error {
	forger := new(workercore.ModelDeploymentForger)
	err := h.apiReader.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: binding.ForgerName}, forger)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return kerrors.NewInternalError(fmt.Errorf("the forger %q could not be read: %w", binding.ForgerName, err))
	}
	if string(forger.UID) != binding.ForgerUID {
		return nil
	}
	return kerrors.NewForbidden(gr, mdName, fmt.Errorf(
		"%s: the forger %q still exists", modelDeploymentForgerProxyFenceRuleOrphan, binding.ForgerName,
	))
}

func parseModelDeploymentForgerBinding(
	md *workercore.ModelDeployment,
) (workercore.ModelDeploymentForgerBinding, bool, error) {
	raw, ok := md.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey]
	if !ok || raw == "" {
		return workercore.ModelDeploymentForgerBinding{}, false, nil
	}
	var binding workercore.ModelDeploymentForgerBinding
	if err := json.Unmarshal([]byte(raw), &binding); err != nil {
		return workercore.ModelDeploymentForgerBinding{}, false, fmt.Errorf(
			"the forger binding annotation is not a binding: %w", err,
		)
	}
	return binding, true, nil
}

func modelDeploymentProxyRoleSetChanged(old, md *workercore.ModelDeployment) bool {
	if old == nil || md == nil {
		return false
	}
	if len(old.Spec.Roles) != len(md.Spec.Roles) {
		return true
	}
	names := make(map[string]bool, len(old.Spec.Roles))
	for i := range old.Spec.Roles {
		names[old.Spec.Roles[i].Name] = true
	}
	for i := range md.Spec.Roles {
		if !names[md.Spec.Roles[i].Name] {
			return true
		}
	}
	return false
}

func modelDeploymentProxyKernelArtifactRefsChanged(old, md *workercore.ModelDeployment) bool {
	if old == nil {
		return false
	}
	if len(old.Spec.Roles) != len(md.Spec.Roles) {
		return true
	}
	refs := make(map[string]*core.LocalObjectReference, len(old.Spec.Roles))
	for i := range old.Spec.Roles {
		refs[old.Spec.Roles[i].Name] = old.Spec.Roles[i].KernelArtifactRef
	}
	for i := range md.Spec.Roles {
		was, ok := refs[md.Spec.Roles[i].Name]
		if !ok {
			return true
		}
		now := md.Spec.Roles[i].KernelArtifactRef
		if (was == nil) != (now == nil) {
			return true
		}
		if was != nil && was.Name != now.Name {
			return true
		}
	}
	return false
}
