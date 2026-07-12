<script lang="ts">
	import { page } from '$app/state';
	import { theme, setTheme, resolveTheme } from '$lib/theme';
	import { Button } from '$lib/components/buttons';
	import { Badge, type BadgeStatus } from '$lib/components/badges';

	interface OrgOption {
		name: string;
		count: string;
	}
	interface ProjectOption {
		name: string;
		status: BadgeStatus;
	}

	let openMenu = $state<'org' | 'project' | null>(null);
	let currentOrg = $state('acme');
	let currentProject = $state('firmware');

	const orgs: OrgOption[] = [
		{ name: 'acme', count: '5' },
		{ name: 'robotics-lab', count: '2' },
		{ name: 'personal', count: '1' }
	];

	const projects: ProjectOption[] = [
		{ name: 'firmware', status: 'hit' },
		{ name: 'tools', status: 'hit' },
		{ name: 'kernel-ci', status: 'stale' },
		{ name: 'images', status: 'hit' },
		{ name: 'sandbox', status: 'idle' }
	];

	const path = $derived(page.url.pathname);

	const projectNav = $derived([
		{ label: 'Overview', href: '/overview', active: path === '/overview' },
		{ label: 'Backends', href: '/backends/sstate', active: path.startsWith('/backends') },
		{ label: 'API keys', href: '/keys', active: path === '/keys' },
		{ label: 'Config snippets', href: '/snippets', active: path === '/snippets' }
	]);

	const orgNav = $derived([
		{ label: 'Projects', href: '/projects', active: path === '/projects' },
		{ label: 'Members', href: '/members', active: path === '/members' },
		{ label: 'Settings', href: '/settings', active: path === '/settings' }
	]);

	const userActive = $derived(path === '/user');
	const projectColor = $derived(currentProject === 'all projects' ? 'text-text-3' : 'text-text-1');

	const resolved = $derived(resolveTheme($theme));
	const themeLabel = $derived(resolved === 'dark' ? 'Light theme' : 'Dark theme');

	function toggleTheme() {
		setTheme(resolved === 'dark' ? 'light' : 'dark');
	}

	function toggleMenu(which: 'org' | 'project') {
		openMenu = openMenu === which ? null : which;
	}

	function selectOrg(name: string) {
		currentOrg = name;
		openMenu = null;
	}

	function selectProject(name: string) {
		currentProject = name;
		openMenu = null;
	}

	const itemChrome =
		'flex w-full items-center gap-2 rounded-1 px-2 text-base no-underline cursor-pointer transition-[background-color,color] duration-[var(--dur-1)] ease-[var(--ease)]';
	const itemIdle = 'text-text-2 hover:bg-bg-3 hover:text-text-1';
	const itemActive = 'bg-accent-muted text-accent-text';
	const itemAccent = 'text-accent-text hover:bg-bg-3';
	const selectorBtn =
		'flex h-[26px] w-full cursor-pointer items-center gap-1.5 rounded-1 border border-border-0 bg-bg-2 px-2 text-left';
	const menu =
		'absolute left-0 top-[calc(100%+4px)] z-40 flex w-[204px] flex-col gap-px rounded-2 border border-border-1 bg-bg-2 p-1 shadow-[var(--shadow-overlay)]';
</script>

<svelte:window
	onkeydown={(e) => {
		if (e.key === 'Escape') openMenu = null;
	}}
/>

<nav
	class="flex min-h-screen w-[var(--sidenav-w)] shrink-0 flex-col gap-px border-r border-border-0 bg-bg-1 p-2 font-sans"
	aria-label="Console"
>
	<div class="px-2 pb-1 pt-1.5 text-md font-semibold text-text-1">Bakery</div>

	<div class="flex flex-col gap-0.5 pb-2 pt-1">
		<div class="relative">
			<button
				type="button"
				aria-haspopup="true"
				aria-expanded={openMenu === 'org'}
				onclick={() => toggleMenu('org')}
				class={selectorBtn}
			>
				<span
					class="w-[34px] shrink-0 text-[9px] font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
					>Org</span
				>
				<span
					class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm text-text-1"
					>{currentOrg}</span
				>
				<span class="ml-auto text-[8px] text-text-3" aria-hidden="true">▼</span>
			</button>
			{#if openMenu === 'org'}
				<div class={menu}>
					{#each orgs as o (o.name)}
						<button
							type="button"
							aria-current={o.name === currentOrg ? 'true' : undefined}
							onclick={() => selectOrg(o.name)}
							class="{itemChrome} h-7 {o.name === currentOrg ? itemActive : itemIdle}"
						>
							<span class="font-mono text-sm">{o.name}</span>
							<span class="tabular ml-auto text-xs text-text-3">{o.count}</span>
						</button>
					{/each}
					<div class="my-[3px] border-t border-border-0"></div>
					<button type="button" class="{itemChrome} h-7 {itemAccent}">Create organization</button>
				</div>
			{/if}
		</div>

		<div class="relative">
			<button
				type="button"
				aria-haspopup="true"
				aria-expanded={openMenu === 'project'}
				onclick={() => toggleMenu('project')}
				class={selectorBtn}
			>
				<span
					class="w-[34px] shrink-0 text-[9px] font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
					>Proj</span
				>
				<span
					class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm {projectColor}"
					>{currentProject}</span
				>
				<span class="ml-auto text-[8px] text-text-3" aria-hidden="true">▼</span>
			</button>
			{#if openMenu === 'project'}
				<div class={menu}>
					{#each projects as p (p.name)}
						<button
							type="button"
							aria-current={p.name === currentProject ? 'true' : undefined}
							onclick={() => selectProject(p.name)}
							class="{itemChrome} h-7 {p.name === currentProject ? itemActive : itemIdle}"
						>
							<span class="font-mono text-sm">{p.name}</span>
							<span class="ml-auto"><Badge status={p.status} /></span>
						</button>
					{/each}
					<div class="my-[3px] border-t border-border-0"></div>
					<button
						type="button"
						onclick={() => selectProject('all projects')}
						class="{itemChrome} h-7 {itemIdle}">All projects</button
					>
					<button type="button" class="{itemChrome} h-7 {itemAccent}">New project</button>
				</div>
			{/if}
		</div>
	</div>

	<div
		class="mb-1 mt-4 px-2 text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
	>
		Project
	</div>
	{#each projectNav as it (it.href)}
		<a
			href={it.href}
			aria-current={it.active ? 'page' : undefined}
			class="{itemChrome} h-7 {it.active ? itemActive : itemIdle}">{it.label}</a
		>
	{/each}

	<div
		class="mb-1 mt-4 px-2 text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
	>
		Org
	</div>
	{#each orgNav as it (it.href)}
		<a
			href={it.href}
			aria-current={it.active ? 'page' : undefined}
			class="{itemChrome} h-7 {it.active ? itemActive : itemIdle}">{it.label}</a
		>
	{/each}

	<div class="mt-auto flex flex-col gap-1 pt-2">
		<Button variant="ghost" size="sm" class="w-full justify-start" onclick={toggleTheme}>
			{themeLabel}
		</Button>
		<a
			href="/user"
			aria-current={userActive ? 'page' : undefined}
			class="{itemChrome} h-9 {userActive ? itemActive : itemIdle}"
		>
			<span
				class="inline-flex h-[22px] w-[22px] shrink-0 items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[10px] font-semibold text-accent-text"
				>A</span
			>
			<span class="overflow-hidden text-ellipsis whitespace-nowrap text-sm">anna@acme.dev</span>
		</a>
	</div>
</nav>
