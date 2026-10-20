interface Env {
	COUNTER: DurableObjectNamespace
}

export class Counter {
	constructor(
		private readonly state: DurableObjectState,
		_env: Env,
	) {}

	async fetch(request: Request): Promise<Response> {
		let n = (await this.state.storage.get<number>("n")) ?? 0
		n++
		await this.state.storage.put("n", n)
		return Response.json({ n, url: request.url })
	}
}

const worker = {
	fetch(request: Request, env: Env): Promise<Response> {
		const name = new URL(request.url).searchParams.get("name") ?? "default"
		const id = env.COUNTER.idFromName(name)
		return env.COUNTER.get(id).fetch(request)
	},
}

export default worker
