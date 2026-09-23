// This file renders for SGLang. Its vehicle is the environment, and the reason is evaluation time
// rather than key coverage.
//
// Every upstream coordinate in this file is READ AT v0.5.18 and names its symbol, because the file
// it points into has tripled in length across releases: a bare line number silently comes to mean
// "wherever this is today", while the symbol is what relocates the claim after the file moves.
//
// SGLang picks its configuration source in `_load_config` (v0.5.18 `mooncake_store.py:294-314`): the
// launch argument's extra config, then a file, then the environment. The first two are fixed when the
// caller runs - at admission, before a Pod has an IP - and both fall back key-for-key to
// `envs.<NAME>.default` (v0.5.18 `mooncake_store.py:110-168` `from_file` and `:212-261`
// `load_from_extra_config`), which is the literal attribute assigned in `EnvField.__init__`
// (v0.5.18 `environ.py:39-40`) and not the accessor that reads the process environment
// (`EnvField.get` at v0.5.18 `environ.py:57`, defaulting through `_resolve_default` at `:52-55`).
// So on either of them `local_hostname` resolves to the literal "localhost" on every Pod in the pool.
//
// That key is an address: vLLM computes its own from the local IP (`rdma_utils.py:21-25`). Only the
// environment defers evaluation to the kubelet, where a fieldRef to status.podIP resolves at container
// start. Waiting for an upstream release would not help either, because the value is not missing from a
// schema - it does not exist yet at the moment a file would be written.
//
// The environment path is selected by NOT setting SGLANG_HICACHE_MOONCAKE_CONFIG_PATH, which is why
// this renderer emits no such variable. A user who sets it themselves takes over, silently and
// correctly: an explicit configuration should outrank a defaulted one.
package inject

import (
	"strconv"

	core "k8s.io/api/core/v1"
)

