import {
	array,
	type BaseIssue,
	type GenericSchema,
	type InferOutput,
	integer,
	minLength,
	minValue,
	number,
	object,
	pipe,
	safeParse,
	string,
} from "valibot"
import { createMessageSchema, type Reaction, reactionSchema, reactMessageInput } from "./contract"

const countSchema = pipe(number(), integer(), minValue(0))

const messageSchema = object({
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
})

export type Message = InferOutput<typeof messageSchema>

const messagesResponseSchema = object({
	messages: array(messageSchema),
})

const reactResponseSchema = object({
	count: countSchema,
	reaction: reactionSchema,
})

const errorResponseSchema = object({
	error: string(),
})

// ValidationError carries per-field messages so the form can show them next to the input that failed.
export class ValidationError extends Error {
	readonly fields: Record<string, string>

	constructor(fields: Record<string, string>) {
		super(Object.values(fields)[0] ?? "Please check the form.")
		this.name = "ValidationError"
		this.fields = fields
	}
}

function toValidationError(issues: readonly BaseIssue<unknown>[]): ValidationError {
	const fields: Record<string, string> = {}
	for (const issue of issues) {
		const key = issue.path?.[0]?.key
		if (typeof key === "string" && !(key in fields)) fields[key] = issue.message
	}
	return new ValidationError(fields)
}

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = safeParse(errorResponseSchema, data)
		throw new Error(error.success ? error.output.error : `The wall API returned ${response.status}`)
	}

	const parsed = safeParse(schema, data)
	if (!parsed.success) throw new Error("The wall API returned an unexpected response shape.")

	return parsed.output
}

export async function fetchMessages(limit = 50): Promise<Message[]> {
	const response = await fetch(`/api/messages?limit=${limit}`)
	const data = await parseJSON(response, messagesResponseSchema)
	return data.messages
}

export async function createMessage(name: string, body: string): Promise<Message> {
	const input = safeParse(createMessageSchema, { body: body.trim(), name: name.trim() })
	if (!input.success) throw toValidationError(input.issues)

	const response = await fetch("/api/messages", {
		body: JSON.stringify(input.output),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, messageSchema)
}

export async function reactToMessage(id: string, reaction: Reaction): Promise<{ reaction: Reaction; count: number }> {
	const input = safeParse(reactMessageInput, { id, reaction })
	if (!input.success) throw toValidationError(input.issues)

	const response = await fetch(`/api/messages/${input.output.id}/react`, {
		body: JSON.stringify({ reaction: input.output.reaction }),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, reactResponseSchema)
}
