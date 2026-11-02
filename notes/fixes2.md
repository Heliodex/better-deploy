# Fixes 2

Architecture and API review, 2026-11-01. Open work only: no code changed in this pass. Priorities are about leverage, not effort. The previous list stays in `notes/fixes.md`; do not re-litigate items already checked there.

The short assessment: the spine is right and unusually well executed for a prototype. Wasm as the deploy artifact, cwasm as a cache, blue/green with draining, the storage authorizer, sessions, checksummed migrations, secrets, scoped tokens, backups, jobs, metrics, and the test suite are all in good shape. The criticisms below are about the layer above the code: what the platform API is, how authority reaches an app, and what the developer experiences daily.

## Fixed in this pass

- [x] **Untrusted app namespaces and seccomp.** Untrusted launches now use user, PID, mount, IPC and UTS namespaces, a minimal root, a PID 1 signal-forwarding helper, a capability drop and a seccomp denylist. Both boundaries are mandatory in untrusted mode. The network namespace remains open.
- [x] **Environment merge.** `mergeEnv` deduplicates the minimal environment and the app environment so an app's `PATH`, `HOME` or `TMPDIR` actually overrides the supervisor's values; the previous concatenation left the first duplicate in place for libc `getenv`.
- [x] **Tests and docs.** Unit tests cover the namespace plan, argument rewriting, sysproc attributes, the seccomp filter and the mandatory-config check. `WT_NS_TEST=1` and `WT_SECCOMP_TEST=1` run the opt-in integration tests. The README documents the boundary, the kernel and container requirements, and the remaining network gap.
- [x] **Migration phase before readiness.** `app.ServeStorage` exposes `POST /_wt/migrate`; the supervisor calls it after port discovery with `WT_STORAGE_MIGRATION_TIMEOUT`, then probes health with the normal 3-second timeout and a fresh `startup_timeout`. Migrations run with `context.WithoutCancel`, so a disconnected probe does not abort them. Bundles without the endpoint fall back to health-only readiness.
- [x] **Per-app operation locks.** Deploy, config, lifecycle, backup restore, rollback and database import now serialize per app instead of globally. The shared artifact cache keeps its own mutex so concurrent per-app deploys cannot race its lookup, compile, install and prune steps.
- [x] **Persistent deploy idempotency.** The 24-hour `Idempotency-Key` replay now lives beside the app's deployment history, survives restarts, keeps the newest 64 keys per app, and is cleared on delete. The in-memory map and its wholesale 512-entry clear are gone.
- [x] **WAL and temp accounting.** Write connections set a quota-scaled `journal_size_limit` and `wal_autocheckpoint`. SQLite temp files go to a dedicated `WT_STORAGE_TMP_DIR`, and a global `WT_STORAGE_TEMP_BUDGET_MB` monitor rejects new storage requests with `507 temp_budget` while the shared directory is over budget. Stale `etilqs_*` files are removed at startup.
- [x] **Invisible supervisor upgrades.** Apps are now children of one app-runner process with a liveness pipe. `SIGHUP` execs the supervisor binary in place while inheriting the storage and app-fetch listeners and the runner FD; the new supervisor reconnects, restores app tokens and adopts the running PIDs and ports. A real handover test verifies the app PID is unchanged.
- [x] **Access-log defaults.** `WT_ACCESS_LOG` and `WT_ACCESS_LOG_EVERY` now populate `configFromEnv`; the previous zero default made `parseConfig` reject every normal startup with `access-log-every must be between 1 and 10000`.
- [x] **App-to-app body signatures.** The supervisor signs the body's SHA-256 with the target token in `X-WT-App-Signature-V2`, and `app.Caller` verifies and restores the buffered body. Trusted mode still sends the legacy metadata-only signature for older targets; untrusted mode sends v2 only.
- [x] **Artifact cache CPU policy.** `artifactKey`, `artifactInfo` and `wt-deploy` artifacts now record the baseline CPU feature policy, so the cache digest changes with it and a future native compile cannot silently share a baseline entry. Legacy artifacts without the field still match.
- [x] **SvelteKit dev loop.** `wt dev` now runs Vite with HMR, proxies `dev.proxy` to the supervisor with the app Host, and rebuilds only the wasm component from `dev.build` on Go, WIT, contract or `dev.watch` changes. `--vite=false` keeps the single-tier loop.
- [x] **Static SvelteKit boundary.** The README documents the supported adapter-static/prerender shape, and the build rejects server-only SvelteKit files instead of letting them fail only in production.
- [x] **Response contracts.** The Valibot registries now cover responses as well as requests. Response schemas generate Go response structs, registered nested schemas become `$ref` files so envelopes reuse the item type, and the client validates responses through the same source-of-truth schemas.
- [x] **Queued deployment resource.** `POST /apps/<name>/deployments` stages the upload, returns `202` with a `Location`, and runs install and readiness in the background. Entries report queued/running/success/failed/canceled, expose a plain-text log and an SSE stream, and can be canceled. `POST /apps/<name>` stays as the synchronous compatibility alias.
- [x] **Signed blob store.** Each app has a content-addressed blob store at `WT_STATE_DIR/.blobs/<app>`, signed with a per-app HMAC key in `WT_BLOB_SECRET`. Uploads hash the body and enforce a declared maximum; downloads support `Range`, `HEAD`, `ETag` and `If-None-Match`; deletes are signed. Blob bytes count toward `disk_mb` and the global budget, and the wasm component never handles the file body.
- [x] **Platform end-user identity.** Apps opt in with `identity: "platform"`. The supervisor stores users and sessions under `WT_STATE_DIR/.identity`, hashes passwords with PBKDF2-SHA256, and serves login, logout, `GET /v1/me` and password change on the app host with a `wt_user` cookie and CSRF token. Admin routes create, list, update, reset and delete users. Proxied requests get signed `X-WT-User-*` headers and `app.UserFromRequest` verifies them; client-supplied identity headers and the session cookie never reach the app.
- [x] **Readiness, dependencies and maintenance.** `GET /readyz` is separate from `/healthz`, `depends_on` gates an app host until its dependencies are ready (transitively, with cycle rejection and forward references), and `maintenance` serves a supervisor page with `503` and `Retry-After` without stopping the process.
- [x] **Host event bus.** Apps publish to a per-app bounded bus through the storage loopback (`/v1/publish`), browsers subscribe with SSE on the app host (`/v1/events`), and storage writes emit compact `storage`-topic triggers. Cursors, `Last-Event-ID`, gap events and authenticated app-to-app publishes are supported; the stream is public to the app origin and topics are not authorization boundaries.

