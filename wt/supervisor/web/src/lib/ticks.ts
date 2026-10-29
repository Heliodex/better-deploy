/** Evenly spaced, human-rounded tick values from zero up to max. */
export function niceTicks(max: number, count: number): number[] {
	if (!(max > 0) || !(count > 0)) return [0]
	const rough = max / count
	const magnitude = 10 ** Math.floor(Math.log10(rough))
	const normalized = rough / magnitude
	const step = (normalized >= 5 ? 5 : normalized >= 2 ? 2 : 1) * magnitude
	const ticks: number[] = []
	for (let value = 0; value <= max * 1.001 && ticks.length <= count + 2; value += step)
		ticks.push(Math.round(value / magnitude) * magnitude)
	return ticks
}
