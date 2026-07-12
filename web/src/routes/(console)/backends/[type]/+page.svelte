<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/buttons';
	import { Badge, type BadgeStatus } from '$lib/components/badges';
	import { StatTile, TimeSeriesChart } from '$lib/components/data';
	import { TableWrap, TableRoot, Tr, Th, Td } from '$lib/components/table';
	import { KeyValueList } from '$lib/components/content';
	import { EmptyState } from '$lib/components/feedback';

	type Delta = 'up' | 'down' | 'flat';

	interface StatDef {
		label: string;
		value: string;
		unit: string;
		delta: string;
		deltaKind: Delta;
	}

	interface ColDef {
		label: string;
		num?: boolean;
	}

	interface BackendDef {
		status: [BadgeStatus, string, string];
		neverUsed?: boolean;
		emptyDesc?: string;
		stats: StatDef[];
		chartTitle: string;
		a: string;
		b: string;
		yMax: number;
		yTicks: string[];
		seriesA: number[];
		seriesB: number[];
		sizeTitle: string;
		sizeLegend: string;
		sizeMax: number;
		sizeTicks: string[];
		size: number[];
		middleTitle: string;
		middleCols: ColDef[];
		middleRows: string[][];
		requestsTitle: string;
		opCol: string;
		keyCol: string;
		reqs: string[][];
		config: [string, string, string][];
	}

	const backends: Record<string, BackendDef> = {
		sstate: {
			status: ['hit', '●', 'healthy'],
			stats: [
				{ label: 'HEAD hit rate', value: '96.1', unit: '%', delta: '+0.9% 24h', deltaKind: 'up' },
				{ label: 'Objects', value: '184,201', unit: '', delta: '+2,114 24h', deltaKind: 'flat' },
				{ label: 'Size', value: '212.4', unit: 'GB', delta: '42% of quota', deltaKind: 'flat' },
				{ label: 'HEAD p50', value: '1.8', unit: 'ms', delta: 'p99 6.2 ms', deltaKind: 'flat' }
			],
			chartTitle: 'HEAD hit / miss — 24h',
			a: 'hit',
			b: 'miss',
			yMax: 100,
			yTicks: ['0%', '50%', '100%'],
			seriesA: [90, 92, 91, 94, 93, 95, 96, 95, 96, 97, 96, 96],
			seriesB: [10, 8, 9, 6, 7, 5, 4, 5, 4, 3, 4, 4],
			sizeTitle: 'Size — 30d',
			sizeLegend: 'GB',
			sizeMax: 250,
			sizeTicks: ['0', '125', '250'],
			size: [150, 158, 161, 170, 168, 176, 184, 190, 196, 205, 209, 212],
			middleTitle: 'Top misses — 24h',
			middleCols: [
				{ label: 'Object key' },
				{ label: 'PN' },
				{ label: 'Misses', num: true },
				{ label: 'Last miss' }
			],
			middleRows: [
				['9c/2e/sstate:openssl:core2-64:3.2.1:r0…|m', 'openssl|m', '212|n', '2 min ago'],
				['4b/17/sstate:linux-yocto:qemux86-64:6.6…|m', 'linux-yocto|m', '148|n', '11 min ago'],
				['d0/a1/sstate:gcc-cross-x86_64:13.2.0…|m', 'gcc-cross|m', '97|n', '26 min ago'],
				['77/f2/sstate:webkitgtk:core2-64:2.42.5…|m', 'webkitgtk|m', '61|n', '1 h ago']
			],
			requestsTitle: 'Recent requests',
			opCol: 'Method',
			keyCol: 'Object key',
			reqs: [
				['14:32:07.412', 'HEAD', 'a3/f8/sstate:busybox:core2-64:1.36.1:r0:core2-64:11:a3f8…', 'hit', '0', '2.1'],
				['14:32:07.415', 'HEAD', '9c/2e/sstate:openssl:core2-64:3.2.1:r0:core2-64:11:9c2e…', 'miss', '0', '1.8'],
				['14:32:07.431', 'GET', 'a3/f8/sstate:busybox:core2-64:1.36.1:r0:core2-64:11:a3f8…', 'hit', '184,201', '12.4'],
				['14:32:08.102', 'PUT', '9c/2e/sstate:openssl:core2-64:3.2.1:r0:core2-64:11:9c2e…', 'hit', '921,544', '88.9'],
				['14:32:09.771', 'HEAD', '4b/17/sstate:glibc:core2-64:2.39:r0:core2-64:11:4b17…', 'hit', '0', '1.2'],
				['14:32:10.204', 'HEAD', 'd0/a1/sstate:gcc-cross-x86_64:13.2.0:r0:x86_64:11:d0a1…', 'hit', '0', '1.4']
			],
			config: [
				['Endpoint', '/cache/acme/firmware/sstate', 'm'],
				['Auth required', 'yes — reads and writes', 'p'],
				['Overwrite', 'no', 'p'],
				['Verify key = sha256', 'no (sstate keys are unihashes)', 'p'],
				['Quota', '500 GB', 'p'],
				['GC', 'coupled with hashserv · weekly', 'p']
			]
		},
		downloads: {
			status: ['hit', '●', 'healthy'],
			stats: [
				{ label: 'Hit rate', value: '99.2', unit: '%', delta: '+0.1% 24h', deltaKind: 'up' },
				{ label: 'Tarballs', value: '12,845', unit: '', delta: '+38 24h', deltaKind: 'flat' },
				{ label: 'Size', value: '84.7', unit: 'GB', delta: '17% of quota', deltaKind: 'flat' },
				{ label: 'GET p50', value: '38.5', unit: 'ms', delta: 'p99 412 ms', deltaKind: 'flat' }
			],
			chartTitle: 'Hit / miss — 24h',
			a: 'hit',
			b: 'miss',
			yMax: 100,
			yTicks: ['0%', '50%', '100%'],
			seriesA: [99, 99, 98, 99, 99, 100, 99, 99, 99, 99, 99, 99],
			seriesB: [1, 1, 2, 1, 1, 0, 1, 1, 1, 1, 1, 1],
			sizeTitle: 'Size — 30d',
			sizeLegend: 'GB',
			sizeMax: 100,
			sizeTicks: ['0', '50', '100'],
			size: [70, 71, 73, 74, 76, 77, 78, 80, 81, 83, 84, 85],
			middleTitle: 'Most-fetched tarballs — 24h',
			middleCols: [
				{ label: 'Basename' },
				{ label: 'Fetches', num: true },
				{ label: 'Size', num: true },
				{ label: 'Verified' }
			],
			middleRows: [
				['git2_github.com.torvalds.linux.tar.gz|m', '84|n', '1.2 GB|n', 'sha256 upstream'],
				['gcc-13.2.0.tar.xz|m', '61|n', '87.7 MB|n', 'sha256 upstream'],
				['openssl-3.2.1.tar.gz|m', '48|n', '17.2 MB|n', 'sha256 upstream'],
				['glibc-2.39.tar.xz|m', '41|n', '18.4 MB|n', 'sha256 upstream']
			],
			requestsTitle: 'Recent requests',
			opCol: 'Method',
			keyCol: 'Basename',
			reqs: [
				['14:31:58.031', 'GET', 'git2_github.com.torvalds.linux.tar.gz', 'hit', '1,204,118,332', '9,412'],
				['14:32:02.114', 'HEAD', 'openssl-3.2.1.tar.gz', 'hit', '0', '1.6'],
				['14:32:02.118', 'GET', 'openssl-3.2.1.tar.gz', 'hit', '17,244,912', '402'],
				['14:32:05.771', 'HEAD', 'zlib-1.3.1.tar.gz', 'miss', '0', '1.1'],
				['14:32:06.104', 'PUT', 'zlib-1.3.1.tar.gz', 'hit', '1,512,204', '61.2'],
				['14:32:09.001', 'GET', 'gcc-13.2.0.tar.xz', 'hit', '87,701,441', '1,104']
			],
			config: [
				['Endpoint', '/cache/acme/firmware/downloads', 'm'],
				['Auth required', 'yes — reads and writes', 'p'],
				['Layout', 'flat basenames', 'p'],
				['Overwrite', 'no', 'p'],
				['Quota', '500 GB', 'p'],
				['Note', 'clients verify SRC_URI checksums', 'p']
			]
		},
		hashserv: {
			status: ['hit', '●', 'healthy'],
			stats: [
				{ label: 'Lookups', value: '1,204', unit: '/s', delta: '+8% 24h', deltaKind: 'flat' },
				{ label: 'Equivalences found', value: '8,412', unit: '24h', delta: '12.4% of reports', deltaKind: 'up' },
				{ label: 'Connected builders', value: '14', unit: '', delta: 'peak 31', deltaKind: 'flat' },
				{ label: 'Unihashes', value: '2.1', unit: 'M', delta: '+31k 24h', deltaKind: 'flat' }
			],
			chartTitle: 'Lookups & equivalences — 24h',
			a: 'lookups/s',
			b: 'equivalences/min',
			yMax: 1500,
			yTicks: ['0', '750', '1.5k'],
			seriesA: [820, 960, 1100, 900, 1204, 1350, 1280, 1100, 1240, 1310, 1204, 1180],
			seriesB: [90, 140, 210, 160, 240, 310, 280, 220, 260, 300, 270, 240],
			sizeTitle: 'Mappings — 30d',
			sizeLegend: 'unihashes (M)',
			sizeMax: 2.5,
			sizeTicks: ['0', '1.25', '2.5'],
			size: [1.5, 1.55, 1.62, 1.68, 1.72, 1.79, 1.85, 1.9, 1.96, 2.0, 2.05, 2.1],
			middleTitle: 'Connected builders',
			middleCols: [
				{ label: 'Client' },
				{ label: 'Key' },
				{ label: 'msgs/s', num: true },
				{ label: 'Connected' }
			],
			middleRows: [
				['ci-runner-04.corp|m', 'ci-writer|m', '412|n', '1 h 12 min'],
				['ci-runner-07.corp|m', 'ci-writer|m', '388|n', '1 h 09 min'],
				['dev-anna.corp|m', 'anna (read)|m', '121|n', '22 min'],
				['dev-marko.corp|m', 'marko (read)|m', '96|n', '8 min']
			],
			requestsTitle: 'Recent RPCs',
			opCol: 'RPC',
			keyCol: 'taskhash → unihash',
			reqs: [
				['14:32:07.412', 'get-stream', '8f04a2…c1 → 77b3e9…04', 'hit', '—', '0.9'],
				['14:32:07.413', 'get-stream', 'b2c944…7a → (none)', 'miss', '—', '0.8'],
				['14:32:07.502', 'report', 'b2c944…7a → 5d81f0…2c minted', 'hit', '—', '3.1'],
				['14:32:08.114', 'report', 'e01ab7…33 → adopted 90cc41…b8', 'stale', '—', '2.7'],
				['14:32:08.921', 'get', '17d5c8…9e → 44a0b1…6f', 'hit', '—', '1.0'],
				['14:32:09.348', 'get-stream', 'a9f012…d4 → 8e77c2…19', 'hit', '—', '0.7']
			],
			config: [
				['Endpoint', 'wss://…/cache/acme/firmware/hashserv', 'm'],
				['Auth', '401 at WS upgrade if absent', 'p'],
				['Topology', 'direct only — no upstream proxying', 'p'],
				['Anon perms', 'none (upstream default not copied)', 'p'],
				['GC root', 'unihash — swept before sstate', 'p'],
				['Transport', 'WebSocket (TCP off)', 'p']
			]
		},
		bazel: {
			status: ['stale', '▲', 'quota 92%'],
			stats: [
				{ label: 'Hit rate', value: '88.4', unit: '%', delta: '−1.9% 24h', deltaKind: 'down' },
				{ label: 'AC entries', value: '412k', unit: '', delta: '+3.1k 24h', deltaKind: 'flat' },
				{ label: 'CAS blobs', value: '9.4', unit: 'M', delta: '+88k 24h', deltaKind: 'flat' },
				{ label: 'Size', value: '460.1', unit: 'GB', delta: '92% of quota', deltaKind: 'down' }
			],
			chartTitle: 'Hit / miss — 24h',
			a: 'hit',
			b: 'miss',
			yMax: 100,
			yTicks: ['0%', '50%', '100%'],
			seriesA: [91, 90, 91, 89, 90, 88, 89, 88, 87, 88, 88, 88],
			seriesB: [9, 10, 9, 11, 10, 12, 11, 12, 13, 12, 12, 12],
			sizeTitle: 'Size — 30d',
			sizeLegend: 'GB',
			sizeMax: 500,
			sizeTicks: ['0', '250', '500'],
			size: [310, 322, 335, 350, 361, 375, 390, 402, 419, 434, 448, 460],
			middleTitle: 'Sub-stores',
			middleCols: [
				{ label: 'Mount' },
				{ label: 'Semantics' },
				{ label: 'Entries', num: true },
				{ label: 'Size', num: true }
			],
			middleRows: [
				['/ac|m', 'opaque · overwrite · unverified', '412,332|n', '8.1 GB|n'],
				['/cas|m', 'content-addressed · verify sha256', '9,401,118|n', '452.0 GB|n']
			],
			requestsTitle: 'Recent requests (gRPC + HTTP)',
			opCol: 'Op',
			keyCol: 'Digest / key',
			reqs: [
				['14:32:07.412', 'FindMissing', 'batch of 1,204 digests → 88 missing', 'hit', '—', '14.2'],
				['14:32:07.688', 'BS.Read', 'cas/7f3a09b2c4d5e6f70819…', 'hit', '18,204,110', '204'],
				['14:32:08.001', 'GET /ac', 'ac/4b17aa20c9f1e8d7b6a5c4d3…', 'miss', '0', '1.3'],
				['14:32:08.220', 'PUT /ac', 'ac/4b17aa20c9f1e8d7b6a5c4d3…', 'hit', '2,114', '6.8'],
				['14:32:08.914', 'BS.Write', 'uploads/…/blobs/90cc41e2b8aa…', 'hit', '44,120,003', '512'],
				['14:32:09.310', 'GetActionRes', 'instance acme/firmware', 'hit', '1,844', '3.9']
			],
			config: [
				['Endpoint (HTTP)', '/cache/acme/firmware/{ac,cas}', 'm'],
				['Endpoint (gRPC)', 'instance_name = acme/firmware', 'm'],
				['Clients', 'moon · ccache · sccache', 'p'],
				['/ac', 'opaque blob store — never parsed', 'p'],
				['/cas', 'verify key = sha256(body)', 'p'],
				['Quota', '500 GB — GC LRU', 'p']
			]
		},
		oci: {
			status: ['idle', '○', 'no requests'],
			neverUsed: true,
			emptyDesc:
				"This backend is configured but has never received a request. The most common cause: the client can't authenticate, or containerd is still using the legacy mirror config that never reaches the proxy. Grab the exact hosts.toml below — it has the ?ns= convention baked in.",
			stats: [],
			chartTitle: '',
			a: '',
			b: '',
			yMax: 1,
			yTicks: ['', '', ''],
			seriesA: [0],
			seriesB: [0],
			sizeTitle: '',
			sizeLegend: '',
			sizeMax: 1,
			sizeTicks: ['', '', ''],
			size: [0],
			middleTitle: '',
			middleCols: [],
			middleRows: [],
			requestsTitle: '',
			opCol: '',
			keyCol: '',
			reqs: [],
			config: [
				['Endpoint (containerd)', '/cache/acme/firmware/docker/v2 + ?ns=', 'm'],
				['Endpoint (BuildKit)', '/v2/acme/firmware/… (prefix after /v2)', 'm'],
				['Auth', 'registry Bearer token flow (/v2/token)', 'p'],
				['Manifests', 'stored and served byte-exact', 'p'],
				['Upstreams', 'docker.io · ghcr.io · quay.io · gcr.io', 'p']
			]
		}
	};

	const order = ['sstate', 'downloads', 'hashserv', 'bazel', 'oci'];
	const ranges = ['1h', '24h', '7d', '30d'];

	const type = $derived(order.includes(page.params.type ?? '') ? (page.params.type ?? 'sstate') : 'sstate');
	const d = $derived(backends[type] ?? backends.sstate);

	let range = $state('24h');

	const hitMissSeries = $derived([
		{ label: d.a, color: 'var(--chart-hit)', data: d.seriesA },
		{ label: d.b, color: 'var(--chart-miss)', data: d.seriesB }
	]);
	const sizeSeries = $derived([{ label: d.sizeLegend, color: 'var(--chart-4)', data: d.size }]);

	function tickFmt(ticks: string[], max: number) {
		return (v: number): string => (v <= 0 ? ticks[0] : v >= max ? ticks[2] : ticks[1]);
	}

	function splitCell(c: string): [string, string] {
		const [text, flag = ''] = c.split('|');
		return [text, flag];
	}

	const microLabel =
		'text-xs font-medium tracking-[var(--tracking-label)] uppercase text-text-3';
	const card = 'rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3';
	const tabBase =
		'-mb-px inline-flex h-8 cursor-pointer items-center gap-1.5 border-b-2 bg-transparent px-[10px] transition-[color] duration-[var(--dur-2)] ease-[var(--ease)] hover:text-text-1';
