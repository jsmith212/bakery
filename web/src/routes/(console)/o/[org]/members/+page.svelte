<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import {
		deleteOrgMember,
		deleteProjectMember,
		listProjectMembers,
		putOrgMember,
		putProjectMember
	} from '$lib/api/members';
	import { isApiError } from '$lib/api/errors';
	import type { Member, OrgRole, ProjectRole } from '$lib/api/types';
	import { memberProvenance } from '$lib/api/types';
	import { canAdminOrg } from '$lib/roles';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Field, Input, Select } from '$lib/components/inputs';
	import { Modal, EmptyState } from '$lib/components/feedback';
	import { Provenance } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const projects = $derived(data.projects ?? []);
	const members = $derived(data.members);
	const canAdmin = $derived(canAdminOrg(data.me, org));

	const ORG_ROLES: readonly OrgRole[] = ['member', 'admin', 'owner'];
	const PROJECT_ROLES: readonly ProjectRole[] = ['reader', 'writer', 'admin'];

	function isOrgRole(v: string): v is OrgRole {
		return (ORG_ROLES as readonly string[]).includes(v);
	}
	function isProjectRole(v: string): v is ProjectRole {
		return (PROJECT_ROLES as readonly string[]).includes(v);
	}

	const orgRoleOptions = ORG_ROLES.map((r) => ({ value: r, label: r }));
	const projectRoleOptions = [
		{ value: '', label: '— no role —' },
		...PROJECT_ROLES.map((r) => ({ value: r, label: r }))
	];

	// Per-project roles have no natural home in this org-scoped URL, so which
	// project's roles are showing is plain component state -- reset whenever
	// the visible project list no longer contains the current selection (a
	// fresh mount, or navigating to a different org that reuses this component).
	let selectedProject = $state<string | null>(null);

	$effect(() => {
		if (!projects.some((p) => p.slug === selectedProject)) {
			selectedProject = projects[0]?.slug ?? null;
		}
	});

	let projectMembersByUser = $state<Map<string, Member>>(new Map());
	let projectMembersLoading = $state(false);

	async function refreshProjectMembers() {
		const proj = selectedProject;
		if (!proj) {
			projectMembersByUser = new Map();
			return;
		}
		projectMembersLoading = true;
		try {
			const res = await listProjectMembers(org.slug, proj);
			projectMembersByUser = new Map(res.items.map((m) => [m.user_id, m]));
		} catch (err) {
			toastError(err, 'Could not load project roles');
		} finally {
			projectMembersLoading = false;
		}
	}

	$effect(() => {
		void org.slug;
		void selectedProject;
		refreshProjectMembers();
	});

	function onProjectSelect(raw: string) {
		selectedProject = raw || null;
	}

	let pendingUsers = $state<Set<string>>(new Set());
	function markPending(id: string) {
		pendingUsers = new Set(pendingUsers).add(id);
	}
	function clearPending(id: string) {
		const next = new Set(pendingUsers);
		next.delete(id);
		pendingUsers = next;
	}

	async function onOrgRoleChange(m: Member, raw: string) {
		if (!isOrgRole(raw) || raw === m.org_role) return;
		markPending(m.user_id);
		try {
			await putOrgMember(org.slug, m.user_id, { role: raw });
			pushToast({ variant: 'success', title: `${m.email} is now ${raw}` });
			await invalidateAll();
		} catch (err) {
			toastError(err, `Could not change ${m.email}'s org role`);
		} finally {
			clearPending(m.user_id);
		}
	}

	async function onProjectRoleChange(m: Member, raw: string) {
		if (!selectedProject) return;
		const current = projectMembersByUser.get(m.user_id)?.project_role ?? '';
		if (raw === current) return;

		markPending(m.user_id);
		try {
			if (raw === '') {
				await deleteProjectMember(org.slug, selectedProject, m.user_id);
			} else if (isProjectRole(raw)) {
				await putProjectMember(org.slug, selectedProject, m.user_id, { role: raw });
			}
			pushToast({ variant: 'success', title: `Updated ${m.email}'s role on ${selectedProject}` });
			// The role change may have revoked keys exceeding the new role in the
			// same transaction (members.go:427) -- there is no keys list on this
			// screen, but the project role table itself must reflect the change.
			await refreshProjectMembers();
		} catch (err) {
			toastError(err, `Could not change ${m.email}'s project role`);
		} finally {
			clearPending(m.user_id);
		}
	}

	let removeTarget = $state<Member | null>(null);
	let removePending = $state(false);
	let removeInlineError = $state<{ userId: string; message: string } | null>(null);

	function openRemove(m: Member) {
		removeTarget = m;
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
		try {
			const res = await deleteOrgMember(org.slug, target.user_id);
			if (res.still_a_member) {
				// The API owns only the local half: an OIDC claim still admits
				// them, so a bare "removed" would be the console lying about it.
				pushToast({
					variant: 'warning',
					title: `${target.email} still has access`,
					detail: res.message
				});
			} else {
				pushToast({ variant: 'success', title: `Removed ${target.email}` });
			}
			removeTarget = null;
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.code === 'claim_derived_role') {
				// A backstop, not the normal path -- the button is disabled whenever
				// `local_role` is absent. Rendered in place, using the server's own
				// message, per the dispatch table's `in_place` treatment.
				removeInlineError = { userId: target.user_id, message: err.message };
				removeTarget = null;
			} else {
				toastError(err, 'Could not remove member');
			}
		} finally {
			removePending = false;
		}
	}

	let showGrant = $state(false);
	let grantEmail = $state('');
	let grantOrgRole = $state<string>('member');
	let grantProjectRole = $state<string>('');
	let grantPending = $state(false);
	let grantEmailError = $state<string | null>(null);

	function openGrant() {
		showGrant = true;
		grantEmail = '';
		grantOrgRole = 'member';
		grantProjectRole = '';
		grantEmailError = null;
	}
	function closeGrant() {
		if (grantPending) return;
		showGrant = false;
	}

	async function submitGrant() {
		if (grantPending || !isOrgRole(grantOrgRole)) return;
		grantEmailError = null;
		grantPending = true;

		try {
			await putOrgMember(org.slug, grantEmail, { role: grantOrgRole });

			if (selectedProject && grantProjectRole !== '' && isProjectRole(grantProjectRole)) {
				try {
					await putProjectMember(org.slug, selectedProject, grantEmail, {
						role: grantProjectRole
					});
				} catch (err) {
					// Two separate PUTs, not one atomic invite -- the org grant already
					// succeeded, so a failure here is a partial-failure toast, not a
					// reason to roll the modal back.
					toastError(err, `Granted the org role, but could not set the project role`);
				}
			}

			pushToast({ variant: 'success', title: `Granted ${grantEmail} a role in ${org.slug}` });
			showGrant = false;
			await invalidateAll();
			await refreshProjectMembers();
		} catch (err) {
			if (isApiError(err) && err.code === 'not_found') {
				// `resolveUser`'s own message: "users are provisioned at their first
				// login; ask them to sign in once, then grant the role." Rendered at
				// the field it is about, not invented copy.
				grantEmailError = err.message;
			} else {
				toastError(err, 'Could not grant role');
			}
		} finally {
			grantPending = false;
		}
	}
