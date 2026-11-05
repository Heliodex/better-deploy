<script lang="ts">
import type { ResourceSample } from "./api"
import { palette } from "./chart"
import { tickScale } from "./ticks"

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
/** Thirty minutes at the supervisor's one-second sampling interval. */
const MAX_POINTS = 1800

let ordered = $derived([...series].toSorted((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)))
let colors = $derived(new Map(ordered.map((entry, index) => [entry.name, palette[index % palette.length]])))

/** Per-series samples inside the selected timespan, cut from the newest sample across all apps. */
let visible = $derived.by(() => {
	const sorted = ordered.map(entry => ({
		name: entry.name,
		samples: entry.samples.toSorted((a, b) => Date.parse(a.at) - Date.parse(b.at)),
	}))

	let lastAt = Number.NaN
	for (const entry of sorted) {
		const last = entry.samples.at(-1)
		if (!last) continue
		const at = Date.parse(last.at)
		if (!Number.isNaN(at) && (Number.isNaN(lastAt) || at > lastAt)) lastAt = at
	}
	if (Number.isNaN(lastAt)) return sorted
	const cutoff = lastAt - span * 1000
	return sorted.map(entry => ({
		name: entry.name,
		samples: entry.samples.filter(sample => Date.parse(sample.at) >= cutoff),
	}))
})

let timeline = $derived.by(() => {
	const times = new Set<number>()
	for (const entry of visible)
		for (const sample of entry.samples) {
			const at = Date.parse(sample.at)
			if (!Number.isNaN(at)) times.add(at)
		}
	const sorted = [...times].toSorted((a, b) => a - b)
	// Apps are sampled at slightly different times, so the union can hold several points per second.
	// Thin it evenly to keep the whole span rather than truncating it to the newest points.
	if (sorted.length <= MAX_POINTS) return sorted
	const stride = (sorted.length - 1) / (MAX_POINTS - 1)
	return Array.from({ length: MAX_POINTS }, (_, index) => sorted[Math.round(index * stride)] ?? 0)
})

let stacked = $derived.by(() => {
	if (ordered.length === 0 || timeline.length === 0) return []
	const firstAt = timeline[0] ?? 0
	const lastAt = timeline.at(-1) ?? 0
	const range = Math.max(lastAt - firstAt, 1)
	const cursors = visible.map(() => 0)
	const lastValues = visible.map(() => 0)
	// Each series only owns its own sample times, so at a timestamp from another app it has no point.
	// Carry its last value forward instead of dropping it to zero, which would sawtooth the stack.
	return timeline.map(timestamp => {
		const x = ((timestamp - firstAt) / range) * (width - padding * 2) + padding
		for (const [index, entry] of visible.entries()) {
			let cursor = cursors[index] ?? 0
			while (cursor < entry.samples.length) {
				const sample = entry.samples[cursor]
				if (!sample || Date.parse(sample.at) > timestamp) break
				lastValues[index] = value(sample)
				cursor += 1
			}
			cursors[index] = cursor
		}
		return { values: [...lastValues], x }
	})
})

let totals = $derived(stacked.map(point => point.values.reduce((sum, current) => sum + current, 0)))
let currentTotal = $derived(totals.at(-1) ?? 0)
let peakTotal = $derived(totals.length > 0 ? Math.max(...totals) : 0)
/** Ticks picked in display units, so memory lands on round megabytes rather than round bytes. The domain ceiling is the top tick, so gridlines are evenly spaced to the top of the chart. */
let scale = $derived(tickScale(peakTotal / unit, 1))
let ticks = $derived(scale.ticks.map(tick => tick * unit))
let maxValue = $derived(Math.max(scale.max * unit, 1))

const yFor = (stackedValue: number): number => height - padding - (stackedValue / maxValue) * (height - padding * 2)

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
	if (first === undefined || last === undefined) return ""
	const seconds = Math.round((last - first) / 1000)
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
			<!-- Labels share the plot's y mapping so each sits centred on its gridline and the gaps between them stay even. -->
			<div aria-hidden="true" class="relative h-24 w-12 shrink-0 text-xs text-neutral-500">
				{#each ticks as tick (tick)}
					<span
						class="absolute right-0 -translate-y-1/2 tabular-nums"
						style={`top: ${(yFor(tick) / height) * 100}%`}
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
