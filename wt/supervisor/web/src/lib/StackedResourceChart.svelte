<script lang="ts">
import type { ResourceSample } from "./api"
import { palette } from "./chart"
import { niceTicks } from "./ticks"

interface Series {
	name: string
	samples: ResourceSample[]
}

interface Props {
	format(value: number): string
	label: string
	series: Series[]
	span: number
	/** Display units per raw value step, so ticks land on round labels (1 for %, 1 MiB for bytes). */
	unit: number
	value(sample: ResourceSample): number
}

let { format, label, series, span, unit, value }: Props = $props()

const width = 100
const height = 40
const padding = 2

let ordered = $derived([...series].toSorted((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)))
let colors = $derived(new Map(ordered.map((entry, index) => [entry.name, palette[index % palette.length]])))

/** Per-series samples inside the selected timespan, cut from the newest sample across all apps. */
let visible = $derived.by(() => {
	let lastAt = Number.NaN
	for (const entry of ordered) {
		const last = entry.samples.at(-1)
		if (!last) continue
		const at = Date.parse(last.at)
		if (!Number.isNaN(at) && (Number.isNaN(lastAt) || at > lastAt)) lastAt = at
	}
	if (Number.isNaN(lastAt)) return ordered
	const cutoff = lastAt - span * 1000
	return ordered.map(entry => ({
		name: entry.name,
		samples: entry.samples.filter(sample => Date.parse(sample.at) >= cutoff),
	}))
})

let timeline = $derived.by(() => {
	const times = new Set<string>()
	for (const entry of visible)
		for (const sample of entry.samples) if (!Number.isNaN(Date.parse(sample.at))) times.add(sample.at)

	// Thirty minutes at the supervisor's one-second sampling interval.
	return [...times].toSorted((a, b) => Date.parse(a) - Date.parse(b)).slice(-1800)
})

let byTime = $derived.by(() => {
	const maps = new Map<string, Map<string, number>>()
	for (const entry of visible) {
		const perSample = new Map<string, number>()
		for (const sample of entry.samples) perSample.set(sample.at, value(sample))
		maps.set(entry.name, perSample)
	}
	return maps
})

let stacked = $derived.by(() => {
	if (ordered.length === 0 || timeline.length === 0) return []
	const firstAt = Date.parse(timeline[0] ?? "")
	const lastAt = Date.parse(timeline.at(-1) ?? "")
	const range = Math.max(lastAt - firstAt, 1)
	return timeline.map(at => {
		const timestamp = Date.parse(at)
		const x = ((timestamp - firstAt) / range) * (width - padding * 2) + padding
		const values = visible.map(entry => byTime.get(entry.name)?.get(at) ?? 0)
		return { at, values, x }
	})
})

let totals = $derived(stacked.map(point => point.values.reduce((sum, current) => sum + current, 0)))
let currentTotal = $derived(totals.at(-1) ?? 0)
let peakTotal = $derived(totals.length > 0 ? Math.max(...totals) : 0)
let maxValue = $derived(Math.max(peakTotal, 1))
/** Ticks picked in display units, so memory lands on round megabytes rather than round bytes. */
let ticks = $derived(niceTicks(peakTotal / unit, 2).map(tick => tick * unit))

function yFor(stackedValue: number): number {
	return height - padding - (stackedValue / maxValue) * (height - padding * 2)
}

let layers = $derived(
	ordered.map((entry, index) => {
		const tops = stacked.map(point => {
			let sum = 0
			for (let i = 0; i <= index; i++) sum += point.values[i] ?? 0
			return { x: point.x, y: yFor(sum) }
		})
		const bottoms = stacked.map(point => {
			let sum = 0
			for (let i = 0; i < index; i++) sum += point.values[i] ?? 0
			return { x: point.x, y: yFor(sum) }
		})
		if (tops.length === 0) return { area: "", entry, line: "" }
		const topLine = tops
			.map((point, i) => `${i === 0 ? "M" : "L"}${point.x.toFixed(2)} ${point.y.toFixed(2)}`)
			.join(" ")
		const bottomLine = bottoms
			.toReversed()
			.map(point => `L${point.x.toFixed(2)} ${point.y.toFixed(2)}`)
			.join(" ")
		return { area: `${topLine} ${bottomLine} Z`, entry, line: topLine }
	}),
)

let spanLabel = $derived.by(() => {
	const first = timeline.at(0)
	const last = timeline.at(-1)
	if (!first || !last) return ""
	const seconds = Math.round((Date.parse(last) - Date.parse(first)) / 1000)
	if (seconds <= 0) return "now"
	if (seconds < 60) return `${seconds}s`
	return `${Math.round(seconds / 60)} min`
})
</script>

<div class="flex flex-col gap-1">
	<div class="flex items-baseline justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">{label}</h3>
		<span class="text-sm tabular-nums">{format(currentTotal)}</span>
	</div>
	{#if stacked.length === 0}
		<div class="flex h-24 items-center justify-center text-xs text-neutral-500">Collecting samples…</div>
	{:else}
		<div class="flex gap-1.5">
			<div aria-hidden="true" class="relative h-24 w-12 shrink-0 text-xs text-neutral-500">
				{#each ticks as tick (tick)}
					{const percent = maxValue > 0 ? (tick / maxValue) * 100 : 0}
					{const placement = percent <= 8 ? "bottom" : percent >= 92 ? "top" : "middle"}
					<span
						class="absolute right-0 tabular-nums {placement === "bottom"
							? "bottom-0"
							: placement === "top"
								? "top-0"
								: "-translate-y-1/2"}"
						style={placement === "middle" ? `bottom: ${percent}%` : ""}
					>
						{format(tick)}
					</span>
				{/each}
			</div>
			<svg
				aria-label={`${label} history for all apps`}
				class="h-24 min-w-0 flex-1"
				preserveAspectRatio="none"
				role="img"
				viewBox={`0 0 ${width} ${height}`}
			>
				<title>{label} history for all apps</title>
				{#each ticks as tick (tick)}
					<line
						class="stroke-line-soft"
						vector-effect="non-scaling-stroke"
						x1={padding}
						x2={width - padding}
						y1={yFor(tick)}
						y2={yFor(tick)}
					></line>
				{/each}
				{#each layers as layer (layer.entry.name)}
					{const color = colors.get(layer.entry.name)}
					<path class={color?.fill} d={layer.area}></path>
				{/each}
				{#each layers as layer (layer.entry.name)}
					{const color = colors.get(layer.entry.name)}
					<path class={color?.stroke} d={layer.line} fill="none" vector-effect="non-scaling-stroke"></path>
				{/each}
			</svg>
		</div>
		<div class="flex justify-between text-xs text-neutral-500">
			<span>{spanLabel}</span>
			<span>peak {format(peakTotal)}</span>
		</div>
	{/if}
</div>
