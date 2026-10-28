<script lang="ts">
import { onMount } from "svelte"
import {
	type AppInfo,
	deployBundle,
	fetchApp,
	fetchApps,
	forgetToken,
	getToken,
	type LogEntry,
	type LogLevel,
	logLevels,
	restartApp,
	setToken,
	subscribe,
	subscribeLogs,
} from "../lib/api"

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

let token = $state<string | null>(null)
let tokenInput = $state("")
let apps = $state<AppInfo[]>([])
let selected = $state<string | null>(null)
let detail = $state<AppInfo | null>(null)
let logs = $state<LogEntry[]>([])
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

function uptime(startedAt: string): string {
	const seconds = Math.max(0, Math.floor((now - new Date(startedAt).getTime()) / 1000))
	if (seconds < 60) return `${seconds}s`
	const minutes = Math.floor(seconds / 60)
	if (minutes < 60) return `${minutes}m`
	const hours = Math.floor(minutes / 60)
	if (hours < 48) return `${hours}h ${minutes % 60}m`
	return `${Math.floor(hours / 24)}d ${hours % 24}h`
}

function formatTime(value: string): string {
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

const logTimeFormat = new Intl.DateTimeFormat(undefined, {
	hour: "2-digit",
	hour12: false,
	minute: "2-digit",
	second: "2-digit",
})

function formatLogTime(value: string): string {
	if (value === "") return ""
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? value : logTimeFormat.format(date)
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
	error = ""
	actionMessage = ""
	confirmRestart = false
	closeLogs()
	const url = name ? `?app=${encodeURIComponent(name)}` : window.location.pathname
	window.history.replaceState(null, "", url)
	if (token && name) {
		void loadDetail(token, name)
		openLogs(name)
	}
}

async function loadDetail(key: string, name: string): Promise<void> {
	try {
		detail = await fetchApp(key, name)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not load the app."
	}
}

/** Stream one app's log: the recent backlog, then every new line as it is written. */
function openLogs(name: string): void {
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
		if (selected) detail = await fetchApp(key, selected)
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

const levelClasses: { [_: string]: string } = {
	debug: "text-neutral-500",
	error: "text-red-400",
	fatal: "font-semibold text-red-500",
	info: "text-neutral-400",
	trace: "text-neutral-600",
	warn: "text-amber-400",
}

const levelMessageClasses: { [_: string]: string } = {
	debug: "text-neutral-300",
	error: "text-red-200",
	fatal: "text-red-200",
	info: "text-neutral-300",
	trace: "text-neutral-400",
	warn: "text-amber-200",
}

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
		if (name) openLogs(name)
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

<div class="flex min-h-dvh flex-col">
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
		<div class="workspace" style="--sidebar-width: {sidebarWidth}px; --logs-height: {selected ? logsHeight : 0}px">
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
											>{uptime(app.startedAt)}</span
										>
									</button>
								</li>
							{/each}
						</ul>
					{/if}
				</div>
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
						<h2 class="text-sm font-semibold tracking-tight">{selected}</h2>
						<span class="flex-1"></span>
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

					<div class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
						{#if detail}
							<dl class="grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-4">
								<div class="flex flex-col">
									<dt class="text-xs tracking-wide text-neutral-500 uppercase">Host</dt>
									<dd class="text-sm wrap-break-word">{detail.host}</dd>
								</div>
								<div class="flex flex-col">
									<dt class="text-xs tracking-wide text-neutral-500 uppercase">Status</dt>
									<dd class="text-sm">{detail.status}</dd>
								</div>
								<div class="flex flex-col">
									<dt class="text-xs tracking-wide text-neutral-500 uppercase">Started</dt>
									<dd class="text-sm">{formatTime(detail.startedAt)}</dd>
								</div>
								<div class="flex flex-col">
									<dt class="text-xs tracking-wide text-neutral-500 uppercase">PID</dt>
									<dd class="text-sm tabular-nums">{detail.pid ?? "–"}</dd>
								</div>
							</dl>
						{/if}

						{#if actionMessage}
							<p
								class="border border-primary-500/40 bg-primary-500/10 px-2 py-1 text-sm text-primary-300"
							>
								{actionMessage}
							</p>
						{/if}

						<form
							class="flex flex-wrap items-center gap-2 border-t border-line-soft pt-4"
							onsubmit={upload}
						>
							<label class="text-xs tracking-wide text-neutral-400 uppercase" for="bundle">
								Deploy bundle
							</label>
							<input
								accept=".tar.gz, application/gzip"
								class="field file:mr-2 file:cursor-pointer file:border-0 file:bg-neutral-800 file:px-2 file:py-0.5 file:text-sm file:text-neutral-200 file:transition-colors hover:file:bg-neutral-700"
								id="bundle"
								type="file"
							>
							<button class="btn btn-primary" disabled={deploying} type="submit">
								{deploying ? "Deploying…" : "Deploy"}
							</button>
						</form>

						{#if error}
							<p
								class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400"
								role="alert"
							>
								{error}
							</p>
						{/if}
					</div>
				{:else if error}
					<div class="p-4">
						<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
							{error}
						</p>
					</div>
				{:else}
					<div class="flex flex-1 items-center justify-center p-4">
						<p class="text-sm text-neutral-500">Select an app to inspect it.</p>
					</div>
				{/if}
			</main>

			{#if selected}
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
							class="min-h-0 flex-1 overflow-auto bg-black p-2 font-mono text-xs"
							onscroll={onLogsScroll}
							bind:this={logPane}
						>
							<table class="w-full border-collapse">
								<tbody>
									{#each filteredLogs as entry, index (index)}
										<tr>
											<td
												class="w-0 pr-3 align-top whitespace-nowrap text-neutral-500 tabular-nums"
											>
												<time datetime={entry.time}
													>{entry.time === "" ? "—" : formatLogTime(entry.time)}</time
												>
											</td>
											<td
												class="w-0 pr-3 align-top whitespace-nowrap {levelClasses[entry.level]}"
											>
												{entry.level.toUpperCase()}
											</td>
											<td
												class="align-top whitespace-pre-wrap wrap-break-word {levelMessageClasses[
													entry.level
												]}"
											>
												{entry.message}
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
