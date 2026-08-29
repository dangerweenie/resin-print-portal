{{/* Common names + labels */}}
{{- define "resin-portal.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "resin-portal.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "resin-portal.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "resin-portal.labels" -}}
app.kubernetes.io/name: {{ include "resin-portal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "resin-portal.selectorLabels" -}}
app.kubernetes.io/name: {{ include "resin-portal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "resin-portal.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{- define "resin-portal.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "resin-portal.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Database DSN. Prefer the bundled Bitnami Postgres; otherwise externalDatabase.dsn.
*/}}
{{- define "resin-portal.databaseURL" -}}
{{- if .Values.postgresql.enabled -}}
{{- $host := printf "%s-postgresql" .Release.Name -}}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" .Values.postgresql.auth.username .Values.postgresql.auth.password $host .Values.postgresql.auth.database -}}
{{- else -}}
{{- required "externalDatabase.dsn is required when postgresql.enabled=false" .Values.externalDatabase.dsn -}}
{{- end -}}
{{- end -}}
