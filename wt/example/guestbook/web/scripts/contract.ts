import { mkdir, rm, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { toJsonSchema } from "@valibot/to-json-schema"
import { createMessageSchema, reactMessageSchema } from "../src/lib/contract"

const outDir = fileURLToPath(new URL("../../contract/schemas", import.meta.url))

const schemas = [
	{ file: "create-message", schema: createMessageSchema, title: "CreateMessageRequest" },
	{ file: "react-message", schema: reactMessageSchema, title: "ReactMessageRequest" },
] as const

await rm(outDir, { force: true, recursive: true })
await mkdir(outDir, { recursive: true })

for (const { file, title, schema } of schemas) {
	const json = {
		...toJsonSchema(schema),
		$id: `https://wt.example/guestbook/${file}.json`,
		title,
	}
	await writeFile(join(outDir, `${file}.json`), `${JSON.stringify(json, null, "\t")}\n`)
	console.log(`wrote contract/schemas/${file}.json`)
}
