<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLSelectAttributes } from 'svelte/elements';

	interface Option {
		value: string;
		label: string;
		disabled?: boolean;
	}

	interface Props extends Omit<HTMLSelectAttributes, 'size' | 'value'> {
		size?: 'sm' | 'md' | 'lg';
		error?: boolean;
		options?: Option[];
		value?: string;
		children?: Snippet;
	}

	let {
		size = 'md',
		error = false,
		options,
		value = $bindable(''),
		children,
		...rest
	}: Props = $props();

	const heights = {
		sm: 'h-[var(--control-sm)]',
		md: 'h-[var(--control-md)]',
		lg: 'h-[var(--control-lg)]'
	} as const;
	const fonts = { sm: 'text-sm', md: 'text-base', lg: 'text-base' } as const;
</script>

<div class="bk-select-wrap relative inline-flex w-full">
	<select
		{...rest}
		class={[
			'w-full cursor-pointer appearance-none font-sans',
			fonts[size],
			heights[size],
			'rounded-1 pr-6 pl-2',
			'bg-bg-1 text-text-1',
			'border',
			error ? 'border-err' : 'border-border-1',
			'transition-colors duration-[var(--dur-2)] ease-[var(--ease)]',
			'hover:not-disabled:not-focus:border-border-2',
			'focus:border-accent-border',
			'disabled:cursor-not-allowed disabled:bg-bg-2 disabled:text-text-disabled'
		]}
		bind:value
	>
		{#if options}
			{#each options as opt (opt.value)}
				<option value={opt.value} disabled={opt.disabled}>{opt.label}</option>
			{/each}
		{:else}
			{@render children?.()}
		{/if}
	</select>
</div>

<style>
	.bk-select-wrap::after {
		content: '';
		position: absolute;
		top: 50%;
		right: 10px;
		width: 6px;
		height: 6px;
		margin-top: -5px;
		border-right: 1.5px solid var(--text-3);
		border-bottom: 1.5px solid var(--text-3);
		transform: rotate(45deg);
		pointer-events: none;
	}
</style>
