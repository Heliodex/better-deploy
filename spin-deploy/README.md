# spin-deploy

A small Go CLI that uploads a Spin application to a spin supervisor. No
third-party dependencies.

## Usage

Run it from the application directory (the one containing `spin.toml`), or
pass `--dir`:

```sh
spin-deploy deploy
spin-deploy deploy --dir ./spin --app gotime --deploy-host deploy.apps.example.com
```

Configuration comes from flags, falling back to the environment:

| flag | env | default | purpose |
| --- | --- | --- | --- |
| `--dir` | | `.` | application directory |
| `--app` | `APP` | `spin.toml` application name | app name on the supervisor |
| `--deploy-host` | `DEPLOY_HOST` | | supervisor host |
| `--token` | `SUPERVISOR_TOKEN` | | bearer token for the deploy API |
| `--skip-build` | | `false` | upload the current build output without running `spin build` |

`--deploy-host` accepts a bare host (https is assumed) or a full URL, so
`http://127.0.0.1:8080` works for a local container.

`deploy` runs `spin build`, packs `spin.toml`, `main.wasm` and `app.json` (if
present) into a tarball, and posts it to the supervisor. It prints the
supervisor's JSON response. The app's `spin/package.json` wires this up as
`bun run deploy`.

## Development

```sh
cd spin-deploy
go test ./...
go vet ./...
go build -o /tmp/spin-deploy .
```
