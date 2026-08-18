{{- define "peevee.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "peevee.fullname" -}}
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

{{- define "peevee.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "peevee.labels" -}}
helm.sh/chart: {{ include "peevee.chart" . }}
{{ include "peevee.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: peevee
{{- end -}}

{{- define "peevee.selectorLabels" -}}
app.kubernetes.io/name: {{ include "peevee.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "peevee.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "peevee.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Secret holding kubeconfigs: either one the operator created, or one
     this chart renders from inline values. */}}
{{- define "peevee.kubeconfigSecretName" -}}
{{- if .Values.kubeconfigs.existingSecret -}}
{{- .Values.kubeconfigs.existingSecret -}}
{{- else -}}
{{- printf "%s-kubeconfigs" (include "peevee.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "peevee.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "peevee.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "peevee.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
