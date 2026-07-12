<script lang="ts">
	import { Button } from '$lib/components/buttons';
	import { Field, Input, Select } from '$lib/components/inputs';
	import { Modal } from '$lib/components/feedback';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	interface Member {
		name: string;
		email: string;
		orgRole: string;
		projRole: string;
		lastActive: string;
	}

	let members = $state<Member[]>([
		{ name: 'Anna Keller', email: 'anna@acme.dev', orgRole: 'admin', projRole: 'maintainer', lastActive: '2 min ago' },
		{ name: 'Marko Ilic', email: 'marko@acme.dev', orgRole: 'member', projRole: 'writer', lastActive: '1 h ago' },
		{ name: 'Deploy Bot', email: 'deploy@acme.dev', orgRole: 'member', projRole: 'writer', lastActive: '3 min ago' },
		{ name: 'Sofia Braun', email: 'sofia@acme.dev', orgRole: 'member', projRole: 'reader', lastActive: 'yesterday' },
		{ name: 'Jens Vogel', email: 'jens@acme.dev', orgRole: 'admin', projRole: 'none', lastActive: '4 days ago' }
	]);

	const footnote =
		'Org role: admins create projects, manage members, and change settings. Project role (for the project selected in the nav): maintainer manages backends and keys · writer mints write keys · reader is read-only. Org admins implicitly have maintainer everywhere.';

	let showInvite = $state(false);
	let inviteEmail = $state('');
	let inviteOrgRole = $state('member');
	let inviteProjRole = $state('reader');

	function openInvite() {
		showInvite = true;
	}
	function closeInvite() {
		showInvite = false;
	}
</script>

<div class="flex w-full max-w-[980px] flex-col gap-[14px]">
	<div class="flex items-center justify-between">
		<div>
			<h1 class="mb-0.5 text-lg font-semibold text-text-1">Members</h1>
			<div class="text-sm text-text-2">
				Roles apply at the org level and per project. Org admins manage everything.
			</div>
		</div>
		<Button variant="primary" size="md" onclick={openInvite}>Invite member</Button>
	</div>

	<TableWrap>
		<TableRoot dense>
			<thead>
				<tr>
					<Th>Member</Th>
					<Th>Email</Th>
					<Th class="w-[132px]">Org role</Th>
					<Th class="w-[148px]">Role on firmware</Th>
					<Th>Last active</Th>
					<Th class="w-[72px]"></Th>
				</tr>
			</thead>
			<tbody>
				{#each members as m (m.email)}
					<Tr>
						<Td>
							<span class="inline-flex items-center gap-2">
								<span
									class="inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[9px] font-semibold text-accent-text"
									>{m.name[0]}</span
								>
								<span class="font-medium">{m.name}</span>
							</span>
						</Td>
						<Td mono>{m.email}</Td>
						<Td>
							<div class="w-[116px]">
								<Select size="sm" bind:value={m.orgRole}>
									<option value="admin">admin</option>
									<option value="member">member</option>
								</Select>
							</div>
						</Td>
						<Td>
							<div class="w-[132px]">
								<Select size="sm" bind:value={m.projRole}>
									<option value="maintainer">maintainer</option>
									<option value="writer">writer</option>
									<option value="reader">reader</option>
									<option value="none">no access</option>
								</Select>
							</div>
						</Td>
						<Td class="whitespace-nowrap text-text-2">{m.lastActive}</Td>
						<Td class="text-right">
							<Button variant="ghost" size="sm" class="text-err!">Remove</Button>
						</Td>
					</Tr>
				{/each}
			</tbody>
		</TableRoot>
	</TableWrap>

	<div class="text-xs text-text-3">{footnote}</div>
</div>

{#if showInvite}
	<Modal title="Invite member" onclose={closeInvite}>
		<div class="flex flex-col gap-[14px]">
			<Field
				label="Email"
				hint="Must match an identity from your OIDC provider — there are no local passwords."
			>
				<Input mono placeholder="name@acme.dev" bind:value={inviteEmail} />
			</Field>
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Org role">
					<Select size="md" bind:value={inviteOrgRole}>
						<option>member</option>
						<option>admin</option>
					</Select>
				</Field>
				<Field label="Role on firmware">
					<Select size="md" bind:value={inviteProjRole}>
						<option>reader</option>
						<option>writer</option>
						<option>maintainer</option>
					</Select>
				</Field>
			</div>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeInvite}>Cancel</Button>
			<Button variant="primary" size="md" onclick={closeInvite}>Send invite</Button>
		{/snippet}
	</Modal>
{/if}
