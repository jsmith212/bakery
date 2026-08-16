<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import { createKey, revokeKey } from '$lib/api/keys';
	import { isApiError } from '$lib/api/errors';
	import type { APIKey, CreatedAPIKey, KeyScope } from '$lib/api/types';
	import { canMintKey } from '$lib/roles';
	import { formatDateTimeUTC, formatExpiry } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Field, Input, Select, Checkbox } from '$lib/components/inputs';
	import { Modal, EmptyState, Callout } from '$lib/components/feedback';
	import { CodeBlock } from '$lib/components/content';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const keys = $derived(data.keys);

	// auth.CreateAPIKey requires a project_memberships row for the CALLER and
	// caps scope against it -- an org owner or site admin who did not create
	// this project has none, even though they can read and see this screen.
	// canMintKey deliberately does NOT short-circuit on is_site_admin: there is
	// nothing to cap a scope against when the row is missing.
	const mintAllowed = $derived(canMintKey({ role: project.role }));
	const ceilingScope: KeyScope = $derived(project.role === 'reader' ? 'read' : 'write');

	// `data.keys` is already resolved (a project's key roster is a cheap read,
	// so it is awaited in `load` rather than handed to `{#await}`) -- there is
	// no real loading state on this screen, only empty vs. loaded.
	const isEmpty = $derived(keys.length === 0);

	// Live keys first (already `created_at DESC` from the server), revoked
	// ones segregated to the bottom -- `ListAPIKeysForProject` returns both
	// (api_keys.sql: revocation is soft so the console can show "revoked N
	// days ago"), and a stable sort preserves each partition's own order.
	const sortedKeys = $derived(
		[...keys].sort((a, b) => (a.revoked_at ? 1 : 0) - (b.revoked_at ? 1 : 0))
	);

	const expiryClass: Record<'none' | 'soon' | 'expired', string> = {
		none: 'text-text-2',
		soon: 'text-warn',
		expired: 'text-err'
	};

	const expiryOptions = [
		{ value: '', label: 'No expiry' },
		{ value: '30', label: '30 days' },
		{ value: '90', label: '90 days' },
		{ value: '365', label: '1 year' }
	];

	let modal = $state<null | 'create' | 'reveal' | 'revoke'>(null);
	let ack = $state(false);
	let draftName = $state('');
	let draftScope = $state<KeyScope>('write');
	let expiryDays = $state('');
	let createPending = $state(false);
	let nameError = $state<string | null>(null);
	let scopeError = $state<string | null>(null);

	let revokeTarget = $state<APIKey | null>(null);
	let revokePending = $state(false);

	let revealedKey = $state<CreatedAPIKey | null>(null);

	function openCreate() {
		modal = 'create';
		ack = false;
		draftName = '';
		draftScope = ceilingScope;
		expiryDays = '';
		nameError = null;
		scopeError = null;
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
		scopeError = null;

		try {
			const created = await createKey(org.slug, project.slug, {
				name: draftName,
				scope: draftScope,
				expires_at: expiresAtFromDays(expiryDays)
			});
			revealedKey = created;
			modal = 'reveal';
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.code === 'scope_exceeds_role') {
				scopeError = err.message;
			} else if (isApiError(err) && err.field === 'name') {
				nameError = err.message;
			} else {
				toastError(err, 'Could not create key');
			}
		} finally {
			createPending = false;
		}
	}

	function openRevoke(key: APIKey) {
		revokeTarget = key;
		modal = 'revoke';
	}

	async function confirmRevoke() {
		if (!revokeTarget || revokePending) return;
		revokePending = true;

		try {
			await revokeKey(org.slug, project.slug, revokeTarget.id);
			pushToast({ variant: 'success', title: `Revoked ${revokeTarget.name}` });
			modal = null;
			revokeTarget = null;
			await invalidateAll();
		} catch (err) {
			toastError(err, 'Could not revoke key');
		} finally {
			revokePending = false;
		}
	}

	function closeModal() {
		if (createPending || revokePending) return;
		modal = null;
		revokeTarget = null;
		revealedKey = null;
		ack = false;
	}
</script>

