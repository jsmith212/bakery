<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import {
		deleteOrgMember,
		deleteProjectMember,
		listProjectMembers,
		putOrgMember,
		putProjectMember
	} from '$lib/api/members';
	import { createRobot, createOrgToken, deleteRobot, revokeOrgToken } from '$lib/api/robots';
	import { isApiError } from '$lib/api/errors';
	import type { CreatedOrgToken, KeyScope, Member, OrgRole, OrgToken, ProjectRole, Robot } from '$lib/api/types';
	import { memberProvenance } from '$lib/api/types';
	import { canAdminOrg } from '$lib/roles';
	import { formatDateTimeUTC, formatExpiry } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Field, Input, Select, Checkbox } from '$lib/components/inputs';
	import { Modal, EmptyState, Callout } from '$lib/components/feedback';
	import { CodeBlock } from '$lib/components/content';
	import { Provenance } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const projects = $derived(data.projects ?? []);
	const members = $derived(data.members);
	const robots = $derived(data.robots ?? []);
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

	// -------------------------------------------------------------------------
	// Robots (org-owned machine identities, `bkro_` tokens). ROBOTS card,
	// below the human roster -- see the section copy in the markup for why a
	// robot never appears in the table above.
	// -------------------------------------------------------------------------

	// Robot tokens must have an expiry (auth.MaxOrgTokenLifetime = 365d), unlike
	// a personal token or an API key -- there is deliberately no "never" preset.
	const robotExpiryOptions = [
		{ value: '30', label: '30 days' },
		{ value: '90', label: '90 days' },
		{ value: '365', label: '1 year' }
	];

	let robotModal = $state<null | 'create-robot' | 'create-token' | 'reveal' | 'revoke-token' | 'delete-robot'>(
		null
	);

	let robotDraftName = $state('');
	let robotDraftDescription = $state('');
	let robotCreatePending = $state(false);
	let robotNameError = $state<string | null>(null);

	function openCreateRobot() {
		robotModal = 'create-robot';
		robotDraftName = '';
		robotDraftDescription = '';
		robotNameError = null;
	}

	async function submitCreateRobot() {
		if (robotCreatePending) return;
		robotCreatePending = true;
		robotNameError = null;

		try {
			await createRobot(org.slug, { name: robotDraftName, description: robotDraftDescription });
			pushToast({ variant: 'success', title: `Created robot ${robotDraftName}` });
			robotModal = null;
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'name') {
				robotNameError = err.message;
			} else {
				toastError(err, 'Could not create robot');
			}
		} finally {
			robotCreatePending = false;
		}
	}

	let deleteRobotTarget = $state<Robot | null>(null);
	let deleteRobotPending = $state(false);

	function openDeleteRobot(robot: Robot) {
		deleteRobotTarget = robot;
		robotModal = 'delete-robot';
	}

	async function confirmDeleteRobot() {
		if (!deleteRobotTarget || deleteRobotPending) return;
		deleteRobotPending = true;

		try {
			await deleteRobot(org.slug, deleteRobotTarget.id);
			pushToast({ variant: 'success', title: `Deleted robot ${deleteRobotTarget.name}` });
			robotModal = null;
			deleteRobotTarget = null;
			await invalidateAll();
		} catch (err) {
			toastError(err, 'Could not delete robot');
		} finally {
			deleteRobotPending = false;
		}
	}

	let tokenTargetRobot = $state<Robot | null>(null);
	let tokenDraftName = $state('');
	let tokenDraftScope = $state<KeyScope>('write');
	let tokenExpiryDays = $state('90');
	let tokenCreatePending = $state(false);
	let tokenNameError = $state<string | null>(null);
	let tokenExpiryError = $state<string | null>(null);

	function openCreateToken(robot: Robot) {
		tokenTargetRobot = robot;
		robotModal = 'create-token';
		tokenDraftName = '';
		tokenDraftScope = 'write';
		tokenExpiryDays = '90';
		tokenNameError = null;
		tokenExpiryError = null;
	}

	let revealedOrgToken = $state<CreatedOrgToken | null>(null);
	let tokenAck = $state(false);

	async function submitCreateToken() {
		if (!tokenTargetRobot || tokenCreatePending) return;
		tokenCreatePending = true;
		tokenNameError = null;
		tokenExpiryError = null;

		try {
			const expiresAt = new Date(
				Date.now() + Number(tokenExpiryDays) * 24 * 60 * 60 * 1000
			).toISOString();

			const created = await createOrgToken(org.slug, tokenTargetRobot.id, {
				name: tokenDraftName,
				scope: tokenDraftScope,
				expires_at: expiresAt
			});
			revealedOrgToken = created;
			robotModal = 'reveal';
			tokenAck = false;
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'name') {
				tokenNameError = err.message;
			} else if (isApiError(err) && err.field === 'expires_at') {
				tokenExpiryError = err.message;
			} else {
				toastError(err, 'Could not create token');
			}
		} finally {
			tokenCreatePending = false;
		}
	}

	let revokeTokenTarget = $state<{ robot: Robot; token: OrgToken } | null>(null);
	let revokeTokenPending = $state(false);

	function openRevokeToken(robot: Robot, token: OrgToken) {
		revokeTokenTarget = { robot, token };
		robotModal = 'revoke-token';
	}

	async function confirmRevokeToken() {
		if (!revokeTokenTarget || revokeTokenPending) return;
		revokeTokenPending = true;

		try {
			await revokeOrgToken(org.slug, revokeTokenTarget.robot.id, revokeTokenTarget.token.id);
			pushToast({ variant: 'success', title: `Revoked ${revokeTokenTarget.token.name}` });
			robotModal = null;
			revokeTokenTarget = null;
			await invalidateAll();
		} catch (err) {
			toastError(err, 'Could not revoke token');
		} finally {
			revokeTokenPending = false;
		}
	}

	function closeRobotModal() {
		if (robotCreatePending || deleteRobotPending || tokenCreatePending || revokeTokenPending) return;
		robotModal = null;
		deleteRobotTarget = null;
		tokenTargetRobot = null;
		revealedOrgToken = null;
		revokeTokenTarget = null;
		tokenAck = false;
	}

	// Every robot's tokens, live first then revoked -- same idiom as the
	// project Keys screen and the personal-token table.
	function sortedTokens(robot: Robot): OrgToken[] {
		return [...robot.tokens].sort((a, b) => (a.revoked_at ? 1 : 0) - (b.revoked_at ? 1 : 0));
	}

	// `robot.tokens` carries every token this robot ever held, live AND
	// revoked (ListOrgTokensForOrg has no revoked_at filter -- the audit
	// trail needs the dead ones too). The "authorizes nothing" empty state
	// is about LIVE tokens specifically: a robot with three revoked tokens
	// and zero live ones is exactly the state that line exists to call out.
	function hasLiveToken(robot: Robot): boolean {
		return robot.tokens.some((t) => !t.revoked_at);
	}

	const expiryClass: Record<'none' | 'soon' | 'expired', string> = {
		none: 'text-text-2',
		soon: 'text-warn',
		expired: 'text-err'
	};
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

	{#if canAdmin}
		<div class="mt-2 flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
			<div class="flex items-center justify-between">
				<div>
					<div
						class="text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
					>
						Robots
					</div>
					<div class="text-sm leading-[18px] text-text-2">
						Robots are org-wide machine identities. A robot's token reads and writes every
						project in {org.slug}, including projects created after it was minted. It is not a
						user: it has no console session, cannot administer anything, and cannot be given a
						per-project role.
					</div>
				</div>
				<Button variant="primary" size="md" onclick={openCreateRobot}>Create robot</Button>
			</div>

			{#if robots.length === 0}
				<EmptyState
					glyph="∅"
					title="No robots in {org.slug} yet"
					desc="Create one for CI: a robot's token authenticates as that robot, org-wide, until it expires or you revoke it."
				/>
			{:else}
				<div class="flex flex-col gap-3">
					{#each robots as robot (robot.id)}
						<div class="rounded-1 border border-border-1">
							<div class="flex items-center justify-between border-b border-border-1 px-3 py-2">
								<div class="flex flex-col">
									<span class="font-medium text-text-1">{robot.name}</span>
									<span class="text-xs text-text-3">
										{robot.description ? robot.description + ' · ' : ''}created by {robot.created_by_email}
										on {formatDateTimeUTC(robot.created_at)}
									</span>
								</div>
								<div class="flex items-center gap-2">
									<Button variant="secondary" size="sm" onclick={() => openCreateToken(robot)}
										>New token</Button
									>
									<Button
										variant="ghost"
										size="sm"
										class="text-err! hover:text-err!"
										onclick={() => openDeleteRobot(robot)}>Delete</Button
									>
								</div>
							</div>

							{#if !hasLiveToken(robot)}
								<!-- Above the table, never instead of it: revoked rows carry
								     created_by_email/created_at — the audit trail this card
								     exists to keep visible. -->
								<div class="px-3 py-2.5 text-xs text-text-3">
									No live tokens. This robot authorizes nothing until one is minted.
								</div>
							{/if}
							{#if robot.tokens.length > 0}
								<TableWrap>
									<TableRoot dense>
										<thead>
											<tr>
												<Th>Name</Th>
												<Th>Status</Th>
												<Th>Prefix</Th>
												<Th>Scope</Th>
												<Th>Created by</Th>
												<Th>Created</Th>
												<Th>Last used</Th>
												<Th>Expires</Th>
												<Th class="w-[72px]"></Th>
											</tr>
										</thead>
										<tbody>
											{#each sortedTokens(robot) as t (t.id)}
												{@const expiry = formatExpiry(t.expires_at)}
												<Tr>
													<Td class="font-medium">{t.name}</Td>
													<Td>
														{#if t.revoked_at}
															<Badge status="miss">revoked</Badge>
														{:else if expiry.kind === 'expired'}
															<Badge status="stale">expired</Badge>
														{:else}
															<Badge status="hit">live</Badge>
														{/if}
													</Td>
													<Td mono>{t.token_prefix}</Td>
													<Td><Badge variant="type">{t.scope}</Badge></Td>
													<Td class="text-text-2">{t.created_by_email}</Td>
													<Td mono class="whitespace-nowrap">{formatDateTimeUTC(t.created_at)}</Td
													>
													<Td class="whitespace-nowrap text-text-2"
														>{t.last_used_at ? formatDateTimeUTC(t.last_used_at) : 'never'}</Td
													>
													<Td class="whitespace-nowrap {expiryClass[expiry.kind]}"
														>{expiry.label}</Td
													>
													<Td class="text-right">
														<Button
															variant="ghost"
															size="sm"
															class="text-err! hover:text-err!"
															disabled={!!t.revoked_at}
															onclick={() => openRevokeToken(robot, t)}>Revoke</Button
														>
													</Td>
												</Tr>
											{/each}
										</tbody>
									</TableRoot>
								</TableWrap>
							{/if}
						</div>
					{/each}
				</div>
			{/if}
		</div>
	{/if}
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

{#if robotModal === 'create-robot'}
	<Modal title="Create robot" onclose={closeRobotModal}>
		<div class="flex flex-col gap-[14px]">
			<Field label="Name" hint="Shown on the ROBOTS card." error={robotNameError ?? undefined}>
				{#snippet children(f)}
					<Input
						size="md"
						placeholder="ci-runner"
						bind:value={robotDraftName}
						error={!!robotNameError}
						{...f}
					/>
				{/snippet}
			</Field>
			<Field label="Description" hint="Optional. What this robot is for.">
				{#snippet children(f)}
					<Input size="md" placeholder="Bazel remote cache for the CI fleet" bind:value={robotDraftDescription} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeRobotModal} disabled={robotCreatePending}
				>Cancel</Button
			>
			<Button variant="primary" size="md" onclick={submitCreateRobot} disabled={robotCreatePending}>
				{robotCreatePending ? 'Creating…' : 'Create robot'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if robotModal === 'create-token' && tokenTargetRobot}
	<Modal title={`New token for ${tokenTargetRobot.name}`} onclose={closeRobotModal}>
		<div class="flex flex-col gap-[14px]">
			<Field label="Name" hint="Shown in the token table." error={tokenNameError ?? undefined}>
				{#snippet children(f)}
					<Input
						size="md"
						placeholder="ci-2026"
						bind:value={tokenDraftName}
						error={!!tokenNameError}
						{...f}
					/>
				{/snippet}
			</Field>
			<Field label="Scope">
				{#snippet children(f)}
					<Select
						size="md"
						bind:value={tokenDraftScope}
						options={[
							{ value: 'read', label: 'Read only' },
							{ value: 'write', label: 'Read + write' }
						]}
						{...f}
					/>
				{/snippet}
			</Field>
			<Field
				label="Expiry"
				hint="Required, capped at 1 year. A robot deliberately outlives its creator, so expiry is the countervailing control — there is no never preset."
				error={tokenExpiryError ?? undefined}
			>
				{#snippet children(f)}
					<Select size="md" options={robotExpiryOptions} bind:value={tokenExpiryDays} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeRobotModal} disabled={tokenCreatePending}
				>Cancel</Button
			>
			<Button variant="primary" size="md" onclick={submitCreateToken} disabled={tokenCreatePending}>
				{tokenCreatePending ? 'Creating…' : 'Create token'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if robotModal === 'reveal' && revealedOrgToken}
	<Modal
		title="Token created — this is the only time you will see the secret"
		width="min(560px, calc(100vw - 32px))"
		showClose={false}
		dismissible={false}
	>
		<div class="flex flex-col gap-3">
			<Callout variant="warn" title="Bakery stores only a hash of this secret.">
				<span class="text-text-2">
					Every build using this token, in any project under {org.slug}, present or future,
					authenticates with it. If you lose it, revoke it and mint a new one — there is no
					recovery.</span
				>
			</Callout>
			<CodeBlock
				title={`${revealedOrgToken.name} · ${revealedOrgToken.scope}`}
				copyLabel="Copy"
				copyText={revealedOrgToken.token}>{revealedOrgToken.token}</CodeBlock
			>
			<Checkbox
				bind:checked={tokenAck}
				label="I have stored the secret. I understand it will never be shown again."
			/>
		</div>
		{#snippet footer()}
			<Button variant="primary" size="md" disabled={!tokenAck} onclick={closeRobotModal}
				>Done</Button
			>
		{/snippet}
	</Modal>
{/if}

{#if robotModal === 'revoke-token' && revokeTokenTarget}
	<Modal title={`Revoke ${revokeTokenTarget.token.name}`} onclose={closeRobotModal}>
		Every build on <span class="font-mono text-[length:var(--mono-xs)] text-text-1"
			>{revokeTokenTarget.robot.name}</span
		>
		authenticating with this token will lose access immediately — in-flight requests fail with 401.
		This cannot be undone.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeRobotModal} disabled={revokeTokenPending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" onclick={confirmRevokeToken} disabled={revokeTokenPending}>
				{revokeTokenPending ? 'Revoking…' : 'Revoke token'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if robotModal === 'delete-robot' && deleteRobotTarget}
	<Modal title={`Delete ${deleteRobotTarget.name}`} onclose={closeRobotModal}>
		Deletes the robot and cascades every token it holds — every build using any of them loses
		access immediately. This cannot be undone.
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeRobotModal} disabled={deleteRobotPending}
				>Cancel</Button
			>
			<Button variant="danger" size="md" onclick={confirmDeleteRobot} disabled={deleteRobotPending}>
				{deleteRobotPending ? 'Deleting…' : 'Delete robot'}
			</Button>
		{/snippet}
	</Modal>
{/if}
