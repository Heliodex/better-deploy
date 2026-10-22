import { env } from "cloudflare:workers"
import { error } from "@sveltejs/kit"
import { command, query } from "$app/server"

interface Note {
	createdAt: string
	id: string
	text: string
}

const PREFIX = "note:"
const MAX_NOTES = 20
const MAX_TEXT_LENGTH = 280

function getGuestbook(): KVNamespace {
	let guestbook: KVNamespace | undefined
	try {
		guestbook = env.GUESTBOOK
	} catch {
		guestbook = undefined
	}

	if (!guestbook) error(503, "GUESTBOOK KV binding is not available")
	return guestbook
}

export const getNotes = query(async (): Promise<Note[]> => {
	const guestbook = getGuestbook()

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
	return notes
})

export const addNote = command("unchecked", async (text: string) => {
	const clean = text.trim()
	if (!clean) error(400, "Write something first.")
	if (clean.length > MAX_TEXT_LENGTH) error(400, `Keep it under ${MAX_TEXT_LENGTH} characters.`)

	const guestbook = getGuestbook()
	const createdAt = new Date().toISOString()
	const id = `${PREFIX}${createdAt}:${crypto.randomUUID()}`
	await guestbook.put(id, JSON.stringify({ createdAt, text: clean }))

	// Single-flight mutation: refresh the notes query in the same round trip.
	void getNotes().refresh()

	return { id }
})

export const removeNote = command("unchecked", async (id: string) => {
	if (!id.startsWith(PREFIX)) error(400, "Unknown note.")

	const guestbook = getGuestbook()
	await guestbook.delete(id)

	// Single-flight mutation: refresh the notes query in the same round trip.
	void getNotes().refresh()

	return { id }
})
