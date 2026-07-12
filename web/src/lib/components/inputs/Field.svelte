<script lang="ts">
	import type { Snippet } from 'svelte';
	import Label from './Label.svelte';
	import Hint from './Hint.svelte';
	import ErrorText from './ErrorText.svelte';

	interface Props {
		label?: string;
		hint?: string;
		error?: string;
		for?: string;
		children?: Snippet;
	}

	let { label, hint, error, for: forId, children }: Props = $props();
</script>

<div class="flex flex-col gap-1">
	{#if label}<Label for={forId}>{label}</Label>{/if}
	{@render children?.()}
	{#if error}<ErrorText>{error}</ErrorText>{:else if hint}<Hint>{hint}</Hint>{/if}
</div>
