import { error } from '@sveltejs/kit';

import { listKeys } from '$lib/api/keys';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET .../{project}/keys`. A project's key roster is small (per-user,
 * per-project), so unlike the object browser this is a cheap read and is
 * awaited here rather than handed to `{#await}`.
 *
 * The caller's OWN project role -- which gates the mint control (spec §2.0) --
 * is not fetched separately: `data.project.role` (from the parent layout's
 * `GET /orgs/{org}/projects`) already reflects the same `project_memberships`
 * row `auth.CreateAPIKey` checks; see `internal/api/types.go`'s `newProject`.
 */
export const load: PageLoad = async (event) => {
	try {
		const keys = await listKeys(event.params.org, event.params.project, { fetch: event.fetch });

		return { keys: keys.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
