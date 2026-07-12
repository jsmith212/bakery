<script lang="ts">
	import type { Snippet } from 'svelte';
	import { bkTr } from './classes';

	let {
		children,
		class: klass = '',
		...rest
	}: { children: Snippet; class?: string } & Record<string, unknown> = $props();
</script>

<tr class="{bkTr} last:[&>td]:border-b-0 {klass}" {...rest}>
	{@render children()}
</tr>
