import { redirect } from '@sveltejs/kit';

import { legacyTarget } from '$lib/legacy';

import type { PageLoad } from './$types';

/** Legacy flat route. Resolves the caller's tenancy and forwards. */
export const load: PageLoad = async (event) => {
	const parent = await event.parent();

	redirect(302, legacyTarget(parent, { org: 'projects', project: 'overview' }));
};
