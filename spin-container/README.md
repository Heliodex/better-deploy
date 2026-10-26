# Spin supervisor in one container

A single-container [Spin](https://spinframework.dev) runtime for several
applications. One `spin up` process runs per app, Caddy routes each hostname to
the app's loopback port, and the supervisor keeps every app's key-value store,
SQLite database and logs on a volume.

Applications are deployed as gzipped tarballs over a small HTTP API. There is no
container registry, no per-app container, and no Kubernetes.

This replaces the previous single-application image (`SPIN_APP`, `SPIN_WATCH`,
`/apps`). The old single-app entrypoint is gone; the Go supervisor is the
container entrypoint. An application now lives under
`/var/lib/spin/apps/<name>/` and is added with the deploy API.

## Build

```sh
podman build -t localhost/spin-supervisor .
```

The image contains Spin and a Go supervisor with Caddy linked in. It does not contain any application. The supervisor is PID 1: on start it creates and chowns the volume directories, drops to uid 1000, then runs Caddy and the apps.

## Run

```sh
podman volume create spin-state
podman run -d --name spin-supervisor --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 \
  -v spin-state:/var/lib/spin \
  -e SUPERVISOR_TOKEN="$(openssl rand -hex 32)" \
  -e APPS_DOMAIN=apps.example.com \
  -e DEPLOY_HOST=deploy.apps.example.com \
  localhost/spin-supervisor
```

- `8080` is Caddy, the only published port.
- `APPS_DOMAIN` gives each app the default host
  `<name>.apps.example.com`. Without it, the default is `<name>.localhost`.
- `DEPLOY_HOST` makes Caddy route that hostname to the deploy API. Without it,
  the API stays on `127.0.0.1:9090` inside the container.
- `SUPERVISOR_TOKEN` is required for deploys. Treat it like a password.
- `/var/lib/spin` holds app bundles, state and logs.
- Start the image as root (the default) so the supervisor can chown the volume; it drops to uid 1000 before starting Caddy or any app. If your platform forces a non-root user, the volume must already be writable by uid 1000.

On Orchard, create one deployment with container port `8080`, a PVC mounted at
`/var/lib/spin`, and one ingress per app hostname (plus one for `DEPLOY_HOST`)
pointing at that port. TLS is terminated by Orchard, so Caddy serves plain HTTP.
An HTTP health check on port `8080` with path `/healthz` works without an app.
Start with something like 1 CPU and 1 GiB for a handful of small apps; each
idle `spin` process is tens of megabytes, and the container request is the
total budget.

## Application layout

```text
/var/lib/spin/
  apps/<name>/
    spin.toml
    main.wasm
    app.json          # optional
  state/<name>/
    runtime-config.toml
    kv.db
    sqlite.db
  logs/<name>/
  cache/
  caddy/
    data/
    config/
```

`app.json` is optional:

```json
{
  "host": "guestbook.apps.example.com",
  "health": "/",
  "env": {
    "session_key": "replace-with-a-random-secret"
  },
  "args": []
}
```

- `host` defaults to `<name>.<APPS_DOMAIN>` or `<name>.localhost`.
- `health` is the path the supervisor polls after starting the app. Any HTTP
  response counts as ready.
- `env` is passed to `spin up` as `-e KEY=VALUE`. This is how an app gets
  secrets. The container's environment is **not** forwarded.
- `args` is appended to `spin up`, for example
  `["--sqlite", "@/var/lib/spin/state/guestbook/migrations.sql"]`.

Every app needs a distinct host. The supervisor refuses a deploy whose host is
already used by another app.

## Deploy

Build the app on the host, then upload it. The app declares its scripts in
`spin/package.json`, mirroring the `celld` app:

```sh
cp spin-container/app.example.json spin/app.json
$EDITOR spin/app.json   # set host and session_key

cd spin
DEPLOY_HOST=deploy.apps.example.com SUPERVISOR_TOKEN="$SUPERVISOR_TOKEN" bun run deploy
```

`bun run deploy` runs the `spin-deploy` CLI (`spin-deploy/`), which builds,
packs `spin.toml`, `main.wasm` and `app.json` into a tarball, and posts it to
`https://$DEPLOY_HOST/apps/$APP` (`APP` defaults to the `spin.toml` application
name). `spin build` on its own only builds; there is no manifest-declared
deploy hook in Spin itself (`spin deploy` just forwards to a deployment
plugin).

The tarball must contain `spin.toml` at its root. `main.wasm` and `app.json`
sit beside it. If an update omits `app.json`, the supervisor keeps the existing
one; for a new app it creates `{}`. Include every file the manifest references
(`component.files`, static assets and so on) in the tarball; the supervisor does
not run `spin build`.

The supervisor starts the new version on a second loopback port while the old
process keeps serving, waits for the new one to answer its health path, then
switches the Caddy route and stops the old process. The response is sent only
once the new version is serving, and the old one stays up if it never becomes
ready. A changed component takes a while on first start because Spin compiles
the Wasm module; the deploy request waits for that (`DEPLOY_READY_TIMEOUT_SECONDS`,
default 180). State under `state/<name>` is untouched. The response is the app's
current status:

```json
{"name":"gotime","host":"gotime.apps.example.com","port":3101,"status":"running","pid":1234}
```

`GET /apps` lists the apps, and `GET /apps/<name>` returns one. These read
endpoints are meant for the loopback API or the Caddy route in front of it.

## Persistence

The supervisor writes a default runtime config for each app, backed by files on
the volume:

```toml
[key_value_store.default]
type = "spin"
path = "/var/lib/spin/state/<name>/kv.db"

[sqlite_database.default]
type = "spin"
path = "/var/lib/spin/state/<name>/sqlite.db"
```

The app's manifest must grant access:

```toml
[component.gotime]
key_value_stores = ["default"]
sqlite_databases = ["default"]
```

Back up `/var/lib/spin/state` (or the whole volume) to keep app data. The
generated config only defines the default stores; custom named stores are a
later iteration.

## Routing

Caddy listens on `LISTEN` (default `0.0.0.0:8080`), matches the request
`Host`, and proxies to the app's loopback port. Unknown hosts get a 404. The
supervisor builds the Caddy config in memory and applies it through Caddy's API,
without restarting the container or the other apps. There is no Caddyfile and no
admin listener. The `DEPLOY_HOST` hostname is reserved for the API and cannot be
used by an app.

`GET /healthz` is routed to the supervisor API on any hostname, so an Orchard
HTTP health check can use container port `8080` and path `/healthz` without
knowing an app hostname. That path is reserved; an app's own `/healthz` is
shadowed externally (the supervisor still polls the app's `health` path
directly).

## Environment

| variable | default | purpose |
| --- | --- | --- |
| `SPIN_VOLUME_ROOT` | `/var/lib/spin` | root the supervisor chowns when starting as root |
| `SPIN_APPS_DIR` | `/var/lib/spin/apps` | one directory per app |
| `SPIN_STATE_DIR` | `/var/lib/spin/state` | per-app KV/SQLite and runtime config |
| `SPIN_LOG_DIR` | `/var/lib/spin/logs` | Spin component logs |
| `SPIN_CACHE_DIR` | `/var/lib/spin/cache` | Spin's component cache |
| `SPIN_TMP_DIR` | `/var/lib/spin/tmp` | upload and unpack temporary files |
| `SPIN_BIN` | `spin` | Spin executable |
| `CADDY_DATA_DIR` | `/var/lib/spin/caddy/data` | Caddy data storage |
| `CADDY_CONFIG_DIR` | `/var/lib/spin/caddy/config` | Caddy config storage |
| `LISTEN` | `0.0.0.0:8080` | Caddy public listener |
| `API_LISTEN` | `127.0.0.1:9090` | supervisor API |
| `APPS_DOMAIN` | unset | default host suffix |
| `DEPLOY_HOST` | unset | host routed to the API |
| `SUPERVISOR_TOKEN` | unset | bearer token for deploys |
| `SPIN_STOP_TIMEOUT_SECONDS` | `10` | SIGTERM grace before SIGKILL |
| `DEPLOY_READY_TIMEOUT_SECONDS` | `180` | how long a deploy waits for the new version to answer its health path |

Only paths under `SPIN_VOLUME_ROOT` are chowned at startup. If you move any
directory outside it, make that directory writable by uid 1000 yourself.

## First iteration limits

This is deliberately small:

- **No rollback.** A deploy switches traffic only after the new version answers
  its health path, but there is no history to roll back to: deploy the previous
  build again. Both versions run at once for a few seconds around the switch,
  so they share the app's state directory and a request served by the old
  process may read `component.files` from the new directory.
- **No delete or restart endpoint.** Deploy again, or restart the container, to
  rescan `SPIN_APPS_DIR`.
- **No per-app CPU or memory cap.** The container's resource request is the
  total.
- **Default stores only.** Custom `runtime-config.toml` files are not merged;
  the supervisor generates the default KV and SQLite config.
- **Trusted app metadata.** `app.json` `args` and `env` are passed through to
  Spin. Do not let untrusted users upload arbitrary apps.
- **Minimal child environment.** The supervisor does not pass the container's
  environment to `spin`; app variables come only from `app.json`. Container
  secrets such as `SUPERVISOR_TOKEN` are therefore not inherited by app
  processes.
- **HTTP only at Caddy.** Terminate TLS at Orchard or another proxy. Caddy sees
  plain HTTP, so an app that compares the request scheme or `Origin` may need to
  trust forwarded headers or be adjusted.
- **Uploads are bounded** to 256 MiB compressed and 512 MiB extracted, with no
  symlinks or hard links in the tarball.

## Development

```sh
cd supervisor
go test ./...
go vet ./...
go build -o /tmp/spin-supervisor .
```

The supervisor has no third-party Go dependencies. The tests cover tar
extraction (including path traversal), app-name and host validation, listen
address parsing, default config and Caddyfile generation.
