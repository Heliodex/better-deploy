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
- The `example/guestbook` build also needs Bun. It runs `bun install` and `bun run build` in `example/guestbook/web`, then copies the static output into `example/guestbook/static`, which `wt-deploy` packs beside the component. `example/guestbook/contract/generate.go` turns the Valibot request and response schemas into JSON Schema and generated Go request and response types. `go generate ./contract/` runs it as part of the build; the generated schema and Go files are not tracked.
- wasmtime **50.0.0** is optional on the machine running `wt-deploy`: bundles always carry `main.wasm` and an `artifact.json` with the wasm digest, size, compiler version and target. An uploaded `main.cwasm` is only a precompiled hint and is accepted when its recorded runtime, target, epoch flag and wasm digest match. The supervisor keeps compiled modules in `WT_CACHE_DIR` keyed by those values, and can compile from `main.wasm` when `WT_WASMTIME_COMPILER` has the `compile` subcommand. The client and supervisor both default to `x86_64-unknown-linux-musl`, which is what the runtime-only image can execute; a different target needs a matching runtime build.
- The WIT in `packages/wit` is the final `wasi:http@0.3.0` from wasmtime v50 (`crates/wasi-http/src/p3/wit`), plus `wasi:cli/imports`.

## Build the supervisor and CLI

```sh
cd wt/supervisor && go generate ./... && go build -o /tmp/wt-supervisor .
cd wt/deploy && go build -o /tmp/wt-deploy .
```

For the local loop, `wt/supervisor/dev.sh` rebuilds the admin panel and binary and restarts the supervisor, reusing the running instance's app and state directories so deployed apps, data and logs survive. `./dev.sh --skip-generate` skips the panel build for a Go-only change.

## CLI

The `wt-deploy` binary also acts as the operator CLI. Every command accepts `--host`, `--token` and `--config`, and most accept `--json` for raw output. Defaults come from environment variables and then an optional `~/.config/wt/config.json` (or `WT_CONFIG`):

```json
{
  "host": "https://wt.example.com",
  "token": "scoped-deploy-token",
  "wasmtime": "/usr/local/bin/wasmtime",
  "target": "x86_64-unknown-linux-musl"
}
```

```sh
wt-deploy apps
wt-deploy logs notes --tail 200 --level warn --source app
wt-deploy logs notes --follow
wt-deploy deploy --dir ./notes --app-host notes.example.com
wt-deploy build --dir ./notes --out notes.tar.gz
wt-deploy rollback notes 20261101T120000.000000000Z
wt-deploy backup list notes
wt-deploy backup create notes
wt-deploy backup restore notes storage-20261101T120000.000000000Z.db
wt-deploy backup download notes storage-20261101T120000.000000000Z.db --out notes.tar.gz
wt-deploy config show notes
wt-deploy config set notes --data '{"memory_mb":256}' --if-match '"etag"'
wt-deploy open notes
wt-deploy completion bash
```

`build` and its `bundle` alias run the app build, componentize it, precompile when wasmtime is available and write the upload tarball; they are the local build path for the pinned Containerfile. `--force-build` bypasses the component-input cache, and deploys automatically use the static-only path when the server already runs the same wasm. `logs --follow` consumes the same SSE stream as the panel, so it works through the proxy. `config show` prints the ETag on stderr. `open` launches the app host with the platform opener; `--print` prints the URL instead. `completion` emits a static bash, zsh or fish script.

## Test

CI runs unit tests for the supervisor, deploy tool and shared packages, vets the wasm guests, checks both Svelte frontends, and has a separate end-to-end job. `wt/e2e.sh` downloads nothing itself: point it at a real wasmtime with the compile and serve subcommands. It builds the hello component, starts a supervisor on `127.0.0.1:18080`, deploys the bundle, proxies a request, checks that a spinning `?spin=1` request is cut off while the app stays up, takes and restores a `/data` snapshot, and rolls back code and data together:

```sh
WT_WASMTIME=/path/to/wasmtime ./wt/e2e.sh
```

Set `WT_E2E_TARGET` when the runtime uses a different target triple; the default is `x86_64-unknown-linux-gnu` to match the released wasmtime binary CI downloads. The script keeps its work directory on failure and prints the supervisor log.

The supervisor has fuzz targets for tar extraction, backup archives, the SQL guard and parameter decoding:

```sh
cd wt/supervisor
go test -run '^$' -fuzz FuzzExtractTarGz -fuzztime 30s
go test -run '^$' -fuzz FuzzBackupArchive -fuzztime 30s
go test -run '^$' -fuzz FuzzValidateStorageSQL -fuzztime 30s
go test -run '^$' -fuzz FuzzDecodeStorageValue -fuzztime 30s
```

`BenchmarkProxyLatency` measures the reverse proxy against a loopback backend, `BenchmarkStorage*`/`BenchmarkSQLite*` cover the storage query path, and `BenchmarkManyAppsSnapshot` covers roster snapshots with 200 apps. The e2e script also checks two apps, a host collision and supervisor restart; set `WT_E2E_EXAMPLES=1` to include the scaffolded app and guestbook, `WT_HANDOVER_TEST=1` to run the real `SIGHUP` handover test, `WT_VITE_TEST=1` to run the Vite dev proxy test, and run `wt/e2e-image.sh` against the musl Containerfile with podman.

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

`WT_DISK_BUDGET_MB` (or `--disk-budget-mb`, default `0` for unlimited) sets a global budget across `WT_APPS_DIR` and `WT_STATE_DIR`, including apps, logs, backups and retained deployment history. `GET /usage` reports the measured apps and state bytes, the total and the budget. When usage is already over the budget, a new deploy, snapshot or import is rejected with `507` instead of adding more data; existing data is not deleted, and a periodic monitor logs the transition. This is a guard, not a filesystem project quota.

The supervisor speaks plain HTTP by default; set `WT_TLS_CERT` and `WT_TLS_KEY` (or `--tls-cert`/`--tls-key`) to terminate TLS itself. Behind an ingress, leave TLS to the ingress, keep the supervisor on HTTP and set `WT_TRUSTED_PROXIES` (or `--trusted-proxies`) to the proxy IPs or CIDRs. Without that list the supervisor ignores inbound `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host`, so a client cannot spoof its address or the scheme. When the immediate peer is trusted, the supervisor takes the rightmost non-proxy `X-Forwarded-For` address as the client, accepts `X-Forwarded-Proto`, and uses `X-Forwarded-Host` for routing and as `Host`, which supports an ingress that rewrites `Host`. It replaces `X-Forwarded-For` with the resolved client and immediate trusted proxy, sets `X-Real-IP`, drops the `Forwarded` header, and sets `X-Request-Id` (generating one when absent), `spin-full-url` and `spin-client-addr` from that trusted view. The proxy connects within five seconds and waits at most thirty seconds for response headers, so a hung app returns `502` instead of holding the client connection open. An app can set `max_concurrency` to cap simultaneous proxied requests (extra requests get `429`) and `request_timeout` to interrupt a request that runs too long, including a spinning guest.

Every app is probed at its `health_interval` (default ten seconds, three-second probe timeout), and the supervisor reports `running`, `unhealthy`, `restarting` or `stopped`. `/healthz` is the default health path. `health_failures` consecutive failures mark the app unready: component traffic gets `503` with `Retry-After` while probing continues, and a later success clears the error, restores routing and updates `lastHealthy`. `ready` appears in `GET /apps`, the SSE snapshot and the admin panel. With `restart_on_unhealthy` set, crossing the threshold kills the process once and the normal restart backoff applies; otherwise health state does not stop or restart it. `startup_timeout` (default 15s) bounds how long a new process may take to answer its health path after the explicit migration phase. The supervisor calls the app's reserved `POST /_wt/migrate` endpoint once the process is listening and before the health probe, with `WT_STORAGE_MIGRATION_TIMEOUT` as its budget, so a slow schema change does not count against `startup_timeout`; bundles without the endpoint fall back to health-only readiness and should be redeployed to get the phase.

Restart, stop, delete, deploy and rollback stop routing new requests, drain in-flight proxied requests for up to ten seconds and then stop the process. On `SIGINT`/`SIGTERM` the supervisor calls `http.Server.Shutdown` with a fifteen-second deadline, then drains apps and stops their processes. On `SIGHUP` it writes the storage and app-fetch listener FDs, the app runner liveness FD and a handover marker into the environment and `exec`s the binary at its own `os.Args[0]` path with the same arguments. The new supervisor reconnects to the app runner, which still owns every wasmtime process, restores the app tokens and adopts the apps from the runner snapshot; app PIDs do not change. The storage and app-fetch listeners are inherited so the apps' existing URLs keep working; the public and admin listeners rebind in the new process, which leaves a short accept gap, and in-flight requests on the old public/admin and storage servers are dropped. Replace the binary at the path you originally started, and do not start a deploy during the handover; the app locks are not drained. This is not crash recovery: if the supervisor dies without `exec`, the runner's liveness pipe closes, the runner stops the apps and exits, and a cold start begins from disk. Replaced processes have their log files closed instead of leaving them to garbage collection.

The supervisor scans `WT_APPS_DIR` at boot, starts every app found there, and serves `GET /apps`, `GET /apps/<name>` and `POST /apps/<name>` (deploy, bearer token). `/healthz` is the supervisor's own health check, and it is also the default component health path on an app host. These supervisor paths, including `/admin/` and `/events`, only exist on hosts that are not app hosts: an app owns every path on its own host, so requests to those names are proxied to the app rather than swallowed by the supervisor. The supervisor's own host always wins over an app route. It reserves `localhost`, IP literals, the configured app domain root and the hostname in `WT_LISTEN`/`WT_ADMIN_LISTEN`, so an app cannot shadow `/admin` by claiming `localhost` or `127.0.0.1`. Exact app hosts are matched first, then one single-label wildcard such as `*.apps.example.com`; the wildcard needs a dotted suffix, so `*.com` and `*.localhost` are rejected.

## Admin panel

