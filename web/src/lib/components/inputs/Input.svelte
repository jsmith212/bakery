<script lang="ts">
	import type { HTMLInputAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLInputAttributes, 'size' | 'value'> {
		size?: 'sm' | 'md' | 'lg';
		mono?: boolean;
		error?: boolean;
		value?: string;
	}

	let {
		size = 'md',
		mono = false,
		error = false,
		value = $bindable(''),
		...rest
	}: Props = $props();

	const heights = {
		sm: 'h-[var(--control-sm)]',
		md: 'h-[var(--control-md)]',
		lg: 'h-[var(--control-lg)]'
	} as const;
	const fonts = { sm: 'text-sm', md: 'text-base', lg: 'text-base' } as const;
</script>

<input
	{...rest}
	class={[
		mono ? 'font-mono text-sm' : `font-sans ${fonts[size]}`,
		heights[size],
		'box-border w-full rounded-1 px-2',
		'bg-bg-1 text-text-1 placeholder:text-text-3',
		'border',
		error ? 'border-err' : 'border-border-1',
		'transition-colors duration-[var(--dur-2)] ease-[var(--ease)]',
		'hover:not-disabled:not-focus:border-border-2',
		error ? 'focus:border-err' : 'focus:border-accent-border',
		'disabled:cursor-not-allowed disabled:bg-bg-2 disabled:text-text-disabled'
	]}
	bind:value
/>
