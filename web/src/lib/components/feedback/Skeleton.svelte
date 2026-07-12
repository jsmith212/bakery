<script lang="ts">
	interface Props {
		width?: string | number;
		height?: string | number;
	}

	let { width, height }: Props = $props();

	const dim = (v: string | number | undefined) => (typeof v === 'number' ? `${v}px` : v);

	let style = $derived(
		[
			width != null ? `width:${dim(width)}` : null,
			height != null ? `height:${dim(height)}` : null
		]
			.filter(Boolean)
			.join(';')
	);
</script>

<div
	class="bk-skeleton rounded-1 bg-bg-3 animate-[ds-shimmer_1.6s_var(--ease)_infinite]"
	style={style || undefined}
	aria-hidden="true"
></div>
