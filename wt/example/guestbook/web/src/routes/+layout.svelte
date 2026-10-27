<script lang="ts">
	import '../app.css';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();
</script>

<svelte:head>
	<link rel="icon" href="/favicon.svg" />
	<meta name="description" content="A small guestbook on a static SvelteKit wall, backed by a Go API." />
</svelte:head>

<div class="page">
	<header class="site-header">
		<a class="brand" href="/">Wasm<span>Wall</span></a>
		<nav aria-label="Main">
			<a href="/">The wall</a>
			<a href="/about/">About</a>
		</nav>
	</header>

	<main>
		{@render children()}
	</main>

	<footer>
		<span>SvelteKit static shell</span>
		<span>Go + SQLite API</span>
		<span>one wasm component</span>
	</footer>
</div>

<style>
	.page {
		width: min(1080px, calc(100% - 36px));
		margin: 0 auto;
		min-height: 100vh;
		display: flex;
		flex-direction: column;
	}

	.site-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 20px;
		padding: 28px 0 20px;
		border-bottom: 1px solid var(--line);
	}

	.brand {
		color: var(--ink);
		font-size: 1.55rem;
		font-weight: 800;
		letter-spacing: -0.05em;
	}

	.brand span {
		color: var(--accent);
	}

	.brand:hover {
		text-decoration: none;
	}

	nav {
		display: flex;
		gap: 18px;
		font-size: 0.88rem;
		font-weight: 650;
	}

	nav a {
		color: var(--muted);
	}

	nav a:hover {
		color: var(--accent);
		text-decoration: none;
	}

	main {
		flex: 1;
		padding: 34px 0 70px;
	}

	footer {
		display: flex;
		flex-wrap: wrap;
		gap: 10px 20px;
		padding: 18px 0 26px;
		border-top: 1px solid var(--line);
		color: var(--muted);
		font-size: 0.74rem;
		text-transform: uppercase;
		letter-spacing: 0.09em;
	}

	footer span + span::before {
		content: '·';
		margin-right: 20px;
		color: var(--line);
	}

	@media (max-width: 560px) {
		.site-header {
			align-items: flex-start;
			flex-direction: column;
		}

		footer span + span::before {
			display: none;
		}
	}
</style>
