import {
	array,
	integer,
	maxLength,
	minLength,
	minValue,
	number,
	object,
	optional,
	picklist,
	pipe,
	regex,
	string,
	title,
} from "valibot"

// These schemas are the source of truth for the generated Go contract. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.
export const maxName = 40
export const maxMessage = 280

export const reactions = Object.freeze(["rocket", "heart", "eyes", "brain"] as const)

export type Reaction = (typeof reactions)[number]

export const reactionSchema = picklist(reactions)

const nonBlank = regex(/\S/, "This field cannot be blank.")

// Request schemas describe the JSON bodies the client sends.
export const createMessageSchema = pipe(
	object({
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
	}),
	title("CreateMessageRequest"),
)

export const reactMessageSchema = pipe(object({ reaction: reactionSchema }), title("ReactMessageRequest"))

// Response schemas describe what the API returns. The client parses every response through these schemas, and the generated Go types keep the handlers in step.
const countSchema = pipe(number(), integer(), minValue(0))

export const messageSchema = pipe(
	object({
		body: pipe(string(), minLength(1)),
		createdAt: pipe(string(), minLength(1)),
		id: pipe(string(), minLength(1)),
		name: pipe(string(), minLength(1)),
		reactions: object({
			brain: countSchema,
			eyes: countSchema,
			heart: countSchema,
			rocket: countSchema,
		}),
	}),
	title("Message"),
)

export const messagesResponseSchema = pipe(object({ messages: array(messageSchema) }), title("MessagesResponse"))

export const reactResponseSchema = pipe(
	object({ count: countSchema, reaction: reactionSchema }),
	title("ReactResponse"),
)

export const statsResponseSchema = pipe(
	object({ messages: countSchema, reactions: countSchema }),
	title("StatsResponse"),
)

export const errorResponseSchema = pipe(
	object({ code: pipe(string(), minLength(1)), error: pipe(string(), minLength(1)), field: optional(string()) }),
	title("ErrorResponse"),
)

// The client also validates the message id from the URL before calling react. This is not a wire body, so it stays out of the registries.
export const reactMessageInput = object({
	id: pipe(string(), minLength(1)),
	reaction: reactionSchema,
})

// Registries of endpoint contracts. Add new request and response schemas here.
export const requestSchemas = Object.freeze([createMessageSchema, reactMessageSchema])

export const responseSchemas = Object.freeze([
	messageSchema,
	messagesResponseSchema,
	reactResponseSchema,
	statsResponseSchema,
	errorResponseSchema,
])
