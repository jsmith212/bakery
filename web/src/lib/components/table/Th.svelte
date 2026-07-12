<script lang="ts">
	import type { Snippet } from 'svelte';
	import { bkTh, bkThNum, bkThSort, bkSortArrow } from './classes';

	let {
		num = false,
		sortable = false,
		sortDir = null,
		onsort,
		children,
		class: klass = '',
		...rest
	}: {
		num?: boolean;
		sortable?: boolean;
		sortDir?: 'asc' | 'desc' | null;
		onsort?: () => void;
		children?: Snippet;
		class?: string;
	} & Record<string, unknown> = $props();
</script>

<th
	class="{num ? bkThNum : bkTh} {klass}"
	aria-sort={sortable
		? sortDir === 'asc'
			? 'ascending'
			: sortDir === 'desc'
				? 'descending'
				: 'none'
		: undefined}
	{...rest}
>
	{#if sortable}
		<button type="button" class={bkThSort} onclick={onsort}>
			{@render children?.()}
			{#if sortDir}
				<span class={bkSortArrow}>{sortDir === 'asc' ? '▲' : '▼'}</span>
			{/if}
		</button>
	{:else}
		{@render children?.()}
	{/if}
</th>