The supervisor serves a small admin panel at `/admin/` on its own address (for example `http://127.0.0.1:8080/admin/` locally, or the deploy host in production). It lists apps with status, uptime and disk usage, streams each app's log live over server-sent events with level and text filters, restarts apps, starts and stops them, edits their runtime config, deletes them with an optional state purge, uploads new bundles, configures and restores database and `/data` backups, browses each app's database tables and recent writes, and shows the retained deployment history with a rollback action. Updates stream over server-sent events, so the dashboard reflects deploys and restarts without refreshing. Every API endpoint except `/healthz` requires an operator or scoped bearer token, which the panel asks for once and keeps in the browser. The static panel shell at `/admin/` is public so the browser can load the token form; it contains no secrets. `POST /login` validates it and sets `wt_token` as an `HttpOnly`, `SameSite=Lax` cookie (also `Secure` when the request is HTTPS or arrives with `X-Forwarded-Proto: https` from a trusted proxy), so event streams authenticate without JavaScript reading the cookie; `POST /logout` clears it. Ordinary panel requests still send a bearer header. The token is also kept in `localStorage` so a reload can re-authenticate, which means a script injection on the admin origin can still read it; scoped tokens, the audit log and login rate limiting are described below. In trusted mode the supervisor refuses to start on a non-loopback address without `WT_TOKEN`, so a loopback-only listener is the only unauthenticated mode. In untrusted mode it refuses to start at all without `WT_TOKEN` and rejects every admin route without it. A reverse proxy on the same host makes remote requests look like loopback, so do not expose the loopback listener through a proxy without setting `WT_TOKEN`; use `WT_ADMIN_LISTEN` to separate admin traffic when apps are untrusted. App logs persist under `/var/lib/wt/.logs` (or `WT_STATE_DIR`), outside each app's `/data` preopen, and every line is stamped with the time the supervisor received it so timestamps do not depend on how an app logs. The supervisor also logs each proxied request at info level into a separate per-app supervisor log, as `METHOD status duration path`; the panel recognises the shape and lays it out as structured columns with each line badged by origin.

## Authentication and access tokens

`WT_TOKEN` (or `--token`) remains the bootstrap operator token and has the `admin` role for every app. It is enough for a single operator, but it should not be the credential in CI: create scoped tokens instead. Scoped tokens are stored in `WT_AUTH_FILE` (or `--auth-file`, default `WT_STATE_DIR/.auth/tokens.json`), directory `0700`, file `0600`, and only a SHA-256 hash of each bearer value is kept. The generated bearer is returned once by the create call and never listed again. Corrupt or unreadable auth state fails startup rather than silently dropping tokens.

Roles:

- `admin` may use every route and app.
- `deploy` may read app status, logs, resources, config and deployments, and may deploy, roll back code, restart, stop and start the apps in its `apps` list. It cannot change config, read or write secrets, touch backups or databases, or delete an app. A rollback with `?backup=` is admin-only because it replaces data.
- `read` may use the non-sensitive `GET` app routes for its apps, including status, logs, resources and deployments. Database contents, backups and secrets stay admin-only.

A token's `apps` list contains app names, or `*` for every app. Admin tokens always imply every app. Scoped tokens are independent of the per-app storage token and the internal app-fetch token.

```text
GET    /auth/tokens            list token metadata, never bearer values
POST   /auth/tokens            {"name":"ci-notes","role":"deploy","apps":["notes"],"expires_in":"720h"}
DELETE /auth/tokens/<id>       revoke one token
GET    /audit?limit=200        newest audit entries, admin only
```

`expires_in` is a Go duration between `1m` and ten years; omit it or use `0`/`never` for no expiry. `POST /auth/tokens` returns the bearer once as `{"token":"...","id":"...","name":"...","role":"...","apps":[...],"createdAt":"...","expiresAt":"..."}`. Store it in the CI secret store and use it as `Authorization: Bearer <token>`. The panel cookie accepts any valid token; an EventSource stays admin-only because it streams every app.

`/login` now accepts the bootstrap token or any scoped token and is rate-limited per remote host: five failed attempts in a minute lock that host out for a minute. The limiter is in memory and resets on restart. The cookie is still `HttpOnly`, `SameSite=Lax` and `Secure` behind HTTPS.

Mutating app requests are appended to `WT_STATE_DIR/.audit/audit.log` (`0700` directory, `0600` file, JSON lines, one rotation at 16 MiB). Entries record the time, actor name, role, action, app, status, whether it succeeded, a redacted error, the remote host and `X-Request-Id` when present. Deploy, rollback, backup restore/import, database import/maintenance, config changes, secret changes, lifecycle actions and app deletion are covered; token create and delete are logged too. The audit log is on the same disk and is not tamper-evident or shipped offsite, and a failed audit write is logged but does not roll back the action.

This is deliberately a single-operator model: there are no owners, namespaces or per-person quotas. Isolation for untrusted apps, per-app storage tokens and the internal app-fetch identity remain separate and are still enforced.

## End-user identity

Platform users are separate from operator tokens. An app opts in with `"identity": "platform"` in `app.json`; it then gets a login, logout, `GET /v1/me` and password-change surface on its own host, while other apps stay anonymous. The supervisor keeps users and sessions under `WT_STATE_DIR/.identity` (`0700`, `0600`) and stores only PBKDF2-SHA256 password hashes. Sessions are random tokens in an `HttpOnly`, `SameSite=Lax` `wt_user` cookie with a 30-day lifetime; logout and password change require the `X-CSRF-Token` returned by login or `me`.

```text
POST /identity/users                     admin: {"email","name","password","groups"}
GET  /identity/users                     admin: list users, never password hashes
GET  /identity/users/<id>                admin: one user
PATCH /identity/users/<id>               admin: {"name","groups","disabled"}
POST /identity/users/<id>/password       admin: {"password"} reset, revokes sessions
DELETE /identity/users/<id>              admin: remove the user and sessions

POST /_wt/identity/login                 app host: {"email","password"}
POST /_wt/identity/logout                app host: requires CSRF
GET  /_wt/identity/me                    app host: {"user":{...},"csrfToken":"..."}
POST /_wt/identity/password              app host: {"currentPassword","newPassword"}
```

`POST /v1/login`, `POST /v1/logout` and `GET /v1/me` are aliases on a platform app host, so a SvelteKit page can use the short paths. Those three paths are reserved while identity is enabled. There is no email delivery, so password reset is an admin action rather than a self-service link; add a mail provider before exposing one.

On every proxied request to a platform app, the supervisor strips any client-supplied `X-WT-User-*` headers and the `wt_user` cookie, then injects `X-WT-User-Id`, `X-WT-User-Email`, `X-WT-User-Name`, `X-WT-User-Groups` and a `X-WT-User-Signature` HMAC. The app verifies them with `app.UserFromRequest(r)` using `WT_IDENTITY_SECRET`; the key is derived per app, so one app cannot forge another app's user headers. Apps that do not opt in receive neither the user headers nor the session cookie.

## Observability

`GET /metrics` (admin only) returns hand-written Prometheus text: `wt_requests_total{app,method,status}`, request duration sum/count, `wt_inflight_requests`, storage operation counts and duration by read/write pool and result, deploy/backup/job outcome counters, and per-app `wt_app_cpu_percent`, `wt_app_memory_bytes`, `wt_app_ready` and `wt_app_disk_bytes`. It has no background scrape goroutine and opens no network listener beyond the admin API.

Proxied requests get an `X-Request-Id` when the caller did not send one, and the supervisor access line appends `request=<id>`. Access logging is controlled by `WT_ACCESS_LOG` (default on) and `WT_ACCESS_LOG_EVERY=N` (default 1); sampled lines are skipped but 5xx lines are always kept. The panel and SSE streams still work with access logging disabled. The app receives the same header and can forward it to storage; the storage change log records it as `requestId` when present. App logs support server-side `level`, `source`, `q`, `since` and `until` filters on the tail and SSE stream. `GET /apps/<name>/logs/export` returns all retained matching lines as text or JSON lines. A `wt:log` WIT import, a full log index and sampling are still future work; log files remain capped and rotated.

## Untrusted tenant mode

`WT_TENANT_MODE` selects the trust model. `trusted` (the default) keeps the personal-app behaviour described elsewhere in this document. `untrusted` treats every app as hostile code and refuses to start an app when a required isolation primitive is unavailable. The supervisor itself refuses to start in untrusted mode without a token, and refuses to start at all with an unknown mode.

```sh
sudo WT_TENANT_MODE=untrusted \
  WT_TOKEN='a long random token' \
  WT_ADMIN_LISTEN=unix:/run/wt/admin.sock \
  WT_APPS_DIR=/var/lib/wt/apps \
  WT_STATE_DIR=/var/lib/wt \
  WT_CGROUP_ROOT=/sys/fs/cgroup/wt \
  WT_IPTABLES=iptables \
  WT_IP6TABLES=ip6tables \
  /tmp/wt-supervisor
```

Untrusted mode requires a non-empty `WT_TOKEN`; there is no tokenless loopback admin path. `WT_ADMIN_LISTEN` (or `--admin-listen`) separates the admin API from app traffic: it accepts a TCP address such as `127.0.0.1:8081` or `unix:/run/wt/admin.sock`. When it is set, `/apps`, `/admin`, `/events`, `/login` and `/logout` exist only on that listener; the public listener proxies app hosts and serves `/healthz` only. A Unix socket has any stale socket removed, is created `0600`, and is removed on shutdown. With no `WT_ADMIN_LISTEN`, the routes stay on the public listener, but untrusted mode still requires the token for every admin API route; the static panel shell can load without one so the login form works.

Requirements and failure behaviour:

