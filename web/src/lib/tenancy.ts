import type { Me, Org } from '$lib/api/types';
import { isSafeReturnPath } from '$lib/storage';

/**
 * Tenancy lives in the PATH: `/o/{org}/p/{project}/...`.
 *
 * The two literal segments cost nothing and buy total immunity from slug
 * collisions -- an org named `projects` cannot be confused with the projects
 * page, and no slug has to be added to the reserved list. Query-param tenancy
 * was rejected: it breaks deep links and back/forward, and it makes `load`
 * functions depend on a store, which is backwards.
 *
 * The last-used org/project in `localStorage` only ever seed the redirect from
 * `/`. They never override an explicit URL.
 */

export function orgPath(org: string): string {
	return `/o/${encodeURIComponent(org)}`;
}

export function projectPath(org: string, project: string): string {
	return `${orgPath(org)}/p/${encodeURIComponent(project)}`;
}

export interface TenancyInput {
	me: Me | null;
	/** `GET /orgs` -- the AUTHORITATIVE visible set, and a superset of `me.orgs`. */
	orgs?: Org[] | null;
	lastOrg?: string | null;
	lastProject?: string | null;
}

export interface Tenancy {
	org: string | null;
	project: string | null;
}

/**
 * Resolves which tenant to land on.
 *
 * Order: remembered org (only if it is still visible) -> the caller's first
 * membership -> the first org they can see at all -> nothing.
 *
 * The third step is the SITE ADMIN case and it is not optional: a site admin has
 * `me.orgs: []` while `GET /orgs` returns every org in the installation. Without
 * it they land on the "create an organization" empty state of an installation
 * full of organizations.
 *
 * The remembered org is validated against the visible set because localStorage
 * outlives access: an org someone was removed from would otherwise send them to
 * a 404 on every sign-in, forever, with no way to notice why.
 */
export function resolveTenancy(input: TenancyInput): Tenancy {
	const visible = new Set<string>();
	for (const o of input.me?.orgs ?? []) visible.add(o.slug);
	for (const o of input.orgs ?? []) visible.add(o.slug);

	if (input.lastOrg && visible.has(input.lastOrg)) {
		return { org: input.lastOrg, project: input.lastProject ?? null };
	}

	const firstMembership = input.me?.orgs[0]?.slug;
	if (firstMembership) return { org: firstMembership, project: null };

	const firstVisible = input.orgs?.[0]?.slug;
	if (firstVisible) return { org: firstVisible, project: null };

	return { org: null, project: null };
}

/**
 * Where `/` sends a signed-in caller.
 *
 * A stashed return path wins over everything -- it is the interrupted
 * navigation, and it is the only thing that survives the OIDC round trip. It is
 * re-validated here as well as on the way into storage: two cheap checks around
 * an open-redirect surface is the right number.
 */
export function resolveLanding(input: TenancyInput & { returnPath?: string | null }): string {
	if (isSafeReturnPath(input.returnPath)) return input.returnPath;

	const { org, project } = resolveTenancy(input);
	if (!org) return '/orgs';
	if (project) return `${projectPath(org, project)}/overview`;

	return `${orgPath(org)}/projects`;
}
