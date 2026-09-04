<script lang="ts">
	import { backendStatus } from '$lib/backendStatus';
	import { backendEndpoints, quotaApplicable } from '$lib/backendConfig';
	import {
		formatBytes,
		formatCount,
		formatDateTimeUTC,
		formatQuota,
		formatRetentionWindow
	} from '$lib/format';
	import { projectPath } from '$lib/tenancy';
	import { BACKEND_KINDS, type BackendKind, type ProjectBackendUsage } from '$lib/api/types';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { EmptyState } from '$lib/components/feedback';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const backends = $derived(data.backends);
	const usage = $derived(data.usage);

	const base = $derived(projectPath(org.slug, project.slug));
	const newHref = $derived(`${base}/backends/new`);

	const usageByKind = $derived(
		new Map<BackendKind, ProjectBackendUsage>(usage.map((u) => [u.kind, u]))
	);

	// There is exactly one mount per kind, so "every kind is configured" is the
	// only state in which `backends/new` has nothing left to offer. Derived from
	// the shared BACKEND_KINDS list rather than a literal 6: the overview said
	// "of 5 kinds" for a month after `registry` landed.
	const configured = $derived(new Set(backends.map((b) => b.kind)));
	const allConfigured = $derived(BACKEND_KINDS.every((k) => configured.has(k)));

	const rows = $derived(
		backends.map((b) => {
			const u = usageByKind.get(b.kind) ?? null;
			const endpoints = backendEndpoints(b.kind, org.slug, project.slug);

			return {
				backend: b,
				usage: u,
				status: backendStatus({ kind: b.kind, enabled: b.enabled, usage: u }),
				hasQuota: quotaApplicable(b.kind),
				// A kind can serve two route families (bazel HTTP+gRPC, both OCI
				// kinds containerd+BuildKit). The cell shows the first and the
				// title carries every one -- a 32px row cannot stack two without
				// breaking the table rhythm, and the detail page lists them all.
				endpoint: endpoints[0]?.value ?? '',
				endpointTitle: endpoints.map((e) => `${e.label}: ${e.value}`).join('\n')
			};
		})
	);
</script>

<div class="flex flex-wrap items-center justify-between gap-3">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Backends</h1>
		<div class="text-sm text-text-2">{project.org_slug}/{project.slug}</div>
	</div>
	{#if backends.length > 0}
		<div class="flex flex-col items-end gap-1">
			<Button href={newHref} variant="primary" size="md" disabled={allConfigured}>
				Add backend
			</Button>
			{#if allConfigured}
				<span class="text-xs text-text-3">All {BACKEND_KINDS.length} kinds configured</span>
			{/if}
		</div>
	{/if}
</div>

<!-- The loading third of the triad is SvelteKit's, not this component's: both
     reads are awaited in `+page.ts`, so the page never paints with a partial
     list. Same arrangement as the overview, whose two states these mirror. -->
{#if backends.length === 0}
	<EmptyState
		glyph="∅"
		title="No backends configured"
		desc="This project has no cache backends yet — nothing here mounts until one is added."
	>
		{#snippet action()}
			<Button href={newHref} variant="primary" size="md">Add a backend</Button>
		{/snippet}
	</EmptyState>
{:else}
	<TableWrap>
		<TableRoot dense>
			<thead>
				<tr>
					<Th>Kind</Th>
					<Th>Status</Th>
					<Th num>Objects</Th>
					<Th num>Size</Th>
					<Th>Retention</Th>
					<Th>Quota</Th>
					<Th>Measured</Th>
					<Th>Endpoint</Th>
				</tr>
			</thead>
			<tbody>
				{#each rows as row (row.backend.kind)}
					<Tr>
						<Td>
							<a
								href="{base}/backends/{row.backend.kind}"
								class="font-mono text-base text-text-1 hover:text-accent-text hover:underline"
								>{row.backend.kind}</a
							>
						</Td>
						<!-- `backendStatus`'s caption ("not yet measured") is deliberately
						     NOT rendered beside the badge here, unlike the overview's
						     table: it exists so a `0 B` cell cannot be read as a
						     measurement that happened, and this table answers that in
						     its own columns instead -- an unmeasured row is `—` objects,
						     `—` size, `never` measured, with no zero anywhere to
						     misread. -->
						<Td><Badge status={row.status.status}>{row.status.label}</Badge></Td>
						<Td num class="tabular">{formatCount(row.usage?.objects_count)}</Td>
						<Td num class="tabular">{formatBytes(row.usage?.logical_bytes)}</Td>
						<Td class="whitespace-nowrap text-text-2"
							>{formatRetentionWindow(row.backend.retention_window)}</Td
						>
						<Td class="whitespace-nowrap text-text-2"
							>{row.hasQuota ? formatQuota(row.backend.quota_bytes) : 'n/a'}</Td
						>
						<Td class="whitespace-nowrap text-text-2"
							>{row.usage?.measured_at ? formatDateTimeUTC(row.usage.measured_at) : 'never'}</Td
						>
						<Td mono>
							<span class="block max-w-[36ch] truncate" title={row.endpointTitle}
								>{row.endpoint}</span
							>
						</Td>
					</Tr>
				{/each}
			</tbody>
		</TableRoot>
	</TableWrap>
{/if}
