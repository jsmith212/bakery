<script lang="ts">
	interface SparklineProps {
		data: number[];
		width?: number;
		height?: number;
		color?: string;
		class?: string;
	}

	let {
		data,
		width = 160,
		height = 24,
		color = 'var(--chart-hit)',
		class: klass = ''
	}: SparklineProps = $props();

	function buildPath(values: number[], w: number, h: number, area: boolean): string {
		const n = values.length;
		if (n === 0) return '';
		const padL = w > 200 ? 36 : 2;
		const padR = w > 200 ? 8 : 2;
		const padT = w > 200 ? 8 : 2;
		const padB = w > 200 ? 18 : 2;
		const max = Math.max(...values, 0) || 1;
		const pts = values.map((v, i) => [
			n === 1 ? padL : padL + (i * (w - padL - padR)) / (n - 1),
			h - padB - (Math.max(0, v) / max) * (h - padT - padB)
		]);
		let d = 'M' + pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' L');
		if (area) d += ` L${w - padR},${h - padB} L${padL},${h - padB} Z`;
		return d;
	}

	const linePath = $derived(buildPath(data, width, height, false));
	const areaPath = $derived(buildPath(data, width, height, true));
</script>

<svg
	width="100%"
	{height}
	viewBox="0 0 {width} {height}"
	preserveAspectRatio="none"
	aria-hidden="true"
	class={klass}
>
	<path
		d={areaPath}
		stroke="none"
		style="fill: {color}; opacity: var(--chart-fill-alpha);"
	/>
	<path
		d={linePath}
		fill="none"
		stroke-width="1.5"
		stroke-linejoin="round"
		style="stroke: {color};"
	/>
</svg>
