<script lang="ts">
import type { AppConfig, AppConfigPatch } from "./api"

interface Props {
	busy: boolean
	config: AppConfig | null
	error: string
	loading: boolean
	message: string
	onaddsecret(key: string, value: string): Promise<void>
	ondelete(purge: boolean): Promise<void>
	onreload(): Promise<void>
	onremovesecret(key: string): Promise<void>
	onsave(patch: AppConfigPatch): Promise<void>
	onstart(): Promise<void>
	onstop(): Promise<void>
	saving: boolean
	status: string
}

let {
	busy,
	config,
	error,
	loading,
	message,
	onaddsecret,
	ondelete,
	onreload,
	onremovesecret,
	onsave,
	onstart,
	onstop,
	saving,
	status,
}: Props = $props()

let host = $state("")
let hosts = $state("")
let health = $state("/healthz")
let memoryMB = $state(0)
let diskMB = $state(0)
let maxConcurrent = $state(0)
let jobConcurrency = $state(1)
let requestTimeout = $state("")
let startupTimeout = $state("15s")
let healthInterval = $state("10s")
let idleTimeout = $state("")
let healthFailures = $state(2)
let restartOnUnhealthy = $state(false)
let network = $state<"inherit" | "none" | "storage">("inherit")
let allowCallers = $state("")
let serverPaths = $state("")
let serverPages = $state("")
let egress = $state("")
let confirmDelete = $state(false)
let purge = $state(false)
let secretKey = $state("")
let secretValue = $state("")
let secretBusy = $state(false)
let secretError = $state("")

const secretKeys = $derived(config?.secret_keys ?? [])

$effect(() => {
	const current = config
	if (!current) return
	host = current.host
	hosts = current.hosts.join(", ")
	health = current.health
	memoryMB = current.memory_mb
	diskMB = current.disk_mb
	maxConcurrent = current.max_concurrency
	jobConcurrency = current.job_concurrency
	requestTimeout = current.request_timeout
	startupTimeout = current.startup_timeout
	healthInterval = current.health_interval
	idleTimeout = current.idle_timeout
	healthFailures = current.health_failures
	restartOnUnhealthy = current.restart_on_unhealthy
	network = current.network
	allowCallers = current.allow_callers.join(", ")
	serverPaths = current.server_paths.join(", ")
	serverPages = current.server_pages.join(", ")
	egress = current.egress.map((rule) => `${rule.cidr}:${rule.port}`).join(", ")
})

const parseNames = (value: string): string[] => [
	...new Set(
		value
			.split(/[\s,]+/)
			.map((entry) => entry.trim().toLowerCase())
			.filter((entry) => entry.length > 0),
	),
]

const parseEgress = (value: string): { cidr: string; port: number }[] => {
	const rules: { cidr: string; port: number }[] = []
	for (const entry of value.split(/[\s,]+/)) {
		const separator = entry.lastIndexOf(":")
		if (separator <= 0) continue
		const cidr = entry.slice(0, separator).trim()
		const port = Number(entry.slice(separator + 1))
		if (cidr && Number.isInteger(port) && port > 0) rules.push({ cidr, port })
	}
	return rules
}

const parsePaths = (value: string): string[] => [
	...new Set(
		value
			.split(/[\s,]+/)
			.map((entry) => entry.trim())
			.filter((entry) => entry.length > 0),
	),
]

const parseHosts = (value: string): string[] => [
	...new Set(
		value
			.split(/[\s,]+/)
			.map((entry) => entry.trim())
			.filter((entry) => entry.length > 0),
	),
]

function save(event: SubmitEvent): void {
	event.preventDefault()
	void onsave({
		allow_callers: parseNames(allowCallers),
		disk_mb: diskMB,
		egress: parseEgress(egress),
		health: health.trim(),
		health_failures: healthFailures,
		health_interval: healthInterval.trim(),
		host: host.trim(),
		hosts: parseHosts(hosts),
		idle_timeout: idleTimeout.trim(),
		job_concurrency: jobConcurrency,
		max_concurrency: maxConcurrent,
		memory_mb: memoryMB,
		network,
		request_timeout: requestTimeout.trim(),
		restart_on_unhealthy: restartOnUnhealthy,
		server_paths: parsePaths(serverPaths),
		server_pages: parsePaths(serverPages),
		startup_timeout: startupTimeout.trim(),
	})
}

