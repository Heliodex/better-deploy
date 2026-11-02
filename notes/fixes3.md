# Fixes 3

Architecture simplification review, 2026-11-01. This pass looks for duplicated mechanisms and accidental complexity rather than correctness bugs. Prior priorities stay in `notes/fixes.md` and `notes/fixes2.md`; this list only adds work that those notes do not already cover.

The short assessment: the code is clean at function level after the earlier splits, but the same concepts are still implemented two or three times. The biggest wins are removing the test-only direct launch path, giving isolation one lifecycle, and making the storage and configuration surfaces share one representation.

The first five items below are now implemented; the remaining list is unchanged in priority order.

## Small changes in this pass

- [x] **Shared storage statement validation and execution.** `batch` now converts to the transaction statement form and calls the shared validator and executor, so write batches, transactions and migrations no longer carry three copies of the SQL and parameter checks. `f649fb5`
- [x] **Shared directory usage walking.** The `/data`, SQLite file and blob-store measurements in `appDataUsage` use one `walkUsage` helper, so the byte and inode rules cannot drift between them. `ee41943`
- [x] **Shared bounded JSON decoding.** Admin, auth, config, dependency, job, secret and event handlers use `readJSONBody` instead of repeating `MaxBytesReader` and the 400 response; identity routes were split into per-method handlers at the same time. `55f987a`, `599cab1`

## Larger changes, priority ordered

1. [x] **Remove the legacy direct launch path and make the runner the only process transport.** Production always has a runner because `startRunner` either connects, starts one, or exits; `launchDirect`, `startDirectProcess`, `watchDirectExit`, `appCredential` and the rlimit path are reached only when `s.runner == nil`, which now happens only in unit tests with a bare `testSupervisor`. Keeping both transports duplicates argument and environment construction, namespace setup, egress, readiness, exit bookkeeping and adoption logic, and it means every launch change must be made twice.
   - Make `supervisor.runner` mandatory in the type and run every app through it.
   - Replace the test-only fake wasmtime process with an in-process runner client or the existing test runner socket, then delete the direct path and its process-level tests.
   - This also lets `process.go` lose the second exit path and makes handover the only process adoption story.
   - Implemented in `7aeb78c`. The direct path and its rlimit, credential and namespace-child helpers are gone. The runner now keys processes by a per-launch instance ID, so a blue/green deploy can keep the old and new generation of one app apart; tests attach the real runner over its Unix socket.

2. [x] **Make isolation a single prepare, activate and close lifecycle.** `process_launch.go` currently orchestrates `prepareLaunch`, `prepareNamespaces`, `applyProcessLimits`, `cgroups.addPID`, `applyEgress`, `quota.apply` and `releaseIsolation` with several inline rollback paths. The boundaries live in six files (`isolation.go`, `ns.go`, `seccomp_linux.go`, `cgroup.go`, `uid.go`, `egress.go`, plus `quota.go` and `rlimit.go`), and the call order is knowledge that only the launch function has.
   - Introduce an `isolationPlan` with `Prepare`, `Start`, `Activate` and an idempotent `Close`, returning the argv and environment it owns.
   - Keep trusted mode as the empty plan, so launch has one code path and untrusted mode is a plan implementation.
   - This is security-relevant: a single `Close` makes it much harder to leak a cgroup, uid, namespace root or firewall rule on a partial launch.
   - Implemented as `isolationPlan` with `prepareIsolation`, `attachProcess`, `activate` and an idempotent `close`. `launch` stores the active plan on the app so `stop` and failed launches release the same resources through one path.

3. [x] **Unify the storage statement types and the SQL handler surface.** There are still two statement shapes (`storageStatement` and `storageTransactionStatement`) plus conversion for batches and migrations, and `storageServer.handle` wraps most routes in `handleSQL` closures for the same semaphore and error handling. One statement type with an optional `Query` flag, one decoder and one executor would cover query, exec, write, batch, transaction, session and migration.
   - Collapse `storageBatchRequest` and `storageTransactionRequest` into one request type; keep the wire names compatible.
   - Turn the route closures into methods that take the app string, so auth, body limits and query semaphore handling live in one place.
   - After this, add tests that send the same statement through every entry point and expect the same result shape and error codes.
   - Implemented in `dce29e9`. One `storageStatement` with `Query` covers batches, transactions, sessions and migrations; batch and transaction share `storageStatementsRequest` and `storageStatementsResponse`; `handleSQL` takes the app and accepts handler methods directly. Batches reject `query` statements explicitly because they cannot return rows.

4. [x] **Table-drive configuration and remove the variadic tenant mode.** `configFromEnv`, `bindConfigFlags`, `applyConfigFlags`, `validateTenantConfig`, `validateIsolationConfig` and `validateDataQuotaConfig` together maintain 57 `WT_*` names and 34 flags, including duplicated MiB conversions and defaults. Separately, `normalizeAppConfig`, `readAppConfig` and `persistAppConfig` take `modes ...string` only to smuggle one tenant mode through, which hides the real parameter and invites call sites to pass the wrong thing.
   - Use one table of `{env, flag, default, parse, validate}` entries to populate the config and bind the flags, or move both to a versioned config file with a single loader and a small env alias layer.
   - Replace `modes ...string` with an explicit `tenantMode string` parameter or a field on the loader.
   - This is also the prerequisite for a real `wt config` that shows effective values and their source.
   - Implemented in `23ed5a9`. `bindString`, `bindBool`, `bindInt` and `bindInt64` load the environment default and register the flag on one line each, so `configFromEnv` and the separate flag pass are gone. `normalizeAppConfig`, `readAppConfig` and `persistAppConfig` now take an explicit tenant mode.

