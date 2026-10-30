<script lang="ts">
import { onMount } from "svelte"
import AllAppsOverview from "../lib/AllAppsOverview.svelte"
import AppSidebar from "../lib/AppSidebar.svelte"
import {
	type AppInfo,
	type BackupInfo,
	type BackupSettings,
	type BackupsResult,
	createBackup,
	type DatabaseRows,
	type DeploymentInfo,
	deployBundle,
	fetchApp,
	fetchApps,
	fetchBackups,
	fetchChanges,
	fetchDatabase,
	fetchDatabaseRows,
	fetchDeployments,
	fetchResourceHistory,
	importBackup,
	importDatabase,
	forgetToken,
	getToken,
	type LogEntry,
	type LogLevel,
	type ResourceSample,
	maintainDatabase,
	restartApp,
	restoreBackup,
	rollbackDeployment,
	type StorageChange,
	saveBackupSettings,
	setToken,
	subscribe,
	subscribeLogs,
} from "../lib/api"
import BackupsTab from "../lib/BackupsTab.svelte"
import { palette } from "../lib/chart"
import DatabaseTab from "../lib/DatabaseTab.svelte"
import DeploymentsTab from "../lib/DeploymentsTab.svelte"
import { formatTime, uptime } from "../lib/format"
import LogsPane from "../lib/LogsPane.svelte"
import { levelIndex } from "../lib/log-format"
import OverviewTab from "../lib/OverviewTab.svelte"
import { type Tab, tabs } from "../lib/tabs"

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 520
const MIN_VIEWPORT_FOR_SIDEBAR = 700
/** Pixels a separator moves per arrow-key press (tripled while Shift is held). */
const KEY_STEP = 16
/** Log pane height bounds; the upper bound also leaves room for the detail pane. */
const MIN_LOGS = 100
const MAX_LOGS = 600
/** Keep the live log stream bounded when a chatty app runs for a long time. */
const MAX_LOG_LINES = 5000
/** Treat the log view as "following" until the user scrolls this far from the bottom. */
const FOLLOW_THRESHOLD = 24
/** Thirty minutes at the supervisor's one-second sampling interval. */
const MAX_HISTORY_POINTS = 1800

let token = $state<string | null>(null)
let tokenInput = $state("")
let apps = $state<AppInfo[]>([])
let selected = $state<string | null>(null)
let tab = $state<Tab>("overview")
let tablistEl = $state<HTMLDivElement | null>(null)
let detail = $state<AppInfo | null>(null)
let logs = $state<LogEntry[]>([])
let logsOpen = $state(true)
let logQuery = $state("")
let tail = $state(200)
let error = $state("")
let loading = $state(true)
let restarting = $state(false)
let deploying = $state(false)
let actionMessage = $state("")
let confirmRestart = $state(false)
let live = $state(false)
let now = $state(Date.now())
let sidebarWidth = $state(280)
let logsHeight = $state(240)
let gripY = $state<number | null>(null)
let logsGripX = $state<number | null>(null)
let stopStream: (() => void) | null = null
let stopLogs: (() => void) | null = null
let logPane = $state<HTMLDivElement | null>(null)
let followLogs = $state(true)
let minLevel = $state<LogLevel>("trace")
let resourceHistory = $state<ResourceSample[]>([])
let overviewHistories = $state<{ [_: string]: ResourceSample[] }>({})
let overviewPending = $state<{ [_: string]: boolean }>({})
let backupSettings = $state<BackupSettings>({ enabled: true, every: "24h", keep: 7 })
let backupList = $state<BackupInfo[]>([])
let backupCurrent = $state("")
let selectedBackup = $state<string | null>(null)
let backupDatabase = $state(false)
let backupsLoading = $state(false)
let backupsError = $state("")
let backingUp = $state(false)
let importingBackups = $state(false)
let savingBackups = $state(false)
let restoringBackup = $state<string | null>(null)
let confirmRestore = $state<string | null>(null)
let databaseExists = $state(false)
let databaseMessage = $state("")
let databaseImporting = $state(false)
let databaseMaintaining = $state(false)
let databaseVersion = $state(0)
let databaseTables = $state<string[]>([])
let databaseTable = $state<string | null>(null)
let databaseRows = $state<DatabaseRows | null>(null)
let databaseLoadedFor = $state<string | null>(null)
let databaseLoading = $state(false)
let databaseRowsLoading = $state(false)
let databaseError = $state("")
let databaseLimit = $state(50)
let databaseOffset = $state(0)
let changes = $state<StorageChange[]>([])
let changesLoading = $state(false)
let changesError = $state("")
let deploymentList = $state<DeploymentInfo[]>([])
let deploymentsLoading = $state(false)
let deploymentsError = $state("")
let deploymentsLoadedFor = $state<string | null>(null)
let rollingBack = $state<string | null>(null)

