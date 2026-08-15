import { API_PREFIX, get, post, type RequestOptions } from './client';
import type { AuthConfig, DevLoginResponse } from './types';

/**
 * `/auth/*`.
 *
 * The SSO entry point is deliberately NOT here as a function: `GET
 * /api/v1/auth/login` answers a 302 that leaves this origin entirely, so it must
 * be a top-level navigation (`<a href>` / `location.assign`). Fetching it would
 * either follow the redirect chain into the IdP under CORS and fail, or -- worse
 * -- succeed at fetching a login page nobody can see.
 */
export const SSO_LOGIN_URL = `${API_PREFIX}/auth/login`;

export function getAuthConfig(opts?: RequestOptions): Promise<AuthConfig> {
	return get<AuthConfig>('/auth/config', opts);
}

/**
 * `POST /auth/dev-login`. **Bodyless**: the endpoint ignores any body it is
 * sent, and this client sends none so the request carries no `Content-Type`
 * either. `requireJSON` would accept a body here too (it only rejects a
 * content type that parses as something OTHER than JSON -- the three shapes a
 * cross-site `<form>` can send); omitting one is a courtesy, not something a
 * 415 requires.
 *
 * The route does not exist unless the server booted with `DEV_LOGIN_ENABLED`, so
 * calling it on a production instance is a 404, not a 403 -- there is nothing to
 * probe.
 */
export function devLogin(opts?: RequestOptions): Promise<DevLoginResponse> {
	return post<DevLoginResponse>('/auth/dev-login', undefined, opts);
}

/** `POST /auth/logout`. Bodyless, 204, idempotent. */
export function logout(opts?: RequestOptions): Promise<void> {
	return post<void>('/auth/logout', undefined, opts);
}