- **Root.** The supervisor must run as root so it can create the app namespaces, set the mapped uid, chown `/data`, populate cgroups and apply iptables rules. If it is not root, untrusted apps refuse to launch.
- **App namespaces.** `WT_APP_NAMESPACES` (default on in untrusted mode) runs each app in a user, PID, mount, IPC and UTS namespace. The re-exec helper maps container uid/gid 0 to the app's host uid/gid, builds a tmpfs root containing only the bundle at `/app`, `/data`, `/proc`, `/tmp` and the files the runtime needs, pivots into it and then drops capabilities. The host filesystem, other apps' state and the supervisor's secrets are not visible. The helper stays as PID 1 only to forward signals and reap the wasmtime child; the child's host pid is reported over a pipe so app info and resource charts describe wasmtime rather than the helper. Cgroup membership and rlimits are inherited by both.
- **App seccomp.** `WT_APP_SECCOMP` (default on in untrusted mode) installs a denylist filter before wasmtime starts. It rejects ptrace, process_vm_*, kcmp, bpf, perf_event_open, userfaultfd, key management, mount/umount/pivot_root, setns/unshare, module and kexec loading, reboot, swap, syslog, quota and clock changes, the new mount API, `prctl(PR_SET_SECCOMP)` and `prctl(PR_SET_MM)`. This is defence in depth: the namespace and dropped capabilities remain the primary boundary, and the filter is a denylist rather than a maintenance-heavy allowlist. Both settings are mandatory in untrusted mode; `WT_APP_NAMESPACES=off` or `WT_APP_SECCOMP=off` makes the supervisor refuse to start. Trusted mode keeps both off by default and accepts `--app-namespaces`/`--app-seccomp` only when the supervisor is not root, because a root trusted supervisor has no per-app uid to map.
- **Kernel support.** The clone and mount calls can be blocked by a container's seccomp profile, AppArmor policy or a kernel with unprivileged user namespaces disabled. A failed namespace, mount or seccomp install aborts the launch and keeps the previous version; there is no fallback to a weaker mode. The container needs `SYS_ADMIN` in addition to the existing `SETUID`, `SETGID` and `NET_ADMIN` for the mount operations, and `WT_STATE_DIR/.ns` holds the empty host directories that become each helper's tmpfs root.
- **cgroup v2.** A writable cgroup v2 hierarchy must contain `cpu`, `memory`, `pids` and `io`. `WT_CGROUP_ROOT` defaults to `/sys/fs/cgroup/wt`; if its `cgroup.controllers` is empty because the parent has not delegated the controllers yet, the supervisor enables them in the parent's `cgroup.subtree_control` and re-reads before failing. Missing controllers, an unwritable root or a failed limit write is fatal in untrusted mode. Trusted mode does not create per-app cgroups at all, so its previous unlimited behaviour is preserved.
- **iptables and ip6tables.** `WT_IPTABLES` (default `iptables`) must be able to create the `WT-EGRESS` chain and an `OUTPUT` jump, and `WT_IP6TABLES` (default `ip6tables`) must create `WT-EGRESS6`; every command runs with `-w`. In untrusted mode a `network: storage` app whose IPv4 or IPv6 rules cannot be installed is stopped. IPv6 is rejected for the app UID because the storage service is IPv4-only. If the host genuinely has IPv6 disabled, set `WT_IPV6_EGRESS=off` (or `--ipv6-egress=false`) as an explicit opt-out; the supervisor logs a warning and does not silently skip a failed ip6tables invocation.

Per-app identity and limits:

- `WT_APP_UID_BASE` (default `20000`) is the first UID/GID. A persistent allocator at `WT_STATE_DIR/.uids.json` gives each app a stable dedicated UID, resolves collisions on load and survives restarts. The app's `/data` tree is chowned to that UID before launch, and the UID appears as `uid` in app info and `GET /apps/<name>`.
- Each app gets `WT_CGROUP_ROOT/<name>` with `cpu.max` derived from `cpu_millicores` (period `100000`), `memory.max` from `memory_mb`, `memory.swap.max=0`, `pids.max` and `io.weight` (`default N`). The process is added after start; on stop the supervisor writes `1` to `cgroup.kill` when available, then removes the cgroup, and it also removes the cgroup after a failed launch.
- After start the supervisor applies `RLIMIT_NOFILE=1024`, `RLIMIT_FSIZE=<disk_mb> MiB`, and `RLIMIT_CORE=0` with `prlimit`, only in untrusted mode. A failure is fatal. `memory_mb` also remains wasmtime's `-W max-memory-size`.
- Before an untrusted launch the app bundle is made readable by its UID: the app directory and its subdirectories become `0755`, regular files become `0644`, and existing execute bits are preserved. The root `app.json` is the exception: it contains env secrets and becomes `0600`. `WT_APPS_DIR` and `WT_STATE_DIR` keep their existing permissions but gain execute bits (`0711` if they were `0700`), so the app UID can traverse to its bundle and `/data` without being able to list those parent directories. `main.cwasm` is also chmodded `0644` when installed from the cache or an upload.
- `cpu_millicores`, `pids_max` and `io_weight` are accepted in `app.json`, `PATCH /apps/<name>/config` and the config response; defaults are 1000, 128 and 100, but they are enforced only in untrusted mode.

Untrusted defaults for omitted fields are finite: `request_timeout=30s`, `memory_mb=256`, `disk_mb=256`, `cpu_millicores=1000`, `pids_max=128`, `io_weight=100` and `network=storage`. An explicit `request_timeout` of `0` or `none` disables the deadline. Trusted mode keeps its old defaults: no memory, disk or request timeout unless configured, and `network=inherit`.

Network modes:

- `storage` is the untrusted default. The app still receives `-S inherit-network` so it can reach the loopback storage service and the internal app-fetch listener, but a per-UID iptables rule set allows only TCP to those two addresses, configured `egress` CIDR/port rules, owner-scoped replies from the app's own listening port back to loopback, and rejects every other IPv4 packet from that UID. A separate `WT-EGRESS6` ip6tables chain rejects all IPv6 traffic from the UID, since storage is IPv4-only. Rules are installed after the app port is known and before readiness, and a failure in either family is fatal.
- `none` passes no `inherit-network`, needs no firewall rules and cannot use storage.
- `inherit` is rejected in untrusted mode. Trusted mode keeps it, and a trusted `storage` app (which has no dedicated UID) logs a warning and is not restricted.

Disk enforcement changes with the mode. Trusted `disk_mb` remains a soft report. In untrusted mode a monitor periodically measures `/data` plus the app's SQLite files, and when usage exceeds the byte quota it stops the app, records the reason in `lastError` and does not restart it automatically. The same monitor enforces a global count of 100,000 entries per app across `/data`, its directories and its `storage.db*` files, so empty files cannot exhaust inodes; exceeding it stops the app with a `file count quota exceeded` error. `RLIMIT_FSIZE` is the per-file backstop. The soft report (`diskBytes`, `diskLimitBytes`, `overQuota`) still appears in app info; entry counts are not yet exposed there.

`WT_DATA_QUOTA_MODE=setquota` (or `--data-quota-mode=setquota`) adds a real per-UID filesystem quota instead of relying on the monitor alone. It requires user or group quotas enabled on the filesystem that holds `WT_STATE_DIR`, plus the `setquota` and `findmnt` binaries; `WT_SETQUOTA` and `WT_FINDMNT` override their paths, and `WT_DATA_QUOTA_FS` (`--data-quota-fs`) names the mount point explicitly instead of resolving it once with `findmnt -n -o TARGET --target <WT_STATE_DIR>`. The container image is `scratch` and includes neither tool nor any host quota configuration, so this mode is for host installs or an image that adds them. The app UID gets `disk_mb` as equal soft and hard block limits and `maxAppEntries` (100,000) as equal inode limits. A quota command failure aborts the launch, and a `disk_mb` change returns `500` without saving the config, rather than silently falling back to the monitor. The default remains `monitor`, with the byte/entry monitor and `RLIMIT_FSIZE`; in `setquota` mode both stay active as a second line of defence. Selecting `setquota` in trusted mode only logs a warning, because trusted apps have no dedicated UID and never receive a quota.

A launch that fails after the process started removes the process, its cgroup and its firewall rules before returning the error.

Remaining limitations of this prototype:

- SQLite still runs **in the supervisor process**, not in a separate storage process. Each connection now uses `temp_store=FILE`, a 2 MiB page cache, `mmap_size=0`, SQL-length/value/row limits, a global 16-query semaphore and a pre-update row capture that checks size before base64 encoding. That reduces the blast radius but is not full control-plane isolation; the complete fix is a separate storage process with its own limits.
- Forwarded headers are trusted only from `WT_TRUSTED_PROXIES`. A trusted proxy must replace client-supplied `X-Forwarded-*` headers rather than appending to them, and `WT_TRUSTED_PROXIES` must not include client networks. There is no per-proxy hop limit or PROXY protocol support.
- There is still no network namespace. The user, PID, mount, IPC and UTS namespaces are in place, but the app keeps the host network namespace so the supervisor can reach its wasmtime listener; the iptables owner rules remain the only network boundary. A network namespace needs a veth pair, address and route setup, and a way for the supervisor to reach the listener, or a proxy inside the namespace. Storage, the internal app-fetch listener and configured egress CIDRs are part of the allowed boundary; the internal listener authenticates every call and the target allowlist still applies. A compromised kernel or an allowed endpoint remains part of the trust boundary.
- The seccomp filter is a denylist, not an allowlist, and the namespaces do not remove the need for the supervisor to run as root in untrusted mode. A kernel or container policy that blocks `CLONE_NEWUSER`, `CLONE_NEWPID` or the mount calls makes untrusted launches fail closed; there is no weaker fallback. Trusted apps can opt into the namespaces only when the supervisor is non-root, because there is no per-app uid to map.
- Data is not encrypted at rest, including `.secrets`, `.auth` and `.audit` files. The bootstrap `WT_TOKEN` still deploys and reads every app; prefer scoped tokens for CI. Scoped tokens, their hashes and the audit log live on the same host, and the audit log is not tamper-evident or shipped offsite. Log redaction cannot scrub existing files or transformed values.
- App ports are dynamic and the proxy container is still expected to run as root (see the container notes).

## Resource usage

The app detail charts CPU and resident memory for the `wasmtime serve` process, read from `/proc/<pid>/stat` and `/proc/<pid>/statm`. For a namespaced app the supervisor samples the wasmtime child's host pid, which the helper reports over a pipe, not the PID 1 helper. CPU is a percentage of one core and can exceed 100% for a multi-threaded component; memory is the process's resident set size. The supervisor samples every second and keeps thirty minutes of history, which the panel fetches from `GET /apps/<name>/resources` and draws as two charts; live figures arrive in the same SSE snapshots, so the charts keep moving without polling. History is persisted under `WT_STATE_DIR/.resources` every ten seconds and on shutdown, so it survives restarts with at most a few seconds lost. A stopped app records zeros, and a container without a mounted `/proc` shows memory as `–`.

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

