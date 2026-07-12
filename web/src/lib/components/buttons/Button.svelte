<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	type Variant = 'primary' | 'secondary' | 'ghost' | 'danger';
	type Size = 'sm' | 'md' | 'lg';

	interface Props extends Omit<HTMLButtonAttributes, 'class'> {
		variant?: Variant;
		size?: Size;
		disabled?: boolean;
		class?: string;
		children?: Snippet;
	}

	let {
		variant = 'primary',
		size = 'md',
		disabled = false,
		type = 'button',
		class: className = '',
		children,
		...rest
	}: Props = $props();

	const base =
		'inline-flex items-center justify-center gap-1.5 font-sans font-medium border border-transparent rounded-1 cursor-pointer whitespace-nowrap select-none transition-colors duration-[var(--dur-2)] ease-[var(--ease)] disabled:cursor-not-allowed disabled:bg-bg-2 disabled:text-text-disabled disabled:border-border-0';

	const sizes: Record<Size, string> = {
		sm: 'h-[var(--control-sm)] px-2 text-sm',
		md: 'h-[var(--control-md)] px-[10px] text-base',
		lg: 'h-[var(--control-lg)] px-3 text-base'
	};

	const variants: Record<Variant, string> = {
		primary:
			'bg-accent-solid text-text-on-solid enabled:hover:bg-accent-solid-hover enabled:active:bg-accent-solid-active',
		secondary:
			'bg-bg-2 text-text-1 border-border-1 enabled:hover:bg-bg-3 enabled:hover:border-border-2 enabled:active:bg-bg-3',
		ghost:
			'bg-transparent text-text-2 enabled:hover:bg-bg-3 enabled:hover:text-text-1 enabled:active:bg-bg-3',
		danger:
			'bg-err-solid text-text-on-solid enabled:hover:bg-err-solid-hover enabled:active:bg-err-solid'
	};
</script>

<button
	{type}
	{disabled}
	class="{base} {sizes[size]} {variants[variant]} {className}"
	{...rest}
>
	{@render children?.()}
</button>
