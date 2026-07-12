<script lang="ts">
	import { Button } from '$lib/components/buttons';
	import { Badge, type BadgeStatus } from '$lib/components/badges';
	import { Input } from '$lib/components/inputs';
	import { KeyValueList } from '$lib/components/content';
	import { EmptyState, Modal } from '$lib/components/feedback';
	import { TableRoot, Tr, Th, Td } from '$lib/components/table';

	type DeltaKind = 'up' | 'down' | 'flat';

	let range = $state('24h');
	let objTab = $state('sstate');
	let filter = $state('');
	let deleteKey = $state<string | null>(null);

	const ranges = ['1h', '24h', '7d', '30d'];

	function path(data: number[], max: number, w: number, h: number, area: boolean): string {
		const n = data.length;
		const padL = w > 200 ? 36 : 2,
			padR = w > 200 ? 8 : 2,
			padT = w > 200 ? 8 : 2,
			padB = w > 200 ? 18 : 2;
		const pts = data.map((v, i) => [
			padL + (i * (w - padL - padR)) / (n - 1),
			h - padB - (Math.max(0, v) / max) * (h - padT - padB)
		]);
		let d = 'M' + pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' L');
		if (area) d += ` L${w - padR},${h - padB} L${padL},${h - padB} Z`;
		return d;
	}

	const hits = [82, 85, 84, 88, 87, 90, 91, 89, 93, 92, 94, 94.2];
	const misses = hits.map((v) => 100 - v);

	interface Stat {
		label: string;
		value: string;
		unit: string;
		delta: string;
		deltaKind: DeltaKind;
		data: number[];
		max: number;
		color: string;
	}

	const statsRaw: Stat[] = [
		{ label: 'Hit rate', value: '94.2', unit: '%', delta: '+1.8% 24h', deltaKind: 'up', data: hits, max: 100, color: 'var(--chart-hit)' },
		{ label: 'Miss rate', value: '5.8', unit: '%', delta: '−1.8% 24h', deltaKind: 'up', data: misses, max: 100, color: 'var(--chart-miss)' },
		{ label: 'Storage', value: '760.3', unit: 'GB', delta: '61% of quota', deltaKind: 'flat', data: [520, 580, 610, 660, 700, 730, 760], max: 800, color: 'var(--chart-4)' },
		{ label: 'Requests', value: '1,847', unit: 'req/s', delta: '−3.1% 24h', deltaKind: 'flat', data: [2100, 1900, 2000, 1850, 1900, 1847], max: 2200, color: 'var(--chart-5)' }
	];

	const stats = statsRaw.map((s) => ({
		...s,
		spark: path(s.data, s.max, 160, 24, false),
		sparkArea: path(s.data, s.max, 160, 24, true)
	}));

	const deltaClass: Record<DeltaKind, string> = {
		up: 'text-ok',
		down: 'text-err',
		flat: 'text-text-3'
	};

	interface Backend {
		type: string;
		statusKind: BadgeStatus;
		statusLabel: string;
		metric: string;
		metricLabel: string;
		size: string;
	}

	const backends: Backend[] = [
		{ type: 'sstate', statusKind: 'hit', statusLabel: 'healthy', metric: '96.1%', metricLabel: 'hit rate', size: '212.4 GB' },
		{ type: 'downloads', statusKind: 'hit', statusLabel: 'healthy', metric: '99.2%', metricLabel: 'hit rate', size: '84.7 GB' },
		{ type: 'hashserv', statusKind: 'hit', statusLabel: 'healthy', metric: '1,204', metricLabel: 'lookups/s', size: '3.1 GB' },
		{ type: 'bazel', statusKind: 'stale', statusLabel: 'quota 92%', metric: '88.4%', metricLabel: 'hit rate', size: '460.1 GB' },
		{ type: 'oci', statusKind: 'idle', statusLabel: 'no requests', metric: '—', metricLabel: 'hit rate', size: '0 B' }
	];

	interface ObjectSet {
		keyLabel: string;
		hitsLabel: string;
		count: string;
		footnote: string;
		warning: string;
		rows: string[][];
		emptyDesc?: string;
	}

	const objects: Record<string, ObjectSet> = {
		sstate: {
			keyLabel: 'Object key',
			hitsLabel: 'Hits',
			count: '184,201 objects · 212.4 GB',
			footnote:
				'Deleting an sstate object also drops unihashes that only it satisfies — the next build rebuilds those tasks.',
			warning:
				'hashserv mappings reachable only through this object are swept with it. Builds re-run the affected tasks; nothing breaks, it just gets slower once.',
			rows: [
				['a3/f8/sstate:busybox:core2-64:1.36.1:r0:core2-64:11:a3f8c92e_populate_sysroot.tar.zst', '180 KB', '4,211', '2 min ago', '2026-05-02'],
				['9c/2e/sstate:openssl:core2-64:3.2.1:r0:core2-64:11:9c2e44d1_package.tar.zst', '902 KB', '3,842', '3 min ago', '2026-05-02'],
				['4b/17/sstate:glibc:core2-64:2.39:r0:core2-64:11:4b17aa20_populate_sysroot.tar.zst', '18.4 MB', '3,514', '6 min ago', '2026-04-28'],
				['d0/a1/sstate:gcc-cross-x86_64:13.2.0:r0:x86_64:11:d0a1f7b3_populate_sysroot.tar.zst', '112.8 MB', '2,981', '10 min ago', '2026-04-28'],
				['77/f2/sstate:webkitgtk:core2-64:2.42.5:r0:core2-64:11:77f20c9e_package.tar.zst', '241.7 MB', '412', '1 h ago', '2026-06-11'],
				['e5/09/sstate:linux-yocto:qemux86-64:6.6.32:r0:qemux86-64:11:e509bb41_deploy.tar.zst', '88.1 MB', '1,204', '14 min ago', '2026-06-30'],
				['1c/6d/sstate:cmake-native:x86_64:3.28.3:r0:x86_64:11:1c6d0f88_populate_sysroot.tar.zst', '31.2 MB', '2,140', '22 min ago', '2026-04-28'],
				['b8/33/sstate:qemu-helper-native:x86_64:8.2.1:r0:x86_64:11:b8330d17_populate_sysroot.tar.zst', '4.4 MB', '988', '38 min ago', '2026-05-15']
			]
		},
		downloads: {
			keyLabel: 'Basename',
			hitsLabel: 'Fetches',
			count: '12,845 tarballs · 84.7 GB',
			footnote:
				'Clients verify SRC_URI checksums — deleting a tarball only causes an upstream re-fetch.',
			warning: 'The next build that needs it falls back to the upstream URL and re-populates the mirror.',
			rows: [
				['git2_github.com.torvalds.linux.tar.gz', '1.2 GB', '84', '31 min ago', '2026-03-14'],
				['gcc-13.2.0.tar.xz', '87.7 MB', '61', '1 h ago', '2026-03-14'],
				['openssl-3.2.1.tar.gz', '17.2 MB', '48', '2 h ago', '2026-03-20'],
				['glibc-2.39.tar.xz', '18.4 MB', '41', '2 h ago', '2026-03-20'],
				['zlib-1.3.1.tar.gz', '1.5 MB', '39', '3 h ago', '2026-04-02'],
				['busybox-1.36.1.tar.bz2', '2.5 MB', '36', '4 h ago', '2026-03-14']
			]
		},
		hashserv: {
			keyLabel: 'taskhash → unihash',
			hitsLabel: 'Lookups',
			count: '2.1M mappings · 3.1 GB',
			footnote:
				'unihashes(method, taskhash) is write-once. Deleting a mapping is the GC root operation — coupled sstate objects become unreachable.',
			warning:
				'sstate objects named by this unihash become unreachable and are swept in the next GC run. Affected tasks rebuild once.',
			rows: [
				['8f04a2…c1 → 77b3e9…04 (do_compile)', '—', '12,411', '1 min ago', '2026-06-01'],
				['b2c944…7a → 5d81f0…2c (do_configure)', '—', '8,904', '4 min ago', '2026-06-01'],
				['e01ab7…33 → 90cc41…b8 (do_install)', '—', '7,112', '9 min ago', '2026-05-22'],
				['17d5c8…9e → 44a0b1…6f (do_package)', '—', '5,388', '12 min ago', '2026-05-22'],
				['a9f012…d4 → 8e77c2…19 (do_compile)', '—', '4,201', '15 min ago', '2026-06-14']
			]
		},
		bazel: {
			keyLabel: 'Digest / key',
			hitsLabel: 'Hits',
			count: '9.8M entries · 460.1 GB',
			footnote:
				'/ac entries are opaque (ccache, sccache, moon) — deleting is always safe. /cas blobs are refcounted.',
			warning: 'Safe: clients treat a missing entry as a cache miss and repopulate it on the next build.',
			rows: [
				['ac/4b17aa20c9f1e8d7b6a5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4', '2.1 KB', '1,204', '2 min ago', '2026-07-01'],
				['cas/7f3a09b2c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7', '18.2 MB', '884', '5 min ago', '2026-06-28'],
				['cas/90cc41e2b8aa17f5d3c6094e2b1a8f7d6c5b4a3928170f6e5d4c3b2a19080716', '44.1 MB', '412', '18 min ago', '2026-06-28'],
				['ac/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855', '0 B', '9,412', '1 min ago', '2026-05-19'],
				['cas/1c6d0f88ab34e2917d5c8f04a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5', '312 KB', '204', '44 min ago', '2026-07-08']
			]
		},
		oci: {
			keyLabel: 'Manifest / blob',
			hitsLabel: 'Pulls',
			count: '0 objects',
			footnote: '',
			warning: '',
			rows: [],
			emptyDesc:
				'This backend is configured but has never received a request. Most likely containerd is still on the legacy mirror config — the snippet generator has the hosts.toml that actually works.'
		}
	};

	const objOrder = ['sstate', 'downloads', 'hashserv', 'bazel', 'oci'];
	const tabCounts: Record<string, string> = {
		sstate: '184k',
		downloads: '12.8k',
		hashserv: '2.1M',
		bazel: '9.8M',
		oci: '0'
	};

	const o = $derived(objects[objTab]);
	const objRows = $derived(
		o.rows.filter((r) => !filter || r[0].toLowerCase().includes(filter.toLowerCase()))
	);
	const hasObjects = $derived(o.rows.length > 0);
	const showDelete = $derived(deleteKey !== null);
	const deleteWarning = $derived(o.warning);

	function selectObjTab(t: string) {
		objTab = t;
		filter = '';
	}

	const tabBtn =
		'-mb-px inline-flex h-8 cursor-pointer items-center gap-1.5 border-b-2 border-transparent bg-transparent px-[10px] text-text-2 transition-[color] duration-[var(--dur-2)] ease-[var(--ease)] hover:text-text-1 aria-selected:border-accent-text aria-selected:text-text-1';
	const tabCount = 'tabular rounded-1 border border-border-0 bg-bg-2 px-1 py-px text-xs text-text-3';
