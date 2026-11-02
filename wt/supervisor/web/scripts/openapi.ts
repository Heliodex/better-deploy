/// <reference types="bun" />
import { toJsonSchema } from "@valibot/to-json-schema"
import { pipe, title } from "valibot"
import { responseContracts } from "../src/lib/api"

// Generate the OpenAPI component schemas from the panel's Valibot response schemas.
// The paths, descriptions and non-panel components stay hand-written.
const path = `${import.meta.dir}/../../../openapi.yaml`
const check = Bun.argv.includes("--check")

const raw = await Bun.file(path).text()
const document = Bun.YAML.parse(raw) as { [_: string]: unknown }
if (typeof document !== "object" || document === null) throw new Error("openapi.yaml is not an object")
const components = document.components as { [_: string]: unknown } | undefined
if (typeof components !== "object" || components === null) throw new Error("openapi.yaml has no components")
const schemas = components.schemas as { [_: string]: unknown } | undefined
if (typeof schemas !== "object" || schemas === null) throw new Error("openapi.yaml has no component schemas")

for (const [name, schema] of Object.entries(responseContracts)) {
	const json = toJsonSchema(pipe(schema, title(name)))
	delete json.$schema
	schemas[name] = json
}

const output = `${Bun.YAML.stringify(document, null, 2)}\n`
if (check) {
	if (output !== raw) {
		console.error("openapi.yaml is stale; run bun run contract")
		process.exit(1)
	}
	console.log("openapi.yaml matches the panel schemas")
} else {
	await Bun.write(path, output)
	console.log(`wrote openapi.yaml with ${Object.keys(responseContracts).length} generated component schemas`)
}
