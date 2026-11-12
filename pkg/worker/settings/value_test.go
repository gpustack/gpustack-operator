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
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// TestMain configures the fake controller-runtime client used by settings reads that reach the
// delegated Secret via the loopback client.
func TestMain(m *testing.M) {
	system.LoopbackCtrlClient.Configure(
		ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
	)
	os.Exit(m.Run())
}

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

// TestModelDeploymentImageSettings pins the three images a ModelDeployment's own roles or server
// run when the object names none: the router, the ModelExpress metadata server, and the sidecar a
// decoder runs to accept a remote prefill handoff. None runs a model, so none links an accelerator
// runtime and one default is safe for every cluster; all stay editable so an air-gapped cluster can
// redirect them at its own registry and a broken upstream release can be pinned back without
// rebuilding the operator. Pinning the tags here is what makes a default bump a reviewed change
// rather than a silent one.
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
			ModelDeploymentModelExpressServerImage,
			"model-deployment-modelexpress-server-image",
			"GPUSTACK_MODEL_DEPLOYMENT_MODELEXPRESS_SERVER_IMAGE",
			"gpustack/mirrored-modelexpress-server:v0.7.0",
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
	"model-deployment-modelexpress-server-image",
	"model-deployment-router-image",
	"model-deployment-router-proxy-image",
	"model-deployment-routing-sidecar-image",
	"model-prefetch-warmup-image",
}

// TestSettingsDefaultTracking_DefaultValue pins the built-in list: the image Settings, in this
// order. The order is the documentation order an administrator reads, so a change to it is a change
// to what an upgrade writes back.
func TestSettingsDefaultTracking_DefaultValue(t *testing.T) {
	assert.Equal(t, trackedImageSettings, strings.Split(DefaultTracking.DefaultValue(), ","),
		"the built-in list is the image Settings, in this order")
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
			t.Skipf("%s is declared in this process environment, so the env rule claims the list "+
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

func TestModelStoreSettingsDefaults(t *testing.T) {
	cases := []struct {
		s           setting.Setting
		wantName    string
		wantDefault string
	}{
		{ModelArtifactDeliveryMode, "model-artifact-delivery-mode", "Engine"},
		{ModelArtifactRemoteDeliveryPolicy, "model-artifact-remote-delivery-policy", ModelArtifactRemoteDeliveryPrefer},
		{ModelStoreHighWatermark, "model-store-high-watermark", "80"},
		{ModelStoreLowWatermark, "model-store-low-watermark", "70"},
		{ModelStoreDownloadConcurrency, "model-store-download-concurrency", "8"},
		{ModelStoreDownloadBandwidth, "model-store-download-bandwidth", "0"},
		{ModelArtifactModelScopeEndpoint, "model-artifact-modelscope-endpoint", "https://www.modelscope.cn"},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			assert.Equal(t, c.wantName, c.s.Name(), "name drives the GPUSTACK_ env mapping")
			assert.Equal(t, c.wantDefault, c.s.DefaultValue())
			assert.True(t, c.s.Editable())
		})
	}
	assert.NoError(t, ValidateModelStoreDefaults(), "the shipped defaults must pass the startup check")
}

func TestModelStoreLayer(t *testing.T) {
	values := map[string]string{
		ModelStoreHighWatermark.Name():          "85",
		ModelStoreLowWatermark.Name():           "75",
		ModelStoreDownloadConcurrency.Name():    "16",
		ModelStoreDownloadBandwidth.Name():      "100Mi",
		ModelArtifactHuggingFaceEndpoint.Name(): "http://hub.local",
		ModelArtifactModelScopeEndpoint.Name():  "https://ms.example",
		ModelArtifactHTTPSProxy.Name():          "http://proxy:3128",
		ModelArtifactNoProxy.Name():             "internal",
		ModelArtifactCABundle.Name():            "hub-ca",
	}
	read := func(s setting.Setting) string { return values[s.Name()] }

	l, err := ModelStoreLayer(read)
	require.NoError(t, err)
	assert.Equal(t, int32(85), *l.HighWatermarkPercent)
	assert.Equal(t, int32(75), *l.LowWatermarkPercent)
	assert.Equal(t, int32(16), *l.DownloadConcurrency)
	assert.Equal(t, int64(100<<20), *l.DownloadBytesPerSecond)
	assert.Equal(t, "http://hub.local", *l.HuggingFaceEndpoint)
	assert.Equal(t, "https://ms.example", *l.ModelScopeEndpoint)
	assert.Equal(t, "http://proxy:3128", *l.HTTPSProxy)
	assert.Equal(t, "internal", *l.NoProxy)
	assert.Equal(t, "hub-ca", *l.CABundleConfigMap)

	values[ModelStoreDownloadConcurrency.Name()] = "many"
	values[ModelStoreDownloadBandwidth.Name()] = "-1"
	_, err = ModelStoreLayer(read)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ModelStoreDownloadConcurrency.Name())
	assert.Contains(t, err.Error(), ModelStoreDownloadBandwidth.Name())
}

func TestValidateModelStoreDefaultsRefusesAnInvalidPair(t *testing.T) {
	// A default is fixed at declaration from the environment, so the check the startup runs on the
	// defaults is exercised on a layer read the same way, with a low watermark above the high one.
	values := map[string]string{
		ModelStoreHighWatermark.Name():          "80",
		ModelStoreLowWatermark.Name():           "90",
		ModelStoreDownloadConcurrency.Name():    "8",
		ModelStoreDownloadBandwidth.Name():      "0",
		ModelArtifactHuggingFaceEndpoint.Name(): "https://huggingface.co",
	}
	l, err := ModelStoreLayer(func(s setting.Setting) string { return values[s.Name()] })
	require.NoError(t, err)
	assert.ErrorContains(t, validateLayer(l), "watermarks")
}