## Do not regress

These are the choices that make this worth continuing. Keep them unless a replacement is strictly better.

- [x] Wasm is the artifact and cwasm is a cache keyed by digest, runtime, target and epoch. Do not go back to a compiled blob as the unit of deployment.
- [x] Blue/green with request draining, static-only deploys that reuse the running component, and content-addressed static files.
- [x] Per-app SQLite with an authorizer instead of a word denylist, a `query_only` read pool, checksummed migrations and typed errors.
- [x] Transaction sessions with a pinned connection. This is the primitive most platforms never get right; keep `WithTx` as the default way to do read-modify-write.
- [x] Scoped tokens, audit, write-only secrets, the backup tree, durable jobs, metrics and per-app log filtering.
- [x] Race tests, fuzzing, a real wasmtime e2e and the image e2e in CI.

## P0: architecture

1. [ ] **Replace `wasmtime serve` with an embedded host.** This is the one change with the largest payoff and the highest cost. The stock CLI is why platform services are loopback HTTP plus bearer tokens, which is ambient authority in a capability sandbox. Once an app has `inherit-network` and a storage token, every remaining isolation feature is host-level plumbing rather than a capability.
   - [ ] Run one supervisor process with one `Store` per app instead of one `wasmtime serve` process per app.
   - [ ] Expose storage, jobs, pub/sub and identity as typed WIT imports with per-app handles.
   - [ ] Remove the per-app storage token, the app-fetch bearer and HMAC, most of the UID allocator and the trusted-mode egress rules from the app-facing path.
   - [ ] Decide the compile story for the runtime image: Cranelift in the serving image, a builder sidecar, or precompiled-only with a hard failure when no matching cwasm exists.
   - [ ] Keep the network-namespace path only as a fallback. A namespace preserves the HTTP control plane and therefore most of the current complexity; the embedded host removes it instead.

