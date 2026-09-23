package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const topologySourceUIDLabel = "topology.gpustack.ai/source-uid"

func topologySourceNodeFeatureName(sourceUID types.UID, nodeName string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(sourceUID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(nodeName))
	return fmt.Sprintf("gpustack-topology-%016x", hash.Sum64())
}

func topologySourcePublishedLabels(node *core.Node, labels map[string]string) (map[string]string, error) {
	published := make(map[string]string)
	for key, value := range labels {
		if isTopologySourceStandardLabel(key) {
			if node.Labels[key] != value {
				return nil, fmt.Errorf("standard label %q on Node %q must already equal %q", key, node.Name, value)
			}
			continue
		}
		published[key] = value
	}
	return published, nil
}

func isTopologySourceNodeFeature(feature *nfd.NodeFeature, source *workercore.TopologySource) bool {
	if owner := kubemeta.GetControllerOfNoCopy(feature); owner != nil {
		return owner.UID == source.UID
	}
	nodeName := feature.Labels[nfd.NodeFeatureObjNodeNameLabel]
	return nodeName != "" && feature.Labels[topologySourceUIDLabel] == string(source.UID) &&
		feature.Name == topologySourceNodeFeatureName(source.UID, nodeName)
}

func (r *TopologySourceReconciler) reconcileTopologySourceNodeFeatures(
	ctx context.Context,
	source *workercore.TopologySource,
	desired map[string]map[string]string,
) (int32, error) {
	features := new(nfd.NodeFeatureList)
	if err := r.Client.List(ctx, features, ctrlcli.InNamespace(kuberess.SystemNamespaceName)); err != nil {
		return 0, err
	}
	mutated := int32(0)
	for i := range features.Items {
		feature := &features.Items[i]
		if !isTopologySourceNodeFeature(feature, source) {
			continue
		}
		nodeName := feature.Labels[nfd.NodeFeatureObjNodeNameLabel]
		if _, found := desired[nodeName]; found && feature.Name == topologySourceNodeFeatureName(source.UID, nodeName) {
			continue
		}
		if err := r.Client.Delete(ctx, feature); err != nil && !kerrors.IsNotFound(err) {
			return mutated, err
		}
		mutated++
	}
	for nodeName, labels := range desired {
		if len(labels) == 0 {
			continue
		}
		key := ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(source.UID, nodeName)}
		feature := new(nfd.NodeFeature)
		err := r.Client.Get(ctx, key, feature)
		if kerrors.IsNotFound(err) {
			feature = &nfd.NodeFeature{ObjectMeta: meta.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
				Labels: map[string]string{
					nfd.NodeFeatureObjNodeNameLabel: nodeName,
					topologySourceUIDLabel:          string(source.UID),
				},
			}}
			feature.Spec = *nfd.NewNodeFeatureSpec()
			feature.Spec.Labels = maps.Clone(labels)
			kubemeta.ControlOnWithoutBlock(feature, source, workercore.SchemeGroupVersionKind("TopologySource"))
			if err := r.Client.Create(ctx, feature); err != nil {
				return mutated, err
			}
			mutated++
			continue
		}
		if err != nil {
			return mutated, err
		}
		if !isTopologySourceNodeFeature(feature, source) {
			return mutated, fmt.Errorf("NodeFeature %s is not owned by TopologySource %q", key, source.Name)
		}
		updated := feature.DeepCopy()
		if updated.Labels == nil {
			updated.Labels = make(map[string]string)
		}
		updated.Labels[nfd.NodeFeatureObjNodeNameLabel] = nodeName
		updated.Labels[topologySourceUIDLabel] = string(source.UID)
		updated.Spec = *nfd.NewNodeFeatureSpec()
		updated.Spec.Labels = maps.Clone(labels)
		kubemeta.ControlOnWithoutBlock(updated, source, workercore.SchemeGroupVersionKind("TopologySource"))
		if kubemeta.DeepEqual(feature.Labels, updated.Labels) && kubemeta.DeepEqual(feature.Spec, updated.Spec) &&
			kubemeta.DeepEqual(feature.OwnerReferences, updated.OwnerReferences) {
			continue
		}
		if err := r.Client.Update(ctx, updated); err != nil {
			return mutated, err
		}
		mutated++
	}
	return mutated, nil
}
