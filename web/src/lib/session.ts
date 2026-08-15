import { writable } from 'svelte/store';

import { getMe } from '$lib/api/me';
import type { FetchLike } from '$lib/api/client';
import type { Me } from '$lib/api/types';
import { clearLastTenancy } from '$lib/storage';

/**
 * The current principal.
 *
 * `theme.ts`'s idiom -- a `writable` plus free functions -- and deliberately not
 * Svelte context, which cannot be read from a `load`.
 *
 * The session payload is a `user_id` and nothing else: every authorization fact
 * is re-read from the database on every request, so a demotion takes effect on
 * the caller's next call and there is no client-side invalidation to build. This
 * store is a render cache for the nav, never an authority.
 */
export const me = writable<Me | null>(null);

/**
 * Fetches `/me` and publishes it. Returns `null` when nobody is signed in --
 * there is no 403 outcome; see `$lib/api/me`.
 */
export async function loadMe(fetch?: FetchLike): Promise<Me | null> {
	const value = await getMe(fetch ? { fetch } : undefined);
	me.set(value);

	return value;
}

/**
 * Drops every trace of the signed-in user from this tab.
 *
 * The remembered org/project go too: they are a convenience for one person, and
 * leaving them behind means the next person to sign in on a shared machine lands
 * on a tenant they may not be able to see.
 */
export function clearSession(): void {
	me.set(null);
	clearLastTenancy();
}
