<script lang="ts">
	import type { Snippet } from 'svelte';
	import { bkTableWrap } from './classes';

	let {
		children,
		class: klass = ''
	}: { children: Snippet; class?: string } = $props();
</script>

<div class="{bkTableWrap} {klass}">
	{@render children()}
</div>
