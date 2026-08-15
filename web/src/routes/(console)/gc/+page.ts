import { error } from '@sveltejs/kit';

import { listGCRuns } from '$lib/api/gc';
import { getInstance } from '$lib/api/instance';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * Site admin. `GET /gc/runs` + `GET /instance`.
 *
 * `include_usage` lives in the URL (`?include_usage=1`) rather than plain
 * component state, so toggling it is a real navigation -- back/forward and a
 * reload land on the same view instead of silently resetting to the
 * memo-mandated default (false: the usage pass mints a run row every six
 * hours forever, and a default-visible listing buries real sweeps within a
 * week).
 */
export const load: PageLoad = async (event) => {
	const includeUsage = event.url.searchParams.get('include_usage') === '1';

	try {
		const [runs, instance] = await Promise.all([
			listGCRuns({ include_usage: includeUsage, limit: 30 }, { fetch: event.fetch }),
			getInstance({ fetch: event.fetch })
		]);

		return { runs: runs.items, nextCursor: runs.next_cursor, includeUsage, instance };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
