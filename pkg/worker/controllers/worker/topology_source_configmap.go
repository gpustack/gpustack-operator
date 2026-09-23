package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func decodeTopologySnapshot(raw []byte) (topologySnapshot, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var snapshot topologySnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return topologySnapshot{}, fmt.Errorf("decode topology snapshot: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return topologySnapshot{}, fmt.Errorf("topology snapshot contains more than one document")
		}
		return topologySnapshot{}, fmt.Errorf("decode topology snapshot trailing document: %w", err)
	}
	return snapshot, nil
}

func (r *TopologySourceReconciler) reconcileTopologySourceConfigMap(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
) (ctrl.Result, error) {
	configMap := new(core.ConfigMap)
	ref := source.Spec.ConfigMap.ConfigMapRef
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, configMap); err != nil {
		return r.markTopologySourceConfigMapInvalid(ctx, source, before, "ConfigMapReadFailed", err)
	}
	raw, found := configMap.Data[source.Spec.ConfigMap.Key]
	if !found {
		err := fmt.Errorf("ConfigMap key %q is missing", source.Spec.ConfigMap.Key)
		return r.markTopologySourceConfigMapInvalid(ctx, source, before, "SnapshotKeyMissing", err)
	}
	snapshot, err := decodeTopologySnapshot([]byte(raw))
	if err != nil {
		return r.markTopologySourceConfigMapInvalid(ctx, source, before, "SnapshotInvalid", err)
	}
	return r.applyTopologySourceSnapshot(ctx, source, before, snapshot)
}

