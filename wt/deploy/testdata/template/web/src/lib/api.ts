import { type BaseIssue, type GenericSchema, type InferOutput, safeParse } from "valibot"
import { createNoteSchema, errorResponseSchema, noteSchema, notesResponseSchema } from "./contract"

export type Note = InferOutput<typeof noteSchema>

// ApiError carries the stable code and optional field from the standard error body.
export class ApiError extends Error {
	readonly code: string
	readonly field: string

	constructor(message: string, code = "", field = "") {
		super(message)
		this.name = "ApiError"
		this.code = code
		this.field = field
	}
}

// ValidationError carries per-field messages so the form can show them next to the input that failed.
export class ValidationError extends Error {
	readonly fields: { [_: string]: string }

	constructor(fields: { [_: string]: string }) {
		super(Object.values(fields)[0] ?? "Please check the form.")
		this.name = "ValidationError"
		this.fields = fields
	}
}

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
