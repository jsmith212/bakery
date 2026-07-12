<script lang="ts">
	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Field, Input, Select, Checkbox } from '$lib/components/inputs';
	import { Modal } from '$lib/components/feedback';
	import { CodeBlock } from '$lib/components/content';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	interface Key {
		name: string;
		id: string;
		scopes: string[];
		owner: string;
		created: string;
		lastUsed: string;
		expires: string;
		exp: 'none' | 'soon' | 'expired';
	}

	const keys: Key[] = [
		{ name: 'ci-writer', id: 'bk_a7f13d', scopes: ['r', 'w'], owner: 'anna', created: '2026-05-12', lastUsed: '2 min ago', expires: '—', exp: 'none' },
		{ name: 'ci-reader', id: 'bk_20c9f1', scopes: ['r'], owner: 'anna', created: '2026-05-12', lastUsed: '8 min ago', expires: '—', exp: 'none' },
		{ name: 'marko-dev', id: 'bk_e8d7b6', scopes: ['r', 'w'], owner: 'marko', created: '2026-06-03', lastUsed: '1 h ago', expires: 'in 21 days', exp: 'soon' },
		{ name: 'old-jenkins', id: 'bk_4b17aa', scopes: ['r'], owner: 'deploy', created: '2025-11-20', lastUsed: '44 days ago', expires: 'expired', exp: 'expired' }
	];

	const expiryClass: Record<Key['exp'], string> = {
		none: 'text-text-2',
		soon: 'text-warn',
		expired: 'text-err'
	};

	const REVEAL_KEY_ID = 'bk_9c44e1';
	const REVEAL_SECRET = 'bks_Xk2mQv8Rw1ZpTn6LhC3jUaGd';

	const expiryOptions = [
		{ value: 'No expiry', label: 'No expiry' },
		{ value: '30 days', label: '30 days' },
		{ value: '90 days', label: '90 days' },
		{ value: '1 year', label: '1 year' }
	];

	let modal = $state<null | 'create' | 'reveal' | 'revoke'>(null);
	let ack = $state(false);
	let draftName = $state('');
	let draftWrite = $state(true);
	let expiry = $state('No expiry');
	let revokeName = $state('');

	const revealName = $derived(draftName || 'ci-writer-2');
	const revokeLabel = $derived(revokeName || 'ci-writer');

	function openCreate() {
		modal = 'create';
		ack = false;
	}
	function createKey() {
		modal = 'reveal';
		ack = false;
	}
	function openRevoke(name: string) {
		revokeName = name;
		modal = 'revoke';
	}
	function closeModal() {
		modal = null;
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
		<Button variant="primary" size="md" onclick={openCreate}>Create key</Button>
	</div>

	<TableWrap>
		<TableRoot dense>
			<thead>
				<tr>
					<Th>Name</Th>
					<Th>Key ID</Th>
					<Th>Scope</Th>
					<Th>Owner</Th>
					<Th>Created</Th>
					<Th>Last used</Th>
					<Th>Expires</Th>
					<Th class="w-[72px]"></Th>
				</tr>
			</thead>
			<tbody>
				{#each keys as k (k.id)}
					<Tr>
						<Td class="font-medium">{k.name}</Td>
						<Td mono>{k.id}</Td>
						<Td>
							<span class="inline-flex gap-1">
								{#each k.scopes as s (s)}
									{#if s === 'r'}
										<Badge variant="accent"
											><span class="text-[9px] leading-none" aria-hidden="true">●</span
											>read</Badge
										>
									{:else}
										<Badge status="stale">write</Badge>
									{/if}
								{/each}
							</span>
						</Td>
						<Td class="text-text-2">{k.owner}</Td>
						<Td mono class="whitespace-nowrap">{k.created}</Td>
						<Td class="whitespace-nowrap text-text-2">{k.lastUsed}</Td>
						<Td class="whitespace-nowrap {expiryClass[k.exp]}">{k.expires}</Td>
						<Td class="text-right">
							<Button variant="ghost" size="sm" class="text-err! hover:text-err!" onclick={() => openRevoke(k.name)}>Revoke</Button>
						</Td>
					</Tr>
				{/each}
			</tbody>
		</TableRoot>
	</TableWrap>

	<div class="text-xs text-text-3">
		Revoking a key takes effect immediately. Keys present credentials differently per backend — see
		<a href="/snippets" class="text-accent-text hover:underline">config snippets</a>.
	</div>
</div>

{#if modal === 'create'}
	<Modal title="Create API key" onclose={closeModal}>
		<div class="flex flex-col gap-[14px]">
			<Field label="Name" hint="Shown in request logs and the builders table.">
				<Input size="md" placeholder="ci-writer" bind:value={draftName} />
			</Field>
			<Field
				label="Scope"
				hint="Write scope is required to push sstate, report unihashes, or upload blobs."
			>
				<div class="flex gap-4">
					<Checkbox checked disabled label="Read" />
					<Checkbox bind:checked={draftWrite} label="Write" />
				</div>
			</Field>
			<Field label="Expiry">
				<Select size="md" options={expiryOptions} bind:value={expiry} />
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal}>Cancel</Button>
			<Button variant="primary" size="md" onclick={createKey}>Create key</Button>
		{/snippet}
	</Modal>
{/if}

{#if modal === 'reveal'}
	<Modal
		title="Key created — this is the only time you will see the secret"
		width="min(560px, calc(100vw - 32px))"
		showClose={false}
		dismissible={false}
	>
		<div class="flex flex-col gap-3">
			<div
				class="flex items-start gap-2.5 rounded-2 border border-warn-border bg-warn-muted px-3 py-2.5"
			>
				<span class="text-xs leading-[18px] text-warn">▲</span>
				<div class="text-sm leading-[18px] text-text-1">
					<span class="font-semibold">Bakery stores only a hash of this secret.</span>
					<span class="text-text-2">
						If you lose it, you will revoke this key and mint a new one. There is no recovery.</span
					>
				</div>
			</div>
			<CodeBlock
				title={`${revealName} · read + write`}
				copyLabel="Copy both"
				copyText={`${REVEAL_KEY_ID}\n${REVEAL_SECRET}`}
				><div><span class="tok-comment"># key id</span></div><div>{REVEAL_KEY_ID}</div><div><span class="tok-comment"># secret</span></div><div>{REVEAL_SECRET}</div></CodeBlock
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

{#if modal === 'revoke'}
	<Modal title={`Revoke ${revokeLabel}`} onclose={closeModal}>
		Builds authenticating with <span
			class="font-mono text-[length:var(--mono-xs)] text-text-1">{revokeLabel}</span
		> will lose cache access immediately — in-flight requests fail with 401. This cannot be undone.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal}>Cancel</Button>
			<Button variant="danger" size="md" onclick={closeModal}>Revoke key</Button>
		{/snippet}
	</Modal>
{/if}
