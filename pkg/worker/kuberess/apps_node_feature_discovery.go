package kuberess

import (
	"context"
	"fmt"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/system"
)

func installNodeFeatureDiscovery(ctx context.Context, helmCli *helm.Client, globalValuesContext map[string]any, disable sets.Set[string]) error {
	// NB: please update the following files if changed.
	// - pack/gpustack/image/Dockerfile.

	name := "node-feature-discovery"
	version := "0.18.3"
	if disable.Has(name) {
		return nil
	}

	release := "gpustack-node-feature-discovery"
	path := filepath.Join(system.SubDataDir("charts"), fmt.Sprintf("%s-%s.tgz", name, version))
	download := fmt.Sprintf("https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v%[1]s/node-feature-discovery-chart-%[1]s.tgz", version) // nolint:lll

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()

	values := getNfdChartTemplateValues(valuesContext)

	chart := &helm.Chart{
		Name:                            name,
		Version:                         version,
		Release:                         release,
		Path:                            path,
		DownloadURL:                     download,
		Values:                          values,
		DisabledInstallCRDs:             true,
		DisableInstallIfApiServiceReady: fmt.Sprintf("%s.%s", nfd.SchemeGroupVersion.Version, nfd.SchemeGroupVersion.Group),
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	return nil
}

const nfdChartValuesTemplate = `
fullnameOverride: "{{ $.Release }}"
namespaceOverride: "{{ $.Namespace }}"

image:
{{- $registry := default "docker.io" $.ContainerRegistry }}
{{- $namespace := default "gpustack" $.ContainerNamespace }}
{{- $image := printf "%s/%s/mirrored-node-feature-discovery" $registry $namespace }}
  repository: "{{ $image }}"
  pullPolicy: "IfNotPresent"
{{- if $.ImagePullSecrets }}
imagePullSecrets:
{{- range $.ImagePullSecrets }}
  - name: {{ . }}
{{- end }}
{{ end }}

master:
  enable: true
  tolerations:
    - operator: "Exists"
  serviceAccount:
    create: true
    annotations:
      {{ $.ManagedLabel }}: "true"
  annotations:
    {{ $.ManagedLabel }}: "true"

worker:
  enable: true
  annotations:
    {{ $.ManagedLabel }}: "true"
  tolerations:
    - operator: "Exists"
  config:
    sources:
      pci:
        deviceClassWhitelist:
          - "02"
          - "03"
          - "0b"
          - "12"
        deviceLabelFields:
          - vendor

topologyUpdater:
  enable: false

gc:
  enable: true
  tolerations:
    - operator: "Exists"
  annotations:
    {{ $.ManagedLabel }}: "true"
  serviceAccount:
    create: true
    annotations:
      {{ $.ManagedLabel }}: "true"

prometheus:
  enable: false
`

func getNfdChartTemplateValues(data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Template: nfdChartValuesTemplate,
		Context:  data,
	}
}
