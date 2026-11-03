import { type BaseIssue, type GenericSchema, type InferOutput, safeParse } from "valibot"
import { ApiError } from "./api-error"
import { createNoteSchema, errorResponseSchema, noteSchema, notesResponseSchema } from "./contract"
import { ValidationError } from "./validation-error"

export type Note = InferOutput<typeof noteSchema>

function toValidationError(issues: readonly BaseIssue<unknown>[]): ValidationError {
	const fields: { [_: string]: string } = {}
	for (const issue of issues) {
		const key = issue.path?.[0]?.key
		if (typeof key !== "string" || key in fields) continue
		fields[key] = issue.message
	}
	return new ValidationError(fields)
}

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = safeParse(errorResponseSchema, data)
		if (error.success) throw new ApiError(error.output.error, error.output.code, error.output.field ?? "")
		throw new ApiError(`The notes API returned ${response.status}`, "http_error")
	}

	const parsed = safeParse(schema, data)
	if (!parsed.success) throw new Error("The notes API returned an unexpected response shape.")

	return parsed.output
}

export async function fetchNotes(limit = 50): Promise<Note[]> {
	const response = await fetch(`/api/notes?limit=${limit}`)
	const data = await parseJSON(response, notesResponseSchema)
	return data.notes
}

export async function createNote(text: string): Promise<Note> {
	const input = safeParse(createNoteSchema, { text: text.trim() })
	if (!input.success) throw toValidationError(input.issues)

	const response = await fetch("/api/notes", {
		body: JSON.stringify(input.output),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, noteSchema)
}
