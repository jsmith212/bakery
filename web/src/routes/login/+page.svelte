<script lang="ts">
	import { page } from '$app/state';
	import Button from '$lib/components/buttons/Button.svelte';
	import { Field, Input } from '$lib/components/inputs';

	const devLogin = $derived(page.url.searchParams.get('dev') === '1');
	let devUser = $state('');
</script>

<div class="grid min-h-screen place-items-center bg-bg-0 p-6">
	<div class="flex w-[340px] flex-col gap-[14px]">
		<div class="pb-1 text-center">
			<div class="text-xl font-semibold tracking-[-0.01em] text-text-1">Bakery</div>
			<div class="mt-1 font-mono text-sm text-text-3">build cache · bakery.corp</div>
		</div>

		<div class="flex flex-col gap-2 rounded-2 border border-border-0 bg-bg-1 p-5">
			<div class="mb-1.5 text-base font-medium text-text-1">Sign in</div>
			<Button variant="secondary" size="lg" class="w-full">Continue with Google</Button>
			<Button variant="secondary" size="lg" class="w-full">Continue with Authelia</Button>
			<Button variant="secondary" size="lg" class="w-full">Continue with GitHub</Button>
			<div class="mt-1.5 text-center text-xs text-text-3">
				Access is granted by your org admin. Build clients authenticate with API keys, not this
				login.
			</div>
		</div>

		{#if devLogin}
			<div class="overflow-hidden rounded-2 border border-dashed border-warn-border">
				<div
					class="flex items-center gap-2 border-b border-dashed border-warn-border bg-warn-muted px-3 py-2"
				>
					<span class="text-xs text-warn">▲</span>
					<span class="font-mono text-xs font-medium text-warn">DEV_LOGIN_ENABLED=1</span>
					<span class="text-xs text-text-2">not a production instance</span>
				</div>
				<div class="flex flex-col gap-[10px] px-3 py-[14px]">
					<Field label="User">
						<Input size="md" mono placeholder="anna" bind:value={devUser} />
					</Field>
					<Button
						variant="secondary"
						size="md"
						class="w-full text-warn! border-warn-border! hover:border-warn-border!"
					>
						Sign in without auth
					</Button>
					<div class="text-[10px] leading-[14px] text-text-3">
						This panel exists only when the env var is set at boot. No UI or API path can enable it.
					</div>
				</div>
			</div>
		{/if}
	</div>
</div>
