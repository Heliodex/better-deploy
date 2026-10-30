<script lang="ts">
import type { BackupInfo } from "./api"
import BackupTree from "./BackupTree.svelte"
import { backupKindLabel } from "./backup-tree"
import { formatSize, formatTime } from "./format"

interface Props {
	actionMessage: string
	backingUp: boolean
	backups: BackupInfo[]
	confirming: string | null
	current: string
	database: boolean
	enabled: boolean
	error: string
	every: string
	importing: boolean
	keep: number
	loading: boolean
	name: string
	onbackup(): Promise<void>
	onimport(file: File): Promise<void>
	onrevert(name: string): Promise<void>
	onsave(event: SubmitEvent): Promise<void>
	onselect(name: string): void
	restoring: string | null
	saving: boolean
	selected: string | null
}

let {
	actionMessage,
	backingUp,
	backups,
	confirming,
	current,
	database,
	enabled = $bindable(),
	error,
	every = $bindable(),
	importing,
	keep = $bindable(),
	loading,
	name,
	onbackup,
	onimport,
	onselect,
	onsave,
	onrevert,
	restoring,
	saving,
	selected,
}: Props = $props()

const selectedInfo = $derived(backups.find(backup => backup.name === selected) ?? null)
const parent = $derived(
	selectedInfo?.parent ? (backups.find(backup => backup.name === selectedInfo.parent) ?? null) : null,
)
const children = $derived(backups.filter(backup => backup.parent === selected).length)

async function importFile(event: Event): Promise<void> {
	const input = event.currentTarget as HTMLInputElement
	const file = input.files?.[0]
	if (!file) return
	await onimport(file)
	input.value = ""
}
</script>

<div
	aria-labelledby="app-tab-backups"
	class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
	id="app-panel-backups"
	role="tabpanel"
>
	<div class="flex items-center justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="backups-title">Backups</h3>
		<div class="flex items-center gap-3">
			<span class="text-xs text-neutral-500">
				{backups.length}
				{backups.length === 1 ? "snapshot" : "snapshots"}
			</span>
			<label class="btn cursor-pointer">
				{importing ? "Importing…" : "Import snapshot"}
				<input
					accept=".gz,application/gzip"
					aria-label="Import snapshot"
					class="sr-only"
					disabled={importing}
					onchange={(event): void => void importFile(event)}
					type="file"
				/>
			</label>
		</div>
	</div>

	<form class="flex flex-wrap items-end gap-3" onsubmit={onsave}>
		<label class="flex items-center gap-2 pb-1.5 text-sm">
			<input aria-label="Automatic backups" class="accent-primary-500" type="checkbox" bind:checked={enabled}>
			Automatic backups
		</label>
		<label class="flex flex-col gap-1">
			<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Every</span>
			<input aria-label="Backup interval" class="field w-24" placeholder="24h" bind:value={every}>
		</label>
		<label class="flex flex-col gap-1">
			<span class="text-[10px] tracking-wide text-neutral-500 uppercase">Keep</span>
			<input aria-label="Backups to keep" class="field w-20" max={100} min={1} type="number" bind:value={keep}>
		</label>
		<button class="btn" disabled={saving} type="submit">
			{saving ? "Saving…" : "Save"}
		</button>
	</form>

	<p class="text-xs text-neutral-500">
		Every snapshot includes <code>/data</code> and, when the app has one, its SQLite database. The database uses
		SQLite's online backup, so the app keeps serving while one is taken.
	</p>

	{#if error}
		<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
			{error}
		</p>
	{/if}

	{#if backups.length === 0}
		<div class="flex flex-col items-start gap-2">
			<p class="text-sm text-neutral-500">
				{loading
					? "Loading snapshots…"
					: database
						? "No snapshots yet."
						: "No database yet. A snapshot will contain /data only."}
			</p>
			<button
				class="btn btn-primary"
				disabled={backingUp}
				onclick={onbackup}
				title="Snapshot the live database and /data"
				type="button"
			>
				{backingUp ? "Backing up…" : "Back up current state"}
			</button>
		</div>
	{:else}
		<BackupTree {backups} {current} {onselect} {selected} />

		{#if selectedInfo}
			<div class="flex flex-col gap-2 border-t border-line-soft pt-2">
				<div class="flex flex-wrap items-center gap-2">
					<h4 class="text-sm font-semibold">
						{formatTime(selectedInfo.createdAt)}
					</h4>
					<span class="border border-line px-1 text-[10px] tracking-wide text-neutral-400 uppercase">
						{backupKindLabel(selectedInfo.kind)}
					</span>
					{#if selected === current}
						<span
							class="border border-primary-500/40 bg-primary-500/10 px-1 text-[10px] tracking-wide text-primary-300 uppercase"
						>
							Current
						</span>
					{/if}
				</div>

				<dl class="grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
					<div>
						<dt class="text-neutral-500">Database</dt>
						<dd class="tabular-nums">
							{selectedInfo.size ? formatSize(selectedInfo.size) : "–"}
						</dd>
					</div>
					<div>
						<dt class="text-neutral-500">Data</dt>
						<dd class="tabular-nums">
							{selectedInfo.hasData ? formatSize(selectedInfo.dataSize ?? 0) : "–"}
						</dd>
					</div>
					<div>
						<dt class="text-neutral-500">After</dt>
						<dd>
							{parent ? formatTime(parent.createdAt) : "Start of tree"}
						</dd>
					</div>
					<div>
						<dt class="text-neutral-500">Children</dt>
						<dd class="tabular-nums">{children}</dd>
					</div>
				</dl>

				<p class="text-xs text-neutral-500">
					{#if selected === current}
						This snapshot is current, so a new backup continues this branch.
					{:else}
						Reverting replaces the data this snapshot captured, and the next backup becomes a new branch
						from it.
					{/if}
				</p>

				<div class="flex flex-wrap items-center gap-2">
					<button
						class="btn btn-primary"
						disabled={backingUp}
						onclick={onbackup}
						title="Snapshot the live database and /data; the snapshot continues from the current node"
						type="button"
					>
						{backingUp ? "Backing up…" : "Back up current state"}
					</button>
					<a
						class="btn"
						download={`${selectedInfo.name}.tar.gz`}
						href={`/apps/${encodeURIComponent(name)}/backups/${encodeURIComponent(selectedInfo.name)}/download`}
					>
						Download
					</a>
					{#if selectedInfo.name !== current}
						<button
							class={confirming === selectedInfo.name ? "btn btn-danger" : "btn"}
							disabled={restoring !== null}
							onclick={(): Promise<void> => onrevert(selectedInfo.name)}
							type="button"
						>
							{restoring === selectedInfo.name
								? "Reverting…"
								: confirming === selectedInfo.name
									? "Click again to confirm"
									: "Revert to this snapshot"}
						</button>
					{/if}
				</div>
			</div>
		{/if}
	{/if}
	{#if actionMessage}
		<p class="border border-primary-500/40 bg-primary-500/10 px-2 py-1 text-sm text-primary-300">
			{actionMessage}
		</p>
	{/if}
</div>
