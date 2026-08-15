<script lang="ts">
	import { dismissToast, toasts } from '$lib/toasts';
	import Toast from './Toast.svelte';
</script>

<!--
	Mounted exactly once, in the console layout. Fixed to the bottom-right,
	above everything, and `pointer-events-none` on the stack so an empty host
	never intercepts a click on the screen behind it.
-->
<div
	class="pointer-events-none fixed bottom-4 right-4 z-50 flex flex-col items-end gap-2"
	aria-live="polite"
>
	{#each $toasts as t (t.id)}
		<div class="pointer-events-auto">
			<Toast
				variant={t.variant}
				title={t.title}
				detail={t.detail}
				onclose={() => dismissToast(t.id)}
			/>
		</div>
	{/each}
</div>
