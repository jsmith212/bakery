import type { FetchLike } from '$lib/api/client';

/**
 * A hand-written `fetch` fake.
 *
 * No MSW and no nock: the repo's convention is hand-written fakes (Go's
 * `mocks_test.go` does the same thing), and the whole point of these tests is to
 * inspect the request the client BUILT -- the header matrix, the presence of a
 * body -- which a network-level interceptor would obscure rather than reveal.
 */

export interface RecordedCall {
	url: string;
	method: string;
	headers: Record<string, string>;
	credentials: RequestCredentials | undefined;
	/** The serialized body, or null when the client sent none. */
	body: string | null;
}

export interface FetchFake {
	fetch: FetchLike;
	calls: RecordedCall[];
	/** The single call, asserting there was exactly one. */
	only(): RecordedCall;
}

export type Responder = (call: RecordedCall) => Response | Promise<Response>;

export function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

/** A body that is not JSON at all -- a gateway's HTML, the degradation case. */
export function textResponse(body: string, status: number): Response {
	return new Response(body, { status, headers: { 'Content-Type': 'text/html' } });
}

export function emptyResponse(status = 204): Response {
	return new Response(null, { status });
}

export function fetchFake(responder: Responder | Response): FetchFake {
	const calls: RecordedCall[] = [];

	const impl = (async (input: RequestInfo | URL, init?: RequestInit) => {
		const headers: Record<string, string> = {};
		for (const [k, v] of Object.entries((init?.headers ?? {}) as Record<string, string>)) {
			headers[k] = v;
		}

		const call: RecordedCall = {
			url: String(input),
			method: init?.method ?? 'GET',
			headers,
			credentials: init?.credentials,
			body: typeof init?.body === 'string' ? init.body : null
		};

		calls.push(call);

		return typeof responder === 'function' ? responder(call) : responder.clone();
	}) as FetchLike;

	return {
		fetch: impl,
		calls,
		only() {
			if (calls.length !== 1) {
				throw new Error(`expected exactly one request, got ${calls.length}`);
			}

			return calls[0];
		}
	};
}
