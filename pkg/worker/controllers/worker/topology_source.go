package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontrollerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
)

const (
	topologySnapshotAPIVersion = "topology.gpustack.ai/v1alpha1"
	topologySourceFinalizer    = "worker.gpustack.ai/topology-source"
	topologySourceRegionLabel  = "topology.gpustack.ai/region"
	topologySourceZoneLabel    = "topology.gpustack.ai/zone"
	topologySourceRackLabel    = "topology.gpustack.ai/rack"

	TopologySourceConditionReady             kubeapistatus.ConditionType = "Ready"
	TopologySourceConditionValid             kubeapistatus.ConditionType = "Valid"
	TopologySourceConditionOwnershipConflict kubeapistatus.ConditionType = "OwnershipConflict"
)

type topologySnapshot struct {
	APIVersion string                       `json:"apiVersion" yaml:"apiVersion"`
	Revision   string                       `json:"revision" yaml:"revision"`
	Nodes      map[string]map[string]string `json:"nodes" yaml:"nodes"`
}

// TopologySourceReconciler observes inventories and publishes writable labels through NodeFeatures.
type TopologySourceReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
	Now       func() time.Time
}

var _ ctrlreconcile.Reconciler = (*TopologySourceReconciler)(nil)

func (r *TopologySourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)
	source := new(workercore.TopologySource)
	if err := r.Client.Get(ctx, req.NamespacedName, source); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	if source.DeletionTimestamp != nil {
		if !ctrlcontrollerutil.ContainsFinalizer(source, topologySourceFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanupTopologySourceNodeFeatures(ctx, source); err != nil {
			return ctrl.Result{}, err
		}
		ctrlcontrollerutil.RemoveFinalizer(source, topologySourceFinalizer)
		return ctrl.Result{}, r.Client.Update(ctx, source)
	}

	before := source.DeepCopy().Status
	source.Status.ObservedGeneration = source.Generation
	source.Status.SourceKind = topologySourceKind(source)
	source.Status.MutatedNodes = 0
	source.Status.ConflictedNodes = 0

	if source.Spec.ConfigMap != nil {
		if !ctrlcontrollerutil.ContainsFinalizer(source, topologySourceFinalizer) {
			ctrlcontrollerutil.AddFinalizer(source, topologySourceFinalizer)
			if err := r.Client.Update(ctx, source); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.reconcileTopologySourceConfigMap(ctx, source, before)
	}
	if source.Spec.Webhook != nil {
		if !ctrlcontrollerutil.ContainsFinalizer(source, topologySourceFinalizer) {
			ctrlcontrollerutil.AddFinalizer(source, topologySourceFinalizer)
			if err := r.Client.Update(ctx, source); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.reconcileTopologySourceWebhook(ctx, source, before)
	}
	if source.Spec.NodeLabels != nil && ctrlcontrollerutil.ContainsFinalizer(source, topologySourceFinalizer) {
		if err := r.cleanupTopologySourceNodeFeatures(ctx, source); err != nil {
			return ctrl.Result{}, err
		}
		ctrlcontrollerutil.RemoveFinalizer(source, topologySourceFinalizer)
		if err := r.Client.Update(ctx, source); err != nil {
			return ctrl.Result{}, err
		}
	}
	if source.Spec.NodeLabels == nil {
		source.Status.SelectedNodes = 0
		TopologySourceConditionReady.False(source, "PendingInventoryAdapter", "the inventory adapter is not active")
		TopologySourceConditionValid.Unknown(source, "PendingInventoryAdapter", "the inventory adapter is not active")
		TopologySourceConditionOwnershipConflict.False(source, "NoConflict", "the inventory adapter has not claimed Nodes")
		return r.updateTopologySourceStatus(ctx, source, before)
	}

	selector, err := meta.LabelSelectorAsSelector(&source.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes, ctrlcli.MatchingLabelsSelector{Selector: selector}); err != nil {
		logger.Error(err, "list selected Nodes")
		return ctrl.Result{}, err
	}
	source.Status.SelectedNodes = int32(len(nodes.Items))
	if err := validateTopologyNodeHierarchy(normalizeTopologyLevels(source.Spec.Levels), nodes.Items); err != nil {
		TopologySourceConditionReady.False(source, "InvalidHierarchy", "selected Node labels do not form a topology tree")
		TopologySourceConditionValid.False(source, "InvalidHierarchy", err.Error())
		TopologySourceConditionOwnershipConflict.False(source, "ReadOnly", "nodeLabels does not claim Node labels")
		return r.updateTopologySourceStatus(ctx, source, before)
	}
	TopologySourceConditionReady.True(source, "Observed", "selected Node labels are available")
	TopologySourceConditionValid.True(source, "Observed", "selected Node labels are valid input")
	TopologySourceConditionOwnershipConflict.False(source, "ReadOnly", "nodeLabels does not claim Node labels")
	return r.updateTopologySourceStatus(ctx, source, before)
}

func (r *TopologySourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *TopologySourceReconciler) updateTopologySourceStatus(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
) (ctrl.Result, error) {
	if kubemeta.DeepEqual(before, source.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Client.Status().Update(ctx, source)
}

func topologySourceKind(source *workercore.TopologySource) string {
	switch {
	case source.Spec.NodeLabels != nil:
		return "NodeLabels"
	case source.Spec.ConfigMap != nil:
		return "ConfigMap"
	case source.Spec.Webhook != nil:
		return "Webhook"
	default:
		return ""
	}
}

func (r *TopologySourceReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("topologysource").
		For(&workercore.TopologySource{}).
		Watches(
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTopologySourcesWhenNodeChanged),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					oldNode, newNode := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
					return !kubemeta.DeepEqual(oldNode.Labels, newNode.Labels)
				},
			}),
		).
		Watches(
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueWritingTopologySourcesWhenNodeChanged),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				UpdateFunc: func(ctrlevent.UpdateEvent) bool { return false },
			}),
		).
		Watches(
			&core.ConfigMap{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTopologySourcesWhenConfigMapChanged),
		).
		Watches(
			&core.Secret{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTopologySourcesWhenSecretChanged),
		).
		Watches(
			&nfd.NodeFeature{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueTopologySourcesWhenNodeFeatureChanged),
		).
		Complete(r)
}

