/// <reference types="bun" />
import { mkdir, rm } from "node:fs/promises"
import { type JsonSchema, toJsonSchema } from "@valibot/to-json-schema"

// Every schema in the request and response registries becomes one JSON file. Filenames come from their v.title(), and the CLI supplies the output directory and app name so this script never changes. WT_CONTRACT_SOURCE points at the tracked contract module so compat can also read the source at an older git ref.
type Schema = Parameters<typeof toJsonSchema>[0]

const schemasDir = Bun.env.WT_CONTRACT_OUT
const appName = Bun.env.WT_APP_NAME
if (!schemasDir) throw new Error("WT_CONTRACT_OUT is not set")
if (!appName) throw new Error("WT_APP_NAME is not set")

const contract: { requestSchemas: Schema[]; responseSchemas: Schema[] } = await import(
	Bun.env.WT_CONTRACT_SOURCE ?? "./src/lib/contract"
)

const { requestSchemas, responseSchemas } = contract

const isRecord = (value: unknown): value is { [_: string]: unknown } =>
	typeof value === "object" && value !== null && !Array.isArray(value)

const kebab = (value: string): string => value.replaceAll(/([a-z0-9])([A-Z])/g, "$1-$2").toLowerCase()

interface ContractDocument {
	file: string
	json: JsonSchema
	title: string
}

const titles = new Map<string, string>()

function nameSchema(schema: Parameters<typeof toJsonSchema>[0]): ContractDocument {
	const json = toJsonSchema(schema)
	const { title } = json
	if (typeof title !== "string" || title === "") throw new Error("every contract schema needs a v.title()")
	const file = kebab(title.replace(/(Request|Response)$/, ""))
	if (titles.has(title)) throw new Error(`contract schema title ${title} is registered twice`)
	if ([...titles.values()].includes(file))
		throw new Error(`contract schema ${title} writes the existing file ${file}.json`)
	titles.set(title, file)
	return { file, json, title }
}

// Point a registered nested schema at its own file, so the Go generator reuses one struct for every endpoint.
function linkRefs(value: unknown, rootTitle: string): unknown {
	if (Array.isArray(value)) return value.map(item => linkRefs(item, rootTitle))
	if (!isRecord(value)) return value
	const { title } = value
	if (typeof title === "string" && title !== rootTitle && titles.has(title))
		return { $ref: `${titles.get(title)}.json` }
	const out: { [_: string]: unknown } = {}
	for (const [key, child] of Object.entries(value)) out[key] = linkRefs(child, rootTitle)
	return out
}

// Name every schema first so a reference can point forward in the registry.
const named = [...requestSchemas, ...responseSchemas].map(nameSchema)
const documents = named.map(document => {
	const linked = linkRefs(document.json, document.title)
	if (!isRecord(linked)) throw new Error(`contract schema ${document.title} did not produce an object`)
	return {
		file: document.file,
		json: { ...linked, $id: `https://wt.example/${appName}/${document.file}.json` },
		title: document.title,
	}
})

await rm(schemasDir, { force: true, recursive: true })
await mkdir(schemasDir, { recursive: true })

for (const { file, json, title } of documents) {
	await Bun.write(`${schemasDir}/${file}.json`, `${JSON.stringify(json, null, "\t")}\n`)
	if (!Bun.env.WT_CONTRACT_QUIET) console.log(`wrote contract/schemas/${file}.json (${title})`)
}
