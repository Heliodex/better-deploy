<script lang="ts">
import { untrack } from "svelte"
import type { BackupInfo } from "./api"
import { BACKUP_NODE_SIZE, backupGraph, backupTitle } from "./backup-tree"

let {
	backups,
	current,
	onselect,
	selected,
}: {
	backups: BackupInfo[]
	current: string
	onselect(name: string): void
	selected: string | null
} = $props()

// The tree is drawn as dots on a pannable, zoomable canvas.
const NODE = BACKUP_NODE_SIZE
const MIN_SCALE = 0.3
const MAX_SCALE = 2
// Keep at least this much of the graph on screen, so panning can't lose every node.
const PAN_MARGIN = 64

const graph = $derived(backupGraph(backups, current))
const currentBackup = $derived(backups.find(backup => backup.name === current) ?? null)

let canvasEl = $state<HTMLDivElement>()
// The view glides to its target through a CSS transform transition (disabled while dragging).
let view = $state({ scale: 1, x: 0, y: 0 })
let dragging = $state<{
	pointerId: number
	startX: number
	startY: number
	originX: number
	originY: number
	moved: boolean
} | null>(null)
// Set while a drag finishes, so the click it emits doesn't also select a node.
let suppressClick = false
let hovered = $state<{ label: string; x: number; y: number } | null>(null)

/** Clamp a view so the graph can't be panned fully off-screen. */
function clampView(next: { x: number; y: number; scale: number }): { x: number; y: number; scale: number } {
	const el = canvasEl
	if (!el) return next
	const limit = (value: number, extent: number, viewport: number): number => {
		const low = Math.min(PAN_MARGIN - extent, viewport - PAN_MARGIN)
		const high = Math.max(PAN_MARGIN - extent, viewport - PAN_MARGIN)
		return Math.min(high, Math.max(low, value))
	}
	return {
		scale: next.scale,
		x: limit(next.x, graph.width * next.scale, el.clientWidth),
		y: limit(next.y, graph.height * next.scale, el.clientHeight),
	}
}

/** Centre the viewport on a node, falling back to the current one. */
function centreOn(name: string | null): void {
	const el = canvasEl
	const node = graph.nodes.find(item => item.id === (name ?? current))
	if (!el || !node) return
	const rect = el.getBoundingClientRect()
	const { scale } = view
	view = clampView({
		scale,
		x: rect.width / 2 - node.x * scale,
		y: rect.height / 2 - node.y * scale,
	})
}

// Keep the current node centred as the tree grows or a restore moves the live database.
$effect(() => {
	const el = canvasEl
	const node = graph.nodes.find(item => item.id === current)
	if (!el || !node) return
	const scale = untrack(() => view.scale)
	const rect = el.getBoundingClientRect()
	view = clampView({
		scale,
		x: rect.width / 2 - node.x * scale,
		y: rect.height / 2 - node.y * scale,
	})
})

function startPan(event: PointerEvent): void {
	if (event.button !== 0) return
	// Pan only from the empty canvas, so pressing a node still delivers its click.
	if ((event.target as HTMLElement).closest("button")) return
	const el = event.currentTarget as HTMLElement
	dragging = {
		moved: false,
		originX: view.x,
		originY: view.y,
		pointerId: event.pointerId,
		startX: event.clientX,
		startY: event.clientY,
	}
	try {
		el.setPointerCapture(event.pointerId)
	} catch {
		// A pointer that is already released can't be captured; dragging still works without it.
	}
}

function movePan(event: PointerEvent): void {
	const state = dragging
	if (!state || event.pointerId !== state.pointerId) return
	const dx = event.clientX - state.startX
	const dy = event.clientY - state.startY
	if (Math.abs(dx) > 3 || Math.abs(dy) > 3) state.moved = true
	view = clampView({
		...view,
		x: state.originX + dx,
		y: state.originY + dy,
	})
}

function endPan(event: PointerEvent): void {
	const state = dragging
	if (!state || event.pointerId !== state.pointerId) return
	const el = event.currentTarget as HTMLElement
	if (el.hasPointerCapture(event.pointerId)) el.releasePointerCapture(event.pointerId)
	dragging = null
	if (!state.moved) return
	suppressClick = true
	setTimeout(() => (suppressClick = false), 0)
}

function handleNodeClick(id: string): void {
	if (suppressClick) return
	onselect(id)
}

function showLabel(label: string, event: PointerEvent): void {
	hovered = { label, x: event.clientX, y: event.clientY }
}

