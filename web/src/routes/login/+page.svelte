<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';

	import { SSO_LOGIN_URL, devLogin } from '$lib/api/auth';
	import { listOrgs } from '$lib/api/orgs';
	import { loadMe } from '$lib/session';
	import { lastOrg, lastProject, takeReturnPath } from '$lib/storage';
	import { resolveLanding } from '$lib/tenancy';
	import { isApiError } from '$lib/api/errors';
	import { deniedCopy } from '$lib/deniedReason';
	import { Button } from '$lib/components/buttons';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const config = $derived(data.config);

	// Every browser-facing failure of the OIDC round trip is a TERMINAL state
	// reached by a `302 /login?denied=<reason>` from `GET /auth/callback` (or, for
	// the two failures that can happen before the browser ever leaves this origin,
	// `GET /auth/login`) -- never a bare JSON page at the callback URL, which would
	// leave a user with no console chrome and nothing to do but close the tab. See
	// `$lib/deniedReason` for the closed reason set and its per-reason copy.
	const denial = $derived(deniedCopy(page.url.searchParams.get('denied')));

	const issuerHost = $derived.by(() => {
		if (!config?.issuer) return null;
		try {
			return new URL(config.issuer).host;
		} catch {
			return config.issuer;
		}
	});

	const hasSSO = $derived(config?.oidc_enabled === true);
	const hasDevLogin = $derived(config?.dev_login_enabled === true);
	const noMethod = $derived(!!config && !hasSSO && !hasDevLogin);

	let devPending = $state(false);

	// `/login` sits OUTSIDE the console layout, so `ToastHost` is not mounted
	// here. A failure is rendered in place instead of pushed to a toast nothing
	// would draw.
	let signInError = $state<string | null>(null);

	async function signInWithoutAuth() {
		if (devPending) return;
		devPending = true;
		signInError = null;

		try {
			// Bodyless and content-type-less: that is exactly what `requireJSON`
			// exempts, and this endpoint ignores any body it is sent anyway. There
			// is no user field -- `HandleDevLogin` always seeds the same account.
			await devLogin();

			const me = await loadMe();
			const orgs = me ? await listOrgs() : null;

			await goto(
				resolveLanding({
					me,
					orgs: orgs?.items ?? null,
					returnPath: takeReturnPath(),
					lastOrg: lastOrg(),
					lastProject: lastProject()
				}),
				{ replaceState: true }
			);
		} catch (err) {
			signInError = isApiError(err)
				? err.message
				: 'The request did not reach the server, or the server did not answer.';
		} finally {
			devPending = false;
		}
	}
</script>

<div class="grid min-h-screen place-items-center bg-bg-0 p-6">
	<div class="flex w-[340px] flex-col gap-[14px]">
		<div class="pb-1 text-center">
			<img src="/logo-192.png" alt="" class="mx-auto mb-2 h-16 w-16" />
			<div class="text-xl font-semibold tracking-[-0.01em] text-text-1">Bakery</div>
			<div class="mt-1 font-mono text-sm text-text-3">bake once. cache everything.</div>
		</div>

		{#if denial}
			<div class="flex flex-col gap-2 rounded-2 border border-err-border bg-err-muted p-5">
				<div class="text-base font-medium text-err">{denial.title}</div>
				<div class="text-sm text-text-2">{denial.body}</div>
			</div>
		{/if}

		<div class="flex flex-col gap-2 rounded-2 border border-border-0 bg-bg-1 p-5">
			<div class="mb-1.5 text-base font-medium text-text-1">Sign in</div>

			{#if data.configError}
				<div class="text-sm text-text-2">{data.configError}</div>
			{/if}

			{#if hasSSO}
				<!--
					A top-level navigation, never a fetch: this is a 302 chain that
					leaves the origin for the identity provider and comes back. An
					`<a>` is the only thing that can follow it.
				-->
				<a
					href={SSO_LOGIN_URL}
					data-sveltekit-reload
					class="inline-flex h-[var(--control-lg)] w-full items-center justify-center rounded-1 border border-border-1 bg-bg-2 px-3 text-base font-medium text-text-1 no-underline transition-colors duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2 hover:bg-bg-3"
				>
					Continue with SSO
				</a>
				{#if issuerHost}
					<div class="text-center font-mono text-xs text-text-3">{issuerHost}</div>
				{/if}
			{/if}

			{#if noMethod}
				<div class="text-sm text-text-2">
					This server has no sign-in method configured. An operator sets an OIDC issuer, or
					enables dev login, at boot.
				</div>
			{/if}

			<div class="mt-1.5 text-center text-xs text-text-3">
				Access is granted by your org admin. Build clients authenticate with API keys, not this
				login.
			</div>
		</div>

		{#if hasDevLogin}
			<div class="overflow-hidden rounded-2 border border-dashed border-warn-border">
				<div
					class="flex items-center gap-2 border-b border-dashed border-warn-border bg-warn-muted px-3 py-2"
				>
					<span class="text-xs text-warn" aria-hidden="true">▲</span>
					<span class="font-mono text-xs font-medium text-warn">DEV_LOGIN_ENABLED=1</span>
					<span class="text-xs text-text-2">not a production instance</span>
				</div>
				<div class="flex flex-col gap-[10px] px-3 py-[14px]">
					<Button
						variant="secondary"
						size="md"
						disabled={devPending}
						onclick={signInWithoutAuth}
						class="w-full text-warn! border-warn-border! hover:border-warn-border!"
					>
						Sign in without auth
					</Button>
					{#if signInError}
						<div class="text-sm text-err">{signInError}</div>
					{/if}
					<div class="text-[10px] leading-[14px] text-text-3">
						This panel exists only when the env var is set at boot. No UI or API path can enable
						it. The account is fixed; there is nothing to type.
					</div>
				</div>
			</div>
		{/if}
	</div>
</div>
