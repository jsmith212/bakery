<script lang="ts">
	import { page } from '$app/state';
	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { CodeBlock } from '$lib/components/content';

	const key = 'bk_a7f13d';
	const secret = 'bks_9YtVq2wXe6LhZn04RmKdSbGu';

	const clients = [
		{
			id: 'yocto',
			name: 'Yocto / bitbake',
			backend: 'sstate',
			hint: 'sstate mirror + premirror + hashserv, Scarthgap 5.0+'
		},
		{
			id: 'moon',
			name: 'moon',
			backend: 'bazel',
			hint: 'gRPC REAPI — instanceName selects the project'
		},
		{
			id: 'ccache',
			name: 'ccache',
			backend: 'bazel',
			hint: 'remote_storage over HTTP, bazel layout'
		},
		{
			id: 'sccache',
			name: 'sccache',
			backend: 'bazel',
			hint: '"WebDAV" mode — plain GET/PUT'
		},
		{
			id: 'containerd',
			name: 'containerd / k8s',
			backend: 'oci',
			hint: 'certs.d hosts.toml per upstream registry'
		},
		{
			id: 'buildkit',
			name: 'BuildKit',
			backend: 'oci',
			hint: 'registry mirror in buildkitd.toml'
		}
	];

	const gotchas: Record<string, [string, string]> = {
		yocto: [
			'The ~/.netrc machine token must be the FULL hashserv URL, not the hostname.',
			'oe-core matches netrc.authenticators(BB_HASHSERVE) as an exact string — "machine bakery.corp" silently leaves hashserv unauthenticated and your build reports nothing. The block below is already in the correct form.'
		],
		moon: [
			'auth.token is the NAME of an environment variable, not the token itself.',
			'If BAKERY_TOKEN is empty or unset, moon silently disables the remote cache with no error — builds "work" but nothing is cached. Also: the project comes from instanceName, because gRPC cannot carry a URL path.'
		],
		ccache: [
			'@layout=bazel is required, not optional.',
			'Without it ccache uses its subdirs layout, which Bakery does not serve on this mount — every request 404s. The line below already includes it.'
		],
		sccache: [
			'"WebDAV" is a misnomer — it\'s plain GET/PUT against the same /ac endpoint.',
			'The token format is key_id:key_secret in one string, exactly as below.'
		],
		containerd: [
			'The legacy registry.mirrors config silently never reaches Bakery.',
			'Only the certs.d + config_path style sends ?ns=<upstream>, and the old style is removed in containerd 2.0. If pulls bypass the cache, this is why.'
		],
		buildkit: [
			'BuildKit puts the path prefix AFTER /v2 — the opposite of containerd.',
			'Requests arrive as /v2/cache/acme/firmware/docker/…?ns=docker.io. Bakery serves both shapes; just don\'t "fix" the mirror URL by adding /v2 yourself.'
		]
	};

	const snippets: Record<string, string> = {
		localconf: `# --- sstate mirror (read) ---\nSSTATE_MIRRORS ?= "file://.* https://bakery.corp/cache/acme/firmware/sstate/PATH;downloadfilename=PATH"\n\n# --- source premirror (read) ---\nINHERIT += "own-mirrors"\nSOURCE_MIRROR_URL ?= "https://bakery.corp/cache/acme/firmware/downloads"\n\n# --- hash equivalence ---\nBB_SIGNATURE_HANDLER = "OEEquivHash"\nBB_HASHSERVE = "wss://bakery.corp/cache/acme/firmware/hashserv"`,
		netrc: `machine wss://bakery.corp/cache/acme/firmware/hashserv login ${key} password ${secret}\nmachine bakery.corp login ${key} password ${secret}`,
		push: `bakery sstate push --project acme/firmware build/sstate-cache\nbakery downloads push --project acme/firmware build/downloads`,
		moonyml: `remote:\n  api: 'grpc'\n  host: 'grpcs://bakery.corp:443'\n  auth:\n    token: 'BAKERY_TOKEN'\n  cache:\n    instanceName: 'acme/firmware'\n    compression: 'zstd'`,
		moonenv: `export BAKERY_TOKEN="${key}:${secret}"`,
		ccacheconf: `remote_storage = http://${key}:${secret}@bakery.corp/cache/acme/firmware @layout=bazel`,
		ccacheenv: `export CCACHE_REMOTE_STORAGE='http://bakery.corp/cache/acme/firmware|layout=bazel|bearer-token=${key}:${secret}'`,
		sccacheenv: `export SCCACHE_WEBDAV_ENDPOINT="https://bakery.corp/cache/acme/firmware"\nexport SCCACHE_WEBDAV_TOKEN="${key}:${secret}"`,
		hoststoml: `server = "https://registry-1.docker.io"\n[host."https://bakery.corp/cache/acme/firmware/docker"]\n  capabilities = ["pull", "resolve"]`,
		configpath: `[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "/etc/containerd/certs.d"`,
		buildkit: `[registry."docker.io"]\n  mirrors = ["bakery.corp/cache/acme/firmware/docker"]`
	};

	const pre: Record<string, string> = {
		localconf: `<div><span class="tok-comment"># --- sstate mirror (read) ---</span></div><div>SSTATE_MIRRORS ?= <span class="tok-string">"file://.* https://bakery.corp/cache/acme/firmware/sstate/PATH;downloadfilename=PATH"</span></div><div>&#160;</div><div><span class="tok-comment"># --- source premirror (read) ---</span></div><div>INHERIT += <span class="tok-string">"own-mirrors"</span></div><div>SOURCE_MIRROR_URL ?= <span class="tok-string">"https://bakery.corp/cache/acme/firmware/downloads"</span></div><div>&#160;</div><div><span class="tok-comment"># --- hash equivalence ---</span></div><div>BB_SIGNATURE_HANDLER = <span class="tok-string">"OEEquivHash"</span></div><div>BB_HASHSERVE = <span class="tok-string">"wss://bakery.corp/cache/acme/firmware/hashserv"</span></div><div><span class="tok-comment"># Do NOT set BB_HASHSERVE = "auto" or BB_HASHSERVE_UPSTREAM —</span></div><div><span class="tok-comment"># that topology never reports hashes back to Bakery.</span></div>`,
		netrc: `<div><span class="tok-comment"># hashserv: machine token is the FULL URL — exact string match</span></div><div>machine wss://bakery.corp/cache/acme/firmware/hashserv login bk_a7f13d password bks_9YtVq2wXe6LhZn04RmKdSbGu</div><div><span class="tok-comment"># sstate + downloads (plain HTTP Basic, keyed by hostname)</span></div><div>machine bakery.corp login bk_a7f13d password bks_9YtVq2wXe6LhZn04RmKdSbGu</div>`,
		push: `<div>bakery sstate push --project acme/firmware build/sstate-cache</div><div>bakery downloads push --project acme/firmware build/downloads</div>`,
		moonyml: `<div>remote:</div><div>  api: <span class="tok-string">'grpc'</span></div><div>  host: <span class="tok-string">'grpcs://bakery.corp:443'</span></div><div>  auth:</div><div>    token: <span class="tok-string">'BAKERY_TOKEN'</span>          <span class="tok-comment"># the NAME of an env var, NOT the token</span></div><div>  cache:</div><div>    instanceName: <span class="tok-string">'acme/firmware'</span>   <span class="tok-comment"># the project selector — gRPC can't carry a URL path</span></div><div>    compression: <span class="tok-string">'zstd'</span></div>`,
		moonenv: `export BAKERY_TOKEN=<span class="tok-string">"bk_a7f13d:bks_9YtVq2wXe6LhZn04RmKdSbGu"</span>`,
		ccacheconf: `remote_storage = http://bk_a7f13d:bks_9YtVq2wXe6LhZn04RmKdSbGu@bakery.corp/cache/acme/firmware @layout=bazel`,
		ccacheenv: `export CCACHE_REMOTE_STORAGE=<span class="tok-string">'http://bakery.corp/cache/acme/firmware|layout=bazel|bearer-token=bk_a7f13d:bks_9YtVq2wXe6LhZn04RmKdSbGu'</span>`,
		sccacheenv: `<div>export SCCACHE_WEBDAV_ENDPOINT=<span class="tok-string">"https://bakery.corp/cache/acme/firmware"</span></div><div>export SCCACHE_WEBDAV_TOKEN=<span class="tok-string">"bk_a7f13d:bks_9YtVq2wXe6LhZn04RmKdSbGu"</span></div>`,
		hoststoml: `<div>server = <span class="tok-string">"https://registry-1.docker.io"</span></div><div>[host.<span class="tok-string">"https://bakery.corp/cache/acme/firmware/docker"</span>]</div><div>  capabilities = [<span class="tok-string">"pull"</span>, <span class="tok-string">"resolve"</span>]</div><div>&#160;</div><div><span class="tok-comment"># repeat per upstream (ghcr.io, quay.io, gcr.io, registry.k8s.io),</span></div><div><span class="tok-comment"># all pointing at the SAME Bakery endpoint — ?ns= does the routing.</span></div>`,
		configpath: `<div>[plugins.<span class="tok-string">"io.containerd.grpc.v1.cri"</span>.registry]</div><div>  config_path = <span class="tok-string">"/etc/containerd/certs.d"</span></div>`,
		buildkit: `<div>[registry.<span class="tok-string">"docker.io"</span>]</div><div>  mirrors = [<span class="tok-string">"bakery.corp/cache/acme/firmware/docker"</span>]</div>`
	};

	const validClients = clients.map((c) => c.id);
	const initial = page.url.searchParams.get('client');
	let selected = $state(initial && validClients.includes(initial) ? initial : 'yocto');

	const gotcha = $derived(gotchas[selected]);
