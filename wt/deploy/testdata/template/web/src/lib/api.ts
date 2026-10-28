import {
	array,
	type BaseIssue,
	type GenericSchema,
	type InferOutput,
	minLength,
	object,
	pipe,
	safeParse,
	string,
} from "valibot"
import { createNoteSchema } from "./contract"

const noteSchema = object({
	createdAt: pipe(string(), minLength(1)),
	id: pipe(string(), minLength(1)),
	text: pipe(string(), minLength(1)),
})

export type Note = InferOutput<typeof noteSchema>

const notesResponseSchema = object({
	notes: array(noteSchema),
})

const errorResponseSchema = object({
	error: string(),
})

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
		throw new Error(error.success ? error.output.error : `The notes API returned ${response.status}`)
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
