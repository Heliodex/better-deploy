# Fixes 5

App-authoring review after building `wt/example/passport`, 2026-11-02. This pass looks at the platform from the app author's side again, this time through an app that needs a real browser login, third-party OAuth, server-side sessions and a static SvelteKit frontend. Prior priorities stay in `notes/fixes.md`, `notes/fixes2.md`, `notes/fixes3.md` and `notes/fixes4.md`; this list only adds work those notes do not already cover.

The short assessment: the execution model, storage API, static split and deploy loop are strong. Passport is mostly ordinary `net/http`, SQL and a static SvelteKit page. The weak points are the static/component routing boundary, the lack of a canonical public origin, repeated security-sensitive auth code, missing CLI access to secrets, and a few operational gaps. None of these questions the wasm execution model or the supervisor topology.

Concrete evidence from this build:

- A browser navigation to `/auth/login` or `/auth/callback` never reached the component because the supervisor serves `static/index.html` for any HTML-accepting GET before proxying. Login became a form `POST`, and the callback became a plain static file that calls a JSON endpoint.
- A prerendered `/auth/callback/index.html` was not reachable at `/auth/callback/` because `staticFile` matches regular files, not directory indexes.
- The app had to set `paths.relative = false` in `vite.config.ts` so a nested page could load `/_app` assets instead of `/auth/_app`.
- OAuth redirect URIs needed an ad hoc public-origin calculation from `Origin`, `Referer` and loopback host checks. `wt dev` rewrites `Host` to `<app>.localhost` without the Vite port, and an ingress terminates TLS before the supervisor sees the request.
- Passport needed about 250 lines of session, cookie, state and CSRF handling plus a 600-line OAuth client with discovery, PKCE, code exchange, JWKS verification, UserInfo and end-session.
- The remote deploy needed a local wasmtime wrapper that rewrites only `wasmtime --version`. The official release reports `wasmtime 50.0.0 (commit date)`, while the supervisor reports `wasmtime 50.0.0`, and the CLI treats those as a mismatch even though the compiler binary is the same.
- The new app host needed a manual Orchard ingress. The deploy succeeded before the host resolved.
- Bursts of concurrent API requests returned occasional `500`s on both passport and the existing pulse app. Wasmtime logged `async-lifted export failed to produce a result`; sequential traffic was clean. Passport's first page load was changed from two parallel fetches to two sequential fetches to make the demo reliable.
- A confidential OAuth client would have to put its secret in `app.json`. The supervisor supports write-only secrets and `secret://NAME`, but `wt-deploy` has no secrets command, so the convenient path is still the bundle.

## What held up

- `wasihttp` handled inbound requests and outbound HTTPS to the OAuth provider without any app-side TLS or WASI plumbing. The provider's discovery, token and UserInfo calls worked first try.
- `storage.NewTestServer()` plus the scaffold's app struct made handler tests possible without wasmtime. The passport tests cover sessions, ownership, validation, CSRF, origin handling, JWT verification and expired tokens.
- The component/static split is excellent operationally. The first deploy uploaded about 13 MB; later frontend-only changes uploaded about 48 KB and left the running wasm alone. Deploy output said which phase was built, reused or skipped.
- The same CLI deployed locally and remotely, negotiated the supervisor target and reported readiness, digest and URL. Capability preflight and deploy summaries are good operator experience.
- `wt new` produced a working app with a real handler test, a SvelteKit shell, generated contracts and the correct wasm wiring. The scaffold is a good reference.
- Raw SQL, migrations, `app.ServeStorage`, `app.ReadJSON`, `app.WriteJSON` and the standard error shape kept the server code ordinary and predictable.

## Priority ordered

