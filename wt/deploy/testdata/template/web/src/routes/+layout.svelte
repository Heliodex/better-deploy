<script lang="ts">
import "../app.css"
import type { LayoutProps } from "./$types"

let { children }: LayoutProps = $props()
</script>

<svelte:head>
	<link href="/favicon.svg" rel="icon">
</svelte:head>

<div class="page">
	<header class="site-header">
		<a class="brand" href="/">__APP_NAME__</a>
	</header>

	<main>
		{@render children()}
	</main>

	<footer>
		<span>Static SvelteKit shell</span>
		<span>Go + SQLite API</span>
	</footer>
</div>

<style>
.page {
	display: flex;
	flex-direction: column;
	width: min(1080px, calc(100% - 36px));
	min-height: 100vh;
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

.brand:hover {
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
</style>
