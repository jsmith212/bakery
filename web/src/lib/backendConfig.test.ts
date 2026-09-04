import { describe, expect, it } from 'vitest';

import {
	backendEndpoints,
	buildOciConfig,
	parseHashservConfig,
	parseOciConfig,
	quotaApplicable
} from './backendConfig';
import { BACKEND_KINDS } from './api/types';

describe('parseOciConfig', () => {
	it('applies the server defaults for an empty config', () => {
		expect(parseOciConfig({})).toEqual({
			defaultUpstream: 'docker.io',
			upstreams: [],
			tagTtl: '10m'
		});
	});

	it('is total over null/undefined/non-object config, same as the server', () => {
		expect(parseOciConfig(null)).toEqual({
			defaultUpstream: 'docker.io',
			upstreams: [],
			tagTtl: '10m'
		});
		expect(parseOciConfig(undefined)).toEqual({
			defaultUpstream: 'docker.io',
			upstreams: [],
			tagTtl: '10m'
		});
		expect(parseOciConfig('not an object')).toEqual({
			defaultUpstream: 'docker.io',
			upstreams: [],
			tagTtl: '10m'
		});
	});

	it('reads a real config', () => {
		expect(
			parseOciConfig({
				default_upstream: 'ghcr.io',
				upstreams: ['ghcr.io', 'quay.io'],
				tag_ttl: '1h'
			})
		).toEqual({ defaultUpstream: 'ghcr.io', upstreams: ['ghcr.io', 'quay.io'], tagTtl: '1h' });
	});

	it('drops non-string entries from upstreams rather than throwing', () => {
		expect(parseOciConfig({ upstreams: ['ghcr.io', 42, null] }).upstreams).toEqual(['ghcr.io']);
	});
});

describe('buildOciConfig', () => {
	it('splits and trims a comma-separated upstream list', () => {
		expect(
			buildOciConfig({ defaultUpstream: 'docker.io', upstreamsCsv: ' ghcr.io, quay.io ,', tagTtl: '10m' })
		).toEqual({ default_upstream: 'docker.io', upstreams: ['ghcr.io', 'quay.io'], tag_ttl: '10m' });
	});

	it('falls back to the default TTL when the field is left blank', () => {
		expect(
			buildOciConfig({ defaultUpstream: 'docker.io', upstreamsCsv: '', tagTtl: '  ' })
		).toEqual({ default_upstream: 'docker.io', upstreams: [], tag_ttl: '10m' });
	});

	it('round-trips through parseOciConfig', () => {
		const built = buildOciConfig({
			defaultUpstream: 'ghcr.io',
			upstreamsCsv: 'ghcr.io,quay.io',
			tagTtl: '1h'
		});
		expect(parseOciConfig(built)).toEqual({
			defaultUpstream: 'ghcr.io',
			upstreams: ['ghcr.io', 'quay.io'],
			tagTtl: '1h'
		});
	});
});

describe('backendEndpoints', () => {
	it('names the two independent mount families for bazel', () => {
		const eps = backendEndpoints('bazel', 'acme', 'firmware');
		expect(eps).toHaveLength(2);
		expect(eps[0].value).toContain('/cache/acme/firmware/{ac,cas}');
		expect(eps[1].value).toBe('instance_name = acme/firmware');
	});

	it('names both route families for oci -- containerd/Docker and BuildKit/podman', () => {
		const eps = backendEndpoints('oci', 'acme', 'firmware');
		expect(eps[0].value).toBe('/cache/acme/firmware/docker/v2/{rest}?ns=');
		expect(eps[1].value).toBe('/v2/acme/firmware/{rest}?ns=');
	});

	it('gives sstate a single blob-store endpoint', () => {
		expect(backendEndpoints('sstate', 'acme', 'firmware')).toEqual([
			{ label: 'Endpoint', value: '/cache/acme/firmware/sstate/{path}' }
		]);
	});

	it('names both route families for registry -- the writable buildcache subtree', () => {
		const eps = backendEndpoints('registry', 'acme', 'firmware');
		expect(eps[0].value).toBe('/cache/acme/firmware/docker/v2/buildcache/{rest}');
		expect(eps[1].value).toBe('/v2/acme/firmware/buildcache/{rest}');
	});
});

describe('parseHashservConfig', () => {
	it('reports no chaining for an empty config', () => {
		expect(parseHashservConfig({})).toEqual({ upstream: null });
		expect(parseHashservConfig(null)).toEqual({ upstream: null });
	});

	it('reads a configured upstream', () => {
		expect(parseHashservConfig({ upstream: 'wss://hashserv.example/ws' })).toEqual({
			upstream: 'wss://hashserv.example/ws'
		});
	});
});

describe('quotaApplicable', () => {
	// Mirrors internal/api/backends.go's backendQuotaPatch. The three screens
	// that ask (new, detail, index) all read this one answer, so a kind that
	// drifts from the server's rule cannot drift on only one of them.
	it('refuses a quota for hashserv and oci, and accepts one for everything else', () => {
		expect(quotaApplicable('hashserv')).toBe(false);
		expect(quotaApplicable('oci')).toBe(false);

		for (const kind of BACKEND_KINDS) {
			if (kind === 'hashserv' || kind === 'oci') continue;
			expect(quotaApplicable(kind)).toBe(true);
		}
	});

	// registry is the one that looks like oci and is not: mode=max exports run
	// multi-GB with no upstream ceiling to bound them.
	it('accepts a quota for registry', () => {
		expect(quotaApplicable('registry')).toBe(true);
	});
});

describe('BACKEND_KINDS', () => {
	// The list is the console's only runtime enumeration of the union -- the
	// route guard, the new-backend tiles, the overview's "of N kinds" and the
	// index's all-configured check all read it. A kind added to the union and
	// forgotten here would type-check everywhere and silently vanish from all
	// four; `backendEndpoints`' switch IS total over the union, so requiring an
	// endpoint for every listed kind ties the two together.
	it('lists every kind exactly once, and each one has a real endpoint', () => {
		expect(new Set(BACKEND_KINDS).size).toBe(BACKEND_KINDS.length);

		for (const kind of BACKEND_KINDS) {
			const eps = backendEndpoints(kind, 'acme', 'firmware');
			expect(eps.length).toBeGreaterThan(0);
			expect(eps[0].value).not.toBe('');
		}
	});
});
