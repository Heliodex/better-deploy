import { type BaseIssue, type GenericSchema, type InferOutput, safeParse } from "valibot"
import {
	type checkSchema,
	createMonitorSchema,
	cycleResultSchema,
	errorResponseSchema,
	monitorSchema,
	monitorsResponseSchema,
	statsSchema,
} from "./contract"

export type Check = InferOutput<typeof checkSchema>
export type Monitor = InferOutput<typeof monitorSchema>
export type Stats = InferOutput<typeof statsSchema>
export type CycleResult = InferOutput<typeof cycleResultSchema>

// ValidationError carries per-field messages so the form can point at the input that failed.
export class ValidationError extends Error {
	readonly fields: { [_: string]: string }

	constructor(fields: { [_: string]: string }) {
		super(Object.values(fields)[0] ?? "Please check the form.")
		this.name = "ValidationError"
		this.fields = fields
	}
}

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

function toValidationError(issues: readonly BaseIssue<unknown>[]): ValidationError {
	const fields: { [_: string]: string } = {}
	for (const issue of issues) {
		const key = issue.path?.[0]?.key
		if (typeof key !== "string" || key in fields) continue
		fields[key] = issue.message
	}
	return new ValidationError(fields)
}

function apiError(data: unknown, status: number): ApiError {
	const error = safeParse(errorResponseSchema, data)
	if (error.success) return new ApiError(error.output.error, error.output.code, error.output.field ?? "")
	return new ApiError(`The pulse API returned ${status}`, "http_error")
}

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)
	if (!response.ok) throw apiError(data, response.status)

	const parsed = safeParse(schema, data)
	if (!parsed.success) throw new ApiError("The pulse API returned an unexpected response shape.", "invalid")
	return parsed.output
}

export async function fetchMonitors(): Promise<Monitor[]> {
	const response = await fetch("/api/monitors")
	const data = await parseJSON(response, monitorsResponseSchema)
	return data.monitors
}

export function fetchStats(): Promise<Stats> {
	return fetch("/api/stats").then(response => parseJSON(response, statsSchema))
}

export function runChecks(): Promise<CycleResult> {
	return fetch("/api/check", { method: "POST" }).then(response => parseJSON(response, cycleResultSchema))
}

export async function deleteMonitor(id: string): Promise<void> {
	const response = await fetch(`/api/monitors/${encodeURIComponent(id)}`, { method: "DELETE" })
	if (response.ok) return

	const data: unknown = await response.json().catch(() => null)
	throw apiError(data, response.status)
}

function normalizeURL(value: string): string {
	const trimmed = value.trim()
	if (trimmed === "" || trimmed.includes("://")) return trimmed
	return `https://${trimmed}`
}

// validateMonitor normalizes the URL, then uses the same schema the server was generated from.
export function validateMonitor(name: string, url: string): { name: string; url: string } {
	const input = safeParse(createMonitorSchema, { name: name.trim(), url: normalizeURL(url) })
	if (!input.success) throw toValidationError(input.issues)
	return input.output
}

export function createMonitor(name: string, url: string): Promise<Monitor> {
	const input = validateMonitor(name, url)
	return fetch("/api/monitors", {
		body: JSON.stringify(input),
		headers: { "content-type": "application/json" },
		method: "POST",
	}).then(response => parseJSON(response, monitorSchema))
}
