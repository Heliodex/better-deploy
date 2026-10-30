<script lang="ts">
import { type LogEntry, type LogLevel, logLevels } from "./api"
import { formatTime } from "./format"
import { accessStatusClass, levelClasses, levelMessageClasses, logTimeParts, parseAccessLine } from "./log-format"

interface Props {
	entries: LogEntry[]
	minLevel: LogLevel
	onminlevel(level: LogLevel): void
	onreload(lines: number): void
	onscroll(): void
	pane: HTMLDivElement | null
	query: string
	tail: number
	total: number
}

let {
	entries,
	minLevel,
	onminlevel,
	onreload,
	onscroll,
	pane = $bindable(),
	query = $bindable(),
	tail,
	total,
}: Props = $props()

const ROW_HEIGHT = 24
const OVERSCAN = 12

let scrollTop = $state(0)
let selected = $state<LogEntry | null>(null)
let viewportHeight = $state(0)
let start = $derived(
	Math.min(Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN), Math.max(0, entries.length - 1)),
)
let end = $derived(
	Math.max(start, Math.min(entries.length, Math.ceil((scrollTop + viewportHeight) / ROW_HEIGHT) + OVERSCAN)),
)
let visible = $derived(entries.slice(start, end))
let topHeight = $derived(start * ROW_HEIGHT)
let bottomHeight = $derived((entries.length - end) * ROW_HEIGHT)

$effect(() => {
	const target = pane
	if (!target) return
	viewportHeight = target.clientHeight
	const observer = new ResizeObserver(() => {
		viewportHeight = target.clientHeight
	})
	observer.observe(target)
	return () => observer.disconnect()
})

function handleScroll(event: Event): void {
	const { currentTarget } = event
	const target = currentTarget as HTMLDivElement
	const { clientHeight, scrollTop: nextScrollTop } = target
	scrollTop = nextScrollTop
	viewportHeight = clientHeight
	onscroll()
}

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

function selectRow(entry: LogEntry): void {
	selected = selected === entry ? null : entry
}

function rowKey(event: KeyboardEvent, entry: LogEntry): void {
	if (event.key !== "Enter" && event.key !== " ") return
	event.preventDefault()
	selectRow(entry)
}
</script>

<section
	aria-labelledby="logs-title"
	class="logs-pane flex min-h-0 flex-col border-t border-line bg-surface"
	id="logs-panel"
>
	<header class="flex shrink-0 items-center gap-2 border-b border-line-soft px-3 py-2">
		<h2 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="logs-title">Logs</h2>
		<input
			aria-label="Filter logs"
			class="field min-w-0 max-w-64 flex-1"
			placeholder="Filter logs"
			bind:value={query}
		>
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
		<span class="text-[10px] text-neutral-500">Click a line to expand</span>
	</header>
	{#if entries.length === 0}
		<p class="p-3 text-sm text-neutral-500">
			{total === 0
				? "No log lines yet."
				: query.trim() !== ""
					? "No lines match this filter."
					: "No lines at this level."}
		</p>
	{:else}
		<div class="min-h-0 flex-1 overflow-auto bg-black p-1 text-xs" onscroll={handleScroll} bind:this={pane}>
			<table class="w-full table-fixed border-collapse">
				<colgroup>
					<col class="w-16">
					<col class="w-14">
					<col>
				</colgroup>
				<tbody>
					{#if topHeight > 0}
						<tr aria-hidden="true" style:height={`${topHeight}px`}>
							<td colspan="3" style:height={`${topHeight}px`}></td>
						</tr>
					{/if}
					{#each visible as entry, index (start + index)}
						{const timeParts = logTimeParts(entry.time)}
						{const access = entry.source === "supervisor" ? parseAccessLine(entry.message) : null}
						<tr
							aria-label={`${entry.level} ${entry.message}`}
							class={[
								"h-6 cursor-pointer",
								entry.source === "supervisor" ? "bg-violet-500/5" : "",
								selected === entry ? "bg-neutral-700/70" : "hover:bg-neutral-800/60",
							]}
							onclick={(): void => selectRow(entry)}
							onkeydown={(event): void => rowKey(event, entry)}
							role="button"
							tabindex="0"
							title={`${entry.message}

Click to expand`}
							style:height={`${ROW_HEIGHT}px`}
						>
							<td class="pr-1 align-middle whitespace-nowrap text-neutral-500 tabular-nums">
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
							<td class="pr-1 align-middle whitespace-nowrap">
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
							<td class="overflow-hidden align-middle {levelMessageClasses[entry.level]}">
								{#if access}
									<span class="flex min-w-0 items-baseline gap-x-2">
										<span class="w-14 shrink-0 font-mono text-neutral-200">{access.method}</span>
										<span class="shrink-0 tabular-nums {accessStatusClass(access.status)}"
											>{access.status}</span
										>
										<span class="shrink-0 text-neutral-500 tabular-nums">{access.duration}</span>
										<span class="min-w-0 flex-1 truncate">{access.path}</span>
									</span>
								{:else}
									<span class="flex min-w-0 items-baseline gap-x-2">
										<span class="w-14 shrink-0 {levelClasses[entry.level]}"
											>{entry.level.toUpperCase()}</span
										>
										<span class="min-w-0 flex-1 truncate">{entry.message}</span>
									</span>
								{/if}
							</td>
						</tr>
					{/each}
					{#if bottomHeight > 0}
						<tr aria-hidden="true" style:height={`${bottomHeight}px`}>
							<td colspan="3" style:height={`${bottomHeight}px`}></td>
						</tr>
					{/if}
				</tbody>
			</table>
		</div>
		{#if selected}
			<div class="shrink-0 border-t border-line-soft bg-neutral-900/60 px-3 py-2 text-xs">
				<div class="flex flex-wrap items-center gap-2">
					<time class="text-neutral-500 tabular-nums" datetime={selected.time}>{formatTime(selected.time)}</time>
					<span class="border border-neutral-700 px-1 text-[10px] tracking-wide text-neutral-400 uppercase">
						{selected.source === "supervisor" ? "proxy" : "app"}
					</span>
					<span class="text-[10px] tracking-wide uppercase {levelClasses[selected.level]}">
						{selected.level}
					</span>
					<span class="flex-1"></span>
					<button class="btn" onclick={(): null => (selected = null)} type="button">Close</button>
				</div>
				<pre class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap wrap-break-word text-neutral-200">{selected.message}</pre>
			</div>
		{/if}
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
