// This file renders for SGLang. Its vehicle is the environment, and the reason is evaluation time
// rather than key coverage.
//
// SGLang picks its configuration source in `_load_config` (`mooncake_store.py:242-260`): the launch
// argument's extra config, then a file, then the environment. The first two are fixed when the caller
// runs - at admission, before a Pod has an IP - and both fall back key-for-key to `envs.<NAME>.default`
// (`mooncake_store.py:114-141` and `:193-221`), which is the literal attribute (`environ.py:41-42`) and
// not the accessor that reads the process environment (`environ.py:54-72`). So on either of them
// `local_hostname` resolves to the literal "localhost" on every Pod in the pool.
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
	// The variables SGLang reads through `.get()` in `load_from_env` (`mooncake_store.py:167-180`).
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

	// sglangTenantEnv is what makes a reuse domain real on this engine, and it is the one variable
	// here whose effect depends on the build rather than only on the spelling.
	//
	// Read at v0.5.18: all three config paths take a tenant_id, the environment one from this
	// variable; the store call then forwards it as a keyword argument, but ONLY when it differs from
	// the literal "default" - which is exactly why omitting a tenant and passing "default" behave
	// identically against a master. A client library too old to accept the argument raises rather
	// than dropping it, so a build mismatch on that side stops the Pod instead of silently losing
	// isolation. An SGLang build older than this variable simply never reads it, and that direction
	// has no signal at all, which is why the stamp claims an injection and never an outcome.
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
	if in.Role != RoleNone {
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
	// Gated on the facts table, not on this file knowing its own engine. That keeps the table a live
	// constraint rather than documentation: substituting the entry changes what this renderer emits,
	// so a test can prove the emission follows the measurement instead of a hardcoded belief about
	// SGLang. It is also what makes the entry worth re-reading when a new build ships.
	env := []core.EnvVar{}
	tenantInjected := SupportsTenant(in.Engine) && in.Domain != ""
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
		// Named only when a tenant was actually emitted. The field's contract is "empty when it
		// travels in the file instead, or when none was produced", and naming a variable this render
		// never wrote breaks the second half: a caller applying the documented rule - non-empty name
		// plus the variable absent from the container means the precedence rule dropped it - would
		// report a dropped tenant for a render that produced none. The current caller narrows
		// tenantApplied to false and never raises it, so it is unaffected either way; the contract is
		// what the next caller reads.
		TenantEnvName: tenantEnvName,
		Args:          []string{sglangBackendArg, sglangBackendValue},
	}, nil
}
