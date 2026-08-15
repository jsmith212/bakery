import { error } from '@sveltejs/kit';

import { getOrgUsage } from '$lib/api/orgs';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * B2a, for the danger zone's real byte count. The project count and list
 * already came from the parent `o/[org]` layout -- this load owes only the
 * one number that layout does not carry.
 */
export const load: PageLoad = async (event) => {
	try {
		const usage = await getOrgUsage(event.params.org, { fetch: event.fetch });

		return { usage: usage.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
