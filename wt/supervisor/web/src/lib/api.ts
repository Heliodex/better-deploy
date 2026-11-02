import { routes } from "./routes.gen"
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
	unknown as unknownSchema,
} from "valibot"

const appInfoSchema = object({
	cpuPercent: pipe(number(), minValue(0)),
	currentDigest: optional(string()),
	diskBytes: optional(pipe(number(), minValue(0))),
	diskLimitBytes: optional(pipe(number(), minValue(0))),
	historyBytes: pipe(number(), minValue(0)),
	host: pipe(string(), minLength(1)),
	lastError: optional(string()),
	lastHealthy: optional(string()),
	memoryBytes: pipe(number(), minValue(0)),
	name: pipe(string(), minLength(1)),
	overQuota: optional(boolean()),
	pid: optional(pipe(number(), integer())),
	port: pipe(number(), integer(), minValue(0)),
	ready: optional(boolean()),
	sampledAt: pipe(string(), minLength(1)),
	startedAt: pipe(string(), minLength(1)),
	status: picklist(["running", "stopped", "unhealthy", "restarting"]),
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

export const getToken = (): string | null => localStorage.getItem(tokenKey)

/** Store the token for bearer requests and ask the supervisor for its HttpOnly cookie, which EventSource needs. */
export async function setToken(token: string): Promise<void> {
	localStorage.setItem(tokenKey, token)
	await fetch(routes.login, {
		body: JSON.stringify({ token }),
		headers: { "content-type": "application/json" },
		method: "POST",
	}).catch(() => undefined)
}

export async function forgetToken(): Promise<void> {
	localStorage.removeItem(tokenKey)
	await fetch(routes.logout, { method: "POST" }).catch(() => undefined)
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

export const fetchApps = (token: string): Promise<AppInfo[]> => request(routes.apps, { token }, appsResponseSchema)

export function subscribe(onSnapshot: (apps: AppInfo[]) => void, onStatus: (live: boolean) => void): () => void {
	const source = new EventSource(routes.events)
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

export const fetchApp = (token: string, name: string): Promise<AppInfo> =>
	request(routes.app(name), { token }, appInfoSchema)

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
	const data = await request(routes.appResources(name), { token }, resourceHistorySchema)
	return data.samples
}

const backupSettingsSchema = object({
	enabled: boolean(),
	every: pipe(string(), minLength(1)),
	keep: pipe(number(), integer(), minValue(1)),
})

export type BackupSettings = InferOutput<typeof backupSettingsSchema>

export const backupKinds = ["auto", "import", "manual", ""] as const

export type BackupKind = (typeof backupKinds)[number]

const backupInfoSchema = object({
	createdAt: pipe(string(), minLength(1)),
	dataSize: optional(pipe(number(), minValue(0))),
	hasData: optional(boolean()),
	kind: picklist(backupKinds),
	name: pipe(string(), minLength(1)),
	parent: string(),
	size: pipe(number(), minValue(0)),
})

export type BackupInfo = InferOutput<typeof backupInfoSchema>

const backupsResponseSchema = object({
	backups: array(backupInfoSchema),
	created: optional(string()),
	current: string(),
	database: boolean(),
	settings: backupSettingsSchema,
})

export type BackupsResult = InferOutput<typeof backupsResponseSchema>

const deploymentInfoSchema = object({
	bytes: pipe(number(), minValue(0)),
	current: optional(boolean()),
	deployedAt: pipe(string(), minLength(1)),
	digest: string(),
	error: optional(string()),
	id: pipe(string(), minLength(1)),
	rebuildable: boolean(),
	runtime: optional(string()),
	size: pipe(number(), minValue(0)),
	status: optional(picklist(["success", "failed"])),
	target: optional(string()),
})

export type DeploymentInfo = InferOutput<typeof deploymentInfoSchema>

const deploymentRunSchema = object({
	digest: string(),
	error: optional(string()),
	id: pipe(string(), minLength(1)),
	mode: optional(string()),
	status: picklist(["queued", "running", "success", "failed", "canceled"]),
})

export type DeploymentRun = InferOutput<typeof deploymentRunSchema>

const deploymentsSchema = array(deploymentInfoSchema)

const databaseTableStatSchema = object({
	bytes: pipe(number(), minValue(0)),
	name: pipe(string(), minLength(1)),
	rows: pipe(number(), minValue(0)),
})

export type DatabaseTableStat = InferOutput<typeof databaseTableStatSchema>

const databaseOverviewSchema = object({
	database: boolean(),
	tables: array(string()),
	version: pipe(number(), minValue(0)),
	stats: optional(array(databaseTableStatSchema)),
})

export type DatabaseOverview = InferOutput<typeof databaseOverviewSchema>

const databaseImportSchema = object({
	imported: boolean(),
})

const databaseMaintenanceSchema = object({
	op: pipe(string(), minLength(1)),
	result: optional(string()),
})

export type DatabaseMaintenance = InferOutput<typeof databaseMaintenanceSchema>

const databaseRowsSchema = object({
	columns: array(string()),
	limit: pipe(number(), integer(), minValue(1)),
	offset: pipe(number(), integer(), minValue(0)),
	rows: array(array(unknownSchema())),
	table: pipe(string(), minLength(1)),
	total: pipe(number(), integer(), minValue(0)),
	truncated: boolean(),
})

export type DatabaseRows = InferOutput<typeof databaseRowsSchema>

const rowChangeSchema = object({
	columns: array(string()),
	new: optional(array(unknownSchema())),
	old: optional(array(unknownSchema())),
	op: picklist(["insert", "update", "delete"]),
	rowId: optional(pipe(number(), integer(), minValue(0))),
	table: pipe(string(), minLength(1)),
})

export type RowChange = InferOutput<typeof rowChangeSchema>

const storageChangeSchema = object({
	params: array(unknownSchema()),
	rows: optional(array(rowChangeSchema)),
	rowsAffected: pipe(number(), integer(), minValue(0)),
	rowsTruncated: optional(boolean()),
	sql: pipe(string(), minLength(1)),
	time: pipe(string(), minLength(1)),
})

export type StorageChange = InferOutput<typeof storageChangeSchema>

const storageChangesResponseSchema = object({
	changes: array(storageChangeSchema),
})

export type StorageChangesResult = InferOutput<typeof storageChangesResponseSchema>

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
	const data = await request(`${routes.appLogs(name)}?tail=${tail}`, { token }, logsResponseSchema)
	return data.logs
}

export function subscribeLogs(
	name: string,
	tail: number,
	onBacklog: (logs: LogEntry[]) => void,
	onLine: (entry: LogEntry) => void,
): () => void {
	const source = new EventSource(`${routes.appLogsStream(name)}?tail=${tail}`)
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

export const restartApp = (token: string, name: string): Promise<AppInfo> =>
	request(routes.appRestart(name), { method: "POST", token }, appInfoSchema)

export const stopApp = (token: string, name: string): Promise<AppInfo> =>
	request(routes.appStop(name), { method: "POST", token }, appInfoSchema)

export const startApp = (token: string, name: string): Promise<AppInfo> =>
	request(routes.appStart(name), { method: "POST", token }, appInfoSchema)

const deleteAppSchema = object({
	name: pipe(string(), minLength(1)),
	purged: boolean(),
})

export const deleteApp = (token: string, name: string, purge: boolean): Promise<boolean> =>
	request(
		`${routes.app(name)}${purge ? "?purge=1" : ""}`,
		{ method: "DELETE", token },
		deleteAppSchema,
	).then(result => result.purged)

const egressRuleSchema = object({
	cidr: pipe(string(), minLength(1)),
	port: pipe(number(), integer(), minValue(1)),
})

const appConfigSchema = object({
	allow_callers: array(string()),
	disk_mb: pipe(number(), minValue(0)),
	egress: array(egressRuleSchema),
	env_keys: array(string()),
	secret_keys: optional(array(string())),
	health: pipe(string(), minLength(1)),
	health_failures: pipe(number(), minValue(1)),
	health_interval: string(),
	host: pipe(string(), minLength(1)),
	job_concurrency: pipe(number(), minValue(1)),
	hosts: array(string()),
	max_concurrency: pipe(number(), minValue(0)),
	memory_mb: pipe(number(), minValue(0)),
	name: pipe(string(), minLength(1)),
	network: picklist(["inherit", "none", "storage"]),
	request_timeout: string(),
	restart_on_unhealthy: boolean(),
	restart_required: optional(boolean()),
	schedule: array(unknownSchema()),
	startup_timeout: string(),
})

export type AppConfig = InferOutput<typeof appConfigSchema>

export interface AppConfigPatch {
	allow_callers?: string[]
	disk_mb?: number
	egress?: { cidr: string; port: number }[]
	health?: string
	health_failures?: number
	health_interval?: string
	host?: string
	hosts?: string[]
	job_concurrency?: number
	max_concurrency?: number
	memory_mb?: number
	network?: "inherit" | "none" | "storage"
	request_timeout?: string
	restart_on_unhealthy?: boolean
	startup_timeout?: string
}

export const fetchAppConfig = (token: string, name: string): Promise<AppConfig> =>
	request(routes.appConfig(name), { token }, appConfigSchema)

export const saveAppConfig = (token: string, name: string, patch: AppConfigPatch): Promise<AppConfig> =>
	request(routes.appConfig(name), { json: patch, method: "PATCH", token }, appConfigSchema)

const secretKeysSchema = object({
	keys: array(string()),
})

const secretMutationSchema = object({
	key: string(),
	restart_required: boolean(),
})

export const fetchSecretKeys = (token: string, name: string): Promise<string[]> =>
	request(routes.appSecrets(name), { token }, secretKeysSchema).then(result => result.keys)

export const setAppSecret = (token: string, name: string, key: string, value: string): Promise<boolean> =>
	request(
		routes.appSecret(name, key),
		{ json: { value }, method: "PUT", token },
		secretMutationSchema,
	).then(result => result.restart_required)

export const deleteAppSecret = (token: string, name: string, key: string): Promise<boolean> =>
	request(
		routes.appSecret(name, key),
		{ method: "DELETE", token },
		secretMutationSchema,
	).then(result => result.restart_required)

export async function deployBundle(token: string, name: string, file: File): Promise<AppInfo> {
	const body = new FormData()
	body.append("artifact", file)
	const path = routes.appDeployments(name)
	let run = await request(path, { body, method: "POST", token }, deploymentRunSchema)
	while (run.status === "queued" || run.status === "running") {
		await new Promise(resolve => setTimeout(resolve, 500))
		run = await request(`${path}/${encodeURIComponent(run.id)}`, { token }, deploymentRunSchema)
	}
	if (run.status !== "success") throw new Error(run.error ?? `deployment ${run.status}`)
	return fetchApp(token, name)
}

export const fetchBackups = (token: string, name: string): Promise<BackupsResult> =>
	request(routes.appBackup(name), { token }, backupsResponseSchema)

export const createBackup = (token: string, name: string): Promise<BackupsResult> =>
	request(routes.appBackup(name), { method: "POST", token }, backupsResponseSchema)

export const saveBackupSettings = (token: string, name: string, settings: BackupSettings): Promise<BackupsResult> =>
	request(
		routes.appBackup(name, "config"),
		{ json: settings, method: "PUT", token },
		backupsResponseSchema,
	)

export const restoreBackup = (token: string, name: string, backup: string): Promise<BackupsResult> =>
	request(
		routes.appBackup(name, `${encodeURIComponent(backup)}/restore`),
		{ method: "POST", token },
		backupsResponseSchema,
	)

export const importBackup = (token: string, name: string, file: File): Promise<BackupsResult> => {
	const body = new FormData()
	body.append("snapshot", file)
	return request(
		routes.appBackup(name, "import"),
		{ body, method: "POST", token },
		backupsResponseSchema,
	)
}

export const fetchDeployments = (token: string, name: string): Promise<DeploymentInfo[]> =>
	request(routes.appDeployments(name), { token }, deploymentsSchema)

export const rollbackDeployment = (token: string, name: string, id: string): Promise<AppInfo> =>
	request(
		routes.appRollback(name, id),
		{ method: "POST", token },
		appInfoSchema,
	)

export const fetchDatabase = (token: string, name: string, stats = false): Promise<DatabaseOverview> =>
	request(`${routes.appDatabase(name)}${stats ? "?stats=1" : ""}`, { token }, databaseOverviewSchema)

export const importDatabase = (token: string, name: string, file: File): Promise<boolean> => {
	const body = new FormData()
	body.append("database", file)
	return request(
		routes.appDatabasePath(name, "import"),
		{ body, method: "POST", token },
		databaseImportSchema,
	).then(result => result.imported)
}

export const maintainDatabase = (token: string, name: string, op: string): Promise<DatabaseMaintenance> =>
	request(
		routes.appDatabasePath(name, "maintenance"),
		{ json: { op }, method: "POST", token },
		databaseMaintenanceSchema,
	)

export function fetchDatabaseRows(
	token: string,
	name: string,
	table: string,
	limit: number,
	offset: number,
): Promise<DatabaseRows> {
	const query = new URLSearchParams({ limit: `${limit}`, offset: `${offset}`, table })
	return request(`${routes.appDatabasePath(name, "rows")}?${query}`, { token }, databaseRowsSchema)
}

export function fetchChanges(token: string, name: string, limit = 200): Promise<StorageChangesResult> {
	const query = new URLSearchParams({ limit: `${limit}` })
	return request(
		`${routes.appDatabasePath(name, "changes")}?${query}`,
		{ token },
		storageChangesResponseSchema,
	)
}
