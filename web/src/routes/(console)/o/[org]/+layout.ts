import { error } from '@sveltejs/kit';

import { isApiError } from '$lib/api/errors';
import { getOrg } from '$lib/api/orgs';
import { listProjects } from '$lib/api/projects';
import { setLastOrg } from '$lib/storage';

import type { LayoutLoad } from './$types';

/**
 * Everything under `/o/[org]`.
 *
 * The project list comes from `GET /orgs/{org}/projects` and **never from
 * `me.projects`**: `/me` reports project MEMBERSHIPS only, while org membership
 * already grants read on every project in the org, so an org owner who created
 * nothing has `me.projects: []` and can read all of them. Driving the switcher
 * from `/me` shows an empty list to the person who owns the org.
 *
 * A 404 here is the authorization ladder working as designed -- "cannot view" and
 * "does not exist" are deliberately indistinguishable so the endpoint is not an
 * organization-name oracle -- so it is surfaced with the server's own message
 * rather than translated into a guess about which one it was.
 */
export const load: LayoutLoad = async (event) => {
	await event.parent();

	try {
		const [org, projects] = await Promise.all([
			getOrg(event.params.org, { fetch: event.fetch }),
			listProjects(event.params.org, { fetch: event.fetch })
		]);

		// Remembered only after the org proved readable, so a stale or revoked
		// slug is never written back and cannot pin the next sign-in to a 404.
		setLastOrg(org.slug);

		return { org, projects: projects.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
