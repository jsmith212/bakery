<script lang="ts" module>
	export type DeltaKind = 'up' | 'down' | 'flat';
</script>

<script lang="ts">
	import Sparkline from './Sparkline.svelte';

	interface StatTileProps {
		label: string;
		value: string | number;
		unit?: string;
		delta?: string;
		deltaKind?: DeltaKind;
		deltaGood?: boolean;
		spark?: number[];
		sparkColor?: string;
		class?: string;
	}

	let {
		label,
		value,
		unit,
		delta,
		deltaKind,
		deltaGood,
		spark,
		sparkColor,
		class: klass = ''
	}: StatTileProps = $props();

	const deltaClasses: Record<DeltaKind, string> = {
		up: 'text-ok',
		down: 'text-err',
		flat: 'text-text-3'
	};

	const resolvedDelta = $derived<DeltaKind>(
		deltaKind ?? (deltaGood === true ? 'up' : deltaGood === false ? 'down' : 'flat')
	);
</script>

<div
	class="flex min-w-0 flex-col gap-1 rounded-2 border border-border-0 bg-bg-1 px-[14px] py-3 font-sans {klass}"
>
	<div class="text-xs font-medium tracking-[var(--tracking-label)] text-text-3 uppercase">
		{label}
	</div>
	<div class="flex flex-wrap items-baseline gap-2">
		<span class="tabular text-metric font-semibold text-text-1">{value}</span>
		{#if unit}
			<span class="text-sm text-text-3">{unit}</span>
		{/if}
		{#if delta}
			<span class="tabular-nums text-xs font-medium whitespace-nowrap {deltaClasses[resolvedDelta]}">
				{delta}
			</span>
		{/if}
	</div>
	{#if spark && spark.length}
		<div class="mt-1">
			<Sparkline data={spark} color={sparkColor} />
		</div>
	{/if}
</div>
