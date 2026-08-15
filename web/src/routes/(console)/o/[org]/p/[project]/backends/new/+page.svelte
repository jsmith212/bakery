<script lang="ts">
	import { goto } from '$app/navigation';

	import { createBackend } from '$lib/api/backends';
	import { isApiError } from '$lib/api/errors';
	import { tri } from '$lib/api/patch';
	import type { BackendKind } from '$lib/api/types';
	import { backendEndpoints, buildOciConfig } from '$lib/backendConfig';
	import { parseHumanBytes } from '$lib/format';
	import { projectPath } from '$lib/tenancy';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Field, Input, Select, Toggle, Label } from '$lib/components/inputs';
	import { Callout } from '$lib/components/feedback';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);

	const KIND_META: Record<BackendKind, { hint: string; section: string }> = {
		sstate: { hint: 'Yocto shared-state', section: 'sstate — blob store' },
		downloads: { hint: 'Yocto premirror', section: 'downloads — source premirror' },
		hashserv: { hint: 'hash equivalence', section: 'hashserv — taskhash → unihash' },
		bazel: { hint: 'REAPI + /ac /cas', section: 'bazel — action cache + CAS' },
		oci: { hint: 'pull-through proxy', section: 'oci — pull-through proxy' }
	};
	const KIND_ORDER: BackendKind[] = ['sstate', 'downloads', 'hashserv', 'bazel', 'oci'];

	// Default to the first kind this project has not already configured, so
	// the common "add the next backend" path lands on a useful choice rather
	// than always re-offering sstate.
	const firstUnconfigured = KIND_ORDER.find((k) => !project.backends.includes(k));
	let kind = $state<BackendKind>(firstUnconfigured ?? 'sstate');

	const alreadyConfigured = $derived(project.backends.includes(kind));
	const quotaApplicable = $derived(kind !== 'hashserv' && kind !== 'oci');
	const endpoints = $derived(backendEndpoints(kind, org.slug, project.slug));

	let readAuthRequired = $state(true);

	// Plain `string` state, not a union: `Select`'s `value` is bindable as
	// `string`, and a bound union would fail the two-way type check. The
	// three real states are enumerated in the `options` lists below and
	// compared as string literals.
	let retentionMode = $state('default');
	let retentionCustom = $state('2160h');
	const retentionModeOptions = [
		{ value: 'default', label: 'Use organization default' },
		{ value: 'forever', label: 'Retain forever' },
		{ value: 'custom', label: 'Custom duration' }
	];

	let quotaMode = $state('default');
	let quotaCustom = $state('500 GB');
	const quotaModeOptions = [
		{ value: 'default', label: 'Use organization default' },
		{ value: 'nocap', label: 'No cap' },
		{ value: 'custom', label: 'Custom size' }
	];

	let ociDefaultUpstream = $state('docker.io');
	let ociUpstreamsCsv = $state('docker.io');
	let ociTagTtl = $state('10m');

	let submitting = $state(false);
	let retentionError = $state<string | null>(null);
	let quotaError = $state<string | null>(null);
	let submitError = $state<string | null>(null);

	async function submit() {
		if (submitting) return;
		retentionError = null;
		quotaError = null;
		submitError = null;

		let retentionWindow: string | null | undefined;
		if (retentionMode === 'default') retentionWindow = undefined;
		else if (retentionMode === 'forever') retentionWindow = null;
		else if (retentionCustom.trim() === '') {
			retentionError = 'Enter a retention duration like "2160h".';
			return;
		} else retentionWindow = retentionCustom.trim();

		let quotaBytes: number | null | undefined;
		if (!quotaApplicable || quotaMode === 'default') quotaBytes = undefined;
		else if (quotaMode === 'nocap') quotaBytes = null;
		else {
			try {
				quotaBytes = parseHumanBytes(quotaCustom);
			} catch (err) {
				quotaError = err instanceof Error ? err.message : 'That quota is not valid.';
				return;
			}
		}

		const config =
			kind === 'oci'
				? buildOciConfig({
						defaultUpstream: ociDefaultUpstream,
						upstreamsCsv: ociUpstreamsCsv,
						tagTtl: ociTagTtl
					})
				: {};

		submitting = true;

		try {
			await createBackend(org.slug, project.slug, {
				kind,
				read_auth_required: readAuthRequired,
				config,
				retention_window: tri(retentionWindow, 'retention_window'),
				...(quotaBytes !== undefined ? { quota_bytes: tri(quotaBytes, 'quota_bytes') } : {})
			});
			pushToast({ variant: 'success', title: `Created ${kind} backend` });
			await goto(`${projectPath(org.slug, project.slug)}/backends/${kind}`);
		} catch (err) {
			// $lib/api/errors' dispatch table: a field error attaches to the field
			// that caused it; `forbidden` renders in place, where the action was;
			// everything else (409 kind-exists, 5xx) is a toast with the server's
			// own message.
			if (isApiError(err) && err.field === 'retention_window') {
				retentionError = err.message;
			} else if (isApiError(err) && err.field === 'quota_bytes') {
				quotaError = err.message;
			} else if (isApiError(err) && err.code === 'forbidden') {
				submitError = err.message;
			} else {
				toastError(err, 'Could not create backend');
			}
		} finally {
			submitting = false;
		}
	}