<div class="flex w-full flex-col gap-[14px]">
	<div class="flex items-center justify-between">
		<div>
			<h1 class="mb-0.5 text-lg font-semibold text-text-1">API keys</h1>
			<div class="text-sm text-text-2">
				Project-scoped, per-user. Secrets are shown exactly once, at creation.
			</div>
		</div>
		<Button variant="primary" size="md" disabled={!mintAllowed} onclick={openCreate}
			>Create key</Button
		>
	</div>

	{#if !mintAllowed}
		<Callout variant="warn" title="You have no role on this project.">
			<span class="text-text-2">
				An org admin can grant you one on the members page — Bakery caps a key's scope at the
				role of the person minting it, so there is nothing to cap against without one.</span
			>
		</Callout>
	{/if}

	{#if isEmpty}
		<EmptyState
			glyph="∅"
			title="No keys on {project.slug} yet"
			desc="A build client authenticates with a project-scoped key. Create one, or use the config snippet generator, which mints one for you."
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
						<Th>Owner</Th>
						<Th>Created</Th>
						<Th>Last used</Th>
						<Th>Expires</Th>
						<Th class="w-[72px]"></Th>
					</tr>
				</thead>
				<tbody>
					{#each sortedKeys as k (k.id)}
						{@const expiry = formatExpiry(k.expires_at)}
						<Tr>
							<Td class="font-medium">{k.name}</Td>
							<Td>
								{#if k.revoked_at}
									<div class="flex flex-col gap-0.5">
										<Badge status="miss">revoked</Badge>
										<span class="whitespace-nowrap text-xs text-text-3"
											>{formatDateTimeUTC(k.revoked_at)}</span
										>
									</div>
								{:else if expiry.kind === 'expired'}
									<Badge status="stale">expired</Badge>
								{:else}
									<Badge status="hit">live</Badge>
								{/if}
							</Td>
							<Td mono>{k.token_prefix}</Td>
							<Td><Badge variant="type">{k.scope}</Badge></Td>
							<Td class="text-text-2">{k.owner_name}</Td>
							<Td mono class="whitespace-nowrap">{formatDateTimeUTC(k.created_at)}</Td>
							<Td class="whitespace-nowrap text-text-2"
								>{k.last_used_at ? formatDateTimeUTC(k.last_used_at) : 'never'}</Td
							>
							<Td class="whitespace-nowrap {expiryClass[expiry.kind]}">{expiry.label}</Td>
							<Td class="text-right">
								<Button
									variant="ghost"
									size="sm"
									class="text-err! hover:text-err!"
									disabled={!!k.revoked_at}
									onclick={() => openRevoke(k)}>Revoke</Button
								>
							</Td>
						</Tr>
					{/each}
				</tbody>
			</TableRoot>
		</TableWrap>
	{/if}

	<div class="text-xs text-text-3">
		Revoking a key takes effect immediately. Keys present credentials differently per backend —
		see <a
			href="/o/{org.slug}/p/{project.slug}/snippets"
			class="text-accent-text hover:underline">config snippets</a
		>.
	</div>
</div>

{#if modal === 'create'}
	<Modal title="Create API key" onclose={closeModal}>
		<div class="flex flex-col gap-[14px]">
			<Field label="Name" hint="Shown in the table above." error={nameError ?? undefined}>
				{#snippet children(f)}
					<Input size="md" placeholder="ci-writer" bind:value={draftName} error={!!nameError} {...f} />
				{/snippet}
			</Field>
			<Field
				label="Scope"
				hint="Write scope is required to push sstate, report unihashes, or upload blobs."
				error={scopeError ?? undefined}
			>
				{#snippet children(f)}
					<Select
						size="md"
						bind:value={draftScope}
						options={[
							{ value: 'read', label: 'Read only' },
							{
								value: 'write',
								label: 'Read + write',
								disabled: ceilingScope === 'read'
							}
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
				{createPending ? 'Creating…' : 'Create key'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if modal === 'reveal' && revealedKey}
	<Modal
		title="Key created — this is the only time you will see the secret"
		width="min(560px, calc(100vw - 32px))"
		showClose={false}
		dismissible={false}
	>
		<div class="flex flex-col gap-3">
			<Callout variant="warn" title="Bakery stores only a hash of this secret.">
				<span class="text-text-2">
					If you lose it, you will revoke this key and mint a new one. There is no recovery.</span
				>
			</Callout>
			<CodeBlock
				title={`${revealedKey.name} · ${revealedKey.scope}`}
				copyLabel="Copy"
				copyText={revealedKey.token}>{revealedKey.token}</CodeBlock
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
		Builds authenticating with <span class="font-mono text-[length:var(--mono-xs)] text-text-1"
			>{revokeTarget.name}</span
		> will lose cache access immediately — in-flight requests fail with 401. This cannot be undone.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal} disabled={revokePending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" onclick={confirmRevoke} disabled={revokePending}>
				{revokePending ? 'Revoking…' : 'Revoke key'}
			</Button>
		{/snippet}
	</Modal>
{/if}
