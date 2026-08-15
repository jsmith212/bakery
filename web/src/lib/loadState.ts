/**
 * A loading/empty/loaded screen state as ONE decision, not three
 * independently-derived booleans.
 *
 * `o/[org]/projects` used to compute `isEmpty` from `projects.length === 0`
 * alone and `isLoading` from `!usageReady` alone -- two booleans that are NOT
 * mutually exclusive. An org with zero projects, before its usage promise
 * resolves, had `isEmpty === true` AND `isLoading === true` at once: the
 * skeleton grid and the "no projects yet" `EmptyState` both rendered on
 * screen simultaneously. `resolveLoadState` makes that unrepresentable by
 * construction -- `ready` gates first, so nothing is judged "empty" or
 * "loaded" before the data it would be judged on has actually arrived.
 */
export type LoadState = 'loading' | 'empty' | 'loaded';

export function resolveLoadState(ready: boolean, isEmpty: boolean): LoadState {
	if (!ready) return 'loading';

	return isEmpty ? 'empty' : 'loaded';
}
