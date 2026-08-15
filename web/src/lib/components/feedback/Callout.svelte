<script lang="ts">
	import type { Snippet } from 'svelte';

	type Variant = 'warn' | 'error' | 'info';

	interface Props {
		variant?: Variant;
		/**
		 * An optional bold lead clause, inline with the body -- not a stacked
		 * heading. Every call site this replaced wrote a `<span
		 * class="font-semibold">lead.</span><span class="text-text-2"> rest</span>`
		 * pair inside one prose block; `title` reproduces the lead half so a
		 * caller with plain-string emphasis doesn't have to hand-write the span,
		 * while a caller whose lead sentence carries its own markup (a `font-mono`
		 * value, a link) still builds it inline in `children`, same as before.
		 */
		title?: string;
		children?: Snippet;
	}

	let { variant = 'info', title, children }: Props = $props();

	// Same glyph/color pairing as `Toast`, so a warning reads the same whether it
	// lands as a toast or sits in place on the page.
	const GLYPH: Record<Variant, string> = { warn: '▲', error: '✕', info: '○' };
	const GLYPH_COLOR: Record<Variant, string> = {
		warn: 'text-warn',
		error: 'text-err',
		info: 'text-accent-text'
	};
	const FRAME: Record<Variant, string> = {
		warn: 'border-warn-border bg-warn-muted',
		error: 'border-err-border bg-err-muted',
		info: 'border-border-0 bg-bg-2'
	};
</script>

<div class="flex items-start gap-2.5 rounded-2 border {FRAME[variant]} px-3 py-2.5">
	<span class="text-xs leading-[18px] {GLYPH_COLOR[variant]}" aria-hidden="true">{GLYPH[variant]}</span>
	<div class="min-w-0 text-sm leading-[18px] text-text-1">
		{#if title}<span class="font-semibold">{title}</span>{/if}{@render children?.()}
	</div>
</div>
