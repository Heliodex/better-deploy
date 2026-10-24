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

Configuration comes from flags, falling back to the environment, then to a
`.env` file in the application directory:

| flag | env | `.env` key | purpose |
| --- | --- | --- | --- |
| `--dir` | | | application directory (default `.`) |
| `--app` | `APP` | `APP` | app name (default: `spin.toml` application name) |
| `--deploy-host` | `DEPLOY_HOST` | `DEPLOY_HOST` | supervisor host |
| `--token` | `SUPERVISOR_TOKEN` | `SUPERVISOR_TOKEN` | bearer token for the deploy API |
| `--skip-build` | | | upload the current build output without running `spin build` |

Precedence is flags, then process environment, then `.env`, then the
`spin.toml` default (for the app name only). An example `.env`:

```sh
DEPLOY_HOST=deploy.apps.example.com
SUPERVISOR_TOKEN=replace-with-a-real-token
APP=gotime
```

`spin/.gitignore` excludes `.env`, since it holds the token. The parser handles
`KEY=value` lines, `export` prefixes, full-line `#` comments, and single or
double quotes.

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
