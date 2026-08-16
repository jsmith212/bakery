<script lang="ts">
	import { page } from '$app/state';

	import { ConsoleNav } from '$lib/components/navigation';
	import { ToastHost } from '$lib/components/feedback';
	import { lastOrg, lastProject } from '$lib/storage';
	import { resolveNavScope } from '$lib/tenancy';
	import type { Project } from '$lib/api/types';
	import type { LayoutProps } from './$types';

	let { data, children }: LayoutProps = $props();

	// Global screens (`/user`, `/gc`, `/orgs`, `/site-admins`) sit beside
	// `(console)/o/[org]/`, not inside it, so `page.params.org/project` are
	// `undefined` there -- without a fallback the switcher reads "none / none"
	// on every one of them. `resolveNavScope` (`$lib/tenancy`) is the pure,
	// unit-tested decision; this is only the two live storage reads it needs.
	const scope = $derived(
		resolveNavScope({
			paramsOrg: page.params.org ?? null,
			paramsProject: page.params.project ?? null,
			visibleOrgs: data.orgs.map((o) => o.slug),
			rememberedOrg: lastOrg(),
			rememberedProject: lastProject
		})
	);

	const orgSlug = $derived(scope.org);
	const projectSlug = $derived(scope.project);

	// `page.data` carries whatever the nested org layout loaded; on the global
	// screens there is no org layout and therefore no list at all. `null` (not
	// `[]`) is deliberate: it lets ConsoleNav tell "not loaded, global page"
	// apart from "loaded, this org genuinely has zero projects" -- collapsing
	// the two would render an enabled project switcher that lies "No projects
	// yet" about an org it never asked about.
	const projects: Project[] | null = $derived(page.data.projects ?? null);
</script>

<div class="flex min-h-screen bg-bg-0">
	<ConsoleNav
		me={data.me}
		orgs={data.orgs}
		{projects}
		currentOrg={orgSlug}
		currentProject={projectSlug}
	/>
	<main class="flex min-w-0 flex-1 flex-col gap-4 px-5 py-4">
		{@render children()}
	</main>
</div>

<ToastHost />
