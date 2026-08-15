<script lang="ts">
	import { untrack } from 'svelte';
	import { invalidateAll } from '$app/navigation';

	import { updateBackend } from '$lib/api/backends';
	import { listCacheObjects } from '$lib/api/objects';
	import { isApiError } from '$lib/api/errors';
	import { tri } from '$lib/api/patch';
	import type { BackendKind, CacheObject } from '$lib/api/types';
	import { canAdminProject } from '$lib/roles';
	import { backendStatus } from '$lib/backendStatus';
	import {
		backendEndpoints,
		buildOciConfig,
		parseHashservConfig,
		parseOciConfig
	} from '$lib/backendConfig';
	import {
		formatApproxAccessed,
		formatBytes,
		formatCount,
		formatDateTimeUTC,
		formatQuota,
		formatRetentionWindow,
		parseHumanBytes
	} from '$lib/format';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { EmptyState, Modal, Skeleton } from '$lib/components/feedback';
	import { KeyValueList } from '$lib/components/content';
	import { Field, Input, Toggle, Checkbox } from '$lib/components/inputs';
	import { Tabs } from '$lib/components/navigation';
	import { StatTile } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const kind = $derived(data.kind);
	const backend = $derived(data.backend);
	const usageRow = $derived(data.usage.find((u) => u.kind === kind) ?? null);

	const status = $derived(backendStatus({ kind, enabled: backend.enabled, usage: usageRow }));
	const canEdit = $derived(canAdminProject(data.me, project));

	// yocto composes sstate/downloads/hashserv into one snippet; bazel-family
	// tools (moon/ccache/sccache/bazel) share the bazel backend; the OCI tools
	// share one backend too -- containerd is the representative tile.
	const snippetTool = $derived(kind === 'bazel' ? 'bazel' : kind === 'oci' ? 'containerd' : 'yocto');

	// A backend cannot have a quota if it structurally can never be reached
	// (hashserv stores no cache objects) or is bounded a different way (oci by
	// its retention window) -- internal/api/backends.go's backendQuotaPatch.
	const quotaApplicable = $derived(kind !== 'hashserv' && kind !== 'oci');

	const protocolFacts: Record<BackendKind, { label: string; value: string }[]> = {
		sstate: [{ label: 'Verify key = sha256', value: 'no (sstate keys are unihashes)' }],
		downloads: [
			{ label: 'Layout', value: 'flat basenames' },
			{ label: 'Note', value: 'clients verify SRC_URI checksums themselves' }
		],
		hashserv: [
			{ label: 'Auth', value: 'upgrade always succeeds; denied in-band on the first privileged RPC' },
			{ label: 'Unconfigured/disabled', value: '404 at upgrade — nothing to serve' },
			{ label: 'GC root', value: 'unihash — swept before sstate' }
		],
		bazel: [
			{ label: '/ac', value: 'opaque · overwrite allowed · never verified' },
			{ label: '/cas', value: 'content-addressed · verify key = sha256(body)' }
		],
		oci: [
			{ label: 'Manifests', value: 'stored and served byte-exact' },
			{ label: 'Upstream credentials', value: 'set by the operator, never per-project' }
		]
	};

	const configPairs = $derived.by(() => {
		const pairs = [
			...backendEndpoints(kind, org.slug, project.slug).map((e) => ({
				key: e.label,
				value: e.value,
				mono: true
			})),
			{ key: 'Enabled', value: backend.enabled ? 'yes' : 'no' },
			{ key: 'Auth required (reads)', value: backend.read_auth_required ? 'yes' : 'no' },
			{ key: 'Retention', value: formatRetentionWindow(backend.retention_window) },
			{
				key: 'Quota',
				value: quotaApplicable ? formatQuota(backend.quota_bytes) : 'not applicable to this kind'
			}
		];

		// hashserv structurally never gets a `cache_backend_usage` row
		// (`backendStatus.ts`'s doc comment: the GC planner gives it no
		// stages), so there is nothing here to time-stamp -- everything else
		// has an honest "not yet measured" until the first usage pass.
		if (kind !== 'hashserv') {
			pairs.push({
				key: 'Measured',
				value: usageRow?.measured_at ? formatDateTimeUTC(usageRow.measured_at) : 'not yet measured'
			});
		}

		if (kind === 'oci') {
			const cfg = parseOciConfig(backend.config);
			pairs.push(
				{ key: 'Default upstream', value: cfg.defaultUpstream, mono: true },
				{
					key: 'Allowed upstreams',
					value: cfg.upstreams.length > 0 ? cfg.upstreams.join(', ') : cfg.defaultUpstream,
					mono: true
				},
				{ key: 'Tag TTL', value: cfg.tagTtl }
			);
		}

		if (kind === 'hashserv') {
			const cfg = parseHashservConfig(backend.config);
			pairs.push({ key: 'Chained upstream', value: cfg.upstream ?? 'none (direct only)', mono: !!cfg.upstream });
		}

		return [...pairs, ...protocolFacts[kind].map((f) => ({ key: f.label, value: f.value }))];
	});

	// -------------------------------------------------------------------------
	// Object browser (B3) -- hashserv has no cache_objects, structurally.
	// -------------------------------------------------------------------------

	const NAMESPACES: Record<BackendKind, string[]> = {
		sstate: [''],
		downloads: [''],
		hashserv: [],
		bazel: ['ac', 'ac-grpc', 'cas', 'sccache'],
		oci: ['tags', 'manifests', 'blobs']
	};

	function nsLabel(ns: string): string {
		return ns === '' ? 'objects' : ns;
	}

	// Only the INITIAL kind matters here -- the route param is stable for the
	// component's lifetime, a navigation to a different [type] remounts it --
	// so this deliberately reads outside reactivity with `untrack`.
	let namespace = $state(untrack(() => NAMESPACES[data.kind][0] ?? ''));
	let objects = $state<CacheObject[]>([]);
	let cursor = $state<string | null>(null);
	let objectsPending = $state(false);
	let objectsFailed = $state<string | null>(null);

	async function loadObjects(reset: boolean) {
		if (kind === 'hashserv') return;
		objectsPending = true;
		objectsFailed = null;

		try {
			const res = await listCacheObjects(org.slug, project.slug, kind, {
				namespace,
				after_key: reset ? undefined : (cursor ?? undefined),
				limit: 50
			});
			objects = reset ? res.items : [...objects, ...res.items];
			cursor = res.next_cursor;
		} catch (err) {
			objectsFailed = isApiError(err) ? err.message : 'Could not reach the server.';
		} finally {
			objectsPending = false;
		}
	}

	$effect(() => {
		void loadObjects(true);
	});

	// -------------------------------------------------------------------------
	// Edit
	// -------------------------------------------------------------------------

	let showEdit = $state(false);
	let editPending = $state(false);
	let editEnabled = $state(true);
	let editReadAuth = $state(true);
	let editRetentionForever = $state(false);
	let editRetentionValue = $state('');
	let editQuotaNoCap = $state(true);
	let editQuotaValue = $state('');
	// `formatBytes` rounds to one decimal place for display, so re-parsing an
	// UNCHANGED field with `parseHumanBytes` is lossy (e.g. a 550 GB-decimal
	// quota renders "512.2 GB" and parses back to a different byte count).
	// This is the string `openEdit` rendered it as; the submit path compares
	// against it and only re-parses when the user actually edited the field,
	// otherwise it resends the exact loaded byte count untouched.
	let editQuotaInitialValue = $state('');
	let editQuotaInitialBytes = $state<number | null>(null);
	let editOciDefaultUpstream = $state('');
	let editOciUpstreamsCsv = $state('');
	let editOciTagTtl = $state('');
	let editHashservUpstream = $state('');
	let editError = $state<string | null>(null);

	function openEdit() {
		editEnabled = backend.enabled;
		editReadAuth = backend.read_auth_required;
		editRetentionForever = backend.retention_window === null;
		editRetentionValue = backend.retention_window ?? '';
		editQuotaNoCap = backend.quota_bytes === null;
		editQuotaValue = backend.quota_bytes !== null ? formatBytes(backend.quota_bytes) : '';
		editQuotaInitialValue = editQuotaValue;
		editQuotaInitialBytes = backend.quota_bytes;

		if (kind === 'oci') {
			const cfg = parseOciConfig(backend.config);
			editOciDefaultUpstream = cfg.defaultUpstream;
			editOciUpstreamsCsv = cfg.upstreams.join(', ');
			editOciTagTtl = cfg.tagTtl;
		}
		if (kind === 'hashserv') {
			editHashservUpstream = parseHashservConfig(backend.config).upstream ?? '';
		}

		editError = null;
		showEdit = true;
	}

	function closeEdit() {
		if (editPending) return;
		showEdit = false;
	}

	async function submitEdit() {
		if (editPending) return;
		editError = null;

		let retentionWindow: string | null;
		if (editRetentionForever) {
			retentionWindow = null;
		} else if (editRetentionValue.trim() === '') {
			editError = 'Enter a retention duration like "2160h", or check "retain forever".';
			return;
		} else {
			retentionWindow = editRetentionValue.trim();
		}

		let quotaBytes: number | null = null;
		if (quotaApplicable) {
			if (!editQuotaNoCap) {
				if (editQuotaValue.trim() === '') {
					editError = 'Enter a quota like "500 GB", or check "no cap".';
					return;
				}
				if (editQuotaValue === editQuotaInitialValue) {
					// Unedited: resend the exact loaded value rather than a
					// lossy re-parse of its rounded display string.
					quotaBytes = editQuotaInitialBytes;
				} else {
					try {
						quotaBytes = parseHumanBytes(editQuotaValue);
					} catch (err) {
						editError = err instanceof Error ? err.message : 'That quota is not valid.';
						return;
					}
				}
			}
		}

		let config: unknown;
		if (kind === 'oci') {
			config = buildOciConfig({
				defaultUpstream: editOciDefaultUpstream,
				upstreamsCsv: editOciUpstreamsCsv,
				tagTtl: editOciTagTtl
			});
		} else if (kind === 'hashserv') {
			config = { upstream: editHashservUpstream.trim() };
		}

		editPending = true;

		try {
			await updateBackend(org.slug, project.slug, kind, {
				enabled: editEnabled,
				read_auth_required: editReadAuth,
				config,
				retention_window: tri(retentionWindow, 'retention_window'),
				...(quotaApplicable ? { quota_bytes: tri(quotaBytes, 'quota_bytes') } : {})
			});
			pushToast({ variant: 'success', title: `Updated ${kind}` });
			showEdit = false;
			await invalidateAll();
		} catch (err) {
			// A field error or `forbidden` renders in place, in the modal that was
			// already open; everything else (409, 5xx) is a toast.
			if (isApiError(err) && (err.field || err.code === 'forbidden')) {
				editError = err.message;
			} else {
				toastError(err, 'Could not update backend');
			}
		} finally {
			editPending = false;
		}
	}