/** Merge history by timestamp so a late fetch cannot clobber samples that arrived over SSE. */
function mergeHistory(existing: ResourceSample[], incoming: ResourceSample[]): ResourceSample[] {
	const byTime = new Map<string, ResourceSample>()
	for (const sample of [...existing, ...incoming]) {
		const at = Date.parse(sample.at)
		if (Number.isNaN(at) || at <= 0) continue
		byTime.set(sample.at, sample)
	}
	return [...byTime.values()].toSorted((a, b) => Date.parse(a.at) - Date.parse(b.at)).slice(-MAX_HISTORY_POINTS)
}

function appendHistory(sample: ResourceSample): void {
	resourceHistory = mergeHistory(resourceHistory, [sample])
}

function appendOverviewHistory(name: string, sample: ResourceSample): void {
	if (Number.isNaN(Date.parse(sample.at))) return
	overviewHistories[name] = mergeHistory(overviewHistories[name] ?? [], [sample])
}

async function loadResourceHistory(key: string, name: string): Promise<void> {
	try {
		const samples = await fetchResourceHistory(key, name)
		if (selected !== name) return
		resourceHistory = mergeHistory(resourceHistory, samples)
	} catch (err) {
		if (selected !== name) return
		error = err instanceof Error ? err.message : "Could not load resource history."
	}
}

async function loadOverviewHistory(key: string, name: string): Promise<void> {
	if (overviewPending[name]) return
	overviewPending[name] = true
	try {
		const samples = await fetchResourceHistory(key, name)
		overviewHistories[name] = mergeHistory(overviewHistories[name] ?? [], samples)
	} catch (err) {
		if (selected === null) error = err instanceof Error ? err.message : "Could not load resource history."
	} finally {
		delete overviewPending[name]
	}
}

/** Fetch five-minute histories for every app missing one, so the overview stacks have depth on first paint. */
function ensureOverviewHistories(key: string): void {
	for (const app of apps)
		if ((overviewHistories[app.name] ?? []).length === 0) void loadOverviewHistory(key, app.name)

	// Drop histories for apps that no longer exist, so their colours free up.
	for (const name of Object.keys(overviewHistories))
		if (!apps.some(app => app.name === name)) delete overviewHistories[name]
}

function readSelection(): string | null {
	const name = new URLSearchParams(window.location.search).get("app")
	return name && name.length > 0 ? name : null
}

function clampSidebar(width: number): number {
	if (typeof window === "undefined") return width
	// Always leave room for the detail pane, so the sidebar cannot swallow the workspace.
	const upper = Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, window.innerWidth - 360))
	return Math.min(Math.max(width, MIN_SIDEBAR), upper)
}

function select(name: string | null): void {
	// Re-selecting the open app would reset its detail and reload it for nothing.
	if (name === selected) return
	selected = name
	detail = null
	logs = []
	logQuery = ""
	resourceHistory = []
	error = ""
	actionMessage = ""
	confirmRestart = false
	backupSettings = { enabled: true, every: "24h", keep: 7 }
	backupList = []
	backupCurrent = ""
	selectedBackup = null
	backupDatabase = false
	backupsLoading = false
	backupsError = ""
	backingUp = false
	importingBackups = false
	savingBackups = false
	restoringBackup = null
	confirmRestore = null
	resetDatabase()
	resetDeployments()
	closeLogs()
	const url = name ? `?app=${encodeURIComponent(name)}` : window.location.pathname
	window.history.replaceState(null, "", url)
	if (!token) return
	if (name) {
		void loadDetail(token, name)
		void loadResourceHistory(token, name)
		void loadBackups(token, name)
		if (tab === "database") void loadDatabase(token, name)
		if (tab === "deployments") void loadDeployments(token, name)
		openLogs(name)
	} else ensureOverviewHistories(token)
}

async function loadDetail(key: string, name: string): Promise<void> {
	try {
		detail = await fetchApp(key, name)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not load the app."
	}
}

/** Selecting another node disarms a revert confirmation from the previous one. */
function selectBackup(name: string): void {
	selectedBackup = name
	confirmRestore = null
}

function applyBackups(result: BackupsResult, selectName: string | null = null): void {
	backupSettings = result.settings
	backupList = result.backups
	backupDatabase = result.database
	backupCurrent = result.current
	const keepSelection = selectedBackup !== null && result.backups.some(backup => backup.name === selectedBackup)
	if (selectName && result.backups.some(backup => backup.name === selectName)) selectedBackup = selectName
	else if (!keepSelection) selectedBackup = result.current || result.backups[0]?.name || null
	confirmRestore = null
}

