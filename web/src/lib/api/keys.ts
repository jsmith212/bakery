import { del, get, post, seg, type RequestOptions } from './client';
import type { APIKey, CreateKeyRequest, CreatedAPIKey, ListResponse } from './types';

/**
 * Project API keys.
 *
 * `ProjectRead` is the floor for all four routes; the real gate is inside
 * `auth.CreateAPIKey`, which requires a `project_memberships` row FOR THE CALLER
 * and caps the requested scope at that role. An org owner or site admin who did
 * not create the project has no such row, so they can open this screen, see it
 * fully populated, and be refused with `403 scope_exceeds_role` -- which is why
 * the mint control is disabled with teaching copy when the caller holds no
 * project role, and why `scope_exceeds_role` has its own error treatment.
 */

export function listKeys(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<ListResponse<APIKey>> {
	return get<ListResponse<APIKey>>(`/orgs/${seg(org)}/projects/${seg(project)}/keys`, opts);
}

/**
 * 201 with the plaintext token, exactly once. Nothing else in the API can return
 * it and the schema stores only its SHA-256, so the reveal is never re-fetchable
 * and the response must never be re-requested by an `invalidate()`.
 */
export function createKey(
	org: string,
	project: string,
	body: CreateKeyRequest,
	opts?: RequestOptions
): Promise<CreatedAPIKey> {
	return post<CreatedAPIKey>(
		`/orgs/${seg(org)}/projects/${seg(project)}/keys`,
		body,
		opts
	);
}

/** 204, idempotent. */
export function revokeKey(
	org: string,
	project: string,
	key: string,
	opts?: RequestOptions
): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/projects/${seg(project)}/keys/${seg(key)}`, opts);
}
