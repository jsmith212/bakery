import { ApiError, codeForStatus, fallbackMessage, isApiErrorCode } from './errors';

/**
 * The one place in the console that talks to `/api/v1`.
 *
 * Three rules are load-bearing, and each of them is a silent breakage if it is
 * got wrong:
 *
 *  1. **`Content-Type: application/json` is sent IF AND ONLY IF there is a
 *     body.** `internal/api/authz.go`'s `requireJSON` is the CSRF defence and it
 *     has no token: a non-GET request must either carry
 *     `Content-Type: application/json` (with or without a body -- the check is on
 *     the header, not the bytes), or carry NO content type AND no bytes. What it
 *     rejects with a 415 is the THIRD shape: a content type that parses but isn't
 *     JSON -- `application/x-www-form-urlencoded`, `multipart/form-data`,
 *     `text/plain` -- the three a cross-site `<form>` can send without a CORS
 *     preflight. `POST /auth/dev-login` and `POST /auth/logout` are both
 *     bodyless; this client omits the header for them too, purely so a request
 *     with nothing to say looks like nothing was said.
 *  2. **`credentials: 'same-origin'`, always, explicitly.** The session is a
 *     cookie and there is no bearer token in the SPA at all. SvelteKit's
 *     `load`-scoped fetch does not carry cookies the way `window.fetch` does
 *     unless it is told to.
 *  3. **Every non-2xx becomes an `ApiError` with a CODE**, including responses
 *     that are not Bakery's envelope at all (a proxy's HTML 502). Downstream
 *     code branches on the code and never on the status or the prose.
 */

export const API_PREFIX = '/api/v1';

export type FetchLike = typeof globalThis.fetch;

export type QueryValue = string | number | boolean | null | undefined;

export interface RequestOptions {
	/** Appended as a query string; `null`/`undefined` values are dropped. */
	query?: Record<string, QueryValue>;
	/** The `load`-scoped fetch when called from a `+page.ts`/`+layout.ts`. */
	fetch?: FetchLike;
	signal?: AbortSignal;
	/**
	 * Do not fire the central 401 hook for this request.
	 *
	 * Exactly one caller sets it: `getMe`, which is the guard's own probe. A
	 * 401 there is the ANSWER ("nobody is signed in"), not a mid-session
	 * expiry, and firing the hook would make the guard's own redirect race a
	 * second one.
	 */
	suppressUnauthorized?: boolean;
}

type UnauthorizedHook = () => void;

let unauthorizedHook: UnauthorizedHook | null = null;

/**
 * Registers the mid-session-401 handler. The root layout wires it to stash the
 * current path and navigate to `/login`.
 *
 * The layout guard alone is not enough and neither is this alone: the guard only
 * fires on navigation, and this only fires on a request. They cover different
 * moments, and a session that expires while a screen is open is the second one.
 */
export function setUnauthorizedHook(hook: UnauthorizedHook | null): void {
	unauthorizedHook = hook;
}

/** Test seam: forget the registered hook. */
export function resetUnauthorizedHook(): void {
	unauthorizedHook = null;
}

function buildURL(path: string, query?: Record<string, QueryValue>): string {
	const url = path.startsWith('/api/') ? path : API_PREFIX + path;
	if (!query) return url;

	const params = new URLSearchParams();
	for (const [key, value] of Object.entries(query)) {
		if (value === undefined || value === null) continue;
		params.set(key, String(value));
	}

	const qs = params.toString();
	return qs ? `${url}?${qs}` : url;
}

/**
 * Turns a non-2xx response into an `ApiError`.
 *
 * The body is read as TEXT and parsed by hand rather than with `res.json()`:
 * a `res.json()` on an HTML error page throws a SyntaxError that would surface
 * to the screen as "Unexpected token <", which tells the user nothing and hides
 * the status that would have.
 */
async function toApiError(res: Response): Promise<ApiError> {
	const status = res.status;
	let code = codeForStatus(status);
	let message = '';
	let field: string | undefined;

	let raw = '';
	try {
		raw = await res.text();
	} catch {
		raw = '';
	}

	if (raw) {
		try {
			const body: unknown = JSON.parse(raw);
			const envelope =
				typeof body === 'object' && body !== null
					? (body as { error?: { code?: unknown; message?: unknown; field?: unknown } }).error
					: undefined;

			if (envelope && isApiErrorCode(envelope.code)) code = envelope.code;
			if (envelope && typeof envelope.message === 'string') message = envelope.message;
			if (envelope && typeof envelope.field === 'string' && envelope.field !== '') {
				field = envelope.field;
			}
		} catch {
			// Not JSON. Keep the status-derived code and the fallback message: a
			// gateway's HTML must never be rendered into the console.
		}
	}

	return new ApiError(status, code, message || fallbackMessage(code), field);
}

/**
 * Issues one request.
 *
 * `T` is the decoded body. A 204 (and any empty body) resolves to `undefined`,
 * so an endpoint that answers 204 is called as `request<void>(...)`.
 */
export async function request<T = void>(
	method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE',
	path: string,
	body?: unknown,
	opts: RequestOptions = {}
): Promise<T> {
	const doFetch = opts.fetch ?? globalThis.fetch;
	const headers: Record<string, string> = { Accept: 'application/json' };

	const init: RequestInit = {
		method,
		credentials: 'same-origin',
		headers,
		signal: opts.signal
	};

	// THE 415 TRAP. The header and the body are set together or not at all.
	if (body !== undefined) {
		headers['Content-Type'] = 'application/json';
		init.body = JSON.stringify(body);
	}

	const res = await doFetch(buildURL(path, opts.query), init);

	if (!res.ok) {
		if (res.status === 401 && !opts.suppressUnauthorized) {
			// Exactly once per 401 response, and BEFORE the throw so a caller that
			// swallows the error still gets the session handled.
			unauthorizedHook?.();
		}

		throw await toApiError(res);
	}

	if (res.status === 204) return undefined as T;

	const text = await res.text();
	if (text === '') return undefined as T;

	return JSON.parse(text) as T;
}

export function get<T>(path: string, opts?: RequestOptions): Promise<T> {
	return request<T>('GET', path, undefined, opts);
}

/**
 * `body` is optional on purpose: `post(path)` sends no body and no
 * `Content-Type`, which is the shape `/auth/dev-login` and `/auth/logout`
 * require.
 */
export function post<T = void>(path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
	return request<T>('POST', path, body, opts);
}

export function put<T = void>(path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
	return request<T>('PUT', path, body, opts);
}

export function patch<T = void>(path: string, body: unknown, opts?: RequestOptions): Promise<T> {
	return request<T>('PATCH', path, body, opts);
}

/** DELETE never carries a body in this API; several of them answer 200 with one. */
export function del<T = void>(path: string, opts?: RequestOptions): Promise<T> {
	return request<T>('DELETE', path, undefined, opts);
}

/** Percent-encodes one path segment. Emails appear in `{user}` and contain `@`. */
export function seg(value: string): string {
	return encodeURIComponent(value);
}