`wt-deploy` reads `app.json`, runs its `build` command, precompiles `main.wasm` with `wasmtime compile` when available, and uploads `app.json`, `main.wasm`, `artifact.json`, `main.cwasm` when it exists, and any `static/` files as a gzipped tarball. It records a component-input hash in `.wt-build.json`; when the hash and `main.wasm` are unchanged, the build and compile steps are skipped. If the running app already has that wasm digest, the client sends `X-WT-Static-Only: 1` with only `app.json` and `static/`, and the supervisor reuses its current component. `--force-build` overrides the cache, and uploads print a coarse progress percentage. `main.wasm` is the artifact; `main.cwasm` is a cache hint. `artifact.json` records the wasm digest and size, the wasmtime version, target triple, baseline CPU policy and compile time. `--skip-compile` does not require a cwasm and warns when none is included, leaving the supervisor to compile from wasm if it has a compiler. `--app-host` overrides the host written into the uploaded `app.json`, which is useful when the supervisor sits behind a real ingress domain. A deploy with the same wasm digest and unchanged runtime config keeps the running process and replaces only `app.json` and `static/`, so a frontend-only change does not restart the app. Otherwise the supervisor starts the new version on a new loopback port while the old one serves, waits for a 2xx health response, swaps the host route, drains in-flight requests for up to ten seconds and then stops the old process. Responses include `X-WT-Wasm-Digest` and `X-WT-Deploy-Mode: full|static`. A deploy also returns an `ETag` over the bundle contents: send it back as `If-None-Match` to get `304 Not Modified` without installing, and send `Idempotency-Key` to replay the same response for 24 hours. The response is stored beside the app's deployment history at `WT_STATE_DIR/.deployments/<app>/idempotency.json` (`0600`), survives supervisor restarts, and keeps the newest 64 keys per app; a delete clears the file. `POST /apps/<name>/validate` is the explicit validation form of `?dry_run=1`, and `GET /apps/<name>/deployments/<id>` returns one history entry with its status, error and current marker. Rollback remains synchronous, and `POST /apps/<name>` remains the synchronous compatibility alias.

`POST /apps/<name>/deployments` is the deployment resource. It uploads and validates the bundle synchronously, then returns `202 Accepted` with `Location: /apps/<name>/deployments/<id>` and installs in the background. `GET` on that URL returns `queued`, `running`, `success`, `failed` or `canceled` with queue/start/finish timestamps, digest, mode and error; `POST .../cancel` cancels a queued run or requests cancellation of a running one; `GET .../logs` returns the plain-text log, and `GET .../logs/stream` follows it with SSE and ends with a `done` event carrying the terminal status. Deployment logs live beside the entry and are capped at 1 MiB. Queued and running entries are never pruned, and `If-None-Match` still returns `304` before anything is queued.

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

The bootstrap operator token and admin-scoped tokens guard the lifecycle and config endpoints; deploy and read tokens follow the role restrictions described above. A hand-maintained OpenAPI document lives at `wt/openapi.yaml`; generated clients are not wired in yet:

```text
GET    /usage                  measured disk usage and the global budget
GET    /audit                  newest audit entries, admin only
GET    /auth/tokens            list scoped tokens, admin only
POST   /auth/tokens            create a scoped token, admin only
DELETE /auth/tokens/<id>       revoke a scoped token, admin only
GET    /apps/<name>/config      runtime config, with env and secret keys but not values
PATCH  /apps/<name>/config      partial config update
GET    /apps/<name>/secrets     write-only secret names, never values
PUT    /apps/<name>/secrets/<key> set or rotate one secret
DELETE /apps/<name>/secrets/<key> remove one secret
GET    /apps/<name>/deployments list retained deployment bundles and failed attempts
GET    /apps/<name>/deployments/<id> one history entry with status and error
POST   /apps/<name>/validate    validate an uploaded bundle without installing it
POST   /apps/<name>/jobs        enqueue a durable job
GET    /apps/<name>/jobs        list jobs, optionally ?status=pending
GET    /apps/<name>/jobs/<id>   one job
POST   /apps/<name>/jobs/<id>/cancel stop a pending or running job
POST   /apps/<name>/jobs/<id>/retry  retry a failed, dead or canceled job
GET    /apps/<name>/logs        filtered tail (?level=&source=&q=&since=&until=&tail=)
GET    /apps/<name>/logs/export download all retained lines (?format=text|jsonl and the same filters)
GET    /metrics                 Prometheus metrics, admin only
POST   /apps/<name>/rollback/<id> install a retained code bundle
       ?backup=<file>             also restore that snapshot's database and /data
POST   /apps/<name>/stop        stop the process, keeping the app and its host
POST   /apps/<name>/start       relaunch a stopped app
DELETE /apps/<name>             remove code and process; keep state
DELETE /apps/<name>?purge=1     also remove the database, backups, logs, history and /data
POST   /apps/<name>?dry_run=1   validate an uploaded bundle without installing it
```

`PATCH` accepts any of `host`, `hosts`, `health`, `env`, `memory_mb`, `disk_mb`, `cpu_millicores`, `pids_max`, `io_weight`, `max_concurrency`, `job_concurrency`, `request_timeout`, `startup_timeout`, `health_interval`, `health_failures`, `restart_on_unhealthy`, `network`, `allow_callers`, `egress` and `schedule`. `GET` and `PATCH` return an `ETag` for the runtime config; send `If-Match` to reject a stale PATCH with `412`, or `If-None-Match` on `GET` to receive `304`. Env changes are a merge; a `null` value removes a key. `GET` returns `env_keys` and `secret_keys` rather than values, so secrets are never echoed back. Host, host aliases, health, allowed callers and schedule changes take effect immediately; env, memory, disk, CPU, pids, IO, concurrency, timeout, network and egress changes are saved to `app.json` and applied on the next start or restart, which the response reports as `restart_required`. Host changes update routing at once, and are rejected with `409` if another app already owns the host, an alias, or an overlapping wildcard.

Secrets live in a write-only per-app store under `WT_STATE_DIR/.secrets/<app>.json` (directory `0700`, file `0600`, atomic replacement) instead of `app.json`. `GET /apps/<name>/secrets` returns only `{"keys":[...]}`. `PUT /apps/<name>/secrets/<key>` takes `{"value":"..."}`, and `DELETE` removes the key; both return `{"key":"...","restart_required":true}` and never echo the value. Keys are environment-style identifiers (`[A-Za-z_][A-Za-z0-9_]*`, at most 128 bytes); values are at most 64 KiB and cannot contain NUL. The body limit is 80 KiB. The secret endpoints use the same admin token and app scope as the rest of `/apps`, and a missing app returns `404`.

In `app.json`, an env value of `secret://NAME` resolves from that app's store at launch. Literal values pass through unchanged. A missing secret is a clear launch error and the app is not started, so the previous version keeps serving. The bundle and deployment history keep the reference, never the resolved value. Each deploy or rollback records only `secret://NAME` because `app.json` is copied as uploaded; the resolved values exist only in the supervisor's memory and the app process environment. `DELETE` without `purge` keeps the secret file as app state; `?purge=1` removes it.

A running process keeps the values it started with. Setting, rotating or deleting a secret returns `restart_required: true` but does not restart the app; use `POST /apps/<name>/restart` or a deploy when the new value should apply. Redaction is best effort: the supervisor replaces whole stored values, longest first, in new app log lines, supervisor access lines, the log tail and `lastError`. Previous values stay in memory for redaction until the supervisor restarts. Existing log files, backups, `/data`, and anything an app already wrote elsewhere are not retroactively scrubbed, and a value an app transforms or logs in pieces may still appear.

`disk_mb` measures `/data` plus the app's SQLite database files; logs, backups and deployment bundles are not counted. In trusted mode it is a soft quota: the app snapshot reports `diskBytes`, `diskLimitBytes` and `overQuota`, and the supervisor does not stop the app. In untrusted mode it defaults to 256 MiB, is enforced by a monitor that stops an over-quota app with a `lastError`, and also caps each file through `RLIMIT_FSIZE`; with `WT_DATA_QUOTA_MODE=setquota` it also becomes the app UID's equal soft and hard block limit, with `maxAppEntries` as its inode limit. `0` or an omitted value means unlimited in trusted mode; untrusted mode fills in its default instead. It also becomes the database's `max_page_count`, so an app cannot write its SQLite file past `disk_mb` even before the monitor runs.

`GET /usage` returns `{"appsBytes", "stateBytes", "totalBytes", "budgetBytes", "overBudget"}`. The total counts the apps tree and the state tree once each, even when one directory is nested inside the other. A configured `WT_DISK_BUDGET_MB` makes a deploy, manual snapshot, backup import or database import fail with `507` while the measured total is already over budget; it does not retroactively delete anything, and the existing history pruning still applies. A monitor logs when usage first crosses the budget and when it falls back under it.

Every successful deploy or rollback retains a bundle under `WT_STATE_DIR/.deployments/<app>`: `app.json`, `main.wasm`, `main.cwasm`, `artifact.json`, `static/`, and a `meta.json` with its wasm digest, size and artifact metadata. Identical artifacts and static files are content-addressed once under `.deployments/<app>/.blobs` and hardlinked into each bundle, so a config- or static-only redeploy does not copy unchanged bytes again. `meta.json` records component digests in `blobs` and static file paths to digests in `static`, and both maps keep their blobs alive during pruning; `bytes` counts each unique blob size once even when several static files share content. Legacy bundles with copied static files but no `static` metadata still list, copy and roll back, and their files remain readable. New records set `digest` to the wasm digest and also carry `wasmDigest` and `wasmBytes`; legacy cwasm-only history keeps the cwasm digest. A deploy that fails while starting or installing a new version is retained as a metadata-only `status: "failed"` entry with its attempted digest and redacted error; it has no bundle and cannot be rolled back to. Successful entries carry `status: "success"`, and the entry matching the app's running `currentDigest` is marked `current` in the list response. `GET /apps` and the SSE snapshot expose `currentDigest`, and the panel shows it on the overview. The newest ten history entries, successful or failed, are kept, and successful bundles are capped at 1 GiB per app; the panel shows the resulting on-disk size. A rollback installs the retained code and config but leaves the live database and `/data` untouched, so schema changes between the two versions still matter.

Deploy, config, lifecycle, backup restore, rollback and database import operations take a per-app lock, so a slow upload or readiness wait for one app does not block another. The compiled-artifact cache is shared, so its lookup, compile, install and prune steps remain serialized separately.

