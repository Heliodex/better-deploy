import adapter from "@sveltejs/adapter-static"
import { sveltekit } from "@sveltejs/kit/vite"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "vite"

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			adapter: adapter({
				assets: "build",
				fallback: undefined,
				pages: "build",
				precompress: false,
				strict: true,
			}),
			compilerOptions: {
				runes: ({ filename }) => (filename.split(/[/\\]/).includes("node_modules") ? undefined : true),
			},
			paths: {
				base: "/admin",
			},
		}),
	],
	server: {
		proxy: {
			"/apps": "http://127.0.0.1:8080",
		},
	},
})
