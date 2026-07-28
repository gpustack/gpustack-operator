package kuberess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	conregname "github.com/google/go-containerregistry/pkg/name"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

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
func installGPUStackOperator(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	chartVersion := gpustackOperatorChartVersion()
	path, err := gpustackOperatorChartPath(chartVersion)
	if err != nil {
		return err
	}

	// Reuse the exact image of the running worker so the device-managers always match the
	// operator in use; fall back to the chart-composed image when it cannot be determined.
	imageRepository, imageTag := splitImageReference(extractWorkerImage(ctx, helmCli.KubeClientSet()))

	// Build the manufacturer -> PCI vendor ID map from the worker's --manufacturer list.
	manufacturers, _ := globalValuesContext["Manufacturers"].([]string)
	manufacturerVendorIDs := make(map[string]string, len(manufacturers))
	for _, m := range manufacturers {
		manufacturerVendorIDs[m] = nodefeature.GetPciVendorID(m)
	}

	valuesContext := globalValuesContext
	valuesContext["Release"] = gpustackOperatorReleaseName
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["ImageRepository"] = imageRepository
	valuesContext["ImageTag"] = imageTag
	valuesContext["ManufacturerVendorIDs"] = manufacturerVendorIDs
	valuesContext["ComponentSwitches"] = componentSwitches(disable)

	values := getGPUStackOperatorChartTemplateValues(gpustackOperatorChartName, valuesContext)

	// Self-heal a Kueue left deadlocked by an earlier upgrade before (re)installing:
	// a Terminating Kueue CRD would otherwise make every install below fail forever
	// and block the operator from starting. No-op on a healthy cluster.
	if err := reapOrphanedKueue(ctx, helmCli); err != nil {
		return fmt.Errorf("reap orphaned kueue: %w", err)
	}

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
	}

	return installConvergedChart(ctx, helmCli, chart)
}

// installConvergedChart installs the chart, tolerating the release a concurrently booting
// replica created in the window between this process's release lookup and its own install:
// only one of them can create it, and the loser must not fail the boot.
//
// The retry re-runs the lookup, which now finds the winner's release and either accepts it
// as converged at these values or waits out its pending install.
func installConvergedChart(ctx context.Context, helmCli *helm.Client, chart *helm.Chart) error {
	_, err := helmCli.Install(ctx, chart)
	if err == nil || !isReleaseNameTaken(err) {
		return err
	}

	klog.InfoS("release was created concurrently, converging on it", "release", chart.Release)
	_, err = helmCli.Install(ctx, chart)

	return err
}

// isReleaseNameTaken reports whether the error is Helm refusing to install because a
// release of that name already exists. Helm returns a bare error for it, so the message is
// the only signal (helm.sh/helm/v3/pkg/action.(*Install).availableName).
func isReleaseNameTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cannot re-use a name that is still in use")
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
{{- if or (not $.ImageRepository) $.ImagePullPolicy $.ImagePullSecrets }}
global:
  {{- if not $.ImageRepository }}
  imageRegistry: {{ default "" $.ContainerRegistry | quote }}
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
{{- end }}

fullnameOverride: gpustack-operator

worker:
  enabled: false

deviceManager:
  enabled: {{ index $.ComponentSwitches "deviceManager" }}
{{- if $.ImageRepository }}
  image:
    repository: {{ $.ImageRepository | quote }}
    {{- if $.ImageTag }}
    tag: {{ $.ImageTag | quote }}
    {{- end }}
{{- end }}

nodeFeatureRule:
  enabled: {{ index $.ComponentSwitches "nodeFeatureRule" }}

kueue:
  enabled: {{ index $.ComponentSwitches "kueue" }}

node-feature-discovery:
  enabled: {{ index $.ComponentSwitches "node-feature-discovery" }}

csi-driver-nfs:
  enabled: {{ index $.ComponentSwitches "csi-driver-nfs" }}

csi-driver-s3:
  enabled: {{ index $.ComponentSwitches "csi-driver-s3" }}

{{- if $.ManufacturerVendorIDs }}
manufacturers:
{{- range $manu, $vendorID := $.ManufacturerVendorIDs }}
  {{ $manu }}: {{ $vendorID | quote }}
{{- end }}
{{- end }}
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
