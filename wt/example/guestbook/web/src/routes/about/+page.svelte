<svelte:head>
	<title>About · Wasm Wall</title>
	<meta
		name="description"
		content="How the Wasm Wall demo combines a prerendered SvelteKit frontend with a Go and SQLite API."
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
	color: var(--accent);
	font-size: 0.72rem;
	font-weight: 750;
	letter-spacing: 0.13em;
	text-transform: uppercase;
	margin-bottom: 10px;
}

h1 {
	font-size: clamp(2.2rem, 6vw, 3.8rem);
	max-width: 760px;
}

.lede {
	max-width: 720px;
	margin-top: 16px;
	color: var(--muted);
	font-size: 1.02rem;
}

code {
	font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
	font-size: 0.9em;
	background: var(--paper-deep);
	border-radius: 5px;
	padding: 1px 5px;
}

.facts {
	display: grid;
	grid-template-columns: repeat(3, minmax(0, 1fr));
	gap: 12px;
	margin-top: 34px;
}

.facts article {
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: var(--radius);
	padding: 20px;
}

.facts article > span {
	color: var(--accent);
	font-size: 0.72rem;
	font-weight: 750;
	letter-spacing: 0.12em;
}

.facts h2 {
	margin-top: 8px;
	font-size: 1.05rem;
}

.facts p {
	margin-top: 8px;
	color: var(--muted);
	font-size: 0.88rem;
}

.stack {
	display: flex;
	flex-wrap: wrap;
	gap: 7px;
	margin-top: 24px;
}

.stack span {
	border: 1px solid var(--line);
	border-radius: 999px;
	background: var(--card);
	color: var(--teal);
	padding: 5px 10px;
	font-size: 0.76rem;
	font-weight: 650;
}

.back {
	display: inline-block;
	margin-top: 30px;
	font-weight: 700;
}

@media (max-width: 760px) {
	.facts {
		grid-template-columns: 1fr;
	}
}
</style>
