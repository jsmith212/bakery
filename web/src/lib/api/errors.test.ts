import { describe, expect, it } from 'vitest';

import {
	API_ERROR_CODES,
	ApiError,
	codeForStatus,
	fallbackMessage,
	isApiError,
	isApiErrorCode,
	treatmentFor,
	type ApiErrorCode,
	type ErrorTreatment
} from './errors';

describe('the closed code vocabulary', () => {
	// `internal/api/errors.go` declares thirteen constants. If it grows a
	// fourteenth, this fails -- which is the point: the union is exhaustive over
	// the SERVER's vocabulary, not over the subset a screen happens to have met.
	it('has exactly the thirteen codes errors.go declares', () => {
		expect(API_ERROR_CODES).toHaveLength(13);
		expect(new Set(API_ERROR_CODES).size).toBe(13);
	});

	it('includes claim_derived_role and not_implemented', () => {
		// Both are easy to miss. `claim_derived_role` is the ORDINARY outcome of
		// removing a member on a pure-LDAP deployment; `not_implemented` is in the
		// constants and reachable the day a handler returns it.
		expect(API_ERROR_CODES).toContain('claim_derived_role');
		expect(API_ERROR_CODES).toContain('not_implemented');
	});

	it('recognises its own members and nothing else', () => {
		for (const code of API_ERROR_CODES) expect(isApiErrorCode(code)).toBe(true);
		expect(isApiErrorCode('teapot')).toBe(false);
		expect(isApiErrorCode(undefined)).toBe(false);
		expect(isApiErrorCode(404)).toBe(false);
	});
});

describe('the dispatch table', () => {
	it('is total: every code has a treatment and a fallback message', () => {
		for (const code of API_ERROR_CODES) {
			expect(treatmentFor(code)).toBeTypeOf('string');
			expect(fallbackMessage(code).length).toBeGreaterThan(0);
		}
	});

	const cases: { code: ApiErrorCode; want: ErrorTreatment; why: string }[] = [
		{ code: 'unauthorized', want: 'session', why: 'central hook, never a toast' },
		{
			code: 'scope_exceeds_role',
			want: 'scope',
			why: 'a 403 that must NOT land in the generic 403 arm'
		},
		{
			code: 'claim_derived_role',
			want: 'in_place',
			why: 'a 409 that is an explanation, not a red toast'
		},
		{ code: 'forbidden', want: 'in_place', why: 'signed in, not allowed here' },
		{ code: 'validation_failed', want: 'field', why: 'attaches to the input' },
		{ code: 'reserved_slug', want: 'field', why: 'same 422, same field treatment' },
		{ code: 'invalid_slug', want: 'field', why: 'same 422, same field treatment' },
		{ code: 'not_found', want: 'context', why: 'EmptyState on load, toast on mutation' },
		{ code: 'conflict', want: 'toast', why: 'the server message IS the value' },
		{ code: 'not_implemented', want: 'toast', why: 'generic, but named' }
	];

	for (const c of cases) {
		it(`${c.code} -> ${c.want} (${c.why})`, () => {
			expect(treatmentFor(c.code)).toBe(c.want);
		});
	}
});

describe('codeForStatus', () => {
	// The ONLY place a status is read, and only to give a NON-envelope a code so
	// that everything downstream still branches on a code.
	const cases: { status: number; want: ApiErrorCode }[] = [
		{ status: 400, want: 'bad_request' },
		{ status: 401, want: 'unauthorized' },
		{ status: 403, want: 'forbidden' },
		{ status: 404, want: 'not_found' },
		{ status: 409, want: 'conflict' },
		{ status: 415, want: 'unsupported_media_type' },
		{ status: 422, want: 'validation_failed' },
		{ status: 500, want: 'internal_error' },
		{ status: 501, want: 'not_implemented' },
		{ status: 502, want: 'internal_error' }
	];

	for (const c of cases) {
		it(`${c.status} -> ${c.want}`, () => {
			expect(codeForStatus(c.status)).toBe(c.want);
		});
	}
});

describe('ApiError', () => {
	it('is recognisable and carries its treatment', () => {
		const err = new ApiError(403, 'scope_exceeds_role', 'nope', 'scope');

		expect(isApiError(err)).toBe(true);
		expect(isApiError(new Error('nope'))).toBe(false);
		expect(err.treatment).toBe('scope');
		expect(err.name).toBe('ApiError');
	});
});
