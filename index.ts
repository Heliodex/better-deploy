type DurableObjectId = unknown

interface DurableObjectStub {
	fetch(request: Request): Promise<Response>
}

interface DurableObjectNamespace {
	get(id: DurableObjectId): DurableObjectStub
	idFromName(name: string): DurableObjectId
}

interface DurableObjectStorage {
	get<T>(key: string): Promise<T | undefined>
	put<T>(key: string, value: T): Promise<void>
}

interface DurableObjectState {
	storage: DurableObjectStorage
}

interface Env {
	COUNTER: DurableObjectNamespace
}

export class Counter {
	private readonly state: DurableObjectState

	constructor(state: DurableObjectState, _env: Env) {
		this.state = state
	}

	async fetch(request: Request): Promise<Response> {
		let n = (await this.state.storage.get<number>("n")) ?? 0
		n++
		await this.state.storage.put("n", n)
		return Response.json({ n, url: request.url })
	}
}

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const name = new URL(request.url).searchParams.get("name") ?? "default"
		const id = env.COUNTER.idFromName(name)
		return env.COUNTER.get(id).fetch(request)
	},
}
