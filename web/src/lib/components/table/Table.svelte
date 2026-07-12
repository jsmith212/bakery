<script lang="ts">
	import type { Snippet } from 'svelte';
	import {
		bkTableWrap,
		bkTable,
		bkTh,
		bkThNum,
		bkTr,
		bkTd,
		bkTdDense,
		bkTdMono,
		bkTdNum,
		bkThSort,
		bkSortArrow
	} from './classes';

	type Row = Record<string, unknown>;

	interface Column {
		key: string;
		label: string;
		mono?: boolean;
		num?: boolean;
		width?: string;
		sortable?: boolean;
		render?: Snippet<[Row]>;
	}

	let {
		columns,
		rows,
		dense = false
	}: { columns: Column[]; rows: Row[]; dense?: boolean } = $props();

	let sortKey = $state<string | null>(null);
	let sortDir = $state<'asc' | 'desc'>('asc');

	function toggleSort(key: string) {
		if (sortKey === key) {
			sortDir = sortDir === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			sortDir = 'asc';
		}
	}

	const sortedRows = $derived.by(() => {
		if (!sortKey) return rows;
		const key = sortKey;
		const dir = sortDir === 'asc' ? 1 : -1;
		return [...rows].sort((a, b) => {
			const av = a[key];
			const bv = b[key];
			if (typeof av === 'number' && typeof bv === 'number') return (av - bv) * dir;
			return String(av ?? '').localeCompare(String(bv ?? '')) * dir;
		});
	});
</script>

<div class={bkTableWrap}>
	<table class={bkTable}>
		<thead>
			<tr>
				{#each columns as col (col.key)}
					<th
						class={col.num ? bkThNum : bkTh}
						style={col.width ? `width:${col.width}` : undefined}
						aria-sort={col.sortable
							? sortKey === col.key
								? sortDir === 'asc'
									? 'ascending'
									: 'descending'
								: 'none'
							: undefined}
					>
						{#if col.sortable}
							<button type="button" class={bkThSort} onclick={() => toggleSort(col.key)}>
								{col.label}
								{#if sortKey === col.key}
									<span class={bkSortArrow}>{sortDir === 'asc' ? '▲' : '▼'}</span>
								{/if}
							</button>
						{:else}
							{col.label}
						{/if}
					</th>
				{/each}
			</tr>
		</thead>
		<tbody>
			{#each sortedRows as row, i (i)}
				<tr class="{bkTr} {i === sortedRows.length - 1 ? '[&>td]:border-b-0' : ''}">
					{#each columns as col (col.key)}
						<td
							class="{dense ? bkTdDense : bkTd} {col.mono ? bkTdMono : ''} {col.num ? bkTdNum : ''}"
						>
							{#if col.render}{@render col.render(row)}{:else}{row[col.key]}{/if}
						</td>
					{/each}
				</tr>
			{/each}
		</tbody>
	</table>
</div>
