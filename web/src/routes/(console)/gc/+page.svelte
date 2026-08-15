<script lang="ts">
	import { untrack } from 'svelte';

	import { goto, invalidateAll } from '$app/navigation';
	import { page } from '$app/state';

	import { getGCRun, listGCRuns, triggerGCRun } from '$lib/api/gc';
	import { isApiError } from '$lib/api/errors';
	import type { GCRun } from '$lib/api/types';
	import { gcStatusBadge } from '$lib/backendStatus';
	import { formatBytes, formatCount, formatDateTimeUTC, formatDuration } from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Toggle } from '$lib/components/inputs';
	import { Modal, EmptyState, Callout } from '$lib/components/feedback';
	import { KeyValueList } from '$lib/components/content';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const instance = $derived(data.instance);

	// Seeded from `load`'s first page; "Load more" appends client-side, same
	// keyset idiom as the B3 object browser on `backends/[type]`. Only the
	// mount-time value is wanted here -- the effect below follows every
	// subsequent `data` change (a navigation re-running `load`).
	let runs = $state<GCRun[]>(untrack(() => data.runs));
	let cursor = $state<number | null>(untrack(() => data.nextCursor));
	let loadingMore = $state(false);

	$effect(() => {
		runs = data.runs;
		cursor = data.nextCursor;
	});

	async function loadMore() {
		if (cursor === null || loadingMore) return;
		loadingMore = true;
		try {
			const res = await listGCRuns({
				include_usage: data.includeUsage,
				before: cursor,
				limit: 30
			});
			runs = [...runs, ...res.items];
			cursor = res.next_cursor;
		} catch (err) {
			toastError(err, 'Could not load more runs');
		} finally {
			loadingMore = false;
		}
	}

	function toggleIncludeUsage(checked: boolean) {
		const url = new URL(page.url);
		if (checked) url.searchParams.set('include_usage', '1');
		else url.searchParams.delete('include_usage');
		goto(url, { invalidateAll: true });
	}

	let detail = $state<GCRun | null>(null);
	let detailRefreshing = $state(false);

	function openDetail(run: GCRun) {
		detail = run;
	}
	function closeDetail() {
		detail = null;
	}

	async function refreshDetail() {
		if (!detail || detailRefreshing) return;
		detailRefreshing = true;
		try {
			detail = await getGCRun(detail.id);
			runs = runs.map((r) => (r.id === detail!.id ? detail! : r));
		} catch (err) {
			toastError(err, 'Could not refresh run status');
		} finally {
			detailRefreshing = false;
		}
	}

	let showTrigger = $state(false);
	let dryRun = $state(true);
	let triggerPending = $state(false);

	function openTrigger() {
		showTrigger = true;
		dryRun = true;
	}
	function closeTrigger() {
		if (triggerPending) return;
		showTrigger = false;
	}

	async function confirmTrigger() {
		if (triggerPending) return;
		triggerPending = true;
		try {
			// `dry_run` is sent EXPLICITLY, always -- its zero value is a real
			// sweep, and an omitted field is not the safer reading.
			const res = await triggerGCRun({ dry_run: dryRun });
			pushToast({
				variant: 'success',
				title: `${dryRun ? 'Dry-run' : 'Sweep'} #${res.id} ${res.status}`
			});
			showTrigger = false;
			await invalidateAll();
			try {
				detail = await getGCRun(res.id);
			} catch {
				// The run itself started fine; a failed follow-up read is not worth
				// surfacing a second error for.
			}
		} catch (err) {
			toastError(err, 'Could not start a GC run');
		} finally {
			triggerPending = false;
		}
	}

	const instancePairs = $derived([
		{ key: 'Version', value: instance.version, mono: true },
		{ key: 'Storage driver', value: instance.storage_driver, mono: true },
		{ key: 'Public listener', value: instance.public_addr, mono: true },
		{ key: 'gRPC listener', value: instance.grpc_addr || 'disabled', mono: true },
		{ key: 'Metrics listener', value: instance.metrics_addr, mono: true },
		{ key: 'External URL', value: instance.external_url || '(none set)', mono: true },
		{
			key: 'gRPC external endpoint',
			value: instance.grpc_external_endpoint || '(derived per request)',
			mono: true
		},
		{ key: 'OIDC issuer', value: instance.oidc_issuer || '(none)', mono: true },
		{ key: 'Dev login', value: instance.dev_login_enabled ? 'enabled' : 'disabled' },
		{ key: 'Self-serve orgs', value: instance.allow_self_serve_orgs ? 'allowed' : 'restricted' },
		{
			key: 'Local site admins',
			value: instance.allow_local_site_admins ? 'allowed' : 'restricted'
		},
		{ key: 'GC', value: instance.gc_enabled ? 'enabled' : 'disabled' },
		{ key: 'GC interval', value: formatDuration(instance.gc_interval) },
		{ key: 'GC usage interval', value: formatDuration(instance.gc_usage_interval) },
		{ key: 'GC grace period', value: formatDuration(instance.gc_grace_period) }
	]);
</script>

<div class="flex items-center justify-between">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Garbage collection</h1>
		<div class="text-sm text-text-2">Instance-wide. Site admin only.</div>
	</div>
	<Button variant="primary" size="md" disabled={!instance.gc_enabled} onclick={openTrigger}
		>Run GC now</Button
	>
</div>

