import adapter from "@sveltejs/adapter-static"
import { sveltekit } from "@sveltejs/kit/vite"
import { defineConfig } from "vite"

export default defineConfig({
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
})
