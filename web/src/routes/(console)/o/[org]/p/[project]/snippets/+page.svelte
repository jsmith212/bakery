<script lang="ts">
	import { page } from '$app/state';

	import { previewSnippet, generateSnippet } from '$lib/api/snippets';
	import { isApiError } from '$lib/api/errors';
	import { SNIPPET_TOOLS, type SnippetResponse, type SnippetTool } from '$lib/api/types';
	import { canMintKey } from '$lib/roles';
	import { toastError, pushToast } from '$lib/toasts';

	import { Button } from '$lib/components/buttons';
	import { Badge } from '$lib/components/badges';
	import { CodeBlock } from '$lib/components/content';
	import { Skeleton, EmptyState, Callout } from '$lib/components/feedback';

	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const org = $derived(data.org);
	const project = $derived(data.project);
	const mintAllowed = $derived(canMintKey({ role: project.role }));

	interface Tile {
		id: SnippetTool;
		name: string;
		backend: string;
		hint: string;
	}

	const tiles: Tile[] = [
		{ id: 'yocto', name: 'Yocto / bitbake', backend: 'sstate', hint: 'sstate + premirror + hashserv' },
		{ id: 'moon', name: 'moon', backend: 'bazel', hint: 'gRPC REAPI — instance_name selects the project' },
		{ id: 'ccache', name: 'ccache', backend: 'bazel', hint: 'HTTP, bazel layout, plaintext only' },
		{ id: 'sccache', name: 'sccache', backend: 'bazel', hint: 'WebDAV — PROPFIND/MKCOL before PUT' },
		{ id: 'bazel', name: 'Bazel', backend: 'bazel', hint: 'gRPC REAPI — .bazelrc' },
		{ id: 'containerd', name: 'containerd / k8s', backend: 'oci', hint: 'certs.d hosts.toml per upstream' },
		{ id: 'buildkit', name: 'BuildKit', backend: 'oci', hint: 'registry mirror in buildkitd.toml' },
		{ id: 'podman', name: 'Podman / skopeo / CRI-O', backend: 'oci', hint: 'registries.conf, no ?ns= ever sent' },
		{ id: 'docker', name: 'Docker Engine', backend: 'oci', hint: 'daemon.json — forwards your real Hub login' }
	];

	const gotchas: Record<SnippetTool, string> = {
		yocto:
			'oe-core matches the hashserv netrc line as an EXACT string against the full URL, not the hostname — a hostname-only line silently leaves hashserv unauthenticated.',
		moon: 'auth.token names an environment variable, not the token itself. An empty or unset variable silently disables the remote cache — no error, no cache.',
		ccache:
			'@layout=bazel is mandatory (the default layout 404s on this mount), and this backend is plaintext HTTP only — ccache has no https scheme.',
		sccache:
			'sccache is WebDAV, not plain GET/PUT: it PROPFINDs and MKCOLs before every PUT. A 405 on either silently degrades sccache to read-only for the whole process.',
		bazel:
			'Do not set --remote_cache_compression. Bakery advertises IDENTITY only, and Bazel hard-fails the connection if ZSTD is requested and not advertised.',
		containerd:
			'Only the certs.d + config_path style sends ?ns=<upstream> — the legacy registry.mirrors config never reaches Bakery and is removed outright in containerd 2.0.',
		buildkit:
			'BuildKit puts the path prefix AFTER /v2 — the opposite of containerd. Basic auth only activates when BOTH username and secret are set; one empty field silently skips the mirror.',
		podman:
			'containers/image never sends ?ns=, so this project needs default_upstream configured server-side. It also strips credentials on a cross-domain mirror — a docker.io login never reaches Bakery.',
		docker:
			'Docker Engine forwards your real Docker Hub login to this mirror, unscoped, on every pull. Only works against a backend with authenticated reads turned off.'
	};

	// Same idiom as `$lib/api/errors`' `isApiErrorCode`: a checked narrowing into
	// the closed union, never a bare `as` cast.
	function isSnippetTool(value: string): value is SnippetTool {
		return (SNIPPET_TOOLS as readonly string[]).includes(value);
	}

	const initialTool = page.url.searchParams.get('tool');

	let selected = $state<SnippetTool>(
		initialTool && isSnippetTool(initialTool) ? initialTool : 'yocto'
	);
	let response = $state<SnippetResponse | null>(null);
	let previewPending = $state(false);
	let previewFailed = $state<string | null>(null);
	let mintPending = $state(false);
	let mintScopeError = $state<string | null>(null);
	// The tool a MINTED (non-preview) `response` belongs to, or null. Like the
	// keys screen's reveal modal, a minted response carries a one-time secret
	// already baked into the blocks on screen -- it is gone the moment it is
	// replaced. `loadPreview`'s own effect depends on `org`/`project` (read
	// before its first `await`), so a re-run triggered by something other than
	// the user picking a different tile -- a parent layout reloading after an
	// unrelated invalidation, say -- must not silently wipe it.
	let mintedForTool = $state<SnippetTool | null>(null);

	async function loadPreview(tool: SnippetTool) {
		if (tool === mintedForTool && response && !response.preview) return;

		previewPending = true;
		previewFailed = null;
		mintScopeError = null;
		response = null;

		try {
			response = await previewSnippet(org.slug, project.slug, tool);
		} catch (err) {
			previewFailed = isApiError(err) ? err.message : 'Could not reach the server.';
		} finally {
			previewPending = false;
		}
	}

	function selectTile(tool: SnippetTool) {
		selected = tool;
	}

	// The initial tile's preview -- selecting a tile is what triggers a
	// preview, and the default tile counts as a selection. This is the ONLY
	// call site: selectTile just changes `selected` and lets this effect
	// react, so a click never double-fires the request.
	$effect(() => {
		void loadPreview(selected);
	});

	async function mintForReal() {
		if (mintPending || !mintAllowed) return;
		mintPending = true;
		mintScopeError = null;

		try {
			response = await generateSnippet(org.slug, project.slug, { tool: selected });
			mintedForTool = selected;
			pushToast({
				variant: 'success',
				title: `Minted key ${response.api_key?.token_prefix ?? ''}`,
				detail: 'The token is shown once, below — it cannot be recovered later. The key can be revoked on the keys page by its prefix.'
			});
		} catch (err) {
			// `mintAllowed` already gates the button on the role this page loaded
			// with, but that can go stale (another admin edits membership
			// mid-session) -- the server's `scope_exceeds_role` is the authority,
			// and matching `keys/+page.svelte`, it renders in place, not a toast.
			if (isApiError(err) && err.treatment === 'scope') {
				mintScopeError = err.message;
			} else {
				toastError(err, 'Could not generate the snippet');
			}
		} finally {
			mintPending = false;
		}
	}

	const gotcha = $derived(gotchas[selected]);
