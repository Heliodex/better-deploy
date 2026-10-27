# Client to server validation

This compares ways to make the guestbook API validate requests on both sides of the wire, rather than only validating responses in the browser. The short version:

- gRPC and GraphQL do not solve this problem. They type the shape of a message; they do not enforce the constraints this API cares about (trimmed non-empty strings, 40/280 character limits, a four-value reaction enum, query ranges).
- Both cost far more than the current JSON API in a Wasm component, and both add client runtime.
- The actual gap is that the client does not validate requests, and the same rules are written twice.

## Method

Every prototype implements the same four operations against the same in-memory store:

- `GET /api/messages?limit=`
- `POST /api/messages`
- `POST /api/messages/{id}/react`
- `GET /api/stats`

They were built as `wt:app/app` components with `componentize-go`, precompiled with the same Wasmtime 50 binary, and served with `wasmtime serve`. Latency is 300 sequential keep-alive requests on loopback, after 20 warm-up requests; the table reports the median. Components and clients are the same code path as the guestbook, just with the store in memory.

Client sizes are `bun build --minify --target=browser` followed by `gzip -9`. Page chunk sizes come from a real `bun run build` of the guestbook frontend with `src/lib/api.ts` replaced.

These numbers compare transport and validation overhead, not absolute production latency. Loopback and a laptop are fast; the network dominates a real request.

## Results

| Approach | wasm | cwasm | create p50 | client gzip | What it validates |
|---|---:|---:|---:|---:|---|
| REST today | 7.54 MB | 16.01 MB | 0.42 ms | 1.97 KB | server only, manual |
| REST + request Valibot | 7.54 MB | 16.01 MB | 0.42 ms | 2.07 KB | both, manual |
| JSON-RPC envelope | 7.54 MB | 16.01 MB | 0.50 ms | 2.58 KB | both, manual, typed field errors |
| JSON Schema runtime | 9.42 MB | 19.86 MB | 0.47 ms | 1.97 KB | both, one schema |
| Go codegen from JSON Schema | 8.11 MB | 17.22 MB | 0.47 ms | 1.97 KB | both, one schema, no runtime |
| Huma + Hey API | 9.78 MB | 20.64 MB | 0.50 ms | 6.15 KB | both, one schema, strict objects |
| GraphQL | 9.72 MB | 20.67 MB | 0.76 ms | 14.09 KB | shape only |
| Connect | 12.87 MB | 27.10 MB | 0.57 ms | 34.16 KB | shape only |
| OpenAPI + kin-openapi | 17.40 MB | 35.93 MB | 1.25 ms | 2.75 KB | both, one spec |
| gRPC-Web | 23.31 MB | 47.47 MB | 0.73 ms | 14.55 KB | shape only |

`cwasm` is what the supervisor actually loads. The baseline is a small component; the guestbook itself is 13.0 MB wasm / 27.2 MB cwasm because it embeds the frontend and the storage client.

Client page chunks for the guestbook page:

| Client | Page chunk | Gzipped |
|---|---:|---:|
| Valibot, response validation only (before) | 11.14 KB | 4.18 KB |
| Valibot, request and response validation (implemented) | 13.02 KB | 4.71 KB |
| Hey API SDK generated from OpenAPI | 25.86 KB | 8.54 KB |

## Validation behaviour

I sent the same probes to every server: missing `name`, a 41-character name, `name: 123`, an extra `admin` field, and a whitespace-only name.

| Approach | Missing / too long | Wrong type | Extra field | Whitespace only | Error shape |
|---|---|---|---|---|---|
| REST today | 400 | 400, generic | accepted | 400 | `{error: "..."}` |
| JSON-RPC envelope | 400 | 400, generic | accepted | 400 | `{ok:false,error:{code,field,message}}` |
| JSON Schema runtime | 400, field | 400, field | accepted | 201, empty name | field and message |
| Go codegen | 400, field | 400, field | accepted | 201, empty name | field and message |
| Huma | 422, `body.name` | 422, `body.name` | 422, rejected | 201, empty name | `errors[].location` |
| OpenAPI + kin-openapi | 400, field | 400, field | 400, rejected | 201, empty name | multi-line schema dump |
| GraphQL | 422 / 200 | 200, `123` became `"123"` | 422, rejected | 200 | GraphQL `errors[]`, often HTTP 200 |
| Connect | 400 | 400, parse error | accepted | 400 | `{code,message}` |
| gRPC-Web | 200, `grpc-status: 3` | 200, `grpc-status: 3` | accepted | 200, `grpc-status: 3` | `grpc-status` / `grpc-message` |

Two results matter more than the rest:

