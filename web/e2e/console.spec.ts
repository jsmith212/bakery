import { expect, test, type Locator, type Page } from '@playwright/test';

// The DESIGN.md:545 flow, driven against the real `bakery serve` binary and a
// real Chromium (see playwright.config.ts). Every step below is a click or a
// fill against the actual console -- no route in this flow is reached by
// constructing a URL for a screen the app itself would navigate to.
//
// A fresh org+project pair is minted per run (see `unique`) so the suite is
// safe to run repeatedly against one long-lived Postgres, the way a laptop
// dev loop would use it -- CI gets a fresh database per job either way.

function unique(prefix: string): string {
	return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

/**
 * Locates a `<CodeBlock title="...">`'s `<pre>` by its rendered title, not by
 * DOM position. `CodeBlock`'s outer wrapper is the only `rounded-2` ancestor
 * carrying the title text as a descendant (the page's own layout containers
 * around it are not `rounded-2`), so this stays correct however many blocks
 * the response emits or in what order.
 */
function codeBlock(page: Page, title: string): Locator {
	return page.locator('div.rounded-2', { hasText: title }).locator('pre');
}

/**
 * `ConsoleNav`, scoped, because two of its labels are near-duplicates of
 * prose elsewhere on the page (the keys screen's own footer sentence links to
 * "config snippets" in lower case, which Playwright's default
 * case-insensitive substring match treats as the same accessible name as the
 * nav's "Config snippets" link) -- go through the nav landmark, never a bare
 * page-wide `getByRole('link', ...)`, for anything that lives in it.
 */
function consoleNav(page: Page): Locator {
	return page.getByRole('navigation', { name: 'Console' });
}

async function signInWithoutAuth(page: Page) {
	await page.goto('/login');
	await page.getByRole('button', { name: 'Sign in without auth' }).click();
	// Proves the (console) layout's `/me` guard passed and the shell painted --
	// not just that the click fired.
	await expect(consoleNav(page)).toBeVisible();
}

async function createOrg(page: Page, slug: string): Promise<void> {
	await page.goto('/orgs');
	await page.getByRole('button', { name: 'New organization' }).click();

	const dialog = page.getByRole('dialog');
	await dialog.locator('input[placeholder="acme"]').fill(slug);
	await dialog.locator('input[placeholder="Acme"]').fill(`E2E ${slug}`);
	await dialog.getByRole('button', { name: 'Create organization' }).click();

	await page.waitForURL(`**/o/${slug}/projects`);
}

async function createProject(page: Page, orgSlug: string, projectSlug: string): Promise<void> {
	await page.getByRole('button', { name: 'New project' }).click();

	const dialog = page.getByRole('dialog');
	await dialog.locator('input[placeholder="my-project"]').fill(projectSlug);
	await dialog.locator('input[placeholder="My project"]').fill(`E2E ${projectSlug}`);
	await dialog.getByRole('button', { name: 'Create project' }).click();
	await expect(dialog).toBeHidden();

	// createProject leaves the caller on the projects grid (no navigation) --
	// open the new card to reach its overview, exactly as a human would.
	await page.locator(`a[href="/o/${orgSlug}/p/${projectSlug}/overview"]`).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/overview`);
}

/** Creates a backend from the project's Overview "Add a backend" empty state. */
async function addFirstBackend(page: Page, orgSlug: string, projectSlug: string): Promise<void> {
	await page.getByRole('button', { name: 'Add a backend' }).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends/new`);
	// sstate is the default-selected kind for a project with none configured
	// yet -- no tile click needed.
	await page.getByRole('button', { name: 'Create backend' }).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends/sstate`);
}

/**
 * Adds another backend of `kind`, entirely through the UI: the nav's
 * "Backends" link, the index's "Add backend" action, then the form.
 *
 * This used to `page.goto('.../backends/new')` because there was no in-app
 * link to it once a project had any backend at all -- the nav pointed at the
 * project's first configured KIND and the "add" affordance existed only in
 * Overview's empty state, which by definition was gone. The backends index is
 * that missing link, so the hop is a click again, and `existing` asserts the
 * index really lists what the project already has rather than merely routing.
 */
async function addBackend(
	page: Page,
	orgSlug: string,
	projectSlug: string,
	kind: string,
	existing: string[]
): Promise<void> {
	await consoleNav(page).getByRole('link', { name: 'Backends' }).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends`);

	for (const configured of existing) {
		await expect(page.getByRole('row').filter({ hasText: configured })).toBeVisible();
	}

	// A link, not a button: `Button href` renders an `<a>` so a link never
	// wraps a `<button>` (FOUNDATION.md's Button contract).
	await page.getByRole('link', { name: 'Add backend' }).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends/new`);

	await page.getByRole('button', { name: kind }).click();
	await page.getByRole('button', { name: 'Create backend' }).click();
	await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends/${kind}`);
}

