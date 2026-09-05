import { error } from '@sveltejs/kit';

import { getProjectUsage } from '$lib/api/projects';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * B2b, for the danger zone's real object and byte counts.
 *
 * `org` and `project` already came from the two parent layouts, so this load owes
 * only what they do not carry -- the same arrangement as the org settings page,
 * which fetches nothing but `getOrgUsage`. It AWAITS rather than streaming: a
 * destructive confirmation that renders before it knows how much it is about to
 * destroy is worse than one that renders a moment later.
 */
export const load: PageLoad = async (event) => {
	try {
		const usage = await getProjectUsage(event.params.org, event.params.project, {
			fetch: event.fetch
		});

		return { usage: usage.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
