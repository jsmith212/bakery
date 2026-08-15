/**
 * Browser storage, behind an injectable port.
 *
 * Vitest runs in the `node` environment (there is no jsdom in this repo and none
 * is added this wave), so `localStorage` and `sessionStorage` do not exist in a
 * test. Rather than adopt a DOM emulator to test twenty lines of string
 * handling, the two stores are reached through a `Storage`-shaped port that a
 * test replaces with a `Map` -- the same idiom `theme.ts` uses when it guards
 * `typeof localStorage === 'undefined'`, made explicit so the logic above it is
 * testable rather than merely non-crashing.
 *
 * # Why the return path is `sessionStorage` and NOT a `?return=` parameter
 *
 * `GET /auth/callback` redirects to `/` unconditionally and is not
 * parameterizable. A `?return=` on the way to the IdP therefore cannot come
 * back, which makes the stash the only mechanism that survives the round trip --
 * so there is exactly one mechanism, and no `?return=` anywhere in this console.
 *
 * A stashed path is attacker-influenceable (anything that can run in this origin
 * can write it), so it is validated before any navigation: it must begin with a
 * single `/` that is not followed by another `/` or a `\`. That rejects
 * `//evil.example` and `/\evil.example`, both of which browsers resolve as
 * protocol-relative absolute URLs -- an open redirect wearing a relative path's
 * clothes.
 *
 * Two more rejections, both closing gaps in that same check rather than adding
 * new ones:
 *
 *  - **C0 controls and DEL are refused anywhere in the string.** The URL
 *    parser strips ASCII tab/newline/CR before doing anything else, so
 *    `"/\n/evil.example"` passes the regex above (the character right after
 *    the leading `/` is `\n`, neither `/` nor `\`) and then becomes
 *    `"//evil.example"` the moment `goto` hands it to the browser -- the exact
 *    open redirect the leading check exists to stop, smuggled in behind a
 *    character the regex never looked at.
 *  - **A bare `"/"` is refused.** It is syntactically safe -- it is also
 *    where the resolver that CONSUMES this value (`/`'s own `load`, via
 *    `resolveLanding`) lives, so accepting it stashes a "return path" that
 *    points back at the thing doing the returning. Not a security hole, just
 *    a useless value masquerading as a real destination.
 */

export interface StorageLike {
	getItem(key: string): string | null;
	setItem(key: string, value: string): void;
	removeItem(key: string): void;
}

export const RETURN_PATH_KEY = 'bakery-return';
export const LAST_ORG_KEY = 'bakery-org';
export const LAST_PROJECT_KEY = 'bakery-project';

/** A `Map`-backed port. The test fake, and a safe no-DOM fallback. */
export function memoryStorage(seed?: Record<string, string>): StorageLike {
	const map = new Map<string, string>(Object.entries(seed ?? {}));

	return {
		getItem: (key) => map.get(key) ?? null,
		setItem: (key, value) => {
			map.set(key, value);
		},
		removeItem: (key) => {
			map.delete(key);
		}
	};
}

// `undefined` means "not configured -- ask the browser"; `null` means
// "explicitly disabled". Collapsing the two would make `setStoragePorts({local:
// null})` silently fall through to real Web Storage, which is exactly what a
// caller disabling persistence is trying to avoid -- and what a Node 22 test
// would then quietly exercise, since Node now ships a real `localStorage`.
let localPort: StorageLike | null | undefined;
let sessionPort: StorageLike | null | undefined;

/** Replaces the ports. `null` disables one entirely; called by tests. */
export function setStoragePorts(ports: {
	local?: StorageLike | null;
	session?: StorageLike | null;
}): void {
	if ('local' in ports) localPort = ports.local;
	if ('session' in ports) sessionPort = ports.session;
}

/** Forgets any injected ports, restoring the browser defaults. */
export function resetStoragePorts(): void {
	localPort = undefined;
	sessionPort = undefined;
}

function browserStorage(pick: 'local' | 'session'): StorageLike | null {
	try {
		const s = pick === 'local' ? globalThis.localStorage : globalThis.sessionStorage;

		return s ?? null;
	} catch {
		// Safari in private mode, and any embedding that blocks storage, throw on
		// ACCESS rather than returning null. Losing the last-used org is a
		// nuisance; a thrown exception in a layout load is a blank console.
		return null;
	}
}

export function local(): StorageLike | null {
	return localPort === undefined ? browserStorage('local') : localPort;
}

export function session(): StorageLike | null {
	return sessionPort === undefined ? browserStorage('session') : sessionPort;
}

/**
 * True for a path safe to `goto`. See the module doc: single leading slash,
 * not followed by `/` or `\`; no C0 control character or DEL anywhere; not the
 * bare root.
 */
export function isSafeReturnPath(value: unknown): value is string {
	return (
		typeof value === 'string' &&
		value !== '/' &&
		/^\/(?![/\\])/.test(value) &&
		!/[\x00-\x1f\x7f]/.test(value)
	);
}

/** Records where to come back to. Unsafe values are dropped, not stored. */
export function stashReturnPath(path: string, port: StorageLike | null = session()): void {
	if (!port || !isSafeReturnPath(path)) return;
	port.setItem(RETURN_PATH_KEY, path);
}

/**
 * Reads and CLEARS the stash. Clearing on read is what stops a stale path from
 * hijacking every later sign-in.
 */
export function takeReturnPath(port: StorageLike | null = session()): string | null {
	if (!port) return null;

	const value = port.getItem(RETURN_PATH_KEY);
	port.removeItem(RETURN_PATH_KEY);

	return isSafeReturnPath(value) ? value : null;
}

export function lastOrg(port: StorageLike | null = local()): string | null {
	return port?.getItem(LAST_ORG_KEY) ?? null;
}

export function setLastOrg(slug: string, port: StorageLike | null = local()): void {
	port?.setItem(LAST_ORG_KEY, slug);
}

export function lastProject(port: StorageLike | null = local()): string | null {
	return port?.getItem(LAST_PROJECT_KEY) ?? null;
}

export function setLastProject(slug: string, port: StorageLike | null = local()): void {
	port?.setItem(LAST_PROJECT_KEY, slug);
}

/** Forgets the remembered tenancy. Called on sign-out. */
export function clearLastTenancy(port: StorageLike | null = local()): void {
	port?.removeItem(LAST_ORG_KEY);
	port?.removeItem(LAST_PROJECT_KEY);
}