</script>

{#snippet block(title: string, content: string)}
	<CodeBlock {title} code={content} />
{/snippet}

<div class="flex flex-col gap-[14px]">
	<div>
		<h1 class="mb-0.5 text-lg font-semibold text-text-1">Config snippets</h1>
		<div class="text-sm text-text-2">
			The exact client config to paste. Each tile previews with a placeholder credential — mint a
			real key only when you are ready to paste it into a config file.
		</div>
	</div>

	<div class="grid grid-cols-[240px_1fr] items-start gap-[14px]">
		<div class="flex flex-col gap-0.5">
			{#each tiles as t (t.id)}
				<button
					type="button"
					aria-pressed={selected === t.id}
					onclick={() => selectTile(t.id)}
					class="box-border flex cursor-pointer flex-col gap-[3px] rounded-1 border px-2.5 py-2 text-left {selected ===
					t.id
						? 'border-accent-border bg-accent-muted'
						: 'border-border-0 bg-bg-1'}"
				>
					<span class="flex items-center gap-2">
						<span
							class="text-base font-medium {selected === t.id
								? 'text-accent-text'
								: 'text-text-1'}">{t.name}</span
						>
						<Badge variant="type" class="ml-auto">{t.backend}</Badge>
					</span>
					<span class="text-xs leading-[15px] text-text-3">{t.hint}</span>
				</button>
			{/each}
		</div>

		<div class="flex min-w-0 flex-col gap-3">
			<Callout variant="error">{gotcha}</Callout>

			{#if previewPending}
				<div class="flex flex-col gap-2">
					<Skeleton height={80} />
					<Skeleton height={40} />
				</div>
			{:else if previewFailed}
				<EmptyState glyph="✕" title="Could not load this snippet" desc={previewFailed}>
					{#snippet action()}
						<Button variant="secondary" size="md" onclick={() => loadPreview(selected)}
							>Retry</Button
						>
					{/snippet}
				</EmptyState>
			{:else if response}
				{#if response.warnings && response.warnings.length > 0}
					<div class="flex flex-col gap-1.5">
						{#each response.warnings as w, i (i)}
							<Callout variant="warn">{w}</Callout>
						{/each}
					</div>
				{/if}

				{#if response.preview}
					<div
						class="flex items-center gap-2.5 rounded-2 border border-border-0 bg-bg-2 px-3 py-2.5 text-sm text-text-2"
					>
						Previewing with a placeholder credential — nothing has been minted yet.
						<Button
							variant="primary"
							size="sm"
							class="ml-auto shrink-0"
							disabled={mintPending || !mintAllowed}
							onclick={mintForReal}
						>
							{mintPending ? 'Generating…' : 'Generate with a new key'}
						</Button>
					</div>
					{#if mintScopeError}
						<div class="text-xs text-err">{mintScopeError}</div>
					{:else if !mintAllowed}
						<div class="text-xs text-text-3">
							You have no role on this project, so Bakery has nothing to cap a minted key's scope
							against. An org admin can grant you one on the members page.
						</div>
					{/if}
				{:else if response.api_key}
					<Callout variant="warn">
						<span class="font-semibold"
							>Key <span class="font-mono">{response.api_key.token_prefix}</span> ({response
								.api_key.scope}) was minted for this snippet and its secret is baked into the
							blocks below.</span
						>
						<span class="text-text-2">
							You will not see the secret again — find it on
							<a href="/o/{org.slug}/p/{project.slug}/keys" class="text-accent-text hover:underline"
								>the keys page</a
							> by its prefix, or revoke it there.</span
						>
					</Callout>
				{/if}

				{#if response.local_conf}
					{@render block('conf/local.conf', response.local_conf)}
				{/if}
				{#if response.netrc}
					{@render block('~/.netrc', response.netrc)}
				{/if}
				{#if response.push_commands.length > 0}
					{@render block('after a build — push to the cache', response.push_commands.join('\n'))}
				{/if}
				{#each response.files ?? [] as f (f.path)}
					<div class="flex flex-col gap-1">
						{@render block(f.path, f.content)}
						<div class="text-xs text-text-3">Write this to <span class="font-mono">{f.path}</span>.</div>
					</div>
				{/each}
				{#if response.env && response.env.length > 0}
					{@render block(
						'environment',
						response.env.map((e) => `export ${e.name}="${e.value}"`).join('\n')
					)}
				{/if}
				{#if !response.local_conf && !response.netrc && response.push_commands.length === 0 && (!response.files || response.files.length === 0) && (!response.env || response.env.length === 0)}
					<EmptyState
						glyph="∅"
						title="Nothing to show"
						desc="This project has no backend configured that {selected} can target. See the warning above."
					/>
				{/if}
			{/if}
		</div>
	</div>
</div>
