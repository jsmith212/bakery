import { get, post, type RequestOptions } from './client';
import type {
	GCRun,
	GCRunList,
	ListGCRunsQuery,
	TriggerGCRunRequest,
	TriggerGCRunResponse
} from './types';

/**
 * `/gc`. Site admin only, all three -- a sweep can delete a tenant's cache
 * wholesale, so triggering one is at least as dangerous as granting a site role,
 * and the guard admits no api-key principal to that level.
 *
 * The org-scoped read (B7) is NOT here: it is `getOrgGCActivity` in `orgs.ts`,
 * because it is an org route with org access.
 */

/**
 * Keyset-paginated on `id`, newest first. `include_usage` defaults FALSE: the
 * usage pass mints a run row every six hours forever, and a default-visible
 * listing buries real sweeps under bookkeeping within a week.
 */
export function listGCRuns(query?: ListGCRunsQuery, opts?: RequestOptions): Promise<GCRunList> {
	return get<GCRunList>('/gc/runs', { ...opts, query });
}

export function getGCRun(id: number, opts?: RequestOptions): Promise<GCRun> {
	return get<GCRun>(`/gc/runs/${id}`, opts);
}

/**
 * 202 with an id to poll -- never 200 with a result: a sweep can legitimately
 * run for hours. Three distinct 409s (already running / gc disabled /
 * multi-instance), and the distinction is the whole value of the response, so
 * the toast renders the server's own message.
 *
 * `dry_run` is sent explicitly, always: its zero value is a REAL sweep, and an
 * omitted field is not the safer reading.
 */
export function triggerGCRun(
	body: TriggerGCRunRequest,
	opts?: RequestOptions
): Promise<TriggerGCRunResponse> {
	return post<TriggerGCRunResponse>('/gc/run', body, opts);
}
