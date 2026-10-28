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
	restartApp,
	setToken,
	subscribe,
} from "../lib/api"

let token = $state<string | null>(null)
let tokenInput = $state("")
let apps = $state<AppInfo[]>([])
let selected = $state<string | null>(null)
let detail = $state<AppInfo | null>(null)
let logs = $state<string[]>([])
let tail = $state(200)
let error = $state("")
let loading = $state(true)
let restarting = $state(false)
let deploying = $state(false)
let actionMessage = $state("")
let confirmRestart = $state(false)
let live = $state(false)
let now = $state(Date.now())
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

function readSelection(): string | null {
	const name = new URLSearchParams(window.location.search).get("app")
	return name && name.length > 0 ? name : null
}

function select(name: string | null): void {
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

<div class="page">
	<header class="row">
		<h1>wt admin</h1>
		<span class="spacer"></span>
		{#if token}
			<span class="status"
				><span aria-hidden="true" class="dot {live ? "running" : ""}"></span>
				{live ? "live" : "connecting…"}</span
			>
			<button onclick={forget} type="button">Forget token</button>
		{/if}
	</header>

	{#if !token}
		<form class="card" onsubmit={submitToken}>
			<h2>Sign in</h2>
			<p class="muted">Enter the supervisor deploy token. It stays in this browser only.</p>
			<label>
				<span>Token</span>
				<input autocomplete="off" placeholder="deploy token" type="password" bind:value={tokenInput}>
			</label>
			<div>
				<button type="submit">Connect</button>
			</div>
			{#if error}
				<div class="error" role="alert">{error}</div>
			{/if}
		</form>
	{:else if loading && apps.length === 0}
		<p class="muted">Loading apps…</p>
	{:else}
		<div class="layout">
			<aside aria-label="Apps" class="card sidebar">
				<div class="row">
					<h2>Apps</h2>
					<span class="spacer"></span>
					<button disabled={loading} onclick={refreshList} type="button">
						{loading ? "Refreshing…" : "Refresh"}
					</button>
				</div>
				{#if apps.length === 0}
					<p class="muted">No apps deployed.</p>
				{:else}
					<ul class="app-list">
						{#each apps as app (app.name)}
							<li>
								<button
									aria-current={selected === app.name}
									onclick={(): void => select(app.name)}
									type="button"
								>
									<span aria-hidden="true" class="dot {app.status}"></span>
									<span>{app.name}</span>
									<span class="muted">{uptime(app.startedAt)}</span>
								</button>
							</li>
						{/each}
					</ul>
				{/if}
			</aside>

			<div class="main">
				{#if selected}
					<section aria-busy={loading} aria-live="polite" class="card">
						<div class="row">
							<h2>{selected}</h2>
							<span class="spacer"></span>
							<button onclick={(): void => select(null)} type="button">Close</button>
							<button disabled={restarting} onclick={restart} type="button">
								{restarting ? "Restarting…" : confirmRestart ? "Click again to confirm" : "Restart"}
							</button>
						</div>
						{#if detail}
							<dl class="facts">
								<div>
									<dt>Host</dt>
									<dd>{detail.host}</dd>
								</div>
								<div>
									<dt>Status</dt>
									<dd>{detail.status}</dd>
								</div>
								<div>
									<dt>Started</dt>
									<dd>{formatTime(detail.startedAt)}</dd>
								</div>
								<div>
									<dt>PID</dt>
									<dd>{detail.pid ?? "–"}</dd>
								</div>
							</dl>
						{/if}
						{#if actionMessage}
							<p>{actionMessage}</p>
						{/if}
						{#if error}
							<div class="error" role="alert">{error}</div>
						{/if}
						<div class="row">
							<h3>Logs</h3>
							<span class="spacer"></span>
							<select aria-label="Lines to show" onchange={reloadLogs} bind:value={tail}>
								<option value={100}>100 lines</option>
								<option value={500}>500 lines</option>
								<option value={2000}>2000 lines</option>
							</select>
							<button disabled={loading} onclick={reloadLogs} type="button">Reload</button>
						</div>
						{#if logs.length === 0}
							<p class="muted">No log lines yet.</p>
						{:else}
							<pre class="logs">{logs.join("\n")}</pre>
						{/if}
						<form class="row" onsubmit={upload}>
							<label for="bundle">Deploy bundle</label>
							<input accept=".tar.gz, application/gzip" id="bundle" type="file">
							<button disabled={deploying} type="submit">{deploying ? "Deploying…" : "Deploy"}</button>
						</form>
					</section>
				{/if}

				{#if error && !selected}
					<div class="error" role="alert">{error}</div>
				{/if}
				{#if !selected && !error}
					<div class="card">
						<p class="muted">Select an app to inspect it.</p>
					</div>
				{/if}
			</div>
		</div>
	{/if}
</div>

<style>
.spacer {
	flex: 1;
}

.facts {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
	gap: 8px 24px;
}

.facts div {
	display: flex;
	flex-direction: column;
}

.facts dt {
	font-size: 0.72rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.08em;
}

.facts dd {
	margin: 0;
	overflow-wrap: anywhere;
}

h3 {
	margin: 0;
	font-size: 0.95rem;
}

label {
	display: flex;
	flex-direction: column;
	gap: 6px;
}

label > span {
	font-size: 0.72rem;
	font-weight: 600;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.08em;
}
</style>
