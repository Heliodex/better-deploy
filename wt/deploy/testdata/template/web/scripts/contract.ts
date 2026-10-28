/// <reference types="bun" />
import { rm } from "node:fs/promises"
import { toJsonSchema } from "@valibot/to-json-schema"
import { createNoteSchema } from "../src/lib/contract"

const schemas = Object.freeze([{ file: "create-note", schema: createNoteSchema, title: "CreateNoteRequest" }] as const)

const schemasDir = `${import.meta.dir}/../../contract/schemas`

await rm(schemasDir, { force: true, recursive: true })

for (const { file, title, schema } of schemas) {
	const json = {
		...toJsonSchema(schema),
		$id: `https://wt.example/__APP_NAME__/${file}.json`,
		title,
	}
	await Bun.write(`${schemasDir}/${file}.json`, `${JSON.stringify(json, null, "\t")}\n`)
	console.log(`wrote contract/schemas/${file}.json`)
}
