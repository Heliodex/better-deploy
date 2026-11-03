import { type GenericSchema, type InferOutput, safeParse } from "valibot"
import {
	checkinSchema,
	checkinsResponseSchema,
	errorResponseSchema,
	logoutResponseSchema,
	sessionResponseSchema,
} from "./contract"

export type Checkin = InferOutput<typeof checkinSchema>
export type Session = InferOutput<typeof sessionResponseSchema>

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

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = safeParse(errorResponseSchema, data)
		if (error.success) throw new ApiError(error.output.error, error.output.code, error.output.field ?? "")
		throw new ApiError(`The passport API returned ${response.status}`, "http_error")
	}

	const parsed = safeParse(schema, data)
	if (!parsed.success) throw new Error("The passport API returned an unexpected response shape.")

	return parsed.output
}

export async function fetchSession(): Promise<Session> {
	const response = await fetch("/api/session", { credentials: "same-origin" })
	return parseJSON(response, sessionResponseSchema)
}

export async function fetchCheckins(limit = 50): Promise<Checkin[]> {
	const response = await fetch(`/api/checkins?limit=${limit}`, { credentials: "same-origin" })
	const data = await parseJSON(response, checkinsResponseSchema)
	return data.checkins
}

export async function createCheckin(message: string): Promise<Checkin> {
	const response = await fetch("/api/checkins", {
		body: JSON.stringify({ message }),
		credentials: "same-origin",
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	return parseJSON(response, checkinSchema)
}

async function throwAPIError(response: Response): Promise<never> {
	const data: unknown = await response.json().catch(() => null)
	const error = safeParse(errorResponseSchema, data)
	if (error.success) throw new ApiError(error.output.error, error.output.code, error.output.field ?? "")

	throw new ApiError(`The passport API returned ${response.status}`, "http_error")
}

export async function deleteCheckin(id: string): Promise<void> {
	const response = await fetch(`/api/checkins/${encodeURIComponent(id)}`, {
		credentials: "same-origin",
		method: "DELETE",
	})
	if (response.ok) return

	await throwAPIError(response)
}

export async function completeLogin(code: string, state: string): Promise<void> {
	const response = await fetch("/api/auth/callback", {
		body: JSON.stringify({ code, state }),
		credentials: "same-origin",
		headers: { "content-type": "application/json" },
		method: "POST",
	})
	if (response.ok) return

	await throwAPIError(response)
}

export async function logout(): Promise<{ endSessionUrl: string }> {
	const response = await fetch("/api/logout", {
		credentials: "same-origin",
		method: "POST",
	})
	return parseJSON(response, logoutResponseSchema)
}
