<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLLabelAttributes } from 'svelte/elements';

	interface Props extends HTMLLabelAttributes {
		children?: Snippet;
	}

	let { children, ...rest }: Props = $props();
</script>

<label {...rest} class="text-sm font-medium text-text-2">
	{@render children?.()}
</label>
