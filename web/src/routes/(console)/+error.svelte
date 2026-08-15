<script lang="ts">
	import { page } from '$app/state';
	import { EmptyState } from '$lib/components/feedback';
	import { Button } from '$lib/components/buttons';

	// The console's own error surface. A load that hit a 403 or a 404 renders the
	// SERVER's message here, in place, inside the console chrome -- not a
	// redirect, and not a guess about which of "you cannot see it" and "it does
	// not exist" happened, because the authorization ladder makes those the same
	// answer on purpose.
	const glyph = $derived(page.status === 404 ? '∅' : '✕');
	const title = $derived(page.status === 404 ? 'Not here' : 'Cannot show this');
</script>

<EmptyState {glyph} {title} desc={page.error?.message ?? 'The server did not say why.'}>
	{#snippet action()}
		<Button href="/orgs" variant="secondary" size="md">Back to organizations</Button>
	{/snippet}
</EmptyState>
