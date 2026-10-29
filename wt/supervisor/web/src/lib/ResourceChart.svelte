<script lang="ts">
import type { ResourceSample } from "./api"
import { niceTicks } from "./ticks"

interface Props {
	color: "primary" | "sky"
	format(value: number): string
	label: string
	samples: ResourceSample[]
	span: number
	/** Display units per raw value step, so ticks land on round labels (1 for %, 1 MiB for bytes). */
	unit: number
	value(sample: ResourceSample): number
}

let { color, format, label, samples, span, unit, value }: Props = $props()

const width = 100
const height = 40
const padding = 2

/** Samples inside the selected timespan, cut from the newest sample so the chart never empties while data exists. */
let visible = $derived.by(() => {
	const last = samples.at(-1)
	if (!last) return []
	const lastAt = Date.parse(last.at)
	if (Number.isNaN(lastAt)) return samples
	const cutoff = lastAt - span * 1000
	return samples.filter(sample => Date.parse(sample.at) >= cutoff)
})

let values = $derived(visible.map(sample => value(sample)))
let current = $derived(values.at(-1) ?? 0)
let peak = $derived(values.length > 0 ? Math.max(...values) : 0)
let maxValue = $derived(Math.max(peak, 1))
/** Ticks picked in display units, so memory lands on round megabytes rather than round bytes. */
let ticks = $derived(niceTicks(peak / unit, 2).map(tick => tick * unit))
let points = $derived.by(() => {
	const first = visible.at(0)
	const last = visible.at(-1)
	if (!first || !last) return []
	const firstAt = Date.parse(first.at)
	const range = Math.max(Date.parse(last.at) - firstAt, 1)
	return visible.map(sample => {
		const x = ((Date.parse(sample.at) - firstAt) / range) * (width - padding * 2) + padding
		const y = height - padding - (value(sample) / maxValue) * (height - padding * 2)
		return { x, y }
	})
})
let line = $derived(
	points.map((point, index) => `${index === 0 ? "M" : "L"}${point.x.toFixed(2)} ${point.y.toFixed(2)}`).join(" "),
)
let area = $derived.by(() => {
	const first = points.at(0)
	const last = points.at(-1)
	if (!first || !last) return ""
	return `${line} L ${last.x.toFixed(2)} ${height - padding} L ${first.x.toFixed(2)} ${height - padding} Z`
})
let marker = $derived(points.length === 1 ? (points.at(0) ?? null) : null)
let spanLabel = $derived.by(() => {
	const first = visible.at(0)
	const last = visible.at(-1)
	if (!first || !last) return ""
	const seconds = Math.round((Date.parse(last.at) - Date.parse(first.at)) / 1000)
	if (seconds <= 0) return "now"
	if (seconds < 60) return `${seconds}s`
	return `${Math.round(seconds / 60)} min`
})
let stroke = $derived(color === "primary" ? "stroke-primary-400" : "stroke-sky-400")
let fill = $derived(color === "primary" ? "fill-primary-500/10" : "fill-sky-500/10")
</script>

<div class="flex flex-col gap-1">
	<div class="flex items-baseline justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">{label}</h3>
		<span class="text-sm tabular-nums">{format(current)}</span>
	</div>
	{#if points.length === 0}
		<div class="flex h-24 items-center justify-center text-xs text-neutral-500">Collecting samples…</div>
	{:else}
		<div class="flex gap-1.5">
			<div aria-hidden="true" class="relative h-24 w-12 shrink-0 text-xs text-neutral-500">
				{#each ticks as tick (tick)}
					{const percent = maxValue > 0 ? (tick / maxValue) * 100 : 0}
					<span
						class="absolute right-0 tabular-nums {tick === 0
							? "bottom-0"
							: tick >= maxValue
								? "top-0"
								: "-translate-y-1/2"}"
						style={tick === 0 || tick >= maxValue ? "" : `bottom: ${percent}%`}
					>
						{format(tick)}
					</span>
				{/each}
			</div>
			<svg
				aria-label={`${label} history`}
				class="h-24 min-w-0 flex-1"
				preserveAspectRatio="none"
				role="img"
				viewBox={`0 0 ${width} ${height}`}
			>
				<title>{label} history</title>
				{#each ticks as tick (tick)}
					<line
						class="stroke-line-soft"
						vector-effect="non-scaling-stroke"
						x1={padding}
						x2={width - padding}
						y1={height - padding - (tick / maxValue) * (height - padding * 2)}
						y2={height - padding - (tick / maxValue) * (height - padding * 2)}
					></line>
				{/each}
				<path class={fill} d={area}></path>
				<path class={stroke} d={line} fill="none" vector-effect="non-scaling-stroke"></path>
				{#if marker}
					<line
						class={stroke}
						stroke-linecap="round"
						vector-effect="non-scaling-stroke"
						x1={marker.x}
						x2={marker.x}
						y1={marker.y - 1}
						y2={marker.y + 1}
					></line>
				{/if}
			</svg>
		</div>
		<div class="flex justify-between text-xs text-neutral-500">
			<span>{spanLabel}</span>
			<span>peak {format(peak)}</span>
		</div>
	{/if}
</div>
