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
//     read, another names a store nothing reads, another refuses to start the process at all,
//     another replaces a setting this spec states while leaving the object stating it, another
//     decides whether a feature this API renders runs at all under a name that mentions no part of
//     it, and several change no value because this operator's own rendering always wins over them,
//     which makes them configure nothing while reading as a setting that moved.
type ExtraArgsRules struct {
	Derived   []string
	Exclusive [][]string
	Forbidden map[string]string
}

// LeaderExtraArgsRules governs the leader's passthrough.
//
// The two lists are COMPLETE against the artifact version this project pins, rather than a record
// of the names that happened to be noticed. Every setting this API renders from a field was traced
// to the write points of its effective value in the artifact's own source, and every other flag
// reaching one of them is listed below. Where that value lives in a const member, the constructor's
// initializer list is the whole write set — which is what makes "complete" a claim a reader can
// check instead of one this comment asserts.
//
// REQUIRED: moving the pinned artifact version invalidates the tracing, so redo it in the same
// commit. A flag added upstream reaches a rendered setting under a name no rule here has, and the
// refusals below are keyed on names — nothing else in this repository would notice.
//
// metrics_host is the one the trace found and DELIBERATELY LEFT REACHABLE, recorded here so the
// next trace does not add it. It moves where the admin and Prometheus surfaces bind, which the two
// probes reach at the Pod's own address -- but its artifact documents "::" as the value that listens
// on IPv6 and, on a dual-stack host, on both. On a cluster that assigns the Pod an IPv6 address, the
// artifact's own 0.0.0.0 default answers no probe at all, so this key is the only way to a leader
// that becomes ready, and refusing it would cost far more than the hatch is worth. A concrete
// address that is not the Pod's own still breaks the probes; that failure stops the rollout rather
// than passing silently, which is the line this list draws.
var LeaderExtraArgsRules = ExtraArgsRules{
	// Keys are the flag's own name without its leading dashes, and that is NOT how extraArgs
	// entries arrive: an entry carries its dashes and may carry its value, so admission strips the
	// dashes and everything from the first "=" on before it compares a key against these tables.
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
		// Both halves of the disk tier's leader switch. They are derived from members[].localDisks —
		// a group declaring a tier is what turns the tier on — and reaching them through the hatch
		// would put the leader's half of that decision out of step with the members' half, with
		// nothing on the object saying so.
		"enable_offload",
		// The snapshot group, derived from leader.highAvailability.snapshot. Reserved
		// UNCONDITIONALLY, like the election group above, and for a sharper version of the same
		// reason: the two switches say WHERE nothing, and the directory they write and read is an
		// environment variable set beside the mounted claim. Reached through the hatch without that
		// claim, enable_snapshot names a local object store whose path variable is unset, and the
		// master refuses to start -- which is the good case. The one to keep out is the object
		// admitted with a claim and a passthrough switch disagreeing about whether to use it.
		//
		// The last two are rendered only when their fields are set, and are reserved all the same:
		// a key accepted here would win over a field left unset, so the object would state a
		// default it is not running with.
		"enable_snapshot",
		"enable_snapshot_restore",
		"snapshot_interval_seconds",
		"snapshot_object_store_type",
		"snapshot_retention_count",
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

		// The CXL switch, and a fourth kind of cost: this one does not shade a setting or refuse to
		// start -- it REPLACES a setting the object states, and the object goes on stating it.
		//
		// Measured in the artifact's source: the master resolves its strategy type from this flag
		// before construction finishes and then replaces the strategy object during startup, so
		// the rendered -allocation_strategy is dead rather than overridden late, and the resolved
		// type goes on gating other paths.
		//
		// It does two things, and they are reported differently, which is why the reason names
		// both. The SUBSTITUTION is reported nowhere, at any verbosity. Enabling CXL is reported:
		// at default verbosity the artifact logs the allocator it creates and its size, and the
		// leader's metrics then carry that size as capacity. Those lines name the ALLOCATOR and not
		// the strategy, so they are not a report of the substitution, and reading them as one is
		// the confusion this entry has to avoid making itself.
		//
		// Measured on a cluster whose nodes have no DAX device: the switch ALONE, with neither
		// operand set and both therefore defaulting to /dev/dax0.0 and 8 GiB, brings the leader up
		// serving and advertising eight gigabytes that nothing backs. It neither fails nor degrades.
		//
		// Refusing it removes the ONLY route to CXL through this API, which carries no field for
		// it, and that is deliberate rather than an oversight: the hatch reaches the capability by
		// accident, and what shipping it would take is recorded in the media spec. The line drawn
		// above is what decides it -- a failure that stops a rollout may pass, one that passes
		// silently may not -- and both of this key's effects are on the silent side.
		//
		// Nothing collides by name -- this API renders no CXL flag -- which is why these belong
		// here and not in Derived. The Derived message would also be untrue: it says the key is
		// derived from a field of this spec, and none of these is.
		"enable_cxl": "it replaces the leader's allocation strategy outright, so the " +
			"-allocation_strategy rendered from leader.allocationStrategy is discarded and the " +
			"object goes on stating a strategy the process is not running with nothing reporting " +
			"that substitution, and it brings the leader up advertising the CXL allocator's size " +
			"as capacity, which on a node with no DAX device is capacity nothing backs",

		// Both are read ONLY where enable_cxl is set, which is refused above. Reserved anyway,
		// because a key that is accepted and then configures nothing is how an operator comes to
		// believe a tier is on: the manifest carries a DAX path, the process never reads it, and
		// no status says so. One reason serves both, from a single constant, because two literals
		// saying the same thing drift apart and the field path already names which key was typed.
		"cxl_path": cxlCompanionKeyReason,
		"cxl_size": cxlCompanionKeyReason,

		// The artifact's deprecated spelling of rpc_port, and INERT here rather than harmful: it is
		// read only where rpc_port is left at zero, and this operator renders -rpc_port
		// unconditionally with a fixed positive value that wins. Refused for the reason the CXL
		// operands are -- a key admission accepted and the process then ignores reads as a port
		// that moved, while the Service and the published endpoint go on naming the old one.
		"port": "it is the artifact's deprecated spelling of rpc_port, which this operator always " +
			"renders and which wins over it, so this key moves nothing while reading as a port " +
			"that moved",

		// The etcd leadership backend's endpoints, and out of reach twice over. It is read ONLY as
		// a fallback for an empty ha_backend_connstring, which this operator renders non-empty
		// whenever the election is on; and the backend it names is not in the image, by the same
		// fact that forbids enable_oplog above.
		"etcd_endpoints": "it supplies a connection string only when ha_backend_connstring is " +
			"empty, which this operator never leaves empty, and the etcd leadership backend it " +
			"names is not compiled into the image this operator runs",

		// The allocator, and the fifth kind of cost: it decides whether a feature this API renders
		// EXISTS, under a name that says nothing about that feature.
		//
		// Measured in the artifact's source: the master builds its snapshot manager inside a
		// condition that requires the allocator to be the offset one. That is the artifact's own
		// default, so nothing is wrong today -- and the condition has no else branch and logs
		// nothing, so an administrator who moves the allocator through the hatch turns snapshot
		// generation off while every rendered flag, the mounted claim and this object all go on
		// saying it is on. The standby then restores the last snapshot the leader wrote before the
		// change, forever, and the object reports the arrangement working.
		"memory_allocator": "it decides whether the leader generates snapshots at all -- the " +
			"master builds its snapshot manager only under the default offset allocator, silently " +
			"and with no log line either way -- so any other value leaves " +
			"leader.highAvailability.snapshot rendered, mounted and stated while nothing new is " +
			"ever written to it",

		// The snapshot's SECOND directory, and it is not the one the claim is mounted at: it is a
		// forensic copy the store writes beside a failed upload, and setting it changes what a
		// failure DOES.
		//
		// Measured in the artifact's source: the snapshot manager returns the upload error to its
		// caller only while this is empty. Non-empty, each of the four payload uploads that fails
		// is logged, saved to this directory and then stepped over, and the snapshot round reports
		// itself finished. The setting reads like extra durability and buys the opposite -- a
		// standby bootstrapping from a snapshot that is missing the shard it needed.
		"snapshot_backup_dir": "it turns a failed snapshot upload from an error the store reports " +
			"into a local copy it writes and steps over, so snapshot rounds go on reporting " +
			"success while what lands on the claim is incomplete",

		// The two deprecated spellings of snapshot_object_store_type, and INERT rather than
		// harmful, exactly like the deprecated port above. Measured in the artifact's source: the
		// canonical flag is tested first and wins whenever it was passed, which this operator does
		// unconditionally under leader.highAvailability.snapshot. Refused because a key admission
		// accepted and the process ignores reads as a store type that moved.
		"snapshot_payload_store_type":   snapshotObjectStoreAliasKeyReason,
		"snapshot_payload_backend_type": snapshotObjectStoreAliasKeyReason,

		// The catalog, and the counterpart to reserving the object store: this operator never
		// renders it, because the whole design rests on the artifact's own default being the
		// embedded catalog -- the index of which snapshots exist, written into the object store
		// itself, so one claim carries both halves. That omission is exactly what makes the key
		// reachable from extraArgs. Pointed at a Redis, the payloads stay on the claim while the
		// index moves to a store nothing here creates, seeds or backs up, and a Redis that is later
		// wiped leaves the master restoring nothing from a claim that is still full.
		"snapshot_catalog_store_type":   snapshotCatalogStoreKeyReason,
		"snapshot_catalog_backend_type": snapshotCatalogStoreKeyReason,

		// Read ONLY where the catalog is the Redis one, which is refused above. Reserved anyway,
		// for the reason the CXL operands are: a key that is accepted and then configures nothing
		// is how an operator comes to believe a catalog moved.
		"snapshot_catalog_store_connstring":   snapshotCatalogConnstringKeyReason,
		"snapshot_catalog_backend_connstring": snapshotCatalogConnstringKeyReason,
	},
}

