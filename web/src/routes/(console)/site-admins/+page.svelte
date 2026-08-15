<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import { deleteSiteAdmin, putSiteAdmin } from '$lib/api/siteAdmins';
	import { isApiError } from '$lib/api/errors';
	import type { SiteAdmin } from '$lib/api/types';
	import { siteAdminProvenance } from '$lib/api/types';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Field, Input } from '$lib/components/inputs';
	import { Modal, EmptyState, Callout } from '$lib/components/feedback';
	import { Provenance } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const admins = $derived(data.admins);
	const grantsAllowed = $derived(data.instance.allow_local_site_admins);

	let pendingUsers = $state<Set<string>>(new Set());
	function markPending(id: string) {
		pendingUsers = new Set(pendingUsers).add(id);
	}
	function clearPending(id: string) {
		const next = new Set(pendingUsers);
		next.delete(id);
		pendingUsers = next;
	}

	let removeTarget = $state<SiteAdmin | null>(null);
	let removePending = $state(false);
	let removeInlineError = $state<{ userId: string; message: string } | null>(null);

	function openRemove(a: SiteAdmin) {
		removeTarget = a;
		removeInlineError = null;
	}
	function closeRemove() {
		if (removePending) return;
		removeTarget = null;
	}

	async function confirmRemove() {
		if (!removeTarget || removePending) return;
		removePending = true;
		const target = removeTarget;
		markPending(target.user_id);
		try {
			const res = await deleteSiteAdmin(target.user_id);
			if (res.still_a_site_admin) {
				// A group claim still grants EVERY privilege in the installation --
				// this is the roster's whole reason to exist, so it is a warning,
				// never a success.
				pushToast({
					variant: 'warning',
					title: `${target.email} is still a site admin`,
					detail: res.message
				});
			} else {
				pushToast({ variant: 'success', title: `Removed ${target.email} as site admin` });
			}
			removeTarget = null;
			await invalidateAll();
		} catch (err) {
			// `claim_derived_role` (409, the ordinary "an LDAP group still grants
			// this") and `forbidden` (403, e.g. the caller's own site-admin
			// session was revoked mid-flow) are both `treatment: 'in_place'` --
			// `toastError` is documented for the `toast` treatment only.
			if (isApiError(err) && err.treatment === 'in_place') {
				removeInlineError = { userId: target.user_id, message: err.message };
				removeTarget = null;
			} else {
				toastError(err, 'Could not remove site admin');
			}
		} finally {
			clearPending(target.user_id);
			removePending = false;
		}
	}

	// Two steps, matching the confirm modal `openRemove`/`confirmRemove` use
	// for the opposite direction: entering an email and clicking the same
	// button that grants it conflates data entry with confirmation, and this
	// is "every privilege in every org" -- the modal's title echoes the exact
	// email about to be granted before the mutating call fires, same as
	// removal's title already echoes `removeTarget.email`.
	let showGrant = $state(false);
	let grantStep = $state<'input' | 'confirm'>('input');
	let grantEmail = $state('');
	let grantPending = $state(false);
	let grantEmailError = $state<string | null>(null);
	let grantError = $state<string | null>(null);

	function openGrant() {
		showGrant = true;
		grantStep = 'input';
		grantEmail = '';
		grantEmailError = null;
		grantError = null;
	}
	function closeGrant() {
		if (grantPending) return;
		showGrant = false;
	}

	function continueGrant() {
		const trimmed = grantEmail.trim();
		if (trimmed === '') {
			grantEmailError = 'Enter an email.';
			return;
		}
		grantEmail = trimmed;
		grantEmailError = null;
		grantStep = 'confirm';
	}

	async function submitGrant() {
		if (grantPending) return;
		grantError = null;
		grantPending = true;
		try {
			const admin = await putSiteAdmin(grantEmail);
			// The server-resolved identity, not the raw string typed into the
			// field -- `{user}` also accepts a user id, and even for an email
			// the stored value is the source of truth for what was granted.
			pushToast({ variant: 'success', title: `${admin.email} is now a site admin` });
			showGrant = false;
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.code === 'not_found') {
				grantEmailError = err.message;
				grantStep = 'input';
			} else if (isApiError(err) && err.treatment === 'in_place') {
				// `forbidden` (--allow-local-site-admins=false, or the caller's
				// own site-admin session was revoked mid-flow): the server's own
				// explanation, in place -- never a generic toast.
				grantError = err.message;
			} else {
				toastError(err, 'Could not grant site admin');
			}
		} finally {
			grantPending = false;
		}
	}
</script>

