// Package kvcache renders the workloads of a KVCacheBackend and reads back what they report.
//
// This file holds only the rules governing the extraArgs escape hatch, because two callers must
// agree on them: the renderers emit the derived flags, and the admission webhook refuses an
// extraArgs entry that would fight one. Keeping the lists here rather than in either caller is what
// makes "two sources for one flag" impossible.
//
// Every entry below is read from the artifact's own source at the version this project pins.
package kvcache

// ExtraArgsRules is what admission enforces on one side's extraArgs. The three kinds are separate
// because they refuse for different reasons and an operator needs to hear which:
//
//   - Derived: this API already renders the flag from a field, so two sources would make the
//     rendered result ambiguous.
//   - Exclusive: the artifact accepts either flag but not both, and setting both leaves the outcome
//     to the artifact rather than to the manifest.
//   - Forbidden: the flag changes how every OTHER flag is read. Nothing collides by name, which is
//     exactly why this class needs to exist.
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
		"metrics_port",
		"pod_name",
		"pod_namespace",
		"rpc_port",
	},

	// The artifact accepts an explicit RPC address or the interface to derive one from. Setting
	// both leaves the address it binds decided by the artifact rather than by the manifest.
	Exclusive: [][]string{
		{"rpc_address", "rpc_interface"},
	},

	Forbidden: map[string]string{
		// Measured in the artifact's source: main() loads the config file first and then calls
		// LoadConfigFromCmdline(config, conf_set), which guards most of its assignments with
		// "if (!conf_set)". A config file therefore makes the command line largely INERT — the
		// reverse of the usual precedence, and silently so.
		//
		// Setting this through the escape hatch would void every flag rendered above it: the
		// ports, the allocation strategy, the pod identity. Nothing would report it.
		"config_path": "it makes the artifact ignore the rest of the command line, " +
			"so every flag rendered from this spec would be silently discarded",
	},
}

// MemberExtraArgsRules governs a member group's passthrough.
//
// These are CONFIG keys, not environment-variable names: the member's extraArgs is keyed the way
// its own entrypoint documents, and the renderer maps each to its MOONCAKE_* variable. The tiering
// knobs — offload, promotion, eviction — are deliberately absent, because reaching them is what the
// escape hatch is for.
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
		"global_segment_size",
		"local_buffer_size",
		"local_hostname",
		"master_server_address",
		"metadata_server",
		"protocol",
	},
}
