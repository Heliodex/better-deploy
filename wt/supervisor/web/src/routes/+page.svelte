<script lang="ts">
import { onMount } from "svelte"
import AllAppsOverview from "../lib/AllAppsOverview.svelte"
import {
	type AppInfo,
	type BackupInfo,
	type BackupSettings,
	type BackupsResult,
	createBackup,
	type DatabaseRows,
	deployBundle,
	fetchApp,
	fetchApps,
	fetchBackups,
	fetchChanges,
	fetchDatabase,
	fetchDatabaseRows,
	fetchResourceHistory,
	forgetToken,
	getToken,
	type LogEntry,
	type LogLevel,
	logLevels,
	type ResourceSample,
	restartApp,
	restoreBackup,
	type StorageChange,
	saveBackupSettings,
	setToken,
	subscribe,
	subscribeLogs,
} from "../lib/api"
import BackupsTab from "../lib/BackupsTab.svelte"
import { palette } from "../lib/chart"
import { formatChangeParams, formatDatabaseValue, formatRowValue, formatTime, rowOpClass, uptime } from "../lib/format"
import { accessStatusClass, levelClasses, levelMessageClasses, logTimeParts, parseAccessLine } from "../lib/log-format"
import OverviewTab from "../lib/OverviewTab.svelte"

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

const tabs = ["overview", "backups", "database"] as const
type Tab = (typeof tabs)[number]

let token = $state<string | null>(null)
let tokenInput = $state("")
let apps = $state<AppInfo[]>([])
let selected = $state<string | null>(null)
let tab = $state<Tab>("overview")
let tablistEl = $state<HTMLDivElement | null>(null)
let detail = $state<AppInfo | null>(null)
let logs = $state<LogEntry[]>([])
let logsOpen = $state(true)
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
let savingBackups = $state(false)
let restoringBackup = $state<string | null>(null)
let confirmRestore = $state<string | null>(null)
let databaseExists = $state(false)
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
	savingBackups = false
	restoringBackup = null
	confirmRestore = null
	resetDatabase()
	closeLogs()
	const url = name ? `?app=${encodeURIComponent(name)}` : window.location.pathname
	window.history.replaceState(null, "", url)
	if (!token) return
	if (name) {
		void loadDetail(token, name)
		void loadResourceHistory(token, name)
		void loadBackups(token, name)
		if (tab === "database") void loadDatabase(token, name)
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
}