test.describe('console: dev-login through config snippets', () => {
	test('org -> project -> sstate+hashserv backends -> key -> yocto snippet with hashserv', async ({
		page
	}) => {
		const orgSlug = unique('e2e-org');
		const projectSlug = 'firmware';

		await test.step('dev-login', async () => {
			await signInWithoutAuth(page);
		});

		await test.step('create org', async () => {
			await createOrg(page, orgSlug);
		});

		await test.step('create project', async () => {
			await createProject(page, orgSlug, projectSlug);
		});

		await test.step('create sstate backend', async () => {
			await addFirstBackend(page, orgSlug, projectSlug);
		});

		await test.step('create hashserv backend', async () => {
			await addBackend(page, orgSlug, projectSlug, 'hashserv', ['sstate']);
		});

		await test.step('the backends index lists both', async () => {
			await consoleNav(page).getByRole('link', { name: 'Backends' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends`);

			await expect(page.getByRole('row').filter({ hasText: 'sstate' })).toBeVisible();
			await expect(page.getByRole('row').filter({ hasText: 'hashserv' })).toBeVisible();
			// Four kinds are still unconfigured, so the action stays live -- the
			// disabled "All N kinds configured" state is the other branch.
			await expect(page.getByRole('link', { name: 'Add backend' })).toBeVisible();
		});

		await test.step('mint a project API key', async () => {
			await consoleNav(page).getByRole('link', { name: 'API keys' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/keys`);

			await page.getByRole('button', { name: 'Create key' }).click();
			const createDialog = page.getByRole('dialog');
			await createDialog
				.locator('input[placeholder="ci-writer"]')
				.fill('e2e-key');
			// Scope and expiry keep their defaults (write, no expiry) -- the
			// project's creator is its project admin (granted in the same
			// transaction as project creation), so `write` is within ceiling.
			await createDialog.getByRole('button', { name: 'Create key' }).click();

			const revealDialog = page.getByRole('dialog');
			await expect(
				revealDialog.getByText('this is the only time you will see the secret')
			).toBeVisible();

			const token = (await revealDialog.locator('pre').first().textContent())?.trim() ?? '';
			expect(token).toMatch(/^bkry_/);

			await revealDialog.getByLabel(/I have stored the secret/).check();
			await revealDialog.getByRole('button', { name: 'Done' }).click();
			await expect(revealDialog).toBeHidden();
		});

		await test.step('yocto snippet preview: BB_HASHSERVE and both netrc lines', async () => {
			await consoleNav(page).getByRole('link', { name: 'Config snippets' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/snippets`);

			// 'yocto' is the default-selected tile; its preview fires on load.
			const localConf = codeBlock(page, 'conf/local.conf');
			await expect(localConf).toContainText('BB_SIGNATURE_HANDLER = "OEEquivHash"');
			await expect(localConf).toContainText('BB_HASHSERVE');

			const netrc = codeBlock(page, '~/.netrc');
			const netrcText = (await netrc.textContent()) ?? '';
			const machineLines = netrcText.split('\n').filter((l) => l.startsWith('machine '));
			// One hostname-keyed line (sstate/downloads HTTP Basic) and one
			// full-URL-keyed line (hash equivalence) -- collapsing them into
			// one is "the single most common way to get a silently
			// unauthenticated build" (client-config.md, quoted in
			// snippets.go's hashservNetrcLine doc).
			// `ws://` over the loopback http the webServer answers on, `wss://`
			// behind TLS -- either is the full-URL-keyed line; the assertion
			// is about the SECOND, distinct line existing, not the scheme.
			expect(machineLines).toHaveLength(2);
			expect(machineLines.some((l) => /\bwss?:\/\//.test(l))).toBe(true);
			expect(machineLines.some((l) => !/\bwss?:\/\//.test(l))).toBe(true);

			// Still a preview: no key was minted for this snippet response
			// specifically (the reveal above was a separate, explicit mint).
			await expect(page.getByText('Previewing with a placeholder credential')).toBeVisible();
		});
	});

	// TEARDOWN, end to end (000018): the project Settings screen, a backend deleted
	// from its own danger zone, and the project deleted from Settings.
	//
	// Both deletes here take the FAST path -- everything created in this test is
	// empty, so the server deletes outright and answers 204. That is deliberate: the
	// 202 teardown path's whole subject is a GC sweep on a database, which is where
	// it is asserted (internal/gc's DB-backed teardown suite). What only a browser can
	// prove is the part below -- that the screens exist, that the confirm gates work,
	// and that the console lands somewhere real afterwards instead of on a 404 for the
	// thing it just deleted.
	test('project settings: rename, delete a backend, delete the project', async ({ page }) => {
		const orgSlug = unique('e2e-org-teardown');
		const projectSlug = 'teardown';

		await test.step('dev-login, create org and project', async () => {
			await signInWithoutAuth(page);
			await createOrg(page, orgSlug);
			await createProject(page, orgSlug, projectSlug);
		});

		await test.step('rename the project from Settings', async () => {
			// Scoped by href, not by name: "Settings" now exists under BOTH the PROJECT
			// and the ORG section of the nav, and a bare getByRole('link', {name}) is a
			// strict-mode violation.
			await consoleNav(page)
				.locator(`a[href="/o/${orgSlug}/p/${projectSlug}/settings"]`)
				.click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/settings`);

			await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible();

			// The slug is immutable and the field says so by being disabled -- renaming
			// must not be a way to move a project's cache URLs. `toHaveValue`, never an
			// `input[value=...]` attribute selector: Svelte sets the DOM PROPERTY, so the
			// attribute a selector matches on is never written.
			const slugInput = page.locator('input[disabled]').first();
			await expect(slugInput).toHaveValue(projectSlug);

			const nameInput = page.locator('input:not([disabled])').first();
			await nameInput.fill('Renamed by e2e');
			await page.getByRole('button', { name: 'Save changes' }).click();

			// The form re-reads `project` after invalidateAll, so this is the rename
			// coming back from the server -- not the value the input was just typed into.
			await expect(page.getByText('Settings saved')).toBeVisible();
			await expect(nameInput).toHaveValue('Renamed by e2e');
		});

		await test.step('add a backend, then delete it from its danger zone', async () => {
			// Back to Overview first: addFirstBackend drives the project overview's own
			// "Add a backend" empty state, and the rename step above left us on Settings.
			await consoleNav(page).getByRole('link', { name: 'Overview' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/overview`);

			await addFirstBackend(page, orgSlug, projectSlug);

			await page.getByRole('button', { name: 'Delete backend' }).click();

			const dialog = page.getByRole('dialog');
			// The confirm gate: the primary button stays disabled until the kind matches.
			await expect(dialog.getByRole('button', { name: 'Delete backend' })).toBeDisabled();
			await dialog.locator('input[placeholder="sstate"]').fill('sstate');
			await dialog.getByRole('button', { name: 'Delete backend' }).click();

			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/backends`);
			await expect(page.getByRole('row').filter({ hasText: 'sstate' })).toHaveCount(0);
		});

		await test.step('delete the project from Settings', async () => {
			await consoleNav(page)
				.locator(`a[href="/o/${orgSlug}/p/${projectSlug}/settings"]`)
				.click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/settings`);

			await page.getByRole('button', { name: 'Delete project' }).click();

			const dialog = page.getByRole('dialog');
			await expect(dialog.getByRole('button', { name: 'Delete project' })).toBeDisabled();
			await dialog.locator(`input[placeholder="${projectSlug}"]`).fill(projectSlug);
			await dialog.getByRole('button', { name: 'Delete project' }).click();

			// Lands on the org's projects grid, and the CARD is gone -- the server excludes
			// a deleting project from every listing, so this assertion holds on both the
			// 204 and the 202 branch.
			//
			// Scoped to <main>, not the page: the nav's own project switcher still links
			// to the remembered project (the slug is stashed in localStorage by the
			// project layout, and nothing clears it on delete), so a page-wide locator
			// matches two elements and would pass only by accident.
			await page.waitForURL(`**/o/${orgSlug}/projects`);
			await expect(
				page.getByRole('main').locator(`a[href="/o/${orgSlug}/p/${projectSlug}/overview"]`)
			).toHaveCount(0);
		});
	});

	test('sstate-only project: yocto preview has no BB_HASHSERVE and warns', async ({ page }) => {
		const orgSlug = unique('e2e-org-neg');
		const projectSlug = 'sstate-only';

		await test.step('dev-login, create org and project', async () => {
			await signInWithoutAuth(page);
			await createOrg(page, orgSlug);
			await createProject(page, orgSlug, projectSlug);
		});

		await test.step('create sstate backend only', async () => {
			await addFirstBackend(page, orgSlug, projectSlug);
		});

		await test.step('yocto snippet preview: no BB_HASHSERVE, loud warning', async () => {
			await consoleNav(page).getByRole('link', { name: 'Config snippets' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/snippets`);

			const localConf = codeBlock(page, 'conf/local.conf');
			await expect(localConf).toContainText('SSTATE_MIRRORS');
			await expect(localConf).not.toContainText('BB_HASHSERVE');

			await expect(page.getByText('BB_HASHSERVE was omitted', { exact: false })).toBeVisible();

			// The netrc block still exists (sstate's own Basic-auth line) but
			// carries exactly the one hostname-keyed line -- no ws(s):// line
			// with nothing configured to key it against.
			const netrc = codeBlock(page, '~/.netrc');
			const netrcText = (await netrc.textContent()) ?? '';
			const machineLines = netrcText.split('\n').filter((l) => l.startsWith('machine '));
			expect(machineLines).toHaveLength(1);
			expect(machineLines[0]).not.toMatch(/\bwss?:\/\//);
		});
	});

	test('sticky nav: a global page still shows and routes through the remembered org/project', async ({
		page
	}) => {
		const orgSlug = unique('e2e-org-sticky');
		const projectSlug = 'fw';

		await test.step('dev-login, create org and project', async () => {
			await signInWithoutAuth(page);
			await createOrg(page, orgSlug);
			// createProject lands on .../overview -- the write that remembers this
			// org/project (o/[org]/+layout.ts, .../p/[project]/+layout.ts) has
			// already fired by the time we navigate away below.
			await createProject(page, orgSlug, projectSlug);
		});

		await test.step('/user has no [org]/[project] segment, but the switcher remembers both', async () => {
			await page.goto('/user');
			await expect(
				consoleNav(page).getByRole('button', { name: `Org ${orgSlug}` })
			).toBeVisible();
			await expect(
				consoleNav(page).getByRole('button', { name: `Proj ${projectSlug}` })
			).toBeVisible();
		});

		await test.step('the project nav still routes back into the remembered project', async () => {
			await consoleNav(page).getByRole('link', { name: 'Overview' }).click();
			await page.waitForURL(`**/o/${orgSlug}/p/${projectSlug}/overview`);
		});

		await test.step('a second org with nothing remembered under it does not inherit the first project', async () => {
			// The entire reason `bakery-project` is namespaced per org
			// (storage.ts#lastProject): visiting a DIFFERENT org, with no
			// project of its own ever remembered, must render "none" -- never
			// the first org's project leaking across via a flat key. This is
			// the one assertion of the whole suite that exercises a real
			// localStorage write from a real `+layout.ts`, not the pure
			// Vitest of `resolveNavScope`/`storage.test.ts`.
			const orgSlugB = unique('e2e-org-sticky-b');
			await createOrg(page, orgSlugB);

			await page.goto('/user');
			await expect(
				consoleNav(page).getByRole('button', { name: `Org ${orgSlugB}` })
			).toBeVisible();
			await expect(
				consoleNav(page).getByRole('button', { name: 'Proj none' })
			).toBeVisible();
		});
	});
});

test.describe('console: personal access tokens and robots (wave 1)', () => {
	test('/user: mint a personal access token, one-time reveal, bkru_ prefix', async ({ page }) => {
		// Declared at test scope: used across two steps. unique() because
		// user_tokens_active_name_key is UNIQUE (user_id, name) WHERE revoked_at
		// IS NULL and the dev user persists across local runs -- a literal name
		// makes the second run fail opaquely at the reveal assert.
		const tokenName = unique('e2e-token');
		await test.step('dev-login', async () => {
			await signInWithoutAuth(page);
		});

		await test.step('mint a token from /user', async () => {
			await page.goto('/user');
			await page.getByRole('button', { name: 'Create token' }).click();

			const createDialog = page.getByRole('dialog');
			await createDialog.locator('input[placeholder="build-host-1"]').fill(tokenName);
			// Scope and expiry keep their defaults (write, 90 days).
			await createDialog.getByRole('button', { name: 'Create token' }).click();

			const revealDialog = page.getByRole('dialog');
			await expect(
				revealDialog.getByText('this is the only time you will see the secret')
			).toBeVisible();

			const token = (await revealDialog.locator('pre').first().textContent())?.trim() ?? '';
			expect(token).toMatch(/^bkru_/);

			// The one-time-reveal safety property, both halves: `Done` is
			// disabled until the ack is checked, and the modal is NOT
			// dismissible out from under an un-acknowledged secret -- a
			// regression to `dismissible` on this Modal would pass every
			// other assertion in this test.
			await expect(revealDialog.getByRole('button', { name: 'Done' })).toBeDisabled();
			await page.keyboard.press('Escape');
			await expect(revealDialog).toBeVisible();

			await revealDialog.getByLabel(/I have stored the secret/).check();
			await revealDialog.getByRole('button', { name: 'Done' }).click();
			await expect(revealDialog).toBeHidden();
		});

		await test.step('the token now appears live in the table', async () => {
			// Scope to the row this test just created: a bare getByText('live')
			// matches every live token's status badge, and the seeded dev user's
			// table is not guaranteed to hold exactly one -- a reused local
			// database carries rows from earlier runs (CI's fresh database
			// masks the ambiguity). Same class as the robot-row locator fix.
			const row = page.getByRole('row').filter({ hasText: tokenName });
			await expect(row).toBeVisible();
			await expect(row.getByText('live')).toBeVisible();
		});

		await test.step('revoke it, then delete the revoked record', async () => {
			const row = page.getByRole('row').filter({ hasText: tokenName });
			await row.getByRole('button', { name: 'Revoke' }).click();
			await page.getByRole('dialog').getByRole('button', { name: 'Revoke token' }).click();
			// Non-exact, same as the 'live' check above: the Badge renders a
			// glyph ('✕') alongside the status text, so an exact match never
			// hits.
			await expect(row.getByText('revoked')).toBeVisible();

			// The action column turns into Delete for a revoked row; the purge is what
			// keeps the list from accumulating every revoked generation of a name.
			await row.getByRole('button', { name: 'Delete' }).click();
			await page.getByRole('dialog').getByRole('button', { name: 'Delete token' }).click();
			await expect(row).toBeHidden();
		});
	});

	test('org members: create a robot, mint its token, one-time reveal, bkro_ prefix', async ({
		page
	}) => {
		const orgSlug = unique('e2e-org-robot');

		await test.step('dev-login, create org', async () => {
			await signInWithoutAuth(page);
			// The org creator is granted a local OWNER role in the same
			// transaction as the org, which is what makes the ROBOTS card
			// (canAdminOrg-gated) visible on the very next screen.
			await createOrg(page, orgSlug);
		});

		await test.step('open members and create a robot', async () => {
			await page.goto(`/o/${orgSlug}/members`);
			await page.getByRole('button', { name: 'Create robot' }).click();

			const createDialog = page.getByRole('dialog');
			await createDialog.locator('input[placeholder="ci-runner"]').fill('e2e-robot');
			await createDialog.getByRole('button', { name: 'Create robot' }).click();
			await expect(createDialog).toBeHidden();

			// exact: the success toast ("Created robot e2e-robot") is still on
			// screen here and substring-matches the bare name -- a strict-mode
			// violation whenever the assertion beats the toast's dismissal.
			await expect(page.getByText('e2e-robot', { exact: true })).toBeVisible();
		});

		await test.step('mint the robot a token', async () => {
			await page.getByRole('button', { name: 'New token' }).click();

			const tokenDialog = page.getByRole('dialog');
			await tokenDialog.locator('input[placeholder="ci-2026"]').fill('e2e-robot-token');
			// Scope and expiry keep their defaults (write, 90 days).
			await tokenDialog.getByRole('button', { name: 'Create token' }).click();

			const revealDialog = page.getByRole('dialog');
			await expect(
				revealDialog.getByText('this is the only time you will see the secret')
			).toBeVisible();

			const token = (await revealDialog.locator('pre').first().textContent())?.trim() ?? '';
			expect(token).toMatch(/^bkro_/);

			// Same one-time-reveal safety property as the personal-token
			// modal: disabled Done before the ack, undismissable throughout.
			await expect(revealDialog.getByRole('button', { name: 'Done' })).toBeDisabled();
			await page.keyboard.press('Escape');
			await expect(revealDialog).toBeVisible();

			await revealDialog.getByLabel(/I have stored the secret/).check();
			await revealDialog.getByRole('button', { name: 'Done' }).click();
			await expect(revealDialog).toBeHidden();
		});

		await test.step('the token now appears live under the robot', async () => {
			await expect(page.getByText('e2e-robot-token')).toBeVisible();
		});
	});
});
