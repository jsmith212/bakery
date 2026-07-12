<script lang="ts">
	import { page } from '$app/state';
	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { Input, Select, Toggle, Checkbox, Field, Label, Hint } from '$lib/components/inputs';
	import { KeyValueList } from '$lib/components/content';

	interface TypeMeta {
		hint: string;
		endpoint: string;
		ehint: string;
		section: string;
	}

	const meta: Record<string, TypeMeta> = {
		sstate: {
			hint: 'Yocto shared-state',
			endpoint: '/cache/acme/firmware/sstate/…',
			ehint: "Derived from org and project slugs. SSTATE_MIRRORS' PATH token expands to the full relative path.",
			section: 'sstate — blob store'
		},
		downloads: {
			hint: 'Yocto premirror',
			endpoint: '/cache/acme/firmware/downloads/…',
			ehint: 'SOURCE_MIRROR_URL points here; flat basenames only.',
			section: 'downloads — source premirror'
		},
		hashserv: {
			hint: 'hash equivalence',
			endpoint: 'wss://…/cache/acme/firmware/hashserv',
			ehint: 'WebSocket upgrade on the shared listener. Not a blob store.',
			section: 'hashserv — taskhash → unihash'
		},
		bazel: {
			hint: 'REAPI + /ac /cas',
			endpoint: '/cache/acme/firmware/{ac,cas}/…',
			ehint: 'gRPC clients select the project via instance_name instead — gRPC cannot carry a URL path.',
			section: 'bazel — action cache + CAS'
		},
		oci: {
			hint: 'pull-through proxy',
			endpoint: '/cache/acme/firmware/docker/v2/…',
			ehint: 'containerd appends /v2 itself; BuildKit puts the prefix after /v2. Both shapes are served.',
			section: 'oci — pull-through proxy'
		}
	};

	const typeIds = Object.keys(meta);
	const qp = page.url.searchParams.get('type');

	let type = $state(qp && typeIds.includes(qp) ? qp : 'sstate');
	let requireAuth = $state(true);

	const m = $derived(meta[type]);
	const authOff = $derived(!requireAuth);

	const isSstate = $derived(type === 'sstate');
	const isDownloads = $derived(type === 'downloads');
	const isHashserv = $derived(type === 'hashserv');
	const isBazel = $derived(type === 'bazel');
	const isOci = $derived(type === 'oci');
</script>