2. [ ] **Measure density before claiming the premise.** The container count went down, but the process count did not, and every app carries a full wasmtime runtime.
   - [ ] Benchmark idle RSS per app, supervisor boot time at N apps, and requests/s at N apps, for N = 1, 10, 50.
   - [ ] Add the result to the README so "no container per app" is a measured claim rather than an assumption.

3. [ ] **Close the untrusted-isolation gap.** Untrusted mode now has user, PID, mount, IPC and UTS namespaces plus seccomp, but still shares the host network namespace and depends on the iptables owner rules. For hostile code that is still behind a container on the network path, and the seccomp filter is a denylist rather than an allowlist.
   - [x] Add user, PID, mount, IPC and UTS namespaces with a minimal tmpfs root that exposes only the bundle, `/data`, `/proc`, `/tmp` and the runtime's files; pivot into it, map container uid/gid 0 to the app uid, drop capabilities and clear supplementary groups.
   - [x] Add a PID 1 re-exec helper that forwards signals and reaps the wasmtime child, and report the child's host pid so app info and resource charts still describe wasmtime.
   - [x] Add a seccomp denylist that rejects ptrace, bpf, perf_event_open, userfaultfd, key management, mount/umount/pivot_root, setns/unshare, module and kexec loading, reboot, swap, syslog, quota and clock changes, the new mount API and replacing the filter through prctl.
   - [x] Make both boundaries mandatory in untrusted mode, add `WT_APP_NAMESPACES`/`WT_APP_SECCOMP` and their flags, and document the boundary, the kernel requirements and the remaining network gap in the README.
   - [ ] Add a network namespace with a veth pair, addresses, routes and an in-namespace egress policy, or move the app listener into the embedded host so the proxy does not need a route into the namespace.
   - [ ] Replace the seccomp denylist with an allowlist derived from a traced wasmtime workload, or keep the denylist and prove it covers the runtime on each upgrade.
   - [ ] Add an integration test that runs a real wasm component in the full untrusted path, including the per-app cgroup, iptables rules and seccomp.

## P1: correctness and operations

4. [x] **Separate migration from readiness.** Launch used `startup_timeout` (default 15s) as the readiness deadline, migrations can take 5m, and migrations ran inside the first health request. A legitimate first migration over 15s failed the deploy, while the 500ms startup probe raced a migration that took longer than that.
   - [x] Add an explicit migrate phase between install and readiness: the supervisor calls the app's reserved `POST /_wt/migrate` endpoint after port discovery and before health, with `WT_STORAGE_MIGRATION_TIMEOUT` as its budget.
   - [x] Run the SDK migration on `context.WithoutCancel`, so a disconnected probe or migrate client does not abort it.
   - [x] Probe health with the normal 3-second timeout and a fresh `startup_timeout` deadline after migration; bundles without the endpoint fall back to health-only readiness.
   - [x] Add SDK tests for the migrate endpoint, failure, unavailable storage, wrong method and client cancellation, and supervisor tests for success, 404 fallback, failure, 503 retry and a slow migration that exceeds `startup_timeout`.
   - [ ] Exempt the migration request from the app's `request_timeout`, or run the migration in a separate process; today a migration longer than the guest request timeout can still be interrupted after it has committed.

5. [x] **Make the deploy lock per app.** `s.deployMu` in `wt/supervisor/deploy.go` serialized every app's deploy, including a 256MB upload and the readiness wait. One slow app blocked all deploys.
   - [x] Replace the global lock with one mutex per app for deploy, config, lifecycle, backup restore, rollback and database import.
   - [x] Add a separate cache mutex for the shared artifact cache, because per-app concurrency would otherwise race cache lookup, compile, install and pruning.
   - [x] Add tests that different apps acquire independent locks and the same app still serializes.