const (
	// The variables SGLang reads through `.get()` in `load_from_env`
	// (v0.5.18 `mooncake_store.py:170-210`).
	//
	// sglangMetadataServerEnv carries a spelling trap worth stating: META_DATA has an underscore the
	// readable METADATA does not. A wrong spelling here does not error - the key simply falls back to
	// its default - so the tests pin this name byte for byte rather than through this constant.
	sglangMasterEnv         = "MOONCAKE_MASTER"
	sglangMetadataServerEnv = "MOONCAKE_TE_META_DATA_SERVER"
	sglangProtocolEnv       = "MOONCAKE_PROTOCOL"
	sglangDeviceEnv         = "MOONCAKE_DEVICE"
	sglangGlobalSegmentEnv  = "MOONCAKE_GLOBAL_SEGMENT_SIZE"
	sglangLocalHostnameEnv  = "MOONCAKE_LOCAL_HOSTNAME"

	// sglangTenantEnv carries the Binding's reuse domain. Whether the selected image reads it is the
	// image owner's compatibility responsibility.
	sglangTenantEnv = "MOONCAKE_TENANT_ID"

	// sglangBackendArg enables the store. Like vLLM's connector argument it has no environment
	// equivalent, so this engine also cannot be configured without appending an argument.
	sglangBackendArg   = "--hicache-storage-backend"
	sglangBackendValue = "mooncake"

	// The disaggregation arguments, which name the prefill/decode split itself. All three are
	// ServerArgs fields: disaggregation_mode (v0.5.18 `server_args.py:3101-3105`, Literal
	// "null"/"prefill"/"decode", "null" meaning no split), disaggregation_transfer_backend
	// (v0.5.18 `server_args.py:3106-3113`, choices DISAGG_TRANSFER_BACKEND_CHOICES, default
	// "mooncake") and disaggregation_bootstrap_port (v0.5.18 `server_args.py:3114-3118`, default
	// 8998, "Bootstrap server port on the prefill server").
	//
	// The backend is written even though its default is already mooncake, because the value is a
	// pair property: the router-side handshake and the engines' transfer engine must agree, and
	// leaving the flag to its default lets a role's own argument change one side of the pair
	// without anything here naming the leg it broke.
	sglangDisaggregationModeArg          = "--disaggregation-mode"
	sglangDisaggregationBackendArg       = "--disaggregation-transfer-backend"
	sglangDisaggregationBackendMooncake  = "mooncake"
	sglangDisaggregationBootstrapPortArg = "--disaggregation-bootstrap-port"

	// sglangDisaggregationBackendMooncakeTCP is the same Mooncake backend pinned to TCP. SGLang's
	// disaggregation hook (v0.5.18 `arg_groups/pd_disaggregation_hook.py`) sets MC_FORCE_TCP when
	// it is unset, rewrites the backend back to mooncake and clears the IB device, so it is the
	// engine's own spelling of what the vLLM renderer does with the variable. The router side of
	// the pair speaks mooncake either way; only the engines' transfer engine is pinned.
	sglangDisaggregationBackendMooncakeTCP = "mooncake_tcp"

	// sglangRetractionBackupArg selects where a decode half keeps the KV cache of a request it
	// retracts (v0.5.18 `server_args.py` ServerArgs.disaggregation_decode_retraction_backup,
	// choices cpu_tensor and host_pool). Left unset, a decode half on an MHA pool infers host_pool
	// and builds a hierarchical host pool (v0.5.18 `mem_cache/kv_cache_builder.py`), which SGLang
	// refuses to build unless the node's available memory, read host-wide, exceeds a fixed 10 GiB
	// reserve plus the pool (v0.5.18 `mem_cache/pool_host/base.py`
	// HICACHE_HOST_MEMORY_RESERVE_BYTES). cpu_tensor keeps per-request CPU tensors and builds no
	// pool, so a decode half starts on a node with less than that to spare.
	sglangRetractionBackupArg      = "--disaggregation-decode-retraction-backup"
	sglangRetractionBackupCPUValue = "cpu_tensor"

	// sglangHierarchicalCacheArg turns on the hierarchical cache the storage backend hangs off.
	// At v0.5.18 the scheduler enables storage prefetch whenever a storage backend is named
	// (`managers/scheduler.py`), while the tree cache that prefetch reads is the hierarchical one
	// only under this flag (`mem_cache/registry.py`); naming the backend alone leaves a plain
	// RadixCache, and the first request raises AttributeError on hicache_storage_pass_prefix_keys.
	sglangHierarchicalCacheArg = "--enable-hierarchical-cache"

	// SGLangBootstrapPort is where a disaggregated prefiller serves the bootstrap registry the
	// decode side learns its transfer endpoints from. Upstream defaults it to 8998
	// (v0.5.18 `server_args.py:3114-3118`, ServerArgs.disaggregation_bootstrap_port); it is
	// rendered explicitly rather than left to that default because TWO other parties pair with
	// it -- the gateway reads it off the prefill Pod's annotation, and the decode Pod's routing
	// sidecar reads it from its environment -- and a default leaves each of them to restate the
	// number on its own.
	SGLangBootstrapPort int32 = 8998

	// sglangBootstrapPortAnnotation publishes the prefiller's bootstrap port for a router that
	// discovers workers through Kubernetes. The gateway reads it on prefill Pods alone
	// (sgl-project/sglang@gateway-v0.3.1 `sgl-model-gateway/src/service_discovery.rs:54,136-141`,
	// the default of Config.bootstrap_port_annotation). The value is the port as a decimal
	// string, which is how the gateway's own discovery test writes it.
	sglangBootstrapPortAnnotation = "sglang.ai/bootstrap-port"

	// sglangPodIPFieldPath is the field the kubelet resolves at container start, which is the whole
	// reason this engine takes the environment.
	sglangPodIPFieldPath = "status.podIP"
)