async function loadDatabase(key: string, name: string): Promise<void> {
	databaseLoading = true
	databaseError = ""
	void loadChanges(key, name)
	try {
		const overview = await fetchDatabase(key, name)
		if (selected !== name) return
		databaseExists = overview.database
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
function reloadLogs(): void {
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
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not deploy the bundle."
	} finally {
		deploying = false
	}
}

/** Lines at or above the selected severity, in the same order as the stream. */
const filteredLogs = $derived(logs.filter(entry => logLevels.indexOf(entry.level) >= logLevels.indexOf(minLevel)))

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

function setMinLevel(event: Event): void {
	const { value } = event.currentTarget as HTMLSelectElement
	const level = logLevels.find(candidate => candidate === value)
	if (level) minLevel = level
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
			<aside aria-label="Apps" class="flex min-h-0 shrink-0 flex-col bg-surface">
				<div class="flex shrink-0 items-center gap-2 border-b border-line px-3 py-2">
					<span class="text-sm font-semibold tracking-tight">wt admin</span>
					<span class="flex-1"></span>
					<span class="flex items-center gap-1.5 text-xs text-neutral-400">
						<span
							aria-hidden="true"
							class={live ? "size-1.5 bg-primary-500" : "size-1.5 border border-neutral-500"}
						></span>
						{live ? "live" : "connecting…"}
					</span>
				</div>
				<header class="flex shrink-0 items-center justify-between gap-2 border-b border-line-soft px-3 py-2">
					<h2 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">Apps</h2>
					<button class="btn" disabled={loading} onclick={refreshList} type="button">
						{loading ? "Refreshing…" : "Refresh"}
					</button>
				</header>
				<div class="flex min-h-0 flex-1 flex-col overflow-y-auto">
					{#if apps.length === 0}
						<p class="p-3 text-sm text-neutral-500">
							{loading ? "Loading apps…" : "No apps deployed."}
						</p>
					{:else}
						<ul class="flex flex-col py-1">
							{#each apps as app (app.name)}
								<li>
									<button
										aria-current={selected === app.name}
										class={[
											"flex w-full items-center gap-2 border-b border-l-2 border-line-soft px-3 py-1.5 text-left text-sm transition-colors",
											selected === app.name
												? "cursor-default border-l-primary-500 bg-neutral-800/40"
												: "border-l-transparent hover:bg-neutral-800/40",
										]}
										disabled={selected === app.name}
										onclick={(): void => select(app.name)}
										type="button"
									>
										<span
											aria-hidden="true"
											class={app.status === "running"
												? "size-1.5 shrink-0 bg-primary-500"
												: "size-1.5 shrink-0 border border-neutral-500"}
										></span>
										<span class="flex-1 truncate">{app.name}</span>
										<span class="text-xs text-neutral-500 tabular-nums"
											>{uptime(app.startedAt, now)}</span
										>
									</button>
								</li>
							{/each}
						</ul>
					{/if}
				</div>
				{#if selected}
					<div
						aria-label="{selected} sections"
						aria-orientation="vertical"
						class="flex shrink-0 flex-col border-t border-line"
						role="tablist"
						bind:this={tablistEl}
					>
						<h2 class="px-3 py-2 text-xs font-semibold tracking-wide text-neutral-400 uppercase">
							Sections
						</h2>
						<button
							aria-controls="app-panel-overview"
							aria-selected={tab === "overview"}
							class={[
								"flex w-full items-center border-b border-l-2 border-line-soft px-3 py-1.5 text-left text-sm transition-colors last:border-b-0",
								tab === "overview"
									? "cursor-default border-l-primary-500 bg-neutral-800/40 text-primary-300"
									: "border-l-transparent text-neutral-400 hover:bg-neutral-800/40 hover:text-neutral-200",
							]}
							id="app-tab-overview"
							onclick={() => openTab("overview")}
							onkeydown={onTabKeydown}
							role="tab"
							tabindex={tab === "overview" ? 0 : -1}
							type="button"
						>
							Overview
						</button>
						<button
							aria-controls="app-panel-backups"
							aria-selected={tab === "backups"}
							class={[
								"flex w-full items-center border-b border-l-2 border-line-soft px-3 py-1.5 text-left text-sm transition-colors last:border-b-0",
								tab === "backups"
									? "cursor-default border-l-primary-500 bg-neutral-800/40 text-primary-300"
									: "border-l-transparent text-neutral-400 hover:bg-neutral-800/40 hover:text-neutral-200",
							]}
							id="app-tab-backups"
							onclick={() => openTab("backups")}
							onkeydown={onTabKeydown}
							role="tab"
							tabindex={tab === "backups" ? 0 : -1}
							type="button"
						>
							Backups
						</button>
						<button
							aria-controls="app-panel-database"
							aria-selected={tab === "database"}
							class={[
								"flex w-full items-center border-b border-l-2 border-line-soft px-3 py-1.5 text-left text-sm transition-colors last:border-b-0",
								tab === "database"
									? "cursor-default border-l-primary-500 bg-neutral-800/40 text-primary-300"
									: "border-l-transparent text-neutral-400 hover:bg-neutral-800/40 hover:text-neutral-200",
							]}
							id="app-tab-database"
							onclick={() => openTab("database")}
							onkeydown={onTabKeydown}
							role="tab"
							tabindex={tab === "database" ? 0 : -1}
							type="button"
						>
							Database
						</button>
					</div>
				{/if}
				<footer class="shrink-0 border-t border-line px-3 py-2">
					<button class="btn w-full" onclick={forget} type="button">Forget token</button>
				</footer>
			</aside>

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
							{error}
							loading={backupsLoading}
							onbackup={backupNow}
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
					{:else}
						<div
							aria-labelledby="app-tab-database"
							class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
							id="app-panel-database"
							role="tabpanel"
						>
							<div class="flex flex-wrap items-center justify-between gap-2">
								<div class="flex items-center gap-2">
									<h3
										class="text-xs font-semibold tracking-wide text-neutral-400 uppercase"
										id="database-title"
									>
										Database
									</h3>
									<span class="text-xs text-neutral-500">
										{databaseTables.length}
										{databaseTables.length === 1 ? "table" : "tables"}
									</span>
								</div>
								<button class="btn" disabled={databaseLoading} onclick={reloadDatabase} type="button">
									{databaseLoading ? "Refreshing…" : "Refresh"}
								</button>
							</div>

							{#if databaseError}
								<p
									class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400"
									role="alert"
								>
									{databaseError}
								</p>
							{/if}

							{#if databaseLoading}
								<p class="text-sm text-neutral-500">Loading database…</p>
							{:else if !databaseExists}
								<p class="text-sm text-neutral-500">
									No database yet. One is created the first time the app uses storage.
								</p>
							{:else if databaseTables.length === 0}
								<p class="text-sm text-neutral-500">The database has no tables yet.</p>
							{:else}
								<div class="flex flex-wrap items-end gap-3">
									<label class="flex flex-col gap-1">
										<span class="text-[10px] tracking-wide text-neutral-500 uppercase">
											Table
										</span>
										<select
											aria-label="Table"
											class="field"
											onchange={selectDatabaseTable}
											value={databaseTable ?? ""}
										>
											{#each databaseTables as table (table)}
												<option value={table}>{table}</option>
											{/each}
										</select>
									</label>
									<label class="flex flex-col gap-1">
										<span class="text-[10px] tracking-wide text-neutral-500 uppercase"> Rows </span>
										<select
											aria-label="Rows per page"
											class="field"
											onchange={changeDatabaseLimit}
											value={databaseLimit}
										>
											{#each [25, 50, 100] as limit (limit)}
												<option value={limit}>{limit}</option>
											{/each}
										</select>
									</label>
									<span class="pb-1.5 text-xs text-neutral-500 tabular-nums">
										{#if databaseRows && databaseRows.rows.length > 0}
											{databaseRows.offset + 1}-{databaseRows.offset + databaseRows.rows.length}
											of {databaseRows.total}
										{:else if databaseRows}
											0 of {databaseRows.total}
										{:else}
											…
										{/if}
									</span>
									<span class="flex-1"></span>
									<button
										class="btn"
										disabled={databaseRowsLoading || databaseOffset === 0}
										onclick={(): void => databasePage(-1)}
										type="button"
									>
										Previous
									</button>
									<button
										class="btn"
										disabled={databaseRowsLoading
											|| !databaseRows?.truncated
											|| (databaseRows?.rows.length ?? 0) === 0}
										onclick={(): void => databasePage(1)}
										type="button"
									>
										Next
									</button>
								</div>

								{#if databaseRowsLoading}
									<p class="text-sm text-neutral-500">Loading rows…</p>
								{:else if databaseRows}
									<div class="overflow-x-auto border border-line-soft">
										<table class="w-full border-collapse text-left text-xs">
											<thead
												class="bg-surface-raised text-[10px] tracking-wide text-neutral-500 uppercase"
											>
												<tr>
													{#each databaseRows.columns as column (column)}
														<th class="px-2 py-1 font-normal whitespace-nowrap">
															{column}
														</th>
													{/each}
												</tr>
											</thead>
											<tbody>
												{#if databaseRows.rows.length === 0}
													<tr>
														<td
															class="px-2 py-2 text-neutral-500"
															colspan={Math.max(databaseRows.columns.length, 1)}
														>
															No rows.
														</td>
													</tr>
												{:else}
													{#each databaseRows.rows as row, rowIndex (rowIndex)}
														<tr class="border-t border-line-soft">
															{#each row as cell, cellIndex (cellIndex)}
																{const text = formatDatabaseValue(cell)}
																<td
																	class="max-w-80 truncate px-2 py-1 font-mono text-neutral-300"
																	title={text}
																>
																	{text}
																</td>
															{/each}
														</tr>
													{/each}
												{/if}
											</tbody>
										</table>
									</div>
								{/if}
							{/if}

							<section
								aria-labelledby="changes-title"
								class="flex flex-col gap-2 border border-line-soft bg-surface p-2"
							>
								<div class="flex items-center justify-between gap-2">
									<h3
										class="text-xs font-semibold tracking-wide text-neutral-400 uppercase"
										id="changes-title"
									>
										Change log
									</h3>
									<span class="text-xs text-neutral-500">{changes.length} recent</span>
								</div>

								{#if changesError}
									<p
										class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400"
										role="alert"
									>
										{changesError}
									</p>
								{/if}

								{#if changesLoading}
									<p class="text-sm text-neutral-500">Loading changes…</p>
								{:else if changes.length === 0}
									<p class="text-sm text-neutral-500">No changes logged yet.</p>
								{:else}
									<ul class="flex flex-col">
										{#each changes as change, index (index)}
											<li
												class="flex flex-col gap-1 border-t border-line-soft py-2 text-xs first:border-t-0 first:pt-0"
											>
												<div
													class="flex flex-wrap items-center justify-between gap-2 text-neutral-500"
												>
													<time datetime={change.time}>{formatTime(change.time)}</time>
													<span class="tabular-nums">
														{change.rowsAffected}
														{change.rowsAffected === 1 ? "row" : "rows"}
													</span>
												</div>
												<code class="whitespace-pre-wrap wrap-break-word text-neutral-200">
													{change.sql}
												</code>
												{#if change.params.length > 0}
													<code class="whitespace-pre-wrap wrap-break-word text-neutral-500">
														{formatChangeParams(change.params)}
													</code>
												{/if}
												{#if change.rows && change.rows.length > 0}
													<div class="flex flex-col gap-1.5 pt-0.5">
														{#each change.rows as row, rowIndex (rowIndex)}
															<div class="border border-line-soft bg-black/30 p-1.5">
																<div
																	class="flex flex-wrap items-center gap-2 pb-1 text-[10px] tracking-wide uppercase"
																>
																	<span class={rowOpClass(row.op)}>{row.op}</span>
																	<span class="normal-case text-neutral-400">
																		{row.table}
																	</span>
																	{#if row.rowId !== undefined}
																		<span
																			class="normal-case text-neutral-500 tabular-nums"
																		>
																			row {row.rowId}
																		</span>
																	{/if}
																</div>
																<dl
																	class="grid grid-cols-1 gap-x-3 gap-y-1 text-xs sm:grid-cols-2"
																>
																	{#each row.columns as column, columnIndex (column)}
																		<div
																			class="flex min-w-0 items-baseline gap-1.5"
																		>
																			<dt class="shrink-0 text-neutral-500">
																				{column}
																			</dt>
																			<dd
																				class="min-w-0 wrap-break-word text-neutral-300"
																			>
																				{#if row.op === "update"}
																					<span class="text-neutral-500">
																						{formatRowValue(
																							row.old,
																							columnIndex,
																						)}
																					</span>
																					<span class="text-neutral-600">
																						→
																					</span>
																				{/if}
																				{#if row.op === "delete"}
																					<span>
																						{formatRowValue(
																							row.old,
																							columnIndex,
																						)}
																					</span>
																				{:else}
																					<span>
																						{formatRowValue(
																							row.new,
																							columnIndex,
																						)}
																					</span>
																				{/if}
																			</dd>
																		</div>
																	{/each}
																</dl>
															</div>
														{/each}
														{#if change.rowsTruncated}
															<p class="text-[10px] text-neutral-500">
																Some row details were omitted.
															</p>
														{/if}
													</div>
												{:else if change.rowsTruncated}
													<p class="text-[10px] text-neutral-500">
														Row details were omitted because the change was too large.
													</p>
												{/if}
											</li>
										{/each}
									</ul>
								{/if}
							</section>
						</div>
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

				<section
					aria-labelledby="logs-title"
					class="logs-pane flex min-h-0 flex-col border-t border-line bg-surface"
					id="logs-panel"
				>
					<header class="flex shrink-0 items-center gap-2 border-b border-line-soft px-3 py-2">
						<h2 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="logs-title">
							Logs
						</h2>
						<span class="flex-1"></span>
						<select aria-label="Minimum level" class="field" onchange={setMinLevel} value={minLevel}>
							<option value="trace">All levels</option>
							<option value="debug">Debug and above</option>
							<option value="info">Info and above</option>
							<option value="warn">Warn and above</option>
							<option value="error">Error and above</option>
							<option value="fatal">Fatal only</option>
						</select>
						<select aria-label="Lines to show" class="field" onchange={reloadLogs} bind:value={tail}>
							<option value={100}>100 lines</option>
							<option value={500}>500 lines</option>
							<option value={2000}>2000 lines</option>
						</select>
					</header>
					{#if filteredLogs.length === 0}
						<p class="p-3 text-sm text-neutral-500">
							{logs.length === 0 ? "No log lines yet." : "No lines at this level."}
						</p>
					{:else}
						<div
							class="min-h-0 flex-1 overflow-auto bg-black p-2 text-xs"
							onscroll={onLogsScroll}
							bind:this={logPane}
						>
							<table class="w-full border-collapse">
								<tbody>
									{#each filteredLogs as entry, index (index)}
										{const timeParts = logTimeParts(entry.time)}
										{const access =
											entry.source === "supervisor" ? parseAccessLine(entry.message) : null}
										<tr class={entry.source === "supervisor" ? "bg-violet-500/5" : ""}>
											<td
												class="w-0 pr-3 align-top whitespace-nowrap text-neutral-500 tabular-nums"
											>
												<time class="log-time" datetime={entry.time}>
													<span class="log-time-segment">{timeParts[0] ?? ""}</span>
													{#if timeParts.length > 1}
														<span class="log-time-colon">:</span>
														<span class="log-time-segment">{timeParts[1]}</span>
													{/if}
													{#if timeParts.length > 2}
														<span class="log-time-colon">:</span>
														<span class="log-time-segment">{timeParts[2]}</span>
													{/if}
												</time>
											</td>
											<td class="w-0 pr-3 align-top whitespace-nowrap">
												{#if entry.source === "supervisor"}
													<span
														class="inline-block border border-violet-500/60 px-1 text-[10px] tracking-wide text-violet-300 uppercase"
														title="Logged by the supervisor"
													>
														proxy
													</span>
												{:else}
													<span
														class="inline-block border border-neutral-700 px-1 text-[10px] tracking-wide text-neutral-500 uppercase"
														title="Logged by the app"
													>
														app
													</span>
												{/if}
											</td>
											<td
												class="w-0 pr-3 align-top whitespace-nowrap {levelClasses[entry.level]}"
											>
												{#if !access}
													{entry.level.toUpperCase()}
												{/if}
											</td>
											<td
												class="align-top whitespace-pre-wrap wrap-break-word {levelMessageClasses[
													entry.level
												]}"
											>
												{#if access}
													<span class="inline-flex flex-wrap items-baseline gap-x-2">
														<span
															class="inline-block w-14 shrink-0 font-mono text-neutral-200"
															>{access.method}</span
														>
														<span
															class="shrink-0 tabular-nums {accessStatusClass(
																access.status,
															)}"
															>{access.status}</span
														>
														<span class="shrink-0 text-neutral-500 tabular-nums"
															>{access.duration}</span
														>
														<span class="min-w-0 flex-1 wrap-break-word"
															>{access.path}</span
														>
													</span>
												{:else}
													{entry.message}
												{/if}
											</td>
										</tr>
									{/each}
								</tbody>
							</table>
						</div>
					{/if}
				</section>
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

.workspace > aside {
	grid-area: sidebar;
	width: var(--sidebar-width);
}

.workspace > main {
	grid-area: main;
	min-width: 0;
}

.workspace > .logs-pane {
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

/* Lexend's digits are proportional; a fixed box per timestamp segment lines the colons up without forcing a monospace log font. */
.log-time {
	display: inline-flex;
	align-items: baseline;
}

.log-time-segment {
	flex: 0 0 1.35em;
	text-align: center;
}

.log-time-colon {
	flex: none;
}

/* Below the split point the panes stack, so the side separator has nothing to adjust. */
@media (max-width: 700px) {
	.workspace {
		display: flex;
		flex-direction: column;
	}

	.workspace > aside {
		width: auto;
		border-bottom: 1px solid var(--color-line);
	}

	.workspace > .logs-pane {
		height: var(--logs-height);
	}

	.resize-handle-sidebar {
		display: none;
	}
}
</style>