6. [x] **Persist deploy idempotency.** `deployReplay` was an in-memory map capped at 512 entries, cleared wholesale when full and lost on restart. "24h replay" was not what the code did.
   - [x] Store each app's idempotency key and response beside its deployment history at `WT_STATE_DIR/.deployments/<app>/idempotency.json`, with `0600` permissions and atomic replacement.
   - [x] Keep the newest 64 keys per app, prune entries older than 24 hours on store, and drop a corrupt file rather than blocking a deploy.
   - [x] Clear the file when an app is deleted, so a recreated app does not replay a deleted app's response.
   - [x] Test persistence across a new deployment manager, expiry, the per-app cap, corrupt files, clearing, file permissions and replay after a supervisor restart.

7. [ ] **Stop treating a `deploy` token as code-execution trust.** A deploy token can upload `main.cwasm`, which is native code fed to wasmtime's deserializer and hardlinked into the cache.
   - [ ] Reject uploaded cwasm for non-admin tokens, or reject it entirely in untrusted mode, and compile `main.wasm` host-side.
   - [ ] Alternatively sign artifacts and verify the signature before import.
   - [ ] Keep the existing rule that the deploy endpoint is the trust boundary, but make the role split match it.

8. [x] **Account for WAL and temp files.** `max_page_count` caps the main database, not the WAL, and `journal_size_limit` and `wal_autocheckpoint` were unset. `temp_store=FILE` spilled into the global `TMPDIR`, shared by every app and invisible to `disk_mb`.
   - [x] Set `journal_size_limit` to `disk_mb / 8`, bounded to 256 KiB through 64 MiB, and a quota-scaled `wal_autocheckpoint` from 16 to 1000 pages on write connections.
   - [x] Point SQLite's process-wide `temp_store_directory` at `WT_STORAGE_TMP_DIR` (default `WT_STATE_DIR/.tmp`, created `0700`) through every connection's DSN.
   - [x] Add `WT_STORAGE_TEMP_BUDGET_MB` (default 512, `0` unlimited), a ten-second monitor and a `507` `temp_budget` rejection for new storage requests while the shared directory is over budget; clean stale `etilqs_*` files at startup.
   - [x] Document that temp files are shared globally, are not part of a per-app `disk_mb`, and that one app's spill can block storage for all apps until the monitor clears the flag.
   - [ ] If true per-app temp attribution is needed, move SQLite into a separate process; in-process SQLite has one process-wide temp directory.

9. [x] **Make supervisor upgrades invisible.** `Pdeathsig` plus no adoption meant every supervisor upgrade restarted the whole fleet.
   - [x] Add one app-runner process that owns every wasmtime child, its log pipe and a liveness pipe. The supervisor connects over a Unix socket; the runner survives an `exec` and replays its app snapshot to the new supervisor.
   - [x] Self-exec on `SIGHUP` with the storage and app-fetch listener FDs and the runner liveness FD in the environment. The public and admin listeners rebind in the new process.
   - [x] Adopt running apps from the runner snapshot in `loadApps`, restoring the effective config, directory, port, child and work PIDs, UID, log file and storage/app-fetch token.
   - [x] Tests cover runner start, output, port discovery, snapshot reconnect, liveness shutdown, supervisor launch through the runner, adoption, inherited listeners, and an opt-in real `SIGHUP` handover that checks the app PID is unchanged (`WT_HANDOVER_TEST=1`).
   - [ ] Add a pre-flight new-binary check or a keeper so a failed `exec` cannot take the service down. The public and admin listeners still have a short accept gap, and in-flight requests on them and on the old storage server are dropped during handover.

10. [x] **Cover the app-to-app body.** The HMAC covered caller, method, path and timestamp, not the body. That was acceptable for trusted apps, not for untrusted ones.
   - [x] Add `X-WT-App-Signature-V2`, an HMAC over the caller, method, path, timestamp and the body's SHA-256, plus `X-WT-App-Body-SHA256`.
   - [x] The supervisor reads and caps the body at 8 MiB before signing and restores it for the target; `app.Caller` reads and restores the body while verifying the digest and signature.
   - [x] Send the legacy metadata-only `X-WT-App-Signature` only in trusted mode; untrusted mode sends v2 only.
   - [x] Test v2 verification, tampered bodies, legacy fallback, stale signatures, missing headers and the untrusted header set.
   - [ ] Stream large bodies through a body digest without buffering the whole request if apps ever need more than 8 MiB.

