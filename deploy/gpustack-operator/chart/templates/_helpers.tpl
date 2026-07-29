{{/*
Chart name, optionally overridden by .Values.nameOverride.
*/}}
{{- define "gpustack-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name used as the base of resource names.
When the release name already contains the chart name, it is used as-is so that
`helm install gpustack-operator .` yields the conventional "gpustack-operator" base.
*/}}
{{- define "gpustack-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "gpustack-operator.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Namespace to deploy into.
*/}}
{{- define "gpustack-operator.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{/*
Chart label value, e.g. "gpustack-operator-0.5.0".
*/}}
{{- define "gpustack-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Worker (control plane) resource name: "<fullname>-worker".
*/}}
{{- define "gpustack-operator.worker.fullname" -}}
{{- printf "%s-worker" (include "gpustack-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Device-manager resource name: "<fullname>-device-manager".
*/}}
{{- define "gpustack-operator.deviceManager.fullname" -}}
{{- printf "%s-device-manager" (include "gpustack-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Resolved operator image reference "{registry/}{namespace/}repository:tag" for a component.
Overlays the component's `image` overrides on the chart-level `image` defaults.
When global.imageNamespace is non-empty it replaces the namespace segment of
`image.repository`; when global.imageRegistry is non-empty it replaces the registry
segment, which is the first path segment carrying a "." or a ":". Each falls back to
what `image.repository` already encodes.
Pass a dict: (dict "root" $ "overrides" .Values.worker.image).
Tag defaults to "v<.Chart.AppVersion>" when unset.
*/}}
{{- define "gpustack-operator.image" -}}
{{- $root := .root -}}
{{- $image := merge (deepCopy .overrides) $root.Values.image -}}
{{- $repository := $image.repository -}}
{{- $registry := "" -}}
{{- $segments := splitList "/" $repository -}}
{{- if and (gt (len $segments) 1) (or (contains "." (first $segments)) (contains ":" (first $segments))) -}}
{{- $registry = first $segments -}}
{{- $repository = join "/" (rest $segments) -}}
{{- end -}}
{{- with $root.Values.global.imageNamespace -}}
{{- $repository = printf "%s/%s" . (last (splitList "/" $repository)) -}}
{{- end -}}
{{- with $root.Values.global.imageRegistry -}}
{{- $registry = trimSuffix "/" . -}}
{{- end -}}
{{- with $registry -}}
{{- $repository = printf "%s/%s" . $repository -}}
{{- end -}}
{{- $tag := default (printf "v%s" $root.Chart.AppVersion) $image.tag -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}

{{/*
Resolved image pull policy for a component, overlaying its `image` overrides on
the chart-level `image` defaults. A non-empty global.imagePullPolicy outranks both,
so that one value governs this chart's workloads and its subcharts' alike. It has to
outrank rather than fill a gap: `image.pullPolicy` carries a non-empty default, which
a fallback could never displace.
Pass a dict: (dict "root" $ "overrides" .Values.worker.image).
*/}}
{{- define "gpustack-operator.imagePullPolicy" -}}
{{- $image := merge (deepCopy .overrides) .root.Values.image -}}
{{- default $image.pullPolicy .root.Values.global.imagePullPolicy -}}
{{- end -}}

{{/*
Common labels added to every resource.
*/}}
{{- define "gpustack-operator.commonLabels" -}}
helm.sh/chart: {{ include "gpustack-operator.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: gpustack-operator
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Worker metadata labels.
*/}}
{{- define "gpustack-operator.worker.labels" -}}
{{ include "gpustack-operator.commonLabels" . }}
app.kubernetes.io/component: operator
app.kubernetes.io/name: {{ include "gpustack-operator.worker.fullname" . }}
{{- end -}}

{{/*
Worker selector labels. Kept minimal and stable across upgrades (immutable selectors).
*/}}
{{- define "gpustack-operator.worker.selectorLabels" -}}
app.kubernetes.io/part-of: gpustack-operator
app.kubernetes.io/component: operator
app.kubernetes.io/name: {{ include "gpustack-operator.worker.fullname" . }}
{{- end -}}

{{/*
Device-manager metadata labels.
*/}}
{{- define "gpustack-operator.deviceManager.labels" -}}
{{ include "gpustack-operator.commonLabels" . }}
app.kubernetes.io/component: device-manager
app.kubernetes.io/name: {{ include "gpustack-operator.deviceManager.fullname" . }}
{{- end -}}

{{/*
Device-manager selector labels. Kept minimal and stable across upgrades.
*/}}
{{- define "gpustack-operator.deviceManager.selectorLabels" -}}
app.kubernetes.io/part-of: gpustack-operator
app.kubernetes.io/component: device-manager
app.kubernetes.io/name: {{ include "gpustack-operator.deviceManager.fullname" . }}
{{- end -}}

{{/*
One manufacturer's identity as the environment variables pkg/nodefeature reads it from, so a
row of global.manufacturers decides those facts instead of restating them. Only the fields the
row states are emitted: an unstated one leaves the operator's own default in force.
Takes a dict of "name" (the manufacturer) and "identity" (its row).
*/}}
{{- define "gpustack-operator.manufacturer.env" -}}
{{- $prefix := printf "GPUSTACK_%s" (upper .name) -}}
{{- with .identity.pciVendorID }}
- name: {{ $prefix }}_PCI_VENDOR_ID
  value: {{ . | quote }}
{{- end }}
{{- with .identity.resourceName }}
- name: {{ $prefix }}_ACCELERATABLE_RESOURCE_NAME
  value: {{ . | quote }}
{{- end }}
{{- with .identity.runtimeName }}
- name: {{ $prefix }}_ACCELERATABLE_RUNTIME_NAME
  value: {{ . | quote }}
{{- end }}
{{- with .identity.partitionKind }}
- name: {{ $prefix }}_PARTITION_KIND
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Resolve whether the worker consumes a cert-manager-issued serving certificate.
Returns "true" when enabled and "" (empty) otherwise.
.Values.worker.certmanager.enabled is a string:
  - "true"  always enables it (cert-manager must be installed);
  - "false" disables it (the worker self-manages its certificate);
  - "auto"  (default) enables it only when the cert-manager CRD is present,
            mirroring the operator bootstrap apply.sh CRD probe via Helm lookup.
*/}}
{{- define "gpustack-operator.worker.certmanager.enabled" -}}
{{- $mode := .Values.worker.certmanager.enabled | toString -}}
{{- if eq $mode "true" -}}
true
{{- else if eq $mode "false" -}}
{{- else if lookup "apiextensions.k8s.io/v1" "CustomResourceDefinition" "" "certificates.cert-manager.io" -}}
true
{{- end -}}
{{- end -}}
