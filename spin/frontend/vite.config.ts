import { svelte } from "@sveltejs/vite-plugin-svelte"
import { defineConfig } from "vite"

export default defineConfig({
	build: {
		emptyOutDir: true,
		manifest: true,
		outDir: "dist",
		rolldownOptions: {
			input: "src/main.ts",
		},
	},
	plugins: [svelte()],
	server: {
		port: 5173,
		strictPort: true,
	},
})