async function loadBackups(key: string, name: string): Promise<void> {
	backupsLoading = true
	backupsError = ""
	try {
		const result = await fetchBackups(key, name)
		if (selected !== name) return
		applyBackups(result)
	} catch (err) {
		if (selected !== name) return
		backupsError = err instanceof Error ? err.message : "Could not load backups."
	} finally {
		if (selected === name) backupsLoading = false
	}
}

async function backupNow(): Promise<void> {
	if (!token || !selected) return
	backingUp = true
	backupsError = ""
	actionMessage = ""
	try {
		const result = await createBackup(token, selected)
		applyBackups(result, result.created ?? null)
		actionMessage = result.created
			? `${selected} backed up.`
			: "No changes since the current snapshot; no backup was made."
	} catch (err) {
		backupsError = err instanceof Error ? err.message : "Could not back up the database."
	} finally {
		backingUp = false
	}
}

async function importBackupFile(file: File): Promise<void> {
	if (!token || !selected) return
	importingBackups = true
	backupsError = ""
	actionMessage = ""
	try {
		const result = await importBackup(token, selected, file)
		applyBackups(result, result.created ?? null)
		actionMessage = "Snapshot imported."
	} catch (err) {
		backupsError = err instanceof Error ? err.message : "Could not import the snapshot."
	} finally {
		importingBackups = false
	}
}

async function saveBackupConfig(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	if (!token || !selected) return
	savingBackups = true
	backupsError = ""
	try {
		const result = await saveBackupSettings(token, selected, {
			enabled: backupSettings.enabled,
			every: backupSettings.every.trim(),
			keep: backupSettings.keep,
		})
		applyBackups(result)
		actionMessage = "Backup settings saved."
	} catch (err) {
		backupsError = err instanceof Error ? err.message : "Could not save backup settings."
	} finally {
		savingBackups = false
	}
}

/** Reverting is destructive, so the first click only arms the row. */
async function revertBackup(name: string): Promise<void> {
	if (!token || !selected) return
	if (confirmRestore !== name) {
		confirmRestore = name
		return
	}
	const restored = backupList.find(backup => backup.name === name)
	confirmRestore = null
	restoringBackup = name
	backupsError = ""
	try {
		const result = await restoreBackup(token, selected, name)
		applyBackups(result, name)
		// The restored database has different contents, so the viewer must reload next time it opens.
		databaseLoadedFor = null
		actionMessage = restored
			? `${selected} reverted to the snapshot from ${formatTime(restored.createdAt)}.`
			: `${selected} reverted.`
	} catch (err) {
		backupsError = err instanceof Error ? err.message : "Could not restore the backup."
	} finally {
		restoringBackup = null
	}
}

function resetDatabase(): void {
	databaseExists = false
	databaseMessage = ""
	databaseImporting = false
	databaseMaintaining = false
	databaseVersion = 0
	databaseTables = []
	databaseTable = null
	databaseRows = null
	databaseLoadedFor = null
	databaseLoading = false
	databaseRowsLoading = false
	databaseError = ""
	databaseOffset = 0
	changes = []
	changesLoading = false
	changesError = ""
}

function openTab(next: Tab): void {
	tab = next
	if (next === "database" && token && selected && databaseLoadedFor !== selected) void loadDatabase(token, selected)
	if (next === "deployments" && token && selected && deploymentsLoadedFor !== selected)
		void loadDeployments(token, selected)
}

function resetDeployments(): void {
	deploymentList = []
	deploymentsLoading = false
	deploymentsError = ""
	deploymentsLoadedFor = null
	rollingBack = null
}

async function loadDeployments(key: string, name: string): Promise<void> {
	deploymentsLoading = true
	deploymentsError = ""
	try {
		const list = await fetchDeployments(key, name)
		if (selected !== name) return
		deploymentList = list
		deploymentsLoadedFor = name
	} catch (err) {
		if (selected === name) deploymentsError = err instanceof Error ? err.message : "Could not load deployments."
	} finally {
		if (selected === name) deploymentsLoading = false
	}
}

async function rollbackToDeployment(id: string): Promise<void> {
	if (!token || !selected) return
	const target = selected
	rollingBack = id
	deploymentsError = ""
	try {
		detail = await rollbackDeployment(token, target, id)
		if (selected !== target) return
		actionMessage = `${target} rolled back.`
		deploymentsLoadedFor = null
		await loadDeployments(token, target)
	} catch (err) {
		if (selected === target) deploymentsError = err instanceof Error ? err.message : "Could not roll back."
	} finally {
		if (selected === target) rollingBack = null
	}
}

