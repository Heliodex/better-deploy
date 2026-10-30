<script lang="ts">
import type { AppInfo, ResourceSample } from "./api"
import { spans } from "./chart"
import { formatCPU, formatMemory, formatTime } from "./format"
import ResourceChart from "./ResourceChart.svelte"

interface Props {
	actionMessage: string
	deploying: boolean
	detail: AppInfo | null
	error: string
	history: ResourceSample[]
	onupload(event: SubmitEvent): Promise<void>
	span: number
}

let { actionMessage, deploying, detail, error, history, onupload, span = $bindable() }: Props = $props()
</script>

<div
	aria-labelledby="app-tab-overview"
	class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
	id="app-panel-overview"
	role="tabpanel"
>
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

		<section aria-labelledby="resources-title" class="flex flex-col gap-2 border border-line-soft bg-surface p-2">
			<div class="flex items-center justify-between gap-2">
				<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="resources-title">
					Resources
				</h3>
				<select aria-label="Resources timespan" class="field px-1 py-0 text-xs" bind:value={span}>
					{#each spans as option (option.seconds)}
						<option value={option.seconds}>{option.label}</option>
					{/each}
				</select>
			</div>
			<div class="flex flex-col gap-4">
				<ResourceChart
					color="primary"
					format={formatCPU}
					label="CPU"
					samples={history}
					{span}
					unit={1}
					value={(sample: ResourceSample) => sample.cpuPercent}
				/>
				<ResourceChart
					color="sky"
					format={formatMemory}
					label="Memory"
					samples={history}
					{span}
					unit={1024 * 1024}
					value={(sample: ResourceSample) => sample.memoryBytes}
				/>
			</div>
		</section>
	{/if}

	{#if actionMessage}
		<p class="border border-primary-500/40 bg-primary-500/10 px-2 py-1 text-sm text-primary-300">
			{actionMessage}
		</p>
	{/if}

	<form class="flex flex-wrap items-center gap-2 border-t border-line-soft pt-4" onsubmit={onupload}>
		<label class="text-xs tracking-wide text-neutral-400 uppercase" for="bundle">Deploy bundle</label>
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
		<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
			{error}
		</p>
	{/if}
</div>
