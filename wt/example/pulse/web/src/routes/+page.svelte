<script lang="ts">
import { onMount } from "svelte"
import {
	type Check,
	createMonitor,
	deleteMonitor,
	fetchMonitors,
	fetchStats,
	type Monitor,
	runChecks,
	type Stats,
	ValidationError,
} from "../lib/api"

let monitors = $state<Monitor[]>([])
let stats = $state<Stats | null>(null)
let loading = $state(true)
let checking = $state(false)
let saving = $state(false)
let live = $state(false)
let error = $state("")
let name = $state("")
let url = $state("")
let fieldErrors = $state<{ [_: string]: string }>({})

let refreshing = false

const numberFormat = new Intl.NumberFormat()

// load fetches the board and totals together; overlapping calls are skipped.
async function load(): Promise<void> {
	if (refreshing) return

	refreshing = true
	try {
		const [board, totals] = await Promise.all([fetchMonitors(), fetchStats()])
		monitors = board
		stats = totals
		error = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the pulse API."
	} finally {
		loading = false
		refreshing = false
	}
}

onMount((): (() => void) => {
	void load()
	const poll = setInterval((): void => void load(), 20000)

	const events = new EventSource("/v1/events?topic=checks")
	events.onopen = (): void => {
		live = true
	}
	events.onerror = (): void => {
		live = false
	}
	events.onmessage = (): void => {
		live = true
		void load()
	}

	return (): void => {
		clearInterval(poll)
		events.close()
	}
})

function statusOf(monitor: Monitor): "up" | "down" | "pending" {
	if (monitor.last === null) return "pending"
	return monitor.last.ok ? "up" : "down"
}

function statusLabel(monitor: Monitor): string {
	const status = statusOf(monitor)
	if (status === "pending") return "waiting"
	return status
}

function formatLatency(milliseconds: number): string {
	if (milliseconds <= 0) return "0 ms"
	if (milliseconds < 1000) return `${milliseconds} ms`
	return `${(milliseconds / 1000).toFixed(2)} s`
}

function formatUptime(value: number): string {
	if (value >= 99.95 || value === 0) return `${Math.round(value)}%`
	return `${value.toFixed(1)}%`
}

function ago(value: string): string {
	if (value === "") return "never"
	const date = new Date(value)
	if (Number.isNaN(date.getTime())) return "unknown"

	const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000))
	if (seconds < 5) return "just now"
	if (seconds < 60) return `${seconds}s ago`
	if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
	if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
	return `${Math.floor(seconds / 86400)}d ago`
}

function barHeight(point: Check, history: Check[]): number {
	if (!point.ok) return 100
	const slowest = Math.max(...history.map(entry => entry.latencyMs), 1)
	return Math.max(9, Math.round((point.latencyMs / slowest) * 100))
}

function barTitle(point: Check): string {
	const status = point.status > 0 ? `${point.status}` : point.error || "no response"
	return `${status} · ${formatLatency(point.latencyMs)} · ${ago(point.checkedAt)}`
}

function clearField(field: string): void {
	if (!(field in fieldErrors)) return

	const next = { ...fieldErrors }
	delete next[field]
	fieldErrors = next
}

async function submit(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	saving = true
	error = ""
	fieldErrors = {}
	try {
		const created = await createMonitor(name, url)
		monitors = [...monitors, created]
		name = ""
		url = ""
		await load()
	} catch (err) {
		if (err instanceof ValidationError) {
			fieldErrors = err.fields
			if (Object.keys(err.fields).length === 0) error = err.message
		} else {
			error = err instanceof Error ? err.message : "Could not add the monitor."
		}
	} finally {
		saving = false
	}
}

async function remove(monitor: Monitor): Promise<void> {
	const previous = monitors
	monitors = monitors.filter(entry => entry.id !== monitor.id)
	try {
		await deleteMonitor(monitor.id)
		await load()
	} catch (err) {
		monitors = previous
		error = err instanceof Error ? err.message : "Could not remove the monitor."
	}
}

async function checkNow(): Promise<void> {
	checking = true
	error = ""
	try {
		await runChecks()
		await load()
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not run the checks."
	} finally {
		checking = false
	}
}
</script>

<svelte:head>
	<title>Pulse – API uptime</title>
</svelte:head>

