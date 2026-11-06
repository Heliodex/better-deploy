/// <reference types="bun" />
import * as contract from "./src/lib/contract"
import { endpoints } from "../contract/endpoints"

// The client generator reads the app's endpoint registry and emits one typed fetch wrapper per
// entry. It runs beside the schema dump on every `wt contract`, in dev, and on deploy, so the
// generated client can never drift from the contract. Run output and validation errors both fail
// the build loudly instead of shipping a stale client.
const outPath = Bun.env.WT_CLIENT_OUT
if (!outPath) throw new Error("WT_CLIENT_OUT is not set")

type Param = {
	default?: string
	in: "path" | "query" | "body" | "init"
	map?: { [_: string]: string }
	name: string
	optional?: boolean
	type: string
	// value fills the slot with a verbatim expression instead of a function argument: the
	// program const in paths, or a constant like '"50"' in queries. The expression must
	// evaluate to a string.
	value?: string
	when?: string
	// wire renames a query param on the wire: searchProjects(query) sends ?q=.
	wire?: string
	// wrap assembles a single scalar body param into an object: login(email) sends { email }.
	wrap?: boolean
}

type Endpoint = {
	doc?: string
	// keepalive rides on the request init: true sends it always, a string names the argument.
	keepalive?: boolean | string
	method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE"
	name: string
	params?: Param[]
	path: string
	response: "void" | { schema: unknown; select?: string[] }
}

const reserved = new Set(["fetch", "parseJSON", "requestVoid", "program", "InferOutput", "Response", "response", "parsed"])

// localName picks a generated local that no param shadows.
function localName(params: Param[], ...candidates: string[]): string {
	const taken = new Set(params.map(param => param.name))
	const found = candidates.find(name => !taken.has(name))
	if (!found) throw new Error(`param names leave no room for generated locals (${candidates.join(", ")})`)
	return found
}

function fail(endpoint: string, message: string): never {
	throw new Error(`endpoint ${endpoint}: ${message}`)
}

function isRecord(value: unknown): value is { [_: string]: unknown } {
	return typeof value === "object" && value !== null && !Array.isArray(value)
}

function isParam(value: unknown): value is Param {
	return isRecord(value) && typeof value.name === "string" && typeof value.in === "string" && typeof value.type === "string"
}

// schemaName resolves a referenced schema to its contract export, so typos fail here instead of emitting a broken import.
const schemaNames = new Map<unknown, string>()
const schemaExports = new Set<string>()
for (const [name, value] of Object.entries(contract)) {
	schemaExports.add(name)
	if (typeof value === "object" && value !== null && !schemaNames.has(value)) schemaNames.set(value, name)
}

function schemaName(endpoint: string, schema: unknown): string {
	const name = schemaNames.get(schema)
	if (!name) fail(endpoint, "response schema is not an export of ./src/lib/contract")
	return name
}

const identifier = /^[A-Za-z_$][A-Za-z0-9_$]*$/
const seen = new Set<string>()

