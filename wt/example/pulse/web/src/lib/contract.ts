import {
	array,
	boolean,
	integer,
	maxLength,
	maxValue,
	minLength,
	minValue,
	number,
	object,
	optional,
	pipe,
	regex,
	string,
	title,
} from "valibot"

// These schemas are the source of truth for the generated Go contract. The client validates requests before fetch and every response through the same schemas.
export const maxName = 60
export const maxURL = 500

const nonBlank = regex(/\S/, "This field cannot be blank.")

export const createMonitorSchema = pipe(
	object({
		name: pipe(
			string(),
			minLength(1, "Please enter a name."),
			maxLength(maxName, `Names are limited to ${maxName} characters.`),
			nonBlank,
		),
		url: pipe(
			string(),
			minLength(1, "Please enter a URL."),
			maxLength(maxURL, `URLs are limited to ${maxURL} characters.`),
			regex(/^https?:\/\/\S+$/, "Enter a valid http or https URL."),
		),
	}),
	title("CreateMonitorRequest"),
)

const count = pipe(number(), integer(), minValue(0))

export const checkSchema = pipe(
	object({
		checkedAt: pipe(string(), minLength(1)),
		error: string(),
		latencyMs: count,
		ok: boolean(),
		status: pipe(number(), integer(), minValue(0), maxValue(599)),
	}),
	title("Check"),
)

export const monitorSchema = pipe(
	object({
		createdAt: pipe(string(), minLength(1)),
		history: array(checkSchema),
		id: pipe(string(), minLength(1)),
		last: optional(checkSchema),
		name: pipe(string(), minLength(1)),
		uptime: pipe(number(), minValue(0), maxValue(100)),
		url: pipe(string(), minLength(1)),
	}),
	title("Monitor"),
)

export const monitorsResponseSchema = pipe(object({ monitors: array(monitorSchema) }), title("MonitorsResponse"))

export const statsSchema = pipe(
	object({
		avgLatencyMs: count,
		checks24h: count,
		down: count,
		lastCheckAt: string(),
		monitors: count,
		up: count,
	}),
	title("Stats"),
)

export const cycleResultSchema = pipe(
	object({
		at: pipe(string(), minLength(1)),
		checked: count,
		down: count,
		up: count,
	}),
	title("CycleResult"),
)

export const errorResponseSchema = pipe(
	object({ code: pipe(string(), minLength(1)), error: pipe(string(), minLength(1)), field: optional(string()) }),
	title("ErrorResponse"),
)

// Registries of endpoint contracts. Add new request and response schemas here.
export const requestSchemas = Object.freeze([createMonitorSchema])

export const responseSchemas = Object.freeze([
	checkSchema,
	monitorSchema,
	monitorsResponseSchema,
	statsSchema,
	cycleResultSchema,
	errorResponseSchema,
])
