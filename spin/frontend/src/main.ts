import { createInertiaApp, type ResolvedComponent } from "@inertiajs/svelte"
import "./app.css"

createInertiaApp({
	resolve: name => {
		const pages = import.meta.glob<ResolvedComponent>("./pages/**/*.svelte")
		const page = pages[`./pages/${name}.svelte`]

		if (!page) {
			throw new Error(`Inertia page not found: ${name}`)
		}

		return page()
	},
})
