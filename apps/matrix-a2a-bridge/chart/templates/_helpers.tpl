{{/* Standard chart helpers */}}
{{- define "matrix-a2a-bridge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "matrix-a2a-bridge.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "matrix-a2a-bridge.labels" -}}
app.kubernetes.io/name: {{ include "matrix-a2a-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: fgentic
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "matrix-a2a-bridge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "matrix-a2a-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "matrix-a2a-bridge.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "matrix-a2a-bridge.fullname" . -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
