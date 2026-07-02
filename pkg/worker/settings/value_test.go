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
