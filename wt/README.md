# wt

A prototype of a small Wasm deploy platform built directly on
[wasmtime](https://wasmtime.dev), with no Spin and no container image.

The point is to move the expensive part (compiling wasm to native code) into
the deploy artifact:

```text
wt-deploy:  build (componentize-go) → wasmtime compile → upload
supervisor: Caddy-less Go reverse proxy + one `wasmtime serve` per app
runtime:    a runtime-only wasmtime 50 loads the precompiled .cwasm
storage:    a per-app SQLite SQL service on loopback, plus /data via WASI
```

The first request for a precompiled Go component is tens of milliseconds on a
laptop. Nothing is compiled on the serving node, and the app is a standard
`wasi:http/service@0.3.0` component.

## Layout

- `packages/` – the shared Go module for the app SDK.
  - `packages/storage` – client for the supervisor's SQLite service.
  - `packages/wasihttp` – the `net/http` adapter (adapted from spin-go-sdk).
  - `packages/binding` – generated bindings for `wt:app/app` (not tracked).
  - `packages/wit` – the `wt:app/app` world and WASI 0.3 WIT dependencies.
  - `packages/generate.sh` – regenerates `packages/binding`.
- `example/` – example apps only.
  - `example/hello` – returns the method and path.
  - `example/notes` – JSON notes persisted in a real `notes` table through
    the supervisor's SQLite service.
  - `example/shelf` – link shelf with tags, search, pagination and click
    analytics.
  - `example/ticker` – logs a `ticker beat` line every 10 seconds from an
    `app.json` schedule, so the supervisor log tail has a steady stream to show.
  - `example/guestbook` – SvelteKit static frontend with a Go JSON API,
    embedded into one wasm component.
- `supervisor/` – runs apps, proxies by `Host`, deploys bundles blue/green.
  - `supervisor/web` – SvelteKit admin panel, embedded into the supervisor
    binary (not tracked: `web/build`, `web/node_modules`, `web/.svelte-kit`).
- `deploy/` – builds, precompiles and uploads an app.

## Requirements

- Go 1.27.1 or newer. `componentize-go` is declared as a tool in both
  `packages/go.mod` (binding generation) and `example/go.mod` (app builds); it
  downloads the patched Go it needs for async components.
- The `example/guestbook` build also needs Bun. It runs `bun install` and
  `bun run build` in `example/guestbook/web`, then embeds the static output.
  `example/guestbook/contract/generate.go` turns the Valibot request schemas into JSON Schema and generated Go request types.
  `go generate ./contract/` runs it as part of the build; the generated schema and Go files are not tracked.
- wasmtime **50.0.0** on the machine running `wt-deploy` to compile the
  artifact. The precompiled artifact is tied to that version and target triple.
  The client and supervisor both default to `x86_64-unknown-linux-musl`, which
  is what the runtime-only image can execute; a different target needs a
  matching runtime build.
- The WIT in `packages/wit` is the final `wasi:http@0.3.0` from wasmtime v50
  (`crates/wasi-http/src/p3/wit`), plus `wasi:cli/imports`.

## Build the supervisor and CLI

```sh
cd wt/supervisor && go generate ./... && go build -o /tmp/wt-supervisor .
cd wt/deploy && go build -o /tmp/wt-deploy .
```

For the local loop, `wt/supervisor/dev.sh` rebuilds the admin panel and binary
and restarts the supervisor, reusing the running instance's app and state
directories so deployed apps, data and logs survive. `./dev.sh --skip-generate`
skips the panel build for a Go-only change.

## Run the supervisor

```sh
mkdir -p /tmp/wt/apps /tmp/wt/state
WT_APPS_DIR=/tmp/wt/apps \
WT_STATE_DIR=/tmp/wt/state \
WT_LISTEN=127.0.0.1:8080 \
WT_WASMTIME=/path/to/wasmtime \
WT_TOKEN=dev \
/tmp/wt-supervisor
```

The supervisor speaks plain HTTP by default; set `WT_TLS_CERT` and
`WT_TLS_KEY` (or `--tls-cert`/`--tls-key`) to terminate TLS itself. Behind the
Orchard ingress, leave TLS to the ingress and keep the supervisor on HTTP.
The proxy preserves the original `Host` and sets `X-Forwarded-Host` and
`X-Forwarded-Proto` for the app.

The supervisor scans `WT_APPS_DIR` at boot, starts every app found there, and
serves `GET /apps`, `GET /apps/<name>` and `POST /apps/<name>` (deploy, bearer
token). `/healthz` is the supervisor's own health check.

## Admin panel

The supervisor serves a small admin panel at `/admin/` on its own address
(for example `http://127.0.0.1:8080/admin/` locally, or the deploy host in
production). It lists apps with status and ticking uptime, streams each app's
log live over server-sent events, restarts apps, and uploads new bundles.
Updates stream over server-sent events, so the dashboard reflects deploys and
restarts without refreshing. Every endpoint except `/healthz` requires the
deploy bearer token, which the panel asks for once and keeps in the browser (a
`wt_token` cookie carries it on the event streams, where headers cannot go).
App logs persist under `/var/lib/wt/.logs` (or `WT_STATE_DIR`), outside each
app's `/data` preopen, and every line is stamped with the time the supervisor
received it so timestamps do not depend on how an app logs.

## Log levels

An app prefixes a line with a level and the supervisor separates it from the
message:

```text
TRACE cache miss for /api/messages
WARN  storage slow: 1.2s
ERROR could not save the note
```

`wt.packages/app` writes that shape for Go apps:

```go
app.Debugf("cache miss for %s", path)
app.Warnf("storage slow: %s", elapsed)
app.Errorf("could not save the note: %v", err)
```

`Logf` takes a level explicitly and the level set is `TRACE`, `DEBUG`, `INFO`,
`WARN`, `ERROR` and `FATAL`; `Fatalf` also exits the component. Lines without a
recognized prefix, including a library's own output, are `INFO`. Multi-line
messages get one line per level. A stdlib logger can adopt levels via
`log.SetFlags(0); log.SetOutput(app.Writer(app.LevelInfo))`.

The supervisor stores the level with the line, sends it in the log stream, and
the panel colours each level and filters by minimum severity.

To deploy from the panel without the CLI, build the bundle first and upload
the file:

```sh
/tmp/wt-deploy bundle --dir wt/example/guestbook --out /tmp/guestbook.tar.gz \
  --wasmtime /path/to/wasmtime
```

Then choose the file in the app's deploy form. The same bundle file works
with `curl -F artifact=@/tmp/guestbook.tar.gz` against
`POST /apps/<name>`, which is useful for CI.

## Deploy an example

```sh
/tmp/wt-deploy deploy \
  --dir wt/example/hello \
  --host http://127.0.0.1:8080 \
  --token dev \
  --wasmtime /path/to/wasmtime

curl -H 'Host: hello.localhost' http://127.0.0.1:8080/hello
# hello from wt
# method=GET
# path=/hello
```

`wt-deploy` reads `app.json`, runs its `build` command, precompiles
`main.wasm` with `wasmtime compile`, and uploads `app.json` and
`main.cwasm` as a gzipped tarball. `--app-host` overrides the host written
into the uploaded `app.json`, which is useful when the supervisor sits behind
a real ingress domain. The supervisor starts the new version on a
new loopback port while the old one serves, waits for its health path, swaps
the host route and stops the old process.

The notes example uses the per-app storage service:

```sh
/tmp/wt-deploy deploy --dir wt/example/notes --host http://127.0.0.1:8080 \
  --token dev --wasmtime /path/to/wasmtime

curl -H 'Host: notes.localhost' -X POST --data 'hello from wasmtime' \
  http://127.0.0.1:8080/notes
curl -H 'Host: notes.localhost' http://127.0.0.1:8080/notes
# data lives in /tmp/wt/state/.storage/notes/storage.db
```

## Scaffold a new app

```sh
/tmp/wt-deploy new --dir wt/example/myapp
/tmp/wt-deploy dev --dir wt/example/myapp --wasmtime /path/to/wasmtime
# myapp at http://127.0.0.1:8080/
```

`wt new` scaffolds a notes app with the guestbook stack: a static SvelteKit
frontend with Valibot request and response validation, a Go JSON API, the
Valibot to JSON Schema to `go-jsonschema` contract pipeline, and per-app
SQLite storage. New apps live under `wt/example/` so the shared module and
the relative `../../packages` paths keep working. `wt dev` builds the app
and serves it locally on loopback, with the SQLite database persisting in
`/tmp/wt-dev/<name>` between runs.

## What each app runs

```sh
wasmtime serve -Scli -Sp3 -S inherit-network -W component-model-async=y \
  --allow-precompiled \
  --addr 127.0.0.1:<port> \
  --dir /tmp/wt/state/<name>::/data \
  --env KEY=VALUE \
  --env WT_STORAGE_URL=http://127.0.0.1:<storage-port> \
  --env WT_STORAGE_TOKEN=<token> \
  main.cwasm
```

`app.json` carries `name`, `host`, `health`, `env`, `memory_mb`, `schedule`
and the local `build` command. `memory_mb`, when set, becomes wasmtime's
`-W max-memory-size`. The runtime-only image has no compiler, so `main.wasm`
cannot run there: `main.cwasm` is required. If it is missing or incompatible,
the deploy fails and the old version keeps serving.

## Scheduled work

A component under `wasmtime serve` only runs while it is handling a request:
instances are reused briefly between requests, and the guest is never scheduled
while idle, so a goroutine with a timer cannot keep working on its own. Periodic
work is therefore driven from the host by `schedule` entries in `app.json`:

```json
"schedule": [
  { "method": "POST", "path": "/tick", "every": "10s" }
]
```

The supervisor starts one goroutine per entry once the app is serving and
cancels it when the app stops or is replaced. Calls are sequential, so a slow
endpoint delays the next tick rather than piling requests up; failures and
non-2xx responses are written to the app's log. `method` defaults to `POST`,
`path` must start with `/`, and `every` is a Go duration between `1s` and `24h`.
Limits are checked by both `wt-deploy` and the supervisor.

## Container

```sh
podman build -f wt/Containerfile -t wt-demo .
podman run -d --name wt-demo -p 127.0.0.1:8080:8080 \
  -e WT_TOKEN=dev -v "$PWD/wt-state:/var/lib/wt" wt-demo
```

The image is built entirely by `wt/Containerfile`: a Rust stage compiles
wasmtime from the v50.0.0 source with `--no-default-features` and only the
serve/run, component-model-async, pooling, GC/threads and WASI HTTP features,
then strips it. There is no Cranelift or Winch in the serving image because it
only deserializes `.cwasm` files. The musl build is static and the final stage
is `scratch`, so the image is about **33 MB** (down from 168 MB with the full
release on Debian).

The C API tarball is a shared library for embedding, not a server, and still
contains the compiler, so it does not replace this runtime-only CLI. The image
runs as root for the prototype; the volume at `/var/lib/wt` holds apps and
state.

On Orchard, create a GitHub-built deployment from this repository with
`dockerfilePath: wt/Containerfile`, `buildFromRoot: true`, port `8080`, and a
`WT_TOKEN` environment variable. Mount a PVC at `/var/lib/wt` so apps and the
storage databases survive a pod roll. Create one ingress per app host, all
targeting port `8080`, then deploy with
`--host https://<ingress-host> --app-host <ingress-host>`. The supervisor
routes by `Host`, so `/apps/...` (the deploy API) and `/` (the app) share the
same ingress host; each app needs its own host.

The deployment can run on a small CPU request because it never compiles: in
the demo deployment two apps start from `.cwasm` files rebuilt on a laptop,
and the notes database survives a pod roll on the PVC.

## Storage

The supervisor starts a loopback SQLite service and gives every app a token
and URL through `WT_STORAGE_URL` and `WT_STORAGE_TOKEN`. Every app gets its own
database at `WT_STATE_DIR/.storage/<app>/storage.db`, outside its `/data`
preopen, so apps cannot touch the file directly.

The API is deliberately small SQL rather than a key-value shim:

```text
POST /v1/query   {"sql": "...", "params": [...], "max_rows": 1000}
POST /v1/exec    {"sql": "...", "params": [...]}
POST /v1/batch   {"statements": [{"sql": "...", "params": [...]}, ...]}
```

`query` returns `{"columns": [...], "rows": [[...]], "truncated": false}`.
`exec` returns `{"rows_affected": n, "last_insert_id": n}`. `batch` runs every
write statement in one transaction and returns one result per statement; use
`query` for reads (it also runs `INSERT ... RETURNING`). Values are JSON
scalars; blobs are `{"$b64": "..."}`. Parameters are bound, not interpolated.
A few SQLite features are blocked (`ATTACH`, `VACUUM INTO`, `PRAGMA`,
`load_extension`, `readfile`, `writefile`, transaction-control statements) and
`SQLITE_LIMIT_ATTACHED` is zero, so a component cannot read another app's
database or write files through SQL. Queries time out after five seconds and
are capped at 10,000 rows, 1 MiB per value and 8 MiB per response.

The shared `packages/storage` package is the client; it takes an
`*http.Client`, and the apps pass `wasihttp.NewClient()` for outbound
`wasi:http`:

```go
rows, err := store.Query(ctx, `select id, body, created_at from notes order by created_at`)
// ...
_, err = store.Batch(ctx,
    storage.Statement{SQL: `insert into notes (id, body, created_at) values (?, ?, ?)`, Args: []any{id, body, at}},
    storage.Statement{SQL: `delete from notes where id not in (select id from notes order by created_at desc, id desc limit ?)`, Args: []any{50}},
)
```

The notes example uses a real `notes` table. The one-time migration from the
old JSON value has already run on the demo deployment, and the old `/v1/kv`
endpoints have been removed.

### Why not a WIT storage import?

There is no durable, standard WASI storage interface to target in this setup:

- `wasmtime serve` 50 can enable the experimental `wasi:keyvalue` API, but its
  only provider is in-memory and per-instance. Writes do not survive the
  instance or the process, so it cannot back the notes app.
- `wasi:sql` is a Phase 1 proposal with no implementation in wasmtime.
- Guest-side SQLite is not practical from Go: `modernc.org/sqlite` does not
  build for `wasip1`, and `ncruces/go-sqlite3` adds a nested wazero runtime
  (about 10 MB) with a separate VFS, so it cannot simply use the outer
  `wasi:filesystem` preopen. WASI preview1 also does not give SQLite the file
  locking it needs across component instances.
- A custom host import would mean replacing `wasmtime serve` with an embedded
  runtime (a fork of its p3 serve path) or composing an adapter component at
  deploy time. Both add more moving parts than the loopback service and still
  need the same host-side SQLite database.

The app still uses a WASI API for transport: `wasi:http` over loopback. If
wasmtime grows a persistent `wasi:keyvalue` or `wasi:sql` provider, the client
package can switch without changing the app's SQL.

The supervisor grants outbound network with `-S inherit-network` so apps can
reach that loopback endpoint. A production version would scope outbound
access per app.

## Bindings

The generated bindings under `packages/binding` target the world:

```wit
package wt:app;

world app {
  include wasi:cli/imports@0.3.0;
  include wasi:http/service@0.3.0;
}
```

They are **not tracked**: `packages/generate.sh` regenerates them from
`packages/wit` with `componentize-go bindings`. The example apps' `build`
commands run that script first, so a fresh clone builds on its first deploy.

The WIT comes from wasmtime v50's `crates/wasi-http/src/p3/wit` and the world
file is ours. If you bump wasmtime, update the WIT and run the generator again;
the export stub is written by the script.

The `net/http` adapter in `packages/wasihttp` is adapted from
[spin-go-sdk v3](https://github.com/spinframework/spin-go-sdk) (Apache-2.0).
The generated bindings and the adapter are what let an app keep normal
`http.HandlerFunc` code.

## Limits of this prototype

- One `wasmtime serve` process per app. A crashed app restarts with exponential
  backoff (2s up to 30s), reusing its port and proxy route.
- `memory_mb` is a per-app linear-memory cap; CPU stays a container-level
  concern.
- TLS is optional and basic (certificate files); use the Orchard ingress for
  real certificates and wildcard hosts.
- Storage is a per-app SQLite SQL service, not a standard WASI import. The
  SQLite file is private to the supervisor; apps get a preopen at `/data` as
  well.
- Outbound network is granted to every app (`-S inherit-network`), including
  the loopback storage service.
- The deploy POST waits for the new version; there are no deploy records or
  long-polling yet. Startup is fast enough that this is usually not noticeable.
- `.cwasm` is native code. Only load artifacts produced by your own deploy
  pipeline; the deploy endpoint is the trust boundary. The CLI and supervisor
  must agree on the wasmtime version and target triple; the runtime-only image
  is musl and accepts only `x86_64-unknown-linux-musl` artifacts.
- The container runs as root and has no per-app CPU limits.
- `Pdeathsig` (Linux) stops app processes if the supervisor dies; adjust for
  other platforms.