</script>

<div class="flex items-center gap-1.5 text-sm text-text-3">
	<a href="/o/{org.slug}/p/{project.slug}/overview" class="text-accent-text hover:underline"
		>{org.slug}/{project.slug}</a
	>
	<span>/</span>
	<span class="font-mono text-text-2">{kind}</span>
</div>

<div class="flex flex-wrap items-center justify-between gap-3">
	<div class="flex items-center gap-2.5">
		<h1 class="m-0 font-mono text-lg font-semibold text-text-1">{kind}</h1>
		<Badge status={status.status}>{status.label}</Badge>
		{#if status.caption}
			<span class="text-xs text-text-3">{status.caption}</span>
		{/if}
	</div>
	<div class="flex items-center gap-2">
		<Button href="/o/{org.slug}/p/{project.slug}/snippets?tool={snippetTool}" variant="secondary" size="md"
			>Get config snippet</Button
		>
	</div>
</div>

<div class="grid grid-cols-4 gap-2">
	<StatTile label="Objects" value={formatCount(usageRow?.objects_count ?? null)} />
	<StatTile label="Size" value={formatBytes(usageRow?.logical_bytes ?? null)} />
	<StatTile label="Quota" value={quotaApplicable ? formatQuota(backend.quota_bytes) : 'n/a'} />
	<StatTile label="Retention" value={formatRetentionWindow(backend.retention_window)} />
</div>

<EmptyState
	glyph="∅"
	title="Rates over time are not recorded"
	desc="Bakery keeps no rollup table. Scrape --metrics-addr with Prometheus for live hit/miss and size counters."
/>

<div class="grid grid-cols-[3fr_2fr] items-start gap-2">
	<div class="flex min-w-0 flex-col gap-[14px]">
		{#if kind === 'hashserv'}
			<EmptyState
				glyph="∅"
				title="Hash equivalence stores unihashes, not cache objects"
				desc="Retention sweeps it together with sstate. There is nothing to browse here."
			/>
		{:else}
			{#if NAMESPACES[kind].length > 1}
				<Tabs
					tabs={NAMESPACES[kind].map((ns) => ({ id: ns, label: nsLabel(ns) }))}
					active={namespace}
					mono
					onChange={(id) => (namespace = id)}
				/>
			{/if}

			{#if objectsFailed}
				<EmptyState glyph="✕" title="Could not load objects" desc={objectsFailed}>
					{#snippet action()}
						<Button variant="secondary" size="md" onclick={() => loadObjects(true)}>Retry</Button>
					{/snippet}
				</EmptyState>
			{:else if objectsPending && objects.length === 0}
				<div class="flex flex-col gap-1">
					{#each [1, 2, 3, 4] as s (s)}
						<Skeleton height={28} />
					{/each}
				</div>
			{:else if objects.length === 0}
				<EmptyState glyph="∅" title="No objects in this namespace yet" />
			{:else}
				<TableWrap>
					<TableRoot dense>
						<thead>
							<tr>
								<Th>Key</Th>
								<Th>Digest</Th>
								<Th num>Size</Th>
								<Th>Created</Th>
								<Th>Last accessed</Th>
							</tr>
						</thead>
						<tbody>
							{#each objects as o (o.namespace + '/' + o.key)}
								<Tr>
									<Td mono>{o.key}</Td>
									<Td mono>{o.digest}</Td>
									<Td num>{formatBytes(o.size_bytes)}</Td>
									<Td class="whitespace-nowrap">{formatDateTimeUTC(o.created_at)}</Td>
									<Td class="whitespace-nowrap">{formatApproxAccessed(o.accessed_at)}</Td>
								</Tr>
							{/each}
						</tbody>
					</TableRoot>
				</TableWrap>
			{/if}

			{#if cursor}
				<div>
					<Button variant="secondary" size="sm" disabled={objectsPending} onclick={() => loadObjects(false)}>
						{objectsPending ? 'Loading…' : 'Load more'}
					</Button>
				</div>
			{/if}
		{/if}
	</div>

	<div class="rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3">
		<div class="mb-2 flex items-center justify-between">
			<div class="text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3">
				Config
			</div>
			{#if canEdit}
				<Button variant="ghost" size="sm" onclick={openEdit}>Edit</Button>
			{/if}
		</div>
		<KeyValueList pairs={configPairs} />
	</div>
</div>

{#if showEdit}
	<Modal title="Edit {kind}" onclose={closeEdit}>
		<div class="flex flex-col gap-[14px]">
			<Toggle bind:checked={editEnabled} label="Enabled" />
			<div class="flex flex-col gap-0.5">
				<Toggle bind:checked={editReadAuth} label="Require authentication for reads" />
				<div class="pl-10 text-sm text-text-3">Writes always require a key regardless.</div>
			</div>

			<Field label="Retention">
				{#snippet children(f)}
					<div class="flex flex-col gap-1.5">
						<Checkbox bind:checked={editRetentionForever} label="Retain forever" />
						<Input
							mono
							size="sm"
							placeholder="2160h"
							disabled={editRetentionForever}
							bind:value={editRetentionValue}
							{...f}
						/>
					</div>
				{/snippet}
			</Field>

			{#if quotaApplicable}
				<Field label="Quota">
					{#snippet children(f)}
						<div class="flex flex-col gap-1.5">
							<Checkbox bind:checked={editQuotaNoCap} label="No cap" />
							<Input
								mono
								size="sm"
								placeholder="500 GB"
								disabled={editQuotaNoCap}
								bind:value={editQuotaValue}
								{...f}
							/>
						</div>
					{/snippet}
				</Field>
			{/if}

			{#if kind === 'oci'}
				<Field label="Default upstream">
					{#snippet children(f)}
						<Input mono bind:value={editOciDefaultUpstream} {...f} />
					{/snippet}
				</Field>
				<Field label="Allowed upstreams" hint="Comma-separated; the SSRF allowlist for ?ns=.">
					{#snippet children(f)}
						<Input mono bind:value={editOciUpstreamsCsv} {...f} />
					{/snippet}
				</Field>
				<Field label="Tag TTL">
					{#snippet children(f)}
						<Input mono bind:value={editOciTagTtl} {...f} />
					{/snippet}
				</Field>
			{/if}

			{#if kind === 'hashserv'}
				<Field
					label="Chained upstream"
					hint="A ws:// or wss:// hashserv address. Leave blank for direct-only."
				>
					{#snippet children(f)}
						<Input mono bind:value={editHashservUpstream} {...f} />
					{/snippet}
				</Field>
			{/if}

			{#if editError}
				<div class="text-sm text-err">{editError}</div>
			{/if}
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={closeEdit} disabled={editPending}>Cancel</Button>
			<Button variant="primary" size="md" onclick={submitEdit} disabled={editPending}>
				{editPending ? 'Saving…' : 'Save changes'}
			</Button>
		{/snippet}
	</Modal>
{/if}
