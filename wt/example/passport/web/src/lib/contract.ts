import { array, boolean, maxLength, minLength, object, optional, pipe, regex, string, title } from "valibot"

// These schemas are the source of truth for the generated Go contract. The client trims before validating and the server trims before storing, so leading and trailing spaces are not part of the contract.
export const maxMessage = 280

const nonBlank = regex(/\S/, "Write a few words.")

// Request schemas describe the JSON bodies the client sends.
export const callbackRequestSchema = pipe(
	object({
		code: pipe(string(), minLength(1)),
		state: pipe(string(), minLength(1)),
	}),
	title("CallbackRequest"),
)

export const createCheckinSchema = pipe(
	object({
		message: pipe(
			string(),
			minLength(1, "Write a few words."),
			maxLength(maxMessage, `Check-ins are limited to ${maxMessage} characters.`),
			nonBlank,
		),
	}),
	title("CreateCheckinRequest"),
)

// Response schemas describe what the API returns. The client parses every response through these schemas, and the generated Go types keep the handlers in step.
export const checkinSchema = pipe(
	object({
		authorName: pipe(string(), minLength(1)),
		authorPicture: string(),
		authorSub: pipe(string(), minLength(1)),
		createdAt: pipe(string(), minLength(1)),
		id: pipe(string(), minLength(1)),
		message: pipe(string(), minLength(1)),
		mine: boolean(),
	}),
	title("Checkin"),
)

export const checkinsResponseSchema = pipe(object({ checkins: array(checkinSchema) }), title("CheckinsResponse"))

export const sessionResponseSchema = pipe(
	object({
		authenticated: boolean(),
		clientId: pipe(string(), minLength(1)),
		expiresAt: string(),
		providerIssuer: pipe(string(), minLength(1)),
		providerName: pipe(string(), minLength(1)),
		scopes: array(string()),
		userEmail: string(),
		userName: string(),
		userPicture: string(),
		userSub: string(),
		userUsername: string(),
		userZoneinfo: string(),
	}),
	title("SessionResponse"),
)

export const logoutResponseSchema = pipe(object({ endSessionUrl: string() }), title("LogoutResponse"))

export const errorResponseSchema = pipe(
	object({ code: pipe(string(), minLength(1)), error: pipe(string(), minLength(1)), field: optional(string()) }),
	title("ErrorResponse"),
)

// Registries of endpoint contracts. Add new request and response schemas here.
export const requestSchemas = Object.freeze([callbackRequestSchema, createCheckinSchema])

export const responseSchemas = Object.freeze([
	checkinSchema,
	checkinsResponseSchema,
	sessionResponseSchema,
	logoutResponseSchema,
	errorResponseSchema,
])
