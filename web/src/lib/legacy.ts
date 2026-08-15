import type { Me, Org } from '$lib/api/types';
import { lastOrg, lastProject } from '$lib/storage';
import { orgPath, projectPath, resolveTenancy } from '$lib/tenancy';

/**
 * One-release redirects for the pre-tenancy flat routes (`/overview`, `/keys`,
 * ...).
 *
 * They exist because the SPA fallback serves `index.html` for ANY path: without
 * these, a bookmarked or linked mock URL does not 404 loudly, it renders the
 * console's own "not found" and looks like the console broke.
 *
 * A project-scoped legacy path with no remembered project resolves to the org's
 * project list rather than guessing one. Guessing would land somebody on a
 * stranger's project and call it their bookmark.
 */
export interface LegacyContext {
	me: Me;
	orgs: Org[];
}

export interface LegacySuffix {
	/** Path under `/o/{org}/`, e.g. `projects`. */
	org: string;
	/** Path under `/o/{org}/p/{project}/`, e.g. `keys`. Absent for org screens. */
	project?: string;
}

export function legacyTarget(ctx: LegacyContext, suffix: LegacySuffix): string {
	const { org, project } = resolveTenancy({
		me: ctx.me,
		orgs: ctx.orgs,
		lastOrg: lastOrg(),
		lastProject: lastProject()
	});

	if (!org) return '/orgs';
	if (!suffix.project) return `${orgPath(org)}/${suffix.org}`;
	if (!project) return `${orgPath(org)}/${suffix.org}`;

	return `${projectPath(org, project)}/${suffix.project}`;
}