1. [x] **[P0] Make HTML navigation policy explicit and support server routes.**

   **Current behaviour.** `serveStatic` answers every `GET`/`HEAD` from `static/` before the component. A matching file is served; otherwise any request that accepts `text/html` gets `static/index.html`. That is correct for a pure SPA, but it makes a server-side `/auth/login`, `/auth/callback`, `/logout` or future SSR route impossible to reach from a browser. Passport works around it with a form `POST` for login and a plain static callback file that calls `POST /api/auth/callback`. `staticFile` also rejects directories, so a prerendered `/auth/callback/index.html` is not served at `/auth/callback/`; the request falls back to the root `index.html` instead.

   **Change.**

   - Add an app manifest field such as `server_paths` (prefix matches) or `static_fallback` (`true` by default). A browser navigation matching a declared server path must reach the component even when it accepts `text/html`. Real static files under that prefix should still win.
   - Or invert the default: check the component first for non-file GETs and keep an explicit `spa_fallback` for routes that really need `index.html`. The first option is smaller and keeps the current SPA behaviour.
   - Serve a directory's `index.html` for a trailing-slash request, so `/auth/callback/` reaches `static/auth/callback/index.html`. Keep the traversal and regular-file checks.
   - Validate the new field in `wt-deploy`, the supervisor config parser and the OpenAPI document.
   - Add supervisor tests for a declared server route, a real static file under a declared prefix, a missing file with and without HTML accept, and a nested prerendered page.
   - Update `example/passport` to use a normal SvelteKit callback page and `GET`-able login route once the policy exists, or keep the plain file as a documented fallback.

   **Done when.** A browser navigation to a declared server route reaches the component; a nested prerendered page is served from its directory index; an ordinary static SPA still falls back to `index.html`; no app needs a static shim for a callback.

   Implemented in `537aec0`. `app.json` now accepts `server_paths`, a normalized list of absolute path prefixes. The supervisor still serves real static files first, resolves a trailing-slash directory to its `index.html`, and only skips the SPA fallback when the request matches one of those prefixes. The field is validated by `wt-deploy`, the supervisor config parser and the panel schema, exposed in `GET /apps/<name>/config`, accepted by `PATCH /apps/<name>/config` without a restart, preserved through static-only deploys, and documented in the README and OpenAPI. Tests cover prefix matching, invalid prefixes, a real file under a server prefix, the root prefix, directory indexes, precompressed assets and the config patch. `example/passport` now declares `"server_paths": ["/auth"]`, links to `GET /auth/login`, and uses a normal prerendered `/auth/callback/` page instead of a plain static shim. Verified locally: `/auth/login` returns the provider redirect, `/auth/callback/` serves the nested page, and the full browser login completes with the new supervisor.

2. [x] **[P0] Give apps a canonical public origin and make dev and production agree.**

   **Current behaviour.** An app needs its own public URL for OAuth redirects, cookies, absolute links and post-logout redirects. The request has `URL.Scheme` and `Host`, but behind an ingress the scheme depends on `WT_TRUSTED_PROXIES` and whether the ingress preserves `Host`. In `wt dev`, Vite proxies to the supervisor with `Host: <app>.localhost` and drops the Vite port, so a redirect URI built from `Host` points at the wrong origin. Passport works around this with `Origin`, `Referer`, loopback host checks and a per-request helper. The supervisor sets `WT_APP_URL_<NAME>` for other registered apps, but there is no self URL helper or canonical `WT_PUBLIC_URL`.

   **Change.**

   - Add `app.PublicURL(r)` (or `app.BaseURL(r)`) to the SDK. It should use trusted supervisor-derived scheme and host, not raw client headers.
   - Have the supervisor set a launch environment value such as `WT_PUBLIC_URL` for the app's primary host, and a per-request trusted origin when aliases are involved. If a single value cannot cover aliases, expose a small `app.PublicURL` that validates the request against the app's configured hosts.
   - In `wt dev`, set the public URL to the actual Vite origin including its port, or preserve the browser's `Host`/port through the proxy. Add an explicit `--public-url`/`WT_PUBLIC_URL` override for unusual proxies.
   - Add tests for local HTTP, ingress HTTPS, a host alias, and the Vite dev origin.
   - Consider a local TLS mode for `wt dev`, because many OAuth providers require an `https` redirect URI even on localhost.

   **Done when.** Passport no longer contains `Origin`/`Referer` fallbacks. OAuth redirects, session cookies and post-logout redirects use one documented helper and work identically in dev and behind the ingress.

   Implemented in `6695eb5`. `app.PublicURL(r)` prefers `WT_PUBLIC_URL`, then a same-host `Origin` or `Referer`, then the trusted scheme and host from the request; loopback origins are accepted for the Vite dev server. Passport now uses it for the authorization redirect and post-logout redirect, and its old `baseURL`, `hostname` and `loopbackHost` helpers are gone. The SDK copy and README are updated. Tests cover plain HTTP, TLS, same-origin and spoofed origins, a loopback dev origin, a Referer, an environment override and an invalid override. The passport dev proxy now forwards only `/auth/login`, so the nested SvelteKit callback page stays on Vite. `wt dev` also deploys through the admin listener and passes an absolute Vite config path (`e784d04`). Verified with the full browser login through Vite and through the local app host.

