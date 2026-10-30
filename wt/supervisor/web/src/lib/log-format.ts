import { type LogLevel, logLevels } from "./api"

/** One supervisor access line, parsed from its `METHOD status duration path` shape. */
export interface AccessLine {
	duration: string
	method: string
	path: string
	status: number
}

const accessLinePattern = /^([A-Za-z]+) (\d{3}) (\S+) (.+)$/

export function parseAccessLine(message: string): AccessLine | null {
	const match = accessLinePattern.exec(message)
	if (!match) return null
	const [, method, status, duration, path] = match
	if (!method || !status || !duration || !path) return null
	return { duration, method, path, status: Number(status) }
}

export function accessStatusClass(status: number): string {
	if (status < 300) return "text-primary-300"
	if (status < 400) return "text-sky-300"
	if (status < 500) return "text-amber-300"
	return "text-red-300"
}

const logTimeFormat = new Intl.DateTimeFormat(undefined, {
	hour: "2-digit",
	hour12: false,
	minute: "2-digit",
	second: "2-digit",
})

// Part-by-part so the panel can give each segment the same width even though Lexend's digits are proportional.
export function logTimeParts(value: string): string[] {
	if (value === "") return ["—"]
	const date = new Date(value)
	if (Number.isNaN(date.getTime())) return [value]
	const parts = logTimeFormat.formatToParts(date)
	const pick = (type: string): string => parts.find(part => part.type === type)?.value ?? ""
	return [pick("hour"), pick("minute"), pick("second")]
}

export const levelClasses: { [_: string]: string } = {
	debug: "text-neutral-500",
	error: "text-red-400",
	fatal: "font-semibold text-red-500",
	info: "text-sky-400",
	trace: "text-neutral-600",
	warn: "text-amber-400",
}

export const levelMessageClasses: { [_: string]: string } = {
	debug: "text-neutral-300",
	error: "text-red-200",
	fatal: "text-red-200",
	info: "text-sky-300",
	trace: "text-neutral-400",
	warn: "text-amber-200",
}

export function levelIndex(level: LogLevel): number {
	return logLevels.indexOf(level)
}
