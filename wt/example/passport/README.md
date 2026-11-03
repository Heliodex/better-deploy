# Passport

A small check-in wall behind a real OAuth 2.0 login. The app was scaffolded with `wt-deploy new --dir wt/example/passport` and keeps the generated SvelteKit static frontend, Go contract and wasm component layout.

Signing in uses the authorization code flow with PKCE against the public [Duende IdentityServer demo](https://demo.duendesoftware.com). The demo client `interactive.public` accepts any redirect URI, so the app needs no registration step. On the demo login page use `alice` / `alice` or `bob` / `bob`.

## What it demonstrates

- `server_paths` keeps the supervisor's static fallback out of `/auth`, so a plain link to `GET /auth/login` reaches the Go component.
- A prerendered nested SvelteKit page at `/auth/callback/` relays the one-time code to a JSON endpoint on the Go component. The supervisor serves its directory `index.html` for the trailing-slash URL.
- The component exchanges the code back-channel with PKCE, verifies the ID token's RS256 signature against the provider's JWKS, and calls UserInfo with the access token.
- Access, refresh and ID tokens stay in SQLite. The browser only receives an HttpOnly, SameSite session cookie whose value is stored as a SHA-256 hash.
- Posting a check-in requires a valid session; removing one only works for its author.
- `POST /api/logout` deletes the local session and returns a provider end-session URL for RP-initiated logout.

## Flow

```text
browser                 Go component                  provider
   |  GET /auth/login         |                           |
   |------------------------->| state, nonce, PKCE        |
   |  302 to /connect/authorize                           |
   |<-------------------------|                           |
   |----------------------------------------------------->|
   |                      authenticate                     |
   |  302 /auth/callback/?code&state                      |
   |<-----------------------------------------------------|
   |  /auth/callback/ page                                 |
   |  POST /api/auth/callback {code,state}                 |
   |------------------------->|  POST /connect/token      |
   |                          |-------------------------->|
   |                          |  id_token + access_token  |
   |                          |<--------------------------|
   |                          |  verify JWKS, GET userinfo|
   |                          |-------------------------->|
   |  204 + session cookie    |                           |
   |<-------------------------|                           |
```

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/auth/login` | Sets the state cookie and redirects to the provider. |
| `POST` | `/api/auth/callback` | Validates state, exchanges the code, verifies the token and creates a session. |
| `GET` | `/api/session` | Provider metadata plus the signed-in profile, or empty user fields. |
| `GET` | `/api/checkins?limit=50` | Newest check-ins; `mine` marks the caller's own entries. |
| `POST` | `/api/checkins` | Create a check-in; requires a session. |
| `DELETE` | `/api/checkins/{id}` | Delete one of the caller's check-ins. |
| `POST` | `/api/logout` | Delete the local session and return `endSessionUrl`. |

## Configuration

All settings are read from the app environment at startup.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OAUTH_ISSUER` | `https://demo.duendesoftware.com` | OIDC issuer whose discovery document is used. |
| `OAUTH_CLIENT_ID` | `interactive.public` | Public client that allows PKCE without a secret. |
| `OAUTH_CLIENT_SECRET` | empty | Optional secret for a confidential client. |
| `OAUTH_SCOPES` | `openid profile email` | Requested scopes. |
| `OAUTH_PROVIDER_NAME` | `Duende IdentityServer demo` | Label shown in the UI. |
| `OAUTH_BASE_URL` | derived | Overrides the browser-facing origin used for redirect URIs. |

The redirect URI is derived from the request. A same-origin request carries the browser's `Origin`, so the scheme and port stay correct behind an ingress that terminates TLS; a loopback `Referer` covers the Vite dev server, where `Host` is rewritten to `<app>.localhost`. Set `OAUTH_BASE_URL` if neither is reliable. The redirect URI must be `<base>/auth/callback/` exactly, including the trailing slash.

## Local deployment

Build `wt-deploy` and deploy to a local supervisor:

```sh
cd wt/deploy
go build -o wt-deploy .

WT_TOKEN=dev ./wt-deploy deploy \
  --dir ../example/passport \
  --host http://127.0.0.1:8080 \
  --app-host passport.localhost
```

Open `http://passport.localhost:8080/`. If the supervisor runs on another port, pass `--app-host passport.localhost:<port>` or set `OAUTH_BASE_URL` in `app.json`.

## Remote deployment

The same CLI deploys to a supervisor behind the Orchard ingress. The application host needs an ingress pointing at the supervisor service port:

```sh
WT_TOKEN=<supervisor-token> ./wt-deploy deploy \
  --dir ../example/passport \
  --host https://wt-demo.k.hackclub.dev \
  --app-host passport-wt-demo.k.hackclub.dev
```

Then open `https://passport-wt-demo.k.hackclub.dev/`. The redirect URI sent to the provider is derived from the app host and becomes `https://passport-wt-demo.k.hackclub.dev/auth/callback/`.

## Server routes and the nested callback

The supervisor serves `GET`/`HEAD` from `static/` before proxying, and an unknown HTML navigation falls back to `static/index.html`. The app sets `"server_paths": ["/auth"]`, so `/auth/login` reaches the Go component instead of the SPA index; real static files under that prefix still win.

The callback is a normal prerendered page at `web/src/routes/auth/callback/+page.svelte`. The supervisor serves its directory `index.html` for `/auth/callback/`, and the page posts the code and state to `/api/auth/callback` before returning to `/`. `POST` requests always reach the component. In development, `dev.proxy` includes `/auth` so Vite forwards the login and callback API calls.

The app sets `paths.relative = false` in `vite.config.ts`, so assets resolve from the site root even on nested pages.

## Development

```sh
# Go handler and OAuth verification tests
cd wt/example/passport
go test ./...

# Frontend checks
cd web
bun run svelte:check
bun run lint

# Live dev loop (Vite + the component)
cd wt
WT_WASMTIME=/path/to/wasmtime ./deploy/wt-deploy dev --dir example/passport
```

The app's `request_timeout` is 30 seconds to leave room for discovery, the token exchange and UserInfo on a cold start.