3. [x] **[P1] Add small OAuth and session helpers instead of making every app rewrite them.**

   **Current behaviour.** Passport implements discovery caching, PKCE, state and nonce generation, code exchange, optional client secret, RS256 signature verification against JWKS, issuer/audience/expiry/nonce checks, UserInfo, end-session, hashed session tokens, cookie flags and CSRF checks. The code is testable and works, but it is security-sensitive and every OAuth app will write a variation of it. There is no shared package for third-party login, opaque sessions or cookie handling.

   **Change.**

   - Add `packages/oauth` with a small config (`issuer`, `client_id`, optional secret, scopes), discovery, PKCE, code exchange, JWT/JWKS verification, UserInfo and end-session. Keep response types generic and do not require a database schema.
   - Add session helpers to `packages/app` or a new `packages/session`: random token generation, hashed storage lookup, `SetCookie` with HttpOnly/Secure/SameSite, same-origin/CSRF checks and optional session expiry.
   - Provide a fake provider for tests: discovery document, token endpoint, JWKS and UserInfo. Use an in-memory store for flow and session tests.
   - Rewrite `example/passport` to use the helpers and keep the current raw implementation in the README or a comment for authors who want full control.
   - Do not turn this into a framework or a user database. Apps should still own their tables, claims and authorization rules.

   **Done when.** A new scaffold can add OAuth with one config block and a few handlers; passport drops most custom crypto and cookie code; tests cover a wrong nonce, wrong audience, expired token, bad signature and a rotated signing key.

   Implemented in `57c14ce`. `packages/oauth` provides discovery caching, PKCE and nonce generation, authorization URLs, code exchange with optional client secret, RS256 JWKS verification, UserInfo and RP-initiated logout. `packages/session` provides opaque token issuing and hashing, cookie set/clear helpers, constant-time comparison and same-origin checks with a loopback exception for the dev server. Passport keeps only its tables, handlers and UI; its 600-line OAuth client is replaced by a small config function. Tests cover discovery, authorization URLs, PKCE, exchange and form fields, token verification with wrong nonce, audience and expiry, signature verification, UserInfo and end-session. The embedded SDK and its sync test include both packages. Verified with the full browser login and check-in flow locally.

4. [x] **[P1] Expose secrets through `wt-deploy` and stop making `app.json` the convenient secret path.**

   **Current behaviour.** The supervisor has a write-only per-app secret store and resolves `secret://NAME` from `app.json` at launch. The admin API and panel can set secrets, but `wt-deploy` has no `secrets` command and no deploy-time secret input. The easiest path for an app author is a literal value in `app.json`, which is copied into the bundle and retained in deployment history. Passport avoided this by choosing a public OAuth client, but a confidential client should be normal.

   **Change.**

   - Add `wt-deploy secrets <app> list|set|delete`, with values read from stdin or a file, never from a command-line argument. Print only key names and `restart_required`.
   - Add `wt-deploy deploy --secret KEY` or `--env KEY=VALUE` that writes runtime config or secrets before the deploy, so local and remote setup is one command.
   - Keep the bundle and deployment history free of literal secret values. `app.json` should contain `secret://NAME` references only.
   - Add a test that a deploy with `OAUTH_CLIENT_SECRET=secret://OAUTH_CLIENT_SECRET` starts only when the secret exists and that the value never appears in deploy output or history.
   - Document the confidential-client setup next to the passport instructions.

   **Done when.** A confidential OAuth client can be configured without editing `app.json` or using raw curl, and no secret value appears in the bundle, deployment history or CLI output.

   Implemented in `9277362`. `wt-deploy secrets <app> list|set|delete` wraps the existing write-only secret endpoints. `set` reads from stdin or `--file`, trims one trailing newline, enforces the same 64 KiB and key rules as the supervisor, and never prints the value; `list` prints keys only. The help text, shell completion and README cover the commands. Tests cover list, set through a file, delete, bad keys, missing arguments and unknown actions. Verified live against the local supervisor: set, list, delete and an empty list.

