<script lang="ts">
	import { Button } from '$lib/components/buttons';
	import { Input, Select, Toggle, Field, Label } from '$lib/components/inputs';
	import { KeyValueList } from '$lib/components/content';
	import { Modal } from '$lib/components/feedback';

	let orgName = $state('Acme Robotics');
	let orgSlug = $state('acme');
	let quota = $state('500 GB');
	let gcWindow = $state('Sundays 02:00–06:00 UTC');
	let authDefault = $state(true);

	let showDelete = $state(false);
	let confirmText = $state('');
	const deleteDisabled = $derived(confirmText !== 'acme');

	const gcOptions = [
		{ value: 'Sundays 02:00–06:00 UTC', label: 'Sundays 02:00–06:00 UTC' },
		{ value: 'Daily 03:00–05:00 UTC', label: 'Daily 03:00–05:00 UTC' },
		{ value: 'Continuous (low priority)', label: 'Continuous (low priority)' }
	];

	const instancePairs = [
		{ key: 'Server', value: 'https://bakery.corp', mono: true },
		{ key: 'OIDC providers', value: 'Google · Authelia · GitHub' },
		{ key: 'Storage', value: 's3://bakery-cache (proxied reads)', mono: true },
		{ key: 'Metrics', value: ':9090/metrics — separate listener, not public', mono: true },
		{ key: 'Version', value: 'bakery v1.4.2', mono: true }
	];

	function openDelete() {
		showDelete = true;
		confirmText = '';
	}
	function closeDelete() {
		showDelete = false;
	}
</script>

<div class="flex w-full max-w-[720px] flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Settings</h1>
		<div class="text-sm text-text-2">
			Organization acme — applies to all projects unless overridden.
		</div>
	</div>

	<section class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Organization
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Name">
				<Input size="md" bind:value={orgName} />
			</Field>
			<div class="flex flex-col gap-1">
				<Label>Slug</Label>
				<Input size="md" mono bind:value={orgSlug} />
				<p class="text-sm text-warn">
					Changing the slug changes every cache endpoint — all client configs break until updated.
				</p>
			</div>
		</div>
		<p class="text-sm text-text-3">
			Reserved slugs (blobs, uploads, ac, cas, v2, api, cache, …) are rejected — they collide with
			protocol routes.
		</p>
	</section>

	<section class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Defaults for new backends
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Quota">
				<Input size="md" bind:value={quota} />
			</Field>
			<Field label="GC window">
				<Select size="md" options={gcOptions} bind:value={gcWindow} />
			</Field>
		</div>
		<div class="flex items-start gap-[10px]">
			<div class="mt-px">
				<Toggle bind:checked={authDefault} />
			</div>
			<div>
				<div class="text-base leading-[18px] text-text-1">
					Require authentication on new backends
				</div>
				<div class="mt-0.5 text-sm text-text-3">
					Per-backend override available. Writes always require a key regardless.
				</div>
			</div>
		</div>
	</section>

	<section class="flex flex-col gap-[10px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Instance
		</div>
		<KeyValueList pairs={instancePairs} />
		<p class="text-sm text-text-3">
			Instance-level configuration is env/CLI only — it cannot be changed from this UI.
		</p>
	</section>

	<section class="flex flex-col gap-3 rounded-2 border border-err-border bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-err uppercase">
			Danger zone
		</div>
		<div class="flex items-center justify-between gap-3">
			<div>
				<div class="text-base text-text-1">Delete organization</div>
				<div class="mt-0.5 text-sm text-text-3">
					Deletes all projects, backends, cached objects, and keys. Metadata first, then bytes.
				</div>
			</div>
			<div class="flex-none">
				<Button variant="danger" size="md" onclick={openDelete}>Delete org</Button>
			</div>
		</div>
	</section>

	<div class="flex justify-end gap-2">
		<Button variant="ghost" size="md">Discard</Button>
		<Button variant="primary" size="md">Save changes</Button>
	</div>
</div>

{#if showDelete}
	<Modal title="Delete acme" onclose={closeDelete}>
		<div class="flex flex-col gap-3">
			<div>
				This deletes <span class="font-semibold text-text-1"
					>5 projects, 1.4 TB of cached objects, and 12 API keys</span
				>. Every build pointed at this org starts missing immediately. This cannot be undone.
			</div>
			<Field label="Type the org slug to confirm">
				<Input size="md" mono placeholder="acme" bind:value={confirmText} />
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeDelete}>Cancel</Button>
			<Button variant="danger" size="md" disabled={deleteDisabled} onclick={closeDelete}>
				Delete organization
			</Button>
		{/snippet}
	</Modal>
{/if}
