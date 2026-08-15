import { describe, expect, it } from 'vitest';

import { DENIED_REASONS, deniedCopy, isDeniedReason } from './deniedReason';

describe('isDeniedReason', () => {
	it('accepts exactly the four reasons the server can redirect with', () => {
		for (const reason of DENIED_REASONS) {
			expect(isDeniedReason(reason)).toBe(true);
		}
	});

	it('rejects null (no ?denied= param at all)', () => {
		expect(isDeniedReason(null)).toBe(false);
	});

	it('rejects the empty string', () => {
		expect(isDeniedReason('')).toBe(false);
	});

	it('rejects a value outside the closed set', () => {
		expect(isDeniedReason('something_else')).toBe(false);
	});
});

describe('deniedCopy', () => {
	it('returns distinct, non-empty copy for each of the four reasons', () => {
		const seen = new Set<string>();

		for (const reason of DENIED_REASONS) {
			const copy = deniedCopy(reason);
			expect(copy).not.toBeNull();
			expect(copy!.title).not.toBe('');
			expect(copy!.body).not.toBe('');

			// Titles must not collide: a reader who has seen one denial card must be
			// able to tell a different reason apart from the title alone.
			expect(seen.has(copy!.title)).toBe(false);
			seen.add(copy!.title);
		}
	});

	it('is null for a value outside the closed set, never a generic fallback card', () => {
		expect(deniedCopy(null)).toBeNull();
		expect(deniedCopy('')).toBeNull();
		expect(deniedCopy('unknown_reason')).toBeNull();
	});

	it('login_gate names the fix (a group) but not a retry', () => {
		const copy = deniedCopy('login_gate')!;
		expect(copy.body.toLowerCase()).not.toContain('try again');
		expect(copy.body.toLowerCase()).not.toContain('sign in again');
	});

	it('stale_request is the one reason that suggests trying again', () => {
		const copy = deniedCopy('stale_request')!;
		expect(copy.body.toLowerCase()).toMatch(/again/);
	});

	it('idp_refused and auth_failed describe the failure without prescribing a retry', () => {
		for (const reason of ['idp_refused', 'auth_failed'] as const) {
			const copy = deniedCopy(reason)!;
			expect(copy.body.toLowerCase()).not.toContain('try again');
			expect(copy.body.toLowerCase()).not.toContain('sign in again');
		}
	});
});
