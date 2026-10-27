import { z } from "zod"

export const reactions = ["rocket", "heart", "eyes", "brain"] as const

export type Reaction = (typeof reactions)[number]

const reactionSchema = z.enum(reactions)

const messageSchema = z.object({
	body: z.string().min(1),
	createdAt: z.string().min(1),
	id: z.string().min(1),
	name: z.string().min(1),
	reactions: z.object({
		brain: z.number().int().nonnegative(),
		eyes: z.number().int().nonnegative(),
		heart: z.number().int().nonnegative(),
		rocket: z.number().int().nonnegative(),
	}),
})

export type Message = z.infer<typeof messageSchema>

const messagesResponseSchema = z.object({
	messages: z.array(messageSchema),
})

const reactResponseSchema = z.object({
	count: z.number().int().nonnegative(),
	reaction: reactionSchema,
})

const errorResponseSchema = z.object({
	error: z.string(),
})

async function parseJSON<T>(response: Response, schema: z.ZodType<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = errorResponseSchema.safeParse(data)
		throw new Error(error.success ? error.data.error : `The wall API returned ${response.status}`)
	}

	const parsed = schema.safeParse(data)
	if (!parsed.success) {
		throw new Error("The wall API returned an unexpected response shape.")
	}
	return parsed.data
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
