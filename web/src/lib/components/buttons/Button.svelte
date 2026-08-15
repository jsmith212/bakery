<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLAnchorAttributes, HTMLButtonAttributes } from 'svelte/elements';

	type Variant = 'primary' | 'secondary' | 'ghost' | 'danger';
	type Size = 'sm' | 'md' | 'lg';

	interface Props extends Omit<HTMLButtonAttributes, 'class'> {
		variant?: Variant;
		size?: Size;
		disabled?: boolean;
		class?: string;
		/**
		 * Renders an `<a>` instead of a `<button>`, keeping every variant/size
		 * class. Exists because `<a href><Button/></a>` -- a button nested inside
		 * a link -- is invalid HTML (two interactive elements, two tab stops) and
		 * silently ignores `disabled` (an anchor has no such attribute). An
		 * anchor has no `:disabled`/`:enabled` pseudo-classes either, so the
		 * disabled look is applied as plain classes here rather than reusing the
		 * button branch's `disabled:*` variants, and the element is made
		 * genuinely inert -- unfocusable (`tabindex=-1`, no `href`) and
		 * unclickable (`aria-disabled` plus a click guard, which covers
		 * Enter-activation too, not just a mouse click).
		 */
		href?: string;
		children?: Snippet;
	}

	let {
		variant = 'primary',
		size = 'md',
		disabled = false,
		type = 'button',
		class: className = '',
		href,
		children,
		onclick,
		...rest
	}: Props = $props();

	const base =
		'inline-flex items-center justify-center gap-1.5 font-sans font-medium border border-transparent rounded-1 cursor-pointer whitespace-nowrap select-none no-underline transition-colors duration-[var(--dur-2)] ease-[var(--ease)]';

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

	// Mirrors the button branch's `disabled:*` classes -- an anchor has no
	// `:disabled` pseudo-class for those to hook, so the same look is applied
	// unconditionally here instead.
	const anchorDisabled = 'pointer-events-none cursor-not-allowed bg-bg-2 text-text-disabled border-border-0';

	function guardedClick(e: MouseEvent) {
		if (disabled) {
			e.preventDefault();
			return;
		}
		(onclick as ((e: MouseEvent) => void) | undefined)?.(e);
	}
</script>

{#if href !== undefined}
	<!-- {...rest} spreads on BOTH branches: an aria-label, title, or data-* passed
	     alongside href must not silently vanish. Explicit attributes come after the
	     spread so the disabled treatment cannot be clobbered by a caller. -->
	<a
		{...rest as HTMLAnchorAttributes}
		href={disabled ? undefined : href}
		aria-disabled={disabled || undefined}
		tabindex={disabled ? -1 : undefined}
		class="{base} {sizes[size]} {disabled ? anchorDisabled : variants[variant]} {className}"
		onclick={guardedClick}
	>
		{@render children?.()}
	</a>
{:else}
	<button
		{type}
		{disabled}
		class="{base} disabled:cursor-not-allowed disabled:bg-bg-2 disabled:text-text-disabled disabled:border-border-0 {sizes[
			size
		]} {variants[variant]} {className}"
		{onclick}
		{...rest}
	>
		{@render children?.()}
	</button>
{/if}
