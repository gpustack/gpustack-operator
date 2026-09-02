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
Tag defaults to "v<.Chart.AppVersion>" when unset, with any "v" the appVersion already carries
trimmed first: whoever packages this chart decides that spelling, and a doubled "v" names an
image tag no registry serves.
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
{{- /*
    `global.hub` is accepted as an alias so an umbrella chart carrying Istio-derived
    subcharts — which read `hub` and will keep doing so upstream — can mirror every
    image with one value instead of two that must be kept equal. `imageRegistry` wins
    when both are set, so a release already setting it is unaffected.
  */ -}}
{{- with (coalesce $root.Values.global.imageRegistry $root.Values.global.hub) -}}
{{- $registry = trimSuffix "/" . -}}
{{- end -}}
{{- with $registry -}}
{{- $repository = printf "%s/%s" . $repository -}}
{{- end -}}
{{- $tag := default (printf "v%s" (trimPrefix "v" $root.Chart.AppVersion)) $image.tag -}}
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
Kueue's "resources.transformations": every accelerator resource key mapped onto its
manufacturer's single credits resource, so a ClusterQueue can advertise one credit budget
instead of one per allocation mode. Derived from global.manufacturers rather than stated, so a
row added there is admissible with no second edit — and rendered here, in the parent, because
a subchart's values are merged rather than templated (a patch has Kueue's manager-config
include this).

The credit basis is pkg/nodefeature's: one whole card is CreditsPerCard credits, a shared
ownership CreditsPerCard/SharedResourceMaxSize, and one fine-grained unit — sliced or
partitioned — CreditsPerCard/ResourceMaxUnits. Every per-mode value stays an integer, so
Kueue's int64 quantization (which ceils a non-CPU resource) never rounds a fractional credit up
to 1. The three constants are restated here; a Go test renders this chart and holds the result
equal to what pkg/nodefeature computes.
*/}}
{{- define "gpustack-operator.kueue.transformations" -}}
{{- $creditsPerCard := 1600000 -}}
{{- $sharedMaxSize := 10 -}}
{{- $resourceMaxUnits := 1600000 -}}
{{- range $manu := (keys .Values.global.manufacturers | sortAlpha) }}
{{- $row := index $.Values.global.manufacturers $manu }}
{{- $credits := printf "credits.gpustack.ai/%s" $manu }}
- input: {{ $row.resourceName }}
  strategy: Replace
  outputs:
    {{ $credits }}: {{ $creditsPerCard | quote }}
- input: {{ $row.resourceName }}.shared
  strategy: Replace
  outputs:
    {{ $credits }}: {{ div $creditsPerCard $sharedMaxSize | quote }}
- input: {{ $row.resourceName }}.sliced.units
  strategy: Replace
  multiplyBy: {{ $row.resourceName }}.sliced
  outputs:
    {{ $credits }}: {{ div $creditsPerCard $resourceMaxUnits | quote }}
{{- if $row.partitionKind }}
{{- /*
  A partition request is confined to a single card, so its units already carry the whole charge
  and are multiplied by no counting resource — unlike the logical/sliced family, whose units are
  spread across however many cards the ".sliced" token grants.
*/}}
- input: {{ $row.resourceName }}.partitioned.units
  strategy: Replace
  outputs:
    {{ $credits }}: {{ div $creditsPerCard $resourceMaxUnits | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
The namespaces Kueue manages jobs in, defaulted to every namespace but kube-system and this
release's own. Kueue refuses to start when this selector matches the namespace it runs in, and
a subchart value cannot be templated, so rendering the default here is what lets the release
install anywhere. A "managedJobsNamespaceSelector" stated in values takes precedence.
*/}}
{{- define "gpustack-operator.kueue.managedJobsNamespaceSelector" -}}
matchExpressions:
  - key: kubernetes.io/metadata.name
    operator: NotIn
    values: [ kube-system, {{ .Release.Namespace }} ]
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
Resolve whether this release's serving certificates are issued by cert-manager. One answer for
every component that needs one — the worker and the vendored Kueue alike, whose templates a patch
has call this through a helper of their own. Returns "true" when enabled and "" (empty) otherwise,
so that both `if` and `if not` read it correctly.
.Values.global.certmanager.enabled is a string:
  - "true"  always enables it (cert-manager must be installed);
  - "false" disables it (each component self-manages its certificate);
  - "auto"  (default) enables it only when the cert-manager CRD is present,
            mirroring the operator bootstrap apply.sh CRD probe via Helm lookup.
*/}}
{{- define "gpustack-operator.certmanager.enabled" -}}
{{- $mode := .Values.global.certmanager.enabled | toString -}}
{{- if eq $mode "true" -}}
true
{{- else if eq $mode "false" -}}
{{- else if lookup "apiextensions.k8s.io/v1" "CustomResourceDefinition" "" "certificates.cert-manager.io" -}}
true
{{- end -}}
{{- end -}}
