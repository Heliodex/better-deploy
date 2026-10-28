<script lang="ts">
import { onMount } from "svelte"
import { createNote, fetchNotes, type Note, ValidationError } from "../lib/api"
import { maxNote } from "../lib/contract"

let notes = $state<Note[]>([])
let text = $state("")
let loading = $state(true)
let sending = $state(false)
let error = $state("")
let fieldErrors = $state<{ [_: string]: string }>({})

const timeFormat = new Intl.DateTimeFormat(undefined, {
	dateStyle: "medium",
	timeStyle: "short",
})

function formatTime(value: string): string {
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? value : timeFormat.format(date)
}

async function loadNotes(): Promise<void> {
	try {
		notes = await fetchNotes(50)
		error = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the notes API."
	} finally {
		loading = false
	}
}

onMount((): (() => void) => {
	void loadNotes()
	const timer = setInterval((): void => void loadNotes(), 20000)
	return (): void => clearInterval(timer)
})

function clearFieldError(field: string): void {
	if (!(field in fieldErrors)) return

	const next = { ...fieldErrors }
	delete next[field]
	fieldErrors = next
}

async function submit(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	sending = true
	error = ""
	fieldErrors = {}
	try {
		const entry = await createNote(text)
		notes = [entry, ...notes]
		text = ""
	} catch (err) {
		if (err instanceof ValidationError) {
			fieldErrors = err.fields
			if (Object.keys(err.fields).length === 0) error = err.message
		} else error = err instanceof Error ? err.message : "Could not save the note."
	} finally {
		sending = false
	}
}
</script>

<svelte:head>
	<title>Notes</title>
</svelte:head>

<section class="hero">
	<div>
		<p class="eyebrow">A tiny notes app</p>
		<h1>Leave a <em>note</em>.</h1>
		<p class="lede">
			A static SvelteKit page with a living Go + SQLite API behind it. Write a note and see it appear below.
		</p>
	</div>
</section>

