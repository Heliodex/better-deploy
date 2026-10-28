import { maxLength, minLength, object, picklist, pipe, regex, string } from "valibot"

// These schemas are the source of truth for the generated Go contract.
export const maxName = 40
export const maxMessage = 280

export const reactions = Object.freeze(["rocket", "heart", "eyes", "brain"] as const)

export type Reaction = (typeof reactions)[number]

export const reactionSchema = picklist(reactions)

const nonBlank = regex(/\S/, "This field cannot be blank.")

// These schemas describe the JSON bodies. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.
export const createMessageSchema = object({
	body: pipe(
		string(),
		minLength(1, "Please write a message."),
		maxLength(maxMessage, `Messages are limited to ${maxMessage} characters.`),
		nonBlank,
	),
	name: pipe(
		string(),
		minLength(1, "Please enter your name."),
		maxLength(maxName, `Names are limited to ${maxName} characters.`),
		nonBlank,
	),
})

export const reactMessageSchema = object({
	reaction: reactionSchema,
})

// The client also validates the message id from the URL before calling react.
export const reactMessageInput = object({
	id: pipe(string(), minLength(1)),
	reaction: reactionSchema,
})
