import { del, get, patch, post, seg, type RequestOptions } from './client';
import type {
	CreateProjectRequest,
	ListResponse,
	Project,
	ProjectBackendUsage,
	UpdateProjectRequest
} from './types';

/**
 * `/orgs/{org}/projects`.
 *
 * **This is the nav's project source, not `me.projects`.** `GET /me` populates
 * `projects` from project MEMBERSHIPS alone, while org membership already grants
 * read on every project in the org -- so an org owner who created no projects
 * has `me.projects: []` and can read all of them. Driving a switcher from `/me`
 * shows an empty list to the person who owns the org.
 */
export function listProjects(
	org: string,
	opts?: RequestOptions
): Promise<ListResponse<Project>> {
	return get<ListResponse<Project>>(`/orgs/${seg(org)}/projects`, opts);
}

/**
 * `POST /orgs/{org}/projects`. 201. Org admin.
 *
 * The creator is granted PROJECT ADMIN in the same transaction, which is the
 * only reason the console's headline flow (create org, create project, mint key)
 * does not dead-end on `scope_exceeds_role`.
 */
export function createProject(
	org: string,
	body: CreateProjectRequest,
	opts?: RequestOptions
): Promise<Project> {
	return post<Project>(`/orgs/${seg(org)}/projects`, body, opts);
}

export function getProject(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<Project> {
	return get<Project>(`/orgs/${seg(org)}/projects/${seg(project)}`, opts);
}

export function updateProject(
	org: string,
	project: string,
	body: UpdateProjectRequest,
	opts?: RequestOptions
): Promise<Project> {
	return patch<Project>(`/orgs/${seg(org)}/projects/${seg(project)}`, body, opts);
}

/**
 * Deletes or tears down a project. **Org** admin, deliberately -- not project admin:
 * this destroys every cache object, key and backend config in the project, and
 * letting the recipient of a delegated role destroy the thing it was delegated over
 * is the wrong default.
 *
 * Same two outcomes as `deleteBackend`, decided the same way: a project whose
 * backends all hold nothing is deleted outright (**204**, resolves to `undefined`);
 * one with cached data has its backends marked and answers **202** with the project,
 * its `deleting_at` set. Either way the project stops resolving immediately, so this
 * is the last response about it -- navigate away rather than re-fetching.
 */
export function deleteProject(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<Project | undefined> {
	return del<Project | undefined>(`/orgs/${seg(org)}/projects/${seg(project)}`, opts);
}

/**
 * Measures this project's usage NOW and returns the fresh rows.
 *
 * Project READ, the same floor as the GET it refreshes: it writes only a derived
 * figure about data the caller can already see. Rate-limited server-side to one
 * measurement per project per 10s and always answers 200 -- "you asked too soon" is
 * not a condition a dashboard can act on, and `measured_at` on every row already
 * says exactly how fresh the answer is.
 */
export function measureProjectUsage(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<ListResponse<ProjectBackendUsage>> {
	return post<ListResponse<ProjectBackendUsage>>(
		`/orgs/${seg(org)}/projects/${seg(project)}/usage/measure`,
		undefined,
		opts
	);
}

/** B2b. Per-backend, unaggregated; counts are NULLABLE when nothing has measured. */
export function getProjectUsage(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<ListResponse<ProjectBackendUsage>> {
	return get<ListResponse<ProjectBackendUsage>>(
		`/orgs/${seg(org)}/projects/${seg(project)}/usage`,
		opts
	);
}
