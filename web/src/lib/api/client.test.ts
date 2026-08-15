import { afterEach, describe, expect, it, vi } from 'vitest';

import {
	del,
	get,
	patch,
	post,
	put,
	resetUnauthorizedHook,
	seg,
	setUnauthorizedHook
} from './client';
import { ApiError, isApiError } from './errors';
import { getMe } from './me';
import {
	emptyResponse,
	fetchFake,
	jsonResponse,
	textResponse
} from '$lib/testing/fetchFake';

afterEach(() => {
	resetUnauthorizedHook();
	vi.restoreAllMocks();
});

describe('the Content-Type matrix', () => {
	// THE 415 TRAP. `requireJSON` accepts a non-GET request only when it carries
	// `Content-Type: application/json`, OR carries no content type AND no bytes.
	// Getting this wrong breaks dev-login and logout first -- which is the first
	// and last thing the console does.
	const cases: {
		name: string;
		call: (f: ReturnType<typeof fetchFake>) => Promise<unknown>;
		wantContentType: boolean;
		wantBody: string | null;
	}[] = [
		{
			name: 'GET sends no content type and no body',
			call: (f) => get('/orgs', { fetch: f.fetch }),
			wantContentType: false,
			wantBody: null
		},
		{
			name: 'POST WITH a body sends the header and the body',
			call: (f) => post('/orgs', { slug: 'acme', name: 'Acme' }, { fetch: f.fetch }),
			wantContentType: true,
			wantBody: '{"slug":"acme","name":"Acme"}'
		},
		{
			name: 'POST with NO body sends neither (dev-login, logout)',
			call: (f) => post('/auth/logout', undefined, { fetch: f.fetch }),
			wantContentType: false,
			wantBody: null
		},
		{
			name: 'PUT with no body sends neither (site-admin grant)',
			call: (f) => put('/site-admins/a%40b.dev', undefined, { fetch: f.fetch }),
			wantContentType: false,
			wantBody: null
		},
		{
			name: 'PATCH sends the header and the body',
			call: (f) => patch('/orgs/acme', { name: 'Acme' }, { fetch: f.fetch }),
			wantContentType: true,
			wantBody: '{"name":"Acme"}'
		},
		{
			name: 'DELETE sends neither',
			call: (f) => del('/orgs/acme', { fetch: f.fetch }),
			wantContentType: false,
			wantBody: null
		}
	];

	for (const c of cases) {
		it(c.name, async () => {
			const f = fetchFake(() => jsonResponse({ ok: true }));
			await c.call(f);

			const call = f.only();
			expect('Content-Type' in call.headers).toBe(c.wantContentType);
			expect(call.body).toBe(c.wantBody);
		});
	}

	it('always sends credentials: same-origin', async () => {
		const f = fetchFake(() => jsonResponse({}));
		await get('/me', { fetch: f.fetch });

		expect(f.only().credentials).toBe('same-origin');
	});
});

describe('url building', () => {
	it('prefixes /api/v1 and drops null and undefined query values', async () => {
		const f = fetchFake(() => jsonResponse({ items: [] }));

		await get('/gc/runs', {
			fetch: f.fetch,
			query: { limit: 50, before: undefined, status: null, include_usage: true }
		});

		expect(f.only().url).toBe('/api/v1/gc/runs?limit=50&include_usage=true');
	});

	it('percent-encodes a path segment, which is how an email reaches {user}', () => {
		expect(seg('alice@corp.example')).toBe('alice%40corp.example');
	});
});

describe('the error envelope', () => {
	it('becomes an ApiError carrying status, code, message and field', async () => {
		const f = fetchFake(() =>
			jsonResponse(
				{
					error: {
						code: 'scope_exceeds_role',
						message: 'a write-scoped key exceeds your role on this project',
						field: 'scope'
					}
				},
				403
			)
		);

		const err = await get('/orgs/acme/projects/firmware/keys', { fetch: f.fetch }).catch(
			(e: unknown) => e
		);

		expect(isApiError(err)).toBe(true);
		const api = err as ApiError;
		expect(api.status).toBe(403);
		expect(api.code).toBe('scope_exceeds_role');
		expect(api.field).toBe('scope');
		expect(api.message).toBe('a write-scoped key exceeds your role on this project');
		// And it is filed under the scope treatment, not the generic 403 arm.
		expect(api.treatment).toBe('scope');
	});

	it('degrades on a body that is not JSON at all instead of throwing a parse error', async () => {
		const f = fetchFake(() => textResponse('<html>502 Bad Gateway</html>', 502));

		const err = await get('/me', { fetch: f.fetch }).catch((e: unknown) => e);

		expect(isApiError(err)).toBe(true);
		const api = err as ApiError;
		expect(api.status).toBe(502);
		expect(api.code).toBe('internal_error');
		// Never the gateway's markup.
		expect(api.message).not.toContain('<html>');
	});

	it('falls back to a status-derived code when the envelope carries an unknown one', async () => {
		const f = fetchFake(() => jsonResponse({ error: { code: 'teapot', message: 'no' } }, 409));

		const err = (await get('/gc/runs', { fetch: f.fetch }).catch((e: unknown) => e)) as ApiError;

		expect(err.code).toBe('conflict');
		expect(err.message).toBe('no');
	});
});

describe('the 401 hook', () => {
	it('fires exactly once for one 401, and before the throw', async () => {
		const hook = vi.fn();
		setUnauthorizedHook(hook);

		const f = fetchFake(() =>
			jsonResponse({ error: { code: 'unauthorized', message: 'sign in' } }, 401)
		);

		await expect(get('/orgs', { fetch: f.fetch })).rejects.toBeInstanceOf(ApiError);
		expect(hook).toHaveBeenCalledTimes(1);
	});

	it('does not fire on a non-401 failure', async () => {
		const hook = vi.fn();
		setUnauthorizedHook(hook);

		const f = fetchFake(() =>
			jsonResponse({ error: { code: 'forbidden', message: 'no' } }, 403)
		);

		await expect(get('/orgs', { fetch: f.fetch })).rejects.toBeInstanceOf(ApiError);
		expect(hook).not.toHaveBeenCalled();
	});

	it('does not fire for getMe, whose 401 is the answer rather than an expiry', async () => {
		const hook = vi.fn();
		setUnauthorizedHook(hook);

		const f = fetchFake(() =>
			jsonResponse({ error: { code: 'unauthorized', message: 'sign in' } }, 401)
		);

		await expect(getMe({ fetch: f.fetch })).resolves.toBeNull();
		expect(hook).not.toHaveBeenCalled();
	});
});

describe('empty responses', () => {
	it('resolves 204 to undefined', async () => {
		const f = fetchFake(() => emptyResponse(204));

		await expect(del('/orgs/acme', { fetch: f.fetch })).resolves.toBeUndefined();
	});

	it('resolves a 200 with an empty body to undefined', async () => {
		const f = fetchFake(() => new Response('', { status: 200 }));

		await expect(post('/auth/logout', undefined, { fetch: f.fetch })).resolves.toBeUndefined();
	});
});
