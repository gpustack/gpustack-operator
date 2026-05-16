package kuberess

import (
	"context"
	"fmt"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"

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
	path := filepath.Join(system.SubConfDir("charts"), fmt.Sprintf("%s-%s.tgz", name, version))
	download := fmt.Sprintf("https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v%[1]s/node-feature-discovery-chart-%[1]s.tgz", version) // nolint:lll

	valuesContext := globalValuesContext
	valuesContext["Release"] = release
	valuesContext["Namespace"] = helmCli.DefaultNamespace()

	values := getNfdChartTemplateValues(name, valuesContext)

	chart := &helm.Chart{
		Name:        name,
		Version:     version,
		Release:     release,
		Path:        path,
		DownloadURL: download,
		Values:      values,
		// Skip installation the CRDs of the chart.
		SkippedCRDsInstallation: true,
	}
	_, err := helmCli.Install(ctx, chart)
	if err != nil {
		return err
	}

	return nil
}

const nfdChartValuesTemplate = `
fullnameOverride: "node-feature-discovery"
namespaceOverride: "{{ $.Namespace }}"

image:
{{- $registry := default "docker.io" $.ContainerRegistry }}
{{- $namespace := default "gpustack" $.ContainerNamespace }}
{{- $prefix := "mirrored" }}
{{- $image := printf "%s/%s/%s-node-feature-discovery" $registry $namespace $prefix }}
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
  annotations:
    {{ $.ManagedLabel }}: "true"
  deploymentAnnotations:
    {{ $.ManagedLabel }}: "true"
  serviceAccount:
    create: true
    annotations:
      {{ $.ManagedLabel }}: "true"
  config:
    restrictions:
      nodeFeatureNamespaceSelector:
       matchLabels:
         kubernetes.io/metadata.name: "{{ $.Namespace }}"

worker:
  enable: true
  tolerations:
    - operator: "Exists"
  annotations:
    {{ $.ManagedLabel }}: "true"
  daemonsetAnnotations:
    {{ $.ManagedLabel }}: "true"
  serviceAccount:
    create: true
    annotations:
      {{ $.ManagedLabel }}: "true"
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
  deploymentAnnotations:
    {{ $.ManagedLabel }}: "true"
  serviceAccount:
    create: true
    annotations:
      {{ $.ManagedLabel }}: "true"

prometheus:
  enable: false
`

func getNfdChartTemplateValues(name string, data map[string]any) helm.TemplateValues {
	return helm.TemplateValues{
		Application: name,
		Template:    nfdChartValuesTemplate,
		Context:     data,
	}
}
