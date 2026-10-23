# Spin in one container

A single-container [Spin](https://spinframework.dev) runtime. The application is
not in the image: mount a built app at `/apps` (or copy one into a volume there)
and the container serves it. Rebuilding the app restarts the runtime, so a
deploy is just `spin build` on the host.

The sibling `spin/` guestbook is the example used below. It is never copied into
the image.

## Build

```sh
podman build -t localhost/spin-runtime .
```

The image is about 125 MB. Almost all of that is the Spin CLI itself, which
embeds Wasmtime (163 MB unstripped, 113 MB after stripping). The rest is Alpine,
bash, CA certificates and `su-exec`.

The build fetches the static Linux release for the host architecture (`amd64` or
`arm64`), verifies it against the release's `checksums-<version>.txt`, and
strips it. Pin a release with `--build-arg SPIN_VERSION=v4.2.5`.

## Run

```sh
podman volume create spin-state
podman run -d --name spin --stop-timeout 20 \
  -p 127.0.0.1:3000:3000 \
  -v /path/to/built-app:/apps:ro \
  -v spin-state:/var/lib/spin \
  -e session_key="$(openssl rand -hex 32)" \
  localhost/spin-runtime
```

- `3000` — the HTTP trigger's listener (`SPIN_LISTEN`).
- `--stop-timeout 20` — time for Spin to finish its own graceful shutdown.
- `/var/lib/spin` — key-value/SQLite state, component logs and caches. Named
  volume or bind mount, or the data is tied to the container.
- `-e session_key=...` — passed to the components, because the entrypoint
  forwards the container environment (see below).

## Deploy the guestbook

Build on the host, then point the runtime at the build:

```sh
# in spin/
spin build

# in spin-container/
podman run -d --name spin --stop-timeout 20 \
  -p 127.0.0.1:3000:3000 \
  -v ../spin:/apps:ro \
  -v spin-state:/var/lib/spin \
  -e session_key="$(openssl rand -hex 32)" \
  localhost/spin-runtime
```

The mount is the whole project directory, but Spin only reads the files the
manifest references: `spin.toml` and `main.wasm` (the Svelte build and
`app.html` are embedded in the Wasm). `.spin/` and `frontend/node_modules/` are
ignored. The container never writes to `/apps`, hence `:ro`.

Because the entrypoint runs `spin watch --skip-build`, running `spin build`
again writes a new `main.wasm` and the runtime restarts the app against it, with
no container restart. State in `spin-state` survives rebuilds.

## Uploading an application

A bind mount is the shortest path when the app is built on the same host. To
copy an already-built app into the container instead:

```sh
podman volume create spin-apps
podman run -d --name spin --stop-timeout 20 \
  -p 127.0.0.1:3000:3000 \
  -v spin-apps:/apps \
  -v spin-state:/var/lib/spin \
  localhost/spin-runtime

# copy the wasm first, then the manifest, so the runtime does not reload while
# the component it points at is still missing
podman cp ../spin/main.wasm spin:/apps/main.wasm
podman cp ../spin/spin.toml spin:/apps/spin.toml
```

Until `/apps/spin.toml` exists, the entrypoint logs `waiting for an
application` and the container stays up. A half-copied app makes `spin watch`
fail; the entrypoint retries with a short backoff that doubles up to 30 s until
the app starts, so a bad upload does not kill the container.

`SPIN_APP` selects a different directory or manifest inside the container if
needed. `/apps` itself is deliberately not a `VOLUME`, so mounting the app is
explicit and the source of truth stays outside the container.

## Configuration

Everything is an environment variable, passed with `podman run -e`:

| variable | default | effect |
| --- | --- | --- |
| `SPIN_LISTEN` | `0.0.0.0:3000` | HTTP trigger listener; the only published port |
| `SPIN_APP` | `/apps` | Directory containing `spin.toml`, or the manifest itself |
| `SPIN_WATCH` | `1` | `1` reloads when app files change; `0` runs `spin up` once |
| `SPIN_STATE_DIR` | `/var/lib/spin/state` | Default key-value store and SQLite databases |
| `SPIN_LOG_DIR` | `/var/lib/spin/logs` | Component stdout/stderr, one file per component |
| `SPIN_CACHE_DIR` | `/var/lib/spin/cache` | Downloads of components and remote assets |

Arguments after the image name are appended to `spin up` / `spin watch`, for
example:

```sh
podman run ... localhost/spin-runtime \
  --runtime-config-file /apps/runtime-config.toml \
  --variable api_url=@/apps/api_url.txt
```

The entrypoint runs as uid 1000 (`spin`). When started as root it takes
ownership of the directories it writes to under `/var/lib/spin` and then
re-execs itself as that user, so a new volume does not have to be prepared by
hand.

### Environment variables

Spin gives components an empty environment unless values are passed with
`-e`, so the entrypoint forwards every variable in the container except the
`SPIN_*` controls above. That makes `podman run -e session_key=...` work as
expected, but it also means any secret in the container's environment is
visible to the application. Add unrelated settings with a runtime-config file or
a `.env`-style wrapper rather than leaving them in the run command.

## Worth knowing

- **One application per container.** Spin serves one app per process. Run
  another container with its own port and state volume for another app.
- **A reload is a restart.** `spin watch` stops the app and starts it again, so
  a request in flight can fail. There is no overlap between old and new
  versions.
- **The app directory must be readable by uid 1000.** That is the usual case for
  a `:ro` bind mount (0644 files, 0755 directories). If the app sits under a
  private home directory (0700), either loosen permissions or run with
  `--userns=keep-id:uid=1000,gid=1000`; the latter needs a state volume created
  under that mapping, since a volume made under the default uid mapping is not
  writable under a different one.
- **Changes outside the manifest and component sources are not watched.** A
  component that serves `component.files` is reloaded when those files change,
  but assets fetched at runtime are not. `podman restart spin` is the fallback.
- **State needs a volume.** Without `-v spin-state:/var/lib/spin` the image's
  anonymous volume keeps data across container restarts, but not across
  `podman rm`. The guestbook's messages live in the default key-value store
  under `state/`.
- **HTTP only.** Spin serves plain HTTP; terminate TLS in a reverse proxy.
- **Read-only `/apps` is supported and recommended.** Spin writes nothing there:
  state, logs and temp files all point into `/var/lib/spin`.
- **The image is not a builder.** `spin build` (Go, bun, `componentize-go`) runs
  on the host. `--skip-build` guarantees a manifest's build command never runs
  inside the container, even if the manifest was written for local development.
