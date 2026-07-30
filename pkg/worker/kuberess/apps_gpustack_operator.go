package kuberess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	conregname "github.com/google/go-containerregistry/pkg/name"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
)

const (
	// gpustackOperatorChartName is the name of the operator's own chart.
	gpustackOperatorChartName = "gpustack-operator"
	// gpustackOperatorReleaseName is the release the worker installs the bundled chart
	// under. It keeps the name earlier versions gave the device-manager-only release,
	// even though the release now covers every bundled application, so no existing
	// cluster needs a release migration.
	gpustackOperatorReleaseName = "gpustack-operator-device-manager"
)

// legacyApplicationReleases are the per-application Helm releases earlier versions
// installed, before the applications became subcharts of the operator's own chart.
// Their presence is what positively identifies a migration.
var legacyApplicationReleases = []string{
	"gpustack-kueue",
	"gpustack-node-feature-discovery",
	"gpustack-csi-driver-nfs",
	"gpustack-csi-driver-s3",
}

// installGPUStackOperator installs the operator's own chart, which bundles Kueue, Node
// Feature Discovery and the CSI drivers as subcharts alongside the device-managers.
//
// This is the worker's self-contained install path: no chart drives the worker, so it
// installs the chart packaged into its own image (see pack/gpustack-operator/Dockerfile)
// with an overlay computed from its settings and flags. When a chart already deploys the
// worker, --disable-applications carries the wildcard and this never runs.
func installGPUStackOperator(
	ctx context.Context,
	helmCli *helm.Client,
	globalValuesContext map[string]any,
	disable sets.Set[string],
	exclusive bool,
) error {
	chartVersion := gpustackOperatorChartVersion()
	path, err := gpustackOperatorChartPath(chartVersion)
	if err != nil {
		return err
	}

	// Reuse the exact image of the running worker so the device-managers always match the
	// operator in use; fall back to the chart-composed image when it cannot be determined.
	imageRepository, imageTag := splitImageReference(extractWorkerImage(ctx, helmCli.KubeClientSet()))

	// Restate the identity of every manufacturer on the worker's --manufacturer list, so the
	// chart renders the device-managers and Kueue's credits mapping on the values in force
	// here rather than on its own defaults.
	manufacturers, _ := globalValuesContext["Manufacturers"].([]string)
	manufacturerIdentities := make(map[string]map[string]string, len(manufacturers))
	for _, m := range manufacturers {
		manufacturerIdentities[m] = manufacturerIdentity(m)
	}

	valuesContext := globalValuesContext
	valuesContext["Release"] = gpustackOperatorReleaseName
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["ImageRepository"] = imageRepository
	valuesContext["ImageTag"] = imageTag
	valuesContext["ManufacturerIdentities"] = manufacturerIdentities
	valuesContext["ComponentSwitches"] = componentSwitches(disable)

	values := getGPUStackOperatorChartTemplateValues(gpustackOperatorChartName, valuesContext)

	takeOwnership, err := hasLegacyApplicationRelease(ctx, helmCli.KubeClientSet(), helmCli.DefaultNamespace())
	if err != nil {
		return fmt.Errorf("detect legacy application releases: %w", err)
	}
	if takeOwnership {
		klog.Info("adopting the objects of the legacy per-application releases")
	}

	chart := &helm.Chart{
		Name:    gpustackOperatorChartName,
		Version: chartVersion,
		Release: gpustackOperatorReleaseName,
		Path:    path,
		Values:  values,
		// The CRDs of the vendored subcharts (Node Feature Discovery's) are installed:
		// nothing else ships them, and the parent's NodeFeatureRule needs them. Helm
		// skips a CRD that already exists, so a cluster carrying its own is left alone.
		// Kueue templates its CRDs, so they arrive with the release either way.
		//
		// Repair a failed release with an upgrade, never the uninstall+install path: the
		// release now owns Kueue, whose CRDs are Helm-managed templates and whose custom
		// resources carry controller finalizers, so uninstalling tears down the controller
		// while the finalizers still pin the CRs and strands the CRDs.
		RepairViaUpgradeOnly: true,
		TakeOwnership:        takeOwnership,
		// Set only where InstallApplications took a Lease its predecessor had released, so
		// a pending release record found here belongs to a replica that finished or died
		// rather than to one still working. Repairing such a record is what clears a wedge
		// no later attempt could.
		ExclusiveAccess: exclusive,
	}

	return installConvergedChart(ctx, helmCli, chart)
}

