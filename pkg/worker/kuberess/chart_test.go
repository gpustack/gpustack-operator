package kuberess

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	helmrelease "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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

// TestChartManufacturersMatchNodeFeature ties every row of the chart's manufacturer map to what
// pkg/nodefeature states about that manufacturer. The map is the chart's single source for the
// device-manager DaemonSets, the worker's --manufacturer list, the RuntimeClasses the chart
// creates, the Kueue credits mapping and the GPUSTACK_<MANUFACTURER>_* variables it fans out, so a
// row drifting from the code that consumes it is detected by nothing else.
//
// runtimeName is asserted as a subset: the chart deliberately states one only for the vendors
// whose container runtime registers a handler by that name, because the operator attaches a
// RuntimeClass to a workload whenever one exists and a class no runtime backs would fail every
// Pod of that vendor. runtimeInjectsDriver is the chart's own fact — no Go code holds it — so it
// is asserted only to name a runtime that is actually there.
func TestChartManufacturersMatchNodeFeature(t *testing.T) {
	var values struct {
		Global struct {
			Manufacturers map[string]struct {
				PciVendorID          string `yaml:"pciVendorID"`
				ResourceName         string `yaml:"resourceName"`
				RuntimeName          string `yaml:"runtimeName"`
				RuntimeInjectsDriver bool   `yaml:"runtimeInjectsDriver"`
				PartitionKind        string `yaml:"partitionKind"`
			} `yaml:"manufacturers"`
		} `yaml:"global"`
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	assert.ElementsMatch(t, nodefeature.GetKnownAcceleratableManufacturers(),
		sets.KeySet(values.Global.Manufacturers).UnsortedList(),
		"the chart states every manufacturer the operator knows, and no other")

	for _, manufacturer := range nodefeature.GetKnownAcceleratableManufacturers() {
		t.Run(manufacturer, func(t *testing.T) {
			row, ok := values.Global.Manufacturers[manufacturer]
			require.True(t, ok, "the chart states this manufacturer")

			assert.Equal(t, nodefeature.GetPciVendorID(manufacturer), row.PciVendorID)
			assert.Equal(t, string(nodefeature.GetAcceleratableResourceName(
				manufacturer, workercore.DeviceAllocationModeExclusive)), row.ResourceName)
			assert.Equal(t, nodefeature.GetPartitionKind(manufacturer), row.PartitionKind)

			if row.RuntimeName != "" {
				assert.Equal(t, nodefeature.GetAcceleratableRuntimeName(manufacturer),
					row.RuntimeName, "a stated runtime name is the operator's own")
			}
			if row.RuntimeInjectsDriver {
				assert.NotEmpty(t, row.RuntimeName,
					"a device-manager can only run under a runtime the row names")
			}
		})
	}
}

// TestChartRuntimeClassesFollowTheDriverInjectors pins WHICH manufacturers the chart creates a
// RuntimeClass for, which is the assertion that keeps two similar-looking fields apart.
// `runtimeName` is the class the operator will use for a vendor; `runtimeInjectsDriver` marks a
// vendor whose driver reaches a container only through that runtime, and only there can the
// runtime's presence be inferred from the accelerators working at all.
//
// Creating one anywhere else breaks the vendor it was meant to serve: InstanceReconciler attaches
// a RuntimeClass whenever one exists, so a class no container runtime backs makes the kubelet
// reject every Pod of that vendor — on a cluster where, without the class, they ran.
func TestChartRuntimeClassesFollowTheDriverInjectors(t *testing.T) {
	var values struct {
		Global struct {
			Manufacturers map[string]struct {
				RuntimeName          string `yaml:"runtimeName"`
				RuntimeInjectsDriver bool   `yaml:"runtimeInjectsDriver"`
			} `yaml:"manufacturers"`
		} `yaml:"global"`
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	want, useOnly := sets.New[string](), sets.New[string]()
	for _, row := range values.Global.Manufacturers {
		switch {
		case row.RuntimeInjectsDriver:
			want.Insert(row.RuntimeName)
		case row.RuntimeName != "":
			useOnly.Insert(row.RuntimeName)
		}
	}
	require.NotEmpty(t, want, "some manufacturer's runtime injects its driver")
	require.NotEmpty(t, useOnly, "some manufacturer names a runtime it does not create")

	got := sets.New[string]()
	for key := range renderChart(t, nil) {
		if name, ok := strings.CutPrefix(key, "RuntimeClass//"); ok {
			got.Insert(name)
		}
	}

	assert.Equal(t, sorted(want), sorted(got),
		"the chart creates a RuntimeClass exactly where the runtime injects the driver")
	assert.Empty(t, sorted(useOnly.Intersection(got)),
		"a runtime this chart does not install is never conjured, only used where it exists")
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

// TestChartKueueTransformationsMatchNodeFeature holds the credits mapping Kueue is configured
// with equal to what pkg/nodefeature computes. The chart renders that mapping from
// global.manufacturers through a template the vendored Kueue chart includes, so the constants it
// is scored on — CreditsPerAccelerator, SharedResourceMaxSize, ResourceMaxUnits — are restated in a
// template and nothing but this test holds the two statements together.
//
// It renders the whole chart on purpose: the mapping only reaches Kueue if the helper, the patch
// and the values all agree, and a check on any one of them in isolation would pass while the
// scheduling chain admitted nothing.
func TestChartKueueTransformationsMatchNodeFeature(t *testing.T) {
	const (
		exclusiveCredits = nodefeature.CreditsPerAccelerator
		sharedCredits    = nodefeature.CreditsPerAccelerator / nodefeature.SharedResourceMaxSize
		unitCredits      = nodefeature.CreditsPerAccelerator / nodefeature.ResourceMaxUnits
	)

	var want []transformation
	for _, manufacturer := range nodefeature.GetKnownAcceleratableManufacturers() {
		credits := string(nodefeature.GetAcceleratableCreditsResourceName(manufacturer))
		resource := func(mode workercore.DeviceAllocationMode) string {
			return string(nodefeature.GetAcceleratableResourceName(manufacturer, mode))
		}
		output := func(credit int) map[string]string {
			return map[string]string{credits: strconv.Itoa(credit)}
		}

		want = append(want,
			transformation{
				Input:    resource(workercore.DeviceAllocationModeExclusive),
				Strategy: "Replace",
				Outputs:  output(exclusiveCredits),
			},
			transformation{
				Input:    resource(workercore.DeviceAllocationModeShared),
				Strategy: "Replace",
				Outputs:  output(sharedCredits),
			},
			transformation{
				Input:      string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)),
				Strategy:   "Replace",
				MultiplyBy: resource(workercore.DeviceAllocationModeSliced),
				Outputs:    output(unitCredits),
			})

		// A manufacturer with no partition kind advertises no ".partitioned" family at all,
		// so it has no fourth rule.
		if partitionedUnits := nodefeature.GetAcceleratablePartitionedUnitsResourceName(
			manufacturer); partitionedUnits != "" {
			want = append(want, transformation{
				Input:    string(partitionedUnits),
				Strategy: "Replace",
				Outputs:  output(unitCredits),
			})
		}
	}

	assert.Equal(t, want, renderKueueConfig(t, nil).Resources.Transformations)
}

// TestChartKueueManagedNamespacesFollowTheRelease covers the other half of what the chart renders
// into Kueue's config. Kueue exits when this selector matches the namespace it runs in, so a
// hard-coded namespace makes the chart installable into exactly one — and a subchart value cannot
// be templated, which is why the default is rendered by the parent instead of stated in values.
func TestChartKueueManagedNamespacesFollowTheRelease(t *testing.T) {
	selector := renderKueueConfig(t, nil).ManagedJobsNamespaceSelector

	require.Len(t, selector.MatchExpressions, 1)
	assert.Equal(t, "kubernetes.io/metadata.name", selector.MatchExpressions[0].Key)
	assert.Equal(t, "NotIn", selector.MatchExpressions[0].Operator)
	assert.Equal(t, []string{"kube-system", SystemNamespaceName},
		selector.MatchExpressions[0].Values, "the release's own namespace is excluded")
}

// transformation is one entry of Kueue's resources.transformations.
type transformation struct {
	Input      string            `yaml:"input"`
	Strategy   string            `yaml:"strategy"`
	MultiplyBy string            `yaml:"multiplyBy,omitempty"`
	Outputs    map[string]string `yaml:"outputs"`
}

// kueueConfig is the part of Kueue's Configuration this chart renders rather than states.
type kueueConfig struct {
	InternalCertManagement struct {
		Enable bool `yaml:"enable"`
	} `yaml:"internalCertManagement"`
	Resources struct {
		Transformations []transformation `yaml:"transformations"`
	} `yaml:"resources"`
	ManagedJobsNamespaceSelector struct {
		MatchExpressions []struct {
			Key      string   `yaml:"key"`
			Operator string   `yaml:"operator"`
			Values   []string `yaml:"values"`
		} `yaml:"matchExpressions"`
	} `yaml:"managedJobsNamespaceSelector"`
}

// renderKueueConfig renders the chart and returns the Configuration the Kueue controller reads.
func renderKueueConfig(t *testing.T, values map[string]any) kueueConfig {
	t.Helper()

	object, ok := renderChart(t, values)["ConfigMap/"+SystemNamespaceName+"/kueue-manager-config"]
	require.True(t, ok, "the render carries Kueue's manager config")

	var manifest struct {
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(object.manifest), &manifest))

	var config kueueConfig
	require.NoError(t, yaml.Unmarshal(
		[]byte(manifest.Data["controller_manager_config.yaml"]), &config))

	return config
}