function checkEndpoint(raw: unknown): asserts raw is Endpoint {
	if (!isRecord(raw) || typeof raw.name !== "string" || !identifier.test(raw.name)) fail("?", "name must be an identifier")
	const endpoint = raw.name as string
	if (seen.has(endpoint)) fail(endpoint, "name is registered twice")
	seen.add(endpoint)
	if (reserved.has(endpoint)) fail(endpoint, "name collides with the client runtime")
	if (!["GET", "POST", "PUT", "PATCH", "DELETE"].includes(raw.method as string)) fail(endpoint, "method must be GET, POST, PUT, PATCH or DELETE")
	if (typeof raw.path !== "string" || !raw.path.startsWith("/")) fail(endpoint, "path must start with /")
	if (
		raw.keepalive !== undefined &&
		typeof raw.keepalive !== "boolean" &&
		typeof raw.keepalive !== "string"
	)
		fail(endpoint, "keepalive is true or a param name")
	if (raw.keepalive !== undefined && raw.method === "GET") fail(endpoint, "keepalive needs a non-GET request")

	const params: Param[] = Array.isArray(raw.params) ? raw.params : []
	if (!Array.isArray(raw.params) && raw.params !== undefined) fail(endpoint, "params must be an array")
	for (const param of params) {
		if (!isParam(param) || !identifier.test(param.name)) fail(endpoint, "every param needs a name, in and type")
		if (!["path", "query", "body", "init"].includes(param.in))
			fail(endpoint, `param ${param.name} is not path, query, body or init`)
	}
	if (raw.method === "GET" && params.some(param => param.in === "body")) fail(endpoint, "GET endpoints cannot send a body")

	const inPath = new Set(params.filter(param => param.in === "path").map(param => param.name))
	for (const match of raw.path.matchAll(/\{([^{}]+)\}/g)) {
		if (!inPath.has(match[1])) fail(endpoint, `path parameter {${match[1]}} has no param`)
	}
	for (const param of params) {
		if (param.in === "path" && !raw.path.includes(`{${param.name}}`)) fail(endpoint, `path param ${param.name} is not in the path`)
	}

	const query = new Set(params.filter(param => param.in === "query").map(param => param.name))
	const bodyParams = params.filter(param => param.in === "body")
	for (const param of params) {
		if (param.when !== undefined && !query.has(param.when)) fail(endpoint, `when target ${param.when} is not a query param`)
		if (param.in === "body" && (param.optional || param.when !== undefined || param.map !== undefined))
			fail(endpoint, `body param ${param.name} takes no optional, when or map`)
		if (param.in === "query" && param.value !== undefined && (param.optional || param.when !== undefined || param.map !== undefined))
			fail(endpoint, `constant query param ${param.name} takes no optional, when or map`)
		if (param.wrap === true && (param.in !== "body" || bodyParams.length !== 1))
			fail(endpoint, "wrap is only for a single body param")
		if (param.wire !== undefined && (param.in !== "query" || param.wire === ""))
			fail(endpoint, `wire name for ${param.name} is only for query params`)
	}
	if (typeof raw.keepalive === "string" && !params.some(param => param.name === raw.keepalive && param.value === undefined))
		fail(endpoint, `keepalive param ${raw.keepalive} is not a function argument`)

	const byName = new Map(params.filter(param => param.in === "query").map(param => [param.name, param]))
	for (const param of params) {
		if (param.when === undefined) continue
		const seen = new Set([param.name])
		let target = byName.get(param.when)
		while (target) {
			if (seen.has(target.name)) fail(endpoint, `when cycle at ${target.name}`)
			seen.add(target.name)
			if (target.when === undefined) break
			target = byName.get(target.when)
		}
	}

	if (raw.response !== "void") {
		if (!isRecord(raw.response) || !("schema" in raw.response)) fail(endpoint, "response is void or { schema }")
		schemaName(endpoint, (raw.response as { schema: unknown }).schema)
		const select = (raw.response as { select?: unknown }).select
		if (select !== undefined && (!Array.isArray(select) || select.some(key => typeof key !== "string" || key === "")))
			fail(endpoint, "select is an array of field names")
	}

	for (const param of params.filter(item => item.in === "body")) {
		for (const match of param.type.matchAll(/typeof ([A-Za-z_$][A-Za-z0-9_$]*)/g)) {
			if (!schemaExports.has(match[1])) fail(endpoint, `body type references unknown schema ${match[1]}`)
		}
	}
}

if (!Array.isArray(endpoints)) throw new Error("contract/endpoints.ts must export an endpoints array")
for (const endpoint of endpoints) checkEndpoint(endpoint)

