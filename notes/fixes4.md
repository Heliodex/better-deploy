# Fixes 4

App-authoring review after building `wt/example/pulse`, 2026-11-02. This pass looks at the platform from the app author's side: what was easy, which APIs were missing, and where the build and dev loops fought back. Prior priorities stay in `notes/fixes.md`, `notes/fixes2.md` and `notes/fixes3.md`; this list only adds work those notes do not already cover.

The short assessment: the runtime and SDK are strong enough that pulse is mostly ordinary `net/http`, SQL and a static SvelteKit page. The weak points are build orchestration around the component, wire-contract duplication, dev-loop parity for host-owned routes, and the lack of test, job and blob helpers. None of these questions the wasm execution model or the supervisor topology.

Concrete evidence from this build:

- The first deploy reached a supervisor from before the queued deployment resource and failed with a raw `405 Method Not Allowed` after the full build.
- An `aria-label` change in the Svelte page re-ran `componentize-go` and uploaded the full wasm component again, about 12 MB, even though the resulting digest was unchanged.
- The component input hash walks the app directory, so the frontend source forces component rebuilds, while the replaced SDK under `wt/packages` and the WIT are outside the hash.
- The dashboard's live updates, blobs and identity are supervisor routes. The dev proxy can reach them, but the default only forwards `/api`, so the app silently falls back to polling unless the app author knows to add the prefixes.
- There is no Go helper for durable jobs, no in-memory storage test helper, and no example using blobs, identity, jobs or app-to-app calls.

## What held up

- `wasihttp`, `app.ServeStorage`, `storage.Client`, `app.ReadJSON` and `app.WriteJSON` kept the app code ordinary. Almost nothing in pulse needed to know it was a wasm component.
- Static SvelteKit served by the supervisor is the right split. Assets never enter the wasm file, and a frontend change should not need a component build once item 1 is fixed.
- `schedule` in `app.json` drove the whole monitor. Deploy, readiness, digest, logs and rollback were enough primitives, without a job runner or cron library.
- The event bus is small: `app.Publish` on the server and `EventSource` on the client gave live updates in a few lines and worked first try.
- Raw SQL with explicit scans is predictable. Migration checksums and the expand/contract guidance are more thoughtful than most prototypes.
- The deploy loop is fast when the cache hits: build, compile, upload, wait for readiness and print the app is a good operator experience.

## Priority ordered

1. [x] **[P0] Make the component input set explicit and cache component and static builds separately.**

   **Current behaviour.** `componentInputHash` in `wt/deploy/cache.go` walks the whole app directory with a denylist. It ignores `static`, `web/build`, `web/.svelte-kit` and `node_modules`, but it hashes `web/src`, so every Svelte source edit invalidates the component. It does not hash the replaced SDK modules under `wt/packages` or the WIT, so an SDK change can leave the cache valid and reuse a stale `main.wasm`. `prepareComponent` then skips the entire `build` command when the hash matches, and `deploy` only keeps the running component when that whole build was skipped and the digest matches.

   **Change.**

   - Replace the denylist with explicit inputs: Go sources, `go.mod`/`go.sum`, the replaced SDK packages, the WIT and the component build command. A generated `component_inputs` list in `app.json` or a `go list -deps` walk would both work; the important part is that the SDK cannot fall outside the cache key.
   - Split the manifest build into a component phase and a static phase, for example `build_component` and `build_static`, keeping the current `build` string as a fallback. Cache each phase separately.
   - Always run the static phase when frontend inputs change and always stage `static/` in the bundle. Always compute the wasm digest and include the component only when the running app does not have it.
   - Print which phases ran and which were reused, so a static-only deploy is visible rather than inferred.

   **Done when.** A frontend-only edit runs only the frontend build, uploads no wasm and leaves the running digest alone. A Go or SDK change rebuilds the component. A change under `wt/packages` invalidates the app's cache. A fresh checkout builds both phases, and the deploy output says which path it took.

   Implemented in `f830bc6` and `309b9f3`. `build_component` and `build_static` are separate cached phases; `build` remains the legacy single-command fallback. `.wt-build.json` now stores `componentHash` and `staticHash` independently. The component hash covers Go and WIT files, the nearest `go.mod`/`go.sum` (including a parent module above the app directory), every local `replace` target resolved from that module such as `../packages`, and explicit `component_inputs`. The static hash covers the `web` directory (or `dev.web`) and explicit `static_inputs`, excluding `web/build`, `static`, `*.gen.go`, `packages/binding` and `packages/tmp`. Deploy always computes the wasm digest and omits the component when the supervisor already runs it, even if the component was rebuilt. Verified on pulse: a `web/static/robots.txt` edit reused the component, rebuilt static and uploaded about 44 KB instead of the 12 MB component; a Go edit rebuilt only the component; a second build reused both phases. The unit tests cover the parent-module and `wt/packages` invalidation, explicit component inputs and the legacy fallback. The phase summary prints `component: reused|built`, `static: reused|built|skipped` and `component: already running; uploading static only`.