</script>

<div class="flex w-full max-w-[1080px] flex-col gap-[14px]">
	<div class="flex items-center justify-between">
		<div>
			<h1 class="mb-0.5 text-lg font-semibold text-text-1">Members</h1>
			<div class="text-sm text-text-2">
				{members.length} member{members.length === 1 ? '' : 's'} of {org.slug}
			</div>
		</div>
		{#if canAdmin}
			<Button variant="primary" size="md" onclick={openGrant}>Grant role</Button>
		{/if}
	</div>

	{#if projects.length > 0}
		<div class="flex items-center gap-2 text-sm text-text-2">
			<span>Project role for</span>
			<div class="w-[200px]">
				<Select
					size="sm"
					value={selectedProject ?? ''}
					onchange={(e) => onProjectSelect((e.currentTarget as HTMLSelectElement).value)}
					options={projects.map((p) => ({ value: p.slug, label: p.slug }))}
				/>
			</div>
			{#if projectMembersLoading}<span class="text-xs text-text-3">loading…</span>{/if}
		</div>
	{:else}
		<div class="text-sm text-text-3">Create a project to grant per-project roles.</div>
	{/if}

	{#snippet grantFirstMemberAction()}
		<Button variant="primary" size="md" onclick={openGrant}>Grant a role</Button>
	{/snippet}

	{#if members.length === 0}
		<EmptyState
			glyph="∅"
			title="No members in {org.slug} yet"
			desc="Grant an existing user a role to give them access to this org's projects — they must have signed in to Bakery at least once first."
			action={canAdmin ? grantFirstMemberAction : undefined}
		/>
	{:else}
		<TableWrap>
			<TableRoot dense>
				<thead>
					<tr>
						<Th>Member</Th>
						<Th>Provenance</Th>
						<Th class="w-[128px]">Org role</Th>
						{#if selectedProject}
							<Th class="w-[148px]">Role on {selectedProject}</Th>
						{/if}
						<Th class="w-[80px]"></Th>
					</tr>
				</thead>
				<tbody>
					{#each members as m (m.user_id)}
						{@const busy = pendingUsers.has(m.user_id)}
						<Tr>
							<Td>
								<span class="inline-flex items-center gap-2">
									<span
										class="inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[9px] font-semibold text-accent-text"
										>{(m.display_name || m.email)[0]?.toUpperCase()}</span
									>
									<span class="flex flex-col">
										<span class="font-medium text-text-1">{m.display_name || m.email}</span>
										<span class="font-mono text-xs text-text-3">{m.email}</span>
									</span>
								</span>
							</Td>
							<Td><Provenance provenance={memberProvenance(m)} /></Td>
							<Td>
								<div class="w-[112px]">
									<Select
										size="sm"
										value={m.org_role}
										disabled={!canAdmin || busy}
										onchange={(e) =>
											onOrgRoleChange(m, (e.currentTarget as HTMLSelectElement).value)}
										options={orgRoleOptions}
									/>
								</div>
							</Td>
							{#if selectedProject}
								<Td>
									<div class="w-[132px]">
										<Select
											size="sm"
											value={projectMembersByUser.get(m.user_id)?.project_role ?? ''}
											disabled={!canAdmin || busy}
											onchange={(e) =>
												onProjectRoleChange(m, (e.currentTarget as HTMLSelectElement).value)}
											options={projectRoleOptions}
										/>
									</div>
								</Td>
							{/if}
							<Td class="text-right">
								{#if removeInlineError?.userId === m.user_id}
									<span class="text-xs text-warn">{removeInlineError.message}</span>
								{:else if canAdmin}
									<Button
										variant="ghost"
										size="sm"
										class="text-err!"
										disabled={!m.local_role || busy}
										onclick={() => openRemove(m)}>Remove</Button
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
		Org role: admins manage members, projects, and settings; owners can also delete the org.
		Project role — reader, writer, admin — governs backends and keys on the project selected
		above. Remove is disabled when a member's role comes only from an identity-provider group:
		change the group in the IdP instead.
	</div>
</div>

{#if showGrant}
	<Modal title="Grant role to an existing user" onclose={closeGrant}>
		<div class="flex flex-col gap-[14px]">
			<Field
				label="Email"
				hint="Must match a user who has already signed in to Bakery at least once."
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
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Org role">
					{#snippet children(f)}
						<Select size="md" bind:value={grantOrgRole} options={orgRoleOptions} {...f} />
					{/snippet}
				</Field>
				{#if selectedProject}
					<Field label="Role on {selectedProject}">
						{#snippet children(f)}
							<Select size="md" bind:value={grantProjectRole} options={projectRoleOptions} {...f} />
						{/snippet}
					</Field>
				{/if}
			</div>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeGrant} disabled={grantPending}
				>Cancel</Button
			>
			<Button variant="primary" size="md" onclick={submitGrant} disabled={grantPending}>
				{grantPending ? 'Granting…' : 'Grant role'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if removeTarget}
	<Modal title="Remove {removeTarget.email}" onclose={closeRemove}>
		Clears their local role grant in {org.slug}. If they also hold the role through an
		identity-provider group, they will keep access until that group changes.
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
