<script lang="ts">
	import { page } from '$app/state';

	import { ConsoleNav } from '$lib/components/navigation';
	import { ToastHost } from '$lib/components/feedback';
	import type { Project } from '$lib/api/types';
	import type { LayoutProps } from './$types';

	let { data, children }: LayoutProps = $props();

	// Org and project scope come from the PATH, never from a store: `page.params`
	// is the single source of truth and it is what makes deep links and the back
	// button correct for free.
	const orgSlug = $derived(page.params.org ?? null);
	const projectSlug = $derived(page.params.project ?? null);

	// `page.data` carries whatever the nested org layout loaded; on the global
	// screens (`/orgs`, `/user`) there is no org layout and therefore no list.
	const projects: Project[] = $derived(page.data.projects ?? []);
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
