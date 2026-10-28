/// <reference types="bun" />
import { rm } from "node:fs/promises"
import { toJsonSchema } from "@valibot/to-json-schema"
import { requestSchemas } from "../src/lib/contract"

// Every schema in the contract registry becomes one JSON file. Filenames come from their v.title() and the app name comes from app.json, so this script never changes.
const schemasDir = `${import.meta.dir}/../../contract/schemas`

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null

const raw: unknown = await Bun.file(new URL("../../app.json", import.meta.url)).json()
if (!isRecord(raw)) throw new Error("app.json is not an object")
const appName = raw.name
if (typeof appName !== "string" || appName === "") throw new Error("app.json has no name")

const kebab = (value: string): string => value.replaceAll(/([a-z0-9])([A-Z])/g, "$1-$2").toLowerCase()

await rm(schemasDir, { force: true, recursive: true })

for (const schema of requestSchemas) {
	const json = toJsonSchema(schema)
	if (typeof json.title !== "string" || json.title === "") throw new Error("request schema has no v.title()")
	const file = kebab(json.title.replace(/Request$/, ""))
	const document = { ...json, $id: `https://wt.example/${appName}/${file}.json` }
	await Bun.write(`${schemasDir}/${file}.json`, `${JSON.stringify(document, null, "\t")}\n`)
	console.log(`wrote contract/schemas/${file}.json`)
}
