package nodefeature

import (
	core "k8s.io/api/core/v1"
)

// TopologyProfileLabel partitions Nodes by their ordered topology label-key set: the
// NodeTopologyReconciler stamps it and every topology-qualified flavor name carries it, so a
// flavor's hardware identity alone never decides a match — the node set's profile is part of
// the flavor's identity. Owned here because it is label algebra both the worker's reconcilers
// and the shared placement resolution read; the worker package keeps a same-valued alias.
const TopologyProfileLabel = "topology.gpustack.ai/profile"

// TopologyQualifiedFlavorName joins a flavor's hardware name with the topology profile of the
// node set that contributes it: flavor names are never used unqualified, because the same
// hardware under two profiles publishes two flavors and two pools.
func TopologyQualifiedFlavorName(flavorName, profile string) string {
	return flavorName + "-" + profile
}

// MatchNodeFlavor returns the node's flavor whose topology-qualified name equals flavorName, or
// nil when the node contributes no such flavor — an unprofiled node contributes nothing at all.
func MatchNodeFlavor(nd *core.Node, flavorName string) *NodeFlavor {
	profile := nd.Labels[TopologyProfileLabel]
	if profile == "" {
		return nil
	}
	for _, f := range ExtractNodeFlavors(nd) {
		if TopologyQualifiedFlavorName(f.Name, profile) == flavorName {
			matched := f
			return &matched
		}
	}
	return nil
}
