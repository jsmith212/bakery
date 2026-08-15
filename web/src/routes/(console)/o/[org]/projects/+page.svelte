<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import { createProject } from '$lib/api/projects';
	import { isApiError } from '$lib/api/errors';
	import type { GCActivityRun, OrgProjectUsage } from '$lib/api/types';
	import { canAdminOrg } from '$lib/roles';
	import { gcStatusBadge } from '$lib/backendStatus';
	import { resolveLoadState } from '$lib/loadState';
	import { projectPath } from '$lib/tenancy';
	import { formatBytes, formatCount, formatDateTimeUTC } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { EmptyState, Skeleton, Modal } from '$lib/components/feedback';
	import { Field, Input } from '$lib/components/inputs';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const projects = $derived(data.projects);
	const canCreate = $derived(canAdminOrg(data.me, org));

	let usageBySlug = $state<Map<string, OrgProjectUsage>>(new Map());
	let usageReady = $state(false);

	$effect(() => {
		usageReady = false;
		data.usage
			.then((res) => {
				usageBySlug = new Map(res.items.map((u) => [u.project_slug, u]));
				usageReady = true;
			})
			.catch((err) => {
				toastError(err, 'Could not load project storage usage');
				usageBySlug = new Map();
				usageReady = true;
			});
	});

	// One mutually-exclusive decision (`resolveLoadState`), not three
	// independently-derived booleans -- see its doc comment for the bug this
	// replaced (an empty org rendered both the skeleton AND the empty state
	// until usage resolved).
	const screenState = $derived(resolveLoadState(usageReady, projects.length === 0));
	const isEmpty = $derived(screenState === 'empty');
	const isLoading = $derived(screenState === 'loading');
	const isLoaded = $derived(screenState === 'loaded');

	const totalBytes = $derived(
		[...usageBySlug.values()].reduce((sum, u) => sum + u.logical_bytes, 0)
	);

	// B7. The org's own slice of recent GC sweeps. Loaded independently of the
	// project grid above -- a slow activity join must never hold up the page,
	// and vice versa.
	let activityRuns = $state<GCActivityRun[]>([]);
	let activityReady = $state(false);

	$effect(() => {
		activityReady = false;
		data.activity
			.then((res) => {
				activityRuns = res.items;
				activityReady = true;
			})
			.catch((err) => {
				toastError(err, 'Could not load retention activity');
				activityRuns = [];
				activityReady = true;
			});
	});

	// One row per (run, backend) pair -- a run with three swept backends is
	// three rows, each naming its own project and kind. A run with zero
	// backend rows (nothing configured yet swept it) has nothing to flatten.
	const activityRows = $derived(
		activityRuns.flatMap((run) =>
			run.backends.map((b) => ({
				runId: run.run_id,
				startedAt: run.started_at,
				status: run.status,
				...b
			}))
		)
	);

	const subtitle = $derived(
		isEmpty
			? '0 projects'
			: `${projects.length} project${projects.length === 1 ? '' : 's'}` +
				(usageReady ? ` · ${formatBytes(totalBytes)} stored` : '')
	);

	const skeletons = [1, 2, 3, 4, 5, 6];

	let showCreate = $state(false);
	let draftSlug = $state('');
	let draftName = $state('');
	let createPending = $state(false);
	let slugError = $state<string | null>(null);
	let nameError = $state<string | null>(null);

	function openCreate() {
		showCreate = true;
		draftSlug = '';
		draftName = '';
		slugError = null;
		nameError = null;
	}

	function closeCreate() {
		if (createPending) return;
		showCreate = false;
	}

	async function submitCreate() {
		if (createPending) return;
		createPending = true;
		slugError = null;
		nameError = null;

		try {
			const created = await createProject(org.slug, { slug: draftSlug, name: draftName });
			showCreate = false;
			pushToast({ variant: 'success', title: `Created ${created.slug}` });
			await invalidateAll();
		} catch (err) {
			if (isApiError(err) && err.field === 'slug') {
				slugError = err.message;
			} else if (isApiError(err) && err.field === 'name') {
				nameError = err.message;
			} else {
				toastError(err, 'Could not create project');
			}
		} finally {
			createPending = false;
		}
	}
</script>

