<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		title?: string;
		onclose?: () => void;
		width?: string;
		showClose?: boolean;
		dismissible?: boolean;
		children?: Snippet;
		footer?: Snippet;
	}

	let {
		title,
		onclose,
		width,
		showClose = true,
		dismissible = true,
		children,
		footer
	}: Props = $props();

	const titleId = `bk-modal-title-${Math.random().toString(36).slice(2, 9)}`;

	let modalEl = $state<HTMLDivElement | null>(null);

	function close() {
		onclose?.();
	}

	function onKeydown(e: KeyboardEvent) {
		if (dismissible && e.key === 'Escape') {
			e.stopPropagation();
			close();
		}
	}

	function onScrimMouseDown(e: MouseEvent) {
		if (dismissible && e.target === e.currentTarget) {
			close();
		}
	}

	$effect(() => {
		const previous = document.activeElement as HTMLElement | null;
		modalEl?.focus();
		return () => previous?.focus?.();
	});
</script>

<svelte:window onkeydown={onKeydown} />

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
	class="bk-scrim fixed inset-0 z-50 grid place-items-center bg-bg-overlay animate-[ds-fade-in_var(--dur-3)_var(--ease)]"
	role="presentation"
	onmousedown={onScrimMouseDown}
>
	<div
		bind:this={modalEl}
		class="bk-modal box-border w-[min(480px,calc(100vw-32px))] rounded-2 border border-border-1 bg-bg-1 shadow-[var(--shadow-overlay)] animate-[ds-slide-up_var(--dur-3)_var(--ease)]"
		style={width ? `width:${width}` : undefined}
		role="dialog"
		aria-modal="true"
		aria-labelledby={title ? titleId : undefined}
		tabindex="-1"
	>
		{#if title || (showClose && onclose)}
			<div
				class="bk-modal-header flex items-center justify-between border-b border-border-0 px-4 py-3"
			>
				{#if title}
					<h2 id={titleId} class="bk-modal-title m-0 text-md font-semibold text-text-1">
						{title}
					</h2>
				{/if}
				{#if showClose && onclose}
					<button
						type="button"
						class="bk-modal-x cursor-pointer rounded-1 border-0 bg-transparent p-1 text-[14px] leading-none text-text-3 hover:bg-bg-3 hover:text-text-1"
						aria-label="Close"
						onclick={close}
					>
						✕
					</button>
				{/if}
			</div>
		{/if}

		<div class="bk-modal-body p-4 text-base text-text-2">
			{@render children?.()}
		</div>

		{#if footer}
			<div
				class="bk-modal-footer flex justify-end gap-2 border-t border-border-0 px-4 py-3"
			>
				{@render footer()}
			</div>
		{/if}
	</div>
</div>
