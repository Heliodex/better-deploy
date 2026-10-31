# wt

A prototype of a small Wasm deploy platform built directly on [wasmtime](https://wasmtime.dev), with no Spin and no container image.

The deploy artifact is `main.wasm`. Compiling it to native code is a cache step, not part of the artifact:

```text
wt-deploy:  build (componentize-go) → optional wasmtime compile → upload main.wasm
supervisor: Caddy-less Go reverse proxy, cwasm cache, one `wasmtime serve` per app
runtime:    a runtime-only wasmtime 50 loads a cached .cwasm compiled from main.wasm
storage:    a per-app SQLite SQL service on loopback, plus /data via WASI
```

The first request for a precompiled Go component is tens of milliseconds on a laptop. A supervisor with a compiler can fill its cache on deploy; a runtime-only image needs an uploaded matching `.cwasm` or a configured compiler. The app is a standard `wasi:http/service@0.3.0` component.

## Layout

- `packages/` – the shared Go module for the app SDK.
  - `packages/storage` – client for the supervisor's SQLite service.
  - `packages/wasihttp` – the `net/http` adapter (adapted from spin-go-sdk).
  - `packages/binding` – generated bindings for `wt:app/app` (not tracked).
  - `packages/wit` – the `wt:app/app` world and WASI 0.3 WIT dependencies.
  - `packages/generate.sh` – regenerates `packages/binding`.
- `example/` – example apps only.
  - `example/hello` – returns the method and path.
  - `example/notes` – JSON notes persisted in a real `notes` table through the supervisor's SQLite service.
  - `example/shelf` – link shelf with tags, search, pagination and click analytics.
  - `example/ticker` – logs a `ticker beat` line every 10 seconds from an `app.json` schedule, so the supervisor log tail has a steady stream to show.
  - `example/guestbook` – SvelteKit static frontend with a Go JSON API; the frontend is served from the bundle's `static/` directory, not embedded in the component.
- `supervisor/` – runs apps, proxies by `Host`, deploys bundles blue/green.
  - `supervisor/web` – SvelteKit admin panel, embedded into the supervisor binary (not tracked: `web/build`, `web/node_modules`, `web/.svelte-kit`).
- `deploy/` – builds, precompiles and uploads an app.

## Requirements

- Go 1.27.1 or newer. `componentize-go` is declared as a tool in both `packages/go.mod` (binding generation) and `example/go.mod` (app builds); it downloads the patched Go it needs for async components.
- The `example/guestbook` build also needs Bun. It runs `bun install` and `bun run build` in `example/guestbook/web`, then copies the static output into `example/guestbook/static`, which `wt-deploy` packs beside the component. `example/guestbook/contract/generate.go` turns the Valibot request schemas into JSON Schema and generated Go request types. `go generate ./contract/` runs it as part of the build; the generated schema and Go files are not tracked.
- wasmtime **50.0.0** is optional on the machine running `wt-deploy`: bundles always carry `main.wasm` and an `artifact.json` with the wasm digest, size, compiler version and target. An uploaded `main.cwasm` is only a precompiled hint and is accepted when its recorded runtime, target, epoch flag and wasm digest match. The supervisor keeps compiled modules in `WT_CACHE_DIR` keyed by those values, and can compile from `main.wasm` when `WT_WASMTIME_COMPILER` has the `compile` subcommand. The client and supervisor both default to `x86_64-unknown-linux-musl`, which is what the runtime-only image can execute; a different target needs a matching runtime build.
- The WIT in `packages/wit` is the final `wasi:http@0.3.0` from wasmtime v50 (`crates/wasi-http/src/p3/wit`), plus `wasi:cli/imports`.

## Build the supervisor and CLI

```sh
cd wt/supervisor && go generate ./... && go build -o /tmp/wt-supervisor .
cd wt/deploy && go build -o /tmp/wt-deploy .
```

For the local loop, `wt/supervisor/dev.sh` rebuilds the admin panel and binary and restarts the supervisor, reusing the running instance's app and state directories so deployed apps, data and logs survive. `./dev.sh --skip-generate` skips the panel build for a Go-only change.

## Test

CI runs unit tests for the supervisor, deploy tool and shared packages, vets the wasm guests, checks both Svelte frontends, and has a separate end-to-end job. `wt/e2e.sh` downloads nothing itself: point it at a real wasmtime with the compile and serve subcommands. It builds the hello component, starts a supervisor on `127.0.0.1:18080`, deploys the bundle, proxies a request, checks that a spinning `?spin=1` request is cut off while the app stays up, takes and restores a `/data` snapshot, and rolls back code and data together:

```sh
WT_WASMTIME=/path/to/wasmtime ./wt/e2e.sh
```

Set `WT_E2E_TARGET` when the runtime uses a different target triple; the default is `x86_64-unknown-linux-gnu` to match the released wasmtime binary CI downloads. The script keeps its work directory on failure and prints the supervisor log.

The supervisor has fuzz targets for tar extraction, the SQL guard and parameter decoding:

```sh
cd wt/supervisor
go test -run '^$' -fuzz FuzzExtractTarGz -fuzztime 30s
go test -run '^$' -fuzz FuzzValidateStorageSQL -fuzztime 30s
go test -run '^$' -fuzz FuzzDecodeStorageValue -fuzztime 30s
```

`BenchmarkProxyLatency` measures the reverse proxy against a loopback backend, and `BenchmarkStorage*`/`BenchmarkSQLite*` cover the storage query path.

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

`WT_WASMTIME_COMPILER` (or `--wasmtime-compiler`) selects the binary used to compile `main.wasm`; it defaults to `WT_WASMTIME`. The compiled module cache lives under `WT_CACHE_DIR` (default `WT_STATE_DIR/.cache`) and is capped by `WT_CACHE_MAX_BYTES` (default 2 GiB). Its key is the wasm digest plus the runtime version, target triple and epoch setting, so a runtime or target change compiles a new entry and old entries are pruned oldest first. A runtime-only wasmtime build cannot compile, so it needs an uploaded matching `main.cwasm` or a separate builder that fills the cache.

The supervisor speaks plain HTTP by default; set `WT_TLS_CERT` and `WT_TLS_KEY` (or `--tls-cert`/`--tls-key`) to terminate TLS itself. Behind the Orchard ingress, leave TLS to the ingress and keep the supervisor on HTTP. The proxy preserves the original `Host` and sets `X-Forwarded-Host` and `X-Forwarded-Proto` for the app. It connects within five seconds and waits at most thirty seconds for response headers, so a hung app returns `502` instead of holding the client connection open. An app can set `max_concurrency` to cap simultaneous proxied requests (extra requests get `429`) and `request_timeout` to interrupt a request that runs too long, including a spinning guest.

Every app is probed at its `health_interval` (default ten seconds, three-second probe timeout), and the supervisor reports `running`, `unhealthy`, `restarting` or `stopped`. `/healthz` is the default health path. `health_failures` consecutive failures mark the app unready: component traffic gets `503` with `Retry-After` while probing continues, and a later success clears the error, restores routing and updates `lastHealthy`. `ready` appears in `GET /apps`, the SSE snapshot and the admin panel. With `restart_on_unhealthy` set, crossing the threshold kills the process once and the normal restart backoff applies; otherwise health state does not stop or restart it. `startup_timeout` (default 15s) bounds how long a new process may take to answer its health path.

The supervisor scans `WT_APPS_DIR` at boot, starts every app found there, and serves `GET /apps`, `GET /apps/<name>` and `POST /apps/<name>` (deploy, bearer token). `/healthz` is the supervisor's own health check, and it is also the default component health path on an app host. These supervisor paths, including `/admin/` and `/events`, only exist on hosts that are not app hosts: an app owns every path on its own host, so requests to those names are proxied to the app rather than swallowed by the supervisor.

## Admin panel

The supervisor serves a small admin panel at `/admin/` on its own address (for example `http://127.0.0.1:8080/admin/` locally, or the deploy host in production). It lists apps with status, uptime and disk usage, streams each app's log live over server-sent events with level and text filters, restarts apps, starts and stops them, edits their runtime config, deletes them with an optional state purge, uploads new bundles, configures and restores database and `/data` backups, browses each app's database tables and recent writes, and shows the retained deployment history with a rollback action. Updates stream over server-sent events, so the dashboard reflects deploys and restarts without refreshing. Every endpoint except `/healthz` requires the deploy bearer token, which the panel asks for once and keeps in the browser. `POST /login` validates it and sets `wt_token` as an `HttpOnly`, `SameSite=Lax` cookie (also `Secure` when the request is HTTPS or arrives with `X-Forwarded-Proto: https`), so event streams authenticate without JavaScript reading the cookie; `POST /logout` clears it. Ordinary panel requests still send a bearer header. The token is also kept in `localStorage` so a reload can re-authenticate, which means a script injection on the admin origin can still read it; there are no scopes, audit log or login rate limiting. The supervisor refuses to start on a non-loopback address without `WT_TOKEN`, so a loopback-only listener is the only unauthenticated mode. A reverse proxy on the same host makes remote requests look like loopback, so do not expose the loopback listener through a proxy without setting `WT_TOKEN`. App logs persist under `/var/lib/wt/.logs` (or `WT_STATE_DIR`), outside each app's `/data` preopen, and every line is stamped with the time the supervisor received it so timestamps do not depend on how an app logs. The supervisor also logs each proxied request at info level into a separate per-app supervisor log, as `METHOD status duration path`; the panel recognises the shape and lays it out as structured columns with each line badged by origin.

## Resource usage

The app detail charts CPU and resident memory for the `wasmtime serve` process, read from `/proc/<pid>/stat` and `/proc/<pid>/statm`. CPU is a percentage of one core and can exceed 100% for a multi-threaded component; memory is the process's resident set size. The supervisor samples every second and keeps thirty minutes of history, which the panel fetches from `GET /apps/<name>/resources` and draws as two charts; live figures arrive in the same SSE snapshots, so the charts keep moving without polling. History is persisted under `WT_STATE_DIR/.resources` every ten seconds and on shutdown, so it survives restarts with at most a few seconds lost. A stopped app records zeros, and a container without a mounted `/proc` shows memory as `–`.

## Log levels

An app prefixes a line with a level and the supervisor separates it from the message:

```text
TRACE cache miss for /api/messages
WARN  storage slow: 1.2s
ERROR could not save the note
```

The panel keeps log rows on one line and virtualises them, so long messages are truncated with the full text in the tooltip; clicking a row opens a detail strip with the whole message and its timestamp. Filtering by minimum level and text happens in the browser over the loaded tail.

`wt.packages/app` writes that shape for Go apps:

```go
app.Debugf("cache miss for %s", path)
app.Warnf("storage slow: %s", elapsed)
app.Errorf("could not save the note: %v", err)
```

`Logf` takes a level explicitly and the level set is `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR` and `FATAL`; `Fatalf` also exits the component. Lines without a recognized prefix, including a library's own output, are `INFO`. Multi-line messages get one line per level. A stdlib logger can adopt levels via `log.SetFlags(0); log.SetOutput(app.Writer(app.LevelInfo))`.

The supervisor stores the level with the line, sends it in the log stream, and the panel colours each level and filters by minimum severity.

To deploy from the panel without the CLI, build the bundle first and upload the file:

```sh
/tmp/wt-deploy bundle --dir wt/example/guestbook --out /tmp/guestbook.tar.gz \
  --wasmtime /path/to/wasmtime
```

Then choose the file in the app's deploy form. The same bundle file works with `curl -F artifact=@/tmp/guestbook.tar.gz` against `POST /apps/<name>`, which is useful for CI.

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

`wt-deploy` reads `app.json`, runs its `build` command, precompiles `main.wasm` with `wasmtime compile` when available, and uploads `app.json`, `main.wasm`, `artifact.json`, `main.cwasm` when it exists, and any `static/` files as a gzipped tarball. `main.wasm` is the artifact; `main.cwasm` is a cache hint. `artifact.json` records the wasm digest and size, the wasmtime version, target triple and compile time. `--skip-compile` does not require a cwasm and warns when none is included, leaving the supervisor to compile from wasm if it has a compiler. `--app-host` overrides the host written into the uploaded `app.json`, which is useful when the supervisor sits behind a real ingress domain. A deploy with the same wasm digest and unchanged runtime config keeps the running process and replaces only `app.json` and `static/`, so a frontend-only change does not restart the app. Otherwise the supervisor starts the new version on a new loopback port while the old one serves, waits for a 2xx health response, swaps the host route, drains in-flight requests for up to ten seconds and then stops the old process. Responses include `X-WT-Wasm-Digest` and `X-WT-Deploy-Mode: full|static`; `?dry_run=1` returns the same digest without installing.

The notes example uses the per-app storage service:

```sh
/tmp/wt-deploy deploy --dir wt/example/notes --host http://127.0.0.1:8080 \
  --token dev --wasmtime /path/to/wasmtime

curl -H 'Host: notes.localhost' -X POST --data 'hello from wasmtime' \
  http://127.0.0.1:8080/notes
curl -H 'Host: notes.localhost' http://127.0.0.1:8080/notes
# data lives in /tmp/wt/state/.storage/notes/storage.db
```

## Lifecycle and configuration

The admin token also guards the lifecycle and config endpoints:

```text
GET    /apps/<name>/config      runtime config, with env keys but not values
PATCH  /apps/<name>/config      partial config update
GET    /apps/<name>/deployments list retained deployment bundles
POST   /apps/<name>/rollback/<id> install a retained code bundle
       ?backup=<file>             also restore that snapshot's database and /data
POST   /apps/<name>/stop        stop the process, keeping the app and its host
POST   /apps/<name>/start       relaunch a stopped app
DELETE /apps/<name>             remove code and process; keep state
DELETE /apps/<name>?purge=1     also remove the database, backups, logs, history and /data
POST   /apps/<name>?dry_run=1   validate an uploaded bundle without installing it
```

`PATCH` accepts any of `host`, `health`, `env`, `memory_mb`, `disk_mb`, `max_concurrency`, `request_timeout`, `startup_timeout`, `health_interval`, `health_failures`, `restart_on_unhealthy`, `network` and `schedule`. Env changes are a merge; a `null` value removes a key. `GET` returns `env_keys` rather than values, so secrets are never echoed back. Host, health and schedule changes take effect immediately; env, memory, concurrency, timeout and network changes are saved to `app.json` and applied on the next start or restart, which the response reports as `restart_required`. Host changes update routing at once, and are rejected with `409` if another app already owns the host.

`disk_mb` is a soft per-app quota over `/data` plus the app's SQLite database files. Logs, backups and deployment bundles are not counted. The app snapshot reports `diskBytes`, `diskLimitBytes` and `overQuota`; the supervisor does not stop an app for exceeding the quota. `0` or an omitted value means unlimited.

Every successful deploy or rollback retains a bundle under `WT_STATE_DIR/.deployments/<app>`: `app.json`, `main.wasm`, `main.cwasm`, `artifact.json`, `static/`, and a `meta.json` with its wasm digest, size and artifact metadata. Identical artifacts are stored once under `.deployments/<app>/.blobs` and hardlinked into each bundle, so a config-only redeploy does not copy the component again. New records set `digest` to the wasm digest and also carry `wasmDigest` and `wasmBytes`; legacy cwasm-only history keeps the cwasm digest. The newest ten bundles are kept, and history is capped at 1 GiB per app; the panel shows the resulting on-disk size. A rollback installs the retained code and config but leaves the live database and `/data` untouched, so schema changes between the two versions still matter.

`DELETE` is deliberately conservative: it removes the app directory and stops the process, but leaves the database, backups, logs, resource history, deployment history and `/data` intact unless `purge=1` is set.

```sh
curl -X PATCH -H "Authorization: Bearer $WT_TOKEN" \
  -d '{"memory_mb":256,"env":{"NEW_KEY":"value","OLD_KEY":null}}' \
  http://127.0.0.1:8080/apps/notes/config
```

## Scaffold a new app

```sh
/tmp/wt-deploy new --dir wt/example/myapp
/tmp/wt-deploy dev --dir wt/example/myapp --wasmtime /path/to/wasmtime
# myapp at http://127.0.0.1:8080/
```

`wt new` scaffolds a notes app with the guestbook stack: a static SvelteKit frontend with Valibot request and response validation, a Go JSON API, the Valibot to JSON Schema to `go-jsonschema` contract pipeline, and per-app SQLite storage. New apps live under `wt/example/` so the shared module and the relative `../../packages` paths keep working. `wt dev` builds the app and serves it locally on loopback, with the SQLite database persisting in `/tmp/wt-dev/<name>` between runs. It watches source files and rebuilds and redeploys after a change, ignoring `static/`, `build`, `dist`, `target`, `node_modules`, `.svelte-kit` and the generated `main.wasm`/`main.cwasm` so a build does not retrigger itself. Pass `--watch=false` to deploy once.

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

Before the proxy runs, the supervisor answers `GET`/`HEAD` from the app's `static/` directory. A matching file is served with immutable caching under `/_app/immutable/` and revalidation elsewhere; an unknown path falls back to `static/index.html` only when the request accepts `text/html`, so API fetches still reach the component. This keeps frontend assets out of the wasm component and lets a static-only change ship without recompiling it. Static files are only served while the app process is running; a stopped app returns `503`. During a deploy the old process keeps its route until draining, and its directory already points at the new bundle, so it serves the new assets. `wt.packages/app.StaticFiles` remains available for apps that prefer to embed a frontend.

`app.json` carries `name`, `host`, `health`, `env`, `memory_mb`, `disk_mb`, `max_concurrency`, `request_timeout`, `startup_timeout`, `health_interval`, `health_failures`, `restart_on_unhealthy`, `network`, `schedule` and the local `build` command. `memory_mb`, when set, becomes wasmtime's `-W max-memory-size`. `max_concurrency` is enforced by the supervisor proxy. `request_timeout` is a Go duration between `1s` and `24h`: the supervisor cancels the proxied request at that deadline and, for bundles compiled with epoch interruption (all new `wt-deploy` bundles are), also passes `-W timeout=<duration>` to wasmtime so a CPU-bound guest is interrupted instead of left spinning. Leave it unset for streaming or long-running responses. Older bundles with no epoch flag in `artifact.json` get only the proxy deadline; redeploy or restart with a compiler available to rebuild them. `startup_timeout` is a Go duration between `1s` and `10m`, default `15s`; `health_interval` is between `1s` and `1h`, default `10s`; `health_failures` is between 1 and 100, default 2. `network` defaults to `inherit`, which passes `-S inherit-network` so the guest can reach the loopback storage service and other hosts; `none` omits it, so the guest cannot open sockets. An app with `network` set to `none` cannot use the storage service or make outbound HTTP requests, which suits static or self-contained components. The health path defaults to `/healthz` and must answer with a 2xx status before a new version is considered ready. The scaffold's `app.ServeStorage` reserves `/healthz` for this probe and answers `204` once storage migrations have run. The runtime-only image has no compiler, so it needs a matching cached or uploaded `main.cwasm`; `main.wasm` alone cannot run there. At startup the supervisor computes the wasm digest and looks in `WT_CACHE_DIR` for an entry built for its runtime version, target and epoch setting. On a hit it hardlinks the cached module into the app directory as `main.cwasm`; on a miss it uses a matching uploaded cwasm (importing it into the cache) or compiles `main.wasm` into the cache. The compiler defaults to `WT_WASMTIME`; set `WT_WASMTIME_COMPILER` (or `--wasmtime-compiler`) to a wasmtime build with the `compile` subcommand when the runtime binary cannot compile. If no compiler and no matching cwasm are available, the app fails to start and keeps its previous version, and you redeploy with a matching `wt-deploy`.

## CPU and request limits

Wasmtime's per-request execution limit is `-W timeout=<duration>`, implemented with epoch interruption: a host thread advances an epoch and the guest is interrupted at its next check. `wasmtime serve` applies it per request, so it fits the same `request_timeout` field the proxy uses. Bundles compiled by `wt-deploy` carry `epoch_interruption: true` in `artifact.json`; when that flag is present the supervisor starts the app with `-W epoch-interruption=y` and, if a timeout is configured, `-W timeout=2s`. The proxy deadline remains as a backstop and returns `502` when it wins the race; the guest is still interrupted, and wasmtime logs `worker failed: guest timed out`.

Fuel (`-W fuel=N`) was the other candidate, but in `serve` it belongs to a store rather than a request. With instance reuse it is shared across requests and is not replenished, so a fixed budget would eventually kill a healthy app. Epoch timeouts reset per request and need no per-request supervisor bookkeeping.

Old bundles without the epoch flag keep working but only get the proxy deadline. Setting `request_timeout` makes the supervisor try to rebuild them with epoch support when a compiler is configured; on the runtime-only image there is no compiler, so redeploy from a machine that has one. The e2e suite requests the hello example's `?spin=1` endpoint to verify that the guest is interrupted and the app stays up.

## Scheduled work

A component under `wasmtime serve` only runs while it is handling a request: instances are reused briefly between requests, and the guest is never scheduled while idle, so a goroutine with a timer cannot keep working on its own. Periodic work is therefore driven from the host by `schedule` entries in `app.json`:

```json
"schedule": [
  { "method": "POST", "path": "/tick", "every": "10s" }
]
```

The supervisor starts one goroutine per entry once the app is serving, restarts it after a manual restart, and cancels it when the app stops or is replaced. Calls are sequential, so a slow endpoint delays the next tick rather than piling requests up; failures and non-2xx responses are written to the app's log. `method` defaults to `POST`, `path` must start with `/`, and `every` is a Go duration between `1s` and `24h`. Limits are checked by both `wt-deploy` and the supervisor.

## Container

```sh
podman build -f wt/Containerfile -t wt-demo .
podman run -d --name wt-demo -p 127.0.0.1:8080:8080 \
  -e WT_TOKEN=dev -v "$PWD/wt-state:/var/lib/wt" wt-demo
```

The image is built entirely by `wt/Containerfile`: a Rust stage compiles wasmtime from the v50.0.0 source with `--no-default-features` and only the serve/run, component-model-async, pooling, GC/threads and WASI HTTP features, then strips it. The build images are pinned (`golang:1.27.3-alpine`, `rust:1.97-slim`, `oven/bun:1.4.4-alpine`) and CI also runs `govulncheck` and a production-only `bun audit`. There is no Cranelift or Winch in the serving image because it only deserializes `.cwasm` files. The musl build is static and the final stage is `scratch`, so the image is about **33 MB** (down from 168 MB with the full release on Debian).

The C API tarball is a shared library for embedding, not a server, and still contains the compiler, so it does not replace this runtime-only CLI. The image runs as root for the prototype; the volume at `/var/lib/wt` holds apps and state.

On Orchard, create a GitHub-built deployment from this repository with `dockerfilePath: wt/Containerfile`, `buildFromRoot: true`, port `8080`, and a `WT_TOKEN` environment variable. Mount a PVC at `/var/lib/wt` so apps and the storage databases survive a pod roll. Create one ingress per app host, all targeting port `8080`, then deploy with `--host https://<ingress-host> --app-host <ingress-host>`. The supervisor routes by `Host`, so `/apps/...` (the deploy API) and `/` (the app) share the same ingress host; each app needs its own host.

The deployment can run on a small CPU request because it never compiles: in the demo deployment two apps start from `.cwasm` files rebuilt on a laptop, and the notes database survives a pod roll on the PVC.

## Storage

The supervisor starts a loopback SQLite service and gives every app a token and URL through `WT_STORAGE_URL` and `WT_STORAGE_TOKEN`. Every app gets its own database at `WT_STATE_DIR/.storage/<app>/storage.db`, outside its `/data` preopen, so apps cannot touch the file directly.

The API is deliberately small SQL rather than a key-value shim:

```text
POST /v1/query   {"sql": "...", "params": [...], "max_rows": 1000}
POST /v1/exec    {"sql": "...", "params": [...]}
POST /v1/batch   {"statements": [{"sql": "...", "params": [...]}, ...]}
POST /v1/transaction {"statements": [{"sql": "...", "query": true}, ...]}
```

`query` returns `{"columns": [...], "rows": [[...]], "truncated": false}`. `exec` returns `{"rows_affected": n, "last_insert_id": n}`. `batch` runs every write statement in one transaction and returns one result per statement; use `query` for reads (it also runs `INSERT ... RETURNING`). `transaction` runs reads and writes in one transaction: mark read statements with `"query": true` to get their rows back, and the rest return `rows_affected` and `last_insert_id`. This keeps a read-modify-write in one atomic request without an open session; in Go, `store.Transaction(ctx, statements...)` returns the same shape. Queries use a small WAL-backed connection pool, so reads can proceed while one write is in flight, while SQLite still serialises writers. Values are JSON scalars; blobs are `{"$b64": "..."}`. Parameters are bound, not interpolated. A few SQLite features are blocked (`ATTACH`, `VACUUM INTO`, `PRAGMA`, `load_extension`, `readfile`, `writefile`, transaction-control statements) and `SQLITE_LIMIT_ATTACHED` is zero, so a component cannot read another app's database or write files through SQL. Queries time out after five seconds and are capped at 10,000 rows, 1 MiB per value and 8 MiB per response.

Schema changes go through a versioned migration instead of raw `PRAGMA`:

```text
GET  /v1/schema
POST /v1/migrate  {"version": 2, "statements": [{"sql": "...", "params": [...]}, ...]}
```

`schema` returns SQLite's `user_version` and every user table with its columns, types, defaults and primary-key positions. `migrate` runs its statements and sets `user_version` in one transaction; a failing statement rolls back both. Applying a version at or below the current one is a no-op, so an older blue or green process can start against a database a newer deploy already migrated. In Go, `store.Migrate(ctx, version, statements...)` and `store.Schema(ctx)` wrap these endpoints. The examples use `Migrate` for their initial schema.

Migrations must be expand/contract: add columns and tables in one release, move writes over, and remove the old shape only after every deploy that reads it is gone. A code rollback is code-only, so it also has to be able to read the newer schema; for a destructive change, restore a snapshot from before the migration as well (`rollback?backup=<file>`).

The shared `packages/storage` package is the client; it takes an `*http.Client`, and the apps pass `wasihttp.NewClient()` for outbound `wasi:http`:

```go
rows, err := store.Query(ctx, `select id, body, created_at from notes order by created_at`)
// ...
_, err = store.Batch(ctx,
    storage.Statement{SQL: `insert into notes (id, body, created_at) values (?, ?, ?)`, Args: []any{id, body, at}},
    storage.Statement{SQL: `delete from notes where id not in (select id from notes order by created_at desc, id desc limit ?)`, Args: []any{50}},
)
```

The notes example uses a real `notes` table. The one-time migration from the old JSON value has already run on the demo deployment, and the old `/v1/kv` endpoints have been removed.

### Why not a WIT storage import?

There is no durable, standard WASI storage interface to target in this setup:

- `wasmtime serve` 50 can enable the experimental `wasi:keyvalue` API, but its only provider is in-memory and per-instance. Writes do not survive the instance or the process, so it cannot back the notes app.
- `wasi:sql` is a Phase 1 proposal with no implementation in wasmtime.
- Guest-side SQLite is not practical from Go: `modernc.org/sqlite` does not build for `wasip1`, and `ncruces/go-sqlite3` adds a nested wazero runtime (about 10 MB) with a separate VFS, so it cannot simply use the outer `wasi:filesystem` preopen. WASI preview1 also does not give SQLite the file locking it needs across component instances.
- A custom host import would mean replacing `wasmtime serve` with an embedded runtime (a fork of its p3 serve path) or composing an adapter component at deploy time. Both add more moving parts than the loopback service and still need the same host-side SQLite database.

The app still uses a WASI API for transport: `wasi:http` over loopback. If wasmtime grows a persistent `wasi:keyvalue` or `wasi:sql` provider, the client package can switch without changing the app's SQL.

The supervisor grants outbound network with `-S inherit-network` so apps can reach that loopback endpoint. A production version would scope outbound access per app.

## Database backups

A snapshot captures the app's whole writable state. When the app has a SQLite database, the supervisor copies it with SQLite's online backup API, so the snapshot is consistent even while the app is reading and writing and the app keeps serving throughout. Every snapshot also has a `storage-<timestamp>.data.tar.gz` archive of the app's `/data` directory; the archive is written while the app runs, so it is best-effort rather than transactional. An app that has never used storage gets a data-only snapshot, represented by the archive alone. Snapshots live at `WT_STATE_DIR/.backups/<app>/`, outside both the storage directory and the app's `/data` preopen. Each app's policy lives in `config.json` and its snapshot ancestry in `tree.json` beside them.

Automatic backups are on by default: every app is snapshotted every 24 hours, and 7 snapshots are kept. The supervisor checks once a minute for policies that are due, including apps that have no database yet. Changing the policy takes effect on the next check, and reducing `keep` prunes older snapshots immediately. Pruning removes abandoned leaves first and keeps the branch the live database follows for as long as possible. Snapshots and their tree survive deploys, restarts and pod rolls with the rest of the state volume.

A new snapshot is rejected when its database and data archive are byte-identical to the current snapshot, so an unchanged app does not add a node. Snapshots made before `/data` archives existed have no data file; restoring one leaves the current `/data` in place. Restoring a snapshot with a data archive stops a running app only for the restore, replaces `/data` and the database when the snapshot has one, then starts it again. A data-only snapshot replaces `/data` and leaves any database created since it was taken in place. Snapshot hashes are kept in the tree, and older snapshots get one when first compared. For automatic backups the check time still advances, so an unchanged database is retried after the next interval rather than every minute. Any committed SQLite write, including one that stores the same value again, can change the file and is therefore accepted as a new snapshot.

The admin panel's app detail shows snapshots as a tree. Each snapshot descends from the snapshot the live database followed when it was taken, so a chain of daily backups reads down the tree. Reverting moves the current node, and the next snapshot then forms a new branch from the restored snapshot instead of continuing the old one. Selecting a node shows its time, size, kind, parent and child count, and offers **Back up current state** and **Revert to this snapshot**. Reverting is a two-click action because it replaces all data written since that snapshot. Snapshots made before the tree existed are chained in creation order when the index is first built. The same operations are available over the authenticated API:

```text
GET  /apps/<name>/backups                          list settings and snapshots
POST /apps/<name>/backups                          take a snapshot now
PUT  /apps/<name>/backups/config                   {"enabled": true, "every": "24h", "keep": 7}
POST /apps/<name>/backups/<file>/restore           replace the data and database
GET  /apps/<name>/backups/<file>/download          download the snapshot as a tar.gz
POST /apps/<name>/backups/import                   store an exported snapshot archive
```

`every` is a Go duration between `1m` and `720h`, and `keep` is between 1 and 100. A manual snapshot works even when automatic backups are disabled. A restore validates the database with SQLite's `quick_check` first, then copies it through the online backup API; connections stay open, but any writes made after the snapshot are lost. Restoring a snapshot with a data archive stops a running app, restores the database when present and `/data`, and starts it again; a database-only legacy snapshot restores online and leaves `/data` alone. To bring back code and data together, pass `?backup=<file>` to the rollback endpoint. The supervisor stages the bundle and snapshot first, stops the app, swaps the code directory, `/data` and the SQLite file while nothing is running, then starts the new version. If any step fails it restores the previous halves and restarts the old process, so the operation is all-or-nothing from the API. A journal under `WT_STATE_DIR/.rollback` is written before the swaps; a supervisor crash mid-operation is rolled back on the next startup. The swap is not one filesystem transaction, so the journal is what makes a crash recoverable. If compensation itself fails, the API returns `500`, keeps the journal, and refuses another rollback for that app until a restart retries recovery.

`download` returns a portable `tar.gz` containing the snapshot's database and `/data` archive. `import` accepts that archive, validates any database with `quick_check`, and adds it to the app's tree as an **Imported** snapshot without making it current; import does not restore it. This is the offsite path: copy downloads to another host or object store on a schedule, and import them into a fresh installation when needed. Responses include each snapshot's `parent`, `kind` (`auto`, `manual`, or empty for one from before the tree index), `dataSize`, `hasData` and the `current` snapshot the live database follows. A `POST` that finds the app unchanged returns `created` empty.

## Database viewer

The admin panel's Database tab lists an app's user tables and pages through their rows. It is read-only: the table name is checked against `sqlite_master` and quoted before use, so the viewer cannot run arbitrary SQL or reach another app's database. SQLite's internal `sqlite_*` tables are hidden. Values are encoded like the app-facing storage API, and blobs are shown as their size rather than their base64 contents.

```text
GET  /apps/<name>/database
GET  /apps/<name>/database/rows?table=<table>&limit=50&offset=0
GET  /apps/<name>/database/changes?limit=200
GET  /apps/<name>/database/export                    download a consistent SQLite copy
POST /apps/<name>/database/import                     replace it with an uploaded SQLite file
POST /apps/<name>/database/maintenance                {"op": "vacuum" | "optimize" | "checkpoint" | "integrity_check"}
```

Rows default to 50 per page and accept `limit` up to 500. The rows response includes the columns, the page of rows, the table's total row count, and whether more rows remain. The overview also reports the schema version from `user_version`.

`export` uses SQLite's online backup API, so the download is consistent while the app keeps serving. `import` validates an upload with `quick_check` and restores it through the online backup API; take a snapshot first if the current data matters. The maintenance operations are the three things apps cannot ask for through raw SQL: `vacuum` compacts the file, `optimize` updates query-planner statistics, `checkpoint` truncates the WAL, and `integrity_check` reports `ok` or the first corruption lines. They are authenticated admin operations and time out after sixty seconds.

Every successful write sent through `/v1/exec`, `/v1/batch` or `/v1/transaction` is appended to the app's change log at `WT_STATE_DIR/.storage/<app>/changes.log`, and the Database tab shows the newest entries below the table. Each entry records its UTC time, SQL, bound parameters and rows affected. SQLite's pre-update hook also captures the affected rows, with old values for updates and deletes and new values for inserts and updates. The UI shows the operation, table, row id and per-column values, using `old → new` for updates.

Row capture is capped at 500 rows or 256 KiB per write operation; if that limit is reached, the entry is marked as truncated. Entries larger than 256 KiB keep the statement but replace the parameters with a marker, and if they are still too large they drop the row details. The log rotates at 1 MiB and keeps one rotated file. Failed statements are not logged, DDL has no row values, and page-level restores bypass SQL entirely.

## Bindings

The generated bindings under `packages/binding` target the world:

```wit
package wt:app;

world app {
  include wasi:cli/imports@0.3.0;
  include wasi:http/service@0.3.0;
}
```

They are **not tracked**: `packages/generate.sh` regenerates them from `packages/wit` with `componentize-go bindings`. The example apps' `build` commands run that script first, so a fresh clone builds on its first deploy.

The WIT comes from wasmtime v50's `crates/wasi-http/src/p3/wit` and the world file is ours. If you bump wasmtime, update the WIT and run the generator again; the export stub is written by the script.

The `net/http` adapter in `packages/wasihttp` is adapted from [spin-go-sdk v3](https://github.com/spinframework/spin-go-sdk) (Apache-2.0). The generated bindings and the adapter are what let an app keep normal `http.HandlerFunc` code.

## Limits of this prototype

- One `wasmtime serve` process per app. A crashed app restarts with exponential backoff (2s up to 30s), reusing its port and proxy route.
- `memory_mb` is a per-app linear-memory cap; CPU stays a container-level concern.
- `disk_mb` is a soft quota over `/data` and database files only. It reports `overQuota` but does not block writes; logs, backups and deployment bundles are outside it. `max_concurrency` and `request_timeout` bound proxy work; `request_timeout` also interrupts CPU-bound guest execution on epoch-enabled bundles. Health failures stop routing to an unready app and optionally restart it, but there is no fuel or percentage-of-CPU quota and no separate maintenance mode.
- TLS is optional and basic (certificate files); use the Orchard ingress for real certificates and wildcard hosts.
- Storage is a per-app SQLite SQL service, not a standard WASI import. The SQLite file is private to the supervisor; apps get a preopen at `/data` as well.
- Outbound network is granted to every app (`-S inherit-network`), including the loopback storage service.
- The deploy POST waits for the new version and retains its last ten bundles; there is no asynchronous deploy job or long-polling yet. Startup is fast enough that this is usually not noticeable.
- `.cwasm` is native code. Only load artifacts produced by your own deploy pipeline; the deploy endpoint is the trust boundary. The CLI and supervisor must agree on the wasmtime version and target triple; the runtime-only image is musl and accepts only `x86_64-unknown-linux-musl` artifacts. Mismatches are keyed out of the cache and rebuilt from `main.wasm` when the supervisor has a compiler. Same-digest deploys update static files without restarting the process, but the cache and deployment history still retain each bundle.
- The container runs as root and has no per-app CPU limits.
- `Pdeathsig` (Linux) stops app processes if the supervisor dies; adjust for other platforms.
