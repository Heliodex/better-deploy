<script lang="ts">
import type { ActionData, PageData } from "./$types"

let { data, form }: { data: PageData; form: ActionData } = $props()

const draft = $derived(form && "text" in form && typeof form.text === "string" ? form.text : "")
</script>

<h1>Welcome to SvelteKit</h1>
<p>Visit <a href="https://svelte.dev/docs/kit">svelte.dev/docs/kit</a> to read the documentation</p>

<h2>Guestbook</h2>
<p>Notes persist in a celld KV namespace (<code>GUESTBOOK</code>).</p>

{#if !data.persistent}
	<p>
		<strong>No KV binding.</strong>
		Run the app with
		<code>bun run dev:celld</code>
		so the worker (and its bindings) serves the page.
	</p>
{:else}
	<form action="?/add" method="POST">
		<label>
			Leave a note
			<input maxlength="280" name="text" placeholder="Hello from celld" type="text" value={draft}>
		</label>
		<button type="submit">Sign</button>
	</form>

	{#if form?.error}
		<p><strong>{form.error}</strong></p>
	{/if}

	{#if data.notes.length === 0}
		<p>No notes yet. Be the first to sign.</p>
	{:else}
		<ul>
			{#each data.notes as note (note.id)}
				<li>
					<time datetime={note.createdAt}>{note.createdAt}</time>
					<span>{note.text}</span>
					<form action="?/remove" method="POST">
						<input name="id" type="hidden" value={note.id}>
						<button type="submit">Delete</button>
					</form>
				</li>
			{/each}
		</ul>
	{/if}
{/if}
