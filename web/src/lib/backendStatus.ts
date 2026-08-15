import type { BadgeStatus } from '$lib/components/badges';
import type { BackendKind, GCRunStatus, ProjectBackendUsage } from '$lib/api/types';

/**
 * The ONLY producer of a `BadgeStatus` from cache-backend data.
 *
 * `BadgeStatus` is a closed union (`hit | miss | stale | idle`) and the console
 * used to reach it with an `as BadgeStatus` cast over an arbitrary string. That
 * cast is deleted and must not come back: there is no JS lint toolchain in this
 * repo and none is added this wave, so the guarantee is structural instead --
 * this function is the only thing that returns the type, and everything else
 * takes its result.
 *
 * # Why it is TOTAL over a MISSING usage row, not over zero
 *
 * Two facts are easy to conflate and are completely different:
 *
 *  - **hashserv NEVER gets a `cache_backend_usage` row.** M6's planner gives it
 *    no stages, and both `MeasureUsage` and the sweep skip a plan with none. A
 *    hashserv backend serving every unihash in the fleet would render `○ idle`
 *    forever under a mapper keyed on bytes. So its badge derives from `enabled`
 *    alone and has no usage dependency at all.
 *  - **`--gc-usage-interval` defaults to SIX HOURS.** A backend created and
 *    filled at 09:00 has no usage row until the next pass, so a working sstate
 *    backend also renders "no bytes" for up to six hours. That is a measurement
 *    that has not happened, not a backend that is idle, and the caption says so.
 *
 * `logical_bytes === 0` WITH a `measured_at` is the honest `idle`: something
 * looked, and there is nothing there.
 */

export interface BackendStatusInput {
	kind: BackendKind;
	enabled: boolean;
	/** B2b's row for this backend, or null/undefined when there is none. */
	usage?: ProjectBackendUsage | null;
}

export interface BackendStatusResult {
	status: BadgeStatus;
	/** Short label for the badge itself. */
	label: string;
	/**
	 * A caption the screen must render beside the badge when present. It exists
	 * for exactly one state -- "not yet measured" -- because that state is
	 * otherwise indistinguishable from "empty", and rendering it as `0 B` is a
	 * lie about freshness.
	 */
	caption: string | null;
}

/** True when nothing has ever reported usage for this backend. */
export function isUnmeasured(usage: ProjectBackendUsage | null | undefined): boolean {
	return !usage || usage.measured_at === null;
}

export function backendStatus(input: BackendStatusInput): BackendStatusResult {
	// 1. Disabled beats everything. A disabled backend 404s its mount whatever
	//    its stored bytes say.
	if (!input.enabled) {
		return { status: 'stale', label: 'disabled', caption: null };
	}

	// 2. hashserv, structurally never measured. Enabled is the whole answer.
	if (input.kind === 'hashserv') {
		return { status: 'hit', label: 'enabled', caption: null };
	}

	const usage = input.usage ?? null;

	// 3. No measurement yet -- NOT idle, and never rendered as zero bytes.
	if (isUnmeasured(usage)) {
		return {
			status: 'idle',
			label: 'not measured',
			caption: 'Not yet measured — usage is sampled on the retention interval.'
		};
	}

	const bytes = usage?.logical_bytes ?? 0;
	const quota = usage?.quota_bytes ?? null;

	// 4. At or over the logical cap. Checked BEFORE "has bytes": a full backend
	//    is the one that has the most bytes, and reporting it healthy is the
	//    failure this ordering exists to prevent.
	if (quota !== null && bytes >= quota) {
		return { status: 'miss', label: 'quota reached', caption: null };
	}

	if (bytes > 0) {
		return { status: 'hit', label: 'storing', caption: null };
	}

	// 5. Measured, and genuinely empty.
	return { status: 'idle', label: 'empty', caption: null };
}

/**
 * The one producer of a `BadgeStatus` for a GC run's status.
 *
 * `gc/+page.svelte`'s run table and the org projects page's "Retention
 * activity" card (B7) both render this exact value -- before this they did so
 * with two independently hand-rolled mappings, one `Record` and one ternary
 * chain, free to drift. A `Record<GCRunStatus, BadgeStatus>` is exhaustive by
 * construction: adding a member to `GCRunStatus` without adding it here is a
 * compile error, not a run silently fine-until-it-isn't with a `default`
 * branch swallowing the new state.
 */
const GC_RUN_BADGE: Record<GCRunStatus, BadgeStatus> = {
	succeeded: 'hit',
	failed: 'miss',
	running: 'stale'
};

export function gcStatusBadge(status: GCRunStatus): BadgeStatus {
	return GC_RUN_BADGE[status];
}
