<script lang="ts">
import "../app.css"
import type { LayoutProps } from "./$types"

let { children }: LayoutProps = $props()
</script>

<svelte:head>
	<link href="/favicon.svg" rel="icon" type="image/svg+xml">
</svelte:head>

<div class="page">
	<header class="site-header">
		<a class="brand" href="/">
			<span aria-hidden="true" class="brand-seal">P</span>
			<span>
				<strong>Passport</strong>
				<small>OAuth 2.0 entry desk</small>
			</span>
		</a>
		<nav aria-label="About this example" class="site-nav">
			<a href="/#how">How it works</a>
			<a href="https://demo.duendesoftware.com" rel="noreferrer" target="_blank">Identity provider ↗</a>
		</nav>
	</header>

	<main>
		{@render children()}
	</main>

	<footer class="site-footer">
		<span>A wt-deploy example · Go + wasm + SvelteKit</span>
		<span>Identity from the Duende IdentityServer demo</span>
	</footer>
</div>

<style>
.page {
	display: flex;
	flex-direction: column;
	width: min(1120px, calc(100% - 40px));
	min-height: 100vh;
}

.site-header {
	display: flex;
	flex-wrap: wrap;
	gap: 18px 26px;
	align-items: center;
	justify-content: space-between;
	padding: 24px 0 18px;
	border-bottom: 1px solid var(--line-strong);
}

.brand {
	display: flex;
	gap: 12px;
	align-items: center;
	color: var(--night);
}

.brand:hover {
	text-decoration: none;
}

.brand-seal {
	display: grid;
	place-items: center;
	width: 38px;
	height: 38px;
	font-family: var(--serif);
	font-size: 1.05rem;
	color: var(--accent);
	background: var(--accent-soft);
	border: 1px solid var(--accent);
	border-radius: 50%;
	box-shadow:
		inset 0 0 0 3px var(--paper),
		inset 0 0 0 4px rgba(140, 47, 57, 0.35);
}

.brand strong {
	display: block;
	font-family: var(--serif);
	font-size: 1.32rem;
	font-weight: 600;
	line-height: 1.1;
	letter-spacing: 0.04em;
}

.brand small {
	display: block;
	margin-top: 3px;
	font-family: var(--mono);
	font-size: 0.58rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.14em;
}

.site-nav {
	display: flex;
	flex-wrap: wrap;
	gap: 20px;
	font-family: var(--mono);
	font-size: 0.68rem;
	text-transform: uppercase;
	letter-spacing: 0.08em;
}

.site-nav a {
	color: var(--teal);
}

main {
	flex: 1;
}

.site-footer {
	display: flex;
	flex-wrap: wrap;
	gap: 8px 24px;
	justify-content: space-between;
	padding: 18px 0 26px;
	font-family: var(--mono);
	font-size: 0.62rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.08em;
	border-top: 1px solid var(--line);
}

@media (max-width: 540px) {
	.site-nav {
		gap: 14px;
		font-size: 0.62rem;
	}
}
</style>