func (r *TopologySourceReconciler) applyTopologySourceSnapshot(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
	snapshot topologySnapshot,
) (ctrl.Result, error) {
	selector, err := meta.LabelSelectorAsSelector(&source.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes, ctrlcli.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, err
	}
	selected := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		selected[nodes.Items[i].Name] = struct{}{}
	}
	if err := validateTopologySnapshot(source, snapshot, selected); err != nil {
		return r.markTopologySourceConfigMapInvalid(ctx, source, before, "SnapshotInvalid", err)
	}
	conflicted, conflictingSources, err := r.topologySourceConflictedNodes(ctx, source, nodes.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflicted > 0 {
		if err := r.cleanupTopologySourceNodeFeatures(ctx, source); err != nil {
			return ctrl.Result{}, err
		}
		source.Status.SelectedNodes = int32(len(nodes.Items))
		source.Status.ConflictedNodes = int32(conflicted)
		conflictMessage := fmt.Sprintf("writing sources %q select the same Nodes and levels", strings.Join(conflictingSources, ", "))
		TopologySourceConditionReady.False(source, "OwnershipConflict", conflictMessage)
		TopologySourceConditionValid.True(source, "Observed", "topology snapshot is valid but cannot claim contested labels")
		TopologySourceConditionOwnershipConflict.True(source, "OverlappingSources", conflictMessage)
		if _, err := r.updateTopologySourceStatus(ctx, source, before); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	desired := make(map[string]map[string]string, len(snapshot.Nodes))
	for i := range nodes.Items {
		labels, describesNode := snapshot.Nodes[nodes.Items[i].Name]
		if !describesNode {
			continue
		}
		published, err := topologySourcePublishedLabels(&nodes.Items[i], labels)
		if err != nil {
			return r.markTopologySourceConfigMapInvalid(ctx, source, before, "SnapshotInvalid", err)
		}
		if len(published) > 0 {
			desired[nodes.Items[i].Name] = published
		}
	}
	mutated, err := r.reconcileTopologySourceNodeFeatures(ctx, source, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	source.Status.MutatedNodes = mutated
	now := meta.NewTime(r.now())
	source.Status.SelectedNodes = int32(len(nodes.Items))
	source.Status.LastSuccessfulRevision = snapshot.Revision
	source.Status.LastSuccessfulRefreshTime = &now
	TopologySourceConditionReady.True(source, "Observed", "topology snapshot is published through NodeFeatures")
	TopologySourceConditionValid.True(source, "Observed", "topology snapshot is valid")
	TopologySourceConditionOwnershipConflict.False(source, "NoConflict", "topology snapshot owns no contested labels")
	return r.updateTopologySourceStatus(ctx, source, before)
}

func (r *TopologySourceReconciler) topologySourceConflictedNodes(
	ctx context.Context,
	source *workercore.TopologySource,
	nodes []core.Node,
) (int, []string, error) {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		return 0, nil, err
	}
	conflicted := make(map[string]struct{})
	conflictingSources := make(map[string]struct{})
	for i := range sources.Items {
		other := &sources.Items[i]
		if other.UID == source.UID || (other.UID == "" && other.Name == source.Name) || other.Spec.NodeLabels != nil {
			continue
		}
		if !topologySourceLevelsOverlap(source.Spec.Levels, other.Spec.Levels) {
			continue
		}
		selector, err := meta.LabelSelectorAsSelector(&other.Spec.NodeSelector)
		if err != nil {
			return 0, nil, err
		}
		selected := false
		for j := range nodes {
			if selector.Matches(labels.Set(nodes[j].Labels)) {
				conflicted[nodes[j].Name] = struct{}{}
				selected = true
			}
		}
		if selected {
			conflictingSources[other.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(conflictingSources))
	for name := range conflictingSources {
		names = append(names, name)
	}
	slices.Sort(names)
	return len(conflicted), names, nil
}

func topologySourceLevelsOverlap(first, second []string) bool {
	levels := make(map[string]struct{}, len(first))
	for _, level := range first {
		levels[level] = struct{}{}
	}
	for _, level := range second {
		if _, found := levels[level]; found {
			return true
		}
	}
	return false
}

func (r *TopologySourceReconciler) markTopologySourceConfigMapInvalid(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
	reason string,
	err error,
) (ctrl.Result, error) {
	return r.markTopologySourceSnapshotInvalid(
		ctx,
		source,
		before,
		reason,
		err,
		source.Spec.ConfigMap.MaxStaleness.Duration,
		"ConfigMap snapshot",
	)
}

func (r *TopologySourceReconciler) markTopologySourceSnapshotInvalid(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
	reason string,
	err error,
	maxStaleness time.Duration,
	sourceDescription string,
) (ctrl.Result, error) {
	if source.Status.LastSuccessfulRefreshTime != nil {
		age := r.now().Sub(source.Status.LastSuccessfulRefreshTime.Time)
		if age < maxStaleness {
			TopologySourceConditionReady.False(source, "Stale", "retaining the last valid "+sourceDescription)
			TopologySourceConditionValid.False(source, reason, err.Error())
			TopologySourceConditionOwnershipConflict.False(source, "NoConflict", "no contested labels were written")
			if _, updateErr := r.updateTopologySourceStatus(ctx, source, before); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: maxStaleness - age}, nil
		}
		if cleanupErr := r.cleanupTopologySourceNodeFeatures(ctx, source); cleanupErr != nil {
			return ctrl.Result{}, cleanupErr
		}
		TopologySourceConditionReady.False(source, "Expired", "the last valid "+sourceDescription+" expired and owned NodeFeatures were removed")
	} else {
		TopologySourceConditionReady.False(source, reason, sourceDescription+" is unavailable")
	}
	TopologySourceConditionValid.False(source, reason, err.Error())
	TopologySourceConditionOwnershipConflict.False(source, "NoConflict", "no contested labels were written")
	return r.updateTopologySourceStatus(ctx, source, before)
}

func (r *TopologySourceReconciler) cleanupTopologySourceNodeFeatures(ctx context.Context, source *workercore.TopologySource) error {
	mutated, err := r.reconcileTopologySourceNodeFeatures(ctx, source, nil)
	source.Status.MutatedNodes = mutated
	return err
}
