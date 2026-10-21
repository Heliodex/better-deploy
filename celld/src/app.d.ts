// See https://svelte.dev/docs/kit/types#app.d.ts
// for information about these interfaces
declare global {
	namespace App {
		interface Platform {
			caches: CacheStorage
			cf?: IncomingRequestCfProperties
			ctx: ExecutionContext
			env: Env
		}

		// interface Error {}
		// interface Locals {}
		// interface PageData {}
		// interface PageState {}
	}
}

export {}
