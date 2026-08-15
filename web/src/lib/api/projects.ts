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

/** `DELETE`. 204. **Org** admin, deliberately -- not project admin. */
export function deleteProject(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/projects/${seg(project)}`, opts);
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
