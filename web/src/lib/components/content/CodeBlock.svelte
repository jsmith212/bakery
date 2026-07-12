<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		title?: string;
		code?: string;
		copyText?: string;
		copyable?: boolean;
		copyLabel?: string;
		children?: Snippet;
	}

	let { title, code, copyText, copyable = true, copyLabel = 'Copy', children }: Props = $props();

	let preEl = $state<HTMLPreElement>();
	let copied = $state(false);
	let timer: ReturnType<typeof setTimeout>;

	function copy() {
		const text = copyText ?? code ?? preEl?.textContent ?? '';
		try {
			navigator.clipboard?.writeText(text);
		} catch {
			/* clipboard unavailable */
		}
		copied = true;
		clearTimeout(timer);
		timer = setTimeout(() => {
			copied = false;
		}, 1500);
	}
</script>

<div
	class="overflow-hidden rounded-2 border border-border-0 bg-bg-inset font-mono text-[length:var(--mono-sm)] leading-[var(--mono-leading-sm)]"
>
	{#if title || copyable}
		<div class="flex items-center justify-between border-b border-border-0 bg-bg-2 py-1.5 pr-2 pl-3">
			<span class="font-mono text-[length:var(--mono-xs)] text-text-3">{title ?? ''}</span>
			{#if copyable}
				<button
					type="button"
					onclick={copy}
					class="inline-flex cursor-pointer items-center gap-1 rounded-1 border border-border-1 bg-bg-1 px-2 py-[3px] font-sans text-xs font-medium text-text-2 transition-colors duration-[var(--dur-2)] ease-[var(--ease)] hover:border-border-2 hover:text-text-1"
				>{copied ? 'Copied' : copyLabel}</button>
			{/if}
		</div>
	{/if}
	<pre bind:this={preEl} class="m-0 overflow-x-auto p-3 text-text-1">{#if children}{@render children()}{:else}{code ?? ''}{/if}</pre>
</div>

<style>
	pre :global(.tok-comment) {
		color: var(--text-3);
	}
	pre :global(.tok-string) {
		color: var(--chart-5);
	}
</style>
