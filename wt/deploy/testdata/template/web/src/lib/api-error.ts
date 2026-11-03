// ApiError carries the stable code and optional field from the standard error body.
export class ApiError extends Error {
	readonly code: string
	readonly field: string

	constructor(message: string, code = "", field = "") {
		super(message)
		this.name = "ApiError"
		this.code = code
		this.field = field
	}
}