`DELETE` is deliberately conservative: it removes the app directory and stops the process, but leaves the database, backups, logs, resource history, deployment history and `/data` intact unless `purge=1` is set.

```sh
curl -X PATCH -H "Authorization: Bearer $WT_TOKEN" \
  -d '{"memory_mb":256,"env":{"NEW_KEY":"value","OLD_KEY":null}}' \
  http://127.0.0.1:8080/apps/notes/config
```

## Scaffold a new app

```sh
/tmp/wt-deploy new --dir wt/example/myapp
/tmp/wt-deploy new --dir ~/src/myapp --module example.com/myapp
/tmp/wt-deploy dev --dir ~/src/myapp --wasmtime /path/to/wasmtime
# myapp at http://127.0.0.1:8080/
```

`wt new` scaffolds a notes app with the guestbook stack: a static SvelteKit frontend with Valibot request and response validation, a Go JSON API, the Valibot to JSON Schema to `go-jsonschema` contract pipeline for both directions of the wire, and per-app SQLite storage. Inside `wt/example` it keeps using the shared monorepo module. Anywhere else it writes a standalone module with a vendored `sdk/` tree (`app`, `storage`, `wasihttp`, `httpmeta`, WIT and `generate.sh`), `go.mod`/`go.sum`, and an `app.json` build command that uses `./sdk`. Pass `--module` to choose the Go module path and `--sdk /path/to/wt/packages` to copy a local SDK instead of the embedded one. The embedded copy is checked against `wt/packages` by `TestEmbeddedSDKMatchesPackages`.

`wt dev` builds the app and serves its API at `http://<name>.localhost:8080/`, while the admin panel moves to a separate listener at `http://127.0.0.1:8081/admin/` (override with `--admin-port`). This keeps `/admin` and `/apps` off the app host. Pass `--workspace DIR` to serve every immediate child directory with an `app.json` together; workspace apps use `<name>.localhost` and share one supervisor and state directory, so app-to-app fetch and storage work across them. `--app-host` is only valid for the single-app form. The SQLite database persists in `/tmp/wt-dev/<name>` between runs.

For a SvelteKit app with a `web/package.json`, `wt dev` also starts `vite dev` on a free port and prints its URL, normally `http://127.0.0.1:<vite-port>/`. The browser gets Vite HMR and component state is preserved. Vite proxies the `dev.proxy` prefixes (default `/api`) to the supervisor with the app's `Host`, so frontend fetches hit the running wasm component. A generated `web/.wt-dev.vite.config.ts` keeps the app's own Vite config and adds that proxy, the loopback host and the chosen port; it is ignored by git and removed on exit.

`dev.command` defaults to `bun run dev`. `dev.build` is the component-only build used after a Go, WIT, `app.json` or `dev.watch` change; the scaffold and guestbook set it to regenerate the contract and componentize without rebuilding the frontend. `dev.watch` lists extra inputs such as `web/src/lib/contract.ts`, because the frontend directory is otherwise left to Vite. `dev.port` fixes the Vite port instead of choosing a free one. A component rebuild redeploys the wasm app behind the still-running Vite server, so the browser does not reload. `--vite=false` forces the original single-tier loop, and workspace mode uses that loop as well. `--watch=false` skips component redeploys but leaves Vite running. Without a web directory, `wt dev` keeps the single-tier build and watch: it rebuilds and redeploys after a change, ignoring `static/`, `build`, `dist`, `target`, `node_modules`, `.svelte-kit` and the generated `main.wasm`/`main.cwasm` so a build does not retrigger itself.

## SvelteKit apps are static

The supported SvelteKit shape is `@sveltejs/adapter-static` with `export const prerender = true` in the root layout. The production artifact is prerendered HTML, JavaScript and CSS copied into `static/` and served by the supervisor; the wasm component is the Go API behind it. There is no Node server in the component.

That boundary is deliberate, not pending: do not add `+page.server.ts`, `+layout.server.ts`, `+server.ts`, `src/hooks.server.ts`, `src/lib/server/` modules or form actions. The build rejects those files with a clear error. There is no SSR, no server `load`, no `$env/dynamic/private` and no server-only SvelteKit API in production. `+page.ts` and `+layout.ts` run during prerender and in the browser, so keep them free of server APIs; fetch dynamic data from the Go component after hydration, or prerender every route with `entries`. The Vite dev server can render pages and run server code in development, but that is a dev-only convenience and not evidence that the same code will work in the static production build.

Contracts stay on the frontend side as Valibot schemas. `web/src/lib/contract.ts` exports a `requestSchemas` and a `responseSchemas` registry; `web/scripts/contract.ts` turns every entry into JSON Schema, and `go generate ./contract/` generates one Go type for each schema. Request schemas produce the Go request structs that handlers decode, and response schemas produce the Go response structs that handlers return, so a changed field name fails to compile instead of drifting silently. The client validates requests before `fetch` and validates every response through the same schemas, including the error body. A registered schema that appears inside another one is emitted as a `$ref` to its own file, so `NotesResponse` and `MessagesResponse` reuse the generated item type rather than duplicating it. A JS component runtime for real SSR is a possible future host feature, but it is not implemented, and apps should not depend on one.

## What each app runs

```sh
KEY=VALUE WT_STORAGE_URL=http://127.0.0.1:<storage-port> WT_STORAGE_TOKEN=<token> \
wasmtime serve -Scli -Sp3 -S inherit-network -W component-model-async=y \
  --allow-precompiled \
  --addr 127.0.0.1:0 \
  --dir /tmp/wt/state/<name>::/data \
  --env KEY \
  --env WT_STORAGE_URL \
  --env WT_STORAGE_TOKEN \
  main.cwasm
```

Env values are passed in the process environment through `cmd.Env`, while argv carries only `--env KEY`. Values therefore do not appear in `/proc/<pid>/cmdline`. This includes `WT_STORAGE_URL` and `WT_STORAGE_TOKEN`. A `secret://NAME` env value is resolved before the process starts. One app-runner process starts and owns all of these wasmtime children; the supervisor connects to it at `WT_STATE_DIR/.runner.sock` and holds a liveness pipe, so the runner survives a `SIGHUP` handover and stops the apps if the supervisor dies.

The supervisor asks wasmtime to bind a free loopback port and reads the actual address from its `Serving HTTP on ...` startup line, so there is no separate port probe to race another process. A restarted process may therefore get a different port; the proxy is updated before traffic is routed to it.

Before the proxy runs, the supervisor answers `GET`/`HEAD` from the app's `static/` directory. A matching file is served with immutable caching under `/_app/immutable/` and revalidation elsewhere; an unknown path falls back to `static/index.html` only when the request accepts `text/html`, so API fetches still reach the component. This keeps frontend assets out of the wasm component and lets a static-only change ship without recompiling it. Static files are only served while the app process is running; a stopped app returns `503`. During a deploy the old process keeps its route until draining, and its directory already points at the new bundle, so it serves the new assets. `wt.packages/app.StaticFiles` remains available for apps that prefer to embed a frontend.

A static file with a sibling `.br` or `.gz` sidecar is served precompressed when the request accepts that coding. The original must still exist, the supervisor sets `Content-Encoding` and the original file's `Content-Type`, and it adds `Vary: Accept-Encoding`; `.br` wins when both are available. Build pipelines can emit both sidecars without changing `app.json`. For proxied component responses the supervisor asks the app for identity and gzips `application/json` and `+json` responses itself when the client accepts gzip, removing `Content-Length` and adding `Vary: Accept-Encoding`; SSE, `no-transform` responses, already encoded bodies and known responses of 256 bytes or less are left alone.

### Realtime

WASI HTTP in this stack has no WebSocket upgrade path, and `wasmtime serve` does not provide one, so a component cannot hold a WebSocket session. Server-sent events work because they are ordinary streaming HTTP responses: the panel uses them for snapshots and log streams, and apps can return `text/event-stream` through the same proxy. Polling is the fallback for clients that cannot consume SSE. A host-side WebSocket bridge would have to translate frames into a WASI-friendly request or import and is not implemented; do not assume `Upgrade: websocket` is forwarded.

`app.json` carries `name`, `host`, `hosts`, `health`, `env`, `memory_mb`, `disk_mb`, `cpu_millicores`, `pids_max`, `io_weight`, `max_concurrency`, `job_concurrency`, `request_timeout`, `startup_timeout`, `health_interval`, `health_failures`, `restart_on_unhealthy`, `network`, `identity`, `allow_callers`, `egress`, `schedule` and the local `build` command. `host` is the primary exact or wildcard host; `hosts` lists additional exact aliases or one-label wildcards such as `*.apps.example.com`. Host patterns are lower-cased, validated as DNS labels (1–63 characters per label, no leading or trailing hyphens, 253 bytes total) and rejected when they are reserved for the supervisor. An exact host beats a wildcard, and two apps cannot claim overlapping patterns. `memory_mb`, when set, becomes wasmtime's `-W max-memory-size`. `max_concurrency` is enforced by the supervisor proxy; `job_concurrency` caps durable jobs for the app. `request_timeout` is a Go duration between `1s` and `24h`: the supervisor cancels the proxied request at that deadline and, for bundles compiled with epoch interruption (all new `wt-deploy` bundles are), also passes `-W timeout=<duration>` to wasmtime so a CPU-bound guest is interrupted instead of left spinning. Leave it unset for streaming or long-running responses. Older bundles with no epoch flag in `artifact.json` get only the proxy deadline; redeploy or restart with a compiler available to rebuild them. `startup_timeout` is a Go duration between `1s` and `10m`, default `15s`; `health_interval` is between `1s` and `1h`, default `10s`; `health_failures` is between 1 and 100, default 2. `network` defaults to `inherit` in trusted mode, which passes `-S inherit-network` so the guest can reach the loopback storage service and other hosts; `none` omits it, so the guest cannot open sockets. In untrusted mode the default is `storage`, which also passes `inherit-network` but restricts the app's UID to the storage address, the internal app-fetch listener, any `egress` CIDR/port rules and its own reply traffic through iptables; `none` needs no rules, and `inherit` is rejected. `allow_callers` is the target-side app-to-app allowlist and `egress` is an explicit IPv4 CIDR/port outbound allowlist for untrusted mode. An app with `network` set to `none` cannot use the storage service, app fetch or make outbound HTTP requests, which suits static or self-contained components. The supervisor also sets `WT_APP_FETCH_URL`, `WT_APP_NAME`, `WT_APP_TOKEN`, `WT_APP_URL_<NAME>`, `WT_BLOB_SECRET`, `WT_BLOB_PREFIX`, `WT_IDENTITY_SECRET` for platform apps and the existing `WT_STORAGE_URL`/`WT_STORAGE_TOKEN`; only the names, never values, appear in argv. The health path defaults to `/healthz` and must answer with a 2xx status before a new version is considered ready. The scaffold's `app.ServeStorage` reserves `/healthz` for this probe and `/_wt/migrate` for the explicit migration phase; both answer `204` once storage migrations have run, and a migration keeps running if the probe or migrate client disconnects. The runtime-only image has no compiler, so it needs a matching cached or uploaded `main.cwasm`; `main.wasm` alone cannot run there. At startup the supervisor computes the wasm digest and looks in `WT_CACHE_DIR` for an entry built for its runtime version, target, baseline CPU policy and epoch setting. On a hit it hardlinks the cached module into the app directory as `main.cwasm`; on a miss it uses a matching uploaded cwasm (importing it into the cache) or compiles `main.wasm` into the cache. The compiler defaults to `WT_WASMTIME`; set `WT_WASMTIME_COMPILER` (or `--wasmtime-compiler`) to a wasmtime build with the `compile` subcommand when the runtime binary cannot compile. If no compiler and no matching cwasm are available, the app fails to start and keeps its previous version, and you redeploy with a matching `wt-deploy`.

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

