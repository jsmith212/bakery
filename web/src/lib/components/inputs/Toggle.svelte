<script lang="ts">
	import type { HTMLButtonAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLButtonAttributes, 'onchange'> {
		checked?: boolean;
		onchange?: (checked: boolean) => void;
	}

	let { checked = $bindable(false), disabled = false, onchange, ...rest }: Props = $props();

	function toggle() {
		checked = !checked;
		onchange?.(checked);
	}
</script>

<button
	{...rest}
	type="button"
	role="switch"
	aria-checked={checked}
	{disabled}
	onclick={toggle}
	class={[
		'bk-toggle-btn relative h-[18px] w-8 flex-none cursor-pointer rounded-full p-0',
		'border border-border-1 bg-bg-3',
		'transition-colors duration-[var(--dur-2)] ease-[var(--ease)]',
		'hover:not-disabled:border-border-2',
		'aria-checked:not-disabled:border-accent-solid aria-checked:not-disabled:bg-accent-solid',
		'disabled:cursor-not-allowed disabled:border-border-0 disabled:bg-bg-2'
	]}
></button>

<style>
	.bk-toggle-btn::after {
		content: '';
		position: absolute;
		top: 2px;
		left: 2px;
		width: 12px;
		height: 12px;
		border-radius: var(--radius-full);
		background: var(--text-2);
		transition:
			transform var(--dur-2) var(--ease),
			background var(--dur-2) var(--ease);
	}
	.bk-toggle-btn[aria-checked='true']::after {
		transform: translateX(14px);
		background: #ffffff;
	}
	.bk-toggle-btn:disabled::after {
		background: var(--text-disabled);
	}
</style>
