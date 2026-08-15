import tailwindcss from '@tailwindcss/vite';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

// The dev proxy exists so the browser only ever talks to one origin.
//
// The console authenticates with a `SameSite=Lax`, HttpOnly session cookie and
// never handles a bearer token; there is no CORS in this product and none
// should be added. Proxying `/api/v1` and `/cache` through the dev server keeps
// development the same shape as production -- one binary, one port, one origin
// -- instead of introducing a second origin that only exists on a laptop.
// Hard-coded rather than read from an env var: `@types/node` is not a
// dependency of this package and adding one to make `process.env` type-check
// would be a real dependency bought for a development convenience. `just run`
// binds :8080.
const BACKEND = 'http://127.0.0.1:8080';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	server: {
		proxy: {
			'/api/v1': { target: BACKEND, changeOrigin: false },
			'/cache': { target: BACKEND, changeOrigin: false }
		}
	},
	test: {
		environment: 'node',
		include: ['src/**/*.{test,spec}.{js,ts}']
	}
});