### Durable jobs

`schedule` repeats one endpoint forever. For work that needs a payload, retries, a future run time or dead-letter handling, enqueue a job:

```json
{
  "path": "/jobs/email",
  "method": "POST",
  "body": "{\"to\":\"one@example.com\"}",
  "headers": {"Content-Type": "application/json"},
  "runAt": "2026-11-01T09:00:00Z",
  "maxAttempts": 5,
  "idempotencyKey": "welcome-one"
}
```

Jobs are stored one JSON file per app under `WT_STATE_DIR/.jobs/<app>.json` (`0700` directory, `0600` file), so they survive restarts. The worker calls the app through its loopback port only while the app is ready, waits for `runAt`, leases a running job for two minutes, retries failures with exponential backoff up to `maxAttempts` (default 3, maximum 20), and moves exhausted jobs to `dead`. `job_concurrency` (default 1, maximum 16) caps simultaneous jobs per app. `idempotencyKey` returns the existing job instead of creating a duplicate. A canceled running job may still have reached the app; delivery is at-least-once. The queue is a host-driven HTTP call, not a `wt:job` WIT export, and a job payload is capped at 64 KiB.

## Container

```sh
podman build -f wt/Containerfile -t wt-demo .
podman run -d --name wt-demo -p 127.0.0.1:8080:8080 \
  --user 10001:10001 --read-only --cap-drop=ALL \
  --security-opt no-new-privileges:true \
  -e WT_TOKEN=dev -v wt-data:/var/lib/wt wt-demo
```

The image defaults to the non-root `wt` user (`10001:10001`). This is enough for trusted mode: the supervisor only writes under `/var/lib/wt`, binds an unprivileged port and needs no capabilities. `/var/lib/wt` is a volume so the root filesystem can be read-only. A bind mount must be owned by `10001:10001`; a named volume inherits ownership from the image. The hardened defaults are also in `wt/compose.yaml`:

```sh
podman compose -f wt/compose.yaml up -d
```

Build for ARM with the matching platform; the Rust stage selects the wasmtime target from `TARGETARCH`, and the final image sets `WT_WASMTIME_TARGET` to the matching musl triple:

```sh
podman build --platform linux/arm64 -f wt/Containerfile -t wt-demo .
```

Untrusted tenant mode is the exception: it needs root to create per-app UIDs, namespaces, cgroups and firewall rules. Run that mode with `--user 0:0`, the cgroup hierarchy mounted where `WT_CGROUP_ROOT` expects it, and the capabilities it needs (`SETUID`, `SETGID`, `NET_ADMIN`, and `SYS_ADMIN` for the namespaces, mounts and cgroup delegation). Keep `no-new-privileges` and drop every capability you do not need there too.

The image is built entirely by `wt/Containerfile`: a Rust stage compiles wasmtime from the v50.0.0 source with `--no-default-features` and only the serve/run, component-model-async, pooling, GC/threads and WASI HTTP features, then strips it. The build images are pinned (`golang:1.27.3-alpine`, `rust:1.97-slim`, `alpine:3.22.2`, `oven/bun:1.4.4-alpine`) and CI also runs `govulncheck` and a production-only `bun audit`. There is no Cranelift or Winch in the serving image because it only deserializes `.cwasm` files. The musl build is static and the final stage is `scratch`, so the image is about **33 MB** (down from 168 MB with the full release on Debian).

The C API tarball is a shared library for embedding, not a server, and still contains the compiler, so it does not replace this runtime-only CLI. The final image carries only the static supervisor, wasmtime, CA certificates, `/etc/passwd` and `/etc/group`; the volume at `/var/lib/wt` holds apps and state.

On Orchard, create a GitHub-built deployment from this repository with `dockerfilePath: wt/Containerfile`, `buildFromRoot: true`, port `8080`, and a `WT_TOKEN` environment variable. Mount a PVC at `/var/lib/wt` so apps and the storage databases survive a pod roll. Point one ingress at port `8080` for the supervisor host and, if you use wildcard app hosts, add a wildcard DNS name and certificate such as `*.apps.example.com` to the same ingress. Then deploy with `--host https://<ingress-host> --app-host <ingress-host>`, or add `host: "*.apps.example.com"`/`hosts` aliases in `app.json` so a new app does not need a new ingress. The supervisor routes by `Host`, so `/apps/...` (the deploy API) and `/` (the app) share the same ingress host; exact app hosts still need their own ingress unless a wildcard covers them.

The deployment can run on a small CPU request because it never compiles: in the demo deployment two apps start from `.cwasm` files rebuilt on a laptop, and the notes database survives a pod roll on the PVC.

## Storage

The supervisor starts a loopback SQLite service and gives every app a token and URL through `WT_STORAGE_URL` and `WT_STORAGE_TOKEN`. Every app gets its own database at `WT_STATE_DIR/.storage/<app>/storage.db`, outside its `/data` preopen, so apps cannot touch the file directly.

Both connection pools are opened through a URL-encoded `file:` DSN, so the settings apply to every physical connection rather than to one connection at startup. The DSN sets `_busy_timeout=5000`, `_foreign_keys=on`, `_synchronous=NORMAL`, `trusted_schema(OFF)`, `temp_store(FILE)`, `cache_size(-2048)` (a 2 MiB page cache) and `mmap_size(0)`. The write pool adds `_journal_mode=WAL` and `_txlock=immediate`, so migrations, batches, transactions and sessions begin with `BEGIN IMMEDIATE` and take the write lock before they read a version or decide what to write. The read pool adds `_query_only=1`, no journal-mode change and no `_txlock`, so both pools report `synchronous=1` and the read pool cannot write. A positive `disk_mb` adds `_pragma=max_page_count(N)`, where `N = max(1, disk_mb × 1024 × 1024 / 4096)`, using SQLite's default 4096-byte page size. Changing `disk_mb` closes the app's write and read pools, and the next request reopens them with the new page limit.

The write pool also sets `journal_size_limit` and a quota-scaled `wal_autocheckpoint` (16 to 1000 pages, about 4 MiB at the 1000-page ceiling). The journal limit is `disk_mb / 8`, bounded to 256 KiB through 64 MiB, or 64 MiB without a quota. It truncates the WAL after a checkpoint, while the autocheckpoint keeps the WAL to about a quarter of a small quota and near 4 MiB for larger ones. A long-lived reader or session can still hold the WAL open, and the disk monitor counts `storage.db-wal` in the app's `disk_mb`.

SQLite temp files use `WT_STORAGE_TMP_DIR` (default `WT_STATE_DIR/.tmp`, created `0700`) through `temp_store_directory` on every connection. The directory is process-wide, so it is shared by every app and cannot be attributed to one app's `disk_mb`. `WT_STORAGE_TEMP_BUDGET_MB` (default 512, `0` unlimited) caps it globally; a monitor checks every ten seconds and, while the directory is over budget, new storage requests get `507` with code `temp_budget` until usage falls back. Existing queries finish under their five-second deadline, and stale `etilqs_*` files from a crash are removed at startup. The budget is a backstop, not a per-query limit: a burst between checks can exceed it, and one app's spill can block storage for all apps. When the directory is under `WT_STATE_DIR`, the global `WT_DISK_BUDGET_MB` accounting also includes it.

`configureStorageConn` only applies the limits the DSN cannot express on each request: `ATTACH` is disabled, SQL is capped at 64 KiB, a value at 1 MiB, 128 variables, 256 columns, expression depth 100 and 20 compound selects. It no longer runs a pragma list, so the fail-closed pragma settings are not repeated per query and cannot be skipped on a pooled connection.

The API is deliberately small SQL rather than a key-value shim:

```text
POST /v1/query   {"sql": "...", "params": [...], "max_rows": 1000}
POST /v1/write   {"sql": "...", "params": [...], "max_rows": 1000}
POST /v1/exec    {"sql": "...", "params": [...]}
POST /v1/batch   {"statements": [{"sql": "...", "params": [...]}, ...]}
POST /v1/transaction {"statements": [{"sql": "...", "query": true}, ...]}

POST /v1/sessions
POST /v1/sessions/{id}/query
POST /v1/sessions/{id}/exec
POST /v1/sessions/{id}/commit
POST /v1/sessions/{id}/rollback
```

