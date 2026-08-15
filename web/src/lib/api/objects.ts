import { get, seg, type RequestOptions } from './client';
import type { BackendKind, CacheObjectList, ListObjectsQuery } from './types';

/**
 * B3, the object browser: a KEYSET page over `cache_objects_pkey`
 * `(backend_id, namespace, key)`.
 *
 * Two things the caller must know:
 *
 *  - **`namespace` absent means the default namespace (`''`)**, not "all
 *    namespaces". sstate and downloads live there; bazel's `ac`/`ac-grpc`/
 *    `cas`/`sccache` and OCI's `tags`/`manifests`/`blobs` each need an explicit
 *    one. Widening it would turn the leading primary-key column from an equality
 *    into a range and cost the index prefix on a table sized in the tens of
 *    millions of rows.
 *  - **`limit` is CLAMPED, not rejected** (default 50, ceiling 200), the
 *    convention every list endpoint in this API follows.
 *
 * Paging is `after_key`, never an offset.
 */
export function listCacheObjects(
	org: string,
	project: string,
	kind: BackendKind,
	query?: ListObjectsQuery,
	opts?: RequestOptions
): Promise<CacheObjectList> {
	return get<CacheObjectList>(
		`/orgs/${seg(org)}/projects/${seg(project)}/backends/${seg(kind)}/objects`,
		{ ...opts, query }
	);
}