2. [ ] **[P1] Add a supervisor capability endpoint and make the CLI preflight against it.**

   **Current behaviour.** The local supervisor predated the queued deployment resource, so `POST /apps/pulse/deployments` returned `405 Method Not Allowed`. The CLI reported the raw status only after building the bundle. The compile target also lives on the operator side: this machine needed `--target x86_64-unknown-linux-gnu` and a specific wasmtime binary to match the supervisor, while the defaults point at musl. Mismatches are recovered by a server-side recompile or a failed start, not by a clear preflight.

   **Change.**

   - Add `GET /version` or `GET /capabilities` to the supervisor, authenticated like the rest of the API, returning the supervisor version, deployment protocol revision, wasmtime version, target triple, epoch flag and named features such as deployments, jobs, blobs, identity and events.
   - Have `wt-deploy` query it before building. If the supervisor does not support the deployment resource, fail with "supervisor predates queued deployments; rebuild or upgrade it" instead of a 405 after a 12 MB build.
   - Use the reported target and runtime to choose the compile target, validate the local wasmtime version and warn when the cwasm will be rebuilt on the server.
   - Include the same capability summary in `wt apps` or `wt config show` so an operator can see the negotiated target without reading process arguments.

   **Done when.** Deploying to an old supervisor fails before the build with an actionable message. A matching local deploy needs no manual `--target` or `--wasmtime` flags. The deploy result says whether the uploaded cwasm matched or the server will compile.

3. [ ] **[P1] Give apps one source of truth for the wire contract and a standard error shape.**

   **Current behaviour.** Pulse hand-writes the TypeScript interfaces and validation in `web/src/lib/api.ts`, the Go response structs in `main.go`, and the trim and validation rules on both sides. Guestbook has a source-of-truth pipeline, but it is three steps: Valibot in `web/src/lib/contract.ts`, `web/scripts/contract.ts` to JSON Schema, then `go generate ./contract/` through `go-jsonschema`. It works, but it is a second build system for a small API, and a nullable nested history is enough friction to make skipping it the easier choice. Errors are a plain `{error: string}` with no stable code or field.

   **Change.**

   - Pick one supported source and generate the other side. Go structs to TypeScript types plus Valibot schemas and a small typed client is the smaller direction for this repo; TypeScript to Go is also viable if the generator handles `null`, arrays, enums and maps without hand edits.
   - Make it one command in `wt new` and the app build, not a bespoke per-app pipeline. Keep the manual path for tiny apps.
   - Extend `app.WriteError` with a stable code and optional field, for example `{error: {code, message, field}}` while keeping the old string body readable for existing clients.
   - Generate or share request and response validation so the client rejects bad input before `fetch` and the server exposes the same field names.

   **Done when.** A field rename is one edit and both sides still compile. A test sends a missing field and an extra field and gets a stable code. The client can distinguish `invalid`, `not_found`, `conflict`, `limit` and `unavailable` without matching message text.