- **GraphQL coerced `name: 123` to the string `"123"` and created the message.** GraphQL types describe shape, and its scalar coercion rules are not validation. Resolver errors also arrive as HTTP 200 with an `errors` array.
- **Schema validation is not normalization.** The JSON Schema, codegen, Huma and OpenAPI prototypes validated the raw strings, then the handler trimmed them, so `"   "` passed and became an empty message. The manual REST and RPC handlers trim before checking. Any schema-first design has to either normalize before validating or encode the trimmed shape in the schema (for example, a `pattern` that rejects leading/trailing whitespace).

Connect also drops unknown JSON fields silently and omits zero-valued proto fields in responses (`reactions: {}`), which is normal proto JSON but awkward for a browser client.

## What each option is good at

- **REST + request Valibot.** Smallest useful change. Request validation costs about 0.1 KB gzip in the real page chunk. The server still needs manual checks, but those already exist and work.
- **JSON-RPC envelope.** No meaningful server cost, a stable `{code,field,message}` error shape, and one place to version the API. It does not remove duplicated constraints.
- **JSON Schema runtime.** One schema validates everything, including nested objects and arrays, with field paths. `santhosh-tekuri/jsonschema/v6` costs 1.9 MB wasm and about 0.1 ms per request. Error messages are generic and include schema text.
- **Go codegen from JSON Schema.** `atombender/go-jsonschema` generates structs with `UnmarshalJSON` validation, including UTF-8 rune counts, enum checks and ranges. No runtime dependency; 0.57 MB wasm is mostly the standard `encoding/json` it pulls in. Errors are strings, not codes.
- **Huma + Hey API.** Best end-to-end contract. Go struct tags drive validation and an OpenAPI 3.1 document; Hey API generates a typed fetch SDK plus Valibot schemas, and its SDK validates both requests and responses. Invalid input is rejected before `fetch`, and the server returns 422 with `errors[].location`. The cost is 2.2 MB wasm, 4.6 MB cwasm and about 4.4 KB gzip on the page. Huma also adds `$schema` to JSON responses by default; that is configurable.
- **OpenAPI + kin-openapi.** Full OpenAPI request validation, but the runtime validator made the request three times slower in this test and its errors dump schema and value. Not worth it for four endpoints.
- **GraphQL.** Good when clients need to choose fields and there are many consumers. The schema is not a constraint validator, errors often use HTTP 200, and every mutation sends its query text (about 200 bytes here instead of 39).
- **Connect / gRPC-Web.** Good when you need streaming, generated clients in many languages, or an existing protobuf contract. Neither validates string lengths or ranges; that needs protovalidate or hand-written checks. gRPC-Web works through the HTTP/1.1 adapter, but adds 15.8 MB wasm and 31.5 MB cwasm.

## Recommendation

1. **Keep the HTTP JSON API.** Switching protocols would add a runtime, a code generator and a second error model before it added any validation.

2. **Request validation is implemented in the client.** `createMessageSchema`, `reactMessageSchema` and `reactMessageInput` live in `web/src/lib/contract.ts`, run before `fetch`, and throw a `ValidationError` with per-field messages. The form shows them under each input and clears them on typing. The page chunk is 13.02 KB / 4.71 KB gzip, about 0.5 KB over the previous build. The server keeps its existing string error body; upgrading it to `{error:{code,field,message}}` would let the form show server-side field errors too.

3. **Share test vectors between the two sides.** A small JSON file of valid and invalid payloads, run against the Go handlers and against the Valibot schemas, catches drift without a framework. This is the cheapest way to keep the manual rules honest.

4. **If you want one source of truth, choose one of these:**
   - **Lean (implemented):** Valibot schemas to JSON Schema at build time, then `go-jsonschema` generated Go types and `UnmarshalJSON` validation. +0.6 MB wasm, no runtime schema library, client unchanged. `contract/generate.sh` runs as part of the app build; the generated JSON Schema and Go file are ignored.
   - **Full:** Huma on the server and Hey API with Valibot on the client. +2.2 MB wasm and +4.4 KB gzip, but validation, OpenAPI docs and the client SDK all come from the Go types. Best developer experience; most framework.

The guestbook build pipeline is:

```text
web/src/lib/contract.ts
  -> bun run contract
  -> contract/schemas/*.json
  -> go tool go-jsonschema
  -> contract/contract.gen.go
```

`contract/generate.sh` runs the two steps, and `app.json` calls it before
componentizing. The generated Go code uses only the standard library, so no
schema validator is linked into the Wasm component.

One mismatch remains: Valibot counts string length in UTF-16 code units, while
JSON Schema and the generated Go code count code points, so astral characters
(emoji) reach the client limit sooner. If that matters, count grapheme clusters
on both sides or exclude emoji from the limited fields.

The prototypes live in `/tmp/opencode/contract-bench` (server variants, client entries, benchmark and validation scripts).
