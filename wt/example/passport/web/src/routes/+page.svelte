<script lang="ts">
import { onMount } from "svelte"
import {
	ApiError,
	type Checkin,
	createCheckin,
	deleteCheckin,
	fetchCheckins,
	fetchSession,
	logout,
	type Session,
} from "../lib/api"
import { maxMessage } from "../lib/contract"

const authNotices: { [_: string]: string } = {
	denied: "The provider declined the sign-in.",
	exchange: "The provider rejected the code exchange. Please try again.",
	expired: "That login took too long. Please start again.",
	failed: "Something went wrong during sign-in. Please try again.",
	internal: "The desk could not finish the sign-in. Please try again.",
	provider: "The identity provider could not be reached.",
	state: "The login state did not match this browser. Please try again.",
	token: "The identity token could not be verified.",
}

const timeFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" })

let session = $state<Session | null>(null)
let checkins = $state<Checkin[]>([])
let loading = $state(true)
let sending = $state(false)
let signingOut = $state(false)
let pendingDelete = $state("")
let message = $state("")
let error = $state("")
let fieldError = $state("")
let notice = $state("")

let signedIn = $derived(session?.authenticated === true)
let remaining = $derived(maxMessage - message.length)
let initials = $derived(initialsOf(session?.userName || session?.userUsername || "Visitor"))

function initialsOf(value: string): string {
	const words = value.trim().split(/\s+/).filter(Boolean)
	if (words.length === 0) return "?"
	return words
		.slice(0, 2)
		.map(word => word[0]?.toUpperCase() ?? "")
		.join("")
}

function formatTime(value: string): string {
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? "—" : timeFormat.format(date)
}

async function refresh(): Promise<void> {
	try {
		session = await fetchSession()
		checkins = await fetchCheckins(50)
		error = ""
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not reach the passport API."
	} finally {
		loading = false
	}
}

onMount((): void => {
	const params = new URLSearchParams(location.search)
	const auth = params.get("auth")
	if (auth) {
		notice = authNotices[auth] ?? authNotices.failed
		history.replaceState(null, "", location.pathname)
	}
	void refresh()
})

async function submit(event: SubmitEvent): Promise<void> {
	event.preventDefault()
	if (sending) return
	error = ""
	fieldError = ""

	const text = message.trim()
	if (text === "") {
		fieldError = "Write a few words."
		return
	}
	if (text.length > maxMessage) {
		fieldError = `Check-ins are limited to ${maxMessage} characters.`
		return
	}

	sending = true
	try {
		const entry = await createCheckin(text)
		checkins = [entry, ...checkins].slice(0, 50)
		message = ""
	} catch (err) {
		if (err instanceof ApiError && err.field === "message") fieldError = err.message
		else error = err instanceof Error ? err.message : "Could not save the check-in."
	} finally {
		sending = false
	}
}

async function remove(entry: Checkin): Promise<void> {
	error = ""
	try {
		await deleteCheckin(entry.id)
		checkins = checkins.filter(item => item.id !== entry.id)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not remove the check-in."
	} finally {
		pendingDelete = ""
	}
}

async function signOut(): Promise<void> {
	if (signingOut) return
	signingOut = true
	error = ""
	try {
		const { endSessionUrl } = await logout()
		if (endSessionUrl !== "") {
			location.assign(endSessionUrl)
			return
		}
		session = await fetchSession()
		checkins = await fetchCheckins(50)
	} catch (err) {
		error = err instanceof Error ? err.message : "Could not sign out."
	} finally {
		signingOut = false
	}
}
</script>

<svelte:head>
	<title>Passport · an OAuth 2.0 check-in desk</title>
	<meta content="A tiny check-in wall behind a real OAuth 2.0 authorization code flow with PKCE." name="description">
</svelte:head>

