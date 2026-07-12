<script lang="ts">
	import { theme, setTheme, type Theme } from '$lib/theme';
	import { Button } from '$lib/components/buttons';
	import { Input, Select, Checkbox, Field } from '$lib/components/inputs';
	import { Modal } from '$lib/components/feedback';
	import { CodeBlock } from '$lib/components/content';
	import { TableRoot, Tr, Th, Td } from '$lib/components/table';

	const microLabel =
		'text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3';
	const card = 'flex flex-col rounded-2 border border-border-0 bg-bg-1 p-[14px]';

	const REVEAL_TOKEN = 'bku_7Hd2wQx9Rm4TnZ6pLc1jVa8Ke0Sf3Ug5';

	const themeOptions: { value: Theme; label: string }[] = [
		{ value: 'dark', label: 'Dark' },
		{ value: 'light', label: 'Light' },
		{ value: 'system', label: 'System' }
	];

	const tokens = [
		{ name: 'laptop-cli', id: 'bku_7Hd2wQ', lastUsed: '18 min ago', expires: 'in 64 days' },
		{ name: 'workstation', id: 'bku_3Fa9Kp', lastUsed: '3 days ago', expires: 'in 12 days' }
	];

	const expiryOptions = [
		{ value: '90 days', label: '90 days' },
		{ value: '30 days', label: '30 days' },
		{ value: '1 year', label: '1 year' },
		{ value: 'No expiry', label: 'No expiry' }
	];

	let displayName = $state('Anna Keller');
	let modal = $state<null | 'create' | 'reveal'>(null);
	let ack = $state(false);
	let draftName = $state('');
	let expiry = $state('90 days');

	const revealName = $derived(draftName || 'laptop-cli-2');

	function openCreate() {
		modal = 'create';
	}
	function createToken() {
		modal = 'reveal';
	}
	function closeModal() {
		modal = null;
		ack = false;
		draftName = '';
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
			<span
				class="inline-flex h-12 w-12 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[18px] font-semibold text-accent-text"
				>A</span
			>
			<div class="flex flex-col gap-1">
				<Button variant="secondary" size="sm" class="self-start">Upload avatar</Button>
				<span class="text-xs text-text-3"
					>PNG or JPEG, square, ≤1 MB. Falls back to your initial.</span
				>
			</div>
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Display name">
				<Input size="md" bind:value={displayName} />
			</Field>
			<Field label="Email" hint="Managed by Google OIDC — change it at your identity provider.">
				<Input size="md" mono value="anna@acme.dev" disabled />
			</Field>
		</div>
	</div>

	<div class="{card} gap-2.5">
		<div class={microLabel}>Appearance</div>
		<div class="flex gap-1.5">
			{#each themeOptions as t (t.value)}
				<button
					type="button"
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
			System follows your OS preference. Applies immediately, saved to your account.
		</div>
	</div>

	<div class="{card} gap-3">
		<div class="flex items-center justify-between">
			<div class={microLabel}>Personal access tokens</div>
			<Button variant="primary" size="sm" onclick={openCreate}>Create token</Button>
		</div>
		<div class="text-sm leading-[18px] text-text-2">
			A personal token acts as <span class="font-medium text-text-1">you</span> — everything you
			can access, at most. Use it for the <span class="font-mono text-xs">bakery</span> CLI. For
			bots and builds, mint a <a href="/keys" class="text-accent-text hover:underline">project-scoped
				key</a
			> instead: those stay narrowly scoped and survive you leaving the org.
		</div>
		<TableRoot dense class="w-full table-fixed">
			<thead>
				<tr>
					<Th>Name</Th>
					<Th class="w-[88px]">Token ID</Th>
					<Th class="w-[92px]">Last used</Th>
					<Th class="w-[86px]">Expires</Th>
					<Th class="w-[64px]"></Th>
				</tr>
			</thead>
			<tbody>
				{#each tokens as t (t.id)}
					<Tr>
						<Td class="overflow-hidden text-ellipsis whitespace-nowrap font-medium">{t.name}</Td>
						<Td mono class="whitespace-nowrap">{t.id}</Td>
						<Td class="whitespace-nowrap text-text-2">{t.lastUsed}</Td>
						<Td class="whitespace-nowrap text-text-2">{t.expires}</Td>
						<Td class="text-right">
							<Button variant="ghost" size="sm" class="text-err! hover:text-err!">Revoke</Button>
						</Td>
					</Tr>
				{/each}
			</tbody>
		</TableRoot>
	</div>

	<div class="flex justify-end gap-2">
		<Button variant="ghost" size="md">Discard</Button>
		<Button variant="primary" size="md">Save changes</Button>
	</div>
</div>

{#if modal === 'create'}
	<Modal title="Create personal token" onclose={closeModal}>
		<div class="flex flex-col gap-[14px]">
			<Field label="Name" hint="Name it after the machine or tool that will hold it.">
				<Input size="md" placeholder="laptop-cli" bind:value={draftName} />
			</Field>
			<Field
				label="Expiry"
				hint="Personal tokens default to expiring — they carry your full access."
			>
				<Select size="md" options={expiryOptions} bind:value={expiry} />
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeModal}>Cancel</Button>
			<Button variant="primary" size="md" onclick={createToken}>Create token</Button>
		{/snippet}
	</Modal>
{/if}

{#if modal === 'reveal'}
	<Modal
		title="Token created — this is the only time you will see it"
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
					<span class="font-semibold"
						>This token acts as you across every org and project you can access.</span
					>
					<span class="text-text-2">
						Treat it like your password. Bakery stores only a hash — there is no recovery.</span
					>
				</div>
			</div>
			<CodeBlock title={`${revealName} · expires in 90 days`} copyText={REVEAL_TOKEN}
				><div><span class="tok-comment"># for ~/.config/bakery/config.toml or BAKERY_USER_TOKEN</span></div><div>{REVEAL_TOKEN}</div></CodeBlock
			>
			<Checkbox
				bind:checked={ack}
				label="I have stored the token. I understand it will never be shown again."
			/>
		</div>
		{#snippet footer()}
			<Button variant="primary" size="md" disabled={!ack} onclick={closeModal}>Done</Button>
		{/snippet}
	</Modal>
{/if}