/** Wheel-to-zoom around the cursor; an attachment keeps the listener non-passive. */
function canvasGestures(node: HTMLDivElement): () => void {
	const onWheel = (event: WheelEvent): void => {
		event.preventDefault()
		const rect = node.getBoundingClientRect()
		const currentView = view
		const scale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, currentView.scale * Math.exp(-event.deltaY * 0.0015)))
		const cursorX = event.clientX - rect.left
		const cursorY = event.clientY - rect.top
		const worldX = (cursorX - currentView.x) / currentView.scale
		const worldY = (cursorY - currentView.y) / currentView.scale
		view = clampView({
			scale,
			x: cursorX - worldX * scale,
			y: cursorY - worldY * scale,
		})
	}
	node.addEventListener("wheel", onWheel, { passive: false })
	return (): void => node.removeEventListener("wheel", onWheel)
}
</script>

<div class="flex flex-col gap-2">
	<div class="flex items-center justify-between gap-2">
		<p class="text-xs text-neutral-500">
			Each node is a snapshot. The filled node is the one the live database follows.
		</p>
		<button
			class="btn px-1.5 py-0.5 text-xs"
			onclick={(): void => centreOn(selected)}
			title="Centre on the selected snapshot"
			type="button"
		>
			Centre
		</button>
	</div>
	<div
		aria-label="Backup tree. Drag to pan, scroll to zoom, and activate a snapshot to see its details."
		class="relative h-52 touch-none overflow-hidden border border-line-soft bg-black select-none {dragging
			? "cursor-grabbing"
			: "cursor-grab"}"
		onpointercancel={endPan}
		onpointerdown={startPan}
		onpointermove={movePan}
		onpointerup={endPan}
		role="application"
		bind:this={canvasEl}
		{@attach canvasGestures}
	>
		<div
			class={[
				"absolute top-0 left-0 origin-top-left motion-reduce:transition-none",
				dragging ? "transition-none" : "transition-transform duration-200 ease-out",
			]}
			style:transform="translate({view.x}px, {view.y}px) scale({view.scale})"
		>
			<svg
				aria-hidden="true"
				class="pointer-events-none absolute top-0 left-0 overflow-visible"
				height={graph.height}
				width={graph.width}
			>
				{#each graph.edges as edge (`${edge.from}→${edge.to}`)}
					<path
						class={edge.onPath ? "stroke-primary-500" : "stroke-neutral-600"}
						d={edge.d}
						fill="none"
						stroke-linecap="round"
						stroke-width="1.5"
					/>
				{/each}
			</svg>
			{#each graph.nodes as node (node.id)}
				<button
					aria-label={node.title}
					aria-pressed={node.id === selected}
					class={[
						"absolute rounded-full border transition-colors",
						node.current
							? "border-primary-300 bg-primary-500"
							: node.id === selected
								? "border-primary-300 bg-neutral-700"
								: node.onPath
									? "border-neutral-300 bg-neutral-400"
									: node.redoable
										? "border-neutral-400 bg-neutral-600"
										: "border-neutral-700 bg-neutral-800 hover:border-neutral-500",
					]}
					onclick={(): void => handleNodeClick(node.id)}
					onpointerenter={(event): void => showLabel(node.title, event)}
					onpointerleave={(): void => {
						hovered = null
					}}
					onpointermove={(event): void => showLabel(node.title, event)}
					type="button"
					style:height="{NODE}px"
					style:left="{node.x - NODE / 2}px"
					style:top="{node.y - NODE / 2}px"
					style:width="{NODE}px"
				>
					{#if node.showLabel}
						<span
							aria-hidden="true"
							class="pointer-events-none absolute top-1/2 left-full ml-1.5 -translate-y-1/2 whitespace-nowrap text-xs {node.current
								? "text-primary-300"
								: node.id === selected
									? "text-primary-200"
									: node.onPath
										? "text-neutral-300"
										: "text-neutral-500"}"
						>
							{node.label}
						</span>
					{/if}
				</button>
			{/each}
		</div>
		{#if currentBackup}
			<p
				class="pointer-events-none absolute bottom-1 left-1 max-w-[calc(100%-0.5rem)] truncate border border-line bg-black/80 px-1.5 py-0.5 text-xs text-neutral-300"
			>
				Current: {backupTitle(currentBackup)}
			</p>
		{/if}
	</div>
	{#if hovered}
		<div
			class="pointer-events-none fixed z-50 border border-line bg-surface-raised px-1.5 py-0.5 text-xs whitespace-nowrap text-neutral-200 shadow-lg shadow-black/50"
			style:left="{hovered.x + 12}px"
			style:top="{hovered.y + 12}px"
		>
			{hovered.label}
		</div>
	{/if}
</div>
