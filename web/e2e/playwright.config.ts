import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { defineConfig, devices } from '@playwright/test';

// This file's own directory. NOT the repo root, and that distinction is the
// whole reason this comment exists: Playwright resolves a bare relative
// `webServer.command` against `webServer.cwd`, which defaults to the CONFIG
// FILE's directory (`web/e2e/`) if `cwd` is left unset. `build/bakery` from
// there is `web/e2e/build/bakery` -- a path that does not exist -- not the
// real binary at the repo root's `build/bakery`. Setting `cwd` explicitly
// below is what makes `command: 'build/bakery serve'` resolve correctly.
const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, '..', '..');

// `just build` writes here (`justfile`: `go build -o ./build/bakery .`), and
// `just web-e2e` depends on `build`, so this binary is always fresh when the
// suite runs.
const bakeryBin = path.join('build', 'bakery');

export default defineConfig({
	testDir: here,
	fullyParallel: false,

	// A `.only` left in a spec must fail CI, not silently narrow the suite to
	// one test and report green -- the same class of trap the four Go
	// conformance recipes' "a skip FAILS" comments exist to refuse.
	forbidOnly: true,
	retries: 0,
	workers: 1,

	// `list` is the human-readable default for a local run. CI (via `just
	// web-e2e`) overrides this on the command line with
	// `--reporter=list,json`, and the `json` reporter writes its report to
	// `PLAYWRIGHT_JSON_OUTPUT_NAME` when that env var is set -- the justfile
	// recipe sets it and then greps the report with `jq -e
	// '.stats.skipped == 0'`, never a stdout string match (the fragility
	// finding: `--reporter=json` alone interleaves the JSON report with
	// `list` output on stdout, which is not a thing to grep).
	reporter: 'list',

	use: {
		baseURL: 'http://127.0.0.1:8080',
		trace: 'retain-on-failure',
		screenshot: 'only-on-failure'
	},

	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],

	// The real binary, real Postgres, real HTTP -- no mock server. `serve`
	// with no `--headless` so the SPA is routed, exactly what a browser needs.
	webServer: {
		command: `${bakeryBin} serve`,
		cwd: repoRoot,
		url: 'http://127.0.0.1:8080/readyz',

		// Boot order (CLAUDE.md): connect+ping -> advisory lock -> migrate ->
		// ... -> bind listeners -> serve. `/readyz` only starts answering once
		// every one of those has finished, so a plain URL poll is sufficient --
		// there is no "server up but not ready yet" gap to race.
		timeout: 60_000,
		reuseExistingServer: !process.env.CI,

		env: {
			...processEnvAsStrings(),

			// The sanctioned CI/e2e bypass for a live IdP (CLAUDE.md:
			// "DEV_LOGIN_ENABLED is settable only via env var / CLI flag"). It
			// is also what turns the session cookie's `Secure` flag off
			// (`boot.go`), which is what lets this suite work over plain http.
			DEV_LOGIN_ENABLED: '1',

			// Passthrough: `just web-e2e` and the CI job both export
			// TEST_DB_URL (the same variable dbtest and every conformance
			// recipe read), and ServeCmd's DBFlags.DBURL is `required:""` --
			// an empty value here fails loudly at boot, which is correct: a
			// missing database is not a skip.
			DB_URL: process.env.TEST_DB_URL ?? ''
		}
	}
});

// child_process (and Playwright's own webServer launcher) wants
// Record<string, string>; process.env is Record<string, string | undefined>.
// This narrows it without an `as` cast into anything the type system cannot
// verify -- a `string | undefined` value that is `undefined` is simply
// dropped, which is exactly child_process's own default behavior for an
// absent variable.
function processEnvAsStrings(): Record<string, string> {
	const out: Record<string, string> = {};
	for (const [k, v] of Object.entries(process.env)) {
		if (v !== undefined) out[k] = v;
	}
	return out;
}
