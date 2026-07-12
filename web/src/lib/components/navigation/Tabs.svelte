<script lang="ts" module>
	export interface Tab {
		id: string;
		label: string;
		count?: number;
		disabled?: boolean;
	}
</script>

<script lang="ts">
	interface Props {
		tabs: Tab[];
		active: string;
		onChange?: (id: string) => void;
		class?: string;
	}

	let { tabs, active, onChange, class: className = '' }: Props = $props();
</script>

<div role="tablist" class="flex gap-0.5 border-b border-border-0 {className}">
	{#each tabs as tab (tab.id)}
		<button
			type="button"
			role="tab"
			aria-selected={tab.id === active}
			disabled={tab.disabled}
			onclick={() => onChange?.(tab.id)}
			class="-mb-px inline-flex h-8 cursor-pointer items-center gap-1.5 border-b-2 border-transparent bg-transparent px-[10px] font-sans text-base text-text-2 transition-[color] duration-[var(--dur-2)] ease-[var(--ease)] hover:text-text-1 disabled:cursor-not-allowed disabled:text-text-disabled aria-selected:border-accent-text aria-selected:text-text-1"
		>
			{tab.label}
			{#if tab.count !== undefined}
				<span
					class="tabular rounded-1 border border-border-0 bg-bg-2 px-1 py-px text-xs text-text-3"
					>{tab.count}</span
				>
			{/if}
		</button>
	{/each}
</div>
