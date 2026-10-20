package main

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"strings"

	"github.com/inertia-go/inertia-go/vite"
)

// viteAssets implements inertia.ViteHelper for Vite's build manifest read from
// an embedded filesystem. The upstream vite package only loads manifests from
// the host filesystem, which a Spin WASM component cannot access.
type viteAssets struct {
	entries map[string]vite.Entry
}

func loadViteAssets(fsys fs.FS) *viteAssets {
	data, err := fs.ReadFile(fsys, ".vite/manifest.json")
	if err != nil {
		return nil
	}

	var entries map[string]vite.Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}

	return &viteAssets{entries: entries}
}

func (v *viteAssets) Tag(entry string) template.HTML {
	if v == nil {
		return ""
	}

	root, ok := v.entries[entry]
	if !ok || root.File == "" {
		return ""
	}

	css, preload := v.collect(entry)

	var b strings.Builder
	b.WriteString(`<script type="module" src="/`)
	b.WriteString(root.File)
	b.WriteString(`"></script>`)
	for _, href := range preload {
		b.WriteString("\n")
		b.WriteString(`<link rel="modulepreload" href="`)
		b.WriteString(href)
		b.WriteString(`" />`)
	}
	for _, href := range css {
		b.WriteString("\n")
		b.WriteString(`<link rel="stylesheet" href="`)
		b.WriteString(href)
		b.WriteString(`" />`)
	}

	return template.HTML(b.String())
}

func (v *viteAssets) Asset(entry string) string {
	if v == nil {
		return entry
	}
	if e, ok := v.entries[entry]; ok {
		return "/" + e.File
	}
	return "/" + entry
}

func (v *viteAssets) CSS(entry string) template.HTML {
	if v == nil {
		return ""
	}

	css, _ := v.collect(entry)

	var b strings.Builder
	for i, href := range css {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(`<link rel="stylesheet" href="`)
		b.WriteString(href)
		b.WriteString(`" />`)
	}

	return template.HTML(b.String())
}

func (v *viteAssets) ReactRefresh() template.HTML {
	return ""
}

// collect walks the entry's static import graph and returns stylesheet and
// modulepreload URLs in dependency-first order.
func (v *viteAssets) collect(entry string) (css, preload []string) {
	seen := make(map[string]bool)
	cssSeen := make(map[string]bool)
	preloadSeen := make(map[string]bool)

	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true

		e, ok := v.entries[name]
		if !ok {
			return
		}

		for _, imp := range e.Imports {
			walk(imp)
		}

		if name != entry {
			href := "/" + e.File
			if !preloadSeen[href] {
				preloadSeen[href] = true
				preload = append(preload, href)
			}
		}

		for _, file := range e.CSS {
			href := "/" + file
			if !cssSeen[href] {
				cssSeen[href] = true
				css = append(css, href)
			}
		}
	}

	walk(entry)
	return css, preload
}
