import { del, get, patch, post, seg, type RequestOptions } from './client';
import type {
	Backend,
	BackendKind,
	CreateBackendRequest,
	ListResponse,
	UpdateBackendRequest
} from './types';

/**
 * Cache backends.
 *
 * A project/kind with no `cache_backends` row is a **404**, everywhere -- the
 * cache mounts, the object browser, and this route. That is the invariant, not
 * an accident: a mount that cannot be served is never advertised.
 */

export function listBackends(
	org: string,
	project: string,
	opts?: RequestOptions
): Promise<ListResponse<Backend>> {
	return get<ListResponse<Backend>>(
		`/orgs/${seg(org)}/projects/${seg(project)}/backends`,
		opts
	);
}

/** 201. Project admin. 409 when the kind already exists (one mount per kind). */
export function createBackend(
	org: string,
	project: string,
	body: CreateBackendRequest,
	opts?: RequestOptions
): Promise<Backend> {
	return post<Backend>(`/orgs/${seg(org)}/projects/${seg(project)}/backends`, body, opts);
}

export function getBackend(
	org: string,
	project: string,
	kind: BackendKind,
	opts?: RequestOptions
): Promise<Backend> {
	return get<Backend>(
		`/orgs/${seg(org)}/projects/${seg(project)}/backends/${seg(kind)}`,
		opts
	);
}

/** `retention_window` and `quota_bytes` are three-state; see `$lib/api/patch`. */
export function updateBackend(
	org: string,
	project: string,
	kind: BackendKind,
	body: UpdateBackendRequest,
	opts?: RequestOptions
): Promise<Backend> {
	return patch<Backend>(
		`/orgs/${seg(org)}/projects/${seg(project)}/backends/${seg(kind)}`,
		body,
		opts
	);
}

/**
 * Deletes or tears down a backend. Project admin.
 *
 * TWO OUTCOMES, AND THE BODY IS THE ONLY WAY TO TELL THEM APART. An empty backend is
 * deleted outright and answers **204**, which `request` resolves as `undefined`. A
 * backend that has ever served a build cannot be deleted synchronously (its objects
 * hold a RESTRICT foreign key, and draining them in an HTTP request is what this
 * design refuses), so it is MARKED and answers **202** with the backend, its
 * `deleting_at` set. `client.ts` returns the decoded body and no status, so
 * `result === undefined` is "gone" and a result is "tearing down".
 *
 * Idempotent: deleting an already-deleting backend is another 202, never a 404. The
 * old **409** ("still holds cache objects") no longer exists -- it was unresolvable,
 * because nothing was ever going to empty the backend.
 */
export function deleteBackend(
	org: string,
	project: string,
	kind: BackendKind,
	opts?: RequestOptions
): Promise<Backend | undefined> {
	return del<Backend | undefined>(
		`/orgs/${seg(org)}/projects/${seg(project)}/backends/${seg(kind)}`,
		opts
	);
}
