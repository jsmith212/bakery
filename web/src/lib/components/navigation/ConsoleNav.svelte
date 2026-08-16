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
		/**
		 * `GET /orgs/{org}/projects` -- `null`, not `[]`, on the global screens
		 * (`/user`, `/gc`, `/orgs`, `/site-admins`), which have no org layout to
		 * load it from. The distinction matters: `[]` means "this org has zero
		 * projects", which the switcher is allowed to say; `null` means "this
		 * page never asked", which it is not.
		 */
		projects: Project[] | null;
		currentOrg: string | null;
		currentProject: string | null;
	}

	let { me, orgs, projects, currentOrg, currentProject }: Props = $props();

	let openMenu = $state<'org' | 'project' | null>(null);
	const projectsLoaded = $derived(projects !== null);

	const path = $derived(page.url.pathname);
	const orgBase = $derived(currentOrg ? orgPath(currentOrg) : null);
	const projectBase = $derived(
		currentOrg && currentProject ? projectPath(currentOrg, currentProject) : null
	);

	// A hardcoded `/backends/sstate` 404s for a project whose only configured
	// kind is, say, `oci` -- there is no sstate mount to land on. Point at the
	// current project's own first configured kind instead, falling back to the
	// "add a backend" screen when it has none yet.
	//
	// That fallback is only honest when `projects` was actually loaded: on a
	// global page (`/user`, `/gc`, ...) with a remembered project, `projects`
	// is `null` and `currentProjectObj` can never resolve, so guessing
	// "/backends/new" would point at the create-a-backend form for a project
	// that may already have five. `!projectsLoaded` -> no href at all, and the
	// nav below omits the link rather than guess -- same rule as the "an
	// absent link says the same thing honestly" comment just below.
	const currentProjectObj = $derived(
		(projects ?? []).find((p) => p.slug === currentProject) ?? null
	);
	const backendsHref = $derived.by(() => {
		if (!projectBase || !projectsLoaded) return null;
		const kind = currentProjectObj?.backends[0];

		return kind ? `${projectBase}/backends/${kind}` : `${projectBase}/backends/new`;
	});

	// Nav sections appear only when their scope exists. A "Backends" link with no
	// project in the path would resolve to nothing; an absent link says the same
	// thing honestly. "Backends" itself is independently omitted when its href
	// is unknown (see backendsHref) -- the other three links are still correct
	// on a global page with a remembered project, so they stay.
	const projectNav = $derived.by(() => {
		if (!projectBase) return [];

		const nav = [{ label: 'Overview', href: `${projectBase}/overview` }];
		if (backendsHref) nav.push({ label: 'Backends', href: backendsHref });
		nav.push(
			{ label: 'API keys', href: `${projectBase}/keys` },
			{ label: 'Config snippets', href: `${projectBase}/snippets` }
		);

		return nav;
	});

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
	// A dead avatar URL degrades to the monogram, never a broken-image glyph.
	// Keyed to the URL that failed, not a bare flag: ConsoleNav is mounted
	// once for the whole console session, so a bare boolean would latch a
	// single transient failure (a captive portal, a rate-limited avatar host,
	// an IdP rotating the picture URL between logins) until a full page
	// reload, even after `me.avatar_url` changes to a working one.
	let failedAvatarURL = $state<string | null>(null);

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
	<div class="flex items-center gap-2 px-2 pb-1 pt-1.5">
		<img src="/logo-96.png" alt="" class="h-5 w-5 shrink-0" />
		<span class="text-md font-semibold text-text-1">Bakery</span>
	</div>

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
					{#if projectsLoaded}
						{#each projects ?? [] as p (p.id)}
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
					{:else if currentProject}
						<!-- This page has no project list loaded (a global screen) -- we
						     know only the remembered slug, not the org's real roster, so
						     render exactly that one link rather than claiming "No
						     projects yet" about an org we never asked. -->
						<a
							href="{projectPath(currentOrg, currentProject)}/overview"
							aria-current="true"
							onclick={() => (openMenu = null)}
							class="{itemChrome} h-7 {itemActive}"
						>
							<span class="overflow-hidden text-ellipsis whitespace-nowrap font-mono text-sm"
								>{currentProject}</span
							>
						</a>
					{:else}
						<!-- Neither a loaded roster nor a remembered project -- a global
						     page with a remembered org and no project history. Render the
						     honest "nothing to show" state rather than a bare divider
						     floating above "All projects" with nothing above it. -->
						<div class="px-2 py-1.5 text-sm text-text-3">No project selected</div>
					{/if}
					{#if projectsLoaded || currentProject}
						<div class="my-[3px] border-t border-border-0"></div>
					{/if}
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
			{#if me.avatar_url && me.avatar_url !== failedAvatarURL}
				<img
					src={me.avatar_url}
					alt=""
					referrerpolicy="no-referrer"
					onerror={() => (failedAvatarURL = me.avatar_url ?? null)}
					class="h-[22px] w-[22px] shrink-0 rounded-full border border-accent-border object-cover"
				/>
			{:else}
				<span
					class="inline-flex h-[22px] w-[22px] shrink-0 items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[10px] font-semibold text-accent-text"
					>{initial}</span
				>
			{/if}
			<span class="overflow-hidden text-ellipsis whitespace-nowrap text-sm">{me.email}</span>
		</a>
	</div>
</nav>
