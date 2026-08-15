<script lang="ts">
	import { goto } from '$app/navigation';

	import { backendStatus, isUnmeasured } from '$lib/backendStatus';
	import { formatBytes, formatCount, formatDateTimeUTC, formatQuota, formatRetentionWindow } from '$lib/format';
	import { projectPath } from '$lib/tenancy';
	import type { BackendKind, ProjectBackendUsage } from '$lib/api/types';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { EmptyState } from '$lib/components/feedback';
	import { KeyValueList } from '$lib/components/content';
	import { StatTile } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const backends = $derived(data.backends);
	const usage = $derived(data.usage);

	const usageByKind = $derived(new Map<BackendKind, ProjectBackendUsage>(usage.map((u) => [u.kind, u])));

	const anyMeasured = $derived(usage.some((u) => u.measured_at !== null));
	const totalObjects = $derived(
		anyMeasured ? usage.reduce((sum, u) => sum + (u.objects_count ?? 0), 0) : null
	);
	const totalBytes = $derived(
		anyMeasured ? usage.reduce((sum, u) => sum + (u.logical_bytes ?? 0), 0) : null
	);

	// Headroom is only meaningful across backends that actually carry a quota.
	// An uncapped backend does not shrink the aggregate -- it just has nothing
	// to say about it, so it is excluded rather than treated as zero headroom.
	const cappedUsage = $derived(usage.filter((u) => u.quota_bytes !== null));
	// An UNMEASURED capped backend must also be excluded from the sum, not
	// treated as 0 bytes used: `isUnmeasured` is the same "not yet sampled"
	// check `backendStatus` uses, and counting it as empty overstates headroom
	// until the next `--gc-usage-interval` pass actually looks.
	const measuredCappedUsage = $derived(cappedUsage.filter((u) => !isUnmeasured(u)));
	const unmeasuredCappedCount = $derived(cappedUsage.length - measuredCappedUsage.length);
	const headroomBytes = $derived(
		measuredCappedUsage.reduce((sum, u) => sum + (u.quota_bytes ?? 0) - (u.logical_bytes ?? 0), 0)
	);
	const headroomCaption = $derived.by(() => {
		if (cappedUsage.length === 0) return 'no backend has a quota set';
		if (measuredCappedUsage.length === 0) {
			return `${cappedUsage.length} capped backend${cappedUsage.length === 1 ? '' : 's'} not yet measured`;
		}

		const base = `across ${measuredCappedUsage.length} capped backend${measuredCappedUsage.length === 1 ? '' : 's'}`;

		return unmeasuredCappedCount > 0
			? `${base} · ${unmeasuredCappedCount} more not yet measured, excluded`
			: base;
	});

	const rows = $derived(
		backends.map((b) => ({
			backend: b,
			usage: usageByKind.get(b.kind) ?? null,
			status: backendStatus({ kind: b.kind, enabled: b.enabled, usage: usageByKind.get(b.kind) })
		}))
	);
</script>

<div class="flex items-center justify-between">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Overview</h1>
		<div class="text-sm text-text-2">{project.org_slug}/{project.slug}</div>
	</div>
	<Button
		variant="primary"
		size="md"
		onclick={() => goto(`${projectPath(org.slug, project.slug)}/snippets`)}
		>Get config snippet</Button
	>
</div>

<div class="grid grid-cols-4 gap-2">
	<StatTile
		label="Objects"
		value={formatCount(totalObjects)}
		caption={anyMeasured ? undefined : 'not yet measured'}
	/>
	<StatTile
		label="Stored"
		value={formatBytes(totalBytes)}
		caption={anyMeasured ? undefined : 'not yet measured'}
	/>
	<StatTile
		label="Quota headroom"
		value={measuredCappedUsage.length > 0
			? formatBytes(headroomBytes)
			: cappedUsage.length > 0
				? 'not yet measured'
				: 'no cap'}
		caption={headroomCaption}
	/>
	<StatTile label="Backends configured" value={backends.length} unit="of 5 kinds" />
</div>

<div class="rounded-2 border border-border-0 bg-bg-1">
	<div class="px-[14px] pt-3 pb-2 text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
		Backends
	</div>
	{#if rows.length === 0}
		<div class="p-[14px]">
			<EmptyState
				glyph="∅"
				title="No backends configured"
				desc="This project has no cache backends yet — nothing here mounts until one is added."
			>
				{#snippet action()}
					<Button
						variant="primary"
						size="md"
						onclick={() => goto(`${projectPath(org.slug, project.slug)}/backends/new`)}
						>Add a backend</Button
					>
				{/snippet}
			</EmptyState>
		</div>
	{:else}
		<TableWrap>
			<TableRoot dense>
				<thead>
					<tr>
						<Th>Kind</Th>
						<Th>Status</Th>
						<Th num>Objects</Th>
						<Th num>Stored</Th>
						<Th>Measured</Th>
					</tr>
				</thead>
				<tbody>
					{#each rows as row (row.backend.kind)}
						<Tr>
							<Td>
								<a
									href={`${projectPath(org.slug, project.slug)}/backends/${row.backend.kind}`}
									class="font-mono text-base text-text-1 hover:text-accent-text hover:underline"
									>{row.backend.kind}</a
								>
							</Td>
							<Td>
								<div class="flex items-center gap-1.5">
									<Badge status={row.status.status}>{row.status.label}</Badge>
									{#if row.status.caption}
										<span class="text-xs text-text-3">{row.status.caption}</span>
									{/if}
								</div>
							</Td>
							<Td num class="tabular">{formatCount(row.usage?.objects_count)}</Td>
							<Td num class="tabular">{formatBytes(row.usage?.logical_bytes)}</Td>
							<Td class="whitespace-nowrap text-text-2"
								>{row.usage?.measured_at ? formatDateTimeUTC(row.usage.measured_at) : 'never'}</Td
							>
						</Tr>
					{/each}
				</tbody>
			</TableRoot>
		</TableWrap>
	{/if}
</div>

<div class="rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3">
	<div class="mb-2.5 text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
		Retention
	</div>
	{#if rows.length === 0}
		<div class="text-sm text-text-3">No backends to show retention for.</div>
	{:else}
		<KeyValueList
			pairs={rows.map((row) => ({
				key: row.backend.kind,
				value: `retain ${formatRetentionWindow(row.backend.retention_window)} · cap ${formatQuota(row.backend.quota_bytes)}`,
				mono: false
			}))}
		/>
		<p class="mt-2 text-xs text-text-3">
			Read-only here — edit a backend's retention or quota on its own detail page.
		</p>
	{/if}
</div>
