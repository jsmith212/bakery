import { error } from '@sveltejs/kit';

import { listBackends } from '$lib/api/backends';
import { getProjectUsage } from '$lib/api/projects';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET .../backends` + B2b usage -- the same two reads, in the same shape, as
 * `overview/+page.ts`.
 *
 * Both are cheap single-query reads for one project and are awaited directly
 * rather than handed to `{#await}`: nothing here has a latency that scales with
 * anything the caller does not already know the size of.
 *
 * A project with ZERO backends is a 200 with an empty list, never a 404 -- the
 * 404 invariant is about a KIND that has no `cache_backends` row (nothing to
 * mount), and this route mounts nothing.
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