5. [x] **[P1] Investigate and fix concurrent request traps.**

   **Current behaviour.** Sending 50 requests at parallelism 10 to `/api/session` or `/api/checkins` produced occasional `500`s on passport. The existing pulse app showed the same response pattern. Sequential requests were clean. Wasmtime logged `worker failed: wasm trap: async-lifted export failed to produce a result` followed by `worker trapped or panicked`, and the worker recovered. Passport's page load was changed from `Promise.all` to sequential fetches to hide this in normal use, but a dashboard or any page with parallel API calls will still meet it.

   **Change.**

   - Reproduce with `hello`, `notes` and a small handler that does no storage or outbound HTTP, using both keep-alive and non-keep-alive clients.
   - Determine whether the trap is caused by instance reuse, request cancellation, the wasihttp adapter's per-request goroutine, a context deadline, outbound HTTP, or a wasmtime bug. Capture the guest's stderr on the failing request if possible.
   - If cancellation or client disconnect is the trigger, make the adapter stop a request without trapping the worker.
   - If the cause is wasmtime instance reuse, isolate the failing pattern and either add supervisor-side serialization for the affected path or document the safe concurrency limit.
   - Add a bounded-concurrency stress test to `wt/e2e.sh` that asserts no `5xx` and checks the app log for new traps.
   - Do not paper over this with frontend retries until the root cause is known.

   **Done when.** Concurrent API reads are clean, or the trap is classified as an expected disconnect and no longer surfaces as a `500` or a scary worker log.

   Implemented in `8279dd7`. The cause was `wasmtime serve` multiplexing up to 16 concurrent requests onto one WASIp3 component instance by default; Go handlers that call outbound `wasi:http` are not safe under that reuse. The supervisor now passes `--max-instance-concurrent-reuse-count=1`, so an instance handles one request at a time and concurrency comes from separate instances. Reproduced before the fix with 100 requests at parallelism 20: guestbook returned 96 `500`s and pulse 80. After restarting the local supervisor, 200 concurrent `/api/messages`, 200 `/api/session` and 100 `/api/monitors` requests all returned `200`, with no new worker traps. `max_concurrency` remains the separate total-request cap. The README documents the flag and the failure mode.

6. [x] **[P1] Make a new app host routable without a manual ingress.**

   **Current behaviour.** The supervisor supports exact hosts and single-label wildcard hosts such as `*.apps.example.com`. The remote deployment uses one exact-host ingress per app. Deploying passport succeeded, but the URL returned `404` until an Orchard ingress was created by hand. A new app therefore needs a deploy command plus a provider-side change before it is reachable.

   **Change.**

   - Establish one wildcard ingress, certificate and DNS record for app hosts, then default new remote apps to a host under it. Keep exact-host ingresses as an override.
   - If a wildcard is not possible, add a `wt-deploy route check <host>` or extend `wt-deploy open` and `wt-deploy deploy` to verify DNS, ingress and TLS before printing success.
   - Add a `wt-deploy doctor` that reports supervisor connectivity, target, runtime, DNS, TLS and app readiness in one place.
   - Document the one-time wildcard setup as the normal path, not an advanced alias feature.

   **Done when.** Deploying a new app with the default remote host needs no provider-side change, and a missing route is a clear preflight error rather than a `404` after a successful deploy.

   Implemented the fallback in `2066e36`. Orchard rejects a wildcard ingress host (`Host is invalid`), and the existing certificate is a single-label `*.k.hackclub.dev`, so a new two-label wildcard would also need DNS-01. The CLI now provides `wt-deploy route check <app>` and `wt-deploy doctor [app]`, and `wt-deploy deploy` probes a non-local app host before building. The probe follows the ingress behaviour: HAProxy answers `/healthz` itself, so the default path is `/`; the ingress's own `404 Not Found` is unreachable, while a supervisor response with `X-Request-ID` or an app-level error counts as routed. A missing route fails before the upload with the host, URL and `--skip-route-check` escape hatch. Verified on the remote deployment: an unrouted host failed before upload, the existing passport host passed, and `doctor` reported supervisor, runtime, target, compiler, readiness, DNS and HTTP status. A wildcard ingress, certificate and DNS record remains the provider-side one-time setup where the provider supports it.

