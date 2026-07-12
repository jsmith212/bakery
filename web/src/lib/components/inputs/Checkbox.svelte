<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLInputAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLInputAttributes, 'type' | 'checked' | 'value'> {
		checked?: boolean;
		label?: string;
		children?: Snippet;
	}

	let { checked = $bindable(false), disabled = false, label, children, ...rest }: Props = $props();
</script>

<label
	class={[
		'inline-flex items-center gap-2 text-base',
		disabled ? 'cursor-not-allowed text-text-disabled' : 'cursor-pointer text-text-1'
	]}
>
	<input
		{...rest}
		type="checkbox"
		{disabled}
		class={[
			'bk-checkbox-box m-0 grid h-[14px] w-[14px] flex-none cursor-pointer appearance-none place-content-center rounded-[3px]',
			'border border-border-1 bg-bg-1',
			'transition-colors duration-[var(--dur-2)] ease-[var(--ease)]',
			'hover:not-disabled:border-border-2',
			'checked:not-disabled:border-accent-solid checked:not-disabled:bg-accent-solid',
			'disabled:cursor-not-allowed disabled:border-border-0 disabled:bg-bg-2',
			'disabled:checked:bg-border-1'
		]}
		bind:checked
	/>
	{#if children}{@render children()}{:else if label}<span>{label}</span>{/if}
</label>

<style>
	.bk-checkbox-box:checked::before {
		content: '';
		width: 8px;
		height: 8px;
		background: #ffffff;
		clip-path: polygon(14% 46%, 0% 62%, 38% 100%, 100% 18%, 84% 4%, 37% 68%);
	}
</style>
