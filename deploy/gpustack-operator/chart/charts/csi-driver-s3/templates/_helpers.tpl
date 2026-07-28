{{/* vim: set filetype=mustache: */}}

{{/* Expand the name of the chart.*/}}
{{- define "s3.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* labels for helm resources */}}
{{- define "s3.labels" -}}
labels:
  app.kubernetes.io/instance: "{{ .Release.Name }}"
  app.kubernetes.io/managed-by: "{{ .Release.Service }}"
  app.kubernetes.io/name: "{{ template "s3.name" . }}"
  app.kubernetes.io/version: "{{ .Chart.AppVersion }}"
  helm.sh/chart: "{{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}"
  {{- if .Values.customLabels }}
{{ toYaml .Values.customLabels | indent 2 -}}
  {{- end }}
{{- end -}}

{{/* Image reference "{registry/}{namespace/}repository:tag" resolved against the parent
     chart's global image overrides: a non-empty global.imageNamespace replaces the namespace
     segment of the reference, and a non-empty global.imageRegistry replaces its registry
     segment, which is the first path segment carrying a "." or a ":". Each falls back to what
     the reference already encodes. The tag rides along in the last path segment.
     Pass a dict: (dict "root" $ "reference" .Values.images.s3). */}}
{{- define "s3.image" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $repository := .reference -}}
{{- $registry := "" -}}
{{- $segments := splitList "/" $repository -}}
{{- if and (gt (len $segments) 1) (or (contains "." (first $segments)) (contains ":" (first $segments))) -}}
{{- $registry = first $segments -}}
{{- $repository = join "/" (rest $segments) -}}
{{- end -}}
{{- with $global.imageNamespace -}}
{{- $repository = printf "%s/%s" . (last (splitList "/" $repository)) -}}
{{- end -}}
{{- with $global.imageRegistry -}}
{{- $registry = trimSuffix "/" . -}}
{{- end -}}
{{- with $registry -}}
{{- $repository = printf "%s/%s" . $repository -}}
{{- end -}}
{{- $repository -}}
{{- end -}}

{{/* Image pull policy, replaced by the parent chart's global.imagePullPolicy when non-empty.
     Pass a dict: (dict "root" $ "pullPolicy" .Values.imagePullPolicy). */}}
{{- define "s3.imagePullPolicy" -}}
{{- $global := .root.Values.global | default dict -}}
{{- default .pullPolicy $global.imagePullPolicy -}}
{{- end -}}

{{/* Image pull secrets as YAML, falling back to the parent chart's global.imagePullSecrets
     when the chart's own value is empty, and to nothing when both are.
     Pass a dict: (dict "root" $ "secrets" .Values.imagePullSecrets). */}}
{{- define "s3.imagePullSecrets" -}}
{{- $global := .root.Values.global | default dict -}}
{{- with (default $global.imagePullSecrets .secrets) -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}
