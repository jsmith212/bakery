<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';

	import { createOrg } from '$lib/api/orgs';
	import { isApiError } from '$lib/api/errors';
	import { canCreateOrg } from '$lib/roles';
	import { orgPath } from '$lib/tenancy';
	import { pushToast, toastError } from '$lib/toasts';
	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { EmptyState, Modal } from '$lib/components/feedback';
	import { Field, Input } from '$lib/components/inputs';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const orgs = $derived(data.orgs);

	// `allow_self_serve_orgs` is a deployment choice on the PUBLIC
	// `/auth/config`; a site admin may always create one regardless. Without
	// consulting both, this button 403s with no explanation on an installation
	// that restricts org creation.
	const mayCreate = $derived(canCreateOrg(data.me, data.authConfig));

	let creating = $state(false);
	let slug = $state('');
	let name = $state('');
	let pending = $state(false);
	let slugError = $state<string | undefined>(undefined);
	let nameError = $state<string | undefined>(undefined);
	let formError = $state<string | null>(null);

	function openCreate() {
		slug = '';
		name = '';
		slugError = undefined;
		nameError = undefined;
		formError = null;
		creating = true;
	}

	function closeCreate() {
		if (pending) return;
		creating = false;
	}

	async function submit() {
		if (pending) return;
		pending = true;
		slugError = undefined;
		nameError = undefined;
		formError = null;

		try {
			const org = await createOrg({ slug: slug.trim(), name: name.trim() });
			creating = false;
			pushToast({
				variant: 'success',
				title: `Created ${org.slug}`,
				detail: 'You are its owner. Add a project next.'
			});
			await invalidateAll();
			await goto(`${orgPath(org.slug)}/projects`);
		} catch (err) {
			// Branch on the CODE, never the status: `reserved_slug` and
			// `invalid_slug` are both 422 and both belong on the slug field, while
			// `forbidden` is an installation policy that no field can fix.
			if (isApiError(err)) {
				switch (err.treatment) {
					case 'field':
						if (err.field === 'name') nameError = err.message;
						else slugError = err.message;
						break;
					case 'in_place':
						formError = err.message;
						break;
					default:
						toastError(err, 'Could not create the organization');
				}
			} else {
				toastError(err, 'Could not create the organization');
			}
		} finally {
			pending = false;
		}
	}
</script>

<div class="flex items-center justify-between">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Organizations</h1>
		<div class="tabular text-sm text-text-2">
			{orgs.length}
			{orgs.length === 1 ? 'organization' : 'organizations'}
		</div>
	</div>
	{#if mayCreate}
		<Button variant="primary" size="md" onclick={openCreate}>New organization</Button>
	{/if}
</div>

{#snippet createOrgAction()}
	<Button variant="primary" size="md" onclick={openCreate}>Create an organization</Button>
{/snippet}

{#if orgs.length === 0}
	<EmptyState
		glyph="∅"
		title="No organizations yet"
		desc={mayCreate
			? 'An organization holds projects, members and cache backends. Create one, add a project, and the config snippet generator will give you the exact client config.'
			: 'This installation restricts organization creation to site administrators. Ask one to create an organization and grant you a role in it.'}
		action={mayCreate ? createOrgAction : undefined}
	/>
{:else}
	<div class="grid grid-cols-3 gap-2.5">
		{#each orgs as o (o.id)}
			<a href="{orgPath(o.slug)}/projects" class="block no-underline">
				<div
					class="flex flex-col gap-2.5 rounded-2 border border-border-0 bg-bg-1 p-[14px] transition-[border-color] duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2"
				>
					<div class="flex items-center gap-2">
						<span class="font-mono text-base font-medium text-text-1">{o.slug}</span>
						{#if o.role}
							<span class="ml-auto"><Badge variant="type">{o.role}</Badge></span>
						{/if}
					</div>
					<div class="overflow-hidden text-ellipsis whitespace-nowrap text-sm text-text-2">
						{o.name}
					</div>
				</div>
			</a>
		{/each}
	</div>
{/if}

{#if creating}
	<Modal title="New organization" onclose={closeCreate}>
		<div class="flex flex-col gap-3">
			<Field
				label="Slug"
				hint="Lower-case, the first segment of every cache URL for this org. It cannot be changed later."
				error={slugError}
			>
				{#snippet children(f)}
					<Input size="md" mono placeholder="acme" bind:value={slug} error={!!slugError} {...f} />
				{/snippet}
			</Field>
			<Field label="Name" hint="Defaults to the slug." error={nameError}>
				{#snippet children(f)}
					<Input size="md" placeholder="Acme" bind:value={name} error={!!nameError} {...f} />
				{/snippet}
			</Field>
			{#if formError}
				<div class="text-sm text-err">{formError}</div>
			{/if}
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" disabled={pending} onclick={closeCreate}>Cancel</Button>
			<Button variant="primary" size="md" disabled={pending || slug.trim() === ''} onclick={submit}>
				{pending ? 'Creating…' : 'Create organization'}
			</Button>
		{/snippet}
	</Modal>
{/if}
