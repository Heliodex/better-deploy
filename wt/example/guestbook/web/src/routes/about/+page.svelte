<svelte:head>
	<title>About · Wasm Wall</title>
	<meta
		content="How the Wasm Wall demo combines a prerendered SvelteKit frontend with a Go and SQLite API."
		name="description"
	>
</svelte:head>

<section class="about">
	<p class="eyebrow">About this demo</p>
	<h1>Static on the outside, Go on the inside.</h1>
	<p class="lede">
		Wasm Wall is a SvelteKit site compiled with <code>adapter-static</code>. The pages are prerendered at build
		time, then embedded into a Go WASI component alongside a small JSON API.
	</p>

	<div class="facts">
		<article>
			<span>01</span>
			<h2>SvelteKit shell</h2>
			<p>The wall and this page are plain static files. No SvelteKit server is running in production.</p>
		</article>
		<article>
			<span>02</span>
			<h2>Go API</h2>
			<p>
				The same component serves <code>/api/messages</code> and the reaction endpoints, backed by the
				supervisor's SQLite service.
			</p>
		</article>
		<article>
			<span>03</span>
			<h2>One wasm bundle</h2>
			<p>
				The built assets and the Go server are packed into a single
				<code>wasi:http</code>
				component and deployed with wt.
			</p>
		</article>
	</div>

	<div class="stack">
		<span>SvelteKit</span>
		<span>adapter-static</span>
		<span>Go</span>
		<span>wasi:http</span>
		<span>SQLite</span>
	</div>

	<a class="back" href="/">← Back to the wall</a>
</section>

<style>
.about {
	max-width: 880px;
}

.eyebrow {
	padding-bottom: 10px;
	font-size: 0.72rem;
	font-weight: 750;
	color: var(--accent);
	text-transform: uppercase;
	letter-spacing: 0.13em;
}

h1 {
	max-width: 760px;
	font-size: clamp(2.2rem, 6vw, 3.8rem);
}

.lede {
	max-width: 720px;
	padding-top: 16px;
	font-size: 1.02rem;
	color: var(--muted);
}

code {
	padding: 1px 5px;
	font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
	font-size: 0.9em;
	background: var(--paper-deep);
	border-radius: 5px;
}

.facts {
	display: grid;
	grid-template-columns: repeat(3, minmax(0, 1fr));
	gap: 12px;
	padding-top: 34px;
}

.facts article {
	padding: 20px;
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: var(--radius);
}

.facts article > span {
	font-size: 0.72rem;
	font-weight: 750;
	color: var(--accent);
	letter-spacing: 0.12em;
}

.facts h2 {
	padding-top: 8px;
	font-size: 1.05rem;
}

.facts p {
	padding-top: 8px;
	font-size: 0.88rem;
	color: var(--muted);
}

.stack {
	display: flex;
	flex-wrap: wrap;
	gap: 7px;
	padding-top: 24px;
}

.about .stack span {
	padding: 5px 10px;
	font-size: 0.76rem;
	font-weight: 650;
	color: var(--teal);
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: 999px;
}

.back {
	display: inline-block;
	padding-top: 30px;
	font-weight: 700;
}

@media (max-width: 760px) {
	.facts {
		grid-template-columns: 1fr;
	}
}
</style>
