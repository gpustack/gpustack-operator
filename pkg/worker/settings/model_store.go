package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
)

// The watermark Settings' names. Each one's admission reads the other by name, since a reference
// from one Setting's declaration to the other's would be an initialization cycle.
const (
	modelStoreHighWatermarkName = "model-store-high-watermark"
	modelStoreLowWatermarkName  = "model-store-low-watermark"
)

// The values of ModelArtifactDeliveryMode.
const (
	// ModelArtifactDeliveryEngine has an engine download a hub artifact's resolved commit itself.
	ModelArtifactDeliveryEngine = "Engine"
	// ModelArtifactDeliveryNode has the node's model-manager plugin materialize and mount it.
	ModelArtifactDeliveryNode = "Node"
)

var (
	// ModelArtifactDeliveryMode is how a Hugging Face artifact's weights reach a ModelDeployment's
	// engine. It is explicit and never inferred from whether the plugin is installed: changing it
	// changes every Hugging Face deployment's Pods, which each roll once. The chart seeds "Node"
	// when it deploys the plugin, and a seed fills only a key the store does not have yet.
	ModelArtifactDeliveryMode = settings.NewEditable(
		"model-artifact-delivery-mode",
		"Indicates how a Hugging Face ModelArtifact's weights reach a ModelDeployment: Engine, the engine "+
			"downloads them, or Node, the node's model-manager plugin materializes and mounts them. Node needs "+
			"the plugin installed. Changing it rolls every Hugging Face deployment once.",
		setting.InitializeFromEnv(ModelArtifactDeliveryEngine),
		admitDeliveryMode,
	)

	// ModelStoreHighWatermark is the cache filesystem's usage, in percent, above which the plugin
	// removes unreferenced content. It must stay below kubelet's eviction and image collection
	// thresholds when the cache shares kubelet's filesystem; the plugin caps it there.
	ModelStoreHighWatermark = settings.NewEditable(
		modelStoreHighWatermarkName,
		"Indicates the node model cache's usage percent above which unreferenced models are removed, "+
			"at most 95 and above model-store-low-watermark.",
		setting.InitializeFromEnv("80"),
		setting.AllowInt64InRange(modelstore.MinLowWatermarkPercent+1, modelstore.MaxHighWatermarkPercent),
		admitWatermarkAgainst(modelStoreLowWatermarkName, true),
	)

	// ModelStoreLowWatermark is the usage, in percent, a collection removes down to.
	ModelStoreLowWatermark = settings.NewEditable(
		modelStoreLowWatermarkName,
		"Indicates the node model cache's usage percent a removal stops at, at least 1 and below "+
			"model-store-high-watermark.",
		setting.InitializeFromEnv("70"),
		setting.AllowInt64InRange(modelstore.MinLowWatermarkPercent, modelstore.MaxHighWatermarkPercent-1),
		admitWatermarkAgainst(modelStoreHighWatermarkName, false),
	)

	// ModelStoreDownloadConcurrency is how many HTTP requests a node runs at once across its
	// downloads. A large file is fetched as ranges that share this budget.
	ModelStoreDownloadConcurrency = settings.NewEditable(
		"model-store-download-concurrency",
		"Indicates how many HTTP requests a node runs at once across its model downloads, 1 to 64.",
		setting.InitializeFromEnv("8"),
		setting.AllowInt64InRange(modelstore.MinDownloadConcurrency, modelstore.MaxDownloadConcurrency),
	)

	// ModelStoreDownloadBandwidth is a node's download rate limit, a quantity of bytes per second
	// such as "200Mi"; "0" is unlimited.
	ModelStoreDownloadBandwidth = settings.NewEditable(
		"model-store-download-bandwidth",
		"Indicates a node's model download rate limit in bytes per second, a quantity such as 200Mi; 0 is unlimited.",
		setting.InitializeFromEnv("0"),
		func(_ context.Context, _, newVal string) error {
			_, err := modelstore.ParseBandwidth(newVal)
			return err
		},
	)
)

// currentValue reads another Setting's stored value by name from the Secret itself, not through the
// Settings read cache: two edits within the cache's thirty seconds would otherwise be checked against
// a value already replaced, and a crossed pair freezes every node's configuration. A Secret that
// cannot be read is an error, so the admission refuses rather than skip the check. It is a variable
// so a test can answer without an API server.
var currentValue = func(ctx context.Context, name string) (string, error) {
	sec, err := system.LoopbackKubeClient.Get().CoreV1().Secrets(setting.DelegatedSecretNamespace).
		Get(ctx, setting.DelegatedSecretName, meta.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", name, err)
	}
	if v, ok := sec.Data[name]; ok {
		return string(v), nil
	}

	return settings[name].DefaultValue(), nil
}

