<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';

	import { deleteProject, updateProject } from '$lib/api/projects';
	import { isApiError } from '$lib/api/errors';
	import { canAdminOrg, canAdminProject } from '$lib/roles';
	import { formatBytes, formatCount } from '$lib/format';
	import { orgPath } from '$lib/tenancy';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Input, Field, Label } from '$lib/components/inputs';
	import { Modal } from '$lib/components/feedback';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const usage = $derived(data.usage);

	// TWO DIFFERENT GATES, and they are not interchangeable. Renaming is
	// AccessProjectAdmin; deleting is AccessOrgAdmin, deliberately -- a project admin
	// is a role the org's admins hand out, and letting the recipient of a delegated
	// role destroy the thing it was delegated over is the wrong default. Gating both
	// on the same helper would either hide the rename from a project admin or show
	// them a delete button that 403s.
	const canRename = $derived(canAdminProject(data.me, project));
	const canDelete = $derived(canAdminOrg(data.me, org));

	const totalObjects = $derived(usage.reduce((sum, u) => sum + (u.objects_count ?? 0), 0));
	const totalBytes = $derived(usage.reduce((sum, u) => sum + (u.logical_bytes ?? 0), 0));
	const backendCount = $derived(usage.length);

	let draftName = $state('');
	let nameError = $state<string | null>(null);
	let savePending = $state(false);

	function resetForm() {
		draftName = project.name;
		nameError = null;
	}

	// Re-syncs the draft on a same-component project switch AND right after a
	// successful save's invalidateAll -- at which point it already equals what was
	// submitted, so it is a no-op there and a correctness fix everywhere else. Same
	// arrangement as the org settings page.
	$effect(() => {
		resetForm();
	});

	async function save() {
		if (savePending) return;
		nameError = null;

		const name = draftName.trim();
		if (name === '') {
			nameError = 'Enter a name.';
			return;
		}

		savePending = true;

		try {
			// The request body is built from the typed interface and never spread from
			// component state: the server decodes with DisallowUnknownFields, so one extra
			// key is a hard 400.
			await updateProject(org.slug, project.slug, { name });
			pushToast({ variant: 'success', title: 'Settings saved' });
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'name') {
				nameError = err.message;
			} else if (isApiError(err) && err.code === 'forbidden') {
				nameError = err.message;
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
	const deleteDisabled = $derived(confirmText !== project.slug || deletePending);

	function openDelete() {
		confirmText = '';
		showDelete = true;
	}

	function closeDelete() {
		if (deletePending) return;
		showDelete = false;
	}

	async function confirmDelete() {
		if (deleteDisabled) return;
		deletePending = true;

		try {
			// 204 resolves to `undefined` (the project is gone); 202 resolves to the
			// project with `deleting_at` set (its backends are being emptied). The client
			// reports the decoded body and no status, so the body IS the distinction.
			// Either way the project has stopped resolving, so both navigate AWAY -- this
			// screen cannot re-render itself, and a plain invalidateAll in place would
			// re-fetch a project that now 404s.
			const result = await deleteProject(org.slug, project.slug);
			pushToast({
				variant: 'success',
				title: result ? `Deleting ${project.slug}…` : `Deleted ${project.slug}`,
				detail: result
					? 'Its cached objects are being removed by the garbage collector.'
					: undefined
			});
			// invalidateAll, and it is NOT redundant with the navigation. The org layout
			// (`o/[org]/+layout.ts`) owns the project list the grid renders, and its params
			// do not change on the way from here to `/o/{org}/projects` -- so SvelteKit
			// reuses its data and the grid paints the project that was just deleted. The
			// server already excludes it from the listing; this is what makes the client
			// ask again.
			await goto(`${orgPath(org.slug)}/projects`, { invalidateAll: true });
		} catch (err) {
			toastError(err, 'Could not delete project');
			deletePending = false;
			showDelete = false;
		}
	}
</script>

<div class="flex w-full max-w-[720px] flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Settings</h1>
		<div class="text-sm text-text-2">
			Project {org.slug}/{project.slug} — backends, retention and quotas are configured per
			backend.
		</div>
	</div>

	<section class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Project
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field label="Name" error={nameError ?? undefined}>
				{#snippet children(f)}
					<Input size="md" bind:value={draftName} disabled={!canRename} error={!!nameError} {...f} />
				{/snippet}
			</Field>
			<div class="flex flex-col gap-1">
				<Label>Slug</Label>
				<Input size="md" mono value={project.slug} disabled />
				<p class="text-sm text-text-3">
					Slugs are immutable — every cache URL under this project embeds it.
				</p>
			</div>
		</div>
	</section>

	{#if canRename}
		<div class="flex justify-end gap-2">
			<Button variant="ghost" size="md" onclick={resetForm} disabled={savePending}>Discard</Button>
			<Button variant="primary" size="md" onclick={save} disabled={savePending}>
				{savePending ? 'Saving…' : 'Save changes'}
			</Button>
		</div>
	{/if}

	<!-- The danger zone is VISIBLE to everyone and ENABLED only for org admins. A
	     hidden control leaves a project admin wondering where deletion lives; a live
	     one 403s. The caption is what makes the disabled state honest. -->
	<section class="flex flex-col gap-3 rounded-2 border border-err-border bg-bg-1 p-[14px]">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-err uppercase">
			Danger zone
		</div>
		<div class="flex items-center justify-between gap-3">
			<div>
				<div class="text-base text-text-1">Delete project</div>
				<div class="mt-0.5 text-sm text-text-3">
					Unmounts <span class="tabular text-text-1">{backendCount}</span>
					backend{backendCount === 1 ? '' : 's'} immediately and hands
					<span class="tabular text-text-1">{formatCount(totalObjects)}</span>
					objects (<span class="tabular text-text-1">{formatBytes(totalBytes)}</span>) to the
					garbage collector, along with every key and project role under it. Every build pointed at
					this project starts missing. This cannot be undone.
				</div>
				{#if !canDelete}
					<div class="mt-1.5 text-sm text-text-3">
						Deleting a project takes an organization admin — a project admin cannot destroy the
						project their role was delegated over.
					</div>
				{/if}
			</div>
			<div class="flex-none">
				<Button variant="danger" size="md" disabled={!canDelete} onclick={openDelete}>
					Delete project
				</Button>
			</div>
		</div>
	</section>
</div>

{#if showDelete}
	<Modal title="Delete {project.slug}" onclose={closeDelete}>
		<div class="flex flex-col gap-3">
			<div>
				This deletes <span class="font-semibold text-text-1"
					>{backendCount} backend{backendCount === 1 ? '' : 's'} and {formatCount(totalObjects)} cached
					objects ({formatBytes(totalBytes)})</span
				>. Every build pointed at this project starts missing immediately. A project holding
				objects is emptied by the garbage collector, so it may take a sweep or two to disappear
				entirely. This cannot be undone.
			</div>
			<Field label="Type the project slug to confirm">
				{#snippet children(f)}
					<Input size="md" mono placeholder={project.slug} bind:value={confirmText} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeDelete} disabled={deletePending}>Cancel</Button>
			<Button variant="danger" size="md" disabled={deleteDisabled} onclick={confirmDelete}>
				{deletePending ? 'Deleting…' : 'Delete project'}
			</Button>
		{/snippet}
	</Modal>
{/if}
