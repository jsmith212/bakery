<script lang="ts">
	import { theme, setTheme, type Theme } from '$lib/theme';
	import { Badge } from '$lib/components/badges';
	import { Input, Field } from '$lib/components/inputs';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const me = $derived(data.me);

	const microLabel = 'text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3';
	const card = 'flex flex-col rounded-2 border border-border-0 bg-bg-1 p-[14px]';

	const themeOptions: { value: Theme; label: string }[] = [
		{ value: 'dark', label: 'Dark' },
		{ value: 'light', label: 'Light' },
		{ value: 'system', label: 'System' }
	];
</script>

<div class="flex max-w-[720px] flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Your account</h1>
		<div class="text-sm text-text-2">Profile and appearance.</div>
	</div>

	<div class="{card} gap-[14px]">
		<div class={microLabel}>Profile</div>
		<div class="flex items-center gap-[14px]">
			<span
				class="inline-flex h-12 w-12 flex-none items-center justify-center rounded-full border border-accent-border bg-accent-muted text-[18px] font-semibold text-accent-text"
				>{(me.display_name || me.email)[0]?.toUpperCase()}</span
			>
			<div class="text-xs text-text-3">
				No avatar upload exists — Bakery renders your initial from your display name.
			</div>
		</div>
		<div class="grid grid-cols-2 gap-[14px]">
			<Field
				label="Display name"
				hint="Managed by your identity provider — change it there, not here."
			>
				{#snippet children(f)}
					<Input size="md" value={me.display_name} disabled {...f} />
				{/snippet}
			</Field>
			<Field label="Email" hint="Managed by your identity provider — change it there, not here.">
				{#snippet children(f)}
					<Input size="md" mono value={me.email} disabled {...f} />
				{/snippet}
			</Field>
		</div>
	</div>

	<div class="{card} gap-2.5">
		<div class={microLabel}>Appearance</div>
		<div class="flex gap-1.5">
			{#each themeOptions as t (t.value)}
				<button
					type="button"
					aria-pressed={$theme === t.value}
					onclick={() => setTheme(t.value)}
					class="cursor-pointer rounded-1 border px-[14px] py-1.5 text-sm transition-colors duration-[var(--dur-2)] ease-[var(--ease)] {$theme ===
					t.value
						? 'border-accent-border bg-accent-muted text-accent-text'
						: 'border-border-1 bg-bg-2 text-text-2 hover:border-border-2 hover:text-text-1'}"
					>{t.label}</button
				>
			{/each}
		</div>
		<div class="text-xs text-text-3">
			System follows your OS preference. Applies immediately, remembered in this browser.
		</div>
	</div>

	<div class="{card} gap-2.5">
		<div class="flex items-center justify-between">
			<div class={microLabel}>Personal access tokens</div>
			<Badge status="idle">planned</Badge>
		</div>
		<div class="text-sm leading-[18px] text-text-2">
			Bakery has no personal access tokens yet — there is no such credential, no table, and no
			auth path for one. For the <span class="font-mono text-xs">bakery</span> CLI, run
			<span class="font-mono text-xs text-text-1">bakery login</span>. For bots and builds, mint a
			project-scoped key on that project's Keys page — those stay narrowly scoped and survive you
			leaving the org.
		</div>
	</div>
</div>
