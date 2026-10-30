/** Human-readable uptime for an app started at the given time. */
export function uptime(startedAt: string, now: number): string {
	const seconds = Math.max(0, Math.floor((now - new Date(startedAt).getTime()) / 1000))
	if (seconds < 60) return `${seconds}s`
	const minutes = Math.floor(seconds / 60)
	if (minutes < 60) return `${minutes}m`
	const hours = Math.floor(minutes / 60)
	if (hours < 48) return `${hours}h ${minutes % 60}m`
	return `${Math.floor(hours / 24)}d ${hours % 24}h`
}

export function formatTime(value: string): string {
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

export function formatCPU(percent: number): string {
	// One decimal below 10%, none at or above it; whole values keep no trailing ".0".
	const value = percent < 10 ? Number(percent.toFixed(1)) : Math.round(percent)
	return `${value}%`
}

export function formatMemory(bytes: number): string {
	if (bytes <= 0) return "–"
	const megabytes = bytes / (1024 * 1024)
	return megabytes < 10 ? `${megabytes.toFixed(1)} MB` : `${Math.round(megabytes)} MB`
}

export function formatSize(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`
	const kilobytes = bytes / 1024
	if (kilobytes < 1024) return `${Math.round(kilobytes)} KB`
	return `${(kilobytes / 1024).toFixed(1)} MB`
}

const isBlobValue = (value: unknown): value is { $b64: string } =>
	typeof value === "object" && value !== null && "$b64" in value && typeof value.$b64 === "string"

export function formatDatabaseValue(value: unknown): string {
	if (value === null) return "–"
	if (typeof value === "string") return value
	if (typeof value === "number" || typeof value === "boolean") return String(value)
	if (typeof value === "bigint") return value.toString()
	if (typeof value === "symbol") return String(value)
	if (isBlobValue(value)) {
		const bytes = Math.floor((value.$b64.length * 3) / 4)
		return `blob (${formatSize(bytes)})`
	}
	return JSON.stringify(value) ?? "undefined"
}

export const formatChangeParams = (params: unknown[]): string => JSON.stringify(params) ?? "[]"

export const formatRowValue = (values: unknown[] | undefined, index: number): string =>
	values && index < values.length ? formatDatabaseValue(values[index]) : "–"

export function rowOpClass(op: string): string {
	switch (op) {
		case "insert":
			return "text-primary-300"
		case "update":
			return "text-amber-300"
		case "delete":
			return "text-red-300"
		default:
			return "text-neutral-400"
	}
}
