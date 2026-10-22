<script lang="ts">
import { addNote, getNotes, removeNote } from "./guestbook.remote"

let text = $state("")
let problem = $state<string | undefined>()

async function sign(event: SubmitEvent) {
	event.preventDefault()
	problem = undefined
	try {
		await addNote(text)
		text = ""
	} catch (e) {
		problem = e instanceof Error ? e.message : "Something went wrong."
	}
}

async function remove(id: string) {
	problem = undefined
	try {
		await removeNote(id)
	} catch (e) {
		problem = e instanceof Error ? e.message : "Something went wrong."
	}
}
</script>

<h1>Welcome to SvelteKit</h1>
<p>Visit <a href="https://svelte.dev/docs/kit">svelte.dev/docs/kit</a> to read the documentation</p>

<h2>Guestbook</h2>
<p>
	Notes persist in a celld KV namespace (<code>GUESTBOOK</code>) through SvelteKit
	<a href="https://svelte.dev/docs/kit/remote-functions">remote functions</a>.
</p>

<svelte:boundary>
	{#if (await getNotes()).length === 0}
		<p>No notes yet. Be the first to sign.</p>
	{:else}
		<ul>
			{#each await getNotes() as note (note.id)}
				<li>
					<time datetime={note.createdAt}>{note.createdAt}</time>
					<span>{note.text}</span>
					<button onclick={() => remove(note.id)} type="button">Delete</button>
				</li>
			{/each}
		</ul>
	{/if}

	<form onsubmit={sign}>
		<label>
			Leave a note
			<input maxlength="280" name="text" placeholder="Hello from celld" type="text" bind:value={text}>
		</label>
		<button type="submit">Sign</button>
	</form>

	{#if problem}
		<p><strong>{problem}</strong></p>
	{/if}

	{#snippet pending()}
		<p>Loading guestbook…</p>
	{/snippet}

	{#snippet failed(
		error,
		reset,
	)}
		<p><strong>Could not load the guestbook: {error instanceof Error ? error.message : "Unknown error"}</strong></p>
		<button onclick={reset} type="button">Retry</button>
	{/snippet}
</svelte:boundary>
