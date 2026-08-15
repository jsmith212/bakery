import { get, type RequestOptions } from './client';
import { isApiError } from './errors';
import type { Me } from './types';

/**
 * `GET /me`, returning `null` rather than throwing when nobody is signed in.
 *
 * **There is no 403 case.** A login-gated account never gets a session in the
 * first place -- `ErrLoginNotAllowed` is produced only by the reconciler, which
 * runs inside `HandleCallback`, and the denial there is now a
 * `302 /login?denied=login_gate`. So the only authentication outcome this probe
 * can see is 401, and a client that also handled 403 here would be defending a
 * state it cannot reach while leaving the reachable one to the generic path.
 *
 * `suppressUnauthorized` is set because a 401 here is the ANSWER, not a
 * mid-session expiry: the guard that called this is about to redirect, and the
 * hook firing as well would race it.
 */
export async function getMe(opts?: RequestOptions): Promise<Me | null> {
	try {
		return await get<Me>('/me', { ...opts, suppressUnauthorized: true });
	} catch (err) {
		if (isApiError(err) && err.code === 'unauthorized') return null;

		throw err;
	}
}
