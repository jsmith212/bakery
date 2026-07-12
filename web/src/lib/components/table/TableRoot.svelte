<script lang="ts">
	import type { Snippet } from 'svelte';
	import { setContext } from 'svelte';
	import { bkTable } from './classes';

	let {
		dense = false,
		children,
		class: klass = ''
	}: { dense?: boolean; children: Snippet; class?: string } = $props();

	setContext<() => boolean>('bk-table-dense', () => dense);
</script>

<table class="{bkTable} {klass}">
	{@render children()}
</table>
