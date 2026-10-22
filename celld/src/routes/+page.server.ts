import { env } from "cloudflare:workers"
import { fail, redirect } from "@sveltejs/kit"
import type { Actions, PageServerLoad } from "./$types"

interface Note {
	createdAt: string
	id: string
	text: string
}

const PREFIX = "note:"
const MAX_NOTES = 20
const MAX_TEXT_LENGTH = 280

function getGuestbook(platform: App.Platform | undefined): KVNamespace | undefined {
	// `vite dev` populates `platform.env` via the adapter's platform proxy, while
	// the built worker reads bindings from `cloudflare:workers`. Neither exists
	// outside those runtimes, so fall back to `undefined` there.
	if (platform?.env.GUESTBOOK) return platform.env.GUESTBOOK
	try {
		return env.GUESTBOOK
	} catch {
		// Not running inside a worker (plain `vite dev` without bindings).
		return undefined
	}
}

const guestbookMissing = {
	notes: [],
	persistent: false,
} as const

export const load: PageServerLoad = async ({ platform }) => {
	const guestbook = getGuestbook(platform)
	if (!guestbook) return guestbookMissing

	const listed = await guestbook.list({ prefix: PREFIX })
	const keys = listed.keys
		.map((entry: KVNamespaceListKey<unknown>) => entry.name)
		.toSorted()
		.toReversed()
		.slice(0, MAX_NOTES)
	const notes = (
		await Promise.all(
			keys.map(async (id: string) => {
				const note = await guestbook.get<Note>(id, "json")
				if (!note?.text) return undefined
				return { createdAt: note.createdAt, id, text: note.text }
			}),
		)
	).filter((note): note is Note => note !== undefined)
	return { notes, persistent: true as const }
}

export const actions: Actions = {
	add: async ({ platform, request }) => {
		const guestbook = getGuestbook(platform)
		if (!guestbook)
			return fail(503, {
				error: "No KV binding. Run the app with `bun run dev:celld`.",
			})

		const data = await request.formData()
		const raw = data.get("text")
		const text = (typeof raw === "string" ? raw : "").trim()
		if (!text) return fail(400, { error: "Write something first." })
		if (text.length > MAX_TEXT_LENGTH)
			return fail(400, {
				error: `Keep it under ${MAX_TEXT_LENGTH} characters.`,
				text,
			})

		const createdAt = new Date().toISOString()
		const id = `${PREFIX}${createdAt}:${crypto.randomUUID()}`
		await guestbook.put(id, JSON.stringify({ createdAt, text }))
		// oxlint-disable-next-line typescript/only-throw-error -- SvelteKit redirects are thrown, never returned.
		throw redirect(303, "/")
	},
	remove: async ({ platform, request }) => {
		const guestbook = getGuestbook(platform)
		if (!guestbook)
			return fail(503, {
				error: "No KV binding. Run the app with `bun run dev:celld`.",
			})

		const data = await request.formData()
		const raw = data.get("id")
		const id = typeof raw === "string" ? raw : ""
		if (!id.startsWith(PREFIX)) return fail(400, { error: "Unknown note." })
		await guestbook.delete(id)
		// oxlint-disable-next-line typescript/only-throw-error -- SvelteKit redirects are thrown, never returned.
		throw redirect(303, "/")
	},
}
