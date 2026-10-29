export interface TickScale {
	/** Domain maximum: the top tick, always at or above the data peak. */
	max: number
	/** Human-rounded tick values from zero up to max. */
	ticks: number[]
}

/** Evenly spaced, human-rounded ticks from zero covering max, so the top gridline is the top of the chart. */
export function tickScale(max: number, count: number): TickScale {
	if (!(max > 0) || !(count > 0)) return { max: 0, ticks: [0] }
	const rough = max / count
	const magnitude = 10 ** Math.floor(Math.log10(rough))
	const normalized = rough / magnitude
	const step = (normalized >= 5 ? 5 : normalized >= 2 ? 2 : 1) * magnitude
	const ticks: number[] = []
	for (let value = 0; value <= max * 1.001 && ticks.length <= count + 2; value += step)
		ticks.push(Math.round(value / magnitude) * magnitude)
	const top = ticks.at(-1) ?? 0
	if (top < max) {
		const rounded = Math.round((top + step) / magnitude) * magnitude
		ticks.push(rounded)
		return { max: rounded, ticks }
	}
	return { max: top, ticks }
}
