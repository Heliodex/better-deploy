export interface PaletteEntry {
	dot: string
	fill: string
	stroke: string
}

// One entry per app, cycled when there are more apps than colours. Full literal
// class names so Tailwind emits them.
export const palette: PaletteEntry[] = [
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

export const spans = [
	{ label: "1m", seconds: 60 },
	{ label: "5m", seconds: 300 },
	{ label: "15m", seconds: 900 },
	{ label: "30m", seconds: 1800 },
]
