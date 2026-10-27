package main

import (
	"bytes"
	"html/template"
	"net/http"
	"time"
)

var (
	indexTmpl  = template.Must(template.New("index").Funcs(templateFuncs()).Parse(baseTemplate + indexContent))
	detailTmpl = template.Must(template.New("detail").Funcs(templateFuncs()).Parse(baseTemplate + detailContent))
)

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

const baseTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root {
  --paper: #f2eee5;
  --paper-deep: #e8e1d3;
  --card: #fffdf8;
  --ink: #1e2527;
  --muted: #70766f;
  --line: #dcd4c4;
  --accent: #ad431f;
  --accent-deep: #8e3317;
  --accent-soft: #f5dfd4;
  --teal: #226464;
  --teal-soft: #dfeceb;
  --shadow: 0 14px 34px rgba(60, 45, 20, .08);
  --radius: 16px;
}
* { box-sizing: border-box; }
html { scroll-behavior: smooth; }
body {
  margin: 0;
  background: var(--paper);
  color: var(--ink);
  font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
  line-height: 1.5;
}
a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }
button, input { font: inherit; }
button { cursor: pointer; }
.shell { width: min(1120px, calc(100% - 36px)); margin: 0 auto; }
.top {
  display: flex; align-items: flex-end; justify-content: space-between;
  gap: 24px; flex-wrap: wrap; padding: 30px 0 18px;
  border-bottom: 1px solid var(--line);
}
.brand { font-size: 1.65rem; font-weight: 800; letter-spacing: -.045em; color: var(--ink); }
.brand span { color: var(--accent); }
.tagline { color: var(--muted); font-size: .88rem; margin-top: 2px; }
.top-note { color: var(--muted); font-size: .76rem; text-transform: uppercase; letter-spacing: .12em; }
main { padding: 28px 0 70px; }
h1, h2, h3 { letter-spacing: -.025em; line-height: 1.15; margin: 0; }
h1 { font-size: clamp(2rem, 5vw, 3.4rem); max-width: 720px; }
h2 { font-size: 1.15rem; }
p { margin: 0; }
.hero { display: flex; justify-content: space-between; align-items: flex-end; gap: 24px; flex-wrap: wrap; margin-bottom: 26px; }
.hero p { color: var(--muted); max-width: 620px; margin-top: 10px; }
.stats { display: flex; gap: 8px; flex-wrap: wrap; }
.stat {
  background: var(--card); border: 1px solid var(--line); border-radius: 999px;
  padding: 7px 13px; font-size: .78rem; color: var(--muted);
}
.stat strong { color: var(--ink); font-variant-numeric: tabular-nums; }
.flash { border-radius: 12px; padding: 11px 14px; margin-bottom: 16px; font-size: .9rem; border: 1px solid; }
.flash.success { background: var(--teal-soft); border-color: #b9d8d5; color: #174b4b; }
.flash.error { background: #fbe4de; border-color: #edc0b4; color: #7d2a12; }
.add {
  background: var(--card); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 20px; box-shadow: var(--shadow); margin-bottom: 26px;
}
.add-head { display: flex; align-items: baseline; justify-content: space-between; gap: 16px; flex-wrap: wrap; margin-bottom: 14px; }
.add-head span { color: var(--muted); font-size: .8rem; }
.form-grid { display: grid; grid-template-columns: 2fr 1.4fr 1fr; gap: 10px 12px; align-items: end; }
.field { display: flex; flex-direction: column; gap: 5px; }
.field.wide { grid-column: span 2; }
.field span { color: var(--muted); font-size: .72rem; text-transform: uppercase; letter-spacing: .09em; }
.field input {
  width: 100%; border: 1px solid var(--line); border-radius: 10px;
  background: #fff; padding: 10px 12px; color: var(--ink); outline: none;
}
.field input:focus { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.primary, .secondary, .danger {
  border: 0; border-radius: 10px; padding: 10px 15px; font-weight: 700; font-size: .85rem;
}
.primary { background: var(--accent); color: #fff; }
.primary:hover { background: var(--accent-deep); text-decoration: none; }
.secondary { background: var(--paper-deep); color: var(--ink); }
.danger { background: transparent; color: #9b2f1a; border: 1px solid #e2b5a8; padding: 7px 11px; }
.danger:hover { background: #fbe4de; }
.toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 22px; }
.search { display: flex; flex: 1 1 340px; gap: 8px; }
.search input {
  flex: 1; border: 1px solid var(--line); border-radius: 999px;
  background: var(--card); padding: 11px 16px; outline: none;
}
.search input:focus { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.search button { border: 1px solid var(--line); background: var(--card); border-radius: 999px; padding: 0 18px; font-weight: 650; }
.clear { font-size: .82rem; color: var(--muted); }
.columns { display: grid; grid-template-columns: 190px minmax(0, 1fr); gap: 26px; align-items: start; }
.tags-panel { position: sticky; top: 18px; }
.tags-panel h2 { font-size: .74rem; text-transform: uppercase; letter-spacing: .12em; color: var(--muted); margin-bottom: 10px; }
.tag-list { display: flex; flex-wrap: wrap; gap: 7px; }
.tag-item {
  display: inline-flex; align-items: center; gap: 6px; border: 1px solid var(--line);
  background: var(--card); border-radius: 999px; padding: 4px 9px; font-size: .76rem; color: var(--ink);
}
.tag-item span { color: var(--muted); font-variant-numeric: tabular-nums; }
.tag-item:hover { border-color: var(--accent); text-decoration: none; }
.links { display: grid; gap: 12px; }
.card {
  background: var(--card); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 17px 18px; box-shadow: 0 4px 14px rgba(60, 45, 20, .035);
}
.card:hover { border-color: #cabfa9; }
.card-top { display: flex; align-items: baseline; gap: 10px; justify-content: space-between; flex-wrap: wrap; }
.card-title { font-size: 1.08rem; font-weight: 750; letter-spacing: -.02em; color: var(--ink); }
.card-title:hover { color: var(--accent); text-decoration: none; }
.host { color: var(--muted); font-size: .74rem; }
.card p { color: var(--muted); font-size: .9rem; margin-top: 6px; }
.chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
.chip {
  display: inline-block; background: var(--teal-soft); color: var(--teal);
  border-radius: 999px; padding: 2px 8px; font-size: .72rem; font-weight: 650;
}
.chip:hover { text-decoration: none; background: #cfe4e2; }
.meta { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; margin-top: 13px; padding-top: 11px; border-top: 1px dashed var(--line); color: var(--muted); font-size: .76rem; }
.meta strong { color: var(--ink); font-variant-numeric: tabular-nums; }
.meta form { margin-left: auto; }
.visit { font-weight: 700; }
.empty { background: var(--card); border: 1px dashed var(--line); border-radius: var(--radius); padding: 34px; text-align: center; color: var(--muted); }
.pagination { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 18px; color: var(--muted); font-size: .82rem; }
.pagination a { background: var(--card); border: 1px solid var(--line); border-radius: 9px; padding: 7px 12px; }
.pagination a:hover { text-decoration: none; border-color: var(--accent); }
.back { display: inline-block; margin-bottom: 18px; color: var(--muted); font-size: .85rem; }
.detail {
  background: var(--card); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 26px; box-shadow: var(--shadow);
}
.detail-head { display: flex; justify-content: space-between; gap: 24px; flex-wrap: wrap; }
.eyebrow { color: var(--muted); font-size: .74rem; text-transform: uppercase; letter-spacing: .11em; }
.detail h1 { font-size: clamp(1.8rem, 4vw, 2.8rem); margin-top: 4px; }
.detail .url { color: var(--muted); font-size: .86rem; margin-top: 8px; word-break: break-all; }
.detail-stats { display: flex; gap: 22px; align-items: flex-start; }
.detail-stats div { display: flex; flex-direction: column; }
.detail-stats strong { font-size: 1.15rem; font-variant-numeric: tabular-nums; }
.detail-stats span { color: var(--muted); font-size: .72rem; text-transform: uppercase; letter-spacing: .08em; }
.description { color: var(--muted); margin-top: 16px; max-width: 760px; }
.detail .chips { margin-top: 16px; }
.actions { display: flex; align-items: center; gap: 10px; margin-top: 20px; flex-wrap: wrap; }
.panel { background: var(--card); border: 1px solid var(--line); border-radius: var(--radius); padding: 20px; margin-top: 18px; }
.panel h2 { margin-bottom: 14px; }
.bars { display: flex; align-items: flex-end; gap: 5px; height: 110px; padding-top: 8px; }
.bar { flex: 1; min-width: 8px; height: 100%; display: flex; flex-direction: column; justify-content: flex-end; align-items: center; gap: 5px; }
.bar span { display: block; width: 100%; min-height: 2px; background: var(--accent); border-radius: 4px 4px 0 0; }
.bar b { font-size: .66rem; font-weight: 700; color: var(--ink); }
.bar small { color: var(--muted); font-size: .63rem; white-space: nowrap; }
table { width: 100%; border-collapse: collapse; font-size: .85rem; }
th, td { text-align: left; padding: 8px 0; border-bottom: 1px solid var(--line); }
th { color: var(--muted); font-size: .7rem; text-transform: uppercase; letter-spacing: .08em; font-weight: 650; }
tr:last-child td { border-bottom: 0; }
@media (max-width: 780px) {
  .columns { grid-template-columns: 1fr; }
  .tags-panel { position: static; }
  .form-grid { grid-template-columns: 1fr; }
  .field.wide { grid-column: auto; }
  .detail-stats { gap: 16px; }
}
</style>
</head>
<body>
<header class="top shell">
  <div>
    <a class="brand" href="/">Link<span>Shelf</span></a>
    <div class="tagline">A small, searchable shelf for the links worth keeping.</div>
  </div>
  <div class="top-note">wt + wasi:http + sqlite</div>
</header>
<main class="shell">
{{template "content" .}}
</main>
</body>
</html>`

const indexContent = `{{define "content"}}
<section class="hero">
  <div>
    <h1>Keep the good links.</h1>
    <p>Save a URL, give it a few tags, and let the shelf remember what you found. Click tracking and a small analytics view are built in.</p>
  </div>
  <div class="stats">
    <div class="stat"><strong>{{.Stats.Links}}</strong> links</div>
    <div class="stat"><strong>{{.Stats.Clicks}}</strong> clicks</div>
    <div class="stat"><strong>{{.Stats.Tags}}</strong> tags</div>
  </div>
</section>
{{if .Error}}<div class="flash error">{{.Error}}</div>{{end}}
{{if .Added}}<div class="flash success">Link added to the shelf.</div>{{end}}
{{if .Deleted}}<div class="flash success">Link removed.</div>{{end}}

<form class="add" method="post" action="/links">
  <div class="add-head">
    <h2>Add a link</h2>
    <span>Tags are comma-separated; title falls back to the host.</span>
  </div>
  <div class="form-grid">
    <label class="field wide"><span>URL</span><input name="url" type="url" placeholder="https://example.com" required></label>
    <label class="field"><span>Title</span><input name="title" placeholder="Optional"></label>
    <label class="field"><span>Tags</span><input name="tags" placeholder="wasm, reading, tools"></label>
    <label class="field wide"><span>Description</span><input name="description" placeholder="Why is it worth keeping?"></label>
    <button class="primary" type="submit">Add to shelf</button>
  </div>
</form>

<div class="toolbar">
  <form class="search" method="get" action="/">
    <input type="search" name="q" value="{{.Q}}" placeholder="Search title, URL, or description…">
    {{if .Tag}}<input type="hidden" name="tag" value="{{.Tag}}">{{end}}
    <button type="submit">Search</button>
  </form>
  {{if .Tag}}<a class="clear" href="{{.ClearURL}}">tag: #{{.Tag}} ×</a>{{end}}
</div>

<div class="columns">
  <aside class="tags-panel">
    <h2>Tags</h2>
    {{if .Tags}}<div class="tag-list">
      {{range .Tags}}<a class="tag-item" href="{{.URL}}">#{{.Tag}} <span>{{.Count}}</span></a>{{end}}
    </div>{{else}}<p class="clear">No tags yet.</p>{{end}}
  </aside>

  <section class="links">
    {{if .Links}}
      {{range .Links}}
      <article class="card">
        <div class="card-top">
          <a class="card-title" href="/links/{{.ID}}">{{.Title}}</a>
          <span class="host">{{.Host}}</span>
        </div>
        {{if .Description}}<p>{{.Description}}</p>{{end}}
        {{if .Tags}}<div class="chips">{{range .Tags}}<a class="chip" href="{{call $.TagURL .}}">#{{.}}</a>{{end}}</div>{{end}}
        <div class="meta">
          <span><strong>{{.Clicks}}</strong> clicks</span>
          <span>last {{formatTime .LastClickedAt}}</span>
          <a class="visit" href="/go/{{.ID}}" target="_blank" rel="noopener">visit ↗</a>
          <form method="post" action="/links/{{.ID}}/delete" onsubmit="return confirm('Remove this link?')">
            <button class="danger" type="submit">delete</button>
          </form>
        </div>
      </article>
      {{end}}
    {{else}}
      <div class="empty">No links match this view. Add one above, or clear the filters.</div>
    {{end}}

    <div class="pagination">
      <span>{{.Total}} links · page {{.Page}} of {{.Pages}}</span>
      <span>
        {{if .PrevURL}}<a href="{{.PrevURL}}">← previous</a>{{end}}
        {{if .NextURL}}<a href="{{.NextURL}}">next →</a>{{end}}
      </span>
    </div>
  </section>
</div>
{{end}}`

const detailContent = `{{define "content"}}
<a class="back" href="/">← all links</a>
<section class="detail">
  <div class="detail-head">
    <div>
      <p class="eyebrow">{{.Link.Host}}</p>
      <h1>{{.Link.Title}}</h1>
      <p class="url"><a href="{{.Link.URL}}" target="_blank" rel="noopener">{{.Link.URL}}</a></p>
    </div>
    <div class="detail-stats">
      <div><strong>{{.Link.Clicks}}</strong><span>clicks</span></div>
      <div><strong>{{formatTime .Link.LastClickedAt}}</strong><span>last click</span></div>
    </div>
  </div>
  {{if .Link.Description}}<p class="description">{{.Link.Description}}</p>{{end}}
  {{if .Link.Tags}}<div class="chips">{{range .Link.Tags}}<a class="chip" href="{{call $.TagURL .}}">#{{.}}</a>{{end}}</div>{{end}}
  <div class="actions">
    <a class="primary" href="/go/{{.Link.ID}}" target="_blank" rel="noopener">Visit link ↗</a>
    <form method="post" action="/links/{{.Link.ID}}/delete" onsubmit="return confirm('Remove this link?')">
      <button class="danger" type="submit">Delete</button>
    </form>
  </div>
</section>

<section class="panel">
  <h2>Last 14 days</h2>
  {{if .Days}}
  <div class="bars">
    {{range .Days}}<div class="bar"><b>{{.Count}}</b><span style="height:{{.Percent}}%"></span><small>{{shortDate .Day}}</small></div>{{end}}
  {{else}}<p class="clear">No clicks recorded yet.</p>{{end}}
</section>

<section class="panel">
  <h2>Recent clicks</h2>
  {{if .Clicks}}
  <table>
    <thead><tr><th>When</th></tr></thead>
    <tbody>{{range .Clicks}}<tr><td>{{formatTime .At}}</td></tr>{{end}}</tbody>
  </table>
  {{else}}<p class="clear">No clicks recorded yet.</p>{{end}}
</section>
{{end}}`
