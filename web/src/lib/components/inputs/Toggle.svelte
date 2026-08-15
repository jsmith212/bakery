<script lang="ts">
	import type { HTMLButtonAttributes } from 'svelte/elements';

	interface Props extends Omit<HTMLButtonAttributes, 'onchange'> {
		checked?: boolean;
		/**
		 * `role="switch"` is not a native form control, so a wrapping `<label>`
		 * gives it nothing -- no implicit label-for, no click-forwarding. This
		 * mints an id for an adjacent text span and wires `aria-labelledby` at
		 * it, the same association `Field` builds for text inputs. Omit it only
		 * when the caller already renders an accessible name some other way
		 * (none of this codebase's call sites do).
		 */
		label?: string;
		onchange?: (checked: boolean) => void;
	}

	let { checked = $bindable(false), disabled = false, label, onchange, ...rest }: Props = $props();

	const uid = $props.id();
	const labelId = `${uid}-label`;

	function toggle() {
		checked = !checked;
		onchange?.(checked);
	}
</script>

<span class="inline-flex items-center gap-2">
	<button
		{...rest}
		type="button"
		role="switch"
		aria-checked={checked}
		aria-labelledby={label ? labelId : undefined}
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
	{#if label}
		<span id={labelId} class={disabled ? 'text-base text-text-disabled' : 'text-base text-text-1'}
			>{label}</span
		>
	{/if}
</span>

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
