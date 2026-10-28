<script lang="ts">
import { onMount } from "svelte"
import {
	type AppInfo,
	deployBundle,
	fetchApp,
	fetchApps,
	fetchLogs,
	forgetToken,
	getToken,
	type LogEntry,
	restartApp,
	setToken,
	subscribe,
} from "../lib/api"

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 520
const MIN_VIEWPORT_FOR_SIDEBAR = 700
/** Pixels a separator moves per arrow-key press (tripled while Shift is held). */
const KEY_STEP = 16

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
let gripY = $state<number | null>(null)
let stopStream: (() => void) | null = null

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
	const url = name ? `?app=${encodeURIComponent(name)}` : window.location.pathname
	window.history.replaceState(null, "", url)
	if (token && name) {
		void loadDetail(token, name)
		void loadLogs(token, name)
	}
}

async function loadDetail(key: string, name: string): Promise<void> {
	try {
		detail = await fetchApp(key, name)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not load the app."
	}
}

async function loadLogs(key: string, name: string): Promise<void> {
	try {
		logs = await fetchLogs(key, name, tail)
		error = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not load logs."
	}
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

function reloadLogs(): void {
	if (!token || !selected) return
	void loadLogs(token, selected)
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
	live = false
}

/** Drag the separator, tracking on the window so the drag survives a fast pointer. */
function startSidebarResize(event: PointerEvent): void {
	event.preventDefault()
	const startX = event.clientX
	const startWidth = sidebarWidth
	const previousCursor = document.body.style.cursor
	const previousSelect = document.body.style.userSelect
	document.body.style.cursor = "col-resize"
	document.body.style.userSelect = "none"

	const onMove = (move: PointerEvent): void => {
		sidebarWidth = clampSidebar(startWidth + move.clientX - startX)
	}
	const stop = (): void => {
		window.removeEventListener("pointermove", onMove)
		window.removeEventListener("pointerup", stop)
		window.removeEventListener("pointercancel", stop)
		document.body.style.cursor = previousCursor
		document.body.style.userSelect = previousSelect
	}
	window.addEventListener("pointermove", onMove)
	window.addEventListener("pointerup", stop)
	window.addEventListener("pointercancel", stop)
}

function onSeparatorKeydown(event: KeyboardEvent): void {
	if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return
	event.preventDefault()
	const step = event.shiftKey ? KEY_STEP * 3 : KEY_STEP
	sidebarWidth = clampSidebar(sidebarWidth + (event.key === "ArrowLeft" ? -step : step))
}

/** The grip tracks the pointer along the separator, hinting where the drag will grab. */
function moveGrip(event: PointerEvent): void {
	const bounds = (event.currentTarget as HTMLElement).getBoundingClientRect()
	gripY = event.clientY - bounds.top
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
		await loadLogs(token, selected)
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

onMount((): (() => void) => {
	const saved = getToken()
	const name = readSelection()
	if (saved) {
		token = saved
		selected = name
		connect(saved)
		void refresh(saved)
		if (name) void loadLogs(saved, name)
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
		<div class="workspace" style="--sidebar-width: {sidebarWidth}px">
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
				class="resize-handle hover:bg-primary-500/10"
				onkeydown={onSeparatorKeydown}
				onpointerdown={startSidebarResize}
				onpointermove={moveGrip}
				role="separator"
				tabindex="0"
			>
				<span aria-hidden="true" class="resize-grip" style:top={gripY === null ? "50%" : `${gripY}px`}></span>
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

						<section aria-labelledby="logs-title" class="flex min-h-0 flex-col gap-2">
							<div class="flex items-center gap-2">
								<h3
									class="text-xs font-semibold tracking-wide text-neutral-400 uppercase"
									id="logs-title"
								>
									Logs
								</h3>
								<span class="flex-1"></span>
								<select
									aria-label="Lines to show"
									class="field"
									onchange={reloadLogs}
									bind:value={tail}
								>
									<option value={100}>100 lines</option>
									<option value={500}>500 lines</option>
									<option value={2000}>2000 lines</option>
								</select>
								<button class="btn" disabled={loading} onclick={reloadLogs} type="button">
									Reload
								</button>
							</div>
							{#if logs.length === 0}
								<p class="text-sm text-neutral-500">No log lines yet.</p>
							{:else}
								<div class="max-h-96 overflow-auto border border-line bg-black p-2 font-mono text-xs">
									{#each logs as entry, index (index)}
										<div class="flex gap-3 whitespace-pre-wrap wrap-break-word">
											<time class="shrink-0 text-neutral-500 tabular-nums" datetime={entry.time}
												>{entry.time === "" ? "—" : formatLogTime(entry.time)}</time
											>
											<span class="text-neutral-300">{entry.message}</span>
										</div>
									{/each}
								</div>
							{/if}
						</section>

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
		</div>
	{/if}
</div>

<style>
.workspace {
	position: relative;
	display: flex;
	flex: 1;
	min-height: 0;
}

.workspace > aside {
	width: var(--sidebar-width);
}

/* The separator rides on the pane boundary, so its grip sits exactly on the divider. */
.resize-handle {
	position: absolute;
	top: 0;
	bottom: 0;
	left: var(--sidebar-width);
	z-index: 30;
	width: 9px;
	touch-action: none;
	translate: -50% 0;
}

.resize-handle::before {
	position: absolute;
	top: 0;
	bottom: 0;
	left: 50%;
	width: 1px;
	content: "";
	background: var(--color-line);
	translate: -50% 0;
}

.resize-grip {
	position: absolute;
	top: 50%;
	left: 50%;
	width: 2px;
	height: 2.5rem;
	background: var(--color-neutral-600);
	opacity: 0.35;
	translate: -50% -50%;
	transition:
		opacity 120ms ease,
		background-color 120ms ease;
}

.resize-handle:hover .resize-grip,
.resize-handle:focus-visible .resize-grip,
.resize-handle:active .resize-grip {
	background: var(--color-primary-500);
	opacity: 1;
}

.resize-handle:hover,
.resize-handle:active {
	cursor: col-resize;
}

/* Below the split point the panes stack, so the separator has nothing to adjust. */
@media (max-width: 700px) {
	.workspace {
		flex-direction: column;
	}

	.workspace > aside {
		width: auto;
		border-bottom: 1px solid var(--color-line);
	}

	.resize-handle {
		display: none;
	}
}
</style>
