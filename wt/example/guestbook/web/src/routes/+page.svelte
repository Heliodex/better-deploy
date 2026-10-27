<script lang="ts">
import { onMount } from "svelte"

const reactionOptions = [
	{ emoji: "🚀", key: "rocket", label: "rocket" },
	{ emoji: "🧡", key: "heart", label: "heart" },
	{ emoji: "👀", key: "eyes", label: "eyes" },
	{ emoji: "🧠", key: "brain", label: "brain" },
] as const

type Reaction = (typeof reactionOptions)[number]["key"]

type Message = {
	id: string
	name: string
	body: string
	createdAt: string
	reactions: Record<Reaction, number>
}

let messages = $state<Message[]>([])
let name = $state("")
let body = $state("")
let loading = $state(true)
let sending = $state(false)
let error = $state("")

let totalReactions = $derived(
	messages.reduce(
		(total, message) => total + reactionOptions.reduce((sum, reaction) => sum + message.reactions[reaction.key], 0),
		0,
	),
)

const timeFormat = new Intl.DateTimeFormat(undefined, {
	dateStyle: "medium",
	timeStyle: "short",
})

function formatTime(value: string): string {
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? value : timeFormat.format(date)
}

async function loadMessages(): Promise<void> {
	try {
		const response = await fetch("/api/messages?limit=50")
		if (!response.ok) {
			throw new Error(`The wall API returned ${response.status}`)
		}
		const data = (await response.json()) as { messages: Message[] }
		messages = data.messages ?? []
		error = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the wall API."
	} finally {
		loading = false
	}
}

onMount(() => {
	void loadMessages()
	const timer = setInterval(() => void loadMessages(), 20000)
	return () => clearInterval(timer)
})

async function submit(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	const trimmedName = name.trim()
	const trimmedBody = body.trim()
	if (!trimmedName || !trimmedBody) {
		error = "A name and a message are both required."
		return
	}

	sending = true
	error = ""
	try {
		const response = await fetch("/api/messages", {
			body: JSON.stringify({ body: trimmedBody, name: trimmedName }),
			headers: { "content-type": "application/json" },
			method: "POST",
		})
		const data = (await response.json().catch(() => null)) as { error?: string } | null
		if (!response.ok) {
			throw new Error(data?.error ?? `The wall API returned ${response.status}`)
		}
		messages = [data as Message, ...messages]
		name = ""
		body = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not post to the wall."
	} finally {
		sending = false
	}
}

async function react(message: Message, reaction: Reaction): Promise<void> {
	const previous = message.reactions[reaction]
	message.reactions[reaction] = previous + 1
	try {
		const response = await fetch(`/api/messages/${message.id}/react`, {
			body: JSON.stringify({ reaction }),
			headers: { "content-type": "application/json" },
			method: "POST",
		})
		const data = (await response.json().catch(() => null)) as { count?: number; error?: string } | null
		if (!response.ok) {
			throw new Error(data?.error ?? `The wall API returned ${response.status}`)
		}
		if (typeof data?.count === "number") {
			message.reactions[reaction] = data.count
		}
	} catch (err) {
		message.reactions[reaction] = previous
		error = err instanceof Error ? err.message : "Could not add that reaction."
	}
}
</script>

<svelte:head>
	<title>Wasm Wall</title>
</svelte:head>

<section class="hero">
	<div>
		<p class="eyebrow">A tiny guestbook for the wasm web</p>
		<h1>Leave a mark on the <em>Wasm Wall</em>.</h1>
		<p class="lede">
			A static SvelteKit page with a living Go + SQLite API behind it. Post a note, send a reaction, and see what
			other people are building.
		</p>
	</div>
	<div class="scoreboard">
		<div><strong>{messages.length}</strong><span>notes</span></div>
		<div><strong>{totalReactions}</strong><span>reactions</span></div>
	</div>
</section>

<section aria-labelledby="composer-title" class="composer">
	<div class="section-head">
		<h2 id="composer-title">Sign the wall</h2>
		<span>Keep it kind. Keep it short.</span>
	</div>
	<form onsubmit={submit}>
		<div class="fields">
			<label class="name-field">
				<span>Name</span>
				<input autocomplete="name" maxlength="40" placeholder="Your name" required bind:value={name}>
			</label>
			<label class="message-field">
				<span>Message</span>
				<textarea
					maxlength="280"
					placeholder="What are you building?"
					required
					rows="3"
					bind:value={body}
				></textarea>
			</label>
		</div>
		<div class="composer-foot">
			<span>{body.length} / 280</span>
			<button disabled={sending} type="submit">
				{sending ? "Posting…" : "Post to the wall"}
			</button>
		</div>
	</form>
</section>

