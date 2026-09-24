package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/setting"
)

// TestInstanceTypeManagementSettings pins the three runtime-adjustable switches
// added for the unified-pool refactor: their stable names (which drive the
// GPUSTACK_${UPPER_SNAKE} env mapping), their boot defaults, and that they are
// editable booleans — so an admin can flip them at runtime (the value is read
// per-reconcile via ShouldValueBool) without restarting the operator.
func TestInstanceTypeManagementSettings(t *testing.T) {
	cases := []struct {
		s           setting.Setting
		wantName    string
		wantDefault string
	}{
		{NodeManagementManual, "node-management-manual", "false"},
		{InstanceTypeMixedOnNode, "instance-type-mixed-on-node", "true"},
		{InstanceTypeDerivedFromNode, "instance-type-derived-from-node", "true"},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			assert.Equal(t, c.wantName, c.s.Name(), "name drives the GPUSTACK_ env mapping")
			assert.Equal(t, c.wantDefault, c.s.DefaultValue(), "boot default value")
			assert.True(t, c.s.Editable(), "must stay editable for runtime adjustment")
		})
	}
}

// TestInstanceHostAccessSettings pins the two administrator gates guarding the ways an
// Instance crosses the node boundary — privileged mode and a hostPath volume mount. They
// are separate settings because privileged grants strictly more than hostPath, so an admin
// can allow node-path mounts without allowing a container escape. This pins their stable
// names (which drive the GPUSTACK_ env mapping), their default-off boot values, and that
// they stay editable booleans so an admin can flip them at runtime.
func TestInstanceHostAccessSettings(t *testing.T) {
	cases := []struct {
		s        setting.Setting
		wantName string
		wantEnv  string
	}{
		{InstancePrivilegedAllowed, "instance-privileged-allowed", "GPUSTACK_INSTANCE_PRIVILEGED_ALLOWED"},
		{
			InstanceHostPathVolumeAllowed,
			"instance-host-path-volume-allowed",
			"GPUSTACK_INSTANCE_HOST_PATH_VOLUME_ALLOWED",
		},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			assert.Equal(t, c.wantName, c.s.Name(), "name drives the GPUSTACK_ env mapping")
			assert.Equal(t, "false", c.s.DefaultValue(), "host access must be opt-in, so the default is off")
			assert.True(t, c.s.Editable(), "must stay editable for runtime adjustment")

			t.Setenv(c.wantEnv, "true")
			assert.Equal(t, "true", setting.InitializeFromEnv("false")(c.s.Name()),
				"env %s must override the default", c.wantEnv)
		})
	}
}

// TestModelDeploymentTCPTWReuseSetting pins the switch that renders net.ipv4.tcp_tw_reuse onto
// SGLang prefill Pods: its stable name (which drives the GPUSTACK_ env mapping), its default-off
// boot value, and that it stays an editable boolean. It must default off because the sysctl is
// not on the kubelet's safe list, so a cluster that has not allowed it refuses every Pod carrying it.
func TestModelDeploymentTCPTWReuseSetting(t *testing.T) {
	s := ModelDeploymentTCPTWReuse
	assert.Equal(t, "model-deployment-tcp-tw-reuse", s.Name(), "name drives the GPUSTACK_ env mapping")
	assert.Equal(t, "false", s.DefaultValue(), "the sysctl needs a kubelet change, so the default is off")
	assert.True(t, s.Editable(), "must stay editable for runtime adjustment")

	t.Setenv("GPUSTACK_MODEL_DEPLOYMENT_TCP_TW_REUSE", "true")
	assert.Equal(t, "true", setting.InitializeFromEnv("false")(s.Name()),
		"env GPUSTACK_MODEL_DEPLOYMENT_TCP_TW_REUSE must override the default")
}

// TestInstanceTypeManagementSettingsEnvMapping pins the operator-facing contract
// that each setting resolves its boot value from GPUSTACK_${UPPER_SNAKE(name)},
// with an env override winning over the default.
func TestInstanceTypeManagementSettingsEnvMapping(t *testing.T) {
	cases := []struct {
		name string
		env  string
		def  string
		set  string
		want string
	}{
		{"node-management-manual", "GPUSTACK_NODE_MANAGEMENT_MANUAL", "false", "true", "true"},
		{"instance-type-mixed-on-node", "GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE", "true", "false", "false"},
		{"instance-type-derived-from-node", "GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE", "true", "false", "false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.env, c.set)
			assert.Equal(t, c.want, setting.InitializeFromEnv(c.def)(c.name),
				"env %s must override the default", c.env)
		})
	}
}

// TestWorkloadFitAffinitySetting pins the switch that lets an administrator turn the Workload
// fit pin off while the fit labels keep being published, so a verification can ablate the pin
// alone: its stable name (which drives the GPUSTACK_ env mapping), its default-on boot value, and
// that it stays an editable boolean read at runtime.
func TestWorkloadFitAffinitySetting(t *testing.T) {
	assert.Equal(t, "workload-fit-affinity", WorkloadFitAffinity.Name(), "name drives the GPUSTACK_ env mapping")
	assert.Equal(t, "true", WorkloadFitAffinity.DefaultValue(), "boot default value")
	assert.True(t, WorkloadFitAffinity.Editable(), "must stay editable for runtime adjustment")
}
