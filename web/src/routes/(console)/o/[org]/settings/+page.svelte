<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';

	import { deleteOrg, updateOrg } from '$lib/api/orgs';
	import { isApiError } from '$lib/api/errors';
	import { tri } from '$lib/api/patch';
	import { canAdminOrg, canOwnOrg } from '$lib/roles';
	import { formatBytes, formatQuota, formatRetentionWindow, parseHumanBytes } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Input, Checkbox, Field, Label } from '$lib/components/inputs';
	import { Modal } from '$lib/components/feedback';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const projects = $derived(data.projects ?? []);
	const usage = $derived(data.usage);

	const canEdit = $derived(canAdminOrg(data.me, org));
	const canDelete = $derived(canOwnOrg(data.me, org));

	const projectCount = $derived(projects.length);
	const totalBytes = $derived(usage.reduce((sum, u) => sum + u.logical_bytes, 0));

	function resetForm() {
		draftName = org.name;
		retentionForever = org.default_retention_window === null;
		retentionValue = org.default_retention_window ?? '';
		quotaNoCap = org.default_quota_bytes === null;
		quotaValue = org.default_quota_bytes !== null ? formatBytes(org.default_quota_bytes) : '';
		nameError = null;
		formError = null;
	}

	let draftName = $state('');
	let retentionForever = $state(false);
	let retentionValue = $state('');
	let quotaNoCap = $state(true);
	let quotaValue = $state('');
	let nameError = $state<string | null>(null);
	let formError = $state<string | null>(null);
	let savePending = $state(false);

	// Tracks every field `resetForm` reads, so the draft re-syncs both on a
	// same-component org switch and right after a successful save's
	// `invalidateAll` -- at which point the draft already equals what was just
	// submitted, so this is a no-op there and a correctness fix everywhere else.
	$effect(() => {
		resetForm();
	});

	async function save() {
		if (savePending) return;
		nameError = null;
		formError = null;

		// Both defaults are 3-state (`patch.ts`): `undefined` here OMITS the key
		// so an unrelated edit (renaming the org, say) cannot silently rewrite a
		// default nobody touched. Diffed against `org`, not against whatever the
		// form last held, because `org` is the value the server actually has.
		let retentionWindow: string | null | undefined;
		if (retentionForever) {
			retentionWindow = org.default_retention_window === null ? undefined : null;
		} else if (retentionValue.trim() === '') {
			formError = 'Enter a default retention like "2160h", or check "retain forever".';
			return;
		} else {
			const trimmed = retentionValue.trim();
			retentionWindow = trimmed === (org.default_retention_window ?? '') ? undefined : trimmed;
		}

		let quotaBytes: number | null | undefined;
		if (quotaNoCap) {
			quotaBytes = org.default_quota_bytes === null ? undefined : null;
		} else if (quotaValue.trim() === '') {
			formError = 'Enter a default quota like "500 GB", or check "no cap".';
			return;
		} else if (quotaValue === formatBytes(org.default_quota_bytes)) {
			// Unedited: omit rather than re-parse `formatBytes`'s rounded display
			// string, which is lossy (`parseHumanBytes(formatBytes(n)) !== n` for
			// values that are not a round number of the chosen unit).
			quotaBytes = undefined;
		} else {
			try {
				quotaBytes = parseHumanBytes(quotaValue);
			} catch (err) {
				formError = err instanceof Error ? err.message : 'That quota is not valid.';
				return;
			}
		}

		savePending = true;
		try {
			await updateOrg(org.slug, {
				name: draftName,
				default_retention_window: tri(retentionWindow, 'default_retention_window'),
				default_quota_bytes: tri(quotaBytes, 'default_quota_bytes')
			});
			pushToast({ variant: 'success', title: 'Settings saved' });
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'name') {
				nameError = err.message;
			} else if (isApiError(err) && (err.field || err.code === 'forbidden')) {
				formError = err.message;
			} else {
				toastError(err, 'Could not save settings');
			}
		} finally {
			savePending = false;
		}
	}

	let showDelete = $state(false);
	let confirmText = $state('');
	let deletePending = $state(false);
	const deleteDisabled = $derived(confirmText !== org.slug || deletePending);

	function openDelete() {
		showDelete = true;
		confirmText = '';
	}
	function closeDelete() {
		if (deletePending) return;
		showDelete = false;
	}

	async function confirmDelete() {
		if (deleteDisabled) return;
		deletePending = true;
		try {
			await deleteOrg(org.slug);
			pushToast({ variant: 'success', title: `Deleted ${org.slug}` });
			await goto('/orgs');
		} catch (err) {
			toastError(err, 'Could not delete organization');
			deletePending = false;
			showDelete = false;
		}
	}
