<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';

	import { logout } from '$lib/api/auth';
	import { isSiteAdmin } from '$lib/roles';
	import { clearSession } from '$lib/session';
	import { toastError } from '$lib/toasts';
	import { orgPath, projectPath } from '$lib/tenancy';
	import { theme, setTheme, resolveTheme } from '$lib/theme';
	import { Button } from '$lib/components/buttons';
	import type { Me, Org, Project } from '$lib/api/types';

	interface Props {
		me: Me;
		/** `GET /orgs` -- every org the caller can VIEW, not only their memberships. */
		orgs: Org[];
		/** `GET /orgs/{org}/projects`, empty on the global screens. */
		projects: Project[];
		currentOrg: string | null;
		currentProject: string | null;
	}

	let { me, orgs, projects, currentOrg, currentProject }: Props = $props();

	let openMenu = $state<'org' | 'project' | null>(null);

	const path = $derived(page.url.pathname);
	const orgBase = $derived(currentOrg ? orgPath(currentOrg) : null);
	const projectBase = $derived(
		currentOrg && currentProject ? projectPath(currentOrg, currentProject) : null
	);

	// A hardcoded `/backends/sstate` 404s for a project whose only configured
	// kind is, say, `oci` -- there is no sstate mount to land on. Point at the
	// current project's own first configured kind instead, falling back to the
	// "add a backend" screen when it has none yet.
	const currentProjectObj = $derived(projects.find((p) => p.slug === currentProject) ?? null);
	const backendsHref = $derived.by(() => {
		if (!projectBase) return null;
		const kind = currentProjectObj?.backends[0];

		return kind ? `${projectBase}/backends/${kind}` : `${projectBase}/backends/new`;
	});

	// Nav sections appear only when their scope exists. A "Backends" link with no
	// project in the path would resolve to nothing; an absent link says the same
	// thing honestly.
	const projectNav = $derived(
		projectBase && backendsHref
			? [
					{ label: 'Overview', href: `${projectBase}/overview` },
					{ label: 'Backends', href: backendsHref },
					{ label: 'API keys', href: `${projectBase}/keys` },
					{ label: 'Config snippets', href: `${projectBase}/snippets` }
				]
			: []
	);

	const orgNav = $derived(
		orgBase
			? [
					{ label: 'Projects', href: `${orgBase}/projects` },
					{ label: 'Members', href: `${orgBase}/members` },
					{ label: 'Settings', href: `${orgBase}/settings` }
				]
			: []
	);

	// Instance-wide operator screens. Gated on `is_site_admin`, which is what
	// `GET /me` reports and what every one of those endpoints re-checks -- the
	// gate is cosmetic and the server is the authority.
	const instanceNav = $derived(
		isSiteAdmin(me)
			? [
					{ label: 'Garbage collection', href: '/gc' },
					{ label: 'Site admins', href: '/site-admins' }
				]
			: []
	);

	function active(href: string): boolean {
		return path === href || path.startsWith(`${href}/`);
	}

	// "Backends" stays lit across `/backends/sstate`, `/backends/oci` and
	// `/backends/new`, which `active()` alone would not do for a link that names
	// one kind.
	function navActive(href: string): boolean {
		if (projectBase && href.startsWith(`${projectBase}/backends`)) {
			return path.startsWith(`${projectBase}/backends`);
		}

		return active(href);
	}

	const userActive = $derived(path === '/user');
	const resolved = $derived(resolveTheme($theme));
	const themeLabel = $derived(resolved === 'dark' ? 'Light theme' : 'Dark theme');
	const initial = $derived((me.display_name || me.email || '?').trim().charAt(0).toUpperCase());

	function toggleTheme() {
		setTheme(resolved === 'dark' ? 'light' : 'dark');
	}

	function toggleMenu(which: 'org' | 'project') {
		openMenu = openMenu === which ? null : which;
	}

	let signingOut = $state(false);

	async function signOut() {
		if (signingOut) return;
		signingOut = true;

		try {
			// Bodyless POST, matching `logout()`'s own contract (see api/auth.ts):
			// `requireJSON` would accept a JSON body here too, but there is nothing
			// to send, so this sends nothing.
			await logout();
			clearSession();
			await goto('/login', { replaceState: true });
		} catch (err) {
			toastError(err, 'Could not sign out');
		} finally {
			signingOut = false;
		}
	}

	const itemChrome =
		'flex w-full items-center gap-2 rounded-1 px-2 text-base no-underline cursor-pointer transition-[background-color,color] duration-[var(--dur-1)] ease-[var(--ease)]';
	const itemIdle = 'text-text-2 hover:bg-bg-3 hover:text-text-1';
	const itemActive = 'bg-accent-muted text-accent-text';
	const itemAccent = 'text-accent-text hover:bg-bg-3';
	const selectorBtn =
		'flex h-[26px] w-full cursor-pointer items-center gap-1.5 rounded-1 border border-border-0 bg-bg-2 px-2 text-left';
	const menu =
		'absolute left-0 top-[calc(100%+4px)] z-40 flex max-h-[320px] w-[204px] flex-col gap-px overflow-y-auto rounded-2 border border-border-1 bg-bg-2 p-1 shadow-[var(--shadow-overlay)]';
	const sectionLabel =
		'mb-1 mt-4 px-2 text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3';
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
					class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm {currentOrg
						? 'text-text-1'
						: 'text-text-3'}">{currentOrg ?? 'none'}</span
				>
				<span class="ml-auto text-[8px] text-text-3" aria-hidden="true">▼</span>
			</button>
			{#if openMenu === 'org'}
				<div class={menu}>
					{#each orgs as o (o.id)}
						<a
							href="{orgPath(o.slug)}/projects"
							aria-current={o.slug === currentOrg ? 'true' : undefined}
							onclick={() => (openMenu = null)}
							class="{itemChrome} h-7 {o.slug === currentOrg ? itemActive : itemIdle}"
						>
							<span class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm"
								>{o.slug}</span
							>
						</a>
					{/each}
					<div class="my-[3px] border-t border-border-0"></div>
					<a href="/orgs" onclick={() => (openMenu = null)} class="{itemChrome} h-7 {itemAccent}"
						>All organizations</a
					>
				</div>
			{/if}
		</div>

		<div class="relative">
			<button
				type="button"
				aria-haspopup="true"
				aria-expanded={openMenu === 'project'}
				disabled={!currentOrg}
				onclick={() => toggleMenu('project')}
				class="{selectorBtn} disabled:cursor-not-allowed"
			>
				<span
					class="w-[34px] shrink-0 text-[9px] font-medium uppercase tracking-[var(--tracking-label)] text-text-3"
					>Proj</span
				>
				<span
					class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm {currentProject
						? 'text-text-1'
						: 'text-text-3'}">{currentProject ?? 'none'}</span
				>
				<span class="ml-auto text-[8px] text-text-3" aria-hidden="true">▼</span>
			</button>
			{#if openMenu === 'project' && currentOrg}
				<div class={menu}>
					{#each projects as p (p.id)}
						<a
							href="{projectPath(currentOrg, p.slug)}/overview"
							aria-current={p.slug === currentProject ? 'true' : undefined}
							onclick={() => (openMenu = null)}
							class="{itemChrome} h-7 {p.slug === currentProject ? itemActive : itemIdle}"
						>
							<span class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm"
								>{p.slug}</span
							>
						</a>
					{:else}
						<div class="px-2 py-1.5 text-sm text-text-3">No projects yet</div>
					{/each}
					<div class="my-[3px] border-t border-border-0"></div>
					<a
						href="{orgPath(currentOrg)}/projects"
						onclick={() => (openMenu = null)}
						class="{itemChrome} h-7 {itemAccent}">All projects</a
					>
				</div>
			{/if}
		</div>
	</div>

	{#if projectNav.length}
		<div class={sectionLabel}>Project</div>
		{#each projectNav as it (it.href)}
			<a
				href={it.href}
				aria-current={navActive(it.href) ? 'page' : undefined}
				class="{itemChrome} h-7 {navActive(it.href) ? itemActive : itemIdle}">{it.label}</a
			>
		{/each}
	{/if}

	{#if orgNav.length}
		<div class={sectionLabel}>Org</div>
		{#each orgNav as it (it.href)}
			<a
				href={it.href}
				aria-current={active(it.href) ? 'page' : undefined}
				class="{itemChrome} h-7 {active(it.href) ? itemActive : itemIdle}">{it.label}</a
			>
		{/each}
	{/if}

	{#if instanceNav.length}
		<div class={sectionLabel}>Instance</div>
		{#each instanceNav as it (it.href)}
			<a
				href={it.href}
				aria-current={active(it.href) ? 'page' : undefined}
				class="{itemChrome} h-7 {active(it.href) ? itemActive : itemIdle}">{it.label}</a
			>
		{/each}
	{/if}

	<div class="mt-auto flex flex-col gap-1 pt-2">
		<Button variant="ghost" size="sm" class="w-full justify-start" onclick={toggleTheme}>
			{themeLabel}
		</Button>
		<Button
			variant="ghost"
			size="sm"
			class="w-full justify-start"
			disabled={signingOut}
			onclick={signOut}
		>
			Sign out
		</Button>
		<a
			href="/user"
			aria-current={userActive ? 'page' : undefined}
			class="{itemChrome} h-9 {userActive ? itemActive : itemIdle}"
		>
			<span
				class="inline-flex h-[22px] w-[22px] shrink-0 items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[10px] font-semibold text-accent-text"
				>{initial}</span
			>
			<span class="overflow-hidden text-ellipsis whitespace-nowrap text-sm">{me.email}</span>
		</a>
	</div>
</nav>
