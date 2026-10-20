# celld counter

A Durable Objects counter Worker running on [celld](https://celld.dev), with the
same code and configuration deployable to Cloudflare Workers.

## Requirements

- [celld](https://celld.dev/docs) 0.6+
- `esbuild` on `PATH` (used by `celld dev` and `celld deploy`)
- [Bun](https://bun.com) for dependencies, type checking and linting

```sh
bun install
```

## Develop

```sh
bun dev
```

Then open <http://127.0.0.1:9876/?name=dev>.

`celld dev` starts a local object store, deploys the Worker and rebuilds on
changes. It needs no Docker, cloud bucket or S3 emulator. State is kept in
`.celld/dev` and survives restarts; `celld dev --clean` starts from an empty
state. Worker and node logs are hidden by default; pass `--logs` to see them.

## Deploy

Copy the example environment file and fill in the values:

```sh
cp .env.example .env
$EDITOR .env
```

`bun run deploy` loads `.env` before invoking `celld deploy`:

```sh
bun run deploy
```

`.env` is git-ignored; commit only `.env.example`.

Start a node against the same bucket (env vars are read automatically):

```sh
celld --listen 127.0.0.1:8080
```

A running node adopts each new deployment without a restart. See the
[celld docs](https://celld.dev/docs) for R2, GCS, Azure and fleet settings.

## Checks

```sh
bun check
bun run lint
```

## Local S3?

Use `celld dev` instead. Its built-in object store covers local development
without a second server. If you specifically need to exercise the S3 code path
locally, MinIO community edition passes celld's storage test (but is not
qualified for production); production buckets should be S3, R2, GCS or Azure
Blob Storage.