<div class="flex max-w-[720px] flex-col gap-[14px]">
	<div class="flex items-center gap-1.5 text-sm text-text-3">
		<a href="/overview" class="text-accent-text hover:underline">acme/firmware</a><span>/</span><a
			href="/backends/sstate"
			class="text-accent-text hover:underline">Backends</a
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
			{#each typeIds as id (id)}
				<button
					type="button"
					onclick={() => (type = id)}
					class={[
						'box-border flex cursor-pointer flex-col gap-0.5 rounded-1 border px-2.5 py-2 text-left',
						id === type ? 'border-accent-border bg-accent-muted' : 'border-border-0 bg-bg-1'
					]}
				>
					<span
						class="font-mono text-sm font-medium {id === type
							? 'text-accent-text'
							: 'text-text-1'}">{id}</span
					>
					<span class="text-[10px] leading-[13px] text-text-3">{meta[id].hint}</span>
				</button>
			{/each}
		</div>
	</div>

	<Field label="Endpoint" hint={m.ehint}>
		<Input mono disabled value={m.endpoint} />
	</Field>

	<div class="flex flex-col gap-3 rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="flex items-start gap-2.5">
			<div class="mt-px">
				<Toggle bind:checked={requireAuth} />
			</div>
			<div>
				<div class="text-base leading-[18px] text-text-1">Require authentication for reads</div>
				<div class="mt-0.5 text-sm text-text-3">
					On by default. Writes always require a key with write scope — this cannot be disabled.
				</div>
			</div>
		</div>
		{#if authOff}
			<div
				class="flex items-start gap-2.5 rounded-2 border border-warn-border bg-warn-muted px-3 py-2.5"
			>
				<span class="text-[11px] leading-[18px] text-warn" aria-hidden="true">▲</span>
				<div class="text-sm leading-[18px] text-text-1">
					<span class="font-semibold"
						>Reads on this backend will be open to anyone who can reach the server.</span
					><span class="text-text-2">
						Writes remain key-protected — an unauthenticated write path is a cache-poisoning vector,
						so Bakery never allows one.</span
					>
				</div>
			</div>
		{/if}
	</div>

	<div class="flex flex-col gap-[14px] rounded-2 border border-border-0 bg-bg-1 p-[14px]">
		<div class="text-xs font-medium uppercase tracking-[var(--tracking-label)] text-text-3">
			{m.section}
		</div>

		{#if isSstate}
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Quota"><Input value="500 GB" /></Field>
				<Field label="Retention">
					<Select value="90 days since last hit">
						<option>90 days since last hit</option>
						<option>30 days since last hit</option>
						<option>LRU under quota</option>
					</Select>
				</Field>
			</div>
			<Field
				label="Hash equivalence coupling"
				hint="The unihash is the GC root. Objects unreachable from a live unihash are swept; hashserv is always swept first."
			>
				<Select value="hashserv (this project) — swept together">
					<option>hashserv (this project) — swept together</option>
					<option>none — sstate only, GC by retention</option>
				</Select>
			</Field>
		{/if}

		{#if isDownloads}
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Quota"><Input value="500 GB" /></Field>
				<Field label="Retention">
					<Select value="180 days since last fetch">
						<option>180 days since last fetch</option>
						<option>90 days since last fetch</option>
						<option>Keep forever</option>
					</Select>
				</Field>
			</div>
			<Hint>
				Flat directory of basenames, including git2_* tarballs. Clients verify SRC_URI checksums
				themselves — a stale mirror can only cause a fallback, never a compromise.
			</Hint>
		{/if}

		{#if isHashserv}
			<Field
				label="Transport"
				hint="WebSocket shares TLS, ingress, and the Authorization header with everything else. Unauthenticated upgrades are rejected with 401."
			>
				<div class="flex gap-4">
					<Checkbox checked disabled>WebSocket (wss)</Checkbox>
					<Checkbox>Raw TCP (off by default)</Checkbox>
				</div>
			</Field>
			<Field
				label="Anonymous permissions"
				hint="Upstream's default grants anonymous @db-admin. Bakery does not copy that default, and this is not configurable."
			>
				<Input mono value="none" disabled />
			</Field>
			<Field label="sstate coupling">
				<Select value="sstate (this project) — swept together">
					<option>sstate (this project) — swept together</option>
					<option>standalone</option>
				</Select>
			</Field>
		{/if}

		{#if isBazel}
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Quota"><Input value="500 GB" /></Field>
				<Field label="Eviction">
					<Select value="LRU under quota">
						<option>LRU under quota</option>
						<option>TTL 30 days</option>
					</Select>
				</Field>
			</div>
			<KeyValueList
				pairs={[
					{
						key: '/ac',
						value: 'opaque · overwrite allowed · never verified — serves ccache, sccache, moon-HTTP'
					},
					{ key: '/cas', value: 'content-addressed · verify key = sha256(body)' },
					{ key: 'gRPC', value: 'instance_name = acme/firmware', mono: true }
				]}
			/>
			<Hint>
				Sub-store semantics are fixed by protocol — they are shown here so you know what you're
				getting, not to be edited.
			</Hint>
		{/if}

		{#if isOci}
			<Field
				label="Upstream registries"
				hint="One endpoint serves all upstreams — the client's ?ns= parameter routes the request."
			>
				<div class="flex flex-wrap gap-1">
					<Badge variant="type">docker.io</Badge>
					<Badge variant="type">ghcr.io</Badge>
					<Badge variant="type">quay.io</Badge>
					<Badge variant="type">gcr.io</Badge>
					<Badge variant="type">registry.k8s.io</Badge>
					<Button variant="ghost" size="sm">+ Add</Button>
				</div>
			</Field>
			<div class="grid grid-cols-2 gap-[14px]">
				<Field label="Docker Hub username"><Input mono placeholder="acme-ci" /></Field>
				<Field label="Docker Hub access token">
					<Input mono type="password" placeholder="dckr_pat_…" />
				</Field>
			</div>
			<Hint>
				Bakery issues its own 401 challenge downstream, so your Hub credentials are never relayed to
				unauthenticated clients.
			</Hint>
		{/if}
	</div>

	<div class="flex justify-end gap-2">
		<Button variant="ghost">Cancel</Button>
		<Button variant="primary">Create backend</Button>
	</div>
</div>
