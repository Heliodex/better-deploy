import { type BaseIssue, type GenericSchema, type InferOutput, safeParse } from "valibot"
import {
	createMessageSchema,
	errorResponseSchema,
	messageSchema,
	messagesResponseSchema,
	type Reaction,
	reactMessageInput,
	reactResponseSchema,
} from "./contract"

export type Message = InferOutput<typeof messageSchema>

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
		throw new ApiError(`The wall API returned ${response.status}`, "http_error")
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

type ReactResult = InferOutput<typeof reactResponseSchema>

export async function reactToMessage(id: string, reaction: Reaction): Promise<ReactResult> {
	const input = safeParse(reactMessageInput, { id, reaction })
	if (!input.success) throw toValidationError(input.issues)

	const response = await fetch(`/api/messages/${input.output.id}/react`, {
		body: JSON.stringify({ reaction: input.output.reaction }),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, reactResponseSchema)
}
