<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLAttributes } from 'svelte/elements';

	interface Props extends HTMLAttributes<HTMLParagraphElement> {
		children?: Snippet;
	}

	let { children, ...rest }: Props = $props();
</script>

<p {...rest} class="text-sm text-text-3">
	{@render children?.()}
</p>
