import { del, get, put, seg, type RequestOptions } from './client';
import type {
	ListResponse,
	Member,
	OrgMemberRemoval,
	PutOrgMemberRequest,
	PutProjectMemberRequest
} from './types';

/**
 * Org and project memberships.
 *
 * **`{user}` accepts a user id OR an email address.** `resolveUser` branches on
 * an `@`, so granting a role to somebody who is not yet a member needs no
 * lookup endpoint and no user search: URL-encode the email and PUT. An unknown
 * address is a 404 whose message already says the right thing ("users are
 * provisioned at their first login"), so the screen renders the server's prose
 * rather than inventing its own.
 */

export function listOrgMembers(
	org: string,
	opts?: RequestOptions
): Promise<ListResponse<Member>> {
	return get<ListResponse<Member>>(`/orgs/${seg(org)}/members`, opts);
}

/** Grants or changes the LOCAL half of an org role. The OIDC half is read-only. */
export function putOrgMember(
	org: string,
	user: string,
	body: PutOrgMemberRequest,
	opts?: RequestOptions
): Promise<Member> {
	return put<Member>(`/orgs/${seg(org)}/members/${seg(user)}`, body, opts);
}

/**
 * Clears the LOCAL half only. 200 with a body, not 204 -- and the body is the
 * point: `still_a_member` true means an OIDC claim still admits them, so the
 * result is rendered as a WARNING. A 409 `claim_derived_role` comes back when
 * there was no local grant to clear at all, which on a pure-LDAP deployment is
 * every member; the control is disabled when `local_role` is absent so that 409
 * is a backstop rather than the normal path.
 */
export function deleteOrgMember(
	org: string,
	user: string,
	opts?: RequestOptions
): Promise<OrgMemberRemoval> {
	return del<OrgMemberRemoval>(`/orgs/${seg(org)}/members/${seg(user)}`, opts);
}

/** Every ORG member, with `project_role` empty for those holding no project role. */
export function listProjectMembers(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<ListResponse<Member>> {
	return get<ListResponse<Member>>(`/orgs/${seg(org)}/projects/${seg(project)}/members`, opts);
}

/**
 * Sets a project role.
 *
 * Two couplings the caller must honour: the response is a PARTIAL `Member`
 * (only `user_id` and `project_role` are set), so it must never be merged into a
 * row -- re-fetch; and the same transaction REVOKES every key exceeding the new
 * role, so the project's key list must be invalidated too.
 */
export function putProjectMember(
	org: string,
	project: string,
	user: string,
	body: PutProjectMemberRequest,
	opts?: RequestOptions
): Promise<Member> {
	return put<Member>(
		`/orgs/${seg(org)}/projects/${seg(project)}/members/${seg(user)}`,
		body,
		opts
	);
}

/** 204. Also revokes the user's keys on this project, same transaction. */
export function deleteProjectMember(
	org: string,
	project: string,
	user: string,
	opts?: RequestOptions
): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/projects/${seg(project)}/members/${seg(user)}`, opts);
}
