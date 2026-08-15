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

/** 204, or **409** while the backend still holds objects. */
export function deleteBackend(
	org: string,
	project: string,
	kind: BackendKind,
	opts?: RequestOptions
): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/projects/${seg(project)}/backends/${seg(kind)}`, opts);
}
