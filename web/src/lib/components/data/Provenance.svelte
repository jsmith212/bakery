<script lang="ts">
	import { Badge } from '$lib/components/badges';
	import { formatDateTimeUTC } from '$lib/format';
	import type { Provenance } from '$lib/api/types';

	/**
	 * Renders WHY somebody holds a role.
	 *
	 * One component for both rosters, fed by `memberProvenance` /
	 * `siteAdminProvenance`, because the two wire shapes do not share a single
	 * provenance tag: `oidc_role`/`oidc_group`/`local_role`/`org_role_source`
	 * against `site_role_oidc`/`site_oidc_group`/`site_role_local`/
	 * `site_role_source`. Reading either set directly in markup is how one of
	 * them silently renders blank.
	 *
	 * This is not decoration. Org membership and the site role are both HYBRID --
	 * an OIDC half the reconciler owns and a local half the API owns -- and the
	 * failure the hybrid model has to answer for is a local grant that outlives
	 * the directory revocation meant to remove it. That state is inherent, so it
	 * is made VISIBLE instead: every row says which half is holding the role up.
	 * A backdoor you can see on a screen is not much of a backdoor, and this is
	 * the screen.
	 *
	 * `org_role_source` is a vocabulary of its own (`oidc_groups` | `local` |
	 * `oidc_groups+local`) and is NEVER a `BadgeStatus` -- it renders as a mono
	 * type badge, not a status glyph.
	 */
	interface Props {
		provenance: Provenance;
	}

	let { provenance }: Props = $props();

	const SOURCE_LABEL = {
		oidc_groups: 'identity provider',
		local: 'granted in Bakery',
		'oidc_groups+local': 'identity provider + local grant'
	} as const;

	const sourceLabel = $derived(
		provenance.source ? SOURCE_LABEL[provenance.source] : 'unknown source'
	);

	// A local grant made by the CLI break-glass has no granter to name, because
	// it had no session. That emptiness is a finding -- somebody with database
	// access made it -- and is said out loud rather than left blank.
	const grantedByLabel = $derived(provenance.grantedByEmail || 'a database operator');
</script>

<div class="flex flex-col gap-1">
	<Badge variant="type">{sourceLabel}</Badge>

	{#if provenance.oidc}
		<div class="text-xs text-text-3">
			<span class="font-mono">{provenance.oidc}</span>
			{#if provenance.oidcGroup}
				via <span class="font-mono">{provenance.oidcGroup}</span>
			{/if}
		</div>
	{/if}

	{#if provenance.local}
		<div class="text-xs text-text-3">
			<span class="font-mono">{provenance.local}</span>
			granted by {grantedByLabel}
			{#if provenance.grantedAt}
				on <span class="font-mono">{formatDateTimeUTC(provenance.grantedAt)}</span>
			{/if}
		</div>
	{/if}
</div>
