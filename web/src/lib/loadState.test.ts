import { describe, expect, it } from 'vitest';

import { resolveLoadState } from './loadState';

describe('resolveLoadState', () => {
	it('is "loading" while not ready, regardless of emptiness', () => {
		expect(resolveLoadState(false, true)).toBe('loading');
		expect(resolveLoadState(false, false)).toBe('loading');
	});

	it('is "empty" once ready with nothing to show', () => {
		expect(resolveLoadState(true, true)).toBe('empty');
	});

	it('is "loaded" once ready with something to show', () => {
		expect(resolveLoadState(true, false)).toBe('loaded');
	});

	// The named regression: an empty org whose usage promise has not resolved
	// yet must be "loading", never "empty" -- the two used to be independently
	// derived booleans (`isEmpty` from project count alone, `isLoading` from
	// usage-readiness alone) that were BOTH true at once, rendering the
	// skeleton grid and the empty state simultaneously.
	it('never returns "empty" while not ready, even for a zero-length collection', () => {
		expect(resolveLoadState(false, /* isEmpty */ true)).not.toBe('empty');
	});

	it('returns exactly one of the three states for every input combination', () => {
		for (const ready of [true, false]) {
			for (const isEmpty of [true, false]) {
				expect(['loading', 'empty', 'loaded']).toContain(resolveLoadState(ready, isEmpty));
			}
		}
	});
});