<div class="feed">
	<section aria-labelledby="composer-title" class="composer">
		<div class="section-head">
			<h2 id="composer-title">New note</h2>
			<span>Keep it short.</span>
		</div>
		<form novalidate onsubmit={submit}>
			<label class="note-field">
				<span>Note</span>
				<textarea
					aria-describedby={fieldErrors.text ? "text-error" : undefined}
					aria-invalid={fieldErrors.text ? "true" : undefined}
					maxlength={maxNote}
					oninput={(): void => clearFieldError("text")}
					placeholder="What are you thinking?"
					required
					rows="3"
					bind:value={text}
				></textarea>
				{#if fieldErrors.text}
					<small class="field-error" id="text-error">{fieldErrors.text}</small>
				{/if}
			</label>
			<div class="composer-foot">
				<span>{text.length} / {maxNote}</span>
				<button disabled={sending} type="submit">
					{sending ? "Saving…" : "Save note"}
				</button>
			</div>
		</form>
	</section>

	{#if error}
		<div class="error" role="alert">{error}</div>
	{/if}

	<section aria-busy={loading} aria-labelledby="notes-title" aria-live="polite" class="wall">
		<div class="wall-head">
			<h2 id="notes-title">Notes</h2>
			<span>{notes.length} {notes.length === 1 ? "note" : "notes"}</span>
		</div>

		{#if loading}
			<div aria-label="Loading notes" class="skeleton" role="status">
				<div></div>
				<div></div>
			</div>
		{:else if notes.length === 0}
			<div class="empty">
				<p>No notes yet.</p>
				<span>Write the first one above.</span>
			</div>
		{:else}
			<div class="messages">
				{#each notes as note (note.id)}
					<article class="message">
						<p>{note.text}</p>
						<time datetime={note.createdAt}>{formatTime(note.createdAt)}</time>
					</article>
				{/each}
			</div>
		{/if}
	</section>
</div>

<style>
.feed {
	display: flex;
	flex-direction: column;
	gap: 22px;
}

.hero {
	display: flex;
	flex-wrap: wrap;
	gap: 28px;
	align-items: flex-end;
	justify-content: space-between;
	padding-bottom: 30px;
}

.eyebrow {
	padding-bottom: 10px;
	font-size: 0.72rem;
	font-weight: 750;
	color: var(--accent);
	text-transform: uppercase;
	letter-spacing: 0.13em;
}

h1 {
	max-width: 720px;
	font-size: clamp(2.2rem, 6vw, 4rem);
}

h1 em {
	font-style: normal;
	color: var(--accent);
}

.lede {
	max-width: 620px;
	padding-top: 14px;
	font-size: 1.02rem;
	color: var(--muted);
}

.composer {
	padding: 22px;
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: var(--radius);
	box-shadow: var(--shadow);
}

.section-head {
	display: flex;
	flex-wrap: wrap;
	gap: 16px;
	align-items: baseline;
	justify-content: space-between;
	padding-bottom: 14px;
}

.section-head h2 {
	font-size: 1.12rem;
}

.section-head span {
	font-size: 0.78rem;
	color: var(--muted);
}

.note-field {
	display: flex;
	flex-direction: column;
	gap: 6px;
}

.note-field > span {
	font-size: 0.68rem;
	font-weight: 650;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.1em;
}

textarea {
	width: 100%;
	padding: 10px 12px;
	color: var(--ink);
	resize: vertical;
	outline: none;
	background: #fff;
	border: 1px solid var(--line);
	border-radius: 11px;
}

textarea:focus {
	border-color: var(--accent);
	box-shadow: 0 0 3px var(--accent-soft);
}

textarea[aria-invalid="true"] {
	background: #fff8f4;
	border-color: var(--accent);
}

.field-error {
	font-size: 0.72rem;
	font-weight: 550;
	color: var(--accent-deep);
}

.composer-foot {
	display: flex;
	gap: 16px;
	align-items: center;
	justify-content: space-between;
	padding-top: 12px;
}

.composer-foot > span {
	font-size: 0.74rem;
	font-variant-numeric: tabular-nums;
	color: var(--muted);
}

.composer-foot button {
	padding: 10px 16px;
	font-size: 0.84rem;
	font-weight: 750;
	color: #fff;
	background: var(--accent);
	border: 0;
	border-radius: 10px;
}

.composer-foot button:hover {
	background: var(--accent-deep);
}

.composer-foot button:disabled {
	cursor: wait;
	opacity: 0.7;
}

.error {
	padding: 11px 14px;
	font-size: 0.88rem;
	color: #7d2a12;
	background: #fbe5df;
	border: 1px solid #e9b9aa;
	border-radius: 12px;
}

.wall-head {
	display: flex;
	gap: 16px;
	align-items: baseline;
	justify-content: space-between;
	padding-bottom: 12px;
}

.wall-head h2 {
	font-size: 1.2rem;
}

.wall-head span {
	font-size: 0.78rem;
	color: var(--muted);
}

.messages {
	display: grid;
	gap: 12px;
}

.message {
	padding: 18px 19px;
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: var(--radius);
	box-shadow: 0 4px 14px rgba(60, 45, 20, 0.035);
}

.message > p {
	font-size: 0.95rem;
	color: #364043;
	overflow-wrap: anywhere;
	white-space: pre-wrap;
}

.message time {
	display: block;
	padding-top: 8px;
	font-size: 0.72rem;
	color: var(--muted);
}

.skeleton {
	display: grid;
	gap: 12px;
}

.skeleton div {
	height: 96px;
	background: linear-gradient(90deg, var(--card), var(--paper-deep), var(--card));
	background-size: 200% 100%;
	border: 1px solid var(--line);
	border-radius: var(--radius);
	animation: shimmer 1.4s infinite;
}

.empty {
	padding: 38px;
	text-align: center;
	background: var(--card);
	border: 1px dashed var(--line);
	border-radius: var(--radius);
}

.empty p {
	font-weight: 700;
}

.empty span {
	font-size: 0.86rem;
	color: var(--muted);
}

@keyframes shimmer {
	to {
		background-position: -200% 0;
	}
}

@media (max-width: 700px) {
	.hero {
		flex-direction: column;
		align-items: flex-start;
	}
}
</style>
