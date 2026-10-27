<script lang="ts">
	import { onMount } from 'svelte';

	const reactionOptions = [
		{ key: 'rocket', emoji: '🚀', label: 'rocket' },
		{ key: 'heart', emoji: '🧡', label: 'heart' },
		{ key: 'eyes', emoji: '👀', label: 'eyes' },
		{ key: 'brain', emoji: '🧠', label: 'brain' }
	] as const;

	type Reaction = (typeof reactionOptions)[number]['key'];

	type Message = {
		id: string;
		name: string;
		body: string;
		createdAt: string;
		reactions: Record<Reaction, number>;
	};

	let messages = $state<Message[]>([]);
	let name = $state('');
	let body = $state('');
	let loading = $state(true);
	let sending = $state(false);
	let error = $state('');

	let totalReactions = $derived(
		messages.reduce(
			(total, message) =>
				total +
				reactionOptions.reduce(
					(sum, reaction) => sum + message.reactions[reaction.key],
					0
				),
			0
		)
	);

	const timeFormat = new Intl.DateTimeFormat(undefined, {
		dateStyle: 'medium',
		timeStyle: 'short'
	});

	function formatTime(value: string): string {
		const date = new Date(value);
		return Number.isNaN(date.getTime()) ? value : timeFormat.format(date);
	}

	async function loadMessages(): Promise<void> {
		try {
			const response = await fetch('/api/messages?limit=50');
			if (!response.ok) {
				throw new Error(`The wall API returned ${response.status}`);
			}
			const data = (await response.json()) as { messages: Message[] };
			messages = data.messages ?? [];
			error = '';
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not reach the wall API.';
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		void loadMessages();
		const timer = setInterval(() => void loadMessages(), 20000);
		return () => clearInterval(timer);
	});

	async function submit(event: SubmitEvent): Promise<void> {
		event.preventDefault();
		const trimmedName = name.trim();
		const trimmedBody = body.trim();
		if (!trimmedName || !trimmedBody) {
			error = 'A name and a message are both required.';
			return;
		}

		sending = true;
		error = '';
		try {
			const response = await fetch('/api/messages', {
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({ name: trimmedName, body: trimmedBody })
			});
			const data = (await response.json().catch(() => null)) as
				| { error?: string }
				| null;
			if (!response.ok) {
				throw new Error(data?.error ?? `The wall API returned ${response.status}`);
			}
			messages = [data as Message, ...messages];
			name = '';
			body = '';
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not post to the wall.';
		} finally {
			sending = false;
		}
	}

	async function react(message: Message, reaction: Reaction): Promise<void> {
		const previous = message.reactions[reaction];
		message.reactions[reaction] = previous + 1;
		try {
			const response = await fetch(`/api/messages/${message.id}/react`, {
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({ reaction })
			});
			const data = (await response.json().catch(() => null)) as
				| { count?: number; error?: string }
				| null;
			if (!response.ok) {
				throw new Error(data?.error ?? `The wall API returned ${response.status}`);
			}
			if (typeof data?.count === 'number') {
				message.reactions[reaction] = data.count;
			}
		} catch (err) {
			message.reactions[reaction] = previous;
			error = err instanceof Error ? err.message : 'Could not add that reaction.';
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
			A static SvelteKit page with a living Go + SQLite API behind it. Post a note,
			send a reaction, and see what other people are building.
		</p>
	</div>
	<div class="scoreboard" aria-label="Wall activity">
		<div><strong>{messages.length}</strong><span>notes</span></div>
		<div><strong>{totalReactions}</strong><span>reactions</span></div>
	</div>
</section>

<section class="composer" aria-labelledby="composer-title">
	<div class="section-head">
		<h2 id="composer-title">Sign the wall</h2>
		<span>Keep it kind. Keep it short.</span>
	</div>
	<form onsubmit={submit}>
		<div class="fields">
			<label class="name-field">
				<span>Name</span>
				<input
					bind:value={name}
					maxlength="40"
					placeholder="Your name"
					autocomplete="name"
					required
				/>
			</label>
			<label class="message-field">
				<span>Message</span>
				<textarea
					bind:value={body}
					maxlength="280"
					rows="3"
					placeholder="What are you building?"
					required
				></textarea>
			</label>
		</div>
		<div class="composer-foot">
			<span>{body.length} / 280</span>
			<button type="submit" disabled={sending}>
				{sending ? 'Posting…' : 'Post to the wall'}
			</button>
		</div>
	</form>
</section>

{#if error}
	<div class="error" role="alert">{error}</div>
{/if}

<section class="wall" aria-labelledby="wall-title" aria-live="polite" aria-busy={loading}>
	<div class="wall-head">
		<h2 id="wall-title">The wall</h2>
		<span>{messages.length} {messages.length === 1 ? 'note' : 'notes'}</span>
	</div>

	{#if loading}
		<div class="skeleton" aria-label="Loading the wall">
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
						<div class="avatar" aria-hidden="true">
							{message.name.charAt(0).toUpperCase()}
						</div>
						<div>
							<h3>{message.name}</h3>
							<time datetime={message.createdAt}>{formatTime(message.createdAt)}</time>
						</div>
					</header>
					<p>{message.body}</p>
					<div class="reactions" aria-label={`Reactions from ${message.name}`}>
						{#each reactionOptions as option (option.key)}
							<button
								type="button"
								onclick={() => react(message, option.key)}
								aria-label={`React with ${option.label}`}
								title={`React with ${option.label}`}
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
		align-items: flex-end;
		justify-content: space-between;
		gap: 28px;
		flex-wrap: wrap;
		margin-bottom: 30px;
	}

	.eyebrow {
		color: var(--accent);
		font-size: 0.72rem;
		font-weight: 750;
		letter-spacing: 0.13em;
		text-transform: uppercase;
		margin-bottom: 10px;
	}

	h1 {
		max-width: 720px;
		font-size: clamp(2.2rem, 6vw, 4rem);
	}

	h1 em {
		color: var(--accent);
		font-style: normal;
	}

	.lede {
		max-width: 620px;
		margin-top: 14px;
		color: var(--muted);
		font-size: 1.02rem;
	}

	.scoreboard {
		display: flex;
		gap: 10px;
	}

	.scoreboard div {
		min-width: 92px;
		padding: 13px 16px;
		background: var(--card);
		border: 1px solid var(--line);
		border-radius: 14px;
		text-align: center;
		box-shadow: 0 8px 20px rgba(60, 45, 20, 0.04);
	}

	.scoreboard strong {
		display: block;
		font-size: 1.5rem;
		font-variant-numeric: tabular-nums;
		letter-spacing: -0.04em;
	}

	.scoreboard span {
		color: var(--muted);
		font-size: 0.68rem;
		letter-spacing: 0.1em;
		text-transform: uppercase;
	}

	.composer {
		background: var(--card);
		border: 1px solid var(--line);
		border-radius: var(--radius);
		padding: 22px;
		box-shadow: var(--shadow);
		margin-bottom: 22px;
	}

	.section-head {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: 16px;
		flex-wrap: wrap;
		margin-bottom: 14px;
	}

	.section-head h2 {
		font-size: 1.12rem;
	}

	.section-head span {
		color: var(--muted);
		font-size: 0.78rem;
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

	label > span {
		color: var(--muted);
		font-size: 0.68rem;
		font-weight: 650;
		letter-spacing: 0.1em;
		text-transform: uppercase;
	}

	input,
	textarea {
		width: 100%;
		border: 1px solid var(--line);
		border-radius: 11px;
		background: #fff;
		color: var(--ink);
		padding: 10px 12px;
		outline: none;
		resize: vertical;
	}

	input:focus,
	textarea:focus {
		border-color: var(--accent);
		box-shadow: 0 0 0 3px var(--accent-soft);
	}

	.composer-foot {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
		margin-top: 12px;
	}

	.composer-foot > span {
		color: var(--muted);
		font-size: 0.74rem;
		font-variant-numeric: tabular-nums;
	}

	.composer-foot button {
		border: 0;
		border-radius: 10px;
		background: var(--accent);
		color: #fff;
		padding: 10px 16px;
		font-size: 0.84rem;
		font-weight: 750;
	}

	.composer-foot button:hover {
		background: var(--accent-deep);
	}

	.composer-foot button:disabled {
		cursor: wait;
		opacity: 0.7;
	}

	.error {
		margin-bottom: 22px;
		padding: 11px 14px;
		border: 1px solid #e9b9aa;
		border-radius: 12px;
		background: #fbe5df;
		color: #7d2a12;
		font-size: 0.88rem;
	}

	.wall-head {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: 16px;
		margin-bottom: 12px;
	}

	.wall-head h2 {
		font-size: 1.2rem;
	}

	.wall-head span {
		color: var(--muted);
		font-size: 0.78rem;
	}

	.messages {
		display: grid;
		gap: 12px;
	}

	.message {
		background: var(--card);
		border: 1px solid var(--line);
		border-radius: var(--radius);
		padding: 18px 19px;
		box-shadow: 0 4px 14px rgba(60, 45, 20, 0.035);
	}

	.message header {
		display: flex;
		align-items: center;
		gap: 11px;
		margin-bottom: 10px;
	}

	.avatar {
		display: grid;
		place-items: center;
		width: 36px;
		height: 36px;
		border-radius: 50%;
		background: var(--teal-soft);
		color: var(--teal);
		font-weight: 800;
		font-size: 0.95rem;
	}

	.message h3 {
		font-size: 0.95rem;
	}

	.message time {
		display: block;
		color: var(--muted);
		font-size: 0.72rem;
		margin-top: 1px;
	}

	.message > p {
		color: #364043;
		font-size: 0.95rem;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}

	.reactions {
		display: flex;
		gap: 7px;
		flex-wrap: wrap;
		margin-top: 14px;
		padding-top: 12px;
		border-top: 1px dashed var(--line);
	}

	.reactions button {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		border: 1px solid var(--line);
		border-radius: 999px;
		background: transparent;
		color: var(--muted);
		padding: 4px 9px;
		font-size: 0.78rem;
		font-variant-numeric: tabular-nums;
	}

	.reactions button:hover {
		border-color: var(--accent);
		background: var(--accent-soft);
		color: var(--accent-deep);
	}

	.skeleton {
		display: grid;
		gap: 12px;
	}

	.skeleton div {
		height: 124px;
		border-radius: var(--radius);
		background: linear-gradient(90deg, var(--card), var(--paper-deep), var(--card));
		background-size: 200% 100%;
		animation: shimmer 1.4s infinite;
		border: 1px solid var(--line);
	}

	.empty {
		padding: 38px;
		border: 1px dashed var(--line);
		border-radius: var(--radius);
		background: var(--card);
		text-align: center;
	}

	.empty p {
		font-weight: 700;
	}

	.empty span {
		color: var(--muted);
		font-size: 0.86rem;
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
