<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		glyph?: string;
		title: string;
		desc?: string;
		action?: Snippet;
	}

	let { glyph = '∅', title, desc, action }: Props = $props();
</script>

<div
	class="bk-empty flex flex-col items-center justify-center gap-1 rounded-2 border border-dashed border-border-1 px-6 py-8 text-center text-text-2"
>
	<div class="bk-empty-glyph mb-1 font-mono text-[16px] text-text-3">{glyph}</div>
	<div class="bk-empty-title text-base font-medium text-text-1">{title}</div>
	{#if desc}
		<div class="bk-empty-desc max-w-[380px] text-sm text-text-2">{desc}</div>
	{/if}
	{#if action}
		<div class="bk-empty-action mt-3">{@render action()}</div>
	{/if}
</div>
