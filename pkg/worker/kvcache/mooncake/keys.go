// Package mooncake is everything specific to ONE kv cache backend implementation: the Mooncake
// store. It renders that store's workloads and reads back what they report.
//
// It is a package of its own because every fact in it comes from Mooncake's own source — the
// entrypoint names, the gflag spellings, the admin routes, the Prometheus series, the tenant quota
// policy schema — and none of it would survive contact with a second implementation. The parent
// package holds what is true of any backend. Nothing here is an abstraction over backends: there is
// one, `spec.type` admits one value, and nothing dispatches on it. The split is where a second one
// would be added, not a seam already built for it.
//
// Two conventions the whole package follows:
//
//   - Renderers are PURE functions with deterministic output, so the full surface is testable without
//     a cluster and a re-render diffs cleanly against the last.
//   - Every observed figure is a POINTER. The store serializes only what it has observed, so absent
//     and zero are different facts, and publishing the second for the first is how a warm cache comes
//     to look empty.
//
// This file holds only the rules governing the escape hatches, because two callers must agree on
// them: the renderers emit the derived flags and variables, and the admission webhook refuses an
// entry that would fight one. Keeping the lists here rather than in either caller is what makes "two
// sources for one setting" impossible.
//
// Every entry below is read from the artifact's own source at the version this project pins.
package mooncake

// ExtraArgsRules is what admission enforces on one side's extraArgs. The three kinds are separate
// because they refuse for different reasons and an operator needs to hear which:
//
//   - Derived: this API already renders the flag from a field, so two sources would make the
//     rendered result ambiguous.
//   - Exclusive: the artifact accepts either flag but not both, and setting both leaves the outcome
//     to the artifact rather than to the manifest.
//   - Forbidden: the flag reaches the artifact intact and then costs more than the hatch is worth.
//     Nothing collides by name, which is exactly why this class needs to exist. Each entry carries
//     its own reason because they are not one kind of problem: one changes how every OTHER flag is
//     read, another names a store nothing reads, another refuses to start the process at all.
type ExtraArgsRules struct {
	Derived   []string
	Exclusive [][]string
	Forbidden map[string]string
}

// LeaderExtraArgsRules governs the leader's passthrough.
var LeaderExtraArgsRules = ExtraArgsRules{
	// Keys are the flag's own name without its leading dash, which is how extraArgs is keyed.
	Derived: []string{
		"allocation_strategy",
		// The four the election renders, reserved as one group because that is how they are
		// rendered: together or not at all. Reserved UNCONDITIONALLY, like the offload pair below,
		// even though nothing renders them for a backend without leader.highAvailability -- a
		// passthrough enable_ha with no connection string beside it is a leader that exits at
		// startup, and cluster_id reached this way would key the store's namespace off something
		// the object does not say.
		"cluster_id",
		"enable_ha",
		"ha_backend_connstring",
		"ha_backend_type",
		// The address the election ADVERTISES, and the interface the artifact would derive it
		// from. Reserved for the same reason and in the same breath, even though only the first is
		// rendered: the artifact folds rpc_address and rpc_port into the local_hostname it
		// campaigns with, so that string is both the election's identity and the address a member
		// following the Lease is handed. A passthrough here does not shade a setting -- it decides
		// which host every member connects to, and gets no say in whether the Pod can be reached
		// there.
		"rpc_address",
		"rpc_interface",
		// Both halves of the disk tier's leader switch. They are derived from leader.offload, and
		// reaching them through the hatch would put the tier's two sides out of step with the
		// admission rule that keeps them paired — a leader offloading with no member declaring a
		// tier, or the reverse, with nothing on the object saying so.
		"enable_offload",
		"enable_multi_tenants",
		"metrics_port",
		"offload_on_evict",
		"pod_name",
		"pod_namespace",
		"rpc_port",
		// Rendered from MultiTenancy alongside enable_multi_tenants, and listed here for the same
		// reason. Left out, a passthrough key could point the master at a file this operator neither
		// seeds nor rewrites — every quota write would land somewhere nothing reads, and the pool
		// would go on reporting Ready over it.
		"tenant_quota_connector_uri",
	},

	Forbidden: map[string]string{
		// The artifact's own check is LOG(FATAL): enable_oplog with any ha_backend_type other than
		// etcd refuses to start. The image this operator runs compiles the Kubernetes Lease backend,
		// which upstream's build cannot combine with etcd, so the etcd backend is not reachable
		// here and neither is the oplog.
		//
		// It is refused rather than passed through BECAUSE the failure is total and immediate. An
		// earlier draft of this rule accepted the key and reported the consequence, reasoning that a
		// trade-off belongs to whoever typed it -- that reasoning was about a plaintext-etcd
		// exposure, and it does not survive the flag becoming a crash. There is nothing to trade.
		"enable_oplog": "the leader refuses to start with it: the artifact requires the etcd " +
			"leadership backend for the operation log, and the image this operator runs compiles " +
			"the Kubernetes Lease backend, which upstream cannot build together with etcd",

		// Measured in the artifact's source: main() loads the config file first and then calls
		// LoadConfigFromCmdline(config, conf_set), which guards most of its assignments with
		// "if (!conf_set)". A config file therefore makes the command line largely INERT — the
		// reverse of the usual precedence, and silently so.
		//
		// Setting this through the escape hatch would void every flag rendered above it: the
		// ports, the allocation strategy, the pod identity. Nothing would report it.
		"config_path": "it makes the artifact ignore the rest of the command line, " +
			"so every flag rendered from this spec would be silently discarded",

		// The counterpart to reserving the URI, and reserved here rather than in Derived because
		// this operator never renders it: multi-tenancy is built on "file" being the artifact's
		// own default, which is exactly what makes the omission reachable from extraArgs. Changed
		// to another kind of source, the URI rendered alongside it stops naming a file, and the
		// seeded policy, the writable copy the leader keeps and every quota write the reconciler
		// makes address a store the master is no longer reading.
		"tenant_quota_connector_type": "it decides what kind of source the master reads the " +
			"tenant quota policy from, so anything other than the default file connector leaves " +
			"the policy this operator seeds and rewrites addressing a store nothing reads",
	},
}