4. [ ] **[P1] Add a typed durable-jobs SDK, a CLI and an example.**

   **Current behaviour.** `schedule` is the only background primitive exposed in `app.json`, and it works well for a fixed recurring call. Durable jobs exist in the supervisor, but `wt/packages/app` has no job types or enqueue helper, so an app that needs a delayed run, retries, a payload or dead-letter handling drops to raw JSON on the supervisor API. There is no `wt jobs` command either. Pulse's "check now" is synchronous because a delayed or retried check has no comfortable client path.

   **Change.**

   - Add typed job helpers such as `app.EnqueueJob(ctx, job)` with path, method, body, headers, `runAt`, `maxAttempts` and `idempotencyKey`, plus a result type that exposes the job ID and state.
   - Add `wt jobs <app> list|show|enqueue|cancel` over the existing supervisor API.
   - Add a small example that uses a job for retry or delayed work, or extend `ticker` to enqueue a job instead of only logging.
   - Document the at-least-once delivery model, the 64 KiB payload limit and the difference between a schedule and a job.

   **Done when.** An app can enqueue and observe a job without hand-written JSON, and an operator can inspect idle, running, dead and failed jobs from the CLI.

5. [ ] **[P1] Make supervisor-owned routes work by default in the dev loop.**

   **Current behaviour.** `wt dev` starts a real dev supervisor and Vite proxies to it with the app's `Host`, so `/v1/events`, `/_wt/blobs` and `/_wt/identity` can work. But `appDevConfig.Proxy` defaults to `["/api"]`, and its comment says prefixes are "forwarded to the wasm app", which makes host-owned routes sound out of scope. A pulse-style dashboard therefore falls back to polling in dev, and a blob or identity app would appear broken until the author added the prefixes by hand.

   **Change.**

   - Default the dev proxy to `/api` plus the platform prefixes the supervisor owns: `/v1/events`, `/_wt/events`, `/_wt/blobs` and `/_wt/identity`. Keep `/api` as the app prefix and add an explicit `supervisor_proxy` or opt-out list if an app wants the defaults off.
   - Correct the comment and the CLI help so the distinction between app routes and host routes is visible.
   - Add a dev test that opens an `EventSource` through Vite and receives one event, and one that follows a signed blob URL.

   **Done when.** Pulse's live updates, a blob upload and a platform identity login all work under `wt dev` with no `app.json` proxy change.

6. [ ] **[P2] Make app handlers and storage testable without deploying.**

   **Current behaviour.** The examples build a package-level `store` in `init()` and register through `wasihttp.Handle`. Handlers are not constructed with a store, so a test has to set environment variables, run a storage service and share globals. `storage.Client` accepts an `http.Client`, so injection is possible, but there is no in-memory storage server or fake that understands the statement shapes. The template has no handler test, and `go test ./...` in an example fails in the generated binding export stub with `missing function body` before it reaches app code.

   **Change.**

   - Scaffold an app struct with the store, mux and handlers as fields, a `New(store)` constructor and a `Handler()` method. Keep `init()` and `main()` as the wasm wiring only.
   - Provide `storage.NewTestServer` or an in-memory fake with `Query`, `Exec`, `Batch`, `Migrate` and `WithTx` so handler tests exercise real responses and error codes.
   - Add one handler test to the `wt new` template: a request body, a stored row and an error path, passing without wasmtime.
   - Consider an `app.Run(store, migrate, mux)` helper to remove the repeated `ServeStorage` plus route registration boilerplate, but do not hide the explicit migration.

   **Done when.** The scaffolded app has a passing handler test with a fake store, and an author can test validation and storage errors without running `wt dev` or deploying.

7. [ ] **[P2] Improve CLI flag parsing and deploy output.**

   **Current behaviour.** `clientFlags` uses `flag.NewFlagSet`, which stops at the first positional argument. `wt logs --host http://127.0.0.1:8080 pulse --tail 40` works, but `wt logs pulse --host http://127.0.0.1:8080 --tail 40` fails with "flag provided but not defined: -tail". Log-specific flags go after the app and client flags before it, which is easy to get wrong. `deploy` prints the raw app JSON on success, while `apps` prints a table.

   **Change.**

   - Parse flags anywhere on the command line, or keep the split but make the error explain it and show a working example.
   - Print a human deploy summary: app, URL, digest, whether the component was reused or built, whether static was rebuilt and how long it took. Keep `--json` for machines.
   - Align the help text and examples with the actual parser, and make `--json` consistent across commands.
   - Consider surfacing the capability preflight from item 2 in the deploy summary.

   **Done when.** The help examples run verbatim, including `wt logs pulse --host ... --tail 40`, and a successful deploy has a readable summary without losing the JSON output.

