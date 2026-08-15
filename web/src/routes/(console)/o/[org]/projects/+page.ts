import { getOrgGCActivity, getOrgUsage } from '$lib/api/orgs';

import type { PageLoad } from './$types';

/**
 * `GET /orgs/{org}/usage` (B2a) + `GET /orgs/{org}/gc/activity` (B7).
 *
 * The project list itself already came from the parent `o/[org]` layout
 * (`GET /orgs/{org}/projects` -- never `me.projects`, see `$lib/tenancy`), so
 * this load only owes the two things that layout does not carry: per-project
 * storage, and the org's own slice of recent GC sweeps. Both are returned as
 * PROMISES, not awaited, so the shell paints immediately and each card
 * unwraps its own with `{#await}` -- this screen has a designed skeleton
 * (memo §5.2's "designed skeleton" case, not its "cheap read" one), and the
 * two cards load independently: a slow GC-activity join must never hold up
 * the project grid, or vice versa.
 */
export const load: PageLoad = (event) => {
	const usage = getOrgUsage(event.params.org, { fetch: event.fetch });
	const activity = getOrgGCActivity(event.params.org, { limit: 10 }, { fetch: event.fetch });

	return { usage, activity };
};