{#if !instance.gc_enabled}
	<Callout variant="warn">
		GC is disabled on this instance (<span class="font-mono text-xs">--gc-enabled=false</span
		>). Scheduled sweeps and manual runs are both refused.
	</Callout>
{/if}

<div class="rounded-2 border border-border-0 bg-bg-1 p-[14px]">
	<div class="mb-2.5 text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
		Instance
	</div>
	<KeyValueList pairs={instancePairs} />
</div>

<div class="rounded-2 border border-border-0 bg-bg-1">
	<div class="flex items-center justify-between px-[14px] py-2.5">
		<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
			Runs
		</div>
		<Toggle checked={data.includeUsage} onchange={toggleIncludeUsage} label="Show usage-only runs" />
	</div>

	{#if runs.length === 0}
		<div class="p-[14px]">
			<EmptyState
				glyph="∅"
				title={data.includeUsage ? 'No runs yet' : 'No sweeps yet'}
				desc={data.includeUsage
					? 'Nothing has run on this instance yet — a sweep runs on the configured interval, or trigger one above.'
					: 'No real sweeps yet — usage-only runs are hidden by default since they mint a row every few hours. Toggle "Show usage-only runs" to see everything.'}
			/>
		</div>
	{:else}
		<TableWrap>
			<TableRoot dense>
				<thead>
					<tr>
						<Th>Run</Th>
						<Th>Trigger</Th>
						<Th>Status</Th>
						<Th>Started</Th>
						<Th num>Objects deleted</Th>
						<Th num>Bytes reclaimed</Th>
						<Th class="w-[64px]"></Th>
					</tr>
				</thead>
				<tbody>
					{#each runs as r (r.id)}
						<Tr>
							<Td mono>#{r.id}</Td>
							<Td class="text-text-2">{r.trigger}{r.dry_run ? ' (dry run)' : ''}</Td>
							<Td><Badge status={gcStatusBadge(r.status)}>{r.status}</Badge></Td>
							<Td mono class="whitespace-nowrap">{formatDateTimeUTC(r.started_at)}</Td>
							<Td num class="tabular">{formatCount(r.objects_deleted)}</Td>
							<Td num class="tabular">{formatBytes(r.bytes_reclaimed)}</Td>
							<Td class="text-right">
								<Button variant="ghost" size="sm" onclick={() => openDetail(r)}>View</Button>
							</Td>
						</Tr>
					{/each}
				</tbody>
			</TableRoot>
		</TableWrap>
		{#if cursor !== null}
			<div class="flex justify-center border-t border-border-0 py-2">
				<Button variant="secondary" size="sm" disabled={loadingMore} onclick={loadMore}>
					{loadingMore ? 'Loading…' : 'Load more'}
				</Button>
			</div>
		{/if}
	{/if}
</div>

{#if showTrigger}
	<Modal title="Run garbage collection" onclose={closeTrigger}>
		<div class="flex flex-col gap-3">
			<div class="text-sm text-text-2">
				A real sweep deletes hashserv rows before sstate objects, and unreferenced blobs after
				that — reachable-from-unihash objects are spared, and a concurrent build's fresh writes are
				spared by the write barrier. This can affect every org on the instance.
			</div>
			<Toggle bind:checked={dryRun} label="Dry run — count what would be deleted, delete nothing" />
			{#if !dryRun}
				<div class="rounded-1 border border-err-border bg-err-muted px-2.5 py-2 text-sm text-err">
					This will actually delete data across every org on this instance.
				</div>
			{/if}
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeTrigger} disabled={triggerPending}
				>Cancel</Button
			>
			<Button
				variant={dryRun ? 'primary' : 'danger'}
				size="md"
				onclick={confirmTrigger}
				disabled={triggerPending}
			>
				{triggerPending ? 'Starting…' : dryRun ? 'Run dry run' : 'Run sweep'}
			</Button>
		{/snippet}
	</Modal>
{/if}

{#if detail}
	{@const d = detail}
	<Modal title="Run #{d.id}" onclose={closeDetail}>
		<div class="flex flex-col gap-3">
			<KeyValueList
				pairs={[
					{ key: 'Status', value: d.status },
					{ key: 'Trigger', value: d.trigger },
					{ key: 'Dry run', value: d.dry_run ? 'yes' : 'no' },
					{ key: 'Started', value: formatDateTimeUTC(d.started_at), mono: true },
					{
						key: 'Finished',
						value: d.finished_at ? formatDateTimeUTC(d.finished_at) : 'still running',
						mono: true
					},
					{ key: 'Hashserv rows deleted', value: formatCount(d.hashserv_rows_deleted) },
					{ key: 'Objects deleted', value: formatCount(d.objects_deleted) },
					{ key: 'Blobs marked', value: formatCount(d.blobs_marked) },
					{ key: 'Blobs deleted', value: formatCount(d.blobs_deleted) },
					{ key: 'Bytes reclaimed', value: formatBytes(d.bytes_reclaimed) },
					...(d.error ? [{ key: 'Error', value: d.error }] : [])
				]}
			/>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeDetail}>Close</Button>
			{#if d.status === 'running'}
				<Button variant="secondary" size="md" onclick={refreshDetail} disabled={detailRefreshing}>
					{detailRefreshing ? 'Refreshing…' : 'Refresh'}
				</Button>
			{/if}
		{/snippet}
	</Modal>
{/if}
