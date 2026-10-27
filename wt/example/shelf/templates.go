package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

var (
	indexTmpl  = parseTemplate("index", "templates/base.html", "templates/index.html")
	detailTmpl = parseTemplate("detail", "templates/base.html", "templates/detail.html")
)

func parseTemplate(name string, files ...string) *template.Template {
	var content strings.Builder
	for _, file := range files {
		data, err := templateFiles.ReadFile(file)
		if err != nil {
			panic(err)
		}
		content.Write(data)
	}
	return template.Must(template.New(name).Funcs(templateFuncs()).Parse(content.String()))
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatTime": func(value string) string {
			if value == "" {
				return "never"
			}
			parsed, err := time.Parse(timeFormat, value)
			if err != nil {
				return value
			}
			return parsed.Format("2 Jan 2006, 15:04")
		},
		"shortDate": func(value string) string {
			parsed, err := time.Parse("2006-01-02", value)
			if err != nil {
				return value
			}
			return parsed.Format("2 Jan")
		},
	}
}

func render(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
