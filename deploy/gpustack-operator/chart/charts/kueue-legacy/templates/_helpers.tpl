{{- /*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/ -}}

{{/*
Expand the name of the chart.
*/}}
{{- define "kueue.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "kueue.fullname" -}}
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
{{- define "kueue.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kueue.labels" -}}
helm.sh/chart: {{ include "kueue.chart" . }}
{{ include "kueue.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kueue.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kueue.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Labels for metrics service
*/}}
{{- define "kueue.metricsService.labels" -}}
{{ include "kueue.labels" . }}
app.kubernetes.io/component: metrics-service
{{- end }}

{{/*
Labels for webhook service
*/}}
{{- define "kueue.webhookService.labels" -}}
{{ include "kueue.labels" . }}
app.kubernetes.io/component: webhook-service
{{- end }}

{{/*
Labels for visibility service
*/}}
{{- define "kueue.visibilityService.labels" -}}
{{ include "kueue.labels" . }}
app.kubernetes.io/component: visibility-service
{{- end }}

{{/*
Labels for controller-manager
*/}}
{{- define "kueue.controllerManager.labels" -}}
{{ include "kueue.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kueue.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kueue.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
FeatureGates
*/}}
{{- define "kueue.featureGates" -}}
{{- $features := "" }}
{{- range .Values.controllerManager.featureGates }}
{{- $str := printf "%s=%t," .name .enabled }}
{{- $features = print $features $str }}
{{- end }}
{{- with .Values.controllerManager.featureGates }}
- --feature-gates={{ $features | trimSuffix "," }}
{{- end }}
{{- end }}

{{/*
IsFeatureGateEnabled - outputs true if the feature gate .Feature is enabled in the .List
Usage:
  {{- if include "kueue.isFeatureGateEnabled" (dict "List" .Values.controllerManager.featureGates "Feature" "VisibilityOnDemand") }}
*/}}
{{- define "kueue.isFeatureGateEnabled" -}}
{{- $feature := .Feature }}
{{- $enabled := false }}
{{- range .List }}
{{- if (and (eq .name $feature) (eq .enabled true)) }}
{{- $enabled = true }}
{{- end }}
{{- end }}
{{- if $enabled }}
{{- $enabled -}}
{{- end }}
{{- end }}

{{/*
Cert-manager issuerRef for the chart-managed certificates.
*/}}
{{- define "kueue.certManager.issuerRef" -}}
{{- if .Values.certManager.issuerRef }}
{{- toYaml .Values.certManager.issuerRef }}
{{- else }}
kind: Issuer
name: '{{ include "kueue.fullname" . }}-selfsigned-issuer'
{{- end }}
{{- end }}

{{/*
Image reference "{registry/}{namespace/}repository:tag" resolved against the parent chart's
global image overrides: a non-empty global.imageNamespace replaces the namespace segment of
`image.repository`, and a non-empty global.imageRegistry replaces its registry segment, which
is the first path segment carrying a "." or a ":". Each falls back to what `image.repository`
already encodes.
Pass a dict: (dict "root" $ "image" .Values.controllerManager.manager.image).
*/}}
{{- define "kueue.image" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $repository := .image.repository -}}
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
{{- printf "%s:%s" $repository (default .root.Chart.AppVersion .image.tag) -}}
{{- end -}}

{{/*
Image pull policy, replaced by the parent chart's global.imagePullPolicy when non-empty.
Pass a dict: (dict "root" $ "pullPolicy" .Values.controllerManager.manager.image.pullPolicy).
*/}}
{{- define "kueue.imagePullPolicy" -}}
{{- $global := .root.Values.global | default dict -}}
{{- default .pullPolicy $global.imagePullPolicy -}}
{{- end -}}

{{/*
Image pull secrets as YAML, falling back to the parent chart's global.imagePullSecrets when
the chart's own value is empty, and to nothing when both are.
Pass a dict: (dict "root" $ "secrets" .Values.controllerManager.imagePullSecrets).
*/}}
{{- define "kueue.imagePullSecrets" -}}
{{- $global := .root.Values.global | default dict -}}
{{- with (default $global.imagePullSecrets .secrets) -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}