5. [x] **Replace the manual app, storage, identity and admin dispatch with one route table.** The supervisor has 72 HTTP handlers and several independent prefix switches: `handleAppPrefixRoute`, `handleAppVerb`, `storageServer.handle`, the identity action switch and the admin routes. Method checks, audit wrapping, body limits and scope checks are repeated at each layer, and the hand-written `openapi.yaml` can drift from all of them.
   - Declare routes as `{pattern, method, scope, body limit, handler}` and let one dispatcher apply authentication, audit, `Allow` headers and 404s.
   - Generate the OpenAPI document and the panel client from the same table, so a new route cannot be added without its contract.
   - Do this after item 3, so storage routes are already methods rather than closures.
   - Implemented in `5d2d277`. `router.go` matches method and path segments, with one table each for app, storage, identity, admin and public routes. App entries carry the action label used for scope checks and audit, so `handleApp` no longer parses action strings. The hand-written `openapi.yaml` and panel client are not generated from the table yet; that remains the second half of this item.

6. [ ] **Unify operator tokens and platform identity behind one principal and session layer.** `authStore` keeps hashed bearer tokens in a JSON file, `identityStore` keeps users, PBKDF2 hashes and sessions, and `supervisor.requireIdentity` bridges both. Login, logout, audit actors and app scope checks each special-case which store answered.
   - Define one `Principal` with an ID, kind, role and app allowlist, and one `Session` interface with `Current`, `Create` and `Delete`.
   - Keep the two backing stores if their lifecycles differ, but make routing, audit and app access consume only the interface.
   - This removes the second "who is calling" model and makes CSRF and cookie handling apply consistently.

7. [ ] **Collapse backup and deployment content storage into one content-addressed store with retention policies.** `deploymentManager` and `backupManager` both write per-app directories, hash files, hardlink blobs, keep a current pointer, prune old entries and maintain metadata. The tree and history formats are different, but the storage problem is the same.
   - Extract a `contentStore` keyed by digest with reference counts and `Put`, `Link`, `Prune` and `Usage` operations.
   - Keep deployment history and snapshot trees as thin policies over that store.
   - This is also the cleanest place to add an offsite or S3 target once, rather than twice.

8. [ ] **Consolidate resource accounting and admission.** A global disk budget, the per-app `disk_mb` monitor, the storage temp budget, the optional `setquota` backend, the artifact cache cap, the session cap and concurrency slots each have their own measurement and rejection path. `disk.go`, `quota.go`, `budget` tests and the storage temp monitor overlap, and the error codes are not consistent.
   - Introduce one accounting component with named dimensions and a single monitor loop, then let each admission point ask for a specific dimension.
   - Keep the existing HTTP status mapping, but derive it from the dimension rather than from ad-hoc sentinels.
   - This would make the metrics and `/usage` response a projection of the same data instead of separate measurements.

9. [ ] **Share the bounded ring and SSE writer across events, logs and changes.** The host event bus, the app log tail and the storage change log each keep a bounded ring and cursor logic, and the SSE handlers repeat frame writing and gap handling. The cursor semantics are already close enough that three copies are a maintenance cost without a benefit.
   - Extract a generic `ring[T]` with `Since(cursor)` and one SSE response helper that emits `Last-Event-ID`, gap frames and heartbeats.
   - Keep the storage change log's on-disk format, but read it through the same cursor interface.
   - This makes a future `wt:log` WIT import or a second subscriber cheaper.

10. [ ] **Consolidate the runner protocol types and op dispatch.** `runner.go` and `runner_client.go` define the request and event shapes and the op strings separately, and handover passes listeners through environment variables and inherited file descriptors. It works and is tested, but a protocol change means editing both sides and the handover setup.
    - Put the request and event types plus op constants in one file shared by client and server, and route both through one state machine.
    - Keep the Unix socket and liveness pipe; only remove the duplicated schema and ad-hoc dispatch.
    - This is lower priority than the items above because the current protocol is small and the handover tests cover it.

11. [ ] **Retire compatibility layers behind an explicit version cutover.** The code carries the legacy metadata-only `X-WT-App-Signature`, artifact records without CPU or epoch, the `X-WT-Static-Only` deploy path, `POST /apps/<name>` beside the queued `/deployments` resource, and legacy `user_version` migration bookkeeping. Each branch has tests and documentation, and each one makes the current path harder to read.
    - Declare the oldest supported app and supervisor version, then delete the branches and their tests in one release.
    - Keep the data migrations, but drop the dual write and dual read paths.
    - Do this last, after the structural changes, so there is only one code path left to document.

## If I picked the next three

1. Unify operator tokens and platform identity under one principal and session layer.
2. Collapse backup and deployment content storage into one content-addressed store with retention policies.
3. Consolidate resource accounting and admission across the disk, temp, quota, cache and session limits.

Keep the invariants in `notes/fixes2.md` under "Do not regress"; none of the items above requires weakening the wasm artifact, blue/green deploys, the storage authorizer, sessions, scoped tokens or the tested isolation boundaries.
