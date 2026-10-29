import {
	array,
	boolean,
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
	union,
} from "valibot"

const appInfoSchema = object({
	cpuPercent: pipe(number(), minValue(0)),
	host: pipe(string(), minLength(1)),
	memoryBytes: pipe(number(), minValue(0)),
	name: pipe(string(), minLength(1)),
	pid: optional(pipe(number(), integer())),
	port: pipe(number(), integer(), minValue(0)),
	sampledAt: pipe(string(), minLength(1)),
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

export const logLevels = ["trace", "debug", "info", "warn", "error", "fatal"] as const

export type LogLevel = (typeof logLevels)[number]

const logEntrySchema = object({
	level: picklist(logLevels),
	message: string(),
	source: picklist(["app", "supervisor"]),
	time: string(),
})

export type LogEntry = InferOutput<typeof logEntrySchema>

const logsResponseSchema = object({
	logs: array(logEntrySchema),
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
	json?: unknown
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
	let { body } = options
	if (options.json !== undefined) {
		headers["content-type"] = "application/json"
		body = JSON.stringify(options.json)
	}

	const response = await fetch(path, {
		body,
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

const resourceSampleSchema = object({
	at: pipe(string(), minLength(1)),
	cpuPercent: pipe(number(), minValue(0)),
	memoryBytes: pipe(number(), minValue(0)),
})

export type ResourceSample = InferOutput<typeof resourceSampleSchema>

const resourceHistorySchema = object({
	samples: array(resourceSampleSchema),
})

export async function fetchResourceHistory(token: string, name: string): Promise<ResourceSample[]> {
	const data = await request(`/apps/${encodeURIComponent(name)}/resources`, { token }, resourceHistorySchema)
	return data.samples
}

const backupSettingsSchema = object({
	enabled: boolean(),
	every: pipe(string(), minLength(1)),
	keep: pipe(number(), integer(), minValue(1)),
})

export type BackupSettings = InferOutput<typeof backupSettingsSchema>

export const backupKinds = ["auto", "manual", ""] as const

export type BackupKind = (typeof backupKinds)[number]

const backupInfoSchema = object({
	createdAt: pipe(string(), minLength(1)),
	kind: picklist(backupKinds),
	name: pipe(string(), minLength(1)),
	parent: string(),
	size: pipe(number(), minValue(0)),
})

export type BackupInfo = InferOutput<typeof backupInfoSchema>

const backupsResponseSchema = object({
	backups: array(backupInfoSchema),
	current: string(),
	database: boolean(),
	settings: backupSettingsSchema,
})

export type BackupsResult = InferOutput<typeof backupsResponseSchema>

const logStreamSchema = union([
	object({
		logs: array(logEntrySchema),
		type: literal("backlog"),
	}),
	object({
		log: logEntrySchema,
		type: literal("line"),
	}),
])

export async function fetchLogs(token: string, name: string, tail: number): Promise<LogEntry[]> {
	const data = await request(`/apps/${encodeURIComponent(name)}/logs?tail=${tail}`, { token }, logsResponseSchema)
	return data.logs
}

export function subscribeLogs(
	name: string,
	tail: number,
	onBacklog: (logs: LogEntry[]) => void,
	onLine: (entry: LogEntry) => void,
): () => void {
	const source = new EventSource(`/apps/${encodeURIComponent(name)}/logs/stream?tail=${tail}`)
	source.addEventListener("message", event => {
		if (typeof event.data !== "string") return
		let data: unknown
		try {
			data = JSON.parse(event.data)
		} catch {
			console.warn("ignoring malformed log frame")
			return
		}
		const parsed = safeParse(logStreamSchema, data)
		if (!parsed.success) {
			console.warn("ignoring unexpected log frame shape")
			return
		}
		if (parsed.output.type === "backlog") onBacklog(parsed.output.logs)
		else onLine(parsed.output.log)
	})
	return (): void => source.close()
}

export function restartApp(token: string, name: string): Promise<AppInfo> {
	return request(`/apps/${encodeURIComponent(name)}/restart`, { method: "POST", token }, appInfoSchema)
}

export function deployBundle(token: string, name: string, file: File): Promise<AppInfo> {
	const body = new FormData()
	body.append("artifact", file)
	return request(`/apps/${encodeURIComponent(name)}`, { body, method: "POST", token }, appInfoSchema)
}

export function fetchBackups(token: string, name: string): Promise<BackupsResult> {
	return request(`/apps/${encodeURIComponent(name)}/backups`, { token }, backupsResponseSchema)
}

export function createBackup(token: string, name: string): Promise<BackupsResult> {
	return request(`/apps/${encodeURIComponent(name)}/backups`, { method: "POST", token }, backupsResponseSchema)
}

export function saveBackupSettings(token: string, name: string, settings: BackupSettings): Promise<BackupsResult> {
	return request(
		`/apps/${encodeURIComponent(name)}/backups/config`,
		{ json: settings, method: "PUT", token },
		backupsResponseSchema,
	)
}

export function restoreBackup(token: string, name: string, backup: string): Promise<BackupsResult> {
	return request(
		`/apps/${encodeURIComponent(name)}/backups/${encodeURIComponent(backup)}/restore`,
		{ method: "POST", token },
		backupsResponseSchema,
	)
}
