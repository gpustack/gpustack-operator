package kvcache

import (
	"fmt"
	"maps"
	"slices"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const (
	// LeaderRPCPort is the port the leader serves its RPC on, and the port an engine client
	// connects to. It is pinned rather than left to the artifact's own default because the
	// Service and the published endpoint have to name a number, and a default that moved would
	// move them silently.
	LeaderRPCPort = 50051
	// LeaderMetricsPort is the port the leader serves BOTH its Prometheus exposition and its HTTP
	// admin API on. One port, two surfaces — that is the artifact's design, not a simplification
	// here, and it is why the published admin endpoint and the scrape target are the same address.
	LeaderMetricsPort = 9003

	// LeaderPodNameEnv and LeaderPodNamespaceEnv are the environment variables the rendered argv
	// refers to. The workload that runs this argv has to define both from the downward API, which
	// is what makes the reference resolve.
	//
	// They carry this repository's own names rather than the bare POD_NAME / POD_NAMESPACE the
	// flag documents as its default source. Nothing is lost by that: the flags are rendered
	// explicitly, so the artifact never falls back to reading them itself, and every other
	// component here already spells them this way.
	LeaderPodNameEnv      = "KUBERNETES_POD_NAME"
	LeaderPodNamespaceEnv = "KUBERNETES_POD_NAMESPACE"
)

// leaderAllocationStrategies maps this API's spelling of an allocation strategy onto the artifact's.
// This is the one place the two vocabularies meet: the API offers CamelCase because every enum in
// this group does, the flag takes snake_case, and a second copy of this table is how they drift.
//
// It is deliberately not every value the flag accepts — see the API type for why.
var leaderAllocationStrategies = map[string]string{
	"Random":         "random",
	"FreeRatioFirst": "free_ratio_first",
}

// RenderLeaderFlags turns a leader spec into the argv its process runs.
//
// It is a pure function with a deterministic order, so the whole flag surface is testable without a
// cluster and a rendered Deployment diffs cleanly against the last one.
//
// What it does NOT render is as load-bearing as what it does:
//
//   - No metadata flag of any kind. The metadata plane is peer-to-peer, so there is no store to
//     point at and -etcd_endpoints has nothing to say.
//   - No -enable_ha, -ha_backend_type or -ha_backend_connstring. Those belong to the axis that
//     elects a leader among several, which this scope refuses at admission.
//   - No -port. It is deprecated in favor of -rpc_port.
//   - No flag at the artifact's own default. A flag this spec does not address is absent, so a
//     default that changes upstream shows up as a behavior change to investigate rather than as a
//     value we silently re-asserted.
func RenderLeaderFlags(leader workercore.KVCacheBackendLeader) []string {
	flags := []string{
		fmt.Sprintf("-rpc_port=%d", LeaderRPCPort),
		fmt.Sprintf("-metrics_port=%d", LeaderMetricsPort),
	}

	// An unset strategy renders nothing rather than a guess: the CRD schema defaults this field, so
	// an empty value means the object never went through admission.
	if mapped, ok := leaderAllocationStrategies[leader.AllocationStrategy]; ok {
		flags = append(flags, "-allocation_strategy="+mapped)
	}

	flags = append(flags,
		fmt.Sprintf("-pod_name=$(%s)", LeaderPodNameEnv),
		fmt.Sprintf("-pod_namespace=$(%s)", LeaderPodNamespaceEnv))

	// The escape hatch goes last and in key order, so two renders of one spec are byte-identical
	// and a passthrough can override nothing the lines above already decided — admission refuses a
	// key that collides with a derived flag, which is what keeps that true.
	for _, key := range slices.Sorted(maps.Keys(leader.ExtraArgs)) {
		flags = append(flags, fmt.Sprintf("-%s=%s", key, leader.ExtraArgs[key]))
	}

	return flags
}