// manufacturerIdentity returns a manufacturer's identity as a row of the chart's
// global.manufacturers map, carrying the values in force in this process — each one a
// pkg/nodefeature default a GPUSTACK_<MANUFACTURER>_* variable may have overridden.
//
// A field the operator has no value for is left out, and so is runtimeName: pkg/nodefeature
// names a runtime for two more manufacturers than the chart creates a RuntimeClass for, and
// Helm merges these values into the chart's defaults row by row, so stating it here would
// conjure classes a chart-mode install never creates.
func manufacturerIdentity(manufacturer string) map[string]string {
	identity := map[string]string{
		"pciVendorID": nodefeature.GetPciVendorID(manufacturer),
		"resourceName": string(nodefeature.GetAcceleratableResourceName(
			manufacturer, workercore.DeviceAllocationModeExclusive)),
	}
	if partitionKind := nodefeature.GetPartitionKind(manufacturer); partitionKind != "" {
		identity["partitionKind"] = partitionKind
	}

	return identity
}

const (
	// heldReleaseRetries bounds how often an install losing to a concurrently booting
	// replica is retried before the boot fails on it.
	heldReleaseRetries = 5
	// heldReleaseRetryInterval is the wait between those retries. It is deliberately flat:
	// what is being waited out is the peer's own install, whose duration has nothing to do
	// with how many times this process has looked.
	heldReleaseRetryInterval = 10 * time.Second
)

// installConvergedChart installs the chart, tolerating the release a concurrently booting
// replica created in the window between this process's release lookup and its own install:
// only one of them can create it, and the loser must not fail the boot.
//
// Each retry re-runs the lookup, which now finds the winner's release and either accepts it
// as converged at these values or waits out its pending install.
func installConvergedChart(ctx context.Context, helmCli *helm.Client, chart *helm.Chart) error {
	for attempt := 0; ; attempt++ {
		_, err := helmCli.Install(ctx, chart)
		if err == nil || !isReleaseHeldByPeer(err) || attempt == heldReleaseRetries {
			return err
		}

		klog.InfoS("release is held by a peer, converging on it",
			"release", chart.Release, "attempt", attempt+1, "err", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(heldReleaseRetryInterval):
		}
	}
}

// isReleaseHeldByPeer reports whether the error is Helm refusing to act because another
// process is already operating on the same release. Helm signals it three ways, one per
// point a loser can land on:
//
//   - the name lookup, before anything is written
//     (helm.sh/helm/v3/pkg/action.(*Install).availableName);
//   - the create of the revision record, which is the only true compare-and-create in the
//     sequence (helm.sh/helm/v3/pkg/storage/driver.ErrReleaseExists);
//   - an upgrade finding the release already pending (helm.sh/helm/v3/pkg/action.errPending).
//
// Only the second carries a sentinel; the other two are bare errors, so their message is
// the sole signal.
func isReleaseHeldByPeer(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, helmdriver.ErrReleaseExists) {
		return true
	}

	msg := err.Error()

	return strings.Contains(msg, "cannot re-use a name that is still in use") ||
		strings.Contains(msg, "another operation (install/upgrade/rollback) is in progress")
}

// hasLegacyApplicationRelease reports whether any per-application Helm release from before
// the subchart layout is still recorded in the namespace.
//
// It gates TakeOwnership, which adopts every live object the render resolves by name,
// namespace and kind — it cannot tell a former release's object from one a user created by
// hand. Adoption is therefore offered only to a cluster that positively carries the
// releases being migrated, never unconditionally.
func hasLegacyApplicationRelease(ctx context.Context, cli kubernetes.Interface, namespace string) (bool, error) {
	// Helm records a release as a Secret labeled with its owner and release name.
	sel := fmt.Sprintf("owner=helm,name in (%s)", strings.Join(legacyApplicationReleases, ","))
	list, err := cli.CoreV1().Secrets(namespace).List(ctx, meta.ListOptions{
		LabelSelector: sel,
		Limit:         1,
	})
	if err != nil {
		return false, fmt.Errorf("list helm release secrets: %w", err)
	}

	return len(list.Items) > 0, nil
}

