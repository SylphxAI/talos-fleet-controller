{{/*
Expand the name of the chart.
*/}}
{{- define "talos-fleet-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Truncated at 63 chars because Kubernetes name fields are limited to this.
*/}}
{{- define "talos-fleet-controller.fullname" -}}
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
{{- define "talos-fleet-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "talos-fleet-controller.labels" -}}
helm.sh/chart: {{ include "talos-fleet-controller.chart" . }}
{{ include "talos-fleet-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: talos-fleet-controller
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "talos-fleet-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "talos-fleet-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "talos-fleet-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- include "talos-fleet-controller.fullname" . }}
{{- else }}
{{- default "default" .Values.fullnameOverride }}
{{- end }}
{{- end }}

{{/*
Namespace — always use the configured namespace, regardless of release namespace.
*/}}
{{- define "talos-fleet-controller.namespace" -}}
{{- .Values.namespace.name }}
{{- end }}

{{/*
Controller image.
*/}}
{{- define "talos-fleet-controller.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Talos ServiceAccount name.
*/}}
{{- define "talos-fleet-controller.talosServiceAccountName" -}}
{{- .Values.talosServiceAccount.name }}
{{- end }}

{{/*
Webhook Service name.
*/}}
{{- define "talos-fleet-controller.webhookServiceName" -}}
{{- printf "%s-webhook" (include "talos-fleet-controller.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Webhook serving-certificate Secret name.
*/}}
{{- define "talos-fleet-controller.webhookCertSecretName" -}}
{{- default (printf "%s-cert" (include "talos-fleet-controller.fullname" .)) .Values.webhook.certSecretName }}
{{- end }}

{{/*
ExtensionConfig name.
*/}}
{{- define "talos-fleet-controller.extensionConfigName" -}}
{{- default (include "talos-fleet-controller.fullname" .) .Values.extensionConfig.name }}
{{- end }}