8. [ ] **[P2] Give blobs a lifecycle helper and a recovery path.**

   **Current behaviour.** `app.BlobUploadURL`, `BlobDownloadURL` and `BlobDeleteURL` cover signing and deletion. There is no list or garbage collection, and the docs correctly say the supervisor cannot know which files an app still references. An app has to store the digest in its own metadata and call delete when the record goes away. If metadata is lost, the blob stays in the store forever.

   **Change.**

   - Document and, if useful, ship an app-side pattern that writes blob metadata with the same transaction as the owning row and deletes on row removal.
   - Add a supervisor admin listing of blob digests, sizes and ages, and an optional age-based garbage collection for unreferenced blobs.
   - Consider an `app.WithBlob` helper that records metadata and signs, but keep the explicit delete path so an app can opt out.
   - Add a small upload example so the signed-URL flow is exercised end to end.

   **Done when.** An app can find or purge orphaned blobs without host shell access, and a normal delete removes both the metadata and the blob.

9. [ ] **[P2] Fill the platform-API example gap and publish a choose-the-API map.**

   **Current behaviour.** Several examples exercise storage, but no example exercises blobs, platform identity, durable jobs or app-to-app calls. Pulse adds SSE. Those documentation-only features feel riskier and more complex than they are, and the concept count is high for a new app author: storage, blobs, events, identity, jobs and app fetch.

   **Change.**

   - Add one small example using blobs and platform identity, for example an authenticated image board, and one using jobs or app-to-app calls, for example a worker that retries delivery.
   - Add a short "which API do I need?" map to the README: request and response state uses storage, long or retried work uses jobs, push to browsers uses events, files use blobs, the current user uses identity, another app uses app fetch.
   - Extend `wt/e2e.sh` for the new examples where the runtime supports it.
   - Keep the examples in the pulse style: one Go file or one small Go package, a static SvelteKit shell and a short README section.

   **Done when.** Every platform feature either has a runnable example or is explicitly marked unsupported, and a new author can choose an API from one table.

## Not worth changing

- **Do not split the supervisor.** One binary owning deploy, storage, blobs, events, identity and the panel is appropriate for this prototype. Splitting it would add network hops, authentication boundaries and a second deployment target before it solved an observed problem.
- **Do not replace raw SQL storage.** The explicit statements and scans are predictable, and the authorizer, migrations and session model are already careful. The gap is test support and maybe generated row helpers, not the SQL surface.
- **Keep static-only SvelteKit and its guard.** `checkStaticSvelteKit` turns a confusing runtime failure into an early build error. A real SSR path belongs in a future JS host runtime, not in the current adapter.
- **No WebSocket bridge.** SSE is enough for server push and is simpler to proxy. WebSocket would add a host-side upgrade path for a feature no example needs.
- **The wasm artifact and one wasmtime per app are the point.** Containerising or embedding the runtime would remove the isolation and caching properties the project is built around. The complexity that hurts is build orchestration and concept count, not this topology.
- **Keep the contract pipeline optional.** If item 3 lands, make it one command with a manual fallback rather than a required framework.

## If I picked the next three

1. [x] **Split component and static builds, and replace the cache key.** This is the biggest daily cost and the stale-SDK case is a correctness bug. Implemented in `f830bc6` and `309b9f3`.
2. [ ] **Add the capability endpoint and deploy preflight.** It is small, removes the 405 class of failures and makes runtime and target matching automatic.
3. [ ] **Add one-source contract generation and a standard error shape.** It is the biggest remaining authoring cost after the build loop.

The dev proxy defaults from item 5 are a close fourth because they are cheap and make the realtime, blob and identity features testable in the normal loop. Every item above keeps the current invariants: ordinary Go over `wasi:http`, a static SvelteKit shell, per-app SQLite, one component per app and blue/green deploys.