{#if error}
	<div class="error" role="alert">{error}</div>
{/if}

<section aria-busy={loading} aria-labelledby="wall-title" aria-live="polite" class="wall">
	<div class="wall-head">
		<h2 id="wall-title">The wall</h2>
		<span>{messages.length} {messages.length === 1 ? "note" : "notes"}</span>
	</div>

	{#if loading}
		<div aria-label="Loading the wall" class="skeleton" role="status">
			<div></div>
			<div></div>
			<div></div>
		</div>
	{:else if messages.length === 0}
		<div class="empty">
			<p>The wall is quiet.</p>
			<span>Be the first to leave a note.</span>
		</div>
	{:else}
		<div class="messages">
			{#each messages as message (message.id)}
				<article class="message">
					<header>
						<div aria-hidden="true" class="avatar">
							{message.name.charAt(0).toUpperCase()}
						</div>
						<div>
							<h3>{message.name}</h3>
							<time datetime={message.createdAt}>{formatTime(message.createdAt)}</time>
						</div>
					</header>
					<p>{message.body}</p>
					<div class="reactions">
						{#each reactionOptions as option (option.key)}
							<button
								aria-label={`React with ${option.label}`}
								onclick={() => react(message, option.key)}
								title={`React with ${option.label}`}
								type="button"
							>
								<span aria-hidden="true">{option.emoji}</span>
								<span>{message.reactions[option.key]}</span>
							</button>
						{/each}
					</div>
				</article>
			{/each}
		</div>
	{/if}
</section>

<style>
.hero {
	display: flex;
	flex-wrap: wrap;
	gap: 28px;
	align-items: flex-end;
	justify-content: space-between;
	margin-bottom: 30px;
}

.eyebrow {
	margin-bottom: 10px;
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
	margin-top: 14px;
	font-size: 1.02rem;
	color: var(--muted);
}

.scoreboard {
	display: flex;
	gap: 10px;
}

.scoreboard div {
	min-width: 92px;
	padding: 13px 16px;
	text-align: center;
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: 14px;
	box-shadow: 0 8px 20px rgba(60, 45, 20, 0.04);
}

.scoreboard strong {
	display: block;
	font-size: 1.5rem;
	font-variant-numeric: tabular-nums;
	letter-spacing: -0.04em;
}

.scoreboard span {
	font-size: 0.68rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.1em;
}

.composer {
	padding: 22px;
	margin-bottom: 22px;
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
	margin-bottom: 14px;
}

.section-head h2 {
	font-size: 1.12rem;
}

.section-head span {
	font-size: 0.78rem;
	color: var(--muted);
}

.fields {
	display: grid;
	grid-template-columns: 220px minmax(0, 1fr);
	gap: 12px;
}

label {
	display: flex;
	flex-direction: column;
	gap: 6px;
}

.fields label > span {
	font-size: 0.68rem;
	font-weight: 650;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.1em;
}

input,
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

input:focus,
textarea:focus {
	border-color: var(--accent);
	box-shadow: 0 0 0 3px var(--accent-soft);
}

.composer-foot {
	display: flex;
	gap: 16px;
	align-items: center;
	justify-content: space-between;
	margin-top: 12px;
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
	margin-bottom: 22px;
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
	margin-bottom: 12px;
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

.message header {
	display: flex;
	gap: 11px;
	align-items: center;
	margin-bottom: 10px;
}

.avatar {
	display: grid;
	place-items: center;
	width: 36px;
	height: 36px;
	font-size: 0.95rem;
	font-weight: 800;
	color: var(--teal);
	background: var(--teal-soft);
	border-radius: 50%;
}

.message h3 {
	font-size: 0.95rem;
}

.message time {
	display: block;
	margin-top: 1px;
	font-size: 0.72rem;
	color: var(--muted);
}

.message > p {
	font-size: 0.95rem;
	color: #364043;
	overflow-wrap: anywhere;
	white-space: pre-wrap;
}

.reactions {
	display: flex;
	flex-wrap: wrap;
	gap: 7px;
	padding-top: 12px;
	margin-top: 14px;
	border-top: 1px dashed var(--line);
}

.reactions button {
	display: inline-flex;
	gap: 6px;
	align-items: center;
	padding: 4px 9px;
	font-size: 0.78rem;
	font-variant-numeric: tabular-nums;
	color: var(--muted);
	background: transparent;
	border: 1px solid var(--line);
	border-radius: 999px;
}

.reactions button:hover {
	color: var(--accent-deep);
	background: var(--accent-soft);
	border-color: var(--accent);
}

.skeleton {
	display: grid;
	gap: 12px;
}

.skeleton div {
	height: 124px;
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
	.fields {
		grid-template-columns: 1fr;
	}

	.scoreboard {
		width: 100%;
	}

	.scoreboard div {
		flex: 1;
	}
}
</style>
