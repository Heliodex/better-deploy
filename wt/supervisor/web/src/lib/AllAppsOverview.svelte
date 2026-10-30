<script lang="ts">
import type { ResourceSample } from "./api"
import { spans } from "./chart"
import { formatCPU, formatMemory } from "./format"
import StackedResourceChart from "./StackedResourceChart.svelte"

interface Series {
	name: string
	samples: ResourceSample[]
}

interface LegendEntry {
	color: { dot: string }
	cpu: number
	memory: number
	name: string
}

interface Props {
	legend: LegendEntry[]
	series: Series[]
	span: number
}

let { legend, series, span = $bindable() }: Props = $props()
</script>

<div class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
	<section aria-labelledby="overview-title" class="flex flex-col gap-2 border border-line-soft bg-surface p-2">
		<div class="flex items-center justify-between gap-2">
			<h2 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="overview-title">All apps</h2>
			<select aria-label="All apps timespan" class="field px-1 py-0 text-xs" bind:value={span}>
				{#each spans as option (option.seconds)}
					<option value={option.seconds}>{option.label}</option>
				{/each}
			</select>
		</div>
		<div class="flex flex-col gap-4">
			<StackedResourceChart
				format={formatCPU}
				label="CPU"
				{series}
				{span}
				unit={1}
				value={(sample: ResourceSample) => sample.cpuPercent}
			/>
			<StackedResourceChart
				format={formatMemory}
				label="Memory"
				{series}
				{span}
				unit={1024 * 1024}
				value={(sample: ResourceSample) => sample.memoryBytes}
			/>
		</div>
		<ul class="flex flex-col gap-1">
			<li aria-hidden="true" class="flex items-center gap-2 text-[10px] tracking-wide text-neutral-600 uppercase">
				<span class="size-2 shrink-0"></span>
				<span class="flex-1"></span>
				<span class="w-14 shrink-0 text-right">cpu</span>
				<span class="w-16 shrink-0 text-right">memory</span>
			</li>
			{#each legend as item (item.name)}
				<li class="flex items-center gap-2 text-xs">
					<span aria-hidden="true" class="size-2 shrink-0 {item.color.dot}"></span>
					<span class="flex-1 truncate text-neutral-300">{item.name}</span>
					<span class="w-14 shrink-0 text-right text-neutral-400 tabular-nums">{formatCPU(item.cpu)}</span>
					<span class="w-16 shrink-0 text-right text-neutral-400 tabular-nums"
						>{formatMemory(item.memory)}</span
					>
				</li>
			{/each}
		</ul>
	</section>
</div>
