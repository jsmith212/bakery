<script lang="ts">
	import type { Snippet } from 'svelte';
	import { getContext } from 'svelte';
	import { bkTd, bkTdDense, bkTdMono, bkTdNum } from './classes';

	let {
		mono = false,
		num = false,
		children,
		class: klass = '',
		...rest
	}: {
		mono?: boolean;
		num?: boolean;
		children?: Snippet;
		class?: string;
	} & Record<string, unknown> = $props();

	const denseGetter = getContext<(() => boolean) | undefined>('bk-table-dense');
	const dense = $derived(denseGetter ? denseGetter() : false);
</script>

<td
	class="{dense ? bkTdDense : bkTd} {mono ? bkTdMono : ''} {num ? bkTdNum : ''} {klass}"
	{...rest}
>
	{@render children?.()}
</td>
