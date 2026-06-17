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

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
)

func installGPUStackDeviceManager(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB(thxCode): the device-managers are rendered from the operator's own chart, which is
	// packaged into the image at build time (see pack/gpustack-operator/Dockerfile). The chart
	// version is resolved by deviceManagerChartVersion below.

	name := "device-manager"
	if disable.Has(name) {
		return nil
	}

	chartName := "gpustack-operator"
	chartVersion := deviceManagerChartVersion()
	release := "gpustack-operator-device-manager"
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", chartName, chartVersion))
	if !osx.ExistsFile(path) {
		dir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		path = filepath.Join(dir, "deploy", "gpustack-operator", "chart")
		if !osx.ExistsDir(path) {
			return fmt.Errorf("chart not found at %s", path)
		}
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
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()
	valuesContext["ImageRepository"] = imageRepository
	valuesContext["ImageTag"] = imageTag
	valuesContext["ManufacturerVendorIDs"] = manufacturerVendorIDs

	values := getGPUStackDeviceManagerChartTemplateValues(chartName, valuesContext)

	chart := &helm.Chart{
		Name:                    chartName,
		Version:                 chartVersion,
		Release:                 release,
		Path:                    path,
		Values:                  values,
		SkippedCRDsInstallation: true,
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	return nil
}

// gpustackDeviceManagerChartTemplate renders the operator chart's values so that only the
// per-manufacturer device-manager DaemonSets are installed (the worker is already running).
// fullnameOverride pins the resource names regardless of the release name.
const gpustackDeviceManagerChartTemplate = `
{{- if or (not $.ImageRepository) $.ImagePullSecrets }}
global:
  {{- if not $.ImageRepository }}
  imageRegistry: {{ default "" $.ContainerRegistry | quote }}
  imageNamespace: {{ default "" $.ContainerNamespace | quote }}
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
  enabled: true
{{- if $.ImageRepository }}
  image:
    repository: {{ $.ImageRepository | quote }}
    {{- if $.ImageTag }}
    tag: {{ $.ImageTag | quote }}
    {{- end }}
{{- end }}

image:
  pullPolicy: {{ default "IfNotPresent" $.ImagePullPolicy | quote }}

{{- if $.ManufacturerVendorIDs }}
manufacturers:
{{- range $manu, $vendorID := $.ManufacturerVendorIDs }}
  {{ $manu }}: {{ $vendorID | quote }}
{{- end }}
{{- end }}
`

func getGPUStackDeviceManagerChartTemplateValues(name string, data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Application: name,
		Template:    gpustackDeviceManagerChartTemplate,
		Context:     data,
	}
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

// deviceManagerChartVersion resolves the version of the operator chart bundled into the image.
// It tracks the operator binary version (a release tag like "v0.6.2", stripped of its "v"),
// falling back to "0.0.0" for development builds. The image build derives the same value when
// packaging the chart (see pack/gpustack-operator/Dockerfile).
func deviceManagerChartVersion() string {
	if version.IsValid() {
		return strings.TrimPrefix(version.Version, "v")
	}
	return "0.0.0"
}
