import type { Me, Org, OrgRole, Project, ProjectRole } from '$lib/api/types';

/**
 * Role gating for the console.
 *
 * **Every check is `me.is_site_admin || <role check>`, and that is the entire
 * reason this file exists.** `GET /me` builds `orgs` from the caller's own
 * MEMBERSHIPS, so a site admin who is a member of nothing has `orgs: []`; and
 * `GET /orgs` returns every org to them with **no `role` field at all**. Gate on
 * the role alone and the most privileged principal in the installation becomes
 * the least privileged user in the console: they see every org, can open any of
 * them, and every admin control is hidden -- while the server would accept every
 * call.
 *
 * The gating is COSMETIC by design. `handleMe`'s own comment says it reports
 * authorization rather than performing it, and every endpoint re-checks. So a
 * gate being wrong in the permissive direction is a 403 to handle, not a
 * privilege escalation -- and being wrong in the restrictive direction is a
 * feature that silently does not exist.
 */

const ORG_RANK: Record<OrgRole, number> = { member: 1, admin: 2, owner: 3 };
const PROJECT_RANK: Record<ProjectRole, number> = { reader: 1, writer: 2, admin: 3 };

/** Anything carrying a caller's org role: an `Org`, a `MeOrg`, or a bare role. */
export interface OrgRoleCarrier {
	role?: OrgRole;
}

/** Anything carrying a caller's project role: a `Project`, a `MeProject`. */
export interface ProjectRoleCarrier {
	role?: ProjectRole;
}

function orgRank(role: OrgRole | undefined): number {
	return role ? ORG_RANK[role] : 0;
}

function projectRank(role: ProjectRole | undefined): number {
	return role ? PROJECT_RANK[role] : 0;
}

export function isSiteAdmin(me: Me | null | undefined): boolean {
	return me?.is_site_admin === true;
}

/**
 * The caller's role in one org, preferring the role the API already computed on
 * the `Org` row and falling back to `me.orgs`. Returns `undefined` for a site
 * admin who is not a member -- which is correct, and is why callers use the
 * `can*` helpers rather than this.
 */
export function orgRoleFor(
	me: Me | null | undefined,
	org: (OrgRoleCarrier & { slug?: string }) | null | undefined
): OrgRole | undefined {
	if (org?.role) return org.role;
	if (!me || !org?.slug) return undefined;

	return me.orgs.find((o) => o.slug === org.slug)?.role;
}

export function canViewOrg(me: Me | null | undefined, org: OrgRoleCarrier | null | undefined) {
	return isSiteAdmin(me) || orgRank(org?.role) >= ORG_RANK.member;
}

export function canAdminOrg(me: Me | null | undefined, org: OrgRoleCarrier | null | undefined) {
	return isSiteAdmin(me) || orgRank(org?.role) >= ORG_RANK.admin;
}

export function canOwnOrg(me: Me | null | undefined, org: OrgRoleCarrier | null | undefined) {
	return isSiteAdmin(me) || orgRank(org?.role) >= ORG_RANK.owner;
}

/**
 * The caller's PROJECT role.
 *
 * A `Project.role` of `undefined` does NOT mean "no access": org membership
 * grants read on every project in the org without a `project_memberships` row.
 * It means "no project role", which is exactly the state in which minting a key
 * is refused with `scope_exceeds_role`.
 */
export function projectRoleFor(
	me: Me | null | undefined,
	project: (ProjectRoleCarrier & { slug?: string; org_slug?: string }) | null | undefined
): ProjectRole | undefined {
	if (project?.role) return project.role;
	if (!me || !project?.slug) return undefined;

	return me.projects.find((p) => p.slug === project.slug && p.org_slug === project.org_slug)
		?.role;
}

export function canWriteProject(
	me: Me | null | undefined,
	project: ProjectRoleCarrier | null | undefined
) {
	return isSiteAdmin(me) || projectRank(project?.role) >= PROJECT_RANK.writer;
}

export function canAdminProject(
	me: Me | null | undefined,
	project: ProjectRoleCarrier | null | undefined
) {
	return isSiteAdmin(me) || projectRank(project?.role) >= PROJECT_RANK.admin;
}

/**
 * Whether the caller may mint an API key on this project AT ALL.
 *
 * Not a role comparison: `auth.CreateAPIKey` requires a `project_memberships`
 * row for the caller and refuses without one, no matter what org role they hold.
 * A site admin is NOT exempt -- the row is what the scope is capped against, and
 * there is nothing to cap against when it is missing. This helper is therefore
 * the one place in the file where `is_site_admin` does not short-circuit, and it
 * is deliberate.
 */
export function canMintKey(project: ProjectRoleCarrier | null | undefined): boolean {
	return projectRank(project?.role) >= PROJECT_RANK.reader;
}

/**
 * Whether to render the create-organization affordance.
 *
 * `allow_self_serve_orgs` is a deployment choice reported on the PUBLIC
 * `/auth/config` document. Without consulting it the button 403s with no
 * explanation on every installation that restricts org creation.
 */
export function canCreateOrg(
	me: Me | null | undefined,
	config: { allow_self_serve_orgs: boolean } | null | undefined
): boolean {
	return isSiteAdmin(me) || config?.allow_self_serve_orgs === true;
}

/** Convenience for screens holding a full `Org`/`Project` rather than a carrier. */
export type OrgLike = Pick<Org, 'slug'> & OrgRoleCarrier;
export type ProjectLike = Pick<Project, 'slug' | 'org_slug'> & ProjectRoleCarrier;