// TestChartCertManagerPathsAgree holds the release to one answer about cert-manager. The worker and
// Kueue each carry a full certificate machinery, and each used to be switched on its own — so a
// cluster with cert-manager installed issued the worker's serving certificate through it while
// Kueue went on self-signing, and no value could line the two up: Helm merges a subchart's values
// instead of templating them, so the CRD probe behind "auto" never reached Kueue at all.
//
// The offline render performs no lookup, which is exactly the cluster without cert-manager; the
// other two cases state the answer, and stating "true" is the same code path the probe takes when
// it finds the CRD.
func TestChartCertManagerPathsAgree(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enabled     string
		certManager bool
	}{
		{name: "auto with cert-manager absent", enabled: "auto", certManager: false},
		{name: "forced on", enabled: "true", certManager: true},
		{name: "forced off", enabled: "false", certManager: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]any{
				"global": map[string]any{
					"certmanager": map[string]any{"enabled": tc.enabled},
				},
			}

			objects := renderChart(t, values)
			for _, key := range []string{
				"Certificate/" + SystemNamespaceName + "/gpustack-operator-worker-cert",
				"Issuer/" + SystemNamespaceName + "/gpustack-operator-worker-selfsigned-issuer",
				"Certificate/" + SystemNamespaceName + "/kueue-serving-cert",
				"Issuer/" + SystemNamespaceName + "/kueue-selfsigned-issuer",
			} {
				_, ok := objects[key]
				assert.Equal(t, tc.certManager, ok, "%s is rendered only for cert-manager", key)
			}

			// The certificate Kueue generates itself, which is the path it takes instead.
			_, ok := objects["Secret/"+SystemNamespaceName+"/kueue-webhook-server-cert"]
			assert.Equal(t, !tc.certManager, ok, "Kueue's self-managed certificate")
			assert.Equal(t, !tc.certManager,
				renderKueueConfig(t, values).InternalCertManagement.Enable,
				"Kueue manages its own certificate exactly when cert-manager is not used")
		})
	}
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