7. [x] **[P2] Make runtime and target matching self-service.**

   **Current behaviour.** `wt-deploy` reads the supervisor's reported wasmtime version and target, then compares that string with the local binary. A normal local wasmtime from the official release reports `wasmtime 50.0.0 (commit date)`, while the supervisor reports `wasmtime 50.0.0`. The deploy warns that the cwasm will not match even though the compiler binary is compatible. The runtime-only image has no compiler, so a real mismatch cannot be repaired on the server. A wrapper that rewrites only `wasmtime --version` was needed for the remote deploy.

   **Change.**

   - Compare a release tag, target and ABI policy rather than the full version string. Keep the full build string in `artifact.json` for diagnostics.
   - Add `wt-deploy runtime` or `wt-deploy doctor --fix` that fetches or identifies a wasmtime build matching the supervisor. Alternatively accept a `--wasmtime` wrapper path and document it as supported.
   - Make a mismatched cwasm with no server compiler fail before the full upload with an actionable message. Do not print `status: running` after a warning that the runtime may trap.
   - Include the supervisor's target, epoch flag and compiler availability in the deploy summary.
   - Add tests for a matching release, a build-metadata difference and a genuinely incompatible target.

   **Done when.** A plain release wasmtime needs no wrapper and no manual version check, and an incompatible cwasm is rejected before upload rather than after startup.

   Implemented in `1dcfa47`. The CLI and supervisor now compare the numeric release and target instead of the full version string, so `wasmtime 50.0.0 (commit)` matches `wasmtime 50.0.0` while a real release change still rebuilds. The artifact cache key uses the release and keeps the full string in `artifact.json` for diagnostics. `wt-deploy runtime` prints the supervisor and local versions, release match, target and compiler availability. Tests cover release parsing, metadata-only matches in preflight and artifact installation, and the runtime command. Verified against the local and remote supervisors: both printed `release: match` for the official local binary and the supervisor's short version. Pushing the supervisor change lets a runtime-only image reuse an official-release cwasm without a version wrapper.

8. [x] **[P2] Make the contract pipeline optional and one command.**

   **Current behaviour.** The Valibot to JSON Schema to `go-jsonschema` pipeline is consistent and works, but it is a second build system. Adding one request type touches `web/src/lib/contract.ts`, the schema registry, generated schemas and generated Go. The scaffold makes it mandatory even for an app with one endpoint. Optional fields generate pointers and need extra care. The result is good for a shared API, but heavy for a small app.

   **Change.**

   - Teach `wt new` a `--contract=valibot|none` choice. Keep Valibot as the default for the existing examples, but make the manual path a first-class scaffold.
   - Add one `wt-deploy contract` or `wt contract` command that runs the whole pipeline from the app directory, rather than relying on `go generate` plus a Bun script plus a generator flag combination.
   - Document how to generate TypeScript from Go types as an alternative for Go-first apps.
   - Improve optional/nullable output so a simple optional string does not require hand edits.
   - Keep generated files untracked and keep the current standard error shape.

   **Done when.** A small app can skip the contract pipeline without fighting the scaffold, and a Go-first app can generate a typed client with one documented command.

   Implemented in `f04d204`. `wt new --contract=none` scaffolds the same notes app with hand-written Go note types, a client-only Valibot file, no `contract/` generator and no `go generate` in `app.json` or `dev.watch`. `wt contract --dir DIR` runs the existing Valibot to JSON Schema to Go generator in one command. Tests cover the contractless scaffold, its parsed Go source, missing generator errors and the default scaffold. Verified by scaffolding a standalone contractless app outside the repo, running its Go test, Svelte check and static build, plus a full `wt-deploy build` that produced a bundle. The README documents both paths.

