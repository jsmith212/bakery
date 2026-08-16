import { del, get, post, seg, type RequestOptions } from './client';
import type { CreatedUserToken, CreateUserTokenRequest, ListResponse, UserToken } from './types';

/**
 * Personal access tokens (`bkru_`): `/user/tokens`.
 *
 * Every route here is `AccessUser` on the server -- a verified human, session
 * or bearer, never a machine and never another user's tokens. There is no
 * `{user}` segment anywhere: a token is always the CALLER's, scoped by
 * `user_id` in the query's own predicate, not by a handler check.
 */

export function listUserTokens(opts?: RequestOptions): Promise<ListResponse<UserToken>> {
	return get<ListResponse<UserToken>>('/user/tokens', opts);
}

/**
 * 201 with the plaintext token, exactly once. The schema stores only its
 * SHA-256, so the reveal is never re-fetchable and the response must never be
 * re-requested by an `invalidate()`.
 */
export function createUserToken(
	body: CreateUserTokenRequest,
	opts?: RequestOptions
): Promise<CreatedUserToken> {
	return post<CreatedUserToken>('/user/tokens', body, opts);
}

/** 204, idempotent -- a double-revoke is not an error. */
export function revokeUserToken(id: string, opts?: RequestOptions): Promise<void> {
	return del<void>(`/user/tokens/${seg(id)}`, opts);
}
