import { array, maxLength, minLength, object, pipe, regex, string, title } from "valibot"

// These schemas are the source of truth for the generated Go contract. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.
export const maxNote = 280

const nonBlank = regex(/\S/, "Please write something.")

// Request schemas describe the JSON bodies the client sends.
export const createNoteSchema = pipe(
	object({
		text: pipe(
			string(),
			minLength(1, "Please write something."),
			maxLength(maxNote, `Notes are limited to ${maxNote} characters.`),
			nonBlank,
		),
	}),
	title("CreateNoteRequest"),
)

// Response schemas describe what the API returns. The client parses every response through these schemas, and the generated Go types keep the handlers in step.
export const noteSchema = pipe(
	object({
		createdAt: pipe(string(), minLength(1)),
		id: pipe(string(), minLength(1)),
		text: pipe(string(), minLength(1)),
	}),
	title("Note"),
)

export const notesResponseSchema = pipe(object({ notes: array(noteSchema) }), title("NotesResponse"))

export const errorResponseSchema = pipe(object({ error: string() }), title("ErrorResponse"))

// Registries of endpoint contracts. Add new request and response schemas here.
export const requestSchemas = Object.freeze([createNoteSchema])

export const responseSchemas = Object.freeze([noteSchema, notesResponseSchema, errorResponseSchema])