async function importDatabaseFile(file: File): Promise<void> {
	if (!token || !selected) return
	databaseImporting = true
	databaseError = ""
	databaseMessage = ""
	try {
		await importDatabase(token, selected, file)
		await loadDatabase(token, selected)
		databaseMessage = "Database imported."
	} catch (err) {
		databaseError = err instanceof Error ? err.message : "Could not import the database."
	} finally {
		databaseImporting = false
	}
}

async function runDatabaseMaintenance(op: string): Promise<void> {
	if (!token || !selected) return
	databaseMaintaining = true
	databaseError = ""
	databaseMessage = ""
	try {
		const result = await maintainDatabase(token, selected, op)
		databaseMessage = result.result ? `${result.op}: ${result.result}` : `${result.op} finished.`
	} catch (err) {
		databaseError = err instanceof Error ? err.message : "Could not run database maintenance."
	} finally {
		databaseMaintaining = false
	}
}

async function loadDatabase(key: string, name: string): Promise<void> {
	databaseLoading = true
	databaseError = ""
	void loadChanges(key, name)
	try {
		const overview = await fetchDatabase(key, name)
		if (selected !== name) return
		databaseExists = overview.database
		databaseVersion = overview.version
		databaseTables = overview.tables
		databaseLoadedFor = name
		databaseOffset = 0
		if (!overview.database || overview.tables.length === 0) {
			databaseTable = null
			databaseRows = null
			return
		}
		const nextTable = databaseTable && overview.tables.includes(databaseTable) ? databaseTable : overview.tables[0]
		databaseTable = nextTable ?? null
		if (nextTable) await loadDatabaseRows(key, name, nextTable, 0)
	} catch (err) {
		if (selected === name) databaseError = err instanceof Error ? err.message : "Could not load the database."
	} finally {
		if (selected === name) databaseLoading = false
	}
}

async function loadDatabaseRows(key: string, name: string, table: string, offset: number): Promise<void> {
	databaseRowsLoading = true
	databaseError = ""
	try {
		const result = await fetchDatabaseRows(key, name, table, databaseLimit, offset)
		if (selected !== name || databaseTable !== table) return
		databaseRows = result
		databaseOffset = result.offset
	} catch (err) {
		if (selected === name && databaseTable === table)
			databaseError = err instanceof Error ? err.message : "Could not load the table."
	} finally {
		if (selected === name && databaseTable === table) databaseRowsLoading = false
	}
}

async function loadChanges(key: string, name: string): Promise<void> {
	changesLoading = true
	changesError = ""
	try {
		const { changes: nextChanges } = await fetchChanges(key, name)
		if (selected !== name) return
		changes = nextChanges
	} catch (err) {
		if (selected === name) changesError = err instanceof Error ? err.message : "Could not load changes."
	} finally {
		if (selected === name) changesLoading = false
	}
}

function selectDatabaseTable(event: Event): void {
	const { value } = event.currentTarget as HTMLSelectElement
	if (!token || !selected || !value || value === databaseTable) return
	databaseTable = value
	databaseOffset = 0
	databaseRows = null
	void loadDatabaseRows(token, selected, value, 0)
}

function changeDatabaseLimit(event: Event): void {
	const limit = Number((event.currentTarget as HTMLSelectElement).value)
	if (!Number.isFinite(limit) || limit === databaseLimit) return
	databaseLimit = limit
	databaseOffset = 0
	if (token && selected && databaseTable) {
		databaseRows = null
		void loadDatabaseRows(token, selected, databaseTable, 0)
	}
}

function databasePage(direction: -1 | 1): void {
	if (!token || !selected || !databaseTable || !databaseRows || databaseRowsLoading) return
	const offset =
		direction < 0 ? Math.max(0, databaseOffset - databaseLimit) : databaseRows.offset + databaseRows.rows.length
	if (offset === databaseOffset) return
	void loadDatabaseRows(token, selected, databaseTable, offset)
}

function reloadDatabase(): void {
	if (!token || !selected) return
	databaseLoadedFor = null
	void loadDatabase(token, selected)
}

/** Stream one app's log: the recent backlog, then every new line as it is written. */
function openLogs(name: string): void {
	if (!logsOpen) return
	closeLogs()
	logs = []
	followLogs = true
	stopLogs = subscribeLogs(
		name,
		tail,
		backlog => {
			logs = backlog
		},
		entry => {
			const next = [...logs, entry]
			logs = next.length > MAX_LOG_LINES ? next.slice(-MAX_LOG_LINES) : next
		},
	)
}

function closeLogs(): void {
	if (stopLogs) {
		stopLogs()
		stopLogs = null
	}
}

function toggleLogs(): void {
	logsOpen = !logsOpen
	if (logsOpen) {
		if (token && selected) openLogs(selected)
	} else closeLogs()
}

/** Reopening the stream picks up a new backlog for the selected line count. */
function reloadLogs(lines: number): void {
	tail = lines
	if (!token || !selected) return
	openLogs(selected)
}

