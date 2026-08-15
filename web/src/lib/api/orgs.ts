import { del, get, patch, post, seg, type RequestOptions } from './client';
import type {
	CreateOrgRequest,
	GCActivityList,
	ListResponse,
	Org,
	OrgProjectUsage,
	UpdateOrgRequest
} from './types';

/**
 * `/orgs`.
 *
 * `GET /orgs` is filtered by `CanViewOrg`, so a site admin sees every org in the
 * installation -- each with NO `role` field, because they are not a member. That
 * is why the nav's org list comes from here and every role gate goes through
 * `$lib/roles`.
 */
export function listOrgs(opts?: RequestOptions): Promise<ListResponse<Org>> {
	return get<ListResponse<Org>>('/orgs', opts);
}

/**
 * `POST /orgs`. 201.
 *
 * Refused with 403 `forbidden` when the installation sets
 * `--allow-self-serve-orgs=false` and the caller is not a site admin -- which is
 * why the affordance is gated on `auth_config.allow_self_serve_orgs` rather than
 * discovered by trying.
 *
 * The creator is granted a LOCAL owner role in the same transaction, so the
 * returned `Org.role` is `owner`.
 */
export function createOrg(body: CreateOrgRequest, opts?: RequestOptions): Promise<Org> {
	return post<Org>('/orgs', body, opts);
}

export function getOrg(org: string, opts?: RequestOptions): Promise<Org> {
	return get<Org>(`/orgs/${seg(org)}`, opts);
}

/** `PATCH /orgs/{org}`. The slug is immutable; the two defaults are three-state. */
export function updateOrg(
	org: string,
	body: UpdateOrgRequest,
	opts?: RequestOptions
): Promise<Org> {
	return patch<Org>(`/orgs/${seg(org)}`, body, opts);
}

/** `DELETE /orgs/{org}`. 204. Org owner only. */
export function deleteOrg(org: string, opts?: RequestOptions): Promise<void> {
	return del<void>(`/orgs/${seg(org)}`, opts);
}

/** B2a. `measured_at` is always on the wire so staleness is renderable. */
export function getOrgUsage(
	org: string,
	opts?: RequestOptions
): Promise<ListResponse<OrgProjectUsage>> {
	return get<ListResponse<OrgProjectUsage>>(`/orgs/${seg(org)}/usage`, opts);
}

/**
 * B7. The org's own retention history: recent runs' per-backend rows, joined to
 * this org's projects only. A row at 0/0 means "swept, nothing eligible"; NO row
 * means "not swept" -- and only the second is a reason to look at the GC config.
 */
export function getOrgGCActivity(
	org: string,
	query?: { before?: number; limit?: number },
	opts?: RequestOptions
): Promise<GCActivityList> {
	return get<GCActivityList>(`/orgs/${seg(org)}/gc/activity`, { ...opts, query });
}
