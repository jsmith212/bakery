import { error } from '@sveltejs/kit';

import { listOrgMembers } from '$lib/api/members';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET /orgs/{org}/members`.
 *
 * The per-project role table (when a project is selected) is loaded
 * reactively from the component itself, not here: which project's roles to
 * show is UI state with no natural home in the URL for this org-scoped
 * route, so it cannot be a `load` dependency.
 */
export const load: PageLoad = async (event) => {
	try {
		const members = await listOrgMembers(event.params.org, { fetch: event.fetch });

		return { members: members.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
