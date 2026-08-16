<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import { theme, setTheme, type Theme } from '$lib/theme';
	import { createUserToken, revokeUserToken } from '$lib/api/tokens';
	import { isApiError } from '$lib/api/errors';
	import type { CreatedUserToken, KeyScope, UserToken } from '$lib/api/types';
	import { formatDateTimeUTC, formatExpiry } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Input, Field, Select, Checkbox } from '$lib/components/inputs';
	import { Modal, EmptyState, Callout } from '$lib/components/feedback';
	import { CodeBlock } from '$lib/components/content';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const me = $derived(data.me);
	const authConfig = $derived(data.authConfig);
	const tokens = $derived(data.tokens);

	// AuthConfig.issuer is the full issuer URL, and is "" when OIDC is
	// disabled (dev-login only) -- Service.AuthConfig returns the zero value
	// there.
	const idpLabel = $derived.by(() => {
		if (!authConfig?.issuer) return 'your identity provider';
		try {
			return new URL(authConfig.issuer).host;
		} catch {
			return 'your identity provider';
		}
	});

	// Keyed to the URL that failed, not a bare flag -- see ConsoleNav's copy of
	// this state for why a plain boolean latches a transient failure forever.
	let failedAvatarURL = $state<string | null>(null);

	// Same expression as ConsoleNav's `initial`, verbatim: with both
	// `display_name` and `email` empty (a robot's `handleMe` shape -- a robot
	// cannot hold a session, but the two components answering "what's the
	// monogram" differently is drift that outlives the reason it was safe)
	// `[0]` on `""` is `undefined`, where `.charAt(0)` is `""` -- not
	// interchangeable, so this is the one true expression, not a rewrite.
	const initial = $derived((me.display_name || me.email || '?').trim().charAt(0).toUpperCase());

	const microLabel = 'text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3';
	const card = 'flex flex-col rounded-2 border border-border-0 bg-bg-1 p-[14px]';

	const themeOptions: { value: Theme; label: string }[] = [
		{ value: 'dark', label: 'Dark' },
		{ value: 'light', label: 'Light' },
		{ value: 'system', label: 'System' }
	];

	// Live keys first (already `created_at DESC` from the server), revoked
	// ones segregated to the bottom -- same idiom as the project Keys screen.
	const sortedTokens = $derived(
		[...tokens].sort((a, b) => (a.revoked_at ? 1 : 0) - (b.revoked_at ? 1 : 0))
	);
	const isEmpty = $derived(tokens.length === 0);

	const expiryClass: Record<'none' | 'soon' | 'expired', string> = {
		none: 'text-text-2',
		soon: 'text-warn',
		expired: 'text-err'
	};

	// A personal token DOES allow "never" -- unlike a robot token, which the
	// API refuses without an expiry. Default is 90 days (spec: console default).
	const expiryOptions = [
		{ value: '30', label: '30 days' },
		{ value: '90', label: '90 days' },
		{ value: '365', label: '1 year' },
		{ value: '', label: 'Never' }
	];

	let modal = $state<null | 'create' | 'reveal' | 'revoke'>(null);
	let ack = $state(false);
	let draftName = $state('');
	let draftScope = $state<KeyScope>('write');
	let expiryDays = $state('90');
	let createPending = $state(false);
	let nameError = $state<string | null>(null);

	let revokeTarget = $state<UserToken | null>(null);
	let revokePending = $state(false);

	let revealedToken = $state<CreatedUserToken | null>(null);

	function openCreate() {
		modal = 'create';
		ack = false;
		draftName = '';
		draftScope = 'write';
		expiryDays = '90';
		nameError = null;
	}

	function expiresAtFromDays(days: string): string | undefined {
		if (days === '') return undefined;
		const n = Number(days);
		return new Date(Date.now() + n * 24 * 60 * 60 * 1000).toISOString();
	}

	async function submitCreate() {
		if (createPending) return;
		createPending = true;
		nameError = null;

		try {
			const created = await createUserToken({
				name: draftName,
				scope: draftScope,
				expires_at: expiresAtFromDays(expiryDays)
			});
			revealedToken = created;
			modal = 'reveal';
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'name') {
				nameError = err.message;
			} else {
				toastError(err, 'Could not create token');
			}
		} finally {
			createPending = false;
		}
	}

	function openRevoke(t: UserToken) {
		revokeTarget = t;
		modal = 'revoke';
	}

	async function confirmRevoke() {
		if (!revokeTarget || revokePending) return;
		revokePending = true;

		try {
			await revokeUserToken(revokeTarget.id);
			pushToast({ variant: 'success', title: `Revoked ${revokeTarget.name}` });
			modal = null;
			revokeTarget = null;
			await invalidateAll();
		} catch (err) {
			toastError(err, 'Could not revoke token');
		} finally {
			revokePending = false;
		}
	}

	function closeModal() {
		if (createPending || revokePending) return;
		modal = null;
		revokeTarget = null;
		revealedToken = null;
		ack = false;
	}
