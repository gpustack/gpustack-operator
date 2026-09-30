{{- if .RawContent -}}
{{- partial "markdown-content.html" . -}}
{{- else -}}
# {{ partial "page-title.html" . }}
{{ range partial "ordered-pages.html" . }}
- [{{ partial "page-title.html" . }}]({{ with .OutputFormats.Get "Markdown" }}{{ .RelPermalink }}{{ end }})
{{ end }}
{{- end -}}
