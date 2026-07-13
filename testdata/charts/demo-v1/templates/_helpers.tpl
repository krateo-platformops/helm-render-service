{{- define "demo.fullname" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end }}
