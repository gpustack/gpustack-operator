package kuberess

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
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

	funcMap := extendNfdChartValuesTemplateFuncMap()

	values := getNfdChartTemplateValues(name, valuesContext, funcMap)

	chart := &helm.Chart{
		Name:                    name,
		Version:                 version,
		Release:                 release,
		Path:                    path,
		DownloadURL:             download,
		Values:                  values,
		SkippedCRDsInstallation: true, // Skip installation the CRDs of the chart.
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
  pullPolicy: "{{ default "IfNotPresent" $.ImagePullPolicy }}"
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
{{- $pciClassPrefixes := getPciClassPrefixes }}
{{- $pciVendorIDs := getPciIDs }}
  config:
    core:
      labelSources:
        - "cpu"
        - "pci"
        - "custom"
      labelWhiteList: '^(pci-|cpu-model\.|acceleratable)'
    sources:
      pci:
        deviceClassWhitelist:
{{- range $pciClassPrefixes }}
          - {{ . | quote }}
{{- end }}
        deviceLabelFields:
          - vendor
      custom:
        - name: "has acceleratable devices"
          vars:
            has-acceleratable-devices: "true"
          matchFeatures:
            - feature: pci.device
              matchExpressions:
                class: {op: InRegexp, value: [{{ range $i, $c := $pciClassPrefixes }}{{ if $i }}, {{ end }}{{ printf "^%s" $c | quote }}{{ end }}]}
                vendor: {op: In, value: {{ toJson $pciVendorIDs }}}
        - name: "hasn't acceleratable devices"
          labels:
            "feature.gpustack.ai/acceleratable": "false"
          matchFeatures:
            - feature: rule.matched
              matchExpressions:
                has-acceleratable-devices: {op: DoesNotExist}

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

func getNfdChartTemplateValues(name string, data map[string]any, extendFuncMap template.FuncMap) helm.TemplateValues {
	return helm.TemplateValues{
		Application:   name,
		Template:      nfdChartValuesTemplate,
		ExtendFuncMap: extendFuncMap,
		Context:       data,
	}
}

func extendNfdChartValuesTemplateFuncMap() template.FuncMap {
	return template.FuncMap{
		"getPciIDs": nodefeature.GetPciIDs,
		"getPciClassPrefixes": func() []string {
			var r []string
			for _, p := range strings.Split(osx.Getenv("GPUSTACK_PCI_CLASS_PREFIXES"), ",") {
				if p = strings.TrimSpace(p); p != "" {
					r = append(r, p)
				}
			}
			if len(r) == 0 {
				// Default to the PCI device classes of display/accelerator related devices,
				// see https://admin.pci-ids.ucw.cz/read/PD.
				r = []string{"02", "03", "06", "0b", "12"}
			}
			return r
		},
	}
}
