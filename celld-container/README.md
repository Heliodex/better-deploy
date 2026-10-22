# celld + versitygw in one container

A single-container image running:

- [celld](https://celld.dev/docs) — one node of the fleet
- [Versity Gateway](https://github.com/versity/versitygw) — the S3 bucket that node runs against

Two processes, one volume, no application in the image. It is a self-contained,
production-shaped stand-in for a cloud bucket, useful for testing deploys against
the real fleet-bucket code path without running the gateway and node as host
processes. Deploy any celld app into it from outside, exactly as you would deploy
to a remote bucket; the sibling `celld/` guestbook is the example used below.

## Build

```sh
podman build --format docker -t localhost/celld-vgw .
```

`--format docker` keeps the image's `HEALTHCHECK`: OCI images cannot store one,
and podman defaults to OCI. Docker users need no flag.

The image is about 195 MB. Roughly 78 MB is the Debian slim base and 112 MB the
two stripped binaries — celld's 64 MB (V8 included) and versitygw's 48 MB — so
there is little left to trim without a different celld build. Alpine is not an
option: celld publishes glibc builds only, and its imports (`__res_init`,
`fcntl64`) are missing from Alpine's gcompat.

The build is x86_64 only and fetches the `x86_64` release assets.

## Run

```sh
podman volume create celld-vgw
podman run -d --name celld-vgw --stop-timeout 75 \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:10000:10000 \
  -v celld-vgw:/var/lib/celld \
  localhost/celld-vgw
```

- `8080` — celld's public Worker listener.
- `10000` — versitygw's S3 API, needed only for `celld deploy` from outside.
- `--stop-timeout 75` — above celld's 40 s drain (`CELLD_SHUTDOWN_TOTAL_MS`);
  anything shorter can SIGKILL the handoff.

## Deploy into it

From the app directory, point `celld deploy` at the gateway. The sibling app's
`.env.bucket` already matches this container, so its script needs no changes:

```sh
# in celld/
bun run deploy:bucket
```

The running node adopts the deployment within `CELLD_DEPLOY_POLL_S` (30 s) and
does not restart. Until a deployment exists, celld holds requests and the health
check stays `unhealthy`; the log reports `awaiting_initial_deployment`.

celld subcommands (`d1`, `kv`, `r2`, `cell`, `diagnose`) need a node's internal
listener, which stays on loopback inside the container. Run them through
`podman exec`, with `--env-file` supplying the bucket settings:

```sh
podman exec --env-file ../celld/.env.bucket celld-vgw celld kv list guestbook
```

## Configuration

Everything is an environment variable, passed with `podman run -e`:

| variable | default | effect |
| --- | --- | --- |
| `CELLD_BUCKET` | `s3://celld` | Bucket to serve, plus an optional key prefix |
| `VGW_ACCESS_KEY_ID` | `testuser` | Gateway root access key; also celld's AWS key |
| `VGW_SECRET_ACCESS_KEY` | `testsecret` | Gateway root secret key |
| `VGW_PORT` | `10000` | Gateway S3 listener, every interface |
| `VGW_FSYNC` | `0` | `1` fsyncs each object, for a filesystem that can lose acknowledged writes |
| `VGW_REQUEST_LOGS` | `0` | `1` keeps versitygw's per-request access log (noisy: the node polls the bucket) |
| `S3_DATA_DIR` | `/var/lib/celld/s3` | Gateway's posix root; a directory per bucket |
| `IAM_DIR` | `/var/lib/celld/iam` | Gateway IAM state |
| `SIDECAR_DIR` | `/var/lib/celld/sidecar` | Gateway metadata store; sidecar files instead of `user.*` xattrs, which some filesystems (e.g. Orchard's overlay) reject with `operation not supported` |
| `CELLD_WATCH` | `/var/lib/celld/work` | celld's local SQLite and replication state |
| `CELLD_ADDR` | `0.0.0.0:8080` | celld's public Worker listener |
| `CELLD_INTERNAL_ADDR` | `127.0.0.1:8081` | Peer/operator listener, never publish it |
| `STOP_GRACE_S` | `75` | Seconds the entrypoint waits for celld's drain |

Every other `CELLD_*` variable (`CELLD_MAX_RESIDENT_CELLS`, `CELLD_DURABILITY`,
…) and `RUST_LOG` passes straight through, and extra arguments to the container
are appended to the node command. Pin releases with
`--build-arg CELLD_VERSION=v0.6.3 --build-arg VGW_VERSION=v1.9.0`.

## Worth knowing

- **One container per volume.** versitygw's default `flock` publish locking is
  only sufficient for one gateway process, and two celld nodes must not share
  `CELLD_WATCH`.
- **The S3 port is the whole bucket.** Whoever reaches port 10000 with the
  credentials controls the fleet, so publish it to loopback (as above) or set
  real credentials. The image defaults are test values.
- **The internal listener is never published.** It has no authentication and
  peer traffic is not encrypted.
- **No deployment, no health.** celld holds requests until a deployment exists,
  so the health check is `unhealthy` until then; then it is 200 and the image
  turns `healthy`.
- **One node.** There is no peer, so `CELLD_DURABILITY=fleet` (the default)
  waits for the bucket on every write, exactly as `bucket` mode does.
- **No esbuild in the image.** It is only needed by `celld deploy`, which runs
  outside the container.
- The image runs as uid 1000. A bind mount for `/var/lib/celld` must be writable
  by that user; the named volume above is.