async function remove(): Promise<void> {
	if (!confirmDelete) {
		confirmDelete = true
		return
	}
	await ondelete(purge)
	confirmDelete = false
}

async function addSecret(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	const key = secretKey.trim()
	if (!key) {
		secretError = "Enter a secret key such as API_KEY."
		return
	}
	secretBusy = true
	secretError = ""
	try {
		await onaddsecret(key, secretValue)
		secretKey = ""
		secretValue = ""
	} catch (err) {
		secretError = err instanceof Error ? err.message : "Could not save the secret."
	} finally {
		secretBusy = false
	}
}

async function removeSecret(key: string): Promise<void> {
	secretBusy = true
	secretError = ""
	try {
		await onremovesecret(key)
	} catch (err) {
		secretError = err instanceof Error ? err.message : "Could not remove the secret."
	} finally {
		secretBusy = false
	}
}
</script>

<div
	aria-labelledby="app-tab-settings"
	class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
	id="app-panel-settings"
	role="tabpanel"
>
	<div class="flex flex-wrap items-center justify-between gap-2">
		<div class="flex items-center gap-2">
			<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="settings-title">
				Lifecycle
			</h3>
			<span class="border border-line px-1 text-[10px] tracking-wide text-neutral-400 uppercase">{status}</span>
		</div>
		<div class="flex flex-wrap items-center gap-2">
			<button class="btn" disabled={loading} onclick={onreload} type="button">
				{loading ? "Reloading…" : "Reload config"}
			</button>
			{#if status === "stopped" || status === "idle"}
				<button class="btn btn-primary" disabled={busy} onclick={onstart} type="button">
					{busy ? "Starting…" : "Start"}
				</button>
			{:else}
				<button class="btn" disabled={busy} onclick={onstop} type="button">
					{busy ? "Stopping…" : "Stop"}
				</button>
			{/if}
		</div>
	</div>

	{#if error}
		<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
			{error}
		</p>
	{/if}
	{#if message}
		<p class="border border-primary-500/40 bg-primary-500/10 px-2 py-1 text-sm text-primary-300">
			{message}
		</p>
	{/if}

	{#if loading && !config}
		<p class="text-sm text-neutral-500">Loading config…</p>
	{:else if config}
		<form class="grid max-w-3xl grid-cols-1 gap-3 sm:grid-cols-2" onsubmit={save}>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Host</span>
				<input aria-label="Host" class="field" bind:value={host}>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Host aliases</span>
				<input
					aria-label="Host aliases"
					class="field"
					placeholder="*.apps.example.com, old.example.com"
					bind:value={hosts}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Health path</span>
				<input aria-label="Health path" class="field" bind:value={health}>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Memory limit (MB)</span>
				<input aria-label="Memory limit in MB" class="field" min="0" bind:value={memoryMB} type="number">
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Disk quota (MB)</span>
				<input aria-label="Disk quota in MB" class="field" min="0" bind:value={diskMB} type="number">
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Max concurrent requests</span>
				<input
					aria-label="Maximum concurrent requests"
					class="field"
					min="0"
					bind:value={maxConcurrent}
					type="number"
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Job concurrency</span>
				<input
					aria-label="Job concurrency"
					class="field"
					max="16"
					min="1"
					type="number"
					bind:value={jobConcurrency}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Request timeout</span>
				<input
					aria-label="Request timeout"
					class="field"
					placeholder="none (for example 30s)"
					bind:value={requestTimeout}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Startup timeout</span>
				<input aria-label="Startup timeout" class="field" bind:value={startupTimeout}>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Health interval</span>
				<input aria-label="Health interval" class="field" bind:value={healthInterval}>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Idle timeout</span>
				<input
					aria-label="Idle timeout"
					class="field"
					placeholder="default 10m; 0/none/off keeps it running"
					bind:value={idleTimeout}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Health failures</span>
				<input
					aria-label="Health failure threshold"
					class="field"
					min="1"
					type="number"
					bind:value={healthFailures}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Network</span>
				<select aria-label="Network access" class="field" bind:value={network}>
					<option value="inherit">inherit (storage and outbound)</option>
					<option value="storage">storage (storage and app fetch only)</option>
					<option value="none">none (no sockets)</option>
				</select>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Allowed callers</span>
				<input
					aria-label="Allowed app callers"
					class="field"
					placeholder="frontend, worker"
					bind:value={allowCallers}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Server paths</span>
				<input
					aria-label="Server-only path prefixes"
					class="field"
					placeholder="/auth, /api"
					bind:value={serverPaths}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Server pages</span>
				<input
					aria-label="Component-selected page prefixes"
					class="field"
					placeholder="/, /login"
					bind:value={serverPages}
				>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Egress allowlist</span>
				<input
					aria-label="Egress allowlist"
					class="field"
					placeholder="203.0.113.0/24:443"
					bind:value={egress}
				>
			</label>
			<label class="flex items-center gap-2 self-end pb-1 text-sm text-neutral-300">
				<input class="accent-primary-500" type="checkbox" bind:checked={restartOnUnhealthy}>
				Restart when health checks fail
			</label>
			<div class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Env keys</span>
				<p class="text-sm text-neutral-400">
					{config.env_keys.length === 0 ? "None" : config.env_keys.join(", ")}
				</p>
			</div>
			<div class="flex flex-wrap items-center gap-2 sm:col-span-2">
				<button class="btn btn-primary" disabled={saving} type="submit">
					{saving ? "Saving…" : "Save config"}
				</button>
				<p class="text-xs text-neutral-500">
					Env values stay hidden. Memory, concurrency, timeout, network and egress changes need a restart;
					host, host aliases, health settings, allowed callers and server paths apply immediately. Egress
					entries are IPv4 CIDR:port pairs for explicit outbound access in untrusted mode. Use wildcard
					aliases such as *.apps.example.com with a matching certificate.
				</p>
			</div>
		</form>

		<section class="mt-4 flex max-w-3xl flex-col gap-2 border-t border-line pt-4">
			<h4 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">Secrets</h4>
			<p class="text-xs text-neutral-500">
				Stored separately from app.json and never shown again. Reference one as secret://KEY in an env
				value; a restart applies it.
			</p>
			{#if secretError}
				<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
					{secretError}
				</p>
			{/if}
			{#if secretKeys.length === 0}
				<p class="text-sm text-neutral-400">No secrets set.</p>
			{:else}
				<ul class="flex flex-col gap-1">
					{#each secretKeys as key (key)}
						<li class="flex items-center justify-between gap-2 border border-line-soft px-2 py-1">
							<span class="font-mono text-sm text-neutral-300">{key}</span>
							<button
								class="btn btn-danger"
								disabled={secretBusy}
								onclick={() => void removeSecret(key)}
								type="button"
							>
								Remove
							</button>
						</li>
					{/each}
				</ul>
			{/if}
			<form class="flex flex-wrap items-end gap-2" onsubmit={addSecret}>
				<label class="flex flex-col gap-1">
					<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Key</span>
					<input aria-label="Secret key" class="field" placeholder="API_KEY" bind:value={secretKey}>
				</label>
				<label class="flex flex-col gap-1">
					<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Value</span>
					<input
						aria-label="Secret value"
						autocomplete="new-password"
						class="field"
						type="password"
						bind:value={secretValue}
					>
				</label>
				<button class="btn btn-primary" disabled={secretBusy} type="submit">
					{secretBusy ? "Saving…" : "Set secret"}
				</button>
			</form>
		</section>

		<section class="mt-4 flex max-w-3xl flex-col gap-2 border-t border-red-500/30 pt-4">
			<h4 class="text-xs font-semibold tracking-wide text-red-400 uppercase">Delete app</h4>
			<p class="text-xs text-neutral-500">
				Removes the code and stops the process. State is kept unless purge is checked.
			</p>
			<label class="flex items-center gap-2 text-sm text-neutral-300">
				<input class="accent-red-500" bind:checked={purge} type="checkbox">
				Also remove the database, backups, logs, deployment history and /data
			</label>
			<button class={confirmDelete ? "btn btn-danger" : "btn"} disabled={busy} onclick={remove} type="button">
				{confirmDelete ? "Click again to delete" : "Delete app"}
			</button>
		</section>
	{/if}
</div>
