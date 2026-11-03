<script lang="ts">
import { onMount } from "svelte"
import { ApiError, completeLogin } from "../../../lib/api"

let status = $state("Checking your papers…")

onMount((): void => {
	const params = new URLSearchParams(location.search)

	if (params.get("error") !== null) {
		location.replace("/?auth=denied")
		return
	}

	const code = params.get("code") ?? ""
	const state = params.get("state") ?? ""
	if (code === "" || state === "") {
		location.replace("/?auth=failed")
		return
	}

	void (async () => {
		try {
			await completeLogin(code, state)
			location.replace("/")
		} catch (err) {
			status = "The desk could not finish the exchange."
			const reason = err instanceof ApiError && err.code !== "" ? err.code : "failed"
			setTimeout(() => location.replace(`/?auth=${encodeURIComponent(reason)}`), 900)
		}
	})()
})
</script>

<svelte:head>
	<title>Completing sign-in · Passport</title>
	<meta content="noindex" name="robots">
</svelte:head>

<div class="callback">
	<div aria-hidden="true" class="seal">P</div>
	<p class="eyebrow">Authorisation callback</p>
	<h1>{status}</h1>
	<p class="note">
		The one-time code is being exchanged on the server. The browser is about to receive an HttpOnly session cookie
		and return to the ledger.
	</p>
</div>

<style>
.callback {
	display: flex;
	flex-direction: column;
	gap: 14px;
	align-items: center;
	max-width: 520px;
	padding: 110px 24px 140px;
	margin: 0 auto;
	text-align: center;
}

.seal {
	display: grid;
	place-items: center;
	width: 62px;
	height: 62px;
	font-family: var(--serif);
	font-size: 1.3rem;
	color: var(--accent);
	background: var(--accent-soft);
	border: 1px solid var(--accent);
	border-radius: 50%;
	box-shadow:
		inset 0 0 0 4px var(--paper),
		inset 0 0 0 5px rgba(140, 47, 57, 0.35);
}

.eyebrow {
	margin-top: 8px;
	font-family: var(--mono);
	font-size: 0.66rem;
	font-weight: 600;
	color: var(--accent);
	text-transform: uppercase;
	letter-spacing: 0.15em;
}

h1 {
	font-size: clamp(1.8rem, 4vw, 2.6rem);
	color: var(--night);
}

.note {
	max-width: 44ch;
	font-size: 0.9rem;
	color: var(--muted);
}
</style>
