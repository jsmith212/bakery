/**
 * Kind-specific parsing of `Backend.config` (`json.RawMessage` on the wire,
 * `unknown` in `types.ts`).
 *
 * Every kind except `oci` (and, for chaining, `hashserv`) has an EMPTY config
 * object -- `internal/api/backends.go`'s `backendConfig` accepts any JSON
 * object and no other backend package defines a shape for it. So this module
 * has exactly two real parsers, mirroring the two Go structs that read
 * `cache_backends.config`: `internal/cache/oci/config.go`'s `backendConfig`
 * and `internal/cache/hashserv/upstreams.go`'s `backendConfig`. Both are
 * total and never throw -- a malformed blob must not break the backend
 * detail screen any more than it breaks the server, which parses it the same
 * defensive way.
 */

import type { BackendKind } from './api/types';

function isRecord(v: unknown): v is Record<string, unknown> {
	return typeof v === 'object' && v !== null && !Array.isArray(v);
}

export interface EndpointLine {
	label: string;
	value: string;
}

/**
 * The mount points a backend of this kind serves, per `CLAUDE.md`'s
 * Addressing section -- fixed by the routing grammar, not per-instance data,
 * so this is safe to compute client-side rather than fetch.
 */
export function backendEndpoints(kind: BackendKind, org: string, project: string): EndpointLine[] {
	const base = `/cache/${org}/${project}`;

	switch (kind) {
		case 'sstate':
			return [{ label: 'Endpoint', value: `${base}/sstate/{path}` }];
		case 'downloads':
			return [{ label: 'Endpoint', value: `${base}/downloads/{basename}` }];
		case 'hashserv':
			return [{ label: 'Endpoint', value: `wss://…${base}/hashserv` }];
		case 'bazel':
			return [
				{ label: 'Endpoint (HTTP)', value: `${base}/{ac,cas}/{hex}` },
				{ label: 'Endpoint (gRPC)', value: `instance_name = ${org}/${project}` }
			];
		case 'oci':
			return [
				{ label: 'Endpoint (containerd/Docker)', value: `${base}/docker/v2/{rest}?ns=` },
				{ label: 'Endpoint (BuildKit/podman)', value: `/v2/${org}/${project}/{rest}?ns=` }
			];
	}
}

// Mirrors internal/cache/oci/config.go's defaultUpstream / defaultTagTTL.
const OCI_DEFAULT_UPSTREAM = 'docker.io';
const OCI_DEFAULT_TAG_TTL = '10m';

export interface OciBackendConfig {
	defaultUpstream: string;
	upstreams: string[];
	tagTtl: string;
}

/** Reads `{default_upstream, upstreams, tag_ttl}`, applying the same defaults the server does. */
export function parseOciConfig(config: unknown): OciBackendConfig {
	const obj = isRecord(config) ? config : {};

	const defaultUpstream =
		typeof obj.default_upstream === 'string' && obj.default_upstream !== ''
			? obj.default_upstream
			: OCI_DEFAULT_UPSTREAM;

	const upstreams = Array.isArray(obj.upstreams)
		? obj.upstreams.filter((u): u is string => typeof u === 'string' && u !== '')
		: [];

	const tagTtl =
		typeof obj.tag_ttl === 'string' && obj.tag_ttl !== '' ? obj.tag_ttl : OCI_DEFAULT_TAG_TTL;

	return { defaultUpstream, upstreams, tagTtl };
}

export interface OciConfigInput {
	defaultUpstream: string;
	/** Comma-separated, as typed into one Input. */
	upstreamsCsv: string;
	tagTtl: string;
}

/** The inverse of `parseOciConfig`: builds the wire `config` object from form state. */
export function buildOciConfig(input: OciConfigInput): Record<string, unknown> {
	const upstreams = input.upstreamsCsv
		.split(',')
		.map((s) => s.trim())
		.filter((s) => s.length > 0);

	return {
		default_upstream: input.defaultUpstream.trim(),
		upstreams,
		tag_ttl: input.tagTtl.trim() || OCI_DEFAULT_TAG_TTL
	};
}

export interface HashservBackendConfig {
	/** `null` means direct-only -- chaining is off, the deliberate default. */
	upstream: string | null;
}

/** Reads `{upstream}`. Absent/empty is "direct only", never an error. */
export function parseHashservConfig(config: unknown): HashservBackendConfig {
	const obj = isRecord(config) ? config : {};
	const upstream = typeof obj.upstream === 'string' && obj.upstream !== '' ? obj.upstream : null;

	return { upstream };
}