</script>

<div class="flex items-center justify-between">
	<h1 class="text-lg font-semibold text-text-1">Overview</h1>
	<div class="flex items-center gap-2">
		<div role="tablist" class="flex gap-0.5">
			{#each ranges as r (r)}
				<button
					type="button"
					role="tab"
					aria-selected={range === r}
					onclick={() => (range = r)}
					class="{tabBtn} font-sans text-base">{r}</button
				>
			{/each}
		</div>
		<Button variant="primary" size="md">Get config snippet</Button>
	</div>
</div>

<div class="grid grid-cols-4 gap-2">
	{#each stats as s (s.label)}
		<div
			class="flex min-w-0 flex-col gap-1 rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3 font-sans"
		>
			<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
				{s.label}
			</div>
			<div class="flex flex-wrap items-baseline gap-2">
				<span class="tabular text-metric font-semibold tracking-[var(--tracking-tight)] text-text-1"
					>{s.value}</span
				>
				<span class="text-sm text-text-3">{s.unit}</span>
				<span class="tabular text-xs font-medium whitespace-nowrap {deltaClass[s.deltaKind]}"
					>{s.delta}</span
				>
			</div>
			<div class="mt-1">
				<svg width="100%" height="24" viewBox="0 0 160 24" preserveAspectRatio="none" aria-hidden="true">
					<path d={s.sparkArea} fill={s.color} opacity="0.10" stroke="none"></path>
					<path d={s.spark} fill="none" stroke={s.color} stroke-width="1.5" stroke-linejoin="round"></path>
				</svg>
			</div>
		</div>
	{/each}
</div>

<div class="grid grid-cols-5 gap-2">
	{#each backends as b (b.type)}
		<a href="/backends/{b.type}" class="block min-w-0 no-underline">
			<div
				class="flex flex-col gap-1.5 rounded-2 border border-border-0 bg-bg-1 px-3 py-2.5 transition-[border-color] duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2"
			>
				<div class="flex flex-wrap items-center justify-between gap-2">
					<Badge variant="type">{b.type}</Badge>
					<Badge status={b.statusKind}>{b.statusLabel}</Badge>
				</div>
				<div class="flex items-baseline gap-1.5">
					<span class="tabular text-[18px] font-semibold text-text-1">{b.metric}</span>
					<span class="text-xs text-text-3">{b.metricLabel}</span>
				</div>
				<div class="tabular text-xs text-text-2">{b.size}</div>
			</div>
		</a>
	{/each}
</div>

<div class="rounded-2 border border-border-0 bg-bg-1">
	<div class="flex items-center gap-3 px-[14px] pt-2">
		<div role="tablist" class="flex min-w-0 flex-1 gap-0.5 overflow-x-auto border-b border-border-0">
			{#each objOrder as t (t)}
				<button
					type="button"
					role="tab"
					aria-selected={objTab === t}
					onclick={() => selectObjTab(t)}
					class="{tabBtn} font-mono text-sm"
				>
					{t}<span class={tabCount}>{tabCounts[t]}</span>
				</button>
			{/each}
		</div>
		<div class="w-[220px] flex-none">
			<Input size="sm" mono placeholder="filter keys…" bind:value={filter} />
		</div>
	</div>

	{#if hasObjects}
		<TableRoot dense class="table-fixed">
			<thead>
				<tr>
					<Th>{o.keyLabel}</Th>
					<Th num style="width: 92px">Size</Th>
					<Th num style="width: 72px">{o.hitsLabel}</Th>
					<Th style="width: 110px">Last hit</Th>
					<Th style="width: 100px">Created</Th>
					<Th style="width: 76px"></Th>
				</tr>
			</thead>
			<tbody>
				{#each objRows as row (row[0])}
					<Tr>
						<Td mono class="overflow-hidden text-ellipsis whitespace-nowrap">{row[0]}</Td>
						<Td num class="whitespace-nowrap">{row[1]}</Td>
						<Td num>{row[2]}</Td>
						<Td class="whitespace-nowrap text-text-2">{row[3]}</Td>
						<Td mono class="whitespace-nowrap">{row[4]}</Td>
						<Td class="text-right">
							<Button
								variant="ghost"
								size="sm"
								style="color: var(--err)"
								onclick={() => (deleteKey = row[0])}>Delete</Button
							>
						</Td>
					</Tr>
				{/each}
			</tbody>
		</TableRoot>
		<div
			class="flex items-center justify-between border-t border-border-0 px-[14px] py-1.5 text-xs text-text-3"
		>
			<span>{o.footnote}</span>
			<span class="tabular">{o.count}</span>
		</div>
	{:else}
		<div class="p-[14px]">
			<EmptyState glyph="∅" title="No objects in {objTab}" desc={o.emptyDesc ?? ''}>
				{#snippet action()}
					<Button variant="secondary" size="md">Get config snippet</Button>
				{/snippet}
			</EmptyState>
		</div>
	{/if}
</div>

<div class="rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3">
	<div class="mb-2.5 text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
		Maintenance
	</div>
	<div class="grid grid-cols-[1fr_1.4fr] items-start gap-5">
		<KeyValueList
			pairs={[
				{ key: 'Last GC run', value: '2026-07-06 02:00 UTC', mono: true },
				{ key: 'Swept', value: '12,410 unihashes · 8,921 objects' },
				{ key: 'Reclaimed', value: '38.2 GB' },
				{ key: 'Next window', value: 'Sun 02:00–06:00 UTC' }
			]}
		/>
		<div class="flex flex-col gap-2">
			<div class="flex items-center justify-between gap-3">
				<div class="min-w-0 text-sm text-text-2">
					Sweep unreachable unihashes and unreferenced blobs now, outside the window.
				</div>
				<Button variant="secondary" size="sm" class="flex-none">Run GC now</Button>
			</div>
			<div class="flex items-center justify-between gap-3">
				<div class="min-w-0 text-sm text-text-2">
					Prune objects with no hit in 90 days — est.
					<span class="tabular text-text-1">41.2 GB</span> across sstate + downloads.
				</div>
				<Button variant="secondary" size="sm" class="flex-none">Preview prune</Button>
			</div>
			<div class="flex items-center justify-between gap-3">
				<div class="min-w-0 text-sm text-text-2">
					Delete every object matching a key prefix, with a dry-run count first.
				</div>
				<Button variant="secondary" size="sm" class="flex-none">Purge by prefix…</Button>
			</div>
		</div>
	</div>
</div>

{#if showDelete}
	<Modal title="Delete cached object" onclose={() => (deleteKey = null)}>
		<div class="flex flex-col gap-2.5">
			<div
				class="rounded-1 border border-border-0 bg-bg-inset px-2.5 py-2 font-mono text-[length:var(--mono-xs)] break-all text-text-1"
			>
				{deleteKey}
			</div>
			<div>{deleteWarning}</div>
		</div>
		{#snippet footer()}
			<Button variant="ghost" size="md" onclick={() => (deleteKey = null)}>Cancel</Button>
			<Button variant="danger" size="md" onclick={() => (deleteKey = null)}>Delete object</Button>
		{/snippet}
	</Modal>
{/if}
