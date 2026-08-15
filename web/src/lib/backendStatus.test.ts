import { describe, expect, it } from 'vitest';

import { backendStatus, gcStatusBadge, isUnmeasured } from './backendStatus';
import type { BackendKind, GCRunStatus, ProjectBackendUsage } from '$lib/api/types';

function usage(over: Partial<ProjectBackendUsage>): ProjectBackendUsage {
	return {
		kind: 'sstate',
		objects_count: 0,
		logical_bytes: 0,
		measured_at: '2026-08-15T06:00:00.000Z',
		quota_bytes: null,
		retention_window: null,
		...over
	};
}

describe('backendStatus', () => {
	const cases: {
		name: string;
		kind: BackendKind;
		enabled: boolean;
		usage: ProjectBackendUsage | null;
		want: { status: string; caption: boolean };
	}[] = [
		{
			name: 'disabled beats everything, including stored bytes',
			kind: 'sstate',
			enabled: false,
			usage: usage({ logical_bytes: 228076715213 }),
			want: { status: 'stale', caption: false }
		},
		{
			name: 'hashserv with NO usage row is healthy when enabled',
			// The critique's finding 7: hashserv structurally never gets a
			// cache_backend_usage row (M6's planner gives it no stages), so a mapper
			// keyed on bytes labels a hashserv serving the whole fleet `idle` forever.
			kind: 'hashserv',
			enabled: true,
			usage: null,
			want: { status: 'hit', caption: false }
		},
		{
			name: 'hashserv disabled is stale, not idle',
			kind: 'hashserv',
			enabled: false,
			usage: null,
			want: { status: 'stale', caption: false }
		},
		{
			name: 'another kind with NO usage row is "not measured", WITH a caption',
			// --gc-usage-interval defaults to six hours, so a backend created and
			// filled at 09:00 has no row until the next pass. That is a measurement
			// that has not happened, not a backend that is idle.
			kind: 'sstate',
			enabled: true,
			usage: null,
			want: { status: 'idle', caption: true }
		},
		{
			name: 'a usage row with a null measured_at is also "not measured"',
			kind: 'bazel',
			enabled: true,
			usage: usage({ kind: 'bazel', measured_at: null, logical_bytes: null }),
			want: { status: 'idle', caption: true }
		},
		{
			name: 'measured and empty is an honest idle, with no caption',
			kind: 'downloads',
			enabled: true,
			usage: usage({ kind: 'downloads', logical_bytes: 0 }),
			want: { status: 'idle', caption: false }
		},
		{
			name: 'measured with bytes is a hit',
			kind: 'sstate',
			enabled: true,
			usage: usage({ logical_bytes: 228076715213 }),
			want: { status: 'hit', caption: false }
		},
		{
			name: 'at the quota is a miss, checked BEFORE "has bytes"',
			kind: 'sstate',
			enabled: true,
			usage: usage({ logical_bytes: 500, quota_bytes: 500 }),
			want: { status: 'miss', caption: false }
		},
		{
			name: 'over the quota is a miss',
			kind: 'oci',
			enabled: true,
			usage: usage({ kind: 'oci', logical_bytes: 900, quota_bytes: 500 }),
			want: { status: 'miss', caption: false }
		},
		{
			name: 'no quota means a full backend is still just storing',
			kind: 'sstate',
			enabled: true,
			usage: usage({ logical_bytes: 9_000_000, quota_bytes: null }),
			want: { status: 'hit', caption: false }
		}
	];

	for (const c of cases) {
		it(c.name, () => {
			const got = backendStatus({ kind: c.kind, enabled: c.enabled, usage: c.usage });

			expect(got.status).toBe(c.want.status);
			expect(got.caption !== null).toBe(c.want.caption);
			expect(got.label.length).toBeGreaterThan(0);
		});
	}

	it('is total: undefined usage behaves exactly like null', () => {
		expect(backendStatus({ kind: 'sstate', enabled: true })).toEqual(
			backendStatus({ kind: 'sstate', enabled: true, usage: null })
		);
	});

	it('never renders an unmeasured backend as zero bytes', () => {
		expect(isUnmeasured(null)).toBe(true);
		expect(isUnmeasured(usage({ measured_at: null }))).toBe(true);
		expect(isUnmeasured(usage({}))).toBe(false);
	});
});

describe('gcStatusBadge', () => {
	// One producer for `gc/+page.svelte`'s run table AND the org projects
	// "Retention activity" card (B7) -- they used to be two independently
	// hand-rolled mappings, free to drift.
	const cases: { status: GCRunStatus; want: 'hit' | 'miss' | 'stale' }[] = [
		{ status: 'succeeded', want: 'hit' },
		{ status: 'failed', want: 'miss' },
		{ status: 'running', want: 'stale' }
	];

	for (const c of cases) {
		it(`maps ${c.status} to ${c.want}`, () => {
			expect(gcStatusBadge(c.status)).toBe(c.want);
		});
	}

	it('covers every GCRunStatus member', () => {
		const covered = new Set(cases.map((c) => c.status));
		const all: GCRunStatus[] = ['running', 'succeeded', 'failed'];

		for (const s of all) expect(covered.has(s)).toBe(true);
	});
});