<div class="passport-page">
	<section class="hero">
		<div class="hero-copy">
			<p class="eyebrow"><span aria-hidden="true" class="pip"></span> OAuth 2.0 entry desk</p>
			<h1>Show your <em>papers</em>.<br>Leave a mark.</h1>
			<p class="lede">
				Passport signs you in through a real OpenID Connect provider, keeps the tokens on the server, and gives
				you a one-line visa for the visitor ledger.
			</p>
			<div class="hero-actions">
				{#if signedIn}
					<a class="button" href="#wall">Read the wall <span aria-hidden="true">→</span></a>
				{:else}
					<form action="/auth/login" class="signin" method="post">
						<button class="button" type="submit">
							Sign in with OAuth <span aria-hidden="true">→</span>
						</button>
					</form>
				{/if}
				<a class="text-link" href="#how">How the flow works</a>
			</div>
		</div>

		<div aria-hidden="true" class="cover-wrap">
			<div class="passport-cover">
				<div class="cover-grain"></div>
				<div class="cover-inner">
					<span class="cover-kicker">Demonstration travel document</span>
					<span class="cover-title">Passport</span>
					<span class="cover-rule"></span>
					<span class="cover-sub">OAuth 2.0 · authorization code + PKCE</span>
					<div class="cover-seal"><span>WT</span></div>
					<span class="cover-serial">client · {session?.clientId ?? "interactive.public"}</span>
				</div>
			</div>
		</div>
	</section>

	{#if notice}
		<p class="notice" role="status">{notice}</p>
	{/if}
	{#if error}
		<p class="notice notice-error" role="alert">{error}</p>
	{/if}

	<section aria-label="Your identity" class="desk">
		{#if signedIn && session}
			<article class="identity-card">
				<div class="identity-head">
					{#if session.userPicture}
						<img alt="" class="avatar" src={session.userPicture}>
					{:else}
						<span class="stamp">{initials}</span>
					{/if}
					<div>
						<p class="label">Bearer of this session</p>
						<h2>{session.userName || session.userUsername}</h2>
						<p class="email">{session.userEmail || "No email claim"}</p>
					</div>
				</div>

				<dl class="facts">
					<div>
						<dt>Subject</dt>
						<dd>{session.userSub}</dd>
					</div>
					<div>
						<dt>Username</dt>
						<dd>{session.userUsername || "—"}</dd>
					</div>
					<div>
						<dt>Zone</dt>
						<dd>{session.userZoneinfo || "—"}</dd>
					</div>
					<div>
						<dt>Session expires</dt>
						<dd>{formatTime(session.expiresAt)}</dd>
					</div>
				</dl>

				<div class="scope-row">
					<span class="label">Granted scopes</span>
					<div class="chips">
						{#each session.scopes as scope (scope)}
							<span class="chip">{scope}</span>
						{/each}
					</div>
				</div>

				<button class="button button-ghost" disabled={signingOut} onclick={signOut} type="button">
					{signingOut ? "Closing the booth…" : "Sign out"}
				</button>
			</article>
		{:else}
			<article class="identity-card">
				<p class="label">Issuing authority</p>
				<h2>{session?.providerName ?? "Identity provider"}</h2>
				<p class="issuer">{session?.providerIssuer ?? "Connecting to the provider…"}</p>

				<dl class="facts">
					<div>
						<dt>Client</dt>
						<dd>{session?.clientId ?? "interactive.public"}</dd>
					</div>
					<div>
						<dt>Flow</dt>
						<dd>Authorization code + PKCE</dd>
					</div>
					<div>
						<dt>Scopes</dt>
						<dd>{session?.scopes.join(" · ") ?? "openid · profile · email"}</dd>
					</div>
					<div>
						<dt>Token storage</dt>
						<dd>Server-side session only</dd>
					</div>
				</dl>

				<form action="/auth/login" class="signin" method="post">
					<button class="button" type="submit">
						Sign in at the provider <span aria-hidden="true">→</span>
					</button>
				</form>
				<p class="hint">
					On the Duende demo, use <code>alice</code> / <code>alice</code> (or <code>bob</code> /
					<code>bob</code>).
				</p>
			</article>
		{/if}

		<article class="checkin-panel">
			{#if signedIn}
				<p class="label">New entry</p>
				<h2>Sign the ledger</h2>
				<form onsubmit={submit}>
					<label for="message">A note for the next traveller</label>
					<textarea
						id="message"
						maxlength={maxMessage}
						placeholder="Landed safely. The coffee is terrible."
						rows="4"
						class:invalid={fieldError !== ""}
						bind:value={message}
					></textarea>
					<div class="form-foot">
						<span class:over={remaining < 0}>{remaining} left</span>
						<button class="button" disabled={sending} type="submit">
							{sending ? "Stamping…" : "Add check-in"}
						</button>
					</div>
					{#if fieldError}
						<p class="field-error" role="alert">{fieldError}</p>
					{/if}
				</form>
			{:else}
				<p class="label">Visitor ledger</p>
				<h2>Read freely. Write with a stamp.</h2>
				<p class="panel-copy">
					The wall below is public. Adding an entry takes a valid OAuth session, which is why the provider has
					to vouch for you first.
				</p>
				<ul class="stamp-list">
					<li><span>01</span> Authorize at the provider</li>
					<li><span>02</span> Return with a one-time code</li>
					<li><span>03</span> Exchange it back-channel</li>
					<li><span>04</span> Post with your session cookie</li>
				</ul>
			{/if}
		</article>
	</section>

	<section class="how" id="how">
		<header class="section-head">
			<p class="eyebrow">Under the cover</p>
			<h2>What happens when you sign in</h2>
			<p>The browser only ever receives an opaque session cookie. The tokens stay in the Go component.</p>
		</header>

		<ol class="flow-list">
			<li>
				<span class="step-number">01</span>
				<div>
					<h3>Depart</h3>
					<p>
						<code>/auth/login</code>
						creates a random state, nonce and PKCE verifier. Only the SHA-256 challenge travels to the
						authorization endpoint.
					</p>
				</div>
			</li>
			<li>
				<span class="step-number">02</span>
				<div>
					<h3>Identify</h3>
					<p>
						The provider authenticates you and redirects back with a one-time code. The state cookie must
						match the state in the URL, or the callback is rejected.
					</p>
				</div>
			</li>
			<li>
				<span class="step-number">03</span>
				<div>
					<h3>Verify</h3>
					<p>
						The component exchanges the code back-channel, checks the ID token's RS256 signature against the
						provider's JWKS, then calls UserInfo with the access token.
					</p>
				</div>
			</li>
			<li>
				<span class="step-number">04</span>
				<div>
					<h3>Stamp</h3>
					<p>
						A random session token is stored as a hash in SQLite. Its plain value lives only in an HttpOnly,
						SameSite cookie, and every write checks it.
					</p>
				</div>
			</li>
		</ol>
	</section>

	<section class="wall" id="wall">
		<header class="section-head wall-head">
			<div>
				<p class="eyebrow">Visitor ledger</p>
				<h2>Recent check-ins</h2>
			</div>
			<p class="entry-count">{checkins.length} {checkins.length === 1 ? "entry" : "entries"}</p>
		</header>

		{#if loading}
			<p class="empty">Opening the ledger…</p>
		{:else if checkins.length === 0}
			<p class="empty">No stamps yet. Be the first to sign in and leave a mark.</p>
		{:else}
			<div class="entries">
				{#each checkins as entry (entry.id)}
					<article class="entry">
						<span class="stamp stamp-small">{initialsOf(entry.authorName)}</span>
						<div class="entry-body">
							<header>
								<strong>{entry.authorName}</strong>
								<time datetime={entry.createdAt}>{formatTime(entry.createdAt)}</time>
							</header>
							<p>{entry.message}</p>
						</div>
						{#if entry.mine}
							{#if pendingDelete === entry.id}
								<div class="remove-group">
									<button class="remove remove-yes" onclick={() => remove(entry)} type="button">
										Remove
									</button>
									<button
										aria-label="Keep this check-in"
										class="remove"
										onclick={() => (pendingDelete = "")}
										title="Keep this check-in"
										type="button"
									>
										×
									</button>
								</div>
							{:else}
								<button
									aria-label={`Remove check-in from ${entry.authorName}`}
									class="remove"
									onclick={() => (pendingDelete = entry.id)}
									title="Remove this check-in"
									type="button"
								>
									×
								</button>
							{/if}
						{/if}
					</article>
				{/each}
			</div>
		{/if}
	</section>
</div>

<style>
.passport-page {
	display: flex;
	flex-direction: column;
	gap: 54px;
	padding: 18px 0 40px;
}

.hero {
	display: grid;
	grid-template-columns: minmax(0, 1.15fr) minmax(280px, 0.85fr);
	gap: clamp(28px, 5vw, 70px);
	align-items: center;
	padding: 28px 0 8px;
}

.eyebrow {
	display: flex;
	gap: 10px;
	align-items: center;
	font-family: var(--mono);
	font-size: 0.72rem;
	font-weight: 600;
	color: var(--accent);
	text-transform: uppercase;
	letter-spacing: 0.16em;
}

.pip {
	width: 7px;
	height: 7px;
	background: var(--accent);
	border-radius: 50%;
	box-shadow: 0 0 0 4px var(--accent-soft);
}

.hero h1 {
	margin-top: 18px;
	font-size: clamp(2.7rem, 6vw, 4.9rem);
	font-weight: 500;
	color: var(--night);
}

.hero h1 em {
	font-style: italic;
	color: var(--accent);
}

.lede {
	max-width: 42ch;
	margin-top: 20px;
	font-size: 1.05rem;
	color: var(--muted);
}

.hero-actions {
	display: flex;
	flex-wrap: wrap;
	gap: 20px;
	align-items: center;
	margin-top: 28px;
}

.cover-wrap {
	display: flex;
	justify-content: center;
}

.passport-cover {
	position: relative;
	width: min(100%, 330px);
	aspect-ratio: 0.74;
	padding: 14px;
	background: linear-gradient(145deg, #1d3352, #101d31 60%, #0c1626);
	border-radius: 8px 18px 18px 8px;
	box-shadow:
		0 26px 55px rgba(15, 24, 40, 0.32),
		inset 0 0 0 1px rgba(255, 255, 255, 0.12);
	transform: rotate(2.5deg);
}

.cover-grain {
	position: absolute;
	inset: 10px;
	pointer-events: none;
	background-image:
		repeating-linear-gradient(115deg, rgba(255, 255, 255, 0.035) 0 1px, transparent 1px 7px),
		repeating-linear-gradient(25deg, rgba(255, 255, 255, 0.02) 0 1px, transparent 1px 11px);
	border-radius: 4px 12px 12px 4px;
	opacity: 0.8;
}

.cover-inner {
	position: relative;
	display: flex;
	flex-direction: column;
	align-items: center;
	height: 100%;
	padding: 30px 22px 24px;
	color: #efe4ca;
	text-align: center;
	border: 1px solid rgba(211, 180, 118, 0.55);
	border-radius: 3px 10px 10px 3px;
}

.cover-kicker {
	font-family: var(--mono);
	font-size: 0.58rem;
	color: #c7a66b;
	text-transform: uppercase;
	letter-spacing: 0.2em;
}

.cover-title {
	margin-top: auto;
	font-family: var(--serif);
	font-size: 2.35rem;
	font-weight: 600;
	text-transform: uppercase;
	letter-spacing: 0.14em;
}

.cover-rule {
	width: 44px;
	height: 1px;
	margin: 14px 0 12px;
	background: #c7a66b;
}

.cover-sub {
	font-family: var(--mono);
	font-size: 0.63rem;
	line-height: 1.7;
	color: #b9c6d7;
	letter-spacing: 0.05em;
}

.cover-seal {
	display: grid;
	place-items: center;
	width: 68px;
	height: 68px;
	margin: 26px 0 auto;
	font-family: var(--serif);
	font-size: 1.2rem;
	color: #c7a66b;
	letter-spacing: 0.1em;
	border: 1px solid #c7a66b;
	border-radius: 50%;
	box-shadow:
		inset 0 0 0 5px #152741,
		inset 0 0 0 6px rgba(199, 166, 107, 0.5);
}

.cover-serial {
	margin-top: 22px;
	font-family: var(--mono);
	font-size: 0.58rem;
	color: #94a6bd;
	letter-spacing: 0.12em;
}

.notice {
	padding: 13px 18px;
	font-size: 0.9rem;
	color: #63451f;
	background: #fbf0df;
	border: 1px solid #d9b8a0;
	border-left: 4px solid var(--gold);
}

.notice-error {
	color: #6f262c;
	background: #fbebe9;
	border-color: #ddb6b2;
	border-left-color: var(--accent);
}

.desk {
	display: grid;
	grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
	gap: 24px;
}

.identity-card,
.checkin-panel {
	display: flex;
	flex-direction: column;
	gap: 18px;
	padding: clamp(24px, 3.4vw, 38px);
	background: var(--card);
	border: 1px solid var(--line);
	border-radius: var(--radius);
	box-shadow: var(--shadow);
}

.identity-card {
	position: relative;
}

.identity-card::after {
	position: absolute;
	inset: 7px;
	pointer-events: none;
	content: "";
	border: 1px dashed rgba(140, 47, 57, 0.22);
	border-radius: calc(var(--radius) - 6px);
}

.identity-head {
	display: flex;
	gap: 18px;
	align-items: center;
}

.avatar,
.stamp {
	display: grid;
	flex: 0 0 auto;
	place-items: center;
	width: 68px;
	height: 68px;
	font-family: var(--serif);
	font-size: 1.3rem;
	color: var(--accent);
	object-fit: cover;
	background: var(--accent-soft);
	border: 1px solid var(--accent);
	border-radius: 50%;
	box-shadow:
		inset 0 0 0 4px var(--card),
		inset 0 0 0 5px rgba(140, 47, 57, 0.35);
}

.stamp-small {
	width: 42px;
	height: 42px;
	font-size: 0.85rem;
	box-shadow:
		inset 0 0 0 3px var(--card),
		inset 0 0 0 4px rgba(140, 47, 57, 0.35);
}

.label {
	font-family: var(--mono);
	font-size: 0.66rem;
	font-weight: 600;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.13em;
}

.identity-card h2,
.checkin-panel h2 {
	font-size: clamp(1.45rem, 2.6vw, 1.9rem);
	color: var(--night);
}

.email,
.issuer {
	margin-top: 3px;
	font-family: var(--mono);
	font-size: 0.76rem;
	color: var(--muted);
	word-break: break-word;
}

.facts {
	display: grid;
	gap: 10px;
	padding-top: 4px;
	margin: 0;
}

.facts div {
	display: grid;
	grid-template-columns: 8.4rem minmax(0, 1fr);
	gap: 12px;
	padding-bottom: 9px;
	border-bottom: 1px solid var(--line);
}

.facts dt {
	font-family: var(--mono);
	font-size: 0.65rem;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.09em;
}

.facts dd {
	margin: 0;
	font-family: var(--mono);
	font-size: 0.76rem;
	color: var(--ink);
	overflow-wrap: anywhere;
}

.scope-row {
	display: flex;
	flex-direction: column;
	gap: 8px;
}

.chips {
	display: flex;
	flex-wrap: wrap;
	gap: 7px;
}

.chip {
	padding: 3px 9px;
	font-family: var(--mono);
	font-size: 0.65rem;
	color: var(--teal);
	background: var(--teal-soft);
	border: 1px solid var(--line-strong);
	border-radius: 999px;
}

.signin {
	display: flex;
	align-self: flex-start;
}

.button {
	display: inline-flex;
	gap: 10px;
	align-items: center;
	align-self: flex-start;
	justify-content: center;
	padding: 11px 18px;
	font-size: 0.88rem;
	font-weight: 650;
	color: #fff8ee;
	letter-spacing: 0.01em;
	cursor: pointer;
	background: var(--accent);
	border: 1px solid var(--accent-deep);
	border-radius: 999px;
	transition:
		background 120ms ease,
		transform 120ms ease;
}

.button:hover {
	text-decoration: none;
	background: var(--accent-deep);
	transform: translateY(-1px);
}

.button:disabled {
	cursor: progress;
	opacity: 0.55;
	transform: none;
}

.button-ghost {
	color: var(--accent);
	background: transparent;
	border-color: var(--accent);
}

.button-ghost:hover {
	background: var(--accent-soft);
}

.text-link {
	font-size: 0.86rem;
	color: var(--teal);
	text-decoration: underline;
	text-underline-offset: 3px;
}

.hint {
	font-size: 0.8rem;
	line-height: 1.6;
	color: var(--muted);
}

code {
	padding: 1px 5px;
	font-family: var(--mono);
	font-size: 0.86em;
	color: var(--night);
	background: var(--paper-deep);
	border-radius: 4px;
}

.panel-copy {
	font-size: 0.95rem;
	color: var(--muted);
}

form {
	display: flex;
	flex-direction: column;
	gap: 10px;
}

form label {
	font-size: 0.83rem;
	font-weight: 600;
	color: var(--night);
}

textarea {
	width: 100%;
	padding: 13px 14px;
	font: inherit;
	font-size: 0.92rem;
	line-height: 1.55;
	color: var(--ink);
	resize: vertical;
	background: #fffdf7;
	border: 1px solid var(--line-strong);
	border-radius: 10px;
}

textarea:focus {
	outline: 2px solid rgba(140, 47, 57, 0.38);
	outline-offset: 1px;
	border-color: var(--accent);
}

textarea.invalid {
	background: #fff6f4;
	border-color: var(--accent);
}

.form-foot {
	display: flex;
	gap: 14px;
	align-items: center;
	justify-content: space-between;
	font-family: var(--mono);
	font-size: 0.7rem;
	color: var(--muted);
}

.form-foot .over {
	color: var(--accent);
}

.field-error {
	font-size: 0.8rem;
	color: var(--accent);
}

.stamp-list {
	display: flex;
	flex-direction: column;
	gap: 10px;
	padding: 0;
	margin: 4px 0 0;
	list-style: none;
}

.stamp-list li {
	display: flex;
	gap: 12px;
	align-items: center;
	padding: 9px 0;
	font-size: 0.86rem;
	color: var(--ink);
	border-bottom: 1px dashed var(--line);
}

.stamp-list span {
	font-family: var(--mono);
	font-size: 0.66rem;
	color: var(--gold);
}

.section-head {
	max-width: 62ch;
}

.section-head h2 {
	margin-top: 10px;
	font-size: clamp(1.7rem, 3.4vw, 2.5rem);
	color: var(--night);
}

.section-head > p:last-child {
	margin-top: 10px;
	font-size: 0.94rem;
	color: var(--muted);
}

.flow-list {
	display: grid;
	grid-template-columns: repeat(4, minmax(0, 1fr));
	gap: 0;
	padding: 0;
	margin: 30px 0 0;
	list-style: none;
	border-top: 1px solid var(--line-strong);
}

.flow-list li {
	display: flex;
	flex-direction: column;
	gap: 14px;
	padding: 22px 22px 0 0;
}

.flow-list li + li {
	padding-left: 22px;
	border-left: 1px solid var(--line);
}

.step-number {
	font-family: var(--mono);
	font-size: 0.7rem;
	color: var(--gold);
	letter-spacing: 0.1em;
}

.flow-list h3 {
	font-size: 1.15rem;
	color: var(--night);
}

.flow-list p {
	margin-top: 8px;
	font-size: 0.84rem;
	line-height: 1.65;
	color: var(--muted);
}

.wall {
	padding-top: 6px;
}

.wall-head {
	display: flex;
	gap: 20px;
	align-items: end;
	justify-content: space-between;
	max-width: none;
	padding-bottom: 16px;
	border-bottom: 1px solid var(--line-strong);
}

.entry-count {
	font-family: var(--mono);
	font-size: 0.72rem;
	color: var(--muted);
	letter-spacing: 0.08em;
}

.entries {
	display: flex;
	flex-direction: column;
}

.entry {
	display: flex;
	gap: 16px;
	align-items: flex-start;
	padding: 20px 4px;
	border-bottom: 1px solid var(--line);
}

.entry-body {
	flex: 1;
	min-width: 0;
}

.entry header {
	display: flex;
	flex-wrap: wrap;
	gap: 8px 16px;
	align-items: baseline;
}

.entry strong {
	font-size: 0.94rem;
	color: var(--night);
}

.entry time {
	font-family: var(--mono);
	font-size: 0.66rem;
	color: var(--muted);
}

.entry p {
	margin-top: 6px;
	font-size: 0.94rem;
	color: var(--ink);
	overflow-wrap: anywhere;
}

.remove {
	flex: 0 0 auto;
	width: 28px;
	height: 28px;
	font-size: 1rem;
	line-height: 1;
	color: var(--muted);
	cursor: pointer;
	background: transparent;
	border: 1px solid var(--line-strong);
	border-radius: 50%;
}

.remove:hover {
	color: var(--accent);
	background: var(--accent-soft);
	border-color: var(--accent);
}

.empty {
	padding: 34px 4px;
	font-size: 0.92rem;
	color: var(--muted);
}

@media (max-width: 860px) {
	.hero {
		grid-template-columns: 1fr;
	}

	.cover-wrap {
		justify-content: flex-start;
		order: -1;
	}

	.passport-cover {
		width: min(64vw, 260px);
	}

	.desk {
		grid-template-columns: 1fr;
	}

	.flow-list {
		grid-template-columns: 1fr 1fr;
	}

	.flow-list li:nth-child(3) {
		padding-left: 0;
		border-left: 0;
	}

	.flow-list li:nth-child(n + 3) {
		padding-top: 26px;
		border-top: 1px solid var(--line);
	}
}

@media (max-width: 540px) {
	.flow-list {
		grid-template-columns: 1fr;
	}

	.flow-list li,
	.flow-list li + li {
		padding: 18px 0 0;
		border-left: 0;
	}

	.flow-list li + li {
		border-top: 1px solid var(--line);
	}

	.facts div {
		grid-template-columns: 1fr;
		gap: 2px;
	}

	.wall-head {
		flex-direction: column;
		align-items: start;
	}
}
</style>