// cxlCompanionKeyReason is why the two CXL operands are refused on their own. They are not a
// setting this API renders, so nothing collides by name; they are inert without the switch that
// carries the real cost, and an inert key that admission accepted reads as a tier that is on.
const cxlCompanionKeyReason = "it is read only when enable_cxl is set, and that key is refused " +
	"here because it discards the -allocation_strategy rendered from leader.allocationStrategy, " +
	"so this key alone configures nothing at all"

// snapshotObjectStoreAliasKeyReason is why the two deprecated spellings of the snapshot object
// store type are refused. The canonical flag is rendered unconditionally under
// leader.highAvailability.snapshot and is read first, so these move nothing while reading as a
// store type that moved. One constant serves both, because two literals saying the same thing
// drift apart and the field path already names which key was typed.
const snapshotObjectStoreAliasKeyReason = "it is a deprecated alias of " +
	"snapshot_object_store_type, which this operator renders from " +
	"leader.highAvailability.snapshot and which wins over it, so this key moves nothing while " +
	"reading as a snapshot store that moved"

// snapshotCatalogStoreKeyReason is why the snapshot catalog's type is refused. It is not a setting
// this API renders, so nothing collides by name -- the embedded catalog is the artifact's own
// default and that default is what puts the index on the same claim as the payloads.
const snapshotCatalogStoreKeyReason = "it moves the index of which snapshots exist off the claim " +
	"leader.highAvailability.snapshot names and into an external store this operator does not " +
	"create, seed or delete, leaving a leader that restores nothing from a claim that still holds " +
	"every payload"

