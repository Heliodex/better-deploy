import {
	array,
	type GenericSchema,
	type InferOutput,
	integer,
	literal,
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

const snapshotSchema = object({
	apps: array(appInfoSchema),
	type: literal("snapshot"),
})

export type Snapshot = InferOutput<typeof snapshotSchema>

const logsResponseSchema = object({
	logs: array(string()),
})

export type LogsResult = InferOutput<typeof logsResponseSchema>

const errorResponseSchema = object({
	error: string(),
})

const tokenKey = "wt-admin-token"
const cookieName = "wt_token"

export function getToken(): string | null {
	return localStorage.getItem(tokenKey)
}

export function setToken(token: string): void {
	localStorage.setItem(tokenKey, token)
	setCookie(cookieName, token, 31536000)
}

export function forgetToken(): void {
	localStorage.removeItem(tokenKey)
	setCookie(cookieName, "", 0)
}

function setCookie(name: string, value: string, maxAge: number): void {
	// biome-ignore lint/suspicious/noDocumentCookie: document.cookie is the only cross-browser way to manage cookies; the Cookie Store API is Chromium-only.
	document.cookie = `${encodeURIComponent(name)}=${encodeURIComponent(value)}; path=/; max-age=${maxAge}; SameSite=Lax`
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

export function subscribe(onSnapshot: (apps: AppInfo[]) => void, onStatus: (live: boolean) => void): () => void {
	const source = new EventSource("/events")
	source.addEventListener("open", () => onStatus(true))
	source.addEventListener("error", () => onStatus(false))
	source.addEventListener("message", event => {
		if (typeof event.data !== "string") {
			console.warn("ignoring snapshot without text data")
			return
		}
		let data: unknown
		try {
			data = JSON.parse(event.data)
		} catch {
			console.warn("ignoring malformed snapshot")
			return
		}
		const parsed = safeParse(snapshotSchema, data)
		if (!parsed.success) {
			console.warn("ignoring unexpected snapshot shape")
			return
		}
		onStatus(true)
		onSnapshot(parsed.output.apps)
	})
	return (): void => source.close()
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
