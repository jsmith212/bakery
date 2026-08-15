import { error } from '@sveltejs/kit';

import { setLastProject } from '$lib/storage';

import type { LayoutLoad } from './$types';

/**
 * Everything under `/o/[org]/p/[project]`.
 *
 * The project is picked out of the parent layout's list rather than re-fetched:
 * `GET /orgs/{org}/projects` already returned every project the caller can read,
 * with the same `Project` shape `GET .../{project}` would answer, so a second
 * round trip would buy nothing but latency on every project navigation.
 *
 * A slug that is not in that list is a 404 for the right reason -- the caller
 * cannot read it, or it does not exist, and those are deliberately the same
 * answer.
 */
export const load: LayoutLoad = async (event) => {
	const parent = await event.parent();
	const project = parent.projects.find((p) => p.slug === event.params.project);

	if (!project) {
		error(404, `no project ${event.params.project} in ${event.params.org}`);
	}

	setLastProject(project.slug);

	return { project };
};