func (r *TopologySourceReconciler) enqueueTopologySourcesWhenNodeFeatureChanged(_ context.Context, feature ctrlcli.Object) []ctrlreconcile.Request {
	owner := kubemeta.GetControllerOfNoCopy(feature)
	if owner == nil || owner.APIVersion != workercore.SchemeGroupVersion.String() || owner.Kind != "TopologySource" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: owner.Name}}}
}

func (r *TopologySourceReconciler) enqueueTopologySourcesWhenSecretChanged(ctx context.Context, secret ctrlcli.Object) []ctrlreconcile.Request {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list topology sources for Secret change")
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0)
	for i := range sources.Items {
		source := &sources.Items[i]
		if source.Spec.Webhook == nil || !topologySourceWebhookReferencesSecret(source.Spec.Webhook, secret) {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(source)})
	}
	return reqs
}

func topologySourceWebhookReferencesSecret(webhook *workercore.TopologySourceWebhook, secret ctrlcli.Object) bool {
	for _, ref := range []*workercore.TopologySourceObjectReference{webhook.BearerTokenSecretRef, webhook.TLSClientCertificateSecretRef} {
		if ref != nil && ref.Namespace == secret.GetNamespace() && ref.Name == secret.GetName() {
			return true
		}
	}
	return false
}

func (r *TopologySourceReconciler) enqueueTopologySourcesWhenNodeChanged(
	ctx context.Context,
	_ ctrlcli.Object,
) []ctrlreconcile.Request {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		if !kerrors.IsNotFound(err) {
			ctrllog.FromContext(ctx).Error(err, "list topology sources for Node change")
		}
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0, len(sources.Items))
	for i := range sources.Items {
		if sources.Items[i].Spec.NodeLabels == nil {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&sources.Items[i])})
	}
	return reqs
}

func (r *TopologySourceReconciler) enqueueWritingTopologySourcesWhenNodeChanged(
	ctx context.Context,
	_ ctrlcli.Object,
) []ctrlreconcile.Request {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list writing topology sources for Node create or delete")
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0, len(sources.Items))
	for i := range sources.Items {
		if sources.Items[i].Spec.ConfigMap == nil && sources.Items[i].Spec.Webhook == nil {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&sources.Items[i])})
	}
	return reqs
}

