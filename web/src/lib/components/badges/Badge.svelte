<script lang="ts" module>
	export type BadgeStatus = 'hit' | 'miss' | 'stale' | 'idle';
	export type BadgeVariant = 'type' | 'accent';
</script>

<script lang="ts">
	import type { Snippet } from 'svelte';

	interface BadgeProps {
		status?: BadgeStatus;
		variant?: BadgeVariant;
		glyph?: boolean;
		class?: string;
		children?: Snippet;
	}

	let { status, variant, glyph = true, class: klass = '', children }: BadgeProps = $props();

	const statusGlyph: Record<BadgeStatus, string> = {
		hit: '●',
		miss: '✕',
		stale: '▲',
		idle: '○'
	};

	const statusClasses: Record<BadgeStatus, string> = {
		hit: 'font-sans font-medium text-ok bg-ok-muted border-ok-border',
		miss: 'font-sans font-medium text-err bg-err-muted border-err-border',
		stale: 'font-sans font-medium text-warn bg-warn-muted border-warn-border',
		idle: 'font-sans font-medium text-idle bg-idle-muted border-idle-border'
	};

	const variantClasses: Record<BadgeVariant, string> = {
		type: 'font-mono font-normal text-text-2 bg-bg-2 border-border-1',
		accent: 'font-sans font-medium text-accent-text bg-accent-muted border-accent-border'
	};

	const modifier = $derived(
		status ? statusClasses[status] : variant ? variantClasses[variant] : variantClasses.accent
	);
</script>

<span
	class="inline-flex h-[18px] items-center gap-1 whitespace-nowrap rounded-1 border px-1.5 text-xs/[1] {modifier} {klass}"
>
	{#if status && glyph}
		<span class="text-[9px] leading-none" aria-hidden="true">{statusGlyph[status]}</span>
	{/if}
	{@render children?.()}
</span>
