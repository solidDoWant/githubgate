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

{{- /* The Secret with the token API's certificate: the admin's, or the one cert-manager makes. */}}
{{- define "githubgate.tlsSecret" -}}
{{- if .Values.tls.existingSecret }}{{ .Values.tls.existingSecret }}
{{- else if .Values.certManager.enabled }}{{ include "githubgate.fullname" . }}-tls
{{- else }}{{ fail "tls.existingSecret is required when certManager.enabled is false" }}
{{- end }}
{{- end }}

{{- /* The Secret with the webhook server's certificate. */}}
{{- define "githubgate.webhookSecret" -}}
{{- if .Values.webhook.existingSecret }}{{ .Values.webhook.existingSecret }}
{{- else if .Values.certManager.enabled }}{{ include "githubgate.fullname" . }}-webhook-tls
{{- else }}{{ fail "webhook.existingSecret is required when certManager.enabled is false and a webhook is on" }}
{{- end }}
{{- end }}

{{- /* The Secret with the proxy certificate. */}}
{{- define "githubgate.proxySecret" -}}
{{- if .Values.proxy.existingSecret }}{{ .Values.proxy.existingSecret }}
{{- else if .Values.certManager.enabled }}{{ include "githubgate.fullname" . }}-proxy-tls
{{- else }}{{ fail "proxy.existingSecret is required when certManager.enabled is false and proxy.enabled is true" }}
{{- end }}
{{- end }}

{{- /* Non-empty when cert-manager makes the webhook server's certificate, so cainjector fills the webhooks' caBundle. */}}
{{- define "githubgate.webhookCertManaged" -}}
{{- if and .Values.certManager.enabled (not .Values.webhook.existingSecret) }}true{{ end }}
{{- end }}

{{- /* Annotations and caBundle source of the webhook configurations. Without
cert-manager making the certificate, the API server needs the CA from
webhook.caBundle or from the admin's own cainjector annotation. */}}
{{- define "githubgate.webhookAnnotations" -}}
{{- $annotations := dict }}
{{- if include "githubgate.webhookCertManaged" . }}
{{- $_ := set $annotations "cert-manager.io/inject-ca-from" (printf "%s/%s-webhook" .Release.Namespace (include "githubgate.fullname" .)) }}
{{- else if not (or .Values.webhook.caBundle .Values.webhook.annotations) }}
{{- fail "webhook.caBundle or webhook.annotations (for example cert-manager.io/inject-ca-from) is required when the chart's cert-manager does not make the webhook certificate" }}
{{- end }}
{{- $annotations = merge (deepCopy (.Values.webhook.annotations | default dict)) $annotations }}
{{- toYaml $annotations }}
{{- end }}