</script>

<div class="flex max-w-[720px] flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Your account</h1>
		<div class="text-sm text-text-2">Profile, appearance, and personal access tokens.</div>
	</div>

	<div class="{card} gap-[14px]">
		<div class={microLabel}>Profile</div>
		<div class="flex items-center gap-[14px]">
			{#if me.avatar_url && me.avatar_url !== failedAvatarURL}
				<img
					src={me.avatar_url}
					alt=""
					referrerpolicy="no-referrer"
					onerror={() => (failedAvatarURL = me.avatar_url ?? null)}
					class="h-12 w-12 flex-none rounded-full border border-accent-border object-cover"
				/>
			{:else}
				<span
					class="inline-flex h-12 w-12 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[18px] font-semibold text-accent-text"
					>{initial}</span
				>
			{/if}
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Display name" hint={`Managed by ${idpLabel}`}>
				{#snippet children(f)}
					<Input size="md" value={me.display_name} disabled {...f} />
				{/snippet}
			</Field>
			<Field label="Email" hint={`Managed by ${idpLabel}`}>
				{#snippet children(f)}
					<Input size="md" mono value={me.email} disabled {...f} />
				{/snippet}
			</Field>
		</div>
	</div>

	<div class="{card} gap-2.5">
		<div class={microLabel}>Appearance</div>
		<div class="flex gap-1.5">
			{#each themeOptions as t (t.value)}
				<button
					type="button"
					aria-pressed={$theme === t.value}
					onclick={() => setTheme(t.value)}
					class="cursor-pointer rounded-1 border px-[14px] py-1.5 text-sm transition-colors duration-[var(--dur-2)] ease-[var(--ease)] {$theme ===
					t.value
						? 'border-accent-border bg-accent-muted text-accent-text'
						: 'border-border-1 bg-bg-2 text-text-2 hover:border-border-2 hover:text-text-1'}"
					>{t.label}</button
				>
			{/each}
		</div>
		<div class="text-xs text-text-3">
			System follows your OS preference. Applies immediately, remembered in this browser.
		</div>
	</div>

	<div class="{card} gap-[14px]">
		<div class="flex items-center justify-between">
			<div>
				<div class={microLabel}>Personal access tokens</div>
				<div class="text-sm leading-[18px] text-text-2">
					For the <span class="font-mono text-xs">bakery</span> CLI and other credential-free
					pushes. A token authenticates as you, on both the console API and the cache — it
					reaches every project your account does.
				</div>
			</div>
			<Button variant="primary" size="md" onclick={openCreate}>Create token</Button>
		</div>

		{#if isEmpty}
			<EmptyState
				glyph="∅"
				title="No personal access tokens yet"
				desc="Mint one to run bakery login --token on a build host, or push sstate without a browser. For bots and CI, prefer a project-scoped key or an org robot instead — both stay narrowly scoped and survive you leaving the org."
			/>
		{:else}
			<TableWrap>
				<TableRoot dense>
					<thead>
						<tr>
							<Th>Name</Th>
							<Th>Status</Th>
							<Th>Prefix</Th>
							<Th>Scope</Th>
							<Th>Created</Th>
							<Th>Last used</Th>
							<Th>Expires</Th>
							<Th class="w-[72px]"></Th>
						</tr>
					</thead>
					<tbody>
						{#each sortedTokens as t (t.id)}
							{@const expiry = formatExpiry(t.expires_at)}
							<Tr>
								<Td class="font-medium">{t.name}</Td>
								<Td>
									{#if t.revoked_at}
										<div class="flex flex-col gap-0.5">
											<Badge status="miss">revoked</Badge>
											<span class="whitespace-nowrap text-xs text-text-3"
												>{formatDateTimeUTC(t.revoked_at)}</span
											>
										</div>
									{:else if expiry.kind === 'expired'}
										<Badge status="stale">expired</Badge>
									{:else}
										<Badge status="hit">live</Badge>
									{/if}
								</Td>
								<Td mono>{t.token_prefix}</Td>
								<Td><Badge variant="type">{t.max_scope}</Badge></Td>
								<Td mono class="whitespace-nowrap">{formatDateTimeUTC(t.created_at)}</Td>
								<Td class="whitespace-nowrap text-text-2"
									>{t.last_used_at ? formatDateTimeUTC(t.last_used_at) : 'never'}</Td
								>
								<Td class="whitespace-nowrap {expiryClass[expiry.kind]}">{expiry.label}</Td>
								<Td class="text-right">
									<Button
										variant="ghost"
										size="sm"
										class="text-err! hover:text-err!"
										disabled={!!t.revoked_at}
										onclick={() => openRevoke(t)}>Revoke</Button
									>
								</Td>
							</Tr>
						{/each}
					</tbody>
				</TableRoot>
			</TableWrap>
		{/if}
	</div>
</div>

{#if modal === 'create'}
	<Modal title="Create personal access token" onclose={closeModal}>
		<div class="flex flex-col gap-[14px]">
			<Callout variant="info" title="This token will act as you, live.">
				<span class="text-text-2">
					It reaches every project your account can, on both the console API and the cache —
					before you decide the name, scope, and expiry below.</span
				>
			</Callout>
			<Field label="Name" hint="Shown in the table above." error={nameError ?? undefined}>
				{#snippet children(f)}
					<Input
						size="md"
						placeholder="build-host-1"
						bind:value={draftName}
						error={!!nameError}
						{...f}
					/>
				{/snippet}
			</Field>
			<Field
				label="Scope"
				hint="The CEILING on what this token may do. Its real authority is your own live roles, narrowed by this."
			>
				{#snippet children(f)}
					<Select
						size="md"
						bind:value={draftScope}
						options={[
							{ value: 'read', label: 'Read only' },
							{ value: 'write', label: 'Read + write' }
						]}
						{...f}
					/>
				{/snippet}
			</Field>
			<Field label="Expiry">
				{#snippet children(f)}
					<Select size="md" options={expiryOptions} bind:value={expiryDays} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal} disabled={createPending}
				>Cancel</Button
			>
			<Button variant="primary" size="md" onclick={submitCreate} disabled={createPending}>
				{createPending ? 'Creating…' : 'Create token'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if modal === 'reveal' && revealedToken}
	<Modal
		title="Token created — this is the only time you will see the secret"
		width="min(560px, calc(100vw - 32px))"
		showClose={false}
		dismissible={false}
	>
		<div class="flex flex-col gap-3">
			<Callout variant="warn" title="This token acts as you, live.">
				<span class="text-text-2">
					It reads and writes every project your account can, and it narrows the moment your
					access does — the next request after a role change or a revoke sees the new authority,
					with no wait. It cannot administer anything and it cannot mint credentials. Revoking it
					takes effect immediately: the next request fails with 401.</span
				>
			</Callout>
			<CodeBlock
				title={`${revealedToken.name} · ${revealedToken.max_scope}`}
				copyLabel="Copy"
				copyText={revealedToken.token}>{revealedToken.token}</CodeBlock
			>
			<Checkbox
				bind:checked={ack}
				label="I have stored the secret. I understand it will never be shown again."
			/>
		</div>
		{#snippet footer()}
			<Button variant="primary" size="md" disabled={!ack} onclick={closeModal}>Done</Button>
		{/snippet}
	</Modal>
{/if}

{#if modal === 'revoke' && revokeTarget}
	<Modal title={`Revoke ${revokeTarget.name}`} onclose={closeModal}>
		Anything authenticating with <span class="font-mono text-[length:var(--mono-xs)] text-text-1"
			>{revokeTarget.name}</span
		> will lose access immediately — in-flight requests fail with 401. This cannot be undone.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal} disabled={revokePending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" onclick={confirmRevoke} disabled={revokePending}>
				{revokePending ? 'Revoking…' : 'Revoke token'}
			</Button>
		{/snippet}
	</Modal>
{/if}
