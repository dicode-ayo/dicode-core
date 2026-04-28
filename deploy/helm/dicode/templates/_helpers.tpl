{{/*
Expand the name of the chart.
*/}}
{{- define "dicode.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.

We truncate at 63 chars because some Kubernetes name fields are limited
to that (DNS-1123). If release name contains chart name, it's used as-is.
*/}}
{{- define "dicode.fullname" -}}
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
{{- define "dicode.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "dicode.labels" -}}
helm.sh/chart: {{ include "dicode.chart" . }}
{{ include "dicode.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: dicode
{{- end }}

{{/*
Selector labels — used in Service.spec.selector and
Deployment.spec.selector. MUST stay stable across upgrades because
selectors are immutable on Deployments.
*/}}
{{- define "dicode.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dicode.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
*/}}
{{- define "dicode.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dicode.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Fully qualified image reference (repo:tag).
Falls back to .Chart.AppVersion when image.tag is empty so the chart
and the image released by release-please stay in lockstep.
*/}}
{{- define "dicode.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Name of the chart-managed Secret. Returns empty string when the user
has neither created an internal secret nor pointed at an existing one.
Templates use this with `if` to decide whether to add envFrom entries.
*/}}
{{- define "dicode.secretName" -}}
{{- if .Values.secret.existingSecret -}}
{{- .Values.secret.existingSecret -}}
{{- else if .Values.secret.create -}}
{{- include "dicode.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
Name of the PVC to mount at /data. Returns empty string when
persistence is disabled (the deployment falls back to emptyDir).
*/}}
{{- define "dicode.dataClaimName" -}}
{{- if not .Values.persistence.enabled -}}
{{- else if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "dicode.fullname" .) -}}
{{- end -}}
{{- end }}
