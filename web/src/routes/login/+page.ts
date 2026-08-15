import { getAuthConfig } from '$lib/api/auth';

import type { PageLoad } from './$types';

/**
 * `/auth/config` is PUBLIC and is the only thing this screen needs: it reports
 * whether OIDC is configured, whether dev-login exists on this instance, and
 * which issuer is behind the SSO button.
 *
 * A failure here is not fatal -- the screen still has a terminal state to render
 * -- so the error is returned rather than thrown: a login page that cannot paint
 * is worse than a login page that says the server did not answer.
 */
export const load: PageLoad = async (event) => {
	try {
		return { config: await getAuthConfig({ fetch: event.fetch }), configError: null };
	} catch {
		return { config: null, configError: 'Could not read this server’s sign-in configuration.' };
	}
};