// MemberExtraArgsRules governs a member group's passthrough.
//
// These are CONFIG keys, not environment-variable names: the member's extraArgs is keyed the way its
// own entrypoint documents, and the renderer maps each to its MOONCAKE_* variable. Names the client
// reads from the ENVIRONMENT ONLY are not reachable through this map at all and are not listed here;
// members[].extraEnvs is the hatch for those, and MemberDerivedEnvs is what it reserves.
//
// The disk tier's two are NOT reachable any more: they come from members[].localDisk now, so they
// are derived.
//
// There is no Forbidden entry here, and the rendering shape is the reason: a member's extraArgs
// becomes a per-key override, so a key named after a config FILE would set a config key of that
// name rather than pointing the entrypoint at a file. The leader's hole does not exist on this side.
//
// device_name is deliberately NOT derived, and the reason is measured rather than stylistic. The
// renderer leaves MOONCAKE_DEVICE unset, which the client reads as an empty device filter and
// therefore as "use every device found". That is the only correct default here: one DaemonSet covers
// every node its group selects, and an RDMA device is named per host — mlx5_0 on one, erdma_0 on the
// next — so no single name could be rendered for the whole group.
//
// The documented value "auto-discovery" is NOT a special value in the code. The client splits
// device_name on commas into a filter list and nothing special-cases that string, so setting it
// produces a filter matching a device of that literal name, which no host has. The docstring says
// auto-discovery; the behavior is that EMPTY means auto and that string means none.
//
// Leaving the key out of Derived is what gives an operator on heterogeneous hardware a way in: a
// member's extraArgs renders as the entrypoint's own -D override, which is applied AFTER the
// environment and wins over it.
var MemberExtraArgsRules = ExtraArgsRules{
	Derived: []string{
		// The disk tier's member half, rendered from members[].localDisk. Reserving them matters
		// more here than on the leader, because of where a member's extraArgs lands in the
		// precedence chain: a real flag beats a config key, and a config key beats the environment.
		// These two are rendered as ENVIRONMENT, and extraArgs renders as a -D config key — so an
		// entry here does not merely add a second source, it WINS. Left reachable, ssd_offload_path
		// takes a host path that never went through the rules admission applies to the field
		// (absolute, not root, no "..", clear of the RDMA device tree), and enable_ssd_offload
		// switches the tier off while the leader goes on queueing offload work for it.
		//
		// The third key the renderer sets, the tier's size limit, needs no entry: the client's
		// config object has no field of that name, so a -D would set a key nothing reads.
		"enable_ssd_offload",
		"global_segment_size",
		"local_buffer_size",
		"local_hostname",
		"master_server_address",
		"metadata_server",
		"protocol",
		"ssd_offload_path",
	},
}

// MemberDerivedEnvs is every environment variable name the member renderer emits, which is what
// admission refuses in a group's extraEnvs.
//
// It is a PLAIN LIST and not an ExtraArgsRules, because the other two kinds that type carries would
// both be empty and an empty field invites a reader to wonder what belongs in it. No two of these
// names are alternatives to one another, so there is nothing to make exclusive; and nothing in this
// namespace has the leader's config_path property -- the one that makes a passthrough void every
// OTHER setting -- so there is nothing to forbid outright. A variable here configures the setting it
// names and nothing else, which leaves collision as the only problem worth reporting.
//
// It carries the names the renderer emits on SOME paths too. LD_LIBRARY_PATH is here although only
// the EFA transport renders it, because the transport is editable: a backend switched onto that
// fabric later would otherwise carry two definitions of the variable that decides whether the host's
// libfabric loads at all, with the winner left to the runtime.
//
// The bucket pair is here for a different reason than the rest. The others protect an invariant
// admission enforces elsewhere -- the tier's two halves, the path that passed the path rules, the
// segment size counted into the Pod's request. The bucket pair protects nothing; it is a value this
// operator CHOSE, and reserving it is the deliberate decision that a tuner who needs to move it gets
// a field rather than a hatch that silently doubles a name.
var MemberDerivedEnvs = []string{
	"LD_LIBRARY_PATH",
	memberEnvGlobalSegmentSize,
	memberEnvLocalBufferSize,
	memberEnvLocalHostname,
	memberEnvMaster,
	memberEnvMetadataServer,
	memberEnvOffloadBucketKeysLimit,
	memberEnvOffloadBucketMaxTotalSize,
	memberEnvOffloadBucketSizeLimit,
	memberEnvOffloadEnabled,
	memberEnvOffloadEvictionPolicy,
	memberEnvOffloadKeysLimit,
	memberEnvOffloadPath,
	memberEnvOffloadSizeLimit,
	memberEnvOffloadWatermarkEnabled,
	memberEnvOffloadWatermarkHigh,
	memberEnvOffloadWatermarkLow,
	memberEnvProtocol,
}