// gpustackOperatorChartTemplate renders the operator chart's values for the worker's own
// install: the worker is already running, so only the applications are deployed, each
// switched by --disable-applications. fullnameOverride pins the resource names regardless
// of the release name.
//
// The pull policy goes onto global.imagePullPolicy, which reaches the subcharts too; the
// parent's own image.pullPolicy never does.
const gpustackOperatorChartTemplate = `
{{- if or $.ContainerRegistry (not $.ImageRepository) $.ImagePullPolicy $.ImagePullSecrets $.ManufacturerIdentities }}
global:
  {{- /*
    The registry goes out whatever the worker's own image turns out to be, because it is the
    subcharts' only route to a mirror: image mode has no user-values channel, so a registry
    withheld here means every subchart pulling from wherever its reference already points.
    Rewriting the pinned operator image's registry with it is a no-op in any deployment that
    has a mirror at all — that image came from it.

    The namespace cannot travel the same way. It replaces the namespace segment of every
    reference, the operator image this overlay just pinned to the running worker's included,
    and the two need not agree: a packaged build puts the operator in its own namespace while
    the mirrored dependencies stay in the default one. Rewriting it there points the
    device-managers and the hook Jobs at an image that does not exist, which fails the install
    on a pull it can never satisfy. So it is emitted only where no worker image was resolved
    and nothing is at risk of being rewritten.
  */}}
  {{- with $.ContainerRegistry }}
  imageRegistry: {{ . | quote }}
  {{- end }}
  {{- if not $.ImageRepository }}
  imageNamespace: {{ default "" $.ContainerNamespace | quote }}
  {{- end }}
  {{- if $.ImagePullPolicy }}
  imagePullPolicy: {{ $.ImagePullPolicy | quote }}
  {{- end }}
  {{- if $.ImagePullSecrets }}
  imagePullSecrets:
  {{- range $.ImagePullSecrets }}
    - name: {{ . }}
  {{- end }}
  {{- end }}
  {{- if $.ManufacturerIdentities }}
  manufacturers:
  {{- range $manu, $identity := $.ManufacturerIdentities }}
    {{ $manu }}:
    {{- range $field, $value := $identity }}
      {{ $field }}: {{ $value | quote }}
    {{- end }}
  {{- end }}
  {{- end }}
{{- end }}

fullnameOverride: gpustack-operator
{{- if $.ImageRepository }}

# The chart's own image knob, not deviceManager's: every component that runs this image reads it
# through the same helper, which merges a per-component override over this one. Setting it here
# reaches the device-managers AND the migration hook Jobs, which run this image too and would
# otherwise resolve the chart's default tag — a tag that need not exist wherever this build came
# from, leaving the hook to fail the install on a pull it can never satisfy.
image:
  repository: {{ $.ImageRepository | quote }}
  {{- if $.ImageTag }}
  tag: {{ $.ImageTag | quote }}
  {{- end }}
{{- end }}

worker:
  enabled: false

deviceManager:
  enabled: {{ index $.ComponentSwitches "deviceManager" }}

kueue:
  enabled: {{ index $.ComponentSwitches "kueue" }}

node-feature-discovery:
  enabled: {{ index $.ComponentSwitches "node-feature-discovery" }}

csi-driver-nfs:
  enabled: {{ index $.ComponentSwitches "csi-driver-nfs" }}

csi-driver-s3:
  enabled: {{ index $.ComponentSwitches "csi-driver-s3" }}
`

func getGPUStackOperatorChartTemplateValues(name string, data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Application: name,
		Template:    gpustackOperatorChartTemplate,
		Context:     data,
	}
}

// gpustackOperatorChartPath locates the chart bundled into the operator image, falling back
// to the source tree so the worker can also run from a checkout.
func gpustackOperatorChartPath(chartVersion string) (string, error) {
	path := filepath.Join(system.SubConfDir("charts"),
		fmt.Sprintf("%s-%s.tgz", gpustackOperatorChartName, chartVersion))
	if osx.ExistsFile(path) {
		return path, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	path = filepath.Join(dir, "deploy", gpustackOperatorChartName, "chart")
	if !osx.ExistsDir(path) {
		return "", fmt.Errorf("chart not found at %s", path)
	}

	return path, nil
}

// extractWorkerImage returns the image of the running worker's "main" container, or an empty
// string when it cannot be determined (e.g. running outside the cluster).
func extractWorkerImage(ctx context.Context, cli kubernetes.Interface) string {
	podName := osx.Getenv("KUBERNETES_POD_NAME")
	if podName == "" {
		return ""
	}

	pod, err := cli.CoreV1().
		Pods(SystemNamespaceName).
		Get(ctx, podName, meta.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return ""
	}

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "main" {
			return pod.Spec.Containers[i].Image
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Image
	}
	return ""
}

// splitImageReference splits a tagged image reference into the repository and tag understood by the
// chart's "image" values. References that cannot be expressed as "<repository>:<tag>" — digest
// pinned ("<repo>@sha256:...") or otherwise unparseable — return empty fields, letting the chart
// compose its own default image instead of an invalid "<repo>@<digest>:<tag>".
func splitImageReference(ref string) (repository, tag string) {
	if ref == "" {
		return "", ""
	}
	// Classify the reference with the registry library; only tag-based references map cleanly onto
	// the chart's repository/tag fields. Digest references parse as conregname.Digest, not Tag.
	parsed, err := conregname.ParseReference(ref)
	if err != nil {
		return "", ""
	}
	if _, ok := parsed.(conregname.Tag); !ok {
		return "", ""
	}
	lastSlash := strings.LastIndex(ref, "/")
	if lastColon := strings.LastIndex(ref, ":"); lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, ""
}

// gpustackOperatorChartVersion resolves the version of the operator chart bundled into the
// image. It tracks the operator binary version (a release tag like "v0.6.2", stripped of its
// "v"), falling back to "0.0.0" for development builds. The image build derives the same value
// when packaging the chart (see pack/gpustack-operator/Dockerfile).
func gpustackOperatorChartVersion() string {
	if version.IsValid() {
		return strings.TrimPrefix(version.Version, "v")
	}
	return "0.0.0"
}
