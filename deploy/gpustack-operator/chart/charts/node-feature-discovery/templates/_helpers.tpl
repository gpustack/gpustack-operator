{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "node-feature-discovery.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "node-feature-discovery.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "node-feature-discovery.namespace" -}}
  {{- if .Values.namespaceOverride -}}
    {{- .Values.namespaceOverride -}}
  {{- else -}}
    {{- .Release.Namespace -}}
  {{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "node-feature-discovery.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "node-feature-discovery.labels" -}}
helm.sh/chart: {{ include "node-feature-discovery.chart" . }}
{{ include "node-feature-discovery.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "node-feature-discovery.selectorLabels" -}}
app.kubernetes.io/name: {{ include "node-feature-discovery.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Create the name of the service account which the nfd master will use
*/}}
{{- define "node-feature-discovery.master.serviceAccountName" -}}
{{- if .Values.master.serviceAccount.create -}}
    {{ default (include "node-feature-discovery.fullname" .) .Values.master.serviceAccount.name }}
{{- else -}}
    {{ default "default" .Values.master.serviceAccount.name }}
{{- end -}}
{{- end -}}

{{/*
Create the name of the service account which the nfd worker will use
*/}}
{{- define "node-feature-discovery.worker.serviceAccountName" -}}
{{- if .Values.worker.serviceAccount.create -}}
    {{ default (printf "%s-worker" (include "node-feature-discovery.fullname" .)) .Values.worker.serviceAccount.name }}
{{- else -}}
    {{ default "default" .Values.worker.serviceAccount.name }}
{{- end -}}
{{- end -}}

{{/*
Create the name of the service account which topologyUpdater will use
*/}}
{{- define "node-feature-discovery.topologyUpdater.serviceAccountName" -}}
{{- if .Values.topologyUpdater.serviceAccount.create -}}
    {{ default (printf "%s-topology-updater" (include "node-feature-discovery.fullname" .)) .Values.topologyUpdater.serviceAccount.name }}
{{- else -}}
    {{ default "default" .Values.topologyUpdater.serviceAccount.name }}
{{- end -}}
{{- end -}}

{{/*
Create the name of the service account which nfd-gc will use
*/}}
{{- define "node-feature-discovery.gc.serviceAccountName" -}}
{{- if .Values.gc.serviceAccount.create -}}
    {{ default (printf "%s-gc" (include "node-feature-discovery.fullname" .)) .Values.gc.serviceAccount.name }}
{{- else -}}
    {{ default "default" .Values.gc.serviceAccount.name }}
{{- end -}}
{{- end -}}

{{/*
imagePullSecrets helper - uses local values or falls back to global values
*/}}
{{- define "node-feature-discovery.imagePullSecrets" -}}
{{- $imagePullSecrets := list -}}
{{- if .Values.imagePullSecrets -}}
  {{- range .Values.imagePullSecrets -}}
    {{- $imagePullSecrets = append $imagePullSecrets . -}}
  {{- end -}}
{{- else if and .Values.global .Values.global.imagePullSecrets -}}
  {{- range .Values.global.imagePullSecrets -}}
    {{- $imagePullSecrets = append $imagePullSecrets . -}}
  {{- end -}}
{{- end -}}
{{- if $imagePullSecrets -}}
{{- $imagePullSecrets | toJson }}
{{- end -}}
{{- end -}}

{{/*
Image reference "{registry/}{namespace/}repository:tag" resolved against the parent chart's
global image overrides: a non-empty global.imageNamespace replaces the namespace segment of
`image.repository`, and a non-empty global.imageRegistry replaces its registry segment, which
is the first path segment carrying a "." or a ":". Each falls back to what `image.repository`
already encodes.
*/}}
{{- define "node-feature-discovery.image" -}}
{{- $global := .Values.global | default dict -}}
{{- $repository := .Values.image.repository -}}
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
{{- printf "%s:%s" $repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/*
Image pull policy, replaced by the parent chart's global.imagePullPolicy when non-empty.
*/}}
{{- define "node-feature-discovery.imagePullPolicy" -}}
{{- $global := .Values.global | default dict -}}
{{- default .Values.image.pullPolicy $global.imagePullPolicy -}}
{{- end -}}
