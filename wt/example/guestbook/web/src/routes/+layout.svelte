<script lang="ts">
import "../app.css"
import type { LayoutProps } from "./$types"

let { children }: LayoutProps = $props()
</script>

<svelte:head>
	<link href="/favicon.svg" rel="icon">
	<meta content="A small guestbook on a static SvelteKit wall, backed by a Go API." name="description">
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
	display: flex;
	flex-direction: column;
	width: min(1080px, calc(100% - 36px));
	min-height: 100vh;
	margin: 0 auto;
}

.site-header {
	display: flex;
	gap: 20px;
	align-items: center;
	justify-content: space-between;
	padding: 28px 0 20px;
	border-bottom: 1px solid var(--line);
}

.brand {
	font-size: 1.55rem;
	font-weight: 800;
	color: var(--ink);
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
	font-size: 0.74rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.09em;
	border-top: 1px solid var(--line);
}

footer span + span::before {
	margin-right: 20px;
	color: var(--line);
	content: "·";
}

@media (max-width: 560px) {
	.site-header {
		flex-direction: column;
		align-items: flex-start;
	}

	footer span + span::before {
		display: none;
	}
}
</style>
