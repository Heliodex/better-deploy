<script lang="ts">
import "../app.css"
import type { LayoutProps } from "./$types"

let { children }: LayoutProps = $props()
</script>

<svelte:head>
	<link href="/favicon.svg" rel="icon" type="image/svg+xml">
	<meta content="A live uptime monitor on one Go + Wasm component." name="description">
</svelte:head>

<div class="page">
	<header class="site-header">
		<a class="brand" href="/">
			<svg aria-hidden="true" class="mark" viewBox="0 0 44 20">
				<path d="M1 10h9l3.5-7 5 14 4-9 3.5 4H43"></path>
			</svg>
			pulse
		</a>
		<p class="tagline">Go + Wasm uptime monitor</p>
	</header>

	<main>
		{@render children()}
	</main>

	<footer class="site-footer">
		<span>SvelteKit static shell</span>
		<span>SQLite history</span>
		<span>SSE event bus</span>
		<span>one wasm component</span>
	</footer>
</div>
