<script lang="ts">
	import { useForm, usePage } from "@inertiajs/svelte"

	interface Message {
		body: string
		createdAt: string
		id: string
		name: string
	}

	interface Props {
		greeting?: string
		messages?: Message[]
	}

	interface SharedProps {
		flash?: {
			success?: string
			error?: string
		}
		[key: string]: unknown
	}

	let { greeting = "Hello from Go!", messages = [] }: Props = $props()

	const page = usePage<SharedProps>()
	const form = useForm({ body: "", name: "" })

	const dateFormat = new Intl.DateTimeFormat("en-GB", {
		day: "numeric",
		hour: "2-digit",
		minute: "2-digit",
		month: "short",
		year: "numeric",
	})

	function submit(event: SubmitEvent) {
		event.preventDefault()

		form.post("/messages", {
			onSuccess: () => form.reset(),
			preserveScroll: true,
		})
	}
</script>

<main>
	<header>
		<h1>{greeting}</h1>
		<p>A guestbook showing Inertia forms, validation errors, flash messages, and a redirect.</p>
	</header>

	{#if page.props.flash?.success}
		<p class="flash success" role="status">{page.props.flash.success}</p>
	{/if}

	{#if page.props.flash?.error}
		<p class="flash error" role="alert">{page.props.flash.error}</p>
	{/if}

	<section class="composer">
		<h2>Leave a message</h2>

		<form onsubmit={submit}>
			<label for="guestbook-name">Name</label>
			<input
				aria-invalid={form.errors.name ? "true" : undefined}
				autocomplete="name"
				id="guestbook-name"
				maxlength={40}
				name="name"
				type="text"
				bind:value={form.name}
			>
			{#if form.errors.name}
				<p class="field-error">{form.errors.name}</p>
			{/if}

			<label for="guestbook-body">Message</label>
			<textarea
				aria-invalid={form.errors.body ? "true" : undefined}
				id="guestbook-body"
				maxlength={280}
				name="body"
				rows={3}
				bind:value={form.body}
			></textarea>
			{#if form.errors.body}
				<p class="field-error">{form.errors.body}</p>
			{/if}

			<div class="actions">
				<button disabled={form.processing} type="submit">
					{form.processing ? "Sending…" : "Send message"}
				</button>
				<span class="hint">{form.body.length}/280</span>
			</div>
		</form>
	</section>

	<section class="messages">
		<h2>Messages</h2>

		{#if messages.length === 0}
			<p class="empty">No messages yet. Be the first!</p>
		{:else}
			<ul>
				{#each messages as message (message.id)}
					<li>
						<p class="body">{message.body}</p>
						<p class="meta">
							{message.name}
							·
							<time datetime={message.createdAt}>
								{dateFormat.format(new Date(message.createdAt))}
							</time>
						</p>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
</main>

<style>
	main {
		max-width: 44rem;
		padding: 4rem 1.5rem;
		margin: 0 auto;
	}

	header {
		margin-bottom: 2rem;
	}

	h1 {
		margin: 0 0 0.5rem;
		font-size: clamp(2rem, 6vw, 3rem);
		letter-spacing: -0.03em;
	}

	h2 {
		margin: 0 0 1rem;
		font-size: 1.15rem;
	}

	header p,
	.empty {
		margin: 0;
		opacity: 0.7;
	}

	.composer,
	.messages {
		padding: 1.5rem;
		margin-bottom: 1.5rem;
		border: 1px solid rgb(127 127 127 / 0.25);
		border-radius: 0.75rem;
	}

	form {
		display: grid;
		gap: 0.35rem;
	}

	label {
		display: block;
		margin-top: 0.65rem;
		font-size: 0.9rem;
		font-weight: 600;
	}

	input,
	textarea {
		width: 100%;
		padding: 0.6rem 0.7rem;
		margin-top: 0.35rem;
		font: inherit;
		font-weight: 400;
		color: inherit;
		background: rgb(127 127 127 / 0.08);
		border: 1px solid rgb(127 127 127 / 0.35);
		border-radius: 0.5rem;
	}

	textarea {
		resize: vertical;
	}

	input[aria-invalid="true"],
	textarea[aria-invalid="true"] {
		border-color: #d33;
	}

	input:focus-visible,
	textarea:focus-visible,
	button:focus-visible {
		outline: 2px solid #5b8def;
		outline-offset: 2px;
	}

	.field-error {
		margin: 0.15rem 0 0;
		font-size: 0.85rem;
		color: #d33;
	}

	.actions {
		display: flex;
		gap: 1rem;
		align-items: center;
		margin-top: 0.9rem;
	}

	button {
		padding: 0.6rem 1.1rem;
		font: inherit;
		color: inherit;
		cursor: pointer;
		background: transparent;
		border: 1px solid currentColor;
		border-radius: 0.5rem;
	}

	button:hover:not(:disabled) {
		background: rgb(127 127 127 / 0.12);
	}

	button:disabled {
		cursor: progress;
		opacity: 0.6;
	}

	.hint {
		font-size: 0.8rem;
		opacity: 0.55;
	}

	.flash {
		padding: 0.7rem 1rem;
		margin: 0 0 1.5rem;
		font-size: 0.9rem;
		border-radius: 0.5rem;
	}

	.flash.success {
		color: #1a7f37;
		background: rgb(26 127 55 / 0.12);
	}

	.flash.error {
		color: #d33;
		background: rgb(221 51 51 / 0.12);
	}

	ul {
		display: grid;
		gap: 0.75rem;
		padding: 0;
		margin: 0;
		list-style: none;
	}

	li {
		padding: 0.9rem 1rem;
		background: rgb(127 127 127 / 0.06);
		border-radius: 0.5rem;
	}

	.body {
		margin: 0 0 0.35rem;
		overflow-wrap: anywhere;
		white-space: pre-wrap;
	}

	.meta {
		margin: 0;
		font-size: 0.8rem;
		opacity: 0.6;
	}
</style>