// csiDriverExists reports whether the plugin's CSIDriver object exists. It reads the API server, not
// an informer cache, so an edit right after the plugin is installed is not refused for a CSIDriver
// the cache has not seen yet. It is a variable so a test can answer without an API server.
var csiDriverExists = func(ctx context.Context) (bool, error) {
	err := system.LoopbackCtrlAPIReader.Get().Get(ctx, ctrlcli.ObjectKey{Name: modelstore.DriverName}, &storage.CSIDriver{})
	switch {
	case err == nil:
		return true, nil
	case kerrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// admitDeliveryMode accepts Engine, and Node only while the plugin's CSIDriver exists: a delivery
// with nothing to deliver would leave every Hugging Face deployment waiting.
func admitDeliveryMode(ctx context.Context, _, newVal string) error {
	switch newVal {
	case ModelArtifactDeliveryEngine:
		return nil
	case ModelArtifactDeliveryNode:
		ok, err := csiDriverExists(ctx)
		if err != nil {
			return fmt.Errorf("check the CSIDriver %s: %w", modelstore.DriverName, err)
		}
		if !ok {
			return fmt.Errorf("the CSIDriver %s does not exist: install the model-manager plugin "+
				"(chart value modelManager.enabled) before choosing Node delivery", modelstore.DriverName)
		}
		return nil
	default:
		return fmt.Errorf("want %s or %s, got %q", ModelArtifactDeliveryEngine, ModelArtifactDeliveryNode, newVal)
	}
}

// admitWatermarkAgainst keeps the high watermark above the low one: a new high must exceed the
// current low, and a new low must stay under the current high. Lowering both therefore means
// lowering the low one first.
func admitWatermarkAgainst(other string, isHigh bool) setting.Admission {
	return func(ctx context.Context, _, newVal string) error {
		v, err := strconv.ParseInt(newVal, 10, 32)
		if err != nil {
			return err
		}
		raw, err := currentValue(ctx, other)
		if err != nil {
			return err
		}
		o, readable := parsePercent(raw)
		switch {
		case !readable:
			// An unreadable other value is left to its own check.
		case isHigh && v <= o:
			return fmt.Errorf("the high watermark %d must be above the low watermark %d", v, o)
		case !isHigh && v >= o:
			return fmt.Errorf("the low watermark %d must be below the high watermark %d", v, o)
		}
		return nil
	}
}

func parsePercent(raw string) (int64, bool) {
	v, err := strconv.ParseInt(raw, 10, 32)
	return v, err == nil
}

// ModelStoreLayer reads the cluster layer of a node model cache's configuration from the Settings,
// through value: a Setting's current value at runtime, or its default at startup.
func ModelStoreLayer(value func(setting.Setting) string) (modelstore.Layer, error) {
	var errs []error
	parse32 := func(s setting.Setting) *int32 {
		v, err := strconv.ParseInt(value(s), 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("setting %s: %w", s.Name(), err))
			return nil
		}
		v32 := int32(v)
		return &v32
	}
	str := func(s setting.Setting) *string {
		v := value(s)
		return &v
	}

	l := modelstore.Layer{
		HighWatermarkPercent: parse32(ModelStoreHighWatermark),
		LowWatermarkPercent:  parse32(ModelStoreLowWatermark),
		DownloadConcurrency:  parse32(ModelStoreDownloadConcurrency),
		HuggingFaceEndpoint:  str(ModelArtifactHuggingFaceEndpoint),
		HTTPSProxy:           str(ModelArtifactHTTPSProxy),
		NoProxy:              str(ModelArtifactNoProxy),
		CABundleConfigMap:    str(ModelArtifactCABundle),
	}
	if bw, err := modelstore.ParseBandwidth(value(ModelStoreDownloadBandwidth)); err != nil {
		errs = append(errs, fmt.Errorf("setting %s: %w", ModelStoreDownloadBandwidth.Name(), err))
	} else {
		l.DownloadBytesPerSecond = &bw
	}

	return l, errors.Join(errs...)
}

// ValidateModelStoreDefaults checks the values the node model cache's Settings take from the
// environment, which no admission reads, so the worker refuses to start on one that would be
// written to every node. The delivery mode's CSIDriver condition is not checked here: whether the
// plugin exists is a runtime fact, which consumers wait on.
func ValidateModelStoreDefaults() error {
	if v := ModelArtifactDeliveryMode.DefaultValue(); v != ModelArtifactDeliveryEngine && v != ModelArtifactDeliveryNode {
		return fmt.Errorf("setting %s: want %s or %s, got %q",
			ModelArtifactDeliveryMode.Name(), ModelArtifactDeliveryEngine, ModelArtifactDeliveryNode, v)
	}
	l, err := ModelStoreLayer(setting.Setting.DefaultValue)
	if err != nil {
		return err
	}

	return validateLayer(l)
}

func validateLayer(l modelstore.Layer) error {
	if err := modelstore.Validate(modelstore.Merge(l)); err != nil {
		return fmt.Errorf("the node model cache's settings: %w", err)
	}

	return nil
}
