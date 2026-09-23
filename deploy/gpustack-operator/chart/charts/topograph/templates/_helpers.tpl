{{/*
Expand the name of the chart.
*/}}
{{- define "topograph.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "topograph.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "topograph.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Container image reference. The tag defaults to the chart appVersion when unset.
*/}}
{{- define "topograph.image" -}}
{{- $root := .root | default . -}}
{{- $image := .image | default $root.Values.image -}}
{{- $global := $root.Values.global | default dict -}}
{{- $repository := $image.repository -}}
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
{{- printf "%s:%s" $repository ($image.tag | default $root.Chart.AppVersion) -}}
{{- end }}

{{- define "topograph.imagePullPolicy" -}}
{{- $root := .root | default . -}}
{{- $global := $root.Values.global | default dict -}}
{{- default .pullPolicy $global.imagePullPolicy -}}
{{- end }}

{{- define "topograph.imagePullSecrets" -}}
{{- $root := .root | default . -}}
{{- $global := $root.Values.global | default dict -}}
{{- with (default $global.imagePullSecrets .secrets) -}}
{{- toYaml . -}}
{{- end -}}
{{- end }}

{{/*
Common labels
*/}}
{{- define "topograph.labels" -}}
helm.sh/chart: {{ include "topograph.chart" . }}
{{ include "topograph.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "topograph.selectorLabels" -}}
app.kubernetes.io/name: {{ include "topograph.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "topograph.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "topograph.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name must be set when serviceAccount.create=false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the RBAC resources.
*/}}
{{- define "topograph.rbacName" -}}
{{- include "topograph.fullname" . }}
{{- end }}

{{/*
Create topograph service URL
*/}}
{{- define "topograph.serviceName" -}}
{{- if eq .Chart.Name "topograph" }}
{{- include "topograph.fullname" . }}
{{- else }}
{{- /* Subcharts render with their own .Chart.Name, so use the parent chart name explicitly. */}}
{{- $name := "topograph" }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "topograph.url" -}}
{{ printf "http://%s.%s.svc.cluster.local:%.0f" (include "topograph.serviceName" .) .Release.Namespace .Values.service.port }}
{{- end }}
