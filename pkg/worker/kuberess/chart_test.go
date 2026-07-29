package kuberess

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/releaseutil"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// chartDir is the operator chart this package installs, as reached from the package directory.
const chartDir = "../../../deploy/gpustack-operator/chart"

// TestCSIDriverNamesMatchChart ties the driver names other packages compile against to the ones
// the chart deploys. They are one contract in two files: the worker writes these into the
// StorageClasses it provisions, and a rename on either side silently orphans every volume.
func TestCSIDriverNamesMatchChart(t *testing.T) {
	var values map[string]any
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	testCases := []struct {
		subchart string
		want     string
	}{
		{subchart: "csi-driver-nfs", want: CSIProvisionerNFS},
		{subchart: "csi-driver-s3", want: CSIProvisionerS3},
	}

	for _, tc := range testCases {
		t.Run(tc.subchart, func(t *testing.T) {
			block, ok := values[tc.subchart].(map[string]any)
			require.True(t, ok, "the chart carries a %q block", tc.subchart)
			driver, ok := block["driver"].(map[string]any)
			require.True(t, ok, "%q sets driver.name", tc.subchart)
			assert.Equal(t, tc.want, driver["name"])
		})
	}
}

// TestChartManufacturersMatchNodeFeature ties the chart's default manufacturer map to the
// manufacturers the operator knows. The map is the chart's single source for the device-manager
// DaemonSets, the worker's --manufacturer list and the NodeFeatureRule's PCI vendor IDs, so a
// manufacturer added to pkg/nodefeature and forgotten here is detected by nothing.
func TestChartManufacturersMatchNodeFeature(t *testing.T) {
	var values map[string]any
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	want := make(map[string]any)
	for _, manufacturer := range nodefeature.GetKnownAcceleratableManufacturers() {
		want[manufacturer] = nodefeature.GetPciVendorID(manufacturer)
	}

	assert.Equal(t, want, values["manufacturers"])
}

// TestChartPciClassWhitelistMatchesNodeFeature ties the PCI device classes the chart tells NFD
// to label to the ones the gpustack-cpu-info NodeFeatureRule matches. The rule is rendered in
// Go (see apps_nfd_node_feature_rule.go), so the two lists are no longer one value read twice:
// a class NFD publishes but the rule ignores labels nothing, and a class the rule matches but
// NFD never publishes matches nothing.
func TestChartPciClassWhitelistMatchesNodeFeature(t *testing.T) {
	var values struct {
		NodeFeatureDiscovery struct {
			Worker struct {
				Config struct {
					Sources struct {
						PCI struct {
							DeviceClassWhitelist []string `yaml:"deviceClassWhitelist"`
						} `yaml:"pci"`
					} `yaml:"sources"`
				} `yaml:"config"`
			} `yaml:"worker"`
		} `yaml:"node-feature-discovery"`
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	assert.Equal(t, nodefeature.GetAcceleratablePciClassPrefixes(),
		values.NodeFeatureDiscovery.Worker.Config.Sources.PCI.DeviceClassWhitelist)
}

// TestChartDefaultsMatchImageModeOverlay renders the chart twice — once at its defaults, as a
// user installs it, and once with the overlay the worker computes for its own install — and
// asserts the two agree on everything but the worker itself, which the worker never redeploys.
//
// It is a defaults-against-defaults comparison by construction: image mode has no user-values
// channel, so it cannot detect a value the overlay fails to forward. What it does catch is the
// overlay diverging from the chart it drives — a renamed switch, a shifted resource name, a
// manufacturer that stops rendering its DaemonSet.
func TestChartDefaultsMatchImageModeOverlay(t *testing.T) {
	overlay, err := getGPUStackOperatorChartTemplateValues(
		gpustackOperatorChartName, operatorChartValuesContext(nil)).
		GetValues(context.Background())
	require.NoError(t, err, "render the image-mode overlay")

	// Both renders take the same release name, so only the values differ. The overlay's
	// fullnameOverride is what makes the two name their resources alike, which is also what
	// lets either release adopt the other's objects.
	atDefaults := renderChart(t, nil)
	fromOverlay := renderChart(t, overlay)

	wantKeys, workerKeys := sets.New[string](), sets.New[string]()
	for k, o := range atDefaults {
		if o.component == "operator" {
			workerKeys.Insert(k)
			continue
		}
		wantKeys.Insert(k)
	}
	require.NotEmpty(t, workerKeys, "the default render deploys the worker")

	gotKeys := sets.KeySet(fromOverlay)
	assert.Empty(t, sorted(gotKeys.Difference(wantKeys)), "objects the overlay adds")
	assert.Empty(t, sorted(wantKeys.Difference(gotKeys)), "objects the overlay drops")

	for _, k := range sorted(wantKeys.Intersection(gotKeys)) {
		assert.Equal(t, atDefaults[k].manifest, fromOverlay[k].manifest, "%s must render alike", k)
	}
}

// renderedObject is one object of a rendered chart.
type renderedObject struct {
	// component is the app.kubernetes.io/component label, which is how the worker's own
	// objects are told apart from everything else the chart deploys.
	component string
	manifest  string
}

// renderChart renders the operator chart offline, the way `helm template` does.
func renderChart(t *testing.T, values map[string]any) map[string]renderedObject {
	t.Helper()

	chart, err := helmloader.Load(chartDir)
	require.NoError(t, err, "load the operator chart")

	install := helmaction.NewInstall(&helmaction.Configuration{})
	install.ClientOnly = true
	install.DryRun = true
	install.ReleaseName = "gpustack-operator"
	install.Namespace = SystemNamespaceName
	install.IncludeCRDs = false
	// Helm's built-in capabilities claim Kubernetes 1.20, which the chart's own floor refuses.
	// Both renders get the same version, so nothing version-gated diverges between them.
	install.KubeVersion = &helmchartutil.KubeVersion{Version: "v1.33.0", Major: "1", Minor: "33"}

	release, err := install.RunWithContext(t.Context(), chart, values)
	require.NoError(t, err, "render the operator chart")

	objects := make(map[string]renderedObject)
	for _, manifest := range releaseutil.SplitManifests(release.Manifest) {
		var object struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string            `yaml:"name"`
				Namespace string            `yaml:"namespace"`
				Labels    map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(manifest), &object))
		if object.Kind == "" {
			continue
		}

		key := strings.Join([]string{object.Kind, object.Metadata.Namespace, object.Metadata.Name}, "/")
		objects[key] = renderedObject{
			component: object.Metadata.Labels["app.kubernetes.io/component"],
			// The source-file header carries the template path, which is stable, but the
			// leading document separator is not.
			manifest: strings.TrimPrefix(strings.TrimSpace(manifest), "---\n"),
		}
	}

	return objects
}

func sorted(s sets.Set[string]) []string {
	out := s.UnsortedList()
	sort.Strings(out)

	return out
}