// renderSGLang produces the variables and the argument an SGLang container needs.
//
// It writes no `local_buffer_size` in any spelling, and that omission is measured rather than
// overlooked: SGLang's reader has no key for it, and it hardcodes its own 16 MiB
// `DEFAULT_LOCAL_BUFFER_SIZE` on both of its store-setup paths - `setup_dummy` and `setup`, not two
// calls to the same function - each commented "Zero copy interface does not need local buffer"
// (v0.5.18 `mooncake_store.py:28,464,514`). Emitting it would write something nothing reads.
//
// IT ALSO WRITES NO MODE, AND THAT ONE IS NOT MEASURED. This comment used to claim the reader has
// no key for a mode in any spelling. It has one, under a name a search for "mode" does not find:
// `load_from_env` binds `standalone_storage` from `MOONCAKE_STANDALONE_STORAGE`, and
// `sglang.srt.environ` defaults it to `EnvBool(False)` (v0.5.18). So this renderer leaves SGLang in
// a different store shape from the one it gives vLLM, whose annotation carries
// `"mode":"standalone-store"` beside the same `"global_segment_size":0` - and `client_config.go`
// says of that pair that the two are always written together.
//
// Whether writing it here is the repair is UNRESOLVED, which is why nothing is written. Setting
// `MOONCAKE_STANDALONE_STORAGE=true` on a deployment moved the engine's failure rather than
// clearing it: the store's warmup stopped being reached and `setup_dummy` raised a TypeError on
// its own signature instead. Naming the gap is what this paragraph is for; closing it needs a
// reading from an engine that gets past that call.
//
// `global_segment_size` IS written, because SGLang defaults it to "4gb" when absent (v0.5.18
// `environ.py:704`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`) - the same trap vLLM has, so the same explicit
// zero. Note that SGLang divides the configured value across tensor-parallel ranks before passing it
// on (v0.5.18 `mooncake_store.py:413,415-416`, `tp_scale_factor`); zero divides to zero, but anyone
// rendering a non-zero value here must account for the multiplication.
//
// Each reference above carries its version and its symbol because the bare line numbers this comment
// used to cite had every one of them drifted - that upstream file has since grown past 1300 lines -
// and a stale line number survives review precisely because it looks checked.
func renderSGLang(in Input) (*Result, error) {
	// Gated on the role table rather than restated in this renderer, so admission and rendering use
	// the same role contract.
	if !SupportsRole(in.Engine, in.Role) {
		return nil, newRefusal(ReasonRoleUnsupported,
			"engine %q has no known prefill/decode equivalent for role %q; accepting the role and "+
				"ignoring it would leave the container looking configured and behaving otherwise",
			in.Engine, in.Role)
	}

	// One variable decides both the emission and what is reported about it. Computing the condition
	// twice let them drift: a mutation that stopped the append still reported an injection, and the
	// stamp would then have claimed a tenant no container carried. The reported flag has to be a
	// consequence of the emission, not a second opinion about it.
	//
	// The tenant is omitted entirely for an empty domain rather than emitted empty, because this
	// engine normalises a blank value back to the store default - so an empty variable would be
	// indistinguishable from not setting one, while still looking, on the Pod, like configuration.
	hasStore := in.Connection.MasterAddress != ""
	// decodeHalf is whether this engine starts in decode mode, which is the disaggregation leg's
	// own condition below: a lone decode half runs undivided and is a server like any other.
	decodeHalf := in.Disaggregated && in.Role == RoleDecode
	env := []core.EnvVar{}
	tenantInjected := hasStore && in.Domain != ""
	tenantEnvName := ""
	if tenantInjected {
		env = append(env, core.EnvVar{Name: sglangTenantEnv, Value: in.Domain})
		tenantEnvName = sglangTenantEnv
	}

	result := &Result{}
	if hasStore {
		env = append(env, []core.EnvVar{
			{Name: sglangMasterEnv, Value: in.Connection.MasterAddress},
			{Name: sglangMetadataServerEnv, Value: MetadataServer},
			{Name: sglangProtocolEnv, Value: in.Connection.Protocol},
			{Name: sglangDeviceEnv, Value: DeviceName},
			{Name: sglangGlobalSegmentEnv, Value: strconv.Itoa(GlobalSegmentSize)},
			{
				Name: sglangLocalHostnameEnv,
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{FieldPath: sglangPodIPFieldPath},
				},
			},
		}...)
		result.Env = env
		result.TenantInjected = tenantInjected
		// Named only when a tenant was actually emitted. Callers use the name to overwrite every
		// workload declaration of this environment variable with the Binding's resolved tenant.
		result.TenantEnvName = tenantEnvName
		result.Args = []string{sglangBackendArg, sglangBackendValue}
		// A decode half never gets the hierarchical cache: SGLang forces the radix cache off on a
		// decode half (v0.5.18 `arg_groups/pd_disaggregation_hook.py`), and refuses the two flags
		// together. Every other shape serves prefills and reads the tree the backend feeds.
		//
		// It builds a pinned host pool of hicache_ratio (default 2.0) times the device KV pool,
		// under the same 10 GiB host-wide reserve sglangRetractionBackupArg describes; that node
		// prerequisite is the price of a working store, not something this renderer can shrink.
		if !decodeHalf {
			result.DefaultedArgs = append(result.DefaultedArgs, []string{sglangHierarchicalCacheArg})
		}
	}

	// A transfer leg with no half to render has nothing to pair, so it is refused rather than
	// approximated - the shared store alone is what a role with no split renders.
	if in.KVTransfer && in.Role != RolePrefill && in.Role != RoleDecode {
		return nil, newRefusal(ReasonRoleUnsupported,
			"point-to-point transfer pairs a prefill half with a decode half, and role %q is "+
				"neither; a role with no split renders the shared store alone", in.Role)
	}

	// THE DISAGGREGATION LEG FOLLOWS THE ROLE AND THE PAIR, NOT THE TRANSFER FLAG, and the
	// load-bearing parts are both in that sentence. The store client above is role-blind, so these
	// arguments are the only rendering the two kinds have here; naming the split IS what it means to
	// be one of them on this engine.
	//
	// Requiring the transfer flag as well would split admission from rendering. The handler that
	// accepts a role asks the support table, which is answered per engine and role and knows nothing
	// about the transfer leg, so a role it admitted would reach a renderer that refuses it - and the
	// refusal would land in the reconciler, on an object already stored, where the user cannot act
	// on it. The table exists precisely so that a role an engine cannot render is refused while the
	// user can still fix it.
	//
	// The PAIR term is a different rule with a different reason: the routers already answer "is
	// this a split" by whether both halves are declared, routing a lone half as the undivided shape
	// rather than entering disaggregation against a set that can serve one side only. An engine
	// started in half mode under that routing waits on a counterpart nothing assigns, so the two
	// layers are made to agree here rather than in the router alone. The cost is carried knowingly:
	// a deployment deliberately declaring one half to feed a shared store runs this engine
	// undivided, where it previously started as one half.
	if in.Disaggregated && (in.Role == RolePrefill || in.Role == RoleDecode) {
		result.KVTransfer = true
		// The backend is a pair property, so both halves choose it from the same inputs: a tcp
		// leg is pinned exactly where directLegForcesTCP allows it, and every other leg keeps
		// the transport the transfer engine selects for itself.
		backend := sglangDisaggregationBackendMooncake
		if directLegForcesTCP(in) {
			backend = sglangDisaggregationBackendMooncakeTCP
		}
		result.Args = append(result.Args,
			sglangDisaggregationModeArg, string(in.Role),
			sglangDisaggregationBackendArg, backend)
		if decodeHalf {
			result.DefaultedArgs = append(result.DefaultedArgs,
				[]string{sglangRetractionBackupArg, sglangRetractionBackupCPUValue})
		}

		// Only the prefill half serves the bootstrap registry, so only it names the port, opens
		// it on the container and publishes it for discovery. The decode half learns the
		// registry per request from whatever routes it, so it renders none of the three.
		if in.Role == RolePrefill {
			result.Args = append(result.Args,
				sglangDisaggregationBootstrapPortArg, strconv.Itoa(int(SGLangBootstrapPort)))
			result.Ports = append(result.Ports, core.ContainerPort{
				Name: "bootstrap", Protocol: core.ProtocolTCP, ContainerPort: SGLangBootstrapPort,
			})
			if result.PodAnnotations == nil {
				result.PodAnnotations = map[string]string{}
			}
			result.PodAnnotations[sglangBootstrapPortAnnotation] = strconv.Itoa(int(SGLangBootstrapPort))
		}
	}

	return result, nil
}
