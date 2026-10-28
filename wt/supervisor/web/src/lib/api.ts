import {
	array,
	type GenericSchema,
	type InferOutput,
	integer,
	minLength,
	minValue,
	number,
	object,
	optional,
	picklist,
	pipe,
	safeParse,
	string,
} from "valibot"

const appInfoSchema = object({
	host: pipe(string(), minLength(1)),
	name: pipe(string(), minLength(1)),
	pid: optional(pipe(number(), integer())),
	port: pipe(number(), integer(), minValue(0)),
	startedAt: pipe(string(), minLength(1)),
	status: picklist(["running", "stopped"]),
})

export type AppInfo = InferOutput<typeof appInfoSchema>

const appsResponseSchema = array(appInfoSchema)

const logsResponseSchema = object({
	logs: array(string()),
})

export type LogsResult = InferOutput<typeof logsResponseSchema>

const errorResponseSchema = object({
	error: string(),
})

const tokenKey = "wt-admin-token"

export function getToken(): string | null {
	return localStorage.getItem(tokenKey)
}

export function setToken(token: string): void {
	localStorage.setItem(tokenKey, token)
}

export function forgetToken(): void {
	localStorage.removeItem(tokenKey)
}

interface RequestOptions {
	body?: BodyInit
	method?: string
	token?: string
}

async function parseJSON<T>(response: Response, schema: GenericSchema<T>): Promise<T> {
	const data: unknown = await response.json().catch(() => null)

	if (!response.ok) {
		const error = safeParse(errorResponseSchema, data)
		throw new Error(error.success ? error.output.error : `The supervisor returned ${response.status}`)
	}

	const parsed = safeParse(schema, data)
	if (!parsed.success) throw new Error("The supervisor returned an unexpected response shape.")

	return parsed.output
}

async function request<T>(path: string, options: RequestOptions, schema: GenericSchema<T>): Promise<T> {
	const headers: { [_: string]: string } = {}
	if (options.token) headers.authorization = `Bearer ${options.token}`

	const response = await fetch(path, {
		body: options.body,
		headers,
		method: options.method,
	})
	return parseJSON(response, schema)
}

export function fetchApps(token: string): Promise<AppInfo[]> {
	return request("/apps", { token }, appsResponseSchema)
}

export function fetchApp(token: string, name: string): Promise<AppInfo> {
	return request(`/apps/${encodeURIComponent(name)}`, { token }, appInfoSchema)
}

export async function fetchLogs(token: string, name: string, tail: number): Promise<string[]> {
	const data = await request(`/apps/${encodeURIComponent(name)}/logs?tail=${tail}`, { token }, logsResponseSchema)
	return data.logs
}

export function restartApp(token: string, name: string): Promise<AppInfo> {
	return request(`/apps/${encodeURIComponent(name)}/restart`, { method: "POST", token }, appInfoSchema)
}

export function deployBundle(token: string, name: string, file: File): Promise<AppInfo> {
	const body = new FormData()
	body.append("artifact", file)
	return request(`/apps/${encodeURIComponent(name)}`, { body, method: "POST", token }, appInfoSchema)
}
