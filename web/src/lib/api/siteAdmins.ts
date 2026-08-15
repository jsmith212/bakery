import { del, get, put, seg, type RequestOptions } from './client';
import type { ListResponse, SiteAdmin, SiteAdminRemoval } from './types';

/**
 * `/site-admins`. Site admin only, and the guard admits NO api-key principal to
 * that level at any scope.
 *
 * `{user}` takes a user id or an email, exactly as the org member routes do.
 *
 * There is no route that grants the FIRST site admin and there cannot be -- it
 * would have to be callable by someone who is not one. That is the CLI
 * break-glass (`bakery user site-admin`), which needs `DB_URL`, not a session.
 */

export function listSiteAdmins(opts?: RequestOptions): Promise<ListResponse<SiteAdmin>> {
	return get<ListResponse<SiteAdmin>>('/site-admins', opts);
}

export function putSiteAdmin(user: string, opts?: RequestOptions): Promise<SiteAdmin> {
	// Bodyless: the role being granted is the route itself.
	return put<SiteAdmin>(`/site-admins/${seg(user)}`, undefined, opts);
}

/**
 * Clears the LOCAL half. `still_a_site_admin` true means a group claim still
 * makes them one -- with every privilege in the installation -- so this reads as
 * a warning, never a success.
 */
export function deleteSiteAdmin(
	user: string,
	opts?: RequestOptions
): Promise<SiteAdminRemoval> {
	return del<SiteAdminRemoval>(`/site-admins/${seg(user)}`, opts);
}