</script>

<div class="flex items-center gap-1.5 text-sm text-text-3">
	<a href="/overview" class="text-accent-text hover:underline">acme/firmware</a>
	<span>/</span>
	<a href="/backends/sstate" class="text-accent-text hover:underline">Backends</a>
	<span>/</span>
	<span class="font-mono text-text-2">{type}</span>
</div>

<div class="flex flex-wrap items-center justify-between gap-3">
	<div class="flex items-center gap-2.5">
		<h1 class="m-0 font-mono text-lg font-semibold text-text-1">{type}</h1>
		<Badge status={d.status[0]}>{d.status[2]}</Badge>
	</div>
	<div class="flex items-center gap-2">
		<div role="tablist" class="flex gap-0.5">
			{#each ranges as r (r)}
				<button
					type="button"
					role="tab"
					aria-selected={r === range}
					onclick={() => (range = r)}
					class="{tabBase} font-sans text-base {r === range
						? 'border-accent-text text-text-1'
						: 'border-transparent text-text-2'}">{r}</button
				>
			{/each}
		</div>
		<Button variant="secondary" size="md" onclick={() => goto('/snippets')}>
			Get config snippet
		</Button>
	</div>
</div>

<div class="flex gap-0.5 border-b border-border-0">
	{#each order as t (t)}
		<a
			href="/backends/{t}"
			aria-current={t === type ? 'page' : undefined}
			class="{tabBase} font-mono text-sm no-underline hover:no-underline {t === type
				? 'border-accent-text text-text-1'
				: 'border-transparent text-text-2'}">{t}</a
		>
	{/each}