11. [x] **Reconcile `artifactKey` with the notes.** `notes/fixes.md` said the cwasm cache was keyed by CPU features; `artifactKey` only had wasm digest, wasmtime version, target and epoch.
   - [x] Add a CPU feature policy to `artifactKey`, `artifactInfo` and the uploaded `artifact.json`, set to `baseline` because both compilers always pass `--target`.
   - [x] Include it in the cache digest, reject a mismatch in `artifactMatches`, and accept legacy artifacts that have no CPU field.
   - [x] Record the baseline policy in `wt-deploy`, test that the key changes with the policy, and test legacy acceptance and mismatch rejection.
   - [x] Correct `notes/fixes.md` and the README to describe the target's baseline CPU features rather than an unrecorded feature set.

## P2: missing APIs

12. [x] **Host event bus for realtime.** `wasmtime serve` does not give shared memory across concurrent requests, so an app cannot reliably fan out SSE from an in-process hub; every realtime page ended up polling SQLite per connection.
   - [x] Add `POST /v1/publish` on the storage loopback and `GET /v1/events?topic=&since=` on the app host with SSE, a bounded per-app ring of 256 events or 1 MiB, sequence cursors, `Last-Event-ID`, and a `gap` event when the cursor falls off the ring.
   - [x] Reuse the storage write path: committed writes publish a compact `storage`-topic event with request id, row count and affected tables/ops, without SQL or row values, so a page can refetch instead of polling.
   - [x] Use it for app-to-app notifications: `app.PublishTo` posts through the authenticated app-fetch path, `allow_callers` still applies, and the event lands on the target app's bus for its browser subscribers.

13. [x] **Blob and file uploads.** A user upload used to stream through the component and consume a concurrency slot, while `/data` had no serving path.
   - [x] Add a content-addressed per-app blob store with signed upload, download and delete URLs under `/_wt/blobs`, using a per-app HMAC key and a same-origin SDK helper.
   - [x] Serve blobs from the supervisor with `Range` support and quota accounting; blob bytes count toward `disk_mb` and the global disk budget.
   - [x] Keep large bodies out of the wasm request path. The supervisor handles upload and download directly on the app host, and no automatic GC removes blobs the app still references.

14. [x] **End-user identity.** Operator auth was solved; user auth was not, so every app reimplemented sessions, CSRF and password reset. The platform-user model now owns that surface.
   - [ ] Cheap version: extend `WT_TRUSTED_PROXIES` to an allowlist of identity headers from a trusted proxy (`Remote-User`, `X-Forwarded-User`, groups), only accepted from that proxy, passed through with a signed marker. This remains an alternative for deployments that already have an IdP.
   - [x] Bigger version: a platform user table, persisted sessions, login/logout, CSRF, password change, admin user management, `GET /v1/me` and opt-in per app via `identity: "platform"`. Password reset is an admin action; there is no email provider.
   - [x] Inject signed `X-WT-User-*` headers only for opted-in apps, strip client-supplied identity headers and the supervisor session cookie, and expose `app.UserFromRequest` for verification.

15. [ ] **A real deployment resource.** `POST /apps/<name>` was the only deploy entry point. It now has a queued resource beside it while the old route stays as a compatibility alias.
   - [x] `POST /apps/<name>/deployments` returns `202` with `Location: /apps/<name>/deployments/<id>`, and the entry reports `queued`, `running`, `success`, `failed` or `canceled` with queue/start/finish timestamps and mode.
   - [x] Add cancel and an SSE log stream for one deployment. Cancellation is best-effort for a running install; a queued run is canceled before install.
   - [ ] Give each retained bundle an immutable preview host such as `<short>--<name>.apps.example.com`; promotion flips the app alias.
   - [x] Keep `POST /apps/<name>` as a compatibility alias until the CLI and panel move.