<div class="flex items-center justify-between">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Projects</h1>
		<div class="text-sm text-text-2">{subtitle}</div>
	</div>
	{#if canCreate}
		<Button variant="primary" size="md" onclick={openCreate}>New project</Button>
	{/if}
</div>

{#if isLoading}
	<div class="grid grid-cols-3 gap-2.5">
		{#each skeletons as s (s)}
			<div class="flex flex-col gap-2.5 rounded-2 border border-border-0 bg-bg-1 p-[14px]">
				<Skeleton width="44%" height={14} />
				<Skeleton width="70%" height={12} />
				<Skeleton width="100%" height={34} />
			</div>
		{/each}
	</div>
{/if}

{#snippet createFirstProjectAction()}
	<Button variant="primary" size="md" onclick={openCreate}>Create your first project</Button>
{/snippet}

{#if isEmpty}
	<EmptyState
		glyph="∅"
		title="No projects in {org.slug} yet"
		desc="A project holds cache backends, API keys, and members. Create one, add a backend, and point your first build at it — the config snippet generator gives you the exact client config."
		action={canCreate ? createFirstProjectAction : undefined}
	/>
{/if}

{#if isLoaded}
	<div class="grid grid-cols-3 gap-2.5">
		{#each projects as p (p.slug)}
			{@const usage = usageBySlug.get(p.slug)}
			<a href="{projectPath(org.slug, p.slug)}/overview" class="block no-underline">
				<div
					class="flex flex-col gap-2.5 rounded-2 border border-border-0 bg-bg-1 p-[14px] transition-[border-color] duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2"
				>
					<div class="flex items-center gap-2">
						<span class="font-mono text-base font-medium text-text-1">{p.slug}</span>
					</div>
					<div class="flex flex-wrap gap-1">
						{#each p.backends as b (b)}
							<Badge variant="type">{b}</Badge>
						{:else}
							<span class="text-xs text-text-3">no backends configured</span>
						{/each}
					</div>
					<div class="flex gap-4 text-xs text-text-2">
						<span
							><span class="tabular font-medium text-text-1">{formatCount(usage?.objects_count)}</span> objects</span
						>
						<span
							><span class="tabular font-medium text-text-1">{formatBytes(usage?.logical_bytes)}</span> stored</span
						>
					</div>
					<div class="text-xs text-text-3">
						{#if usage?.measured_at}
							measured {formatDateTimeUTC(usage.measured_at)}
						{:else}
							not yet measured
						{/if}
					</div>
				</div>
			</a>
		{/each}
	</div>
{/if}

<div class="rounded-2 border border-border-0 bg-bg-1 p-[14px]">
	<div class="mb-2.5 flex items-center justify-between">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Retention activity
		</div>
		<div class="text-xs text-text-3">Garbage collection is instance-wide and interval-driven.</div>
	</div>

	{#if !activityReady}
		<div class="flex flex-col gap-1">
			{#each [1, 2, 3] as s (s)}
				<Skeleton height={24} />
			{/each}
		</div>
	{:else if activityRows.length === 0}
		<EmptyState
			glyph="∅"
			title="No sweeps have touched this org yet"
			desc="Garbage collection runs on the instance's interval, or a site admin can trigger one from the /gc screen. A row here means a run actually swept one of this org's backends — even a 0/0 row means it looked and found nothing eligible."
		/>
	{:else}
		<TableWrap>
			<TableRoot dense>
				<thead>
					<tr>
						<Th>Run</Th>
						<Th>Started</Th>
						<Th>Status</Th>
						<Th>Project</Th>
						<Th>Kind</Th>
						<Th num>Objects deleted</Th>
						<Th num>Bytes freed</Th>
					</tr>
				</thead>
				<tbody>
					{#each activityRows as row (row.runId + ':' + row.project_slug + ':' + row.kind)}
						<Tr>
							<Td mono>#{row.runId}</Td>
							<Td mono class="whitespace-nowrap">{formatDateTimeUTC(row.startedAt)}</Td>
							<Td>
								<Badge status={gcStatusBadge(row.status)}>{row.status}</Badge>
							</Td>
							<Td class="font-medium">{row.project_slug}</Td>
							<Td><Badge variant="type">{row.kind}</Badge></Td>
							<Td num class="tabular">{formatCount(row.objects_deleted)}</Td>
							<Td num class="tabular">{formatBytes(row.bytes_freed)}</Td>
						</Tr>
					{/each}
				</tbody>
			</TableRoot>
		</TableWrap>
	{/if}
</div>

{#if showCreate}
	<Modal title="New project" onclose={closeCreate}>
		<div class="flex flex-col gap-[14px]">
			<Field
				label="Slug"
				hint="Lower-case, used in every cache URL under this project. Cannot be changed later."
				error={slugError ?? undefined}
			>
				{#snippet children(f)}
					<Input mono placeholder="my-project" bind:value={draftSlug} error={!!slugError} {...f} />
				{/snippet}
			</Field>
			<Field label="Name" error={nameError ?? undefined}>
				{#snippet children(f)}
					<Input placeholder="My project" bind:value={draftName} error={!!nameError} {...f} />
				{/snippet}
			</Field>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeCreate} disabled={createPending}
				>Cancel</Button
			>
			<Button variant="primary" size="md" onclick={submitCreate} disabled={createPending}>
				{createPending ? 'Creating…' : 'Create project'}
			</Button>
		{/snippet}
	</Modal>
{/if}
