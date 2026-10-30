<script lang="ts">
import type { AppInfo } from "./api"
import { uptime } from "./format"
import { type Tab, tabLabels, tabs } from "./tabs"

interface Props {
	apps: AppInfo[]
	live: boolean
	loading: boolean
	now: number
	onforget(event: MouseEvent): void
	onrefresh(): void
	onselect(name: string): void
	ontabkeydown(event: KeyboardEvent): void
	opentab(next: Tab): void
	selected: string | null
	tab: Tab
	tablist: HTMLDivElement | null
}

let {
	apps,
	live,
	loading,
	now,
	onforget,
	onrefresh,
	onselect,
	ontabkeydown,
	opentab,
	selected,
	tab,
	tablist = $bindable(),
}: Props = $props()
</script>

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
		<button class="btn" disabled={loading} onclick={onrefresh} type="button">
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
							onclick={(): void => onselect(app.name)}
							type="button"
						>
							<span
								aria-hidden="true"
								class={app.status === "running"
									? "size-1.5 shrink-0 bg-primary-500"
									: "size-1.5 shrink-0 border border-neutral-500"}
							></span>
							<span class="flex-1 truncate">{app.name}</span>
							<span class="text-xs text-neutral-500 tabular-nums">{uptime(app.startedAt, now)}</span>
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
			bind:this={tablist}
		>
			<h2 class="px-3 py-2 text-xs font-semibold tracking-wide text-neutral-400 uppercase">Sections</h2>
			{#each tabs as name (name)}
				<button
					aria-controls="app-panel-{name}"
					aria-selected={tab === name}
					class={[
						"flex w-full items-center border-b border-l-2 border-line-soft px-3 py-1.5 text-left text-sm transition-colors last:border-b-0",
						tab === name
							? "cursor-default border-l-primary-500 bg-neutral-800/40 text-primary-300"
							: "border-l-transparent text-neutral-400 hover:bg-neutral-800/40 hover:text-neutral-200",
					]}
					id="app-tab-{name}"
					onclick={(): void => opentab(name)}
					onkeydown={ontabkeydown}
					role="tab"
					tabindex={tab === name ? 0 : -1}
					type="button"
				>
					{tabLabels[name]}
				</button>
			{/each}
		</div>
	{/if}
	<footer class="shrink-0 border-t border-line px-3 py-2">
		<button class="btn w-full" onclick={onforget} type="button">Forget token</button>
	</footer>
</aside>