16. [x] **Readiness, dependencies and maintenance.** Startup ordering across a frontend, API and worker was implicit.
   - [x] Add `GET /readyz`, distinct from liveness: liveness stays `200` while the process serves HTTP, readiness is `503` until startup has loaded every app and during shutdown.
   - [x] Add `depends_on` to gate routing until dependencies are ready. Dependencies are transitive, missing/stopped/maintenance apps block dependents, forward references are allowed, cycles are rejected, and health probes still run so the app becomes routable without a restart.
   - [x] Add an explicit maintenance mode that serves a supervisor HTML page with `503` and `Retry-After` instead of proxying, toggled with `POST /apps/<name>/maintenance` or the `maintenance` config field without restarting the process.

17. [ ] **Contract tooling around SvelteKit.** The pipeline generates request and response types from the Valibot registries (TS → JSON Schema → Go). Response types used to be hand-written on both sides; they now come from the same source as requests, so a field rename fails to compile instead of drifting silently.
   - [x] Add a `responseSchemas` registry, generate Go response structs with `go-jsonschema`, use them in the handlers, and validate every client response, including the error body, with the same schemas. Registered nested schemas become `$ref` files so an envelope reuses the generated item type.
   - [ ] Publish an `app.json` JSON Schema so editors and `wt validate` share one set of rules.
   - [ ] Add `wt db` (schema, query, export, import, maintenance) and `wt secrets` so the panel is not the only interface.

18. [ ] **Storage ergonomics.** SQL is the right escape hatch; it should not be the only rung.
   - [ ] Add `Get`/`Put`/`Delete` for the common record case.
   - [ ] Add a keyset pagination helper, not only offset pages.
   - [ ] Add optimistic concurrency with a `version` column and a 409 path.
   - [ ] Add a change subscription on top of the existing change log.

## P3: developer experience and docs

19. [x] **Give SvelteKit a real dev loop.** `wt dev --watch` rebuilt the whole bundle on any change: frontend build, contract generation, componentize, compile, upload and restart, with no Vite HMR or preserved browser state.
   - [x] Run `vite dev` on a free or configured port and proxy `dev.proxy` (default `/api`) to the supervisor with the app Host, so browser fetches reach the wasm component.
   - [x] Rebuild only the component on Go, WIT, `app.json`, contract or `dev.watch` changes, using `dev.build` and leaving Vite running.
   - [x] Generate and clean up `web/.wt-dev.vite.config.ts`, preserving the app's own Vite config, and ignore it in git.
   - [x] Keep `--vite=false` and workspace mode on the single-tier loop for component-only work and e2e.
   - [x] Test the Vite config generation, proxy target and Host header, component input filtering, dev manifest validation and the server-file check.

20. [x] **Accept the static-only boundary explicitly.** `adapter-static` and `prerender` mean no SSR, no `+server.ts`, no form actions and no server load.
   - [x] Document the supported SvelteKit shape in the README, including client data loading, prerender entries, the contract pipeline and the absence of a Node server.
   - [x] Reject `+page.server.*`, `+layout.server.*`, `+server.*`, `hooks.server.*` and `src/lib/server/` in the build with a clear error.
   - [x] Annotate the scaffold and guestbook root layouts so the boundary is visible in the app.
   - [ ] A JS component runtime for real SSR remains a future host feature.

21. [ ] **Split the docs and test the claims.** The README is 87KB and `notes/fixes.md` is 60KB, and small claims are already drifting, such as `wt` versus `wt-deploy` in usage strings.
   - [ ] Move rationale into `docs/` and keep the README to install, run, deploy and limits.
   - [x] Add cheap assertions for the idempotency TTL, per-app replay cap and artifact cache key contents.
   - [ ] Fix the CLI naming: the binary is `wt-deploy` and the usage string says `wt`.

## If I picked the next five

1. [ ] Embedded host with typed WIT imports, and the density benchmark that justifies it.
2. [x] Vite HMR plus the two-tier dev loop.
3. [ ] Deployment resource: async deploys and immutable preview hosts; per-app locks and persisted idempotency are done.
4. [ ] Host event bus and blob/uploads.
5. [ ] Explicit migrate phase, dependency-aware readiness and maintenance mode.
