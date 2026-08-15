import { redirect } from '@sveltejs/kit';

import { listOrgs } from '$lib/api/orgs';
import { loadMe } from '$lib/session';
import { lastOrg, lastProject, takeReturnPath } from '$lib/storage';
import { resolveLanding } from '$lib/tenancy';

import type { PageLoad } from './$types';

/**
 * `/` resolves a tenant and forwards. It renders nothing itself.
 *
 * The order is: stashed return path -> remembered org -> the caller's first
 * membership -> the first org they can see at all -> `/orgs`. See
 * `$lib/tenancy` for why each step is there; the third and fourth are separate
 * because a site admin has no memberships and every visibility.
 *
 * `GET /orgs` is fetched unconditionally rather than only for site admins: it is
 * one small request on a landing that is about to make several anyway, and it is
 * what makes the remembered org checkable against real visibility instead of
 * trusted blindly out of `localStorage`.
 */
export const load: PageLoad = async (event) => {
	const me = await loadMe(event.fetch);

	if (!me) {
		// `/` itself is not worth returning to, so nothing is stashed for it --
		// but a deep link that bounced through here is.
		redirect(302, '/login');
	}

	const orgs = await listOrgs({ fetch: event.fetch });

	redirect(
		302,
		resolveLanding({
			me,
			orgs: orgs.items,
			returnPath: takeReturnPath(),
			lastOrg: lastOrg(),
			lastProject: lastProject()
		})
	);
};