// storedSettings is the Settings store an admission reads, on a fake API server: the Secret holding
// values, configured once per test binary as the loopback client.
func storedSettings(t *testing.T, values map[string]string) kubernetes.Interface {
	t.Helper()
	data := map[string][]byte{}
	for k, v := range values {
		data[k] = []byte(v)
	}
	cli := kubefake.NewSimpleClientset(&core.Secret{
		ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName},
		Data:       data,
	})
	system.LoopbackKubeClient.Configure(cli)
	setting.InvalidateCache()
	t.Cleanup(setting.InvalidateCache)

	return system.LoopbackKubeClient.Get()
}

func TestAdmissionReadsTheStore(t *testing.T) {
	cli := storedSettings(t, map[string]string{
		modelStoreHighWatermarkName: "90", modelStoreLowWatermarkName: "60",
		ModelArtifactHuggingFaceEndpoint.Name(): "https://huggingface.co",
	})
	ctx := context.Background()
	store := func(name, value string) {
		sec, err := cli.CoreV1().Secrets(setting.DelegatedSecretNamespace).Get(ctx, setting.DelegatedSecretName, meta.GetOptions{})
		require.NoError(t, err)
		sec.Data[name] = []byte(value)
		_, err = cli.CoreV1().Secrets(setting.DelegatedSecretNamespace).Update(ctx, sec, meta.UpdateOptions{})
		require.NoError(t, err)
	}

	cases := []struct {
		name    string
		setting setting.Setting
		value   string
		wantErr string
	}{
		{
			// The low watermark was read (and cached) as 60, then stored as 85 within the cache's
			// thirty seconds: a high of 70 checked against the cached 60 would cross the pair.
			name: "the other watermark is the stored one, not the cached one", setting: ModelStoreHighWatermark,
			value: "70", wantErr: "must be above model-store-low-watermark (85)",
		},
		{name: "an endpoint without a host is refused", setting: ModelArtifactHuggingFaceEndpoint, value: "https://", wantErr: "with a host"},
		{name: "an endpoint with a host is admitted", setting: ModelArtifactHuggingFaceEndpoint, value: "https://hub.example"},
		{name: "a modelscope endpoint without a host is refused", setting: ModelArtifactModelScopeEndpoint, value: "https://", wantErr: "with a host"},
		{name: "a modelscope endpoint with a host is admitted", setting: ModelArtifactModelScopeEndpoint, value: "https://ms.example"},
	}
	_ = ModelStoreLowWatermark.ShouldValueFromRemote(ctx) // caches 60
	store(modelStoreLowWatermarkName, "85")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.setting.Configure(ctx, c.value)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

// TestAWSEFAEnabledSetting pins the cluster Setting that selects AWS Elastic Fabric Adapter
// (EFA) for fabric interfaces and NIXL transfers: its stable name (which drives the GPUSTACK_
// env mapping), its default-off boot value, that it stays an editable boolean, its constraint
// validation, and its delegated Secret override.
func TestAWSEFAEnabledSetting(t *testing.T) {
	assert.Equal(t, "aws-efa-enabled", AWSEFAEnabled.Name(), "name drives the GPUSTACK_ env mapping")
	assert.Equal(t, "false", AWSEFAEnabled.DefaultValue(), "EFA must be opt-in, so the default is off")
	assert.True(t, AWSEFAEnabled.Editable(), "must stay editable for runtime adjustment")

	ctx := context.Background()

	// Default value without overrides.
	assert.False(t, AWSEFAEnabled.ShouldValueBool(ctx), "default value must evaluate to false")

	// Environment variable mapping.
	_, envName, _ := setting.InitializeFromEnv("false")(AWSEFAEnabled.Name())
	assert.Equal(t, "GPUSTACK_AWS_EFA_ENABLED", envName, "env name must follow the GPUSTACK_${UPPER_SNAKE} convention")

	envCases := []struct {
		name     string
		envValue string
		want     string
	}{
		{name: "env true", envValue: "true", want: "true"},
		{name: "env false", envValue: "false", want: "false"},
	}
	for _, tc := range envCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envName, tc.envValue)
			val, _, _ := setting.InitializeFromEnv("false")(AWSEFAEnabled.Name())
			assert.Equal(t, tc.want, val)
		})
	}

	// Boolean constraint validation via Validate.
	validateCases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid true", value: "true", wantErr: false},
		{name: "valid false", value: "false", wantErr: false},
		{name: "valid 1", value: "1", wantErr: false},
		{name: "valid 0", value: "0", wantErr: false},
		{name: "invalid yes", value: "yes", wantErr: true},
		{name: "invalid no", value: "no", wantErr: true},
		{name: "invalid non-boolean string", value: "invalid", wantErr: true},
		{name: "invalid blank", value: "", wantErr: true},
	}
	for _, tc := range validateCases {
		t.Run("validate_"+tc.name, func(t *testing.T) {
			err := AWSEFAEnabled.Validate(ctx, tc.value)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}

	// Delegated Secret override.
	t.Run("delegated secret override", func(t *testing.T) {
		settingtest.MergeDelegatedSettings(t, map[string]string{
			AWSEFAEnabled.Name(): "true",
		})
		val, err := AWSEFAEnabled.ValueBool(ctx)
		require.NoError(t, err)
		assert.True(t, val)
		assert.True(t, AWSEFAEnabled.ShouldValueBool(ctx), "ShouldValueBool must return true when overridden in delegated secret")
	})
}