</script>

{#snippet code(title: string, copy: string, html: string)}
	<CodeBlock {title} copyText={copy}>{@html html}</CodeBlock>
{/snippet}

<div class="flex flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Config snippets</h1>
		<div class="text-sm text-text-2">
			The exact client config to paste, with a working key already baked in. You should not need the
			docs.
		</div>
	</div>

	<div
		class="flex items-start gap-2.5 rounded-2 border border-warn-border bg-warn-muted px-3 py-2.5"
	>
		<span class="text-xs leading-[18px] text-warn" aria-hidden="true">▲</span>
		<div class="min-w-0 text-sm text-text-1">
			<span class="font-medium"
				>Key <span class="font-mono">ci-writer-2</span> (read + write) was minted for these snippets
				and its secret is baked into every block below.</span
			>
			<span class="text-text-2">
				You will never see the secret again after leaving this page — paste the blocks into your
				config now, or revoke the key.</span
			>
		</div>
		<Button variant="ghost" size="sm" class="ml-auto shrink-0">Mint different key</Button>
	</div>

	<div class="grid grid-cols-[240px_1fr] items-start gap-[14px]">
		<div class="flex flex-col gap-0.5">
			{#each clients as c (c.id)}
				<button
					type="button"
					aria-pressed={selected === c.id}
					onclick={() => (selected = c.id)}
					class="box-border flex cursor-pointer flex-col gap-[3px] rounded-1 border px-2.5 py-2 text-left {selected ===
					c.id
						? 'border-accent-border bg-accent-muted'
						: 'border-border-0 bg-bg-1'}"
				>
					<span class="flex items-center gap-2">
						<span
							class="text-base font-medium {selected === c.id
								? 'text-accent-text'
								: 'text-text-1'}">{c.name}</span
						>
						<Badge variant="type" class="ml-auto">{c.backend}</Badge>
					</span>
					<span class="text-xs leading-[15px] text-text-3">{c.hint}</span>
				</button>
			{/each}
		</div>

		<div class="flex min-w-0 flex-col gap-3">
			<div
				class="flex items-start gap-2.5 rounded-2 border border-err-border bg-err-muted px-3 py-2.5"
			>
				<span class="text-xs leading-[18px] text-err" aria-hidden="true">✕</span>
				<div class="min-w-0 text-sm text-text-1">
					<span class="font-semibold">{gotcha[0]}</span>
					<span class="text-text-2"> {gotcha[1]}</span>
				</div>
			</div>

			{#if selected === 'yocto'}
				{@render code('conf/local.conf', snippets.localconf, pre.localconf)}
				{@render code('~/.netrc', snippets.netrc, pre.netrc)}
				{@render code('after a build — push to the cache', snippets.push, pre.push)}
			{:else if selected === 'moon'}
				{@render code('.moon/workspace.yml', snippets.moonyml, pre.moonyml)}
				{@render code('environment', snippets.moonenv, pre.moonenv)}
			{:else if selected === 'ccache'}
				{@render code('~/.config/ccache/ccache.conf', snippets.ccacheconf, pre.ccacheconf)}
				{@render code('or via environment', snippets.ccacheenv, pre.ccacheenv)}
			{:else if selected === 'sccache'}
				{@render code('environment', snippets.sccacheenv, pre.sccacheenv)}
			{:else if selected === 'containerd'}
				{@render code(
					'/etc/containerd/certs.d/docker.io/hosts.toml',
					snippets.hoststoml,
					pre.hoststoml
				)}
				{@render code(
					'containerd config — enable the certs.d path',
					snippets.configpath,
					pre.configpath
				)}
			{:else if selected === 'buildkit'}
				{@render code('buildkitd.toml', snippets.buildkit, pre.buildkit)}
			{/if}
		</div>
	</div>
</div>
