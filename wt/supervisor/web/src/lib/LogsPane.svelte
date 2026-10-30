<script lang="ts">
import { type LogEntry, type LogLevel, logLevels } from "./api"
import { accessStatusClass, levelClasses, levelMessageClasses, logTimeParts, parseAccessLine } from "./log-format"

interface Props {
	entries: LogEntry[]
	minLevel: LogLevel
	onminlevel(level: LogLevel): void
	onreload(lines: number): void
	onscroll(): void
	pane: HTMLDivElement | null
	tail: number
	total: number
}

let { entries, minLevel, onminlevel, onreload, onscroll, pane = $bindable(), tail, total }: Props = $props()

function changeLevel(event: Event): void {
	const { value } = event.currentTarget as HTMLSelectElement
	const level = logLevels.find(candidate => candidate === value)
	if (level) onminlevel(level)
}

function changeLines(event: Event): void {
	const { value } = event.currentTarget as HTMLSelectElement
	const lines = Number(value)
	if (Number.isFinite(lines)) onreload(lines)
}
</script>

<section
	aria-labelledby="logs-title"
	class="logs-pane flex min-h-0 flex-col border-t border-line bg-surface"
	id="logs-panel"
>
	<header class="flex shrink-0 items-center gap-2 border-b border-line-soft px-3 py-2">
		<h2 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="logs-title">Logs</h2>
		<span class="flex-1"></span>
		<select aria-label="Minimum level" class="field" onchange={changeLevel} value={minLevel}>
			<option value="trace">All levels</option>
			<option value="debug">Debug and above</option>
			<option value="info">Info and above</option>
			<option value="warn">Warn and above</option>
			<option value="error">Error and above</option>
			<option value="fatal">Fatal only</option>
		</select>
		<select aria-label="Lines to show" class="field" onchange={changeLines}>
			<option value={100}>100 lines</option>
			<option value={500}>500 lines</option>
			<option value={2000}>2000 lines</option>
		</select>
	</header>
	{#if entries.length === 0}
		<p class="p-3 text-sm text-neutral-500">
			{total === 0 ? "No log lines yet." : "No lines at this level."}
		</p>
	{:else}
		<div class="min-h-0 flex-1 overflow-auto bg-black p-2 text-xs" {onscroll} bind:this={pane}>
			<table class="w-full border-collapse">
				<tbody>
					{#each entries as entry, index (index)}
						{const timeParts = logTimeParts(entry.time)}
						{const access = entry.source === "supervisor" ? parseAccessLine(entry.message) : null}
						<tr class={entry.source === "supervisor" ? "bg-violet-500/5" : ""}>
							<td class="w-0 pr-3 align-top whitespace-nowrap text-neutral-500 tabular-nums">
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
							<td class="w-0 pr-3 align-top whitespace-nowrap {levelClasses[entry.level]}">
								{#if !access}
									{entry.level.toUpperCase()}
								{/if}
							</td>
							<td
								class="align-top whitespace-pre-wrap wrap-break-word {levelMessageClasses[entry.level]}"
							>
								{#if access}
									<span class="inline-flex flex-wrap items-baseline gap-x-2">
										<span class="inline-block w-14 shrink-0 font-mono text-neutral-200"
											>{access.method}</span
										>
										<span class="shrink-0 tabular-nums {accessStatusClass(access.status)}"
											>{access.status}</span
										>
										<span class="shrink-0 text-neutral-500 tabular-nums">{access.duration}</span>
										<span class="min-w-0 flex-1 wrap-break-word">{access.path}</span>
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

<style>
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
</style>