/** Follow new lines only while the view is scrolled to the bottom. */
function onLogsScroll(): void {
	if (!logPane) return
	followLogs = logPane.scrollHeight - logPane.scrollTop - logPane.clientHeight < FOLLOW_THRESHOLD
}

async function refresh(key: string): Promise<void> {
	loading = true
	error = ""
	try {
		apps = await fetchApps(key)
		if (selected) {
			detail = await fetchApp(key, selected)
			void loadBackups(key, selected)
			if (tab === "database") void loadDatabase(key, selected)
			if (tab === "deployments") void loadDeployments(key, selected)
		} else ensureOverviewHistories(key)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the supervisor."
	} finally {
		loading = false
	}
}

function refreshList(): void {
	if (!token) return
	void refresh(token)
}

function connect(key: string): void {
	disconnect()
	stopStream = subscribe(
		updated => {
			apps = updated
			for (const app of updated)
				appendOverviewHistory(app.name, {
					at: app.sampledAt,
					cpuPercent: app.cpuPercent,
					memoryBytes: app.memoryBytes,
				})

			for (const name of Object.keys(overviewHistories))
				if (!updated.some(app => app.name === name)) delete overviewHistories[name]

			if (selected) {
				const current = updated.find(app => app.name === selected)
				if (current) {
					detail = current
					appendHistory({
						at: current.sampledAt,
						cpuPercent: current.cpuPercent,
						memoryBytes: current.memoryBytes,
					})
				}
			} else ensureOverviewHistories(key)

			loading = false
			error = ""
		},
		value => {
			live = value
		},
	)
}

function disconnect(): void {
	if (stopStream) {
		stopStream()
		stopStream = null
	}
	closeLogs()
	live = false
}

/** Drag the separator, tracking on the window so the drag survives a fast pointer. */
function clampLogsHeight(height: number): number {
	if (typeof window === "undefined") return height
	// Leave room for the detail pane above the log, however tall the window is.
	const upper = Math.min(MAX_LOGS, Math.max(MIN_LOGS, window.innerHeight - 260))
	return Math.min(Math.max(height, MIN_LOGS), upper)
}

/** Run a window-level pointer drag, reporting the total movement from the start. */
function trackPointerDrag(event: PointerEvent, cursor: string, onMove: (dx: number, dy: number) => void): void {
	event.preventDefault()
	const startX = event.clientX
	const startY = event.clientY
	const previousCursor = document.body.style.cursor
	const previousSelect = document.body.style.userSelect
	document.body.style.cursor = cursor
	document.body.style.userSelect = "none"

	const handleMove = (move: PointerEvent): void => {
		onMove(move.clientX - startX, move.clientY - startY)
	}
	const stop = (): void => {
		window.removeEventListener("pointermove", handleMove)
		window.removeEventListener("pointerup", stop)
		window.removeEventListener("pointercancel", stop)
		document.body.style.cursor = previousCursor
		document.body.style.userSelect = previousSelect
	}
	window.addEventListener("pointermove", handleMove)
	window.addEventListener("pointerup", stop)
	window.addEventListener("pointercancel", stop)
}

function startSidebarResize(event: PointerEvent): void {
	const start = sidebarWidth
	trackPointerDrag(event, "col-resize", dx => {
		sidebarWidth = clampSidebar(start + dx)
	})
}

/** Drag the log pane's top border: dragging up grows the pane. */
function startLogsResize(event: PointerEvent): void {
	const start = logsHeight
	trackPointerDrag(event, "row-resize", (_dx, dy) => {
		logsHeight = clampLogsHeight(start - dy)
	})
}

function onSeparatorKeydown(event: KeyboardEvent): void {
	if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return
	event.preventDefault()
	const step = event.shiftKey ? KEY_STEP * 3 : KEY_STEP
	sidebarWidth = clampSidebar(sidebarWidth + (event.key === "ArrowLeft" ? -step : step))
}

function onLogsSeparatorKeydown(event: KeyboardEvent): void {
	if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return
	event.preventDefault()
	const step = event.shiftKey ? KEY_STEP * 3 : KEY_STEP
	logsHeight = clampLogsHeight(logsHeight + (event.key === "ArrowUp" ? step : -step))
}

