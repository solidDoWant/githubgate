{{- define "githubgate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "githubgate.fullname" -}}
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

{{- define "githubgate.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "githubgate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "githubgate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "githubgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "githubgate.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "githubgate.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "githubgate.adminNamespaces" -}}
{{- if .Values.adminNamespaces }}{{ join "," .Values.adminNamespaces }}{{ else }}{{ .Release.Namespace }}{{ end }}
{{- end }}

{{- /* Non-empty when the webhook server runs: the validating or the injection webhook is on. */}}
{{- define "githubgate.webhookServer" -}}
{{- if or .Values.webhook.enabled .Values.inject.enabled }}true{{ end }}
{{- end }}