func (r *TopologySourceReconciler) enqueueTopologySourcesWhenConfigMapChanged(
	ctx context.Context,
	configMap ctrlcli.Object,
) []ctrlreconcile.Request {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list topology sources for ConfigMap change")
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0)
	for i := range sources.Items {
		source := &sources.Items[i]
		matches := false
		if source.Spec.ConfigMap != nil {
			ref := source.Spec.ConfigMap.ConfigMapRef
			matches = ref.Namespace == configMap.GetNamespace() && ref.Name == configMap.GetName()
		}
		if source.Spec.Webhook != nil && source.Spec.Webhook.CABundleConfigMapRef != nil {
			ref := source.Spec.Webhook.CABundleConfigMapRef
			matches = matches || ref.Namespace == configMap.GetNamespace() && ref.Name == configMap.GetName()
		}
		if matches {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(source)})
		}
	}
	return reqs
}

func validateTopologySourceLabel(key, value, additionalWritePrefix string) error {
	if !isTopologySourceStandardLabel(key) && !isTopologySourceWritableLabel(key, additionalWritePrefix) {
		return fmt.Errorf("topology source cannot publish label %q", key)
	}
	if messages := validation.IsValidLabelValue(value); len(messages) > 0 || value == "" {
		return fmt.Errorf("label %q has invalid value %q", key, value)
	}
	return nil
}

func isTopologySourceWritableLabel(key, additionalWritePrefix string) bool {
	if key == TopologyProfileLabel {
		return false
	}
	return strings.HasPrefix(key, "topology.gpustack.ai/") ||
		(additionalWritePrefix != "" && strings.HasPrefix(key, additionalWritePrefix))
}

func isTopologySourceStandardLabel(key string) bool {
	return key == core.LabelTopologyRegion || key == core.LabelTopologyZone
}

func validateTopologySnapshot(source *workercore.TopologySource, snapshot topologySnapshot, selected map[string]struct{}) error {
	if snapshot.APIVersion != topologySnapshotAPIVersion {
		return fmt.Errorf("unsupported topology snapshot apiVersion %q", snapshot.APIVersion)
	}
	if strings.TrimSpace(snapshot.Revision) == "" {
		return fmt.Errorf("topology snapshot revision is required")
	}
	levelIndex := make(map[string]int, len(source.Spec.Levels))
	for i, level := range source.Spec.Levels {
		levelIndex[level] = i
	}
	parents := make(map[string]map[string]string)
	for nodeName, labels := range snapshot.Nodes {
		if _, found := selected[nodeName]; !found {
			return fmt.Errorf("topology snapshot references unknown or unselected Node %q", nodeName)
		}
		for key, value := range labels {
			index, declared := levelIndex[key]
			if !declared {
				return fmt.Errorf("topology snapshot Node %q declares undeclared level %q", nodeName, key)
			}
			if err := validateTopologySourceLabel(key, value, source.Spec.AdditionalWritePrefix); err != nil {
				return fmt.Errorf("topology snapshot Node %q: %w", nodeName, err)
			}
			for parentIndex := 0; parentIndex < index; parentIndex++ {
				if _, found := labels[source.Spec.Levels[parentIndex]]; !found {
					return fmt.Errorf("topology snapshot Node %q has level %q without parent level %q", nodeName, key, source.Spec.Levels[parentIndex])
				}
			}
		}
		for i := 1; i < len(source.Spec.Levels); i++ {
			child := source.Spec.Levels[i]
			childValue, found := labels[child]
			if !found {
				continue
			}
			parentTuple := ""
			for parentIndex := 0; parentIndex < i; parentIndex++ {
				if parentIndex > 0 {
					parentTuple += "\x00"
				}
				parentTuple += labels[source.Spec.Levels[parentIndex]]
			}
			byValue := parents[child]
			if byValue == nil {
				byValue = make(map[string]string)
				parents[child] = byValue
			}
			if existing, exists := byValue[childValue]; exists && existing != parentTuple {
				return fmt.Errorf("topology snapshot level %q value %q has conflicting parent tuples", child, childValue)
			}
			byValue[childValue] = parentTuple
		}
	}
	return nil
}