// snapshotCatalogConnstringKeyReason is why the catalog's two connection-string spellings are
// refused on their own: they are read only under a catalog kind that is itself refused.
const snapshotCatalogConnstringKeyReason = "it is read only when the snapshot catalog is the " +
	"external one, and that key is refused here, so this key alone configures nothing at all"

// MemberExtraArgsRules governs a member group's passthrough.
//
// These are CONFIG keys, not environment-variable names: the member's own entrypoint documents
// them as its per-key overrides, and the renderer maps each to its MOONCAKE_* variable. Names the
// client reads from the ENVIRONMENT ONLY are not reachable through this list at all and are not
// listed here; members[].extraEnv is the hatch for those, and MemberDerivedEnvs is what it reserves.
//
// The disk tier's two are NOT reachable any more: they come from members[].localDisks now, so they
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
		// The disk tier's member half, rendered from members[].localDisks. Reserving them matters
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
// admission refuses in a group's extraEnv.
//
// It is a PLAIN LIST and not an ExtraArgsRules, because the other two kinds that type carries would
// both be empty and an empty field invites a reader to wonder what belongs in it. No two of these
// names are alternatives to one another, so there is nothing to make exclusive; and nothing in this
// namespace has the leader's config_path property -- the one that makes a passthrough void every
// OTHER setting -- so there is nothing to forbid outright. A variable here configures the setting it
// names and nothing else, which leaves collision as the only problem worth reporting.
//
// The bucket pair is here for a different reason than the rest. The others protect an invariant
// admission enforces elsewhere -- the tier's two halves, the path that passed the path rules, the
// segment size counted into the Pod's request. The bucket pair protects nothing; it is a value this
// operator CHOSE, and reserving it is the deliberate decision that a tuner who needs to move it gets
// a field rather than a hatch that silently doubles a name.
var MemberDerivedEnvs = []string{
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
	memberEnvVisibleDevicesAMD,
	memberEnvVisibleDevicesCambricon,
	memberEnvVisibleDevicesIluvatar,
	memberEnvVisibleDevicesMThreads,
	memberEnvVisibleDevicesNVIDIA,
}

// LeaderDerivedEnvs is every environment variable name the leader renderer emits, which is what
// admission refuses in the leader's extraEnv.
//
// It is a PLAIN LIST for the same reason MemberDerivedEnvs is: the exclusive and forbidden kinds
// would both be empty here, and nothing in the leader's namespace voids another setting.
//
// The last two are rendered only under high availability and under a snapshot declaration, and are
// reserved UNCONDITIONALLY for the same reason the election flags are: an object must be creatable
// with the variable already in place and the field turned on afterwards, and a passthrough value
// would silently win over the reference the rendered argv or the mounted claim arrives with.
var LeaderDerivedEnvs = []string{
	LeaderPodIPEnv,
	LeaderPodNameEnv,
	LeaderPodNamespaceEnv,
	LeaderSnapshotLocalPathEnv,
}
