export interface Check {
	checkedAt: string
	error: string
	latencyMs: number
	ok: boolean
	status: number
}

export interface Monitor {
	createdAt: string
	history: Check[]
	id: string
	last: Check | null
	name: string
	uptime: number
	url: string
}

export interface Stats {
	avgLatencyMs: number
	checks24h: number
	down: number
	lastCheckAt: string
	monitors: number
	up: number
}

export interface CycleResult {
	at: string
	checked: number
	down: number
	up: number
}

// ValidationError carries per-field messages so the form can point at the input that failed.
export class ValidationError extends Error {
	readonly fields: { [_: string]: string }

	constructor(fields: { [_: string]: string }) {
		super(Object.values(fields)[0] ?? "Please check the form.")
		this.name = "ValidationError"
		this.fields = fields
	}
}

function isErrorBody(value: unknown): value is { error: string } {
	if (typeof value !== "object" || value === null) return false
	if (!("error" in value)) return false
	return typeof value.error === "string"
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const response = await fetch(path, init)
	const data: unknown = await response.json().catch(() => null)
	if (!response.ok) {
		throw new Error(isErrorBody(data) ? data.error : `The pulse API returned ${response.status}`)
	}
	return data as T
}

export async function fetchMonitors(): Promise<Monitor[]> {
	const data = await request<{ monitors: Monitor[] }>("/api/monitors")
	return data.monitors
}

export function fetchStats(): Promise<Stats> {
	return request<Stats>("/api/stats")
}

export function runChecks(): Promise<CycleResult> {
	return request<CycleResult>("/api/check", { method: "POST" })
}

export async function deleteMonitor(id: string): Promise<void> {
	const response = await fetch(`/api/monitors/${encodeURIComponent(id)}`, { method: "DELETE" })
	if (response.ok) return

	const data: unknown = await response.json().catch(() => null)
	throw new Error(isErrorBody(data) ? data.error : `The pulse API returned ${response.status}`)
}

// validateMonitor mirrors the server's checks so the form can fail before the request leaves the browser.
export function validateMonitor(name: string, url: string): { name: string; url: string } {
	const fields: { [_: string]: string } = {}
	const cleanName = name.trim()
	const cleanURL = url.trim()

	if (cleanName === "") fields.name = "Give the monitor a name."
	else if (cleanName.length > 60) fields.name = "Names are limited to 60 characters."

	let parsed: URL | null = null
	try {
		parsed = new URL(cleanURL.includes("://") ? cleanURL : `https://${cleanURL}`)
	} catch {
		parsed = null
	}

	if (parsed === null || parsed.hostname === "") {
		fields.url = "Enter a valid URL."
	} else if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
		fields.url = "Only http and https URLs are supported."
	} else if (cleanURL.length > 500) {
		fields.url = "URLs are limited to 500 characters."
	}

	if (Object.keys(fields).length > 0) throw new ValidationError(fields)
	return { name: cleanName, url: parsed?.toString() ?? cleanURL }
}

export function createMonitor(name: string, url: string): Promise<Monitor> {
	const input = validateMonitor(name, url)
	return request<Monitor>("/api/monitors", {
		body: JSON.stringify(input),
		headers: { "content-type": "application/json" },
		method: "POST",
	})
}
