package settings

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/utils/stringx"
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
			value, _, _ := setting.InitializeFromEnv("false")(c.s.Name())
			assert.Equal(t, "true", value,
				"env %s must override the default", c.wantEnv)
		})
	}
}

// TestModelDeploymentImageSettings pins the two images a ModelDeployment's own roles run when the
// object names none: the router, and the sidecar a decoder runs to accept a remote prefill handoff.
// Neither runs a model, so neither links an accelerator runtime and one default is safe for every
// cluster; both stay editable so an air-gapped cluster can redirect them at its own registry and a
// broken upstream release can be pinned back without rebuilding the operator. Pinning the tags
// here is what makes a default bump a reviewed change rather than a silent one.
func TestModelDeploymentImageSettings(t *testing.T) {
	cases := []struct {
		s           setting.Setting
		wantName    string
		wantEnv     string
		wantDefault string
	}{
		{
			ModelDeploymentRouterImage,
			"model-deployment-router-image",
			"GPUSTACK_MODEL_DEPLOYMENT_ROUTER_IMAGE",
			"gpustack/llm-router:v0.4.0",
		},
		{
			ModelDeploymentRoutingSidecarImage,
			"model-deployment-routing-sidecar-image",
			"GPUSTACK_MODEL_DEPLOYMENT_ROUTING_SIDECAR_IMAGE",
			"gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0",
		},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			assert.Equal(t, c.wantName, c.s.Name(), "name drives the GPUSTACK_ env mapping")
			assert.Equal(t, c.wantDefault, c.s.DefaultValue(), "boot default image")
			assert.True(t, c.s.Editable(), "must stay editable so a cluster can redirect or pin it")

			t.Setenv(c.wantEnv, "registry.example.com/override:v0")
			value, _, _ := setting.InitializeFromEnv(c.wantDefault)(c.s.Name())
			assert.Equal(t, "registry.example.com/override:v0", value,
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
	value, _, _ := setting.InitializeFromEnv("false")(s.Name())
	assert.Equal(t, "true", value,
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
			value, _, _ := setting.InitializeFromEnv(c.def)(c.name)
			assert.Equal(t, c.want, value,
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

// TestInstancePersistentVolumePlacementSetting pins the escape switch for placing an Instance where
// its persistent volumes can attach: its stable name (which drives the GPUSTACK_ env mapping), its
// default-on boot value, and that it stays an editable boolean, so an administrator can return to
// the render that knew only the claim names without a redeploy.
func TestInstancePersistentVolumePlacementSetting(t *testing.T) {
	s := InstancePersistentVolumePlacement
	assert.Equal(t, "instance-persistent-volume-placement", s.Name(), "name drives the GPUSTACK_ env mapping")
	assert.Equal(t, "true", s.DefaultValue(), "boot default value")
	assert.True(t, s.Editable(), "must stay editable for runtime adjustment")
}

// trackedImageSettings is the built-in tracking list, written out longhand rather than joined from
// the image Setting vars. A name derived from those vars could not disagree with the registry, and a
// disagreement is what the resolve test below exists to catch: the align pass drops a name it cannot
// resolve, at a verbosity nobody reads.
var trackedImageSettings = []string{
	"instance-ssh-server-image",
	"kv-cache-backend-image",
	"model-deployment-router-image",
	"model-deployment-router-proxy-image",
	"model-deployment-routing-sidecar-image",
	"model-prefetch-warmup-image",
}

// TestSettingsDefaultTracking_DefaultValue pins the built-in list: the six image Settings, in this
// order. The order is the documentation order an administrator reads, so a change to it is a change
// to what an upgrade writes back.
func TestSettingsDefaultTracking_DefaultValue(t *testing.T) {
	assert.Equal(t, trackedImageSettings, strings.Split(DefaultTracking.DefaultValue(), ","),
		"the built-in list is the six image Settings, in this order")
}

// TestSettingsDefaultTracking_IsRegistered pins that the registry carries this Setting under the name
// the align pass resolves. Two copies of that literal that drift leave the tracking list matching
// nothing at all, silently, and no other test would see it.
func TestSettingsDefaultTracking_IsRegistered(t *testing.T) {
	assert.Equal(t, setting.DefaultTrackingName, DefaultTracking.Name(),
		"the registration and the reader must share one name")

	s, ok := settings.Index(setting.DefaultTrackingName)
	require.True(t, ok, "the align pass resolves the tracking list by name in this registry")
	assert.True(t, s.Editable(), "an administrator clears the list through this Setting")
}

// TestSettingsDefaultTracking_DefaultNamesResolve pins that every name in the built-in list resolves
// to a registered Setting, read from the registered default rather than from this file's own copy, so
// a typo in the registration fails here instead of being dropped at read time.
func TestSettingsDefaultTracking_DefaultNamesResolve(t *testing.T) {
	for _, name := range strings.Split(DefaultTracking.DefaultValue(), ",") {
		t.Run(name, func(t *testing.T) {
			_, ok := settings.Index(name)
			assert.True(t, ok, "a name the registry does not carry tracks nothing")
		})
	}
}

// TestSettingsDefaultTracking_RefusesItsOwnName pins that a list naming the tracking Setting itself is
// refused at the write, because a Setting that tracked itself would reconcile its own membership.
// The refusal compares trimmed segments, so padding and burial are the same case.
func TestSettingsDefaultTracking_RefusesItsOwnName(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{
			name:    "its own name alone is refused",
			value:   setting.DefaultTrackingName,
			wantErr: true,
		},
		{
			name:    "its own name padded with whitespace is refused",
			value:   " " + trackedImageSettings[0] + " , " + setting.DefaultTrackingName + "  ",
			wantErr: true,
		},
		{
			name:    "its own name buried among valid names is refused",
			value:   trackedImageSettings[0] + ", " + setting.DefaultTrackingName + ", " + trackedImageSettings[1],
			wantErr: true,
		},
		{
			name:  "a list that does not name itself is admitted",
			value: strings.Join(trackedImageSettings, ","),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := setting.DisallowListWith(setting.DefaultTrackingName)(context.Background(), "", c.value)
			if !c.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, setting.DefaultTrackingName,
				"the error must name the Setting an administrator has to fix")
		})
	}
}

// TestSettingsDefaultTracking_BlankListIsAllowed pins that a blank list is admitted. Clearing it is
// the only way an administrator stops tracking everything, so a refusal here would leave no way out
// of the rule the list applies.
//
// The chain is built here instead of run through the registered Setting, because Configure is the
// only path that runs it and it writes the delegated Secret through a process-wide loopback client
// another test in this package configures: driving it from here would make this case's coverage
// depend on that test running first, which the shuffled order of make test decides. What the
// blank value then does to the align pass is pinned by the cleared row of the test below.
func TestSettingsDefaultTracking_BlankListIsAllowed(t *testing.T) {
	admit := setting.AdmitWith(setting.AllowBlank(), setting.DisallowListWith(setting.DefaultTrackingName))
	assert.NoError(t, admit(context.Background(), trackedImageSettings[0], ""),
		"an administrator must be able to stop tracking everything")
}

// TestSettingsDefaultTracking_AppliesOnThePassThatBackfillsItsKey pins the seam between this
// registration and the align pass: on a Secret predating this Setting the key is absent, and the
// backfill writes the built-in list into the Secret before the align pass reads it, so the built-in
// list still applies and a cluster reaches the new defaults on the upgrade rather than a start
// later. The second row clears the list and pins the other side of the same pass, so the first row
// cannot pass because the align pass rewrites those Settings whatever the list says.
func TestSettingsDefaultTracking_AppliesOnThePassThatBackfillsItsKey(t *testing.T) {
	const driftedImage = "pinned.example/image:v0"

	// A declaration claims a Setting ahead of the tracking list, so a variable set in the process
	// environment would let both rows pass for the wrong reason. Their values are fixed when the
	// package registers, which this test cannot change.
	for _, name := range trackedImageSettings {
		envName := "GPUSTACK_" + stringx.UnderscoreUpper(name)
		if _, declared := os.LookupEnv(envName); declared {
			t.Skipf("%s is declared in this process environment, so the env rule claims the six "+
				"ahead of the tracking list and this case cannot observe the built-in list", envName)
		}
	}

	cleared := ""
	cases := []struct {
		name        string
		tracking    *string // the stored tracking value; nil means the Secret predates the Setting
		wantTracked bool
	}{
		{
			name:        "the built-in list applies when the Secret has no tracking key",
			wantTracked: true,
		},
		{
			name:        "a list the administrator cleared tracks nothing",
			tracking:    &cleared,
			wantTracked: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := map[string][]byte{}
			for _, name := range trackedImageSettings {
				data[name] = []byte(driftedImage)
			}
			if c.tracking != nil {
				data[setting.DefaultTrackingName] = []byte(*c.tracking)
			}
			cli := kubefake.NewSimpleClientset(&core.Secret{
				ObjectMeta: meta.ObjectMeta{
					Namespace: setting.DelegatedSecretNamespace,
					Name:      setting.DelegatedSecretName,
				},
				Data: data,
			})
			allowSelfSubjectAccessReviews(cli)

			require.NoError(t, settings.Initialize(context.Background(), cli))

			ctx := context.Background()
			sec, err := cli.CoreV1().Secrets(setting.DelegatedSecretNamespace).
				Get(ctx, setting.DelegatedSecretName, meta.GetOptions{})
			require.NoError(t, err)
			for _, name := range trackedImageSettings {
				want := driftedImage
				if c.wantTracked {
					s, ok := settings.Index(name)
					require.True(t, ok)
					want = s.DefaultValue()
				}
				assert.Equal(t, want, string(sec.Data[name]), "setting %s", name)
			}
		})
	}
}

// allowSelfSubjectAccessReviews makes the permission check Initialize runs first pass, which a fake
// client denies by default.
func allowSelfSubjectAccessReviews(cli *kubefake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, ok := action.(k8stesting.CreateAction).GetObject().(*authz.SelfSubjectAccessReview)
			if !ok {
				return false, nil, nil
			}
			sar = sar.DeepCopy()
			sar.Status.Allowed = true
			return true, sar, nil
		})
}
