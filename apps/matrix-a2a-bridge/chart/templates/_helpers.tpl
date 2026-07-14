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
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
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

{{/*
Render an externally configurable byte count without passing Helm's floating-point notation to
the bridge. Requiring a decimal string also makes a malformed value fail at render time, before a
Deployment can enter CrashLoopBackOff.
*/}}
{{- define "matrix-a2a-bridge.nonNegativeDecimal" -}}
{{- $name := index . 0 -}}
{{- $value := printf "%v" (index . 1) -}}
{{- if not (regexMatch "^[0-9]+$" $value) -}}
{{- fail (printf "%s must be a non-negative decimal string" $name) -}}
{{- end -}}
{{- $value -}}
{{- end -}}