`query` is read-only: it uses a separate `query_only` connection pool and an authorizer that denies writes, DDL, `PRAGMA`, `ATTACH`, transaction control and unsafe functions, so `INSERT ... RETURNING` must not be sent there. It returns `{"columns": [...], "rows": [[...]], "truncated": false}` and still accepts `SELECT`, `EXPLAIN`, CTEs, FTS reads and table-valued pragma functions such as `pragma_table_info`. `write` is the read-write counterpart for statements that return rows, mainly `INSERT`, `UPDATE` and `DELETE ... RETURNING`; it runs on the write pool, returns the same shape plus `rows_affected`, and records the change in the change log. `exec` returns `{"rows_affected": n, "last_insert_id": n}` and is the no-rows write path. `batch` runs every write statement in one transaction and returns one result per statement. `transaction` runs reads and writes in one transaction: mark read statements with `"query": true` to get their rows back, and the rest return `rows_affected` and `last_insert_id`.

Errors are flat JSON with a stable `code` and, for SQLite failures, `sqlite_code` and `constraint`: `{"error": "...", "code": "constraint", "sqlite_code": 2067, "constraint": "UNIQUE"}`. Constraints are 409, busy or locked databases are 503, a full disk is 507, denied or read-only statements are 403, invalid parameters are 400 and unexpected SQL or storage failures are 500. An edited migration whose version is already applied is also a 409, with code `migration_checksum`. The Go client exposes a typed `*storage.Error` with `Status`, `Code`, `Message`, `SQLiteCode` and `Constraint`.

A session pins one connection and an open transaction across requests, so an app can read a value, decide in code and write it back without another writer slipping in. `POST /v1/sessions` returns `{"id": "...", "expiresAt": "..."}`; the session endpoints take the same query/exec bodies as their non-session counterparts and commit or roll back everything at once. The idle lease is 30 seconds and resets on every operation; expiry rolls back automatically and releases the connection. A supervisor keeps at most 8 sessions per app and leaves half the connection pool for ordinary queries, closes sessions before replacing a database file, and logs session writes only when they commit. In Go, `store.WithTx(ctx, func(tx *storage.Tx) error { ... })` commits on success and rolls back on error; `store.BeginTx` exposes the session when the callback style does not fit. `store.WriteReturning(ctx, query, args...)` wraps the returning-write endpoint. `store.QueryN(ctx, n, query, args...)` sends `max_rows` to cap a read, and `store.QueryOptions(ctx, query, storage.QueryOptions{MaxRows: n}, args...)` is the general form. `store.QueryPage(ctx, query, storage.Page{Limit: 50, Offset: 100}, args...)` wraps the query as `select * from (<query>) limit ? offset ?`; `rows.NextPage()` then returns the next offset while the page is full. This is offset pagination, not keyset: a concurrent write can shift rows between pages, so give the query a deterministic `ORDER BY`, and build keyset predicates on top when you need a stable snapshot. Queries use a small WAL-backed connection pool, so reads can proceed while one write is in flight, while SQLite still serialises writers. Values are JSON scalars; blobs are `{"$b64": "..."}`. Parameters are bound, not interpolated. SQLite's authorizer, not a word denylist, blocks `ATTACH`, `DETACH`, `PRAGMA`, `load_extension`, `readfile`, `writefile`, transaction control and virtual-table modules other than `fts5`, `fts5vocab`, `rtree` and `rtree_i32`; `SQLITE_LIMIT_ATTACHED` is also zero, so a component cannot read another app's database or write files through SQL. Identifiers that merely contain words such as `pragma`, `attach` or `load_extension` are allowed. Queries time out after five seconds and are capped at 10,000 rows, 1 MiB per value and 8 MiB per response.

SQLite still runs in the supervisor process, so the storage API is not a separate control-plane boundary yet. The current hardening is: every physical connection gets `synchronous=NORMAL`, `temp_store=FILE` instead of anonymous memory, a 2 MiB page cache, `mmap_size=0` and foreign keys from the DSN; SQL and value limits and no `ATTACH` come from the per-request limit calls; a per-app `max_page_count` caps each database; a global semaphore allows at most 16 storage requests at once across all apps; and the pre-update row capture checks a value's encoded size before base64-encoding it, so a large blob cannot be duplicated just to be rejected by the 256 KiB cap. Fixing this properly needs a separate storage process with its own memory and CPU limits; do not treat the in-process SQLite service as isolation between untrusted tenants and the control plane.

Schema changes go through a versioned, checksummed migration instead of raw `PRAGMA`:

```text
GET  /v1/schema[?stats=1]
POST /v1/migrate  {"version": 2, "statements": [{"sql": "...", "params": [...]}, ...]}
```

`schema` returns SQLite's `user_version`, every user table with its columns, types, defaults and primary-key positions, each table's indexes and foreign keys, every view with its SQL and columns where SQLite can describe them, and the applied migrations with their version, checksum and `appliedAt` time. An index carries its name, uniqueness, origin, partial flag and key columns in index order; an expression or rowid column has a `null` name. A foreign key carries its id, sequence, referenced table, `from`, `to`, `onUpdate`, `onDelete` and `match`; a `null` target column stays null. The reserved `wt_migrations` table and SQLite's internal `sqlite_*` tables are hidden from every listing. With `?stats=1`, each table also gets `rows` from `count(*)` and `bytes` from `dbstat`; if `dbstat` is unavailable, `bytes` stays zero rather than failing the request.

`migrate` runs its statements, inserts the version, checksum and `applied_at` into `wt_migrations`, and sets `user_version` in one `BEGIN IMMEDIATE` transaction; a failing statement rolls back all of it. The checksum is a deterministic SHA-256 over the version, each statement's SQL and its raw JSON parameters, so editing an already applied migration fails with a 409 `migration_checksum` error instead of silently doing nothing. Applying a version at or below the current one is a no-op when the stored checksum matches, or when the database predates `wt_migrations` and has only `user_version`; legacy versions are not given invented checksums. This lets an older blue or green process start against a database a newer deploy already migrated while stale code cannot quietly rewrite history. In Go, `store.Migrate(ctx, version, statements...)`, `store.Schema(ctx)` and `store.SchemaStats(ctx)` wrap these endpoints; `Schema` stays cheap, while `SchemaStats` requests row counts and page sizes. The examples use `Migrate` for their initial schema.

Migrations have their own timeout because a `CREATE INDEX` or backfill can take much longer than a query. `WT_STORAGE_MIGRATION_TIMEOUT` (or `--storage-migration-timeout`) defaults to `5m` and must be between `1s` and `1h`; normal storage queries keep the five-second deadline. The supervisor's storage service applies this deadline, so it is one host-wide setting rather than per app.

The supervisor calls the app's `POST /_wt/migrate` endpoint after the process is listening and before `/healthz`, so a migration is not raced by the startup probe or bounded by `startup_timeout`. The SDK runs the migration on a context without cancellation, so a disconnected probe does not abort it, and an app without the endpoint (an older bundle) falls back to health-only readiness. The app's own `request_timeout` still bounds the guest request that performs the migration; set it at least as long as the longest migration, or leave it unset, when a migration can exceed the default 30s.

Migrations must be expand/contract: add columns and tables in one release, move writes over, and remove the old shape only after every deploy that reads it is gone. A code rollback is code-only, so it also has to be able to read the newer schema; for a destructive change, restore a snapshot from before the migration as well (`rollback?backup=<file>`).

The shared `packages/storage` package is the client; it takes an `*http.Client`, and the apps pass `wasihttp.NewClient()` for outbound `wasi:http`:

```go
rows, err := store.Query(ctx, `select id, body, created_at from notes order by created_at`)
// ...
_, err = store.Batch(ctx,
    storage.Statement{SQL: `insert into notes (id, body, created_at) values (?, ?, ?)`, Args: []any{id, body, at}},
    storage.Statement{SQL: `delete from notes where id not in (select id from notes order by created_at desc, id desc limit ?)`, Args: []any{50}},
)

err = store.WithTx(ctx, func(tx *storage.Tx) error {
    var reactions int
    if err := tx.QueryRow(ctx, `select reactions from notes where id = ?`, id).Scan(&reactions); err != nil {
        return err
    }
    _, err := tx.Exec(ctx, `update notes set reactions = ? where id = ?`, reactions+1, id)
    return err
})
```

The notes example uses a real `notes` table. The one-time migration from the old JSON value has already run on the demo deployment, and the old `/v1/kv` endpoints have been removed.

## Blobs

Blobs are per-app, content-addressed files stored at `WT_STATE_DIR/.blobs/<app>` and served by the supervisor, so an upload or download never takes a wasm concurrency slot. The store is outside `/data`; an app cannot read or write it directly. Blob bytes and metadata count toward the app's `disk_mb` and the global `WT_DISK_BUDGET_MB`, and beyond that an upload is rejected with `507`.

The app signs URLs with the per-app key in `WT_BLOB_SECRET`; the supervisor injects the key and `WT_BLOB_PREFIX` (`/_wt/blobs`) into the process environment. The shared `app` package produces the same-origin URLs:

```go
upload, err := app.BlobUploadURL(8<<20, "image/png", 10*time.Minute)
// send upload to the browser; it PUTs the file body there
download, err := app.BlobDownloadURL(digest, time.Now().Add(time.Hour))
deleteURL, err := app.BlobDeleteURL(digest, time.Now().Add(time.Hour))
```

The wire API is deliberately small:

```text
PUT  /_wt/blobs?exp=&max=&ct=&sig=       upload body, returns {digest,size,contentType,url}
GET  /_wt/blobs/<digest>?exp=&sig=       download, Range and HEAD supported
DELETE /_wt/blobs/<digest>?exp=&sig=     remove the blob
```

`exp` is a Unix timestamp no more than 24 hours out, and `sig` is HMAC-SHA256 over the method, app name, digest, expiry, max bytes and content type. Uploads hash the body while streaming it to a temporary file, check the declared `max`, and move it under the SHA-256 digest, so a client cannot choose the digest. A duplicate upload is a no-op (`200` instead of `201`). Downloads set an `ETag` and `Cache-Control: private`, and `http.ServeContent` supplies `Range`, `If-Range`, `If-None-Match` and partial responses. There is no automatic garbage collection because only the app knows which files its data still references; call the delete URL when a record is removed, or purge the app's state.

### Why not a WIT storage import?

There is no durable, standard WASI storage interface to target in this setup:

