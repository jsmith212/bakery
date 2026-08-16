import { error } from '@sveltejs/kit';

import { listOrgMembers } from '$lib/api/members';
import { listRobots } from '$lib/api/robots';
import { isApiError } from '$lib/api/errors';
import { canAdminOrg } from '$lib/roles';

import type { PageLoad } from './$types';

/**
 * `GET /orgs/{org}/members` (+ `GET /orgs/{org}/robots` for an org admin).
 *
 * The per-project role table (when a project is selected) is loaded
 * reactively from the component itself, not here: which project's roles to
 * show is UI state with no natural home in the URL for this org-scoped
 * route, so it cannot be a `load` dependency.
 *
 * Robots are fetched here, GATED on `canAdminOrg`, rather than unconditionally:
 * `GET .../robots` is `AccessOrgAdmin` on the server, so a non-admin's request
 * would be a guaranteed 403 -- and the ROBOTS card itself only renders for an
 * admin, so there is nothing to show a non-admin the failure for. `robots`
 * being `[]` for a non-admin therefore means "not fetched", not "org has none".
 *
 * The members request fires BEFORE `event.parent()` is awaited, not after: the
 * roster is this page's primary content, and it does not depend on anything the
 * parent layouts load (`/me`, `/orgs`, `/orgs/{org}`, `/orgs/{org}/projects`) --
 * waiting for all of that first would put the one thing the page is FOR strictly
 * last. And the robots leg is independently `.catch`-guarded: `admin` is derived
 * from `parent.me`, which the epoch-keyed principal cache can legitimately serve
 * one request stale, so a just-demoted admin's `GET .../robots` can 403 even
 * though the ROBOTS card renders. A rejection there must degrade to "not
 * fetched" (the card's own documented meaning of `[]`), never take the members
 * table down with it via a shared `Promise.all`.
 */
export const load: PageLoad = async (event) => {
	const membersP = listOrgMembers(event.params.org, { fetch: event.fetch });

	const parent = await event.parent();
	const admin = canAdminOrg(parent.me, parent.org);

	const robotsP = admin
		? listRobots(event.params.org, { fetch: event.fetch }).catch(() => ({ items: [] }))
		: Promise.resolve({ items: [] });

	try {
		const [members, robots] = await Promise.all([membersP, robotsP]);

		return { members: members.items, robots: robots.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