<section class="masthead">
	<div class="masthead-copy">
		<p class="eyebrow">
			<span aria-hidden="true" class="signal" class:on={live}></span>
			{live ? "live event stream" : "polling every 20s"}
		</p>
		<h1>Every endpoint,<br><em>one pulse.</em></h1>
		<p class="lede">
			The Go component probes each URL on a 30 second supervisor schedule, keeps a short latency history in SQLite
			and wakes every open dashboard over the platform event bus.
		</p>
		<div class="actions">
			<button class="primary" disabled={checking} onclick={checkNow} type="button">
				{checking ? "Probing…" : "Check now"}
			</button>
			<span class="cadence">scheduled every 30s</span>
		</div>
	</div>

	<div class="stats">
		<div class="stat">
			<span class="stat-value">{stats?.monitors ?? "—"}</span>
			<span class="stat-label">monitors</span>
		</div>
		<div class="stat good">
			<span class="stat-value">{stats?.up ?? "—"}</span>
			<span class="stat-label">up now</span>
		</div>
		<div class="stat bad">
			<span class="stat-value">{stats?.down ?? "—"}</span>
			<span class="stat-label">down</span>
		</div>
		<div class="stat">
			<span class="stat-value">{stats ? formatLatency(stats.avgLatencyMs) : "—"}</span>
			<span class="stat-label">avg latency</span>
		</div>
	</div>
</section>

{#if error}
	<p class="banner" role="alert">{error}</p>
{/if}

<section class="panel">
	<div class="panel-head">
		<div>
			<h2>Add a monitor</h2>
			<p>Any public HTTP endpoint. Probes run from this app's own network, with a 4 second timeout.</p>
		</div>
		<span class="count">{numberFormat.format(stats?.checks24h ?? 0)} checks in 24h</span>
	</div>

	<form novalidate onsubmit={submit}>
		<label class="field">
			<span>Name</span>
			<input
				aria-invalid={fieldErrors.name ? "true" : undefined}
				aria-label="Monitor name"
				autocomplete="off"
				maxlength={60}
				oninput={() => clearField("name")}
				placeholder="Payments API"
				type="text"
				bind:value={name}
			>
			{#if fieldErrors.name}
				<small>{fieldErrors.name}</small>
			{/if}
		</label>
		<label class="field grow">
			<span>URL</span>
			<input
				aria-invalid={fieldErrors.url ? "true" : undefined}
				aria-label="Monitor URL"
				autocomplete="off"
				inputmode="url"
				oninput={() => clearField("url")}
				placeholder="https://api.example.com/health"
				spellcheck="false"
				type="url"
				bind:value={url}
			>
			{#if fieldErrors.url}
				<small>{fieldErrors.url}</small>
			{/if}
		</label>
		<button class="primary" disabled={saving} type="submit">{saving ? "Adding…" : "Add monitor"}</button>
	</form>
</section>

<section aria-busy={loading} aria-label="Monitors" class="board">
	{#if loading && monitors.length === 0}
		<p class="empty">Loading monitors…</p>
	{:else if monitors.length === 0}
		<p class="empty">No monitors yet. Add one above and the next cycle will pick it up.</p>
	{:else}
		{#each monitors as monitor (monitor.id)}
			<article class="monitor" class:down={statusOf(monitor) === "down"} class:up={statusOf(monitor) === "up"}>
				<header>
					<div class="identity">
						<span aria-hidden="true" class="dot"></span>
						<div>
							<h3>{monitor.name}</h3>
							<a href={monitor.url} rel="noreferrer" target="_blank">{monitor.url}</a>
						</div>
					</div>
					<button
						aria-label={`Remove ${monitor.name}`}
						class="remove"
						onclick={() => void remove(monitor)}
						title="Remove monitor"
						type="button"
					>
						×
					</button>
				</header>

				<div class="readout">
					<div>
						<span class="metric">{monitor.last ? formatLatency(monitor.last.latencyMs) : "—"}</span>
						<span class="metric-label">latency</span>
					</div>
					<div>
						<span class="metric"
							>{monitor.last ? (monitor.last.status > 0 ? monitor.last.status : "ERR") : "—"}</span
						>
						<span class="metric-label">status</span>
					</div>
					<div>
						<span class="metric">{monitor.history.length > 0 ? formatUptime(monitor.uptime) : "—"}</span>
						<span class="metric-label">uptime</span>
					</div>
				</div>

				<div aria-hidden="true" class="spark">
					{#each monitor.history as point (point.checkedAt)}
						<span
							class="bar"
							style={`--height:${barHeight(point, monitor.history)}%`}
							title={barTitle(point)}
							class:bad={!point.ok}
						></span>
					{:else}
						<span class="waiting">awaiting first check</span>
					{/each}
				</div>

				<footer>
					<span class="state">{statusLabel(monitor)}</span>
					{#if monitor.last}
						<time datetime={monitor.last.checkedAt}>checked {ago(monitor.last.checkedAt)}</time>
					{:else}
						<span>not checked yet</span>
					{/if}
					{#if monitor.last?.error}
						<span class="detail" title={monitor.last.error}>{monitor.last.error}</span>
					{/if}
				</footer>
			</article>
		{/each}
	{/if}
</section>