/** Move between tabs with the arrow keys (and Home/End), as expected of an ARIA tablist. Both axes work because the tablist is vertical on desktop and can wrap on narrow screens. */
function onTabKeydown(event: KeyboardEvent): void {
	let next: number | null = null
	const index = tabs.indexOf(tab)
	if (event.key === "ArrowDown" || event.key === "ArrowRight") next = (index + 1) % tabs.length
	else if (event.key === "ArrowUp" || event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length
	else if (event.key === "Home") next = 0
	else if (event.key === "End") next = tabs.length - 1
	if (next === null) return
	event.preventDefault()
	const nextTab = tabs[next]
	if (nextTab) openTab(nextTab)
	tablistEl?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus()
}

/** The grip tracks the pointer along the separator, hinting where the drag will grab. */
function moveGrip(event: PointerEvent): void {
	const bounds = (event.currentTarget as HTMLElement).getBoundingClientRect()
	gripY = event.clientY - bounds.top
}

function moveLogsGrip(event: PointerEvent): void {
	const bounds = (event.currentTarget as HTMLElement).getBoundingClientRect()
	logsGripX = event.clientX - bounds.left
}

async function submitToken(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	const value = tokenInput.trim()
	if (!value) return
	try {
		await fetchApps(value)
		setToken(value)
		token = value
		tokenInput = ""
		error = ""
		connect(value)
		await refresh(value)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the supervisor."
	}
}

function forget(event: MouseEvent): void {
	event.preventDefault()
	forgetToken()
	disconnect()
	token = null
	apps = []
	select(null)
}

async function restart(event: MouseEvent): Promise<void> {
	event.preventDefault()
	if (!token || !selected) return
	if (!confirmRestart) {
		confirmRestart = true
		return
	}
	confirmRestart = false
	restarting = true
	actionMessage = ""
	try {
		detail = await restartApp(token, selected)
		actionMessage = `${selected} restarted.`
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not restart the app."
	} finally {
		restarting = false
	}
}

async function upload(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	if (!token || !selected) return
	const input = document.querySelector("#bundle")
	if (!(input instanceof HTMLInputElement)) {
		error = "Choose a bundle file first."
		return
	}
	const file = input.files?.[0]
	if (!file) {
		error = "Choose a bundle file first."
		return
	}
	deploying = true
	actionMessage = ""
	try {
		detail = await deployBundle(token, selected, file)
		apps = await fetchApps(token)
		actionMessage = `${selected} deployed.`
		deploymentsLoadedFor = null
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not deploy the bundle."
	} finally {
		deploying = false
	}
}

/** Lines at or above the selected severity and matching the text filter, in stream order. */
const filteredLogs = $derived.by(() => {
	const needle = logQuery.trim().toLowerCase()
	return logs.filter(
		entry =>
			levelIndex(entry.level) >= levelIndex(minLevel)
			&& (needle === "" || entry.message.toLowerCase().includes(needle)),
	)
})

/** One stacked series per known app, so the overview charts share colours and depth. */
const overviewSeries = $derived(
	apps
		.toSorted((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
		.map(app => ({
			name: app.name,
			samples: overviewHistories[app.name] ?? [],
		})),
)

/** One legend for both overview charts: colours are assigned by sorted app name. */
const legendApps = $derived(
	overviewSeries.map((entry, index) => {
		const last = entry.samples.at(-1)
		return {
			color: palette[index % palette.length],
			cpu: last?.cpuPercent ?? 0,
			memory: last?.memoryBytes ?? 0,
			name: entry.name,
		}
	}),
)

let detailSpan = $state(300)
let overviewSpan = $state(300)

function setMinLevel(level: LogLevel): void {
	minLevel = level
}

/** Keep the newest lines in view, unless the reader scrolled away from the bottom. */
$effect(() => {
	// Reading the length subscribes the effect to new lines, a fresh backlog and the filter.
	const count = filteredLogs.length
	if (count === 0 || !logPane || !followLogs) return
	logPane.scrollTop = logPane.scrollHeight
})

onMount((): (() => void) => {
	const saved = getToken()
	const name = readSelection()
	if (saved) {
		token = saved
		selected = name
		connect(saved)
		void refresh(saved)
		if (name) {
			void loadResourceHistory(saved, name)
			void loadBackups(saved, name)
			openLogs(name)
		}
	} else {
		loading = false
		selected = name
	}
	const clock = setInterval((): void => {
		now = Date.now()
	}, 1000)
	return (): void => {
		clearInterval(clock)
		disconnect()
	}
})
</script>

<svelte:head>
	<title>wt admin</title>
</svelte:head>

<div class={token ? "flex h-dvh flex-col overflow-hidden" : "flex min-h-dvh flex-col"}>
	{#if !token}
		<main class="flex flex-1 items-center justify-center p-4">
			<form
				class="relative w-full max-w-sm border border-line bg-surface p-8 text-center shadow-2xl shadow-black"
				onsubmit={submitToken}
			>
				<div
					aria-hidden="true"
					class="absolute inset-x-0 top-0 h-0.5 bg-linear-to-r from-emerald-500 via-primary-400 to-green-500"
				></div>
				<h2 class="text-xl font-semibold tracking-tight">wt admin</h2>
				<small class="block pt-1 text-xs tracking-wide text-neutral-400 uppercase">
					Sign in with your deploy token
				</small>
				<label class="flex flex-col gap-1.5 pt-6 text-left">
					<span class="text-xs tracking-wide text-neutral-400 uppercase">Token</span>
					<input
						autocomplete="off"
						class="field"
						placeholder="deploy token"
						type="password"
						bind:value={tokenInput}
					>
				</label>
				<button class="btn btn-primary mt-4 w-full" type="submit">Connect</button>
				{#if error}
					<p class="mt-4 border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
						{error}
					</p>
				{/if}
				<p class="pt-6 text-xs text-neutral-500">The token stays in this browser only.</p>
			</form>
		</main>
	{:else}
		<div
			class="workspace"
			style="--sidebar-width: {sidebarWidth}px; --logs-height: {selected && logsOpen ? logsHeight : 0}px"
		>
			<AppSidebar
				{apps}
				{live}
				{loading}
				{now}
				onforget={forget}
				onrefresh={refreshList}
				onselect={select}
				ontabkeydown={onTabKeydown}
				opentab={openTab}
				{selected}
				{tab}
				bind:tablist={tablistEl}
			/>

			<!-- A focusable separator is a window splitter, adjustable by pointer and by arrow keys. -->
			<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
			<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
			<!-- biome-ignore lint/a11y/useSemanticElements: A window splitter is not an <hr>. -->
			<div
				aria-label="Resize app list"
				aria-orientation="vertical"
				aria-valuemax={MAX_SIDEBAR}
				aria-valuemin={MIN_SIDEBAR}
				aria-valuenow={Math.round(sidebarWidth)}
				class="resize-handle resize-handle-sidebar hover:bg-primary-500/10"
				onkeydown={onSeparatorKeydown}
				onpointerdown={startSidebarResize}
				onpointermove={moveGrip}
				role="separator"
				tabindex="0"
			>
				<span
					aria-hidden="true"
					class="resize-grip resize-grip-vertical"
					style:top={gripY === null ? "50%" : `${gripY}px`}
				></span>
			</div>

			<main class="flex min-w-0 flex-1 flex-col bg-black">
				{#if selected}
					<header class="flex shrink-0 items-center gap-2 border-b border-line bg-surface px-3 py-2">
						<h2 class="min-w-0 truncate text-sm font-semibold tracking-tight">{selected}</h2>
						<span class="flex-1"></span>
						<button
							aria-controls="logs-panel"
							aria-expanded={logsOpen}
							class="btn"
							onclick={toggleLogs}
							type="button"
						>
							{logsOpen ? "Hide logs" : "Show logs"}
						</button>
						<button class="btn" onclick={(): void => select(null)} type="button">Close</button>
						<button
							class={confirmRestart ? "btn btn-danger" : "btn"}
							disabled={restarting}
							onclick={restart}
							type="button"
						>
							{restarting ? "Restarting…" : confirmRestart ? "Click again to confirm" : "Restart"}
						</button>
					</header>

					{#if tab === "overview"}
						<OverviewTab
							{actionMessage}
							{deploying}
							{detail}
							{error}
							history={resourceHistory}
							onupload={upload}
							bind:span={detailSpan}
						/>
					{:else if tab === "backups"}
						<BackupsTab
							{actionMessage}
							{backingUp}
							backups={backupList}
							confirming={confirmRestore}
							current={backupCurrent}
							database={backupDatabase}
							error={backupsError || error}
							importing={importingBackups}
							loading={backupsLoading}
							name={selected}
							onbackup={backupNow}
							onimport={importBackupFile}
							onrevert={revertBackup}
							onsave={saveBackupConfig}
							onselect={selectBackup}
							restoring={restoringBackup}
							saving={savingBackups}
							selected={selectedBackup}
							bind:enabled={backupSettings.enabled}
							bind:every={backupSettings.every}
							bind:keep={backupSettings.keep}
						/>
					{:else if tab === "database"}
						<DatabaseTab
							{changes}
							{changesError}
							{changesLoading}
							error={databaseError || error}
							exists={databaseExists}
							importing={databaseImporting}
							limit={databaseLimit}
							loading={databaseLoading}
							maintaining={databaseMaintaining}
							message={databaseMessage}
							name={selected}
							offset={databaseOffset}
							onchangelimit={changeDatabaseLimit}
							onimport={importDatabaseFile}
							onmaintenance={runDatabaseMaintenance}
							onpage={databasePage}
							onreload={reloadDatabase}
							onselecttable={selectDatabaseTable}
							rows={databaseRows}
							rowsLoading={databaseRowsLoading}
							table={databaseTable}
							tables={databaseTables}
							version={databaseVersion}
						/>
					{:else}
						<DeploymentsTab
							{actionMessage}
							deployments={deploymentList}
							error={deploymentsError}
							loading={deploymentsLoading}
							onrollback={rollbackToDeployment}
							{rollingBack}
						/>
					{/if}
				{:else if error}
					<div class="p-4">
						<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
							{error}
						</p>
					</div>
				{:else if apps.length === 0}
					<div class="flex flex-1 items-center justify-center p-4">
						<p class="text-sm text-neutral-500">
							{loading ? "Loading apps…" : "No apps deployed."}
						</p>
					</div>
				{:else}
					<AllAppsOverview legend={legendApps} series={overviewSeries} bind:span={overviewSpan} />
				{/if}
			</main>

			{#if selected && logsOpen}
				<!-- A focusable separator is a window splitter, adjustable by pointer and by arrow keys. -->
				<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
				<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
				<!-- biome-ignore lint/a11y/useSemanticElements: A window splitter is not an <hr>. -->
				<div
					aria-label="Resize logs"
					aria-orientation="horizontal"
					aria-valuemax={MAX_LOGS}
					aria-valuemin={MIN_LOGS}
					aria-valuenow={Math.round(logsHeight)}
					class="resize-handle resize-handle-logs hover:bg-primary-500/10"
					onkeydown={onLogsSeparatorKeydown}
					onpointerdown={startLogsResize}
					onpointermove={moveLogsGrip}
					role="separator"
					tabindex="0"
				>
					<span
						aria-hidden="true"
						class="resize-grip resize-grip-horizontal"
						style:left={logsGripX === null ? "50%" : `${logsGripX}px`}
					></span>
				</div>

				<LogsPane
					entries={filteredLogs}
					{minLevel}
					onminlevel={setMinLevel}
					onreload={reloadLogs}
					onscroll={onLogsScroll}
					{tail}
					total={logs.length}
					bind:pane={logPane}
					bind:query={logQuery}
				/>
			{/if}
		</div>
	{/if}
</div>

<style>
.workspace {
	position: relative;
	display: grid;
	flex: 1;
	grid-template-areas:
		"sidebar main"
		"sidebar logs";
	grid-template-rows: minmax(0, 1fr) var(--logs-height);
	grid-template-columns: var(--sidebar-width) minmax(0, 1fr);
	min-height: 0;
	overflow: hidden;
}

:global(.workspace > aside) {
	grid-area: sidebar;
	width: var(--sidebar-width);
}

.workspace > main {
	grid-area: main;
	min-width: 0;
}

:global(.workspace > .logs-pane) {
	grid-area: logs;
}

/* The separators ride on the pane boundaries, so their grips sit exactly on the dividers. */
.resize-handle {
	position: absolute;
	z-index: 30;
	touch-action: none;
}

.resize-handle::before {
	position: absolute;
	content: "";
	background: var(--color-line);
}

.resize-handle-sidebar {
	top: 0;
	bottom: 0;
	left: var(--sidebar-width);
	width: 9px;
	translate: -50% 0;
}

.resize-handle-sidebar::before {
	top: 0;
	bottom: 0;
	left: 50%;
	width: 1px;
	translate: -50% 0;
}

.resize-handle-logs {
	right: 0;
	bottom: var(--logs-height);
	left: var(--sidebar-width);
	height: 9px;
	translate: 0 50%;
}

.resize-handle-logs::before {
	top: 50%;
	right: 0;
	left: 0;
	height: 1px;
	translate: 0 -50%;
}

.resize-grip {
	position: absolute;
	top: 50%;
	left: 50%;
	background: var(--color-neutral-600);
	opacity: 0.35;
	translate: -50% -50%;
	transition:
		opacity 120ms ease,
		background-color 120ms ease;
}

.resize-grip-vertical {
	width: 2px;
	height: 2.5rem;
}

.resize-grip-horizontal {
	width: 2.5rem;
	height: 2px;
}

.resize-handle:hover .resize-grip,
.resize-handle:focus-visible .resize-grip,
.resize-handle:active .resize-grip {
	background: var(--color-primary-500);
	opacity: 1;
}

.resize-handle-sidebar:hover,
.resize-handle-sidebar:active {
	cursor: col-resize;
}

.resize-handle-logs:hover,
.resize-handle-logs:active {
	cursor: row-resize;
}

/* Below the split point the panes stack, so the side separator has nothing to adjust. */
@media (max-width: 700px) {
	.workspace {
		display: flex;
		flex-direction: column;
	}

	:global(.workspace > aside) {
		width: auto;
		border-bottom: 1px solid var(--color-line);
	}

	:global(.workspace > .logs-pane) {
		height: var(--logs-height);
	}

	.resize-handle-sidebar {
		display: none;
	}
}
</style>
