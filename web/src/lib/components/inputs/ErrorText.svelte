<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLAttributes } from 'svelte/elements';

	interface Props extends HTMLAttributes<HTMLParagraphElement> {
		children?: Snippet;
	}

	let { children, ...rest }: Props = $props();
</script>

<p role="alert" {...rest} class="text-sm text-err">
	{@render children?.()}
</p>
