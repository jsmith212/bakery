<script lang="ts">
	interface Pair {
		key: string;
		value: string;
		mono?: boolean;
		/**
		 * Hover text for the VALUE. It exists for one shape: a value rendered
		 * relatively ("measured 3 min ago") whose exact form is still worth having
		 * without spending a row on it.
		 */
		title?: string;
	}

	interface Props {
		pairs: Pair[];
	}

	let { pairs }: Props = $props();
</script>

<div class="grid grid-cols-[max-content_1fr] gap-x-6 text-sm">
	{#each pairs as pair, i (i)}
		{@const border = i === pairs.length - 1 ? '' : 'border-b border-border-0'}
		<div class="flex min-h-[18px] items-center py-1.5 text-text-3 {border}">{pair.key}</div>
		<div
			class="flex min-h-[18px] items-center py-1.5 {border} {pair.mono
				? 'font-mono text-[length:var(--mono-xs)] text-text-2'
				: 'text-text-1'}"
			title={pair.title}
		>
			{pair.value}
		</div>
	{/each}
</div>
