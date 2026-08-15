import { error } from '@sveltejs/kit';

import { listSiteAdmins } from '$lib/api/siteAdmins';
import { getInstance } from '$lib/api/instance';
import { isApiError } from '$lib/api/errors';

import type { PageLoad } from './$types';

/**
 * `GET /site-admins` + `GET /instance`. Site admin only, and the guard admits
 * no API-key principal at any scope -- so reaching this page at all already
 * implies every mutation on it is available; there is no read-only site-admin.
 *
 * `instance.allow_local_site_admins` gates the Grant affordance: when it is
 * off, `handlePutSiteAdmin` refuses EVERY grant with a 403 naming the flag
 * (`internal/api/siteadmins.go`), so the console must not offer a control
 * that always dead-ends.
 */
export const load: PageLoad = async (event) => {
	try {
		const [admins, instance] = await Promise.all([
			listSiteAdmins({ fetch: event.fetch }),
			getInstance({ fetch: event.fetch })
		]);

		return { admins: admins.items, instance };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