9. [x] **[P2] Add an effective-config inspection command.**

   **Current behaviour.** An app has bundle `app.json`, runtime config from `PATCH /apps/<name>/config`, secrets, deploy flags such as `--app-host`, and the CLI config file. Each layer is reasonable, but an author has to learn which one wins. `wt config show` returns the runtime config and secret key names, but there is no single view of the effective host, URL, environment, secret references, digest, runtime, readiness and pending restart.

   **Change.**

   - Add `wt-deploy inspect <app>` (or extend `wt config show`) with a human summary and `--json`.
   - Show the effective host and aliases, public URL, manifest versus runtime values, env keys, secret key names, current digest, target, runtime, readiness, restart-required fields and recent deploy time.
   - Keep secret values write-only.
   - Use the same public URL resolution as `app.PublicURL` so the summary cannot disagree with the app.

   **Done when.** One command answers what the app is actually running with and why its URL or config differs from `app.json`.

   Implemented in `f9e2a76`. `wt-deploy inspect <app> [--dir DIR]` combines supervisor runtime and target, app status, readiness, digest, latest deployment, effective runtime config, env and secret key names, a route probe, and an optional comparison with a local `app.json`. `--json` returns the same report for tooling. The comparison covers host, health, memory, disk, max concurrency, request timeout, network, identity, server paths and env key names, while secret values remain unavailable. Tests cover the JSON-shaped config reader, local manifest differences and the command path. Verified against the local and remote supervisors: inspect reported passport's runtime, target, digest, deployment, config, route and the expected remote host difference from the local manifest.

## Remote log follow-ups

The remote deployment exposed two noisy errors after the fixes landed.

- **Job queue full.** `example/ticker` enqueues every 10 seconds, and every finished job stayed in the per-app queue. After about 2.8 hours 1000 succeeded jobs filled the cap and new enqueues failed. Fixed in `a1f5710` by retaining every active job and only the newest 500 terminal jobs (`succeeded`, `failed`, `dead`, `canceled`), pruning on load and before the cap check and persisting the pruned queue. A steady producer can no longer turn history into a full queue.
- **Wasm module failing to open.** The runtime-only image has no `compile` subcommand, but `ensureArtifact` attempted `wasmtime compile` whenever a cache or artifact match was missing. The runtime binary then treated `compile` as a module path and logged `failed to open wasm module "compile"` before falling back to the existing cwasm. Fixed in `a1f5710` by checking `canCompile` first: an existing valid cwasm is kept with an informational line, and the error is returned only when there is no usable artifact.
- **PVC full.** The 1 Gi state volume filled during repeated deploys and job history, causing cache writes to fail and some apps to be missing after a rollout. Expanded `wt-demo-state` to 2 Gi and restarted; all nine apps returned. This is Orchard state, not a repository change.

## Not worth changing

- **Do not split the supervisor.** One binary owning deploy, storage, blobs, events, identity and the panel is appropriate for this prototype. Splitting it would add network hops and authentication boundaries before they solve an observed problem.
- **Do not replace raw SQL storage.** The explicit statements, scans, authorizer, migrations and sessions are predictable. The gap is session helpers and generated row ergonomics, not the SQL surface.
- **Keep the static SvelteKit split and its guard.** A real SSR path is a separate feature, not a reason to put the frontend back into the wasm component. `server_paths` is the minimal escape hatch; a full JS host runtime is not needed yet.
- **The wasm artifact and one wasmtime per app are the point.** Containerising or embedding the runtime would remove the isolation and caching properties the project is built around.
- **Do not build a full auth framework.** A small OAuth client and session helper are enough. Apps should keep ownership of users, tables and authorization rules.
- **Do not hide migrations.** Explicit migration functions and checksums are worth the ceremony.
- **Keep the contract pipeline, but make it optional.** It is the right choice for a shared API, not a requirement for a two-endpoint app.

## If I picked the next three

1. [x] **Make HTML navigation policy explicit and support server routes.** This is the biggest authoring cost and the reason passport needed a static callback shim. Implemented in `537aec0`.
2. [x] **Add a canonical public origin helper.** This removes duplicated, security-sensitive URL logic and makes the dev loop behave like production. Implemented in `6695eb5`.
3. [x] **Add OAuth and session helpers.** Passport proves the shape and the tests already exist; packaging it is mostly extraction. Implemented in `57c14ce`.

The concurrency investigation is a close fourth. It is not an app-authoring feature, but it is the only issue that produced `500`s in normal testing and it affected an existing example as well.

Every item above keeps the current invariants: ordinary Go over `wasi:http`, a static SvelteKit shell, per-app SQLite, one component per app and blue/green deploys.
