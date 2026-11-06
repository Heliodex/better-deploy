/// <reference types="bun" />
import { type GenericSchema, safeParse } from "valibot"
import { type JsonSchema, toJsonSchema } from "@valibot/to-json-schema"
import * as contract from "./src/lib/contract"
import { responseSchemas } from "./src/lib/contract"

// The fixtures generator reads the app's response schemas and emits one fake factory per
// entry. It runs beside the schema dump on every `wt contract`, in dev, and on deploy, so
// tests stub from schema-verified shapes instead of hand-written payloads that drift.
// Every generated value is parsed against its live schema here: a field the walker cannot
// fill fails the build with its path instead of failing a test at midnight.
const outPath = Bun.env.WT_FIXTURES_OUT
if (!outPath) throw new Error("WT_FIXTURES_OUT is not set")

const TIMESTAMP = "2026-01-01T00:00:00Z"

const reserved = new Set(["deepMerge", "DeepPartial", "InferOutput", "Response"])

function fail(message: string): never {
	throw new Error(message)
}

function isRecord(value: unknown): value is { [_: string]: unknown } {
	return typeof value === "object" && value !== null && !Array.isArray(value)
}

// schemaNames resolves a registry schema to its contract export, so the emitted import is exact.
const schemaNames = new Map<unknown, string>()
for (const [name, value] of Object.entries(contract)) {
	if (typeof value === "object" && value !== null && !schemaNames.has(value)) schemaNames.set(value, name)
}

const identifier = /^[A-Za-z_$][A-Za-z0-9_$]*$/

// pickPattern resolves an alternation like ^(approved|changes|rejected)$ to its first branch.
// Anything fancier returns null and the caller falls back to a readable string.
function pickPattern(pattern: string): string | null {
	if (!pattern.startsWith("^(") || !pattern.endsWith(")$")) return null
	const first = pattern.slice(2, -2).split("|")[0]
	if (first === undefined || first === "" || first.includes("(") || first.includes("\\")) return null
	return first
}

function stringFor(key: string, node: { [_: string]: unknown }): string {
	const pattern = typeof node.pattern === "string" ? node.pattern : null
	if (pattern) {
		const pick = pickPattern(pattern)
		if (pick !== null) return pick
	}

	let base = key
	if (/email/i.test(key)) base = "test@example.com"
	else if (/At$/.test(key)) base = TIMESTAMP

	const min = typeof node.minLength === "number" ? node.minLength : 1
	while (base.length < min) base += base
	if (typeof node.maxLength === "number" && base.length > node.maxLength) base = base.slice(0, node.maxLength)

	return base
}

function numberFor(node: { [_: string]: unknown }, integer: boolean): number {
	let value = typeof node.minimum === "number" ? node.minimum : 0
	const exclusive = node.exclusiveMinimum
	if (typeof exclusive === "number" && value <= exclusive) value = exclusive + 1
	else if (exclusive === true) value += 1
	if (typeof node.maximum === "number" && value > node.maximum) value = node.maximum
	if (integer) value = Math.ceil(value)

	return value
}

function walk(node: { [_: string]: unknown }, key: string): unknown {
	if ("const" in node) return node.const
	if (Array.isArray(node.enum) && node.enum.length > 0) return node.enum[0]

	const union = Array.isArray(node.anyOf) ? node.anyOf : Array.isArray(node.oneOf) ? node.oneOf : null
	if (union) {
		const first = union[0]
		if (!isRecord(first)) fail(`fixture cannot build ${key}: empty union`)
		return walk(first, key)
	}

	if (node.type === "object" || (node.type === undefined && isRecord(node.properties))) {
		const properties = isRecord(node.properties) ? node.properties : {}
		const out: { [_: string]: unknown } = {}
		for (const [name, child] of Object.entries(properties)) {
			if (!isRecord(child)) fail(`fixture cannot build ${key}.${name}: not a schema`)
			out[name] = walk(child, name)
		}
		return out
	}

	if (node.type === "array") {
		if (isRecord(node.items)) return [walk(node.items, key)]
		return []
	}

	if (node.type === "string") return stringFor(key, node)
	if (node.type === "integer") return numberFor(node, true)
	if (node.type === "number") return numberFor(node, false)
	if (node.type === "boolean") return false
	if (node.type === "null") return null

	fail(`fixture cannot build ${key}: unsupported schema ${JSON.stringify(node).slice(0, 120)}`)
}

