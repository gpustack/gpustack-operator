package settings

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
)

func TestModelArtifactDeliveryModeAdmission(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		exists  bool
		lookErr error
		wantErr string
	}{
		{name: "Engine needs nothing", value: "Engine"},
		{name: "Node with the CSIDriver", value: "Node", exists: true},
		{name: "Node without the CSIDriver", value: "Node", wantErr: "does not exist"},
		{name: "Node when the lookup fails", value: "Node", lookErr: errors.New("boom"), wantErr: "check the CSIDriver"},
		{name: "another value", value: "Pvc", wantErr: "want Engine or Node"},
		{name: "a lowercase value", value: "node", wantErr: "want Engine or Node"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := csiDriverExists
			t.Cleanup(func() { csiDriverExists = orig })
			csiDriverExists = func(context.Context) (bool, error) { return c.exists, c.lookErr }

			err := admitDeliveryMode(context.Background(), "", c.value)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestModelStoreWatermarkAdmission(t *testing.T) {
	cases := []struct {
		name     string
		isHigh   bool
		other    string
		otherErr error
		value    string
		wantErr  string
	}{
		{name: "a high above the low", isHigh: true, other: "70", value: "85"},
		{name: "a high equal to the low", isHigh: true, other: "70", value: "70", wantErr: "above the low"},
		{name: "a high below the low", isHigh: true, other: "70", value: "60", wantErr: "above the low"},
		{name: "a low below the high", other: "80", value: "50"},
		{name: "a low equal to the high", other: "80", value: "80", wantErr: "below the high"},
		{name: "a low that is not a number", other: "80", value: "x", wantErr: "invalid syntax"},
		{name: "an unparseable other value is left to its own check", isHigh: true, other: "x", value: "85"},
		{name: "a store that cannot be read refuses", isHigh: true, otherErr: errors.New("connection refused"), value: "85", wantErr: "connection refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := currentValue
			t.Cleanup(func() { currentValue = orig })
			currentValue = func(context.Context, string) (string, error) { return c.other, c.otherErr }

			err := admitWatermarkAgainst("other", c.isHigh)(context.Background(), "", c.value)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestModelStoreSettingsDefaults(t *testing.T) {
	cases := []struct {
		s           setting.Setting
		wantName    string
		wantDefault string
	}{
		{ModelArtifactDeliveryMode, "model-artifact-delivery-mode", "Engine"},
		{ModelStoreHighWatermark, "model-store-high-watermark", "80"},
		{ModelStoreLowWatermark, "model-store-low-watermark", "70"},
		{ModelStoreDownloadConcurrency, "model-store-download-concurrency", "8"},
		{ModelStoreDownloadBandwidth, "model-store-download-bandwidth", "0"},
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
			value: "70", wantErr: "above the low watermark 85",
		},
		{name: "an endpoint without a host is refused", setting: ModelArtifactHuggingFaceEndpoint, value: "https://", wantErr: "with a host"},
		{name: "an endpoint with a host is admitted", setting: ModelArtifactHuggingFaceEndpoint, value: "https://hub.example"},
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