- `wasmtime serve` 50 can enable the experimental `wasi:keyvalue` API, but its only provider is in-memory and per-instance. Writes do not survive the instance or the process, so it cannot back the notes app.
- `wasi:sql` is a Phase 1 proposal with no implementation in wasmtime.
- Guest-side SQLite is not practical from Go: `modernc.org/sqlite` does not build for `wasip1`, and `ncruces/go-sqlite3` adds a nested wazero runtime (about 10 MB) with a separate VFS, so it cannot simply use the outer `wasi:filesystem` preopen. WASI preview1 also does not give SQLite the file locking it needs across component instances.
- A custom host import would mean replacing `wasmtime serve` with an embedded runtime (a fork of its p3 serve path) or composing an adapter component at deploy time. Both add more moving parts than the loopback service and still need the same host-side SQLite database.

The app still uses a WASI API for transport: `wasi:http` over loopback. If wasmtime grows a persistent `wasi:keyvalue` or `wasi:sql` provider, the client package can switch without changing the app's SQL.

The supervisor grants outbound network with `-S inherit-network` so apps can reach that loopback endpoint. In trusted mode that also exposes other hosts and the loopback admin API. Untrusted mode adds per-UID iptables owner rules that allow only the storage address, the internal app-fetch listener, configured `egress` rules and the app's own replies; see [Untrusted tenant mode](#untrusted-tenant-mode).

## App-to-app calls

An app can call another app through the supervisor's internal fetch listener (`WT_APP_FETCH_LISTEN`, loopback only, default `127.0.0.1:0`). At launch the supervisor sets `WT_APP_FETCH_URL`, `WT_APP_NAME`, `WT_APP_TOKEN` (the same per-app credential as `WT_STORAGE_TOKEN`) and `WT_APP_URL_<NAME>` for every app that is already registered. `packages/app.AppURL(name)` falls back to `WT_APP_FETCH_URL + "/apps/" + name`, so an app deployed later still has a working URL without a restart. The base path is `/apps/<target>/<path>`.

The target opts in with `allow_callers` in `app.json`, for example `{"allow_callers":["frontend","worker"]}`. The list is normalised, sorted and default-deny. The caller proves its identity with its own bearer token; the supervisor resolves that token to the caller, checks the target's list and returns `401`, `403` or `404` before forwarding. The caller's `Authorization`, `Cookie`, `X-Forwarded-*` and `X-WT-*` headers are removed, so target credentials and spoofed identity do not cross the boundary. Request bodies are capped at 8 MiB.

On the forwarded request the supervisor sets `X-WT-App-Caller`, `X-WT-App-Timestamp`, `X-WT-App-Body-SHA256` and `X-WT-App-Signature-V2`. The v2 signature is an HMAC-SHA256 over the caller, method, path with query, timestamp and the body's SHA-256, keyed by the target's storage token, and is valid for five minutes. The supervisor reads and caps the body at 8 MiB before signing, then restores it for the target. The target verifies it with `app.Caller(r)`, which reads and restores the request body; call it before the handler reads the body. In trusted mode the supervisor also sends the legacy `X-WT-App-Signature` metadata-only signature for older targets; untrusted mode sends v2 only. The body is buffered, so this path is not suitable for streaming uploads larger than 8 MiB. `app.NewAppRequest(ctx, method, target, path, body)` builds a request with the bearer identity; the supervisor adds the signature when it forwards the call to the target. Pass the request to the `wasihttp.NewClient()` client:

```go
req, err := app.NewAppRequest(r.Context(), http.MethodGet, "notes-api", "/v1/notes", nil)
if err != nil {
    return err
}
resp, err := wasihttp.NewClient().Do(req)
```

The signature covers the path the target receives, so do not rewrite the URL between `app.Caller` and the handler. A target that does not call `app.Caller` still receives the headers but is responsible for trusting them only behind this internal listener. Calls between two instances of the same app, retries and fan-out are left to the caller.

Explicit outbound access in untrusted mode uses `egress` in `app.json`: a list of `{"cidr":"203.0.113.0/24","port":443}` rules. Only IPv4 TCP is supported; the rules are installed with the app UID before readiness and need a restart after a change. DNS is not automatically allowed, so use an IP or CIDR, or add the resolver to the list. In trusted mode egress rules are ignored because the app already has unrestricted network access.

## Database backups

A snapshot captures the app's whole writable state. When the app has a SQLite database, the supervisor copies it with SQLite's online backup API, so the snapshot is consistent even while the app is reading and writing and the app keeps serving throughout. Every snapshot also has a `storage-<timestamp>.data.tar.gz` archive of the app's `/data` directory; the archive is written while the app runs, so it is best-effort rather than transactional. An app that has never used storage gets a data-only snapshot, represented by the archive alone. Snapshots live at `WT_STATE_DIR/.backups/<app>/`, outside both the storage directory and the app's `/data` preopen. Each app's policy lives in `config.json` and its snapshot ancestry in `tree.json` beside them.

Automatic backups are on by default: every app is snapshotted every 24 hours, and 7 snapshots are kept. The supervisor checks about once a minute, with up to a fifth of the interval of jitter so several supervisors do not all archive at the same second, including apps that have no database yet. Due apps are processed by a small worker pool, and each app has its own backup lock, so a large `/data` archive, export or restore only delays that app's own snapshots instead of every app's. Changing the policy takes effect on the next check, and reducing `keep` prunes older snapshots immediately. Pruning removes abandoned leaves first and keeps the branch the live database follows for as long as possible. Snapshots and their tree survive deploys, restarts and pod rolls with the rest of the state volume.

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

The admin panel's Database tab lists an app's user tables and pages through their rows. It is read-only: the table name is checked against `sqlite_master` and quoted before use, so the viewer cannot run arbitrary SQL or reach another app's database. SQLite's internal `sqlite_*` tables and the reserved `wt_migrations` table are hidden. Values are encoded like the app-facing storage API, and blobs are shown as their size rather than their base64 contents.

```text
GET  /apps/<name>/database[?stats=1]
GET  /apps/<name>/database/rows?table=<table>&limit=50&offset=0
GET  /apps/<name>/database/changes?limit=200
GET  /apps/<name>/database/export                    download a consistent SQLite copy
POST /apps/<name>/database/import                     replace it with an uploaded SQLite file
POST /apps/<name>/database/maintenance                {"op": "vacuum" | "optimize" | "checkpoint" | "integrity_check"}
```

Rows default to 50 per page and accept `limit` up to 500. The rows response includes the columns, the page of rows, the table's total row count, and whether more rows remain. The overview also reports the schema version from `user_version`. With `?stats=1` it adds a `stats` array of `{name, rows, bytes}` entries for the visible tables; ordinary loads omit the array so they stay cheap. The panel requests stats and shows the selected table's row count and formatted size beside the table selector.

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

An incoming request now fills the standard `net/http` fields: `URL.Scheme`, `URL.Host`, `Host`, `RequestURI`, `RemoteAddr` and `TLS` come from `spin-full-url`, `spin-client-addr` and the `Host`, `X-Forwarded-Host` and `X-Forwarded-Proto` fallbacks. `X-Request-Id` is preserved in the request headers. The handler context is cancelled when a response write discovers that the client dropped the body stream or when the handler returns; the adapter has no separate disconnect signal before the first write, so a handler that never writes cannot be cancelled sooner. Outgoing requests use the request context: a cancelled context stops the body copier, an existing deadline becomes the connect, first-byte and between-bytes timeouts, and `wasihttp.NewClient()` defaults to a 30-second timeout (set `Client.Timeout` to zero to opt out). A cancellation without a deadline is observed before a send and after it returns, but cannot interrupt an already blocked `wasi:http` call. `wasihttp.Get` and `Post` still send without a default client timeout.

## Limits of this prototype

- One `wasmtime serve` process per app. A crashed app restarts with exponential backoff (2s up to 30s) on a fresh loopback port that the supervisor discovers before restoring its proxy route.
- `memory_mb` is a per-app linear-memory cap. Trusted mode leaves CPU to the container; untrusted mode also writes a per-app `cpu.max`, `pids.max` and `io.weight` cgroup and applies rlimits.
- `disk_mb` is a soft quota in trusted mode and an enforced stop condition in untrusted mode, over `/data` and database files only. It also sets the database's `max_page_count`. Logs, backups and deployment bundles are outside it, but `WT_DISK_BUDGET_MB` measures the apps and state trees together and reports through `GET /usage`. `/data` still has no hard filesystem or project quota: untrusted mode uses the monitor plus `RLIMIT_FSIZE` unless the host provides a real filesystem quota. `max_concurrency` and `request_timeout` bound proxy work; `request_timeout` also interrupts CPU-bound guest execution on epoch-enabled bundles. Health failures stop routing to an unready app and optionally restart it, but there is no fuel-based accounting and no separate maintenance mode.
- TLS is optional and basic (certificate files). A single certificate covers every app host, so wildcard app hosts such as `*.apps.example.com` need a matching wildcard certificate; there is no per-app certificate or SNI lookup yet. Use the Orchard ingress for real certificates and DNS automation.
- Storage is a per-app SQLite SQL service, not a standard WASI import. The SQLite file is private to the supervisor; apps get a preopen at `/data` as well.
- In trusted mode outbound network is granted to every app (`-S inherit-network`), including the loopback storage and app-fetch services. Untrusted mode defaults to `storage`, rejects `inherit` and restricts the app UID with iptables to storage, app fetch and explicit `egress` CIDRs; there is still no network namespace, so the firewall is the boundary.
- The deploy POST waits for the new version and retains its last ten bundles; there is no asynchronous deploy job or long-polling yet. Startup is fast enough that this is usually not noticeable.
- `.cwasm` is native code. Only load artifacts produced by your own deploy pipeline; the deploy endpoint is the trust boundary. The CLI and supervisor must agree on the wasmtime version and target triple; the runtime-only image is musl and accepts only `x86_64-unknown-linux-musl` artifacts. Mismatches are keyed out of the cache and rebuilt from `main.wasm` when the supervisor has a compiler. Same-digest deploys update static files without restarting the process, but the cache and deployment history still retain each bundle.
- The container runs as root. Untrusted mode uses that root to create per-app UIDs, cgroups, rlimits and firewall rules, so the container needs cgroup v2 mounted writable and the `NET_ADMIN` capability; the image itself does not create those mounts.
- Apps are children of one app-runner process. The runner stops them if its liveness pipe from the supervisor closes, and survives a `SIGHUP` handover; this relies on Linux process and Unix socket behaviour.