if (!Array.isArray(responseSchemas)) throw new Error("src/lib/contract.ts must export a responseSchemas array")

const seen = new Set<string>()
const factories: { exportName: string; title: string; value: unknown }[] = []

for (const schema of responseSchemas) {
	const json: JsonSchema = toJsonSchema(schema as Parameters<typeof toJsonSchema>[0])
	if (typeof json.title !== "string" || json.title === "" || !identifier.test(json.title))
		fail("every response schema needs a title that is a valid identifier")
	if (seen.has(json.title)) fail(`response schema title ${json.title} is registered twice`)
	seen.add(json.title)

	const exportName = schemaNames.get(schema)
	if (!exportName) fail(`response schema ${json.title} is not an export of ./src/lib/contract`)
	if (reserved.has(`fake${json.title}`)) fail(`fixture fake${json.title} collides with the fixture runtime`)

	const value = walk(json as unknown as { [_: string]: unknown }, json.title)
	const parsed = safeParse(schema as GenericSchema<unknown>, value)
	if (!parsed.success) {
		const issue = parsed.issues[0]
		const path =
			issue?.path
				?.map(item => String((item as { key?: unknown }).key ?? "?"))
				.join(".") ?? ""
		fail(`fixture ${json.title}: generated value fails its schema at ${path || "<root>"}`)
	}

	factories.push({ exportName, title: json.title, value })
}

factories.sort((a, b) => (a.title < b.title ? -1 : 1))

const header = [
	"// Code generated by wt contract from the response schemas. DO NOT EDIT.",
	"// Excluded from lint and format; verified byte-for-byte by wt contract --check.",
	'import type { InferOutput } from "valibot"',
	`import type { ${factories
		.map(factory => factory.exportName)
		.sort()
		.join(", ")} } from "./contract"`,
].join("\n")

const runtime = `type DeepPartial<T> = T extends readonly unknown[]
	? T
	: T extends object
		? { [K in keyof T]?: DeepPartial<T[K]> }
		: T

function deepMerge(base: unknown, overrides: unknown): unknown {
	if (Array.isArray(overrides)) return [...overrides]
	if (typeof overrides === "object" && overrides !== null) {
		const out: { [_: string]: unknown } = {}
		if (typeof base === "object" && base !== null && !Array.isArray(base)) {
			for (const [key, value] of Object.entries(base)) out[key] = value
		}
		for (const [key, value] of Object.entries(overrides)) {
			out[key] = key in out ? deepMerge(out[key], value) : value
		}
		return out
	}
	return overrides
}`

const bodies = factories.map(
	factory => `const ${factory.title[0]?.toLowerCase()}${factory.title.slice(1)}Fixture: InferOutput<typeof ${factory.exportName}> = ${JSON.stringify(factory.value, null, "\t")}

// fake${factory.title} builds a valid ${factory.title} for tests. Overrides deep-merge over the generated defaults; arrays replace wholesale.
export function fake${factory.title}(
	overrides: DeepPartial<InferOutput<typeof ${factory.exportName}>> = {},
): InferOutput<typeof ${factory.exportName}> {
	// oxlint-disable-next-line typescript/no-unsafe-type-assertion -- The merge output matches the schema-verified fixture shape.
	return deepMerge(${factory.title[0]?.toLowerCase()}${factory.title.slice(1)}Fixture, overrides) as InferOutput<typeof ${factory.exportName}>
}`,
)

await Bun.write(outPath, `${header}\n\n${runtime}\n\n${bodies.join("\n\n")}\n`)
if (!Bun.env.WT_FIXTURES_QUIET) console.log(`wrote src/lib/fixtures.gen.ts (${factories.length} fixtures)`)