</script>

<div class="flex w-full max-w-[720px] flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Settings</h1>
		<div class="text-sm text-text-2">
			Organization {org.slug} — defaults apply to new backends unless overridden per backend.
		</div>
	</div>

	<section class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Organization
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Name" error={nameError ?? undefined}>
				{#snippet children(f)}
					<Input size="md" bind:value={draftName} disabled={!canEdit} error={!!nameError} {...f} />
				{/snippet}
			</Field>
			<div class="flex flex-col gap-1">
				<Label>Slug</Label>
				<Input size="md" mono value={org.slug} disabled />
				<p class="text-sm text-text-3">
					Slugs are immutable — every cache URL under this org embeds it.
				</p>
			</div>
		</div>
	</section>

	<section class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Defaults for new backends
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field
				label="Default retention"
				hint="Seeds a new backend's retention window. Existing backends are unaffected — edit them individually."
			>
				{#snippet children(f)}
					<Input
						size="md"
						mono
						placeholder="2160h"
						bind:value={retentionValue}
						disabled={!canEdit || retentionForever}
						{...f}
					/>
					<Checkbox bind:checked={retentionForever} disabled={!canEdit} label="Retain forever by default" />
				{/snippet}
			</Field>
			<Field label="Default quota" hint="A new backend's storage cap, e.g. 500 GB.">
				{#snippet children(f)}
					<Input
						size="md"
						placeholder="500 GB"
						bind:value={quotaValue}
						disabled={!canEdit || quotaNoCap}
						{...f}
					/>
					<Checkbox bind:checked={quotaNoCap} disabled={!canEdit} label="No cap by default" />
				{/snippet}
			</Field>
		</div>
		<p class="text-sm text-text-3">
			Currently: {formatRetentionWindow(org.default_retention_window)} · {formatQuota(
				org.default_quota_bytes
			)}.
		</p>
		{#if formError}
			<p class="text-sm text-err">{formError}</p>
		{/if}
	</section>

	{#if canEdit}
		<div class="flex justify-end gap-2">
			<Button variant="ghost" size="md" onclick={resetForm} disabled={savePending}
				>Discard</Button
			>
			<Button variant="primary" size="md" onclick={save} disabled={savePending}>
				{savePending ? 'Saving…' : 'Save changes'}
			</Button>
		</div>
	{/if}

	{#if canDelete}
		<section class="flex flex-col gap-3 rounded-2 border border-err-border bg-bg-1 p-[14px]">
			<div class="text-xs font-medium tracking-[var(--tracking-label)] text-err uppercase">
				Danger zone
			</div>
			<div class="flex items-center justify-between gap-3">
				<div>
					<div class="text-base text-text-1">Delete organization</div>
					<div class="mt-0.5 text-sm text-text-3">
						Deletes <span class="tabular text-text-1">{projectCount}</span> project{projectCount ===
						1
							? ''
							: 's'}, <span class="tabular text-text-1">{formatBytes(totalBytes)}</span> of cached
						objects, and every key and member grant under this org. Metadata first, then bytes.
						Every build pointed at this org starts missing immediately. This cannot be undone.
					</div>
				</div>
				<div class="flex-none">
					<Button variant="danger" size="md" onclick={openDelete}>Delete org</Button>
				</div>
			</div>
		</section>
	{/if}
</div>

{#if showDelete}
	<Modal title="Delete {org.slug}" onclose={closeDelete}>
		<div class="flex flex-col gap-3">
			<div>
				This deletes <span class="font-semibold text-text-1"
					>{projectCount} project{projectCount === 1 ? '' : 's'} and {formatBytes(
						totalBytes
					)} of cached objects</span
				>. Every build pointed at this org starts missing immediately. This cannot be undone.
			</div>
			<Field label="Type the org slug to confirm">
				{#snippet children(f)}
					<Input size="md" mono placeholder={org.slug} bind:value={confirmText} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeDelete} disabled={deletePending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" disabled={deleteDisabled} onclick={confirmDelete}>
				{deletePending ? 'Deleting…' : 'Delete organization'}
			</Button>
		{/snippet}
	</Modal>
{/if}
