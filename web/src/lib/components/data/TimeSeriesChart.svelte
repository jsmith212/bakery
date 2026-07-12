<script lang="ts" module>
	export interface ChartSeries {
		label: string;
		color: string;
		data: number[];
	}
</script>

<script lang="ts">
	interface TimeSeriesChartProps {
		series: ChartSeries[];
		yMax: number;
		yFormat?: (v: number) => string;
		xLabels: string[];
		width?: number;
		height?: number;
		class?: string;
	}

	let {
		series,
		yMax,
		yFormat = (v: number) => String(v),
		xLabels,
		width = 640,
		height = 150,
		class: klass = ''
	}: TimeSeriesChartProps = $props();

	const PAD = { l: 36, r: 8, t: 8, b: 20 };

	const baseline = $derived(height - PAD.b);
	const topY = $derived(PAD.t);
	const midY = $derived((PAD.t + height - PAD.b) / 2);
	const rightX = $derived(width - PAD.r);

	function buildPath(data: number[], max: number, area: boolean): string {
		const n = data.length;
		if (n === 0) return '';
		const m = max <= 0 ? 1 : max;
		const usableW = width - PAD.l - PAD.r;
		const usableH = height - PAD.t - PAD.b;
		const base = height - PAD.b;
		const pts = data.map((v, i) => [
			n === 1 ? PAD.l : PAD.l + (i * usableW) / (n - 1),
			base - (Math.max(0, v) / m) * usableH
		]);
		let d = 'M' + pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' L');
		if (area) d += ` L${width - PAD.r},${base} L${PAD.l},${base} Z`;
		return d;
	}

	const areaPath = $derived(series[0] ? buildPath(series[0].data, yMax, true) : '');
	const lines = $derived(series.map((s) => ({ color: s.color, d: buildPath(s.data, yMax, false) })));
	const xFirst = $derived(xLabels[0] ?? '');
	const xLast = $derived(xLabels.length > 1 ? xLabels[xLabels.length - 1] : '');
</script>

<div class="font-sans {klass}">
	<div class="mb-2 flex flex-wrap gap-4">
		{#each series as s, i (i)}
			<span class="inline-flex items-center gap-1.5 text-xs text-text-2">
				<span class="h-2 w-2 flex-none rounded-[2px]" style="background: {s.color};"></span>
				{s.label}
			</span>
		{/each}
	</div>
	<svg viewBox="0 0 {width} {height}" class="block w-full">
		<line x1={PAD.l} x2={rightX} y1={baseline} y2={baseline} class="stroke-chart-grid" />
		<line x1={PAD.l} x2={rightX} y1={midY} y2={midY} class="stroke-chart-grid" />
		<line x1={PAD.l} x2={rightX} y1={topY} y2={topY} class="stroke-chart-grid" />

		<text x={PAD.l - 6} y={baseline + 3} text-anchor="end" class="fill-chart-axis font-mono text-[10px]">
			{yFormat(0)}
		</text>
		<text x={PAD.l - 6} y={midY + 3} text-anchor="end" class="fill-chart-axis font-mono text-[10px]">
			{yFormat(yMax / 2)}
		</text>
		<text x={PAD.l - 6} y={topY + 3} text-anchor="end" class="fill-chart-axis font-mono text-[10px]">
			{yFormat(yMax)}
		</text>

		<text x={PAD.l} y={height - 4} class="fill-chart-axis font-mono text-[10px]">{xFirst}</text>
		<text x={rightX} y={height - 4} text-anchor="end" class="fill-chart-axis font-mono text-[10px]">
			{xLast}
		</text>

		{#if areaPath && series[0]}
			<path d={areaPath} stroke="none" style="fill: {series[0].color}; opacity: var(--chart-fill-alpha);" />
		{/if}
		{#each lines as line, i (i)}
			<path d={line.d} fill="none" stroke-width="1.5" stroke-linejoin="round" style="stroke: {line.color};" />
		{/each}
	</svg>
</div>