</script>

<div class="flex max-w-[720px] flex-col gap-[14px]">
	<div class="flex items-center gap-1.5 text-sm text-text-3">
		<a href="{projectPath(org.slug, project.slug)}/overview" class="text-accent-text hover:underline"
			>{org.slug}/{project.slug}</a
		><span>/</span><span class="text-text-2">New backend</span>
	</div>

	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">New backend</h1>
		<div class="text-sm text-text-2">
			The type cannot be changed after creation — it defines the wire protocol.
		</div>
	</div>

	<div class="flex flex-col gap-1">
		<Label>Type</Label>
		<div class="grid grid-cols-5 gap-1.5">
			{#each KIND_ORDER as id (id)}
				<button
					type="button"
					onclick={() => (kind = id)}
					class={[
						'box-border flex cursor-pointer flex-col gap-0.5 rounded-1 border px-2.5 py-2 text-left',
						id === kind ? 'border-accent-border bg-accent-muted' : 'border-border-0 bg-bg-1'
					]}
				>
					<span
						class="font-mono text-sm font-medium {id === kind ? 'text-accent-text' : 'text-text-1'}"
						>{id}</span
					>
					<span class="text-[10px] leading-[13px] text-text-3">{KIND_META[id].hint}</span>
				</button>
			{/each}
		</div>
	</div>

	{#if alreadyConfigured}
		<Callout variant="warn">
			This project already has a {kind} backend. There is one mount per kind — submitting will be
			refused.
		</Callout>
	{/if}

	<div class="flex flex-col gap-1">
		{#each endpoints as e (e.label)}
			<Field label={e.label}>
				{#snippet children(f)}
					<Input mono disabled value={e.value} {...f} />
				{/snippet}
			</Field>
		{/each}
	</div>

	<div class="flex flex-col gap-3 rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="flex flex-col gap-0.5">
			<Toggle bind:checked={readAuthRequired} label="Require authentication for reads" />
			<div class="pl-10 text-sm text-text-3">
				On by default. Writes always require a key with write scope — this cannot be disabled.
			</div>
		</div>
		{#if !readAuthRequired}
			<Callout variant="warn" title="Reads on this backend will be open to anyone who can reach the server.">
				<span class="text-text-2">
					Writes remain key-protected — an unauthenticated write path is a cache-poisoning vector,
					so Bakery never allows one.</span
				>
			</Callout>
		{/if}
	</div>

	<div class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3">
			{KIND_META[kind].section}
		</div>

		<Field label="Retention" error={retentionError ?? undefined}>
			{#snippet children(f)}
				<div class="flex flex-col gap-1.5">
					<Select size="md" bind:value={retentionMode} options={retentionModeOptions} {...f} />
					{#if retentionMode === 'custom'}
						<Input
							mono
							size="sm"
							placeholder="2160h"
							error={!!retentionError}
							bind:value={retentionCustom}
						/>
					{/if}
				</div>
			{/snippet}
		</Field>

		{#if quotaApplicable}
			<Field label="Quota" error={quotaError ?? undefined}>
				{#snippet children(f)}
					<div class="flex flex-col gap-1.5">
						<Select size="md" bind:value={quotaMode} options={quotaModeOptions} {...f} />
						{#if quotaMode === 'custom'}
							<Input
								mono
								size="sm"
								placeholder="500 GB"
								error={!!quotaError}
								bind:value={quotaCustom}
							/>
						{/if}
					</div>
				{/snippet}
			</Field>
		{:else}
			<div class="text-sm text-text-3">
				{kind === 'hashserv'
					? 'hashserv stores no cache objects, so it cannot have a quota.'
					: 'oci is bounded by its retention window, not a quota.'}
			</div>
		{/if}

		{#if kind === 'oci'}
			<Field
				label="Default upstream"
				hint="Proxied when a request carries no ?ns= (podman, skopeo, Docker Engine)."
			>
				{#snippet children(f)}
					<Input mono bind:value={ociDefaultUpstream} {...f} />
				{/snippet}
			</Field>
			<Field
				label="Allowed upstreams"
				hint="Comma-separated. The SSRF allowlist for ?ns= — an unlisted host 404s."
			>
				{#snippet children(f)}
					<Input mono bind:value={ociUpstreamsCsv} {...f} />
				{/snippet}
			</Field>
			<Field label="Tag TTL" hint="How long a cached tag → digest mapping is served before revalidating.">
				{#snippet children(f)}
					<Input mono bind:value={ociTagTtl} {...f} />
				{/snippet}
			</Field>
			<div class="text-sm text-text-3">
				Upstream registry credentials are set by the operator with <span class="font-mono"
					>--oci-upstream-auth</span
				>. They are server-level config, never stored per project.
			</div>
		{/if}
	</div>

	{#if submitError}
		<div class="text-sm text-err">{submitError}</div>
	{/if}

	<div class="flex justify-end gap-2">
		<Button href="{projectPath(org.slug, project.slug)}/overview" variant="ghost" disabled={submitting}
			>Cancel</Button
		>
		<Button variant="primary" onclick={submit} disabled={submitting}>
			{submitting ? 'Creating…' : 'Create backend'}
		</Button>
	</div>
</div>
