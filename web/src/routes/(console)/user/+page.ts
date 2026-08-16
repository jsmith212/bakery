import { error } from '@sveltejs/kit';

import { listUserTokens } from '$lib/api/tokens';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET /user/tokens`. A personal token roster is small and per-user, so like
 * the project Keys screen this is a cheap read and is awaited here rather
 * than handed to `{#await}`.
 */
export const load: PageLoad = async (event) => {
	try {
		const tokens = await listUserTokens({ fetch: event.fetch });

		return { tokens: tokens.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