</div>

{#if d.neverUsed}
	<EmptyState title="Configured, but no requests yet" desc={d.emptyDesc}>
		{#snippet action()}
			<div class="flex gap-2">
				<Button variant="primary" size="md" onclick={() => goto('/snippets')}>
					Get config snippet
				</Button>
				<Button variant="ghost" size="md">Check auth setup</Button>
			</div>
		{/snippet}
	</EmptyState>
{:else}
	<div class="grid grid-cols-4 gap-2">
		{#each d.stats as s (s.label)}
			<StatTile
				label={s.label}
				value={s.value}
				unit={s.unit}
				delta={s.delta}
				deltaKind={s.deltaKind}
			/>
		{/each}
	</div>

	<div class="grid grid-cols-[3fr_2fr] gap-2">
		<div class={card}>
			<div class="{microLabel} mb-2">{d.chartTitle}</div>
			<TimeSeriesChart
				series={hitMissSeries}
				yMax={d.yMax}
				yFormat={tickFmt(d.yTicks, d.yMax)}
				xLabels={['00:00', '22:00']}
			/>
		</div>
		<div class={card}>
			<div class="{microLabel} mb-2">{d.sizeTitle}</div>
			<TimeSeriesChart
				series={sizeSeries}
				yMax={d.sizeMax}
				yFormat={tickFmt(d.sizeTicks, d.sizeMax)}
				xLabels={['30d ago', 'today']}
			/>
		</div>
	</div>

	<div class="grid grid-cols-[3fr_2fr] items-start gap-2">
		<div class="flex min-w-0 flex-col gap-[14px]">
			<div>
				<div class="{microLabel} mb-2">{d.middleTitle}</div>
				<TableWrap>
					<TableRoot dense>
						<thead>
							<Tr>
								{#each d.middleCols as c (c.label)}
									<Th num={c.num}>{c.label}</Th>
								{/each}
							</Tr>
						</thead>
						<tbody>
							{#each d.middleRows as row, ri (ri)}
								<Tr>
									{#each row as cell, ci (ci)}
										{@const parts = splitCell(cell)}
										<Td mono={parts[1] === 'm'} num={parts[1] === 'n'}>{parts[0]}</Td>
									{/each}
								</Tr>
							{/each}
						</tbody>
					</TableRoot>
				</TableWrap>
			</div>

			<div>
				<div class="{microLabel} mb-2">{d.requestsTitle}</div>
				<TableWrap>
					<TableRoot dense>
						<thead>
							<Tr>
								<Th class="w-[104px]">Time</Th>
								<Th class="w-[88px]">{d.opCol}</Th>
								<Th>{d.keyCol}</Th>
								<Th class="w-[76px]">Status</Th>
								<Th num class="w-[88px]">Bytes</Th>
								<Th num class="w-[60px]">ms</Th>
							</Tr>
						</thead>
						<tbody>
							{#each d.reqs as r, ri (ri)}
								<Tr>
									<Td mono>{r[0]}</Td>
									<Td mono>{r[1]}</Td>
									<Td mono class="max-w-[340px] overflow-hidden text-ellipsis whitespace-nowrap"
										>{r[2]}</Td
									>
									<Td>
										<Badge status={r[3] as BadgeStatus}>{r[3]}</Badge>
									</Td>
									<Td num>{r[4]}</Td>
									<Td num>{r[5]}</Td>
								</Tr>
							{/each}
						</tbody>
					</TableRoot>
				</TableWrap>
			</div>
		</div>

		<div class={card}>
			<div class="mb-2 flex items-center justify-between">
				<div class={microLabel}>Config</div>
				<Button variant="ghost" size="sm">Edit</Button>
			</div>
			<KeyValueList
				pairs={d.config.map((c) => ({ key: c[0], value: c[1], mono: c[2] === 'm' }))}
			/>
		</div>
	</div>
{/if}
