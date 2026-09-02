{{/* vim: set filetype=mustache: */}}

{{/* Expand the name of the chart.*/}}
{{- define "nfs.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* labels for helm resources */}}
{{- define "nfs.labels" -}}
labels:
  app.kubernetes.io/instance: "{{ .Release.Name }}"
  app.kubernetes.io/managed-by: "{{ .Release.Service }}"
  app.kubernetes.io/name: "{{ template "nfs.name" . }}"
  app.kubernetes.io/version: "{{ .Chart.AppVersion }}"
  helm.sh/chart: "{{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}"
  {{- if .Values.customLabels }}
{{ toYaml .Values.customLabels | indent 2 -}}
  {{- end }}
{{- end -}}

{{/* Image reference "{registry/}{namespace/}repository:tag" resolved against the parent
     chart's global image overrides. A repository starting with "/" is a suffix of
     image.baseRepo, which is folded in first; then a non-empty global.imageNamespace replaces
     the namespace segment, and a non-empty global.imageRegistry replaces the registry segment,
     which is the first path segment carrying a "." or a ":". Each falls back to what the
     chart's own values already encode.
     Pass a dict: (dict "root" $ "image" .Values.image.nfs). */}}
{{- define "nfs.image" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $repository := .image.repository -}}
{{- if hasPrefix "/" $repository -}}
{{- $repository = printf "%s%s" .root.Values.image.baseRepo $repository -}}
{{- end -}}
{{- $registry := "" -}}
{{- $segments := splitList "/" $repository -}}
{{- if and (gt (len $segments) 1) (or (contains "." (first $segments)) (contains ":" (first $segments))) -}}
{{- $registry = first $segments -}}
{{- $repository = join "/" (rest $segments) -}}
{{- end -}}
{{- with $global.imageNamespace -}}
{{- $repository = printf "%s/%s" . (last (splitList "/" $repository)) -}}
{{- end -}}
{{- /*
    `global.hub` is accepted as an alias so an umbrella chart carrying Istio-derived
    subcharts — which read `hub` and will keep doing so upstream — can mirror every
    image with one value instead of two that must be kept equal. `imageRegistry` wins
    when both are set, so a release already setting it is unaffected.
  */ -}}
{{- with (coalesce $global.imageRegistry $global.hub) -}}
{{- $registry = trimSuffix "/" . -}}
{{- end -}}
{{- with $registry -}}
{{- $repository = printf "%s/%s" . $repository -}}
{{- end -}}
{{- printf "%s:%s" $repository .image.tag -}}
{{- end -}}

{{/* Image pull policy, replaced by the parent chart's global.imagePullPolicy when non-empty.
     Pass a dict: (dict "root" $ "pullPolicy" .Values.image.nfs.pullPolicy). */}}
{{- define "nfs.imagePullPolicy" -}}
{{- $global := .root.Values.global | default dict -}}
{{- default .pullPolicy $global.imagePullPolicy -}}
{{- end -}}

{{/* Image pull secrets as YAML, falling back to the parent chart's global.imagePullSecrets
     when the chart's own value is empty, and to nothing when both are.
     Pass a dict: (dict "root" $ "secrets" .Values.imagePullSecrets). */}}
{{- define "nfs.imagePullSecrets" -}}
{{- $global := .root.Values.global | default dict -}}
{{- with (default $global.imagePullSecrets .secrets) -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}
