<script lang="ts">
import type { ResourceSample } from "./api"

interface Props {
	color: "primary" | "sky"
	format(value: number): string
	label: string
	samples: ResourceSample[]
	value(sample: ResourceSample): number
}

let { color, format, label, samples, value }: Props = $props()

const width = 100
const height = 40
const padding = 2

let values = $derived(samples.map(sample => value(sample)))
let current = $derived(values.at(-1) ?? 0)
let peak = $derived(values.length > 0 ? Math.max(...values) : 0)
let maxValue = $derived(Math.max(peak, 1))
let points = $derived.by(() => {
	const first = samples.at(0)
	const last = samples.at(-1)
	if (!first || !last) return []
	const firstAt = Date.parse(first.at)
	const span = Math.max(Date.parse(last.at) - firstAt, 1)
	return samples.map(sample => {
		const x = ((Date.parse(sample.at) - firstAt) / span) * (width - padding * 2) + padding
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
	const first = samples.at(0)
	const last = samples.at(-1)
	if (!first || !last) return ""
	const seconds = Math.round((Date.parse(last.at) - Date.parse(first.at)) / 1000)
	if (seconds <= 0) return "now"
	if (seconds < 60) return `${seconds}s`
	return `${Math.round(seconds / 60)} min`
})
let stroke = $derived(color === "primary" ? "stroke-primary-400" : "stroke-sky-400")
let fill = $derived(color === "primary" ? "fill-primary-500/10" : "fill-sky-500/10")
</script>

<div class="flex flex-col gap-1 border border-line-soft bg-surface p-2">
	<div class="flex items-baseline justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">{label}</h3>
		<span class="text-sm tabular-nums">{format(current)}</span>
	</div>
	{#if points.length === 0}
		<div class="flex h-24 items-center justify-center text-xs text-neutral-500">Collecting samples…</div>
	{:else}
		<svg
			aria-label={`${label} history`}
			class="h-24 w-full"
			preserveAspectRatio="none"
			role="img"
			viewBox={`0 0 ${width} ${height}`}
		>
			<title>{label} history</title>
			<line
				class="stroke-line-soft"
				vector-effect="non-scaling-stroke"
				x1={padding}
				x2={width - padding}
				y1={height / 2}
				y2={height / 2}
			></line>
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
		<div class="flex justify-between text-xs text-neutral-500">
			<span>{spanLabel}</span>
			<span>peak {format(peak)}</span>
		</div>
	{/if}
</div>
