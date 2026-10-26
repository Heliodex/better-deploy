# wt

A prototype of a small Wasm deploy platform built directly on
[wasmtime](https://wasmtime.dev), with no Spin and no container image.

The point is to move the expensive part (compiling wasm to native code) into
the deploy artifact:

```text
wt-deploy:  build (componentize-go) → wasmtime compile → upload
supervisor: Caddy-less Go reverse proxy + one `wasmtime serve` per app
runtime:    a runtime-only wasmtime 50 loads the precompiled .cwasm
storage:    a per-app SQLite key-value service on loopback, plus /data via WASI
```

The first request for a precompiled Go component is tens of milliseconds on a
laptop. Nothing is compiled on the serving node, and the app is a standard
`wasi:http/service@0.3.0` component.

## Layout

- `example/` – the Go module for apps, generated WASI bindings and a small
  `net/http` adapter.
  - `example/hello` – returns the method and path.
  - `example/notes` – JSON notes persisted through the supervisor's storage service.
  - `example/binding` – generated bindings for `wt:app/app` (not tracked).
  - `example/wasihttp` – the `net/http` adapter (adapted from spin-go-sdk).
- `supervisor/` – runs apps, proxies by `Host`, deploys bundles blue/green.
- `deploy/` – builds, precompiles and uploads an app.

## Requirements

- Go 1.27.1 or newer. `componentize-go` is a Go tool in `example/go.mod`; it
  downloads the patched Go it needs for async components.
- wasmtime **50.0.0** on the machine running `wt-deploy` to compile the
  artifact. The precompiled artifact is tied to that version and target triple.
  The client and supervisor both default to `x86_64-unknown-linux-musl`, which
  is what the runtime-only image can execute; a different target needs a
  matching runtime build.
- The example WIT is the final `wasi:http@0.3.0` from wasmtime v50
  (`crates/wasi-http/src/p3/wit`), plus `wasi:cli/imports`.

## Build the supervisor and CLI

```sh
cd wt/supervisor && go build -o /tmp/wt-supervisor .
cd wt/deploy && go build -o /tmp/wt-deploy .
```

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
`main.wasm` with `wasmtime compile`, and uploads `app.json`, `main.wasm` and
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

`app.json` carries `name`, `host`, `health`, `env`, `memory_mb` and the local
`build` command. `memory_mb`, when set, becomes wasmtime's
`-W max-memory-size`. The runtime-only image has no compiler, so `main.wasm`
cannot run there: `main.cwasm` is required. If it is missing or incompatible,
the deploy fails and the old version keeps serving.

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
and URL through `WT_STORAGE_URL` and `WT_STORAGE_TOKEN`. The API is a small
key-value store:

```text
GET    /v1/kv/<key>          bytes, 404 if missing
PUT    /v1/kv/<key>          raw body (up to 1 MiB)
DELETE /v1/kv/<key>
GET    /v1/kv?prefix=<p>     JSON {"keys":[...]}
```

Every app gets its own database at `WT_STATE_DIR/.storage/<app>/storage.db`,
outside its `/data` preopen, so apps cannot touch the file directly. The
example module's `storage` package is the client; it takes an `*http.Client`,
and the apps pass `wasihttp.NewClient()` for outbound `wasi:http`.

The supervisor grants outbound network with `-S inherit-network` so apps can
reach that loopback endpoint. A production version would scope outbound
access per app.

## Bindings

The generated bindings under `example/binding` target the world:

```wit
package wt:app;

world app {
  include wasi:cli/imports@0.3.0;
  include wasi:http/service@0.3.0;
}
```

They are **not tracked**: `example/generate.sh` regenerates them from
`example/wit` with `componentize-go bindings`. The example apps' `build`
commands run that script first, so a fresh clone builds on its first deploy.

The WIT comes from wasmtime v50's `crates/wasi-http/src/p3/wit` and the world
file is ours. If you bump wasmtime, update the WIT and run the generator again;
the export stub is written by the script.

The `net/http` adapter in `example/wasihttp` is adapted from
[spin-go-sdk v3](https://github.com/spinframework/spin-go-sdk) (Apache-2.0,
see `example/wasihttp/LICENSE`). The generated bindings and the adapter are
what let an app keep normal `http.HandlerFunc` code.

## Limits of this prototype

- One `wasmtime serve` process per app. A crashed app restarts with exponential
  backoff (2s up to 30s), reusing its port and proxy route.
- `memory_mb` is a per-app linear-memory cap; CPU stays a container-level
  concern.
- TLS is optional and basic (certificate files); use the Orchard ingress for
  real certificates and wildcard hosts.
- Storage is a per-app key-value API, not general SQL. The SQLite file is
  private to the supervisor; apps get a preopen at `/data` as well.
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
