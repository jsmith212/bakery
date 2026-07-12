<script lang="ts">
	import { page } from '$app/state';
	import { Button } from '$lib/components/buttons';
	import { Badge, type BadgeStatus } from '$lib/components/badges';
	import { EmptyState, Skeleton } from '$lib/components/feedback';

	interface Project {
		slug: string;
		statusKind: BadgeStatus;
		statusGlyph: string;
		statusLabel: string;
		backends: string[];
		hitRate: string;
		size: string;
		reqs: string;
		activity: string;
	}

	const currentOrg = 'acme';

	const projects: Project[] = [
		{
			slug: 'firmware',
			statusKind: 'hit',
			statusGlyph: '●',
			statusLabel: 'healthy',
			backends: ['sstate', 'downloads', 'hashserv', 'bazel', 'oci'],
			hitRate: '94.2%',
			size: '760 GB',
			reqs: '1,847',
			activity: 'Last write 2 min ago'
		},
		{
			slug: 'tools',
			statusKind: 'hit',
			statusGlyph: '●',
			statusLabel: 'healthy',
			backends: ['bazel'],
			hitRate: '91.7%',
			size: '112 GB',
			reqs: '312',
			activity: 'Last write 14 min ago'
		},
		{
			slug: 'kernel-ci',
			statusKind: 'stale',
			statusGlyph: '▲',
			statusLabel: 'quota 95%',
			backends: ['sstate', 'hashserv'],
			hitRate: '88.9%',
			size: '475 GB',
			reqs: '640',
			activity: 'Last write 1 min ago'
		},
		{
			slug: 'images',
			statusKind: 'hit',
			statusGlyph: '●',
			statusLabel: 'healthy',
			backends: ['oci'],
			hitRate: '97.3%',
			size: '88 GB',
			reqs: '51',
			activity: 'Last pull 6 min ago'
		},
		{
			slug: 'sandbox',
			statusKind: 'idle',
			statusGlyph: '○',
			statusLabel: 'no traffic',
			backends: ['bazel', 'oci'],
			hitRate: '—',
			size: '0 B',
			reqs: '0',
			activity: 'Created 3 days ago — never used'
		}
	];

	const skeletons = [1, 2, 3, 4, 5, 6];

	const screen = $derived.by(() => {
		const s = page.url.searchParams.get('state');
		return s === 'loading' || s === 'empty' ? s : 'loaded';
	});
	const isLoading = $derived(screen === 'loading');
	const isEmpty = $derived(screen === 'empty');
	const isLoaded = $derived(screen === 'loaded');
	const subtitle = $derived(
		isEmpty ? '0 projects' : '5 projects · 1.4 TB stored · 2,850 req/s across the org'
	);
</script>

<div class="flex items-center justify-between">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Projects</h1>
		<div class="text-sm text-text-2">{subtitle}</div>
	</div>
	<Button variant="primary" size="md">New project</Button>
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

{#if isEmpty}
	<EmptyState
		glyph="∅"
		title="No projects in {currentOrg} yet"
		desc="A project holds cache backends, API keys, and members. Create one, add a backend, and point your first build at it — the config snippet generator gives you the exact client config."
	>
		{#snippet action()}
			<Button variant="primary" size="md">Create your first project</Button>
		{/snippet}
	</EmptyState>
{/if}

{#if isLoaded}
	<div class="grid grid-cols-3 gap-2.5">
		{#each projects as p (p.slug)}
			<a href="/overview" class="block no-underline">
				<div
					class="flex flex-col gap-2.5 rounded-2 border border-border-0 bg-bg-1 p-[14px] transition-[border-color] duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2"
				>
					<div class="flex items-center gap-2">
						<span class="font-mono text-base font-medium text-text-1">{p.slug}</span>
						<Badge status={p.statusKind} class="ml-auto">{p.statusLabel}</Badge>
					</div>
					<div class="flex flex-wrap gap-1">
						{#each p.backends as b (b)}
							<Badge variant="type">{b}</Badge>
						{/each}
					</div>
					<div class="flex gap-4 text-xs text-text-2">
						<span><span class="tabular font-medium text-text-1">{p.hitRate}</span> hit</span>
						<span><span class="tabular font-medium text-text-1">{p.size}</span> stored</span>
						<span><span class="tabular font-medium text-text-1">{p.reqs}</span> req/s</span>
					</div>
					<div class="text-xs text-text-3">{p.activity}</div>
				</div>
			</a>
		{/each}
	</div>
{/if}
