<script lang="ts">
	type Variant = 'success' | 'error' | 'warning' | 'info';

	interface Props {
		variant?: Variant;
		title: string;
		detail?: string;
		onclose?: () => void;
	}

	let { variant = 'info', title, detail, onclose }: Props = $props();

	const GLYPH: Record<Variant, string> = {
		success: '●',
		error: '✕',
		warning: '▲',
		info: '○'
	};
	const GLYPH_COLOR: Record<Variant, string> = {
		success: 'text-ok',
		error: 'text-err',
		warning: 'text-warn',
		info: 'text-accent-text'
	};
</script>

<div
	class="bk-toast box-border flex w-[360px] items-start gap-[10px] rounded-2 border border-border-1 bg-bg-2 px-3 py-[10px] shadow-[var(--shadow-overlay)] animate-[ds-slide-up_var(--dur-3)_var(--ease)]"
	role="status"
	aria-live="polite"
>
	<span class="bk-toast-glyph flex-none text-[11px] leading-[18px] {GLYPH_COLOR[variant]}">
		{GLYPH[variant]}
	</span>
	<div class="min-w-0 flex-1">
		<div class="bk-toast-title text-base font-medium leading-[18px] text-text-1">{title}</div>
		{#if detail}
			<div class="bk-toast-detail mt-0.5 text-sm text-text-2">{detail}</div>
		{/if}
	</div>
	{#if onclose}
		<button
			type="button"
			class="bk-toast-x ml-auto cursor-pointer border-0 bg-transparent px-0.5 leading-[18px] text-text-3 hover:text-text-1"
			aria-label="Dismiss"
			onclick={onclose}
		>
			✕
		</button>
	{/if}
</div>
