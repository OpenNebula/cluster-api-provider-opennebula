{{- define "oneks-application-controller.name" -}}
oneks-application-controller
{{- end -}}

{{- define "oneks-application-controller.labels" -}}
app.kubernetes.io/name: {{ include "oneks-application-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
