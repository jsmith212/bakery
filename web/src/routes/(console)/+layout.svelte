<script lang="ts">
	import { ConsoleNav } from '$lib/components/navigation';

	let { children } = $props();
</script>

<div class="flex min-h-screen bg-bg-0">
	<ConsoleNav />
	<main class="flex min-w-0 flex-1 flex-col gap-4 px-5 py-4">
		{@render children()}
	</main>
</div>
