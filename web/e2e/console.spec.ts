import { expect, test, type Locator, type Page } from '@playwright/test';

// The DESIGN.md:545 flow, driven against the real `bakery serve` binary and a
// real Chromium (see playwright.config.ts). Every step below is a click or a
// fill against the actual console -- no route is reached by constructing a
// URL for a screen the app itself would navigate to, except the one hop
// (`backends/new` a second time) that has no in-app link to it at all, noted
// at that call site.
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
 * Adds a second backend of `kind`. There is no in-app link to `backends/new`
 * once a project already has a backend (Overview's empty-state action is
 * gone, and the backend detail page has no "add another" affordance) -- this
 * is the one hop in the whole flow reached by URL rather than a click, and it
 * still exercises the real route, guard and load function.
 */
async function addBackend(
	page: Page,
	orgSlug: string,
	projectSlug: string,
	kind: string
): Promise<void> {
	await page.goto(`/o/${orgSlug}/p/${projectSlug}/backends/new`);
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
			await addBackend(page, orgSlug, projectSlug, 'hashserv');
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
			await expect(page.getByText(tokenName)).toBeVisible();
			await expect(page.getByText('live')).toBeVisible();
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
