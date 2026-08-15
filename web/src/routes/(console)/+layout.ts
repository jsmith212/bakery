import { redirect } from '@sveltejs/kit';

import { getAuthConfig } from '$lib/api/auth';
import { listOrgs } from '$lib/api/orgs';
import { loadMe } from '$lib/session';
import { stashReturnPath } from '$lib/storage';

import type { LayoutLoad } from './$types';

/**
 * The console guard.
 *
 * `GET /me` has exactly two outcomes here: a principal, or 401. **There is no
 * 403 branch, and adding one would be defending an unreachable state.** A
 * login-gated account never receives a session at all -- `ErrLoginNotAllowed` is
 * produced only by the reconciler inside `HandleCallback`, which now answers
 * `302 /login?denied=login_gate` -- so the gate is a terminal state on the login
 * screen, not a status code the console ever sees.
 *
 * `event.url` is read ONLY on the 401 path. Reading it unconditionally would
 * register a dependency on the URL and re-run this load (and its two extra
 * requests) on every client-side navigation inside the console.
 */
export const load: LayoutLoad = async (event) => {
	const me = await loadMe(event.fetch);

	if (!me) {
		stashReturnPath(event.url.pathname + event.url.search);
		redirect(302, '/login');
	}

	// `GET /orgs` -- not `me.orgs` -- is the nav's org list: it is filtered by
	// CanViewOrg, so a site admin sees every org while holding no membership.
	//
	// `/auth/config` is public and carries `allow_self_serve_orgs`, which gates
	// the create-organization affordance. Without it the button 403s with no
	// explanation on every installation that restricts org creation.
	const [orgs, authConfig] = await Promise.all([
		listOrgs({ fetch: event.fetch }),
		getAuthConfig({ fetch: event.fetch })
	]);

	return { me, orgs: orgs.items, authConfig };
};