function argList(params: Param[]): string {
	return params
		.filter(param => param.value === undefined)
		.map(param => {
			// A literal default already carries a precise type; annotating it again is noise.
			const inferred =
				param.default !== undefined && /^(["']).*\1$/.test(param.default.trim())
					? "string"
					: param.default !== undefined && /^-?\d+(\.\d+)?$/.test(param.default.trim())
						? "number"
						: param.default !== undefined && (param.default.trim() === "true" || param.default.trim() === "false")
							? "boolean"
							: ""
			const annotation = inferred !== "" && param.type.trim() === inferred ? "" : `: ${param.type}`
			const fallback =
				param.default !== undefined ? ` = ${param.default}` : param.optional ? "?" : ""
			return `${param.name}${annotation}${fallback}`
		})
		.join(",\n\t")
}

// isStringType reports a declared type that is already a string once nullish members narrow away.
function isStringType(type: string): boolean {
	const members = type
		.split("|")
		.map(part => part.trim().replace(/\?$/, ""))
		.filter(part => part !== "null" && part !== "undefined" && part !== "")
	return members.length === 1 && members[0] === "string"
}

function encodeValue(param: Param, value: string): string {
	// A value-bound slot fills from its verbatim expression, which must evaluate to a string.
	if (param.value !== undefined) return param.value
	if (!param.map) return isStringType(param.type) ? value : `String(${value})`
	const entries = Object.keys(param.map)
		.sort()
		.map(key => `${JSON.stringify(key)}: ${JSON.stringify(param.map?.[key] ?? "")}`)
		.join(", ")
	return `({ ${entries} } as Record<string, string>)[String(${value})]`
}

function queryBlock(varName: string, params: Param[]): string[] {
	const query = params.filter(param => param.in === "query")
	if (query.length === 0) return []

	const lines = [`\tconst ${varName} = new URLSearchParams()`]
	const byName = new Map(query.map(param => [param.name, param]))
	const blocks = new Map<string | null, string[]>([[null, []]])

	function block(condition: string | null): string[] {
		let lines = blocks.get(condition)
		if (!lines) {
			lines = []
			blocks.set(condition, lines)
		}
		return lines
	}

	function setLine(param: Param, indent: string): string {
		return `${indent}${varName}.set(${JSON.stringify(param.wire ?? param.name)}, ${encodeValue(param, param.name)})`
	}

	for (const param of query) {
		// The gate stacks every when-link above the param, so a dependent only sends with its target.
		// Gates are truthiness checks, mirroring the hand-written clients: an empty string, zero or false omits the param.
		const conditions: string[] = []
		const seen = new Set([param.name])
		let target = param.when !== undefined ? byName.get(param.when) : undefined
		while (target) {
			if (seen.has(target.name)) throw new Error(`when cycle at ${target.name}`)
			seen.add(target.name)
			conditions.unshift(target.name)
			target = target.when !== undefined ? byName.get(target.when) : undefined
		}
		if (param.optional) conditions.push(param.name)

		const lines = block(conditions.length > 0 ? conditions.join(" && ") : null)
		const indent = "\t".repeat(conditions.length + 1)
		lines.push(setLine(param, indent))
	}

	for (const [condition, sets] of blocks) {
		if (sets.length === 0) continue
		if (condition === null) lines.push(...sets)
		else {
			lines.push(`\tif (${condition}) {`)
			lines.push(...sets)
			lines.push("\t}")
		}
	}

	return lines
}

function urlLiteral(path: string, params: Param[]): string {
	const url = path.replace(/\{([^{}]+)\}/g, (_, key: string) => {
		const param = params.find(item => item.in === "path" && item.name === key)
		if (param?.value !== undefined) return `\${${param.value}}`
		return "${encodeURIComponent(" + key + ")}"
	})
	return url.includes("${") ? `\`${url}\`` : JSON.stringify(url)
}

function returnType(schema: string, select: string[] | undefined): string {
	let type = `InferOutput<typeof ${schema}>`
	for (const key of select ?? []) type += `[${JSON.stringify(key)}]`
	return type
}

const schemaImports = new Set<string>()
const schemaTypeImports = new Set<string>()
let usesParse = false
let usesVoid = false
let usesProgram = false
const bodies: string[] = []

// typeSchemas collects the schemas a verbatim type string references, so they import as types.
function typeSchemas(type: string): string[] {
	const found: string[] = []
	for (const match of type.matchAll(/typeof ([A-Za-z_$][A-Za-z0-9_$]*)/g)) {
		if (schemaExports.has(match[1]) && !found.includes(match[1])) found.push(match[1])
	}
	return found
}

for (const raw of endpoints as Endpoint[]) {
	const params = raw.params ?? []
	const queryParams = params.filter(param => param.in === "query")
	const isVoid = raw.response === "void"
	let schema = ""
	let select: string[] | undefined
	if (!isVoid) {
		schema = schemaName(raw.name, (raw.response as { schema: unknown }).schema)
		schemaImports.add(schema)
		select = (raw.response as { select?: string[] }).select
		usesParse = true
	} else usesVoid = true
	for (const param of params.filter(item => item.in === "body")) {
		for (const name of typeSchemas(param.type)) schemaTypeImports.add(name)
	}
	if (params.some(param => param.value === "program")) usesProgram = true

	const args = argList(params)
	const queryVar = localName(params, "query", "searchParams", "requestQuery")
	const urlVar = localName(params, "url", "requestUrl")
	const lines = [...queryBlock(queryVar, params)]
	const literal = urlLiteral(raw.path, params)
	const target = queryParams.length > 0 ? urlVar : literal
	if (queryParams.length > 0) {
		lines.push(`\tlet ${urlVar} = ${literal}`, `\tif (${queryVar}.size > 0) ${urlVar} += \`?\${${queryVar}}\``)
	}

	// keepalive rides the init after the body: a literal sends it always, a name binds the argument.
	const keepalive =
		raw.keepalive === true ? ["keepalive: true"] : typeof raw.keepalive === "string" ? ["keepalive"] : []
	const fields = params.filter(param => param.in === "body")
	const payload =
		fields.length === 0
			? []
			: [
					`body: JSON.stringify(${fields.length > 1 || fields[0].wrap ? `{ ${fields.map(item => item.name).join(", ")} }` : fields[0].name})`,
				]
	const init = [...payload, ...(fields.length > 0 ? ['headers: { "content-type": "application/json" }'] : []), ...keepalive, `method: ${JSON.stringify(raw.method)}`]

	let call: string
	if (raw.method === "GET") call = `const response = await fetch(${target})`
	else if (isVoid)
		call = `await requestVoid(${target}, {\n\t\t${[...keepalive, `method: ${JSON.stringify(raw.method)}`].join(",\n\t\t")},\n\t})`
	else call = `const response = await fetch(${target}, {\n\t\t${init.join(",\n\t\t")},\n\t})`
	lines.push(`\t${call}`)
	if (!isVoid) {
		lines.push(`\tconst parsed = await parseJSON(response, ${schema})`)
		lines.push(select?.length ? `\treturn ${["parsed", ...select].join(".")}` : "\treturn parsed")
	}

	const doc = typeof raw.doc === "string" && raw.doc !== "" ? `// ${raw.doc}\n` : ""
	bodies.push(
		`${doc}export async function ${raw.name}(${args ? `\n\t${args},\n` : ""}): Promise<${isVoid ? "void" : returnType(schema, select)}> {\n${lines.join("\n")}\n}`,
	)
}

const runtime = ["parseJSON", "program", "requestVoid"].filter(name =>
	(name === "program" && usesProgram) || (name === "parseJSON" && usesParse) || (name === "requestVoid" && usesVoid),
)
const typeOnly = [...schemaTypeImports].filter(name => !schemaImports.has(name)).sort()
const contractImports = [...[...schemaImports].sort().map(name => name), ...typeOnly.map(name => `type ${name}`)]
const header = [
	"// Code generated by wt contract from contract/endpoints.ts. DO NOT EDIT.",
	"// Excluded from lint and format; verified byte-for-byte by wt contract --check.",
	...(usesParse ? ["import type { InferOutput } from \"valibot\""] : []),
	...(runtime.length > 0 ? [`import { ${runtime.join(", ")} } from "./api-runtime"`] : []),
	...(contractImports.length > 0 ? [`import { ${contractImports.join(", ")} } from "./contract"`] : []),
].join("\n")

await Bun.write(Bun.env.WT_CLIENT_OUT, `${header}\n\n${bodies.join("\n\n")}\n`)
if (!Bun.env.WT_CLIENT_QUIET) console.log(`wrote src/lib/api.gen.ts (${(endpoints as unknown[]).length} endpoints)`)
