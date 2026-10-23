# Spin supervisor in one container

A single-container [Spin](https://spinframework.dev) runtime for several
applications. One `spin up` process runs per app, Caddy routes each hostname to
the app's loopback port, and the supervisor keeps every app's key-value store,
SQLite database and logs on a volume.

Applications are deployed as gzipped tarballs over a small HTTP API. There is no
container registry, no per-app container, and no Kubernetes.

This replaces the previous single-application image (`SPIN_APP`, `SPIN_WATCH`,
`/apps`). The old entrypoint is gone; an application now lives under
`/var/lib/spin/apps/<name>/` and is added with the deploy API.

## Build

```sh
podman build -t localhost/spin-supervisor .
```

The image contains Spin, Caddy and a small Go supervisor. It does not contain
any application.

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
- `/var/lib/spin` holds app bundles, state, logs and Caddy's generated config.

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
    Caddyfile
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

Build the app on the host, then upload it:

```sh
cp spin-container/app.example.json spin/app.json
$EDITOR spin/app.json   # set host and session_key

cd spin
spin build

tar -czf /tmp/gotime.tgz -C . spin.toml main.wasm app.json
curl -fsS -X POST "https://deploy.apps.example.com/apps/gotime" \
  -H "Authorization: Bearer $SUPERVISOR_TOKEN" \
  -F "artifact=@/tmp/gotime.tgz"
```

The tarball must contain `spin.toml` at its root. `main.wasm` and `app.json`
sit beside it. If an update omits `app.json`, the supervisor keeps the existing
one; for a new app it creates `{}`. Include every file the manifest references
(`component.files`, static assets and so on) in the tarball; the supervisor does
not run `spin build`.

The supervisor stops the old process, swaps the app directory, starts the new
one and lets Caddy reload its route. State under `state/<name>` is untouched.
The response is the app's current status:

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
supervisor rewrites `CADDY_CONFIG` and Caddy's `--watch` reloads it without
restarting the container or the other apps. The `DEPLOY_HOST` hostname is
reserved for the API and cannot be used by an app.

The Caddy admin endpoint stays on `CADDY_ADMIN` (default
`127.0.0.1:2019`). Do not publish it.

`GET /healthz` is routed to the supervisor API on any hostname, so an Orchard
HTTP health check can use container port `8080` and path `/healthz` without
knowing an app hostname. That path is reserved; an app's own `/healthz` is
shadowed externally (the supervisor still polls the app's `health` path
directly).

## Environment

| variable | default | purpose |
| --- | --- | --- |
| `SPIN_APPS_DIR` | `/var/lib/spin/apps` | one directory per app |
| `SPIN_STATE_DIR` | `/var/lib/spin/state` | per-app KV/SQLite and runtime config |
| `SPIN_LOG_DIR` | `/var/lib/spin/logs` | Spin component logs |
| `SPIN_CACHE_DIR` | `/var/lib/spin/cache` | Spin's component cache |
| `SPIN_TMP_DIR` | `/var/lib/spin/tmp` | upload and unpack temporary files |
| `SPIN_BIN` | `spin` | Spin executable |
| `CADDY_BIN` | `caddy` | Caddy executable |
| `CADDY_CONFIG` | `/var/lib/spin/caddy/Caddyfile` | generated Caddyfile |
| `CADDY_ADMIN` | `127.0.0.1:2019` | Caddy admin listener |
| `CADDY_DATA_DIR` | `/var/lib/spin/caddy/data` | Caddy data |
| `CADDY_CONFIG_DIR` | `/var/lib/spin/caddy/config` | Caddy autosave |
| `LISTEN` | `0.0.0.0:8080` | Caddy public listener |
| `API_LISTEN` | `127.0.0.1:9090` | supervisor API |
| `APPS_DOMAIN` | unset | default host suffix |
| `DEPLOY_HOST` | unset | host routed to the API |
| `SUPERVISOR_TOKEN` | unset | bearer token for deploys |
| `SPIN_STOP_TIMEOUT_SECONDS` | `10` | SIGTERM grace before SIGKILL |

## First iteration limits

This is deliberately small:

- **No blue/green or rollback.** A deploy stops the app, replaces its files and
  starts it again. Expect a short gap for that app.
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
