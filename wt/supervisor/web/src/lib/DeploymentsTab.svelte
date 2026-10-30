<script lang="ts">
import type { DeploymentInfo } from "./api"
import { formatSize, formatTime } from "./format"

interface Props {
	actionMessage: string
	deployments: DeploymentInfo[]
	error: string
	loading: boolean
	onrollback(id: string): Promise<void>
	rollingBack: string | null
}

let { actionMessage, deployments, error, loading, onrollback, rollingBack }: Props = $props()
let confirming = $state<string | null>(null)

async function rollback(id: string): Promise<void> {
	if (confirming !== id) {
		confirming = id
		return
	}
	confirming = null
	await onrollback(id)
}
</script>

<div
	aria-labelledby="app-tab-deployments"
	class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
	id="app-panel-deployments"
	role="tabpanel"
>
	<div class="flex items-center justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="deployments-title">
			Deployments
		</h3>
		<span class="text-xs text-neutral-500">
			{deployments.length}
			{deployments.length === 1 ? "bundle" : "bundles"}
		</span>
	</div>

	<p class="text-xs text-neutral-500">
		Rolling back swaps code and app.json only. The database and /data stay as they are, so schema changes between
		versions still matter.
	</p>

	{#if error}
		<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
			{error}
		</p>
	{/if}

	{#if loading}
		<p class="text-sm text-neutral-500">Loading deployments…</p>
	{:else if deployments.length === 0}
		<p class="text-sm text-neutral-500">No retained deployments yet.</p>
	{:else}
		<ul class="flex flex-col">
			{#each deployments as deployment (deployment.id)}
				<li
					class="flex flex-wrap items-center gap-3 border-t border-line-soft py-2 text-xs first:border-t-0 first:pt-0"
				>
					<time class="text-neutral-300" datetime={deployment.deployedAt}>
						{formatTime(deployment.deployedAt)}
					</time>
					<code class="text-neutral-500">{deployment.digest.slice(0, 12)}</code>
					<span class="text-neutral-500 tabular-nums">{formatSize(deployment.bytes)}</span>
					<span class="flex-1"></span>
					<button
						class={confirming === deployment.id ? "btn btn-danger" : "btn"}
						disabled={rollingBack !== null}
						onclick={(): Promise<void> => rollback(deployment.id)}
						type="button"
					>
						{rollingBack === deployment.id
							? "Rolling back…"
							: confirming === deployment.id
								? "Click again to confirm"
								: "Roll back"}
					</button>
				</li>
			{/each}
		</ul>
	{/if}

	{#if actionMessage}
		<p class="border border-primary-500/40 bg-primary-500/10 px-2 py-1 text-sm text-primary-300">
			{actionMessage}
		</p>
	{/if}
</div>
