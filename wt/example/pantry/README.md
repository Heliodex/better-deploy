# Pantry

A small shared pantry inventory built with **Go and HTMX**. It is the counterpoint to the SvelteKit examples: there is no separate frontend build, no JSON API and no client-side state. Each interaction posts a normal HTML form and HTMX swaps the returned fragment into `#pantry`.

## What it demonstrates

- Server-rendered `html/template` pages embedded in the Go component.
- HTMX live search and filtering (`hx-get`, `hx-trigger`, `hx-target`, `hx-swap`).
- Inline quantity steppers and deletes, including `hx-confirm`.
- Form validation and an `X-Pantry-Error` response header so the add form resets only after a successful save.
- A real SQLite schema with a migration, search, low-stock filtering and grouped shelves.
- The same code runs on wasm or native. The only transport difference is in `packages/wasihttp`.

## Routes

| Route | Purpose |
| --- | --- |
| `GET /` | Full page |
| `GET /items` | Pantry fragment for search and filter requests |
| `POST /items` | Add an item |
| `POST /items/{id}/adjust` | Increment or decrement one item |
| `POST /items/{id}/delete` | Remove an item |
| `GET /healthz` | Supervisor health probe, supplied by `app.ServeStorage` |

## Run it

```sh
# local native dev session
wt dev --dir wt/example/pantry --runtime native

# or build the wasm bundle
wt bundle --dir wt/example/pantry
```

`static/` is checked in rather than generated: `htmx.min.js`, `app.css` and the favicon are source files, so there is no Bun or Vite step. The component build is `componentize-go`; the native build is `CGO_ENABLED=0 go build -tags wt_native`.

## Notes

- HTMX 2.0.4 is vendored with its Zero-Clause BSD licence in `static/htmx.LICENSE`.
- The table is intentionally small (`maxItems = 500`); search and filtering happen in SQLite.
- The app is unauthenticated like the other examples. It is a demonstration of the rendering model, not a multi-user authorisation reference.
