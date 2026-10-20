<script lang="ts">
	let { greeting = 'Hello from Go!' }: { greeting?: string } = $props();

	let count = $state(0);
</script>

<main>
	<h1>{greeting}</h1>
	<p>This page is rendered by Inertia and hydrated by Svelte.</p>

	<button onclick={() => count++}>
		Clicked {count}
		{count === 1 ? 'time' : 'times'}
	</button>
</main>

<style>
	main {
		max-width: 40rem;
		margin: 0 auto;
		padding: 4rem 1.5rem;
	}

	h1 {
		margin: 0 0 0.5rem;
		font-size: clamp(2rem, 6vw, 3rem);
		letter-spacing: -0.03em;
	}

	p {
		margin: 0 0 2rem;
		opacity: 0.7;
	}

	button {
		padding: 0.6rem 1.1rem;
		border: 1px solid currentColor;
		border-radius: 0.5rem;
		background: transparent;
		color: inherit;
		font: inherit;
		cursor: pointer;
	}

	button:hover {
		background: rgb(127 127 127 / 0.12);
	}
</style>
