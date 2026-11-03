import adapter from "@sveltejs/adapter-static"
import { sveltekit } from "@sveltejs/kit/vite"
import { defineConfig } from "vite"

export default defineConfig({
	// wt dev runs Vite beside the supervisor, so keep both logs on screen.
	clearScreen: false,
	plugins: [
		sveltekit({
			adapter: adapter({
				precompress: true,
			}),
			compilerOptions: {
				cssHash: ({ css, filename, hash }) => `s-${hash(filename ?? css)}`,
				experimental: {
					async: true,
				},
				modernAst: true,
				runes: true,
			},
		}),
	],
	server: {
		// Fail instead of silently moving to another port when the configured one is taken.
		strictPort: true,
	},
})
