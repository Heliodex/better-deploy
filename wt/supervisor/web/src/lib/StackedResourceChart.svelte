<script lang="ts">
import type { ResourceSample } from "./api"

interface Series {
	name: string
	samples: ResourceSample[]
}

interface Props {
	format(value: number): string
	label: string
	series: Series[]
	value(sample: ResourceSample): number
}

let { format, label, series, value }: Props = $props()

const width = 100
const height = 40
const padding = 2

interface PaletteEntry {
	dot: string
	fill: string
	stroke: string
}

// One entry per app, cycled when there are more apps than colours. Full literal
// class names so Tailwind emits them.
const palette: PaletteEntry[] = [
	{ dot: "bg-emerald-400", fill: "fill-emerald-500/25", stroke: "stroke-emerald-400" },
	{ dot: "bg-sky-400", fill: "fill-sky-500/25", stroke: "stroke-sky-400" },
	{ dot: "bg-amber-400", fill: "fill-amber-500/25", stroke: "stroke-amber-400" },
	{ dot: "bg-violet-400", fill: "fill-violet-500/25", stroke: "stroke-violet-400" },
	{ dot: "bg-rose-400", fill: "fill-rose-500/25", stroke: "stroke-rose-400" },
	{ dot: "bg-cyan-400", fill: "fill-cyan-500/25", stroke: "stroke-cyan-400" },
	{ dot: "bg-lime-400", fill: "fill-lime-500/25", stroke: "stroke-lime-400" },
	{ dot: "bg-orange-400", fill: "fill-orange-500/25", stroke: "stroke-orange-400" },
	{ dot: "bg-fuchsia-400", fill: "fill-fuchsia-500/25", stroke: "stroke-fuchsia-400" },
	{ dot: "bg-teal-400", fill: "fill-teal-500/25", stroke: "stroke-teal-400" },
]

let ordered = $derived([...series].toSorted((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)))
let colors = $derived(new Map(ordered.map((entry, index) => [entry.name, palette[index % palette.length]])))

const spans = [
	{ label: "1m", seconds: 60 },
	{ label: "5m", seconds: 300 },
	{ label: "15m", seconds: 900 },
	{ label: "30m", seconds: 1800 },
]

let span = $state(300)

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

let currents = $derived(
	visible.map(entry => {
		const { samples } = entry
		const last = samples.at(-1)
		return { current: last ? value(last) : 0, name: entry.name }
	}),
)
</script>

<div class="flex flex-col gap-1 border border-line-soft bg-surface p-2">
	<div class="flex items-baseline justify-between gap-2">
		<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase">{label}</h3>
		<select aria-label={`${label} timespan`} class="field px-1 py-0 text-xs" bind:value={span}>
			{#each spans as option (option.seconds)}
				<option value={option.seconds}>{option.label}</option>
			{/each}
		</select>
		<span class="text-sm tabular-nums">{format(currentTotal)}</span>
	</div>
	{#if stacked.length === 0}
		<div class="flex h-24 items-center justify-center text-xs text-neutral-500">Collecting samples…</div>
	{:else}
		<svg
			aria-label={`${label} history for all apps`}
			class="h-24 w-full"
			preserveAspectRatio="none"
			role="img"
			viewBox={`0 0 ${width} ${height}`}
		>
			<title>{label} history for all apps</title>
			<line
				class="stroke-line-soft"
				vector-effect="non-scaling-stroke"
				x1={padding}
				x2={width - padding}
				y1={height / 2}
				y2={height / 2}
			></line>
			{#each layers as layer (layer.entry.name)}
				{const color = colors.get(layer.entry.name)}
				<path class={color?.fill} d={layer.area}></path>
			{/each}
			{#each layers as layer (layer.entry.name)}
				{const color = colors.get(layer.entry.name)}
				<path class={color?.stroke} d={layer.line} fill="none" vector-effect="non-scaling-stroke"></path>
			{/each}
		</svg>
		<div class="flex justify-between text-xs text-neutral-500">
			<span>{spanLabel}</span>
			<span>peak {format(peakTotal)}</span>
		</div>
		<ul class="flex flex-col gap-1 pt-1">
			{#each currents as item (item.name)}
				{const color = colors.get(item.name)}
				<li class="flex items-center gap-2 text-xs">
					<span aria-hidden="true" class="size-2 shrink-0 {color?.dot}"></span>
					<span class="flex-1 truncate text-neutral-300">{item.name}</span>
					<span class="text-neutral-400 tabular-nums">{format(item.current)}</span>
				</li>
			{/each}
		</ul>
	{/if}
</div>
