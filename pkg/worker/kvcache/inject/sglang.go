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

	// sglangPodIPFieldPath is the field the kubelet resolves at container start, which is the whole
	// reason this engine takes the environment.
	sglangPodIPFieldPath = "status.podIP"
)

// renderSGLang produces the variables and the argument an SGLang container needs.
//
// It writes neither `mode` nor `local_buffer_size` in any spelling, and both omissions are measured
// rather than overlooked: SGLang's reader has no key for either, and it hardcodes its own 16 MiB
// `DEFAULT_LOCAL_BUFFER_SIZE` on both of its store-setup paths - `setup_dummy` and `setup`, not two
// calls to the same function - each commented "Zero copy interface does not need local buffer"
// (v0.5.18 `mooncake_store.py:28,464,514`). Emitting either would write something nothing reads.
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
	env := []core.EnvVar{}
	tenantInjected := in.Domain != ""
	tenantEnvName := ""
	if tenantInjected {
		env = append(env, core.EnvVar{Name: sglangTenantEnv, Value: in.Domain})
		tenantEnvName = sglangTenantEnv
	}

	return &Result{
		Env: append(env, []core.EnvVar{
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
		}...),
		TenantInjected: tenantInjected,
		// Named only when a tenant was actually emitted. Callers use the name to overwrite every
		// workload declaration of this environment variable with the Binding's resolved tenant.
		TenantEnvName: tenantEnvName,
		Args:          []string{sglangBackendArg, sglangBackendValue},
	}, nil
}
