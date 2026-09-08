package mooncake

import (
	"fmt"
	"maps"
	"slices"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
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
	// LeaderPodIPEnv is the third, and it is defined only under high availability because only the
	// election reads it. See the -rpc_address flag for what it decides.
	LeaderPodIPEnv = "KUBERNETES_POD_IP"
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

// RenderLeaderFlags turns a backend into the argv its leader process runs.
//
// It is a pure function with a deterministic order, so the whole flag surface is testable without a
// cluster and a rendered Deployment diffs cleanly against the last one.
//
// It takes the whole object rather than just the leader spec because the election flags name the
// Lease this backend elects through, and that name is the object's identity.
//
// What it does NOT render is as load-bearing as what it does:
//
//   - No metadata flag of any kind. The metadata plane is peer-to-peer, so there is no store to
//     point at and -etcd_endpoints has nothing to say.
//   - No -port. It is deprecated in favor of -rpc_port.
//   - No flag at the artifact's own default. A flag this spec does not address is absent, so a
//     default that changes upstream shows up as a behavior change to investigate rather than as a
//     value we silently re-asserted.
func RenderLeaderFlags(kvcb *workercore.KVCacheBackend) []string {
	leader := kvcb.Spec.Connection.Managed.Leader

	flags := []string{
		fmt.Sprintf("-rpc_port=%d", LeaderRPCPort),
		fmt.Sprintf("-metrics_port=%d", LeaderMetricsPort),
	}

	// The election, rendered as one group or not at all. Splitting it is what a partial render would
	// do, and each half alone is a specific failure: -enable_ha without a connection string exits at
	// startup, and a connection string without -enable_ha is accepted and ignored with a warning.
	//
	// -ha_backend_type is not a field. The image carries two leadership backends -- the Lease and
	// Redis -- and only the Lease exists inside Kubernetes, so this operator has one value to render
	// and a single-value enum in an API is a name, not a choice.
	//
	// -ha_backend_connstring is "namespace/lease-name", and both halves are derived: a backend is
	// cluster-scoped, its objects live in one shared namespace, and LeaderObjectName is already what
	// keeps two backends' objects apart there. Two backends sharing one Lease would elect one leader
	// between them, which is the failure this whole subject exists to prevent.
	//
	// REQUIRED: -rpc_address belongs to this group even though it looks like a bind setting. The
	// artifact folds it into "rpc_address:rpc_port" and campaigns with that string, which becomes
	// BOTH the election's identity and the address written into the Lease for members to connect
	// to. Left at its 0.0.0.0 default, every replica campaigns under one identity and every member
	// following the Lease is handed 0.0.0.0 -- an address that resolves back to the member itself.
	// The Pod IP is the only value that is unique per replica and reachable from another Pod, and
	// it also binds correctly, because the artifact hands the same string to its RPC server.
	if leader.HighAvailability != nil {
		flags = append(flags,
			"-enable_ha=true",
			"-ha_backend_type=k8s",
			fmt.Sprintf("-ha_backend_connstring=%s/%s",
				kuberess.SystemNamespaceName, LeaderObjectName(kvcb)),
			fmt.Sprintf("-rpc_address=$(%s)", LeaderPodIPEnv),
			// The artifact's own default is a fixed string shared by every deployment, so leaving
			// this alone is what would collide. It keys the store's own namespacing rather than the
			// election, which is why it is derived from the identity rather than from the Lease.
			"-cluster_id="+LeaderObjectName(kvcb))
	}

	// An unset strategy renders nothing rather than a guess: the CRD schema defaults this field, so
	// an empty value means the object never went through admission.
	if mapped, ok := leaderAllocationStrategies[leader.AllocationStrategy]; ok {
		flags = append(flags, "-allocation_strategy="+mapped)
	}

	// Rendered only when asked for. An explicit -enable_multi_tenants=false would be the artifact's
	// own default restated, and would move the command line of every backend that never wanted it.
	//
	// The connector URI is rendered WITH the switch and never apart from it. The master builds its
	// quota policy store when multi-tenancy is on, the file connector refuses the empty URI that is
	// that flag's own default, and the constructor rethrows the refusal rather than degrading — so
	// the switch alone is a process that does not start. -tenant_quota_connector_type stays absent
	// under the rule above: "file" is already its default, and the workload provides no other kind
	// of source.
	if leader.MultiTenancy {
		flags = append(flags,
			"-enable_multi_tenants=true",
			"-tenant_quota_connector_uri="+QuotaPolicyFilePath)
	}

	// The disk tier's leader half. Rendered only when asked for, like every other flag here: the
	// artifact's own default is false, so an explicit -enable_offload=false would restate a default
	// and move the command line of every backend that never wanted the tier.
	//
	// -offload_on_evict is rendered only WITH the switch, and never apart from it. The artifact ands
	// the two, so alone it is accepted, echoed back in the startup log, and does nothing — admission
	// refuses that pairing, and this renderer would have no way to report it if one slipped through.
	if offload := leader.Offload; offload != nil && offload.Enabled {
		flags = append(flags, "-enable_offload=true")
		if offload.OnEvict {
			flags = append(flags, "-offload_on_evict=true")
		}
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
