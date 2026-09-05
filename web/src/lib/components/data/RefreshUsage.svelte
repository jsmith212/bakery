<script lang="ts">
	import { invalidateAll } from '$app/navigation';

	import { measureProjectUsage } from '$lib/api/projects';
	import { formatRelative } from '$lib/format';
	import { toastError } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';

	/**
	 * The measured-at caption and its Refresh control.
	 *
	 * ONE COMPONENT, THREE SCREENS. The overview, the backends index and the backend
	 * detail page all render the same figures from the same `cache_backend_usage`
	 * rows, and a Refresh that behaved differently on one of them would be a bug
	 * nobody would notice until somebody compared two tabs.
	 *
	 * The caption is RELATIVE ("measured 3 min ago") with the absolute UTC instant in
	 * a `title`. Absolute-only was the previous rendering, and it answers the wrong
	 * question: what a reader wants from a usage figure is not when it was taken but
	 * how much to trust it, and "2026-09-04 12:00 UTC" makes them do the subtraction.
	 */
	interface Props {
		org: string;
		project: string;
		/** The newest `measured_at` across the rows on screen, or null when none. */
		measuredAt: string | null;
	}

	let { org, project, measuredAt }: Props = $props();

	let pending = $state(false);

	// Recomputed on every render rather than ticking on a timer: these screens are
	// re-rendered by `invalidateAll` after any mutation, and a caption that says
	// "3 min ago" when it is really four is not a defect worth a per-second interval
	// on every dashboard.
	const label = $derived(measuredAt ? `measured ${formatRelative(measuredAt)}` : 'not yet measured');
	const absolute = $derived(measuredAt ?? undefined);

	async function refresh() {
		if (pending) return;
		pending = true;

		try {
			// The endpoint is rate-limited server-side and answers 200 either way, so
			// there is no "too soon" branch to handle here. invalidateAll rather than
			// merging the response: every screen that mounts this reads usage through its
			// own load, and merging would leave the others stale.
			await measureProjectUsage(org, project);
			await invalidateAll();
		} catch (err) {
			toastError(err, 'Could not refresh usage');
		} finally {
			pending = false;
		}
	}
</script>

<div class="flex items-center gap-2">
	<span class="text-xs text-text-3" title={absolute}>{label}</span>
	<Button variant="ghost" size="sm" onclick={refresh} disabled={pending}>
		{pending ? 'Refreshing…' : 'Refresh'}
	</Button>
</div>
