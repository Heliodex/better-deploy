import { maxLength, minLength, object, pipe, regex, string } from "valibot"

// These schemas are the source of truth for the generated Go contract. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.
export const maxNote = 280

const nonBlank = regex(/\S/, "Please write something.")

// These schemas describe the JSON bodies.
export const createNoteSchema = object({
	text: pipe(
		string(),
		minLength(1, "Please write something."),
		maxLength(maxNote, `Notes are limited to ${maxNote} characters.`),
		nonBlank,
	),
})
