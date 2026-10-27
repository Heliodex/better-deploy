import {
	array,
	type GenericSchema,
	type InferOutput,
	integer,
	minLength,
	minValue,
	number,
	object,
	picklist,
	pipe,
	safeParse,
	string,
} from "valibot"

export const reactions = ["rocket", "heart", "eyes", "brain"] as const

export type Reaction = (typeof reactions)[number]

const reactionSchema = picklist(reactions)

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

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = safeParse(errorResponseSchema, data)
		throw new Error(error.success ? error.output.error : `The wall API returned ${response.status}`)
	}

	const parsed = safeParse(schema, data)
	if (!parsed.success) {
		throw new Error("The wall API returned an unexpected response shape.")
	}
	return parsed.output
}

export async function fetchMessages(limit = 50): Promise<Message[]> {
	const response = await fetch(`/api/messages?limit=${limit}`)
	const data = await parseJSON(response, messagesResponseSchema)
	return data.messages
}

export async function createMessage(name: string, body: string): Promise<Message> {
	const response = await fetch("/api/messages", {
		body: JSON.stringify({ body, name }),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, messageSchema)
}

export async function reactToMessage(id: string, reaction: Reaction): Promise<{ reaction: Reaction; count: number }> {
	const response = await fetch(`/api/messages/${id}/react`, {
		body: JSON.stringify({ reaction }),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, reactResponseSchema)
}