// TestChartImageTagDefaultsToAppVersion holds the chart's default image tag to exactly one "v",
// whichever spelling of the version the packaged chart carries.
//
// The image build packages this chart with the version the binary reports
// (pack/gpustack-operator/Dockerfile), and that string carries a leading "v" — a chart whose
// appVersion kept it then asked for "...:vv0.7.2", which no registry serves. Nothing else catches
// it: the e2e version checks compare the packaged chart's version, never its appVersion, and every
// e2e install passes an explicit image, so the default tag is never rendered.
func TestChartImageTagDefaultsToAppVersion(t *testing.T) {
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
		} `yaml:"image"`
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err, "read the chart values")
	require.NoError(t, yaml.Unmarshal(raw, &values))

	testCases := []struct {
		name       string
		appVersion string
	}{
		{name: "prefixed", appVersion: "v9.9.9"},
		{name: "bare", appVersion: "9.9.9"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chart, err := helmloader.Load(chartDir)
			require.NoError(t, err, "load the operator chart")
			chart.Metadata.AppVersion = tc.appVersion

			images := renderedOperatorImages(t, renderRelease(t, chart, nil), values.Image.Repository)
			require.NotEmpty(t, images, "the chart renders operator images")
			for _, image := range images {
				assert.Equal(t, values.Image.Repository+":v9.9.9", image)
			}
		})
	}
}

// renderedImageRE captures the reference of a rendered container image.
var renderedImageRE = regexp.MustCompile(`(?m)^\s*image:\s*"?([^"\s]+)"?\s*$`)

// renderedOperatorImages returns every rendered image that references the operator's own
// repository, taking the release's hooks as well as its manifest: half the images this chart
// emits belong to hook Jobs (the two migrate Jobs and the cleanup Job).
func renderedOperatorImages(t *testing.T, release *helmrelease.Release, repository string) []string {
	t.Helper()

	require.NotEmpty(t, release.Hooks, "the chart renders hooks")
	manifests := make([]string, 0, len(release.Hooks)+1)
	manifests = append(manifests, release.Manifest)
	for _, hook := range release.Hooks {
		manifests = append(manifests, hook.Manifest)
	}

	var images []string
	for _, manifest := range manifests {
		for _, match := range renderedImageRE.FindAllStringSubmatch(manifest, -1) {
			if strings.HasPrefix(match[1], repository+":") {
				images = append(images, match[1])
			}
		}
	}

	return images
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

	objects := make(map[string]renderedObject)
	for _, manifest := range releaseutil.SplitManifests(renderRelease(t, chart, values).Manifest) {
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

// renderRelease renders a loaded chart offline, the way `helm template` does, returning the
// whole release so that a caller can reach the hooks as well as the manifest.
func renderRelease(t *testing.T, chart *helmchart.Chart, values map[string]any) *helmrelease.Release {
	t.Helper()

	install := helmaction.NewInstall(&helmaction.Configuration{})
	install.ClientOnly = true
	install.DryRun = true
	install.ReleaseName = "gpustack-operator"
	install.Namespace = SystemNamespaceName
	install.IncludeCRDs = false
	// Helm's built-in capabilities claim Kubernetes 1.20, which the chart's own floor refuses.
	// Every render gets the same version, so nothing version-gated diverges between them.
	install.KubeVersion = &helmchartutil.KubeVersion{Version: "v1.33.0", Major: "1", Minor: "33"}

	release, err := install.RunWithContext(t.Context(), chart, values)
	require.NoError(t, err, "render the operator chart")

	return release
}

func sorted(s sets.Set[string]) []string {
	out := s.UnsortedList()
	sort.Strings(out)

	return out
}
