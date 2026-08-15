import { error } from '@sveltejs/kit';

import { listBackends } from '$lib/api/backends';
import { getProjectUsage } from '$lib/api/projects';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET .../backends` + B2b usage.
 *
 * Both cheap, single-query reads for one project, awaited directly rather
 * than handed to `{#await}` -- unlike the org projects grid (which fans out
 * over every project in the org), there is nothing here whose latency scales
 * with anything the caller does not already know the size of.
 *
 * The object browser (B3) deliberately does NOT reload here: it already has
 * a home on `backends/[type]`, and duplicating a keyset-paginated per-object
 * table on the project's front page would be the same feature rendered
 * twice, not two different ones.
 */
export const load: PageLoad = async (event) => {
	const { org, project } = event.params;

	try {
		const [backends, usage] = await Promise.all([
			listBackends(org, project, { fetch: event.fetch }),
			getProjectUsage(org, project, { fetch: event.fetch })
		]);

		return { backends: backends.items, usage: usage.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