<div class="flex w-full max-w-[820px] flex-col gap-[14px]">
	<div class="flex items-center justify-between">
		<div>
			<h1 class="mb-0.5 text-lg font-semibold text-text-1">Site admins</h1>
			<div class="text-sm text-text-2">
				Every privilege on this instance, in every org. Provenance is the whole safeguard the
				hybrid role model rests on.
			</div>
		</div>
		<Button
			variant="primary"
			size="md"
			disabled={!grantsAllowed}
			title={grantsAllowed
				? undefined
				: 'Local site-admin grants are disabled on this installation (--allow-local-site-admins=false).'}
			onclick={openGrant}>Grant site admin</Button
		>
	</div>

	{#if !grantsAllowed}
		<Callout variant="info" title="Local site-admin grants are disabled on this installation.">
			<span class="text-text-2">
				Site administrators are derived from OIDC group claims (<span class="font-mono"
					>site_admin_groups</span
				>). Start the server with <span class="font-mono">--allow-local-site-admins</span> to
				grant them here instead.</span
			>
		</Callout>
	{/if}

	{#if admins.length === 0}
		<EmptyState
			glyph="∅"
			title="No site admins"
			desc="At least one exists, or this page would be unreachable — a break-glass grant runs from the CLI (bakery user site-admin) against the database directly, since granting the first one cannot itself require being one."
		/>
	{:else}
		<TableWrap>
			<TableRoot dense>
				<thead>
					<tr>
						<Th>Admin</Th>
						<Th>Provenance</Th>
						<Th class="w-[80px]"></Th>
					</tr>
				</thead>
				<tbody>
					{#each admins as a (a.user_id)}
						<Tr>
							<Td>
								<span class="inline-flex items-center gap-2">
									<span
										class="inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[9px] font-semibold text-accent-text"
										>{(a.display_name || a.email)[0]?.toUpperCase()}</span
									>
									<span class="flex flex-col">
										<span class="font-medium text-text-1">{a.display_name || a.email}</span>
										<span class="font-mono text-xs text-text-3">{a.email}</span>
									</span>
								</span>
							</Td>
							<Td><Provenance provenance={siteAdminProvenance(a)} /></Td>
							<Td class="text-right">
								{#if removeInlineError?.userId === a.user_id}
									<span class="text-xs text-warn">{removeInlineError.message}</span>
								{:else}
									<Button
										variant="ghost"
										size="sm"
										class="text-err!"
										disabled={!a.site_role_local || pendingUsers.has(a.user_id)}
										onclick={() => openRemove(a)}>Remove</Button
									>
								{/if}
							</Td>
						</Tr>
					{/each}
				</tbody>
			</TableRoot>
		</TableWrap>
	{/if}

	<div class="text-xs text-text-3">
		Remove is disabled when an admin's role comes only from an identity-provider group — change
		the group in the IdP instead of here.
	</div>
</div>

{#if showGrant}
	<Modal
		title={grantStep === 'input' ? 'Grant site admin' : `Grant site admin to ${grantEmail}?`}
		onclose={closeGrant}
	>
		{#if grantStep === 'input'}
			<Field
				label="Email"
				hint="Must match a user who has already signed in to Bakery at least once. Grants every privilege in every org."
				error={grantEmailError ?? undefined}
			>
				{#snippet children(f)}
					<Input
						mono
						placeholder="name@acme.dev"
						bind:value={grantEmail}
						error={!!grantEmailError}
						{...f}
					/>
				{/snippet}
			</Field>
		{:else}
			<div class="flex flex-col gap-3">
				<div>
					<span class="font-mono text-text-1">{grantEmail}</span> will gain every privilege in every
					org on this instance, effective immediately.
				</div>
				{#if grantError}
					<Callout variant="error">{grantError}</Callout>
				{/if}
			</div>
		{/if}
		{#snippet footer()}
			{#if grantStep === 'input'}
				<Button variant="ghost" size="md" onclick={closeGrant}>Cancel</Button>
				<Button variant="primary" size="md" onclick={continueGrant}>Continue</Button>
			{:else}
				<Button
					variant="ghost"
					size="md"
					onclick={() => (grantStep = 'input')}
					disabled={grantPending}>Back</Button
				>
				<Button variant="danger" size="md" onclick={submitGrant} disabled={grantPending}>
					{grantPending ? 'Granting…' : 'Grant site admin'}
				</Button>
			{/if}
		{/snippet}
	</Modal>
{/if}

{#if removeTarget}
	<Modal title="Remove {removeTarget.email} as site admin" onclose={closeRemove}>
		Clears their local site-admin grant. If they also hold it through an identity-provider group,
		they will keep every privilege on this instance until that group changes.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeRemove} disabled={removePending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" onclick={confirmRemove} disabled={removePending}>
				{removePending ? 'Removing…' : 'Remove'}
			</Button>
		{/snippet}
	</Modal>
{/if}
