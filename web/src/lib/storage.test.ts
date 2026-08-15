import { beforeEach, describe, expect, it } from 'vitest';

import {
	LAST_ORG_KEY,
	LAST_PROJECT_KEY,
	RETURN_PATH_KEY,
	clearLastTenancy,
	isSafeReturnPath,
	lastOrg,
	lastProject,
	memoryStorage,
	setLastOrg,
	setLastProject,
	setStoragePorts,
	stashReturnPath,
	takeReturnPath
} from './storage';

beforeEach(() => {
	setStoragePorts({ local: memoryStorage(), session: memoryStorage() });
});

describe('isSafeReturnPath', () => {
	const good = ['/orgs', '/o/acme/p/firmware/keys?tab=1', '/a#b'];
	const bad = [
		'//evil.example',
		'/\\evil.example',
		'https://evil.example',
		'javascript:alert(1)',
		'orgs',
		'',
		null,
		undefined,
		42,
		// The bare root: syntactically fine, but it is where the CONSUMER of this
		// value (`/`'s own `resolveLanding`) lives -- accepting it stashes a
		// "return path" that points back at the thing doing the returning.
		'/',
		// C0/DEL smuggled in right after the leading slash. The browser's URL
		// parser strips ASCII tab/newline/CR before anything else runs, so each
		// of these becomes `//evil.example` -- a protocol-relative absolute URL --
		// the moment `goto` hands it off, even though the character right after
		// the leading `/` is neither `/` nor `\` and so passes the first check.
		'/\t/evil.example',
		'/\n/evil.example',
		'/\r/evil.example',
		// Elsewhere in the string, not just right after the slash.
		'/orgs\n/evil.example',
		'/orgs\x7f'
	];

	for (const v of good) {
		it(`accepts ${JSON.stringify(v)}`, () => expect(isSafeReturnPath(v)).toBe(true));
	}

	for (const v of bad) {
		// `//evil.example` and `/\evil.example` are the two that matter: browsers
		// resolve both as protocol-relative ABSOLUTE urls, so a `goto` on either is
		// an open redirect wearing a relative path's clothes.
		it(`rejects ${JSON.stringify(v)}`, () => expect(isSafeReturnPath(v)).toBe(false));
	}
});

describe('the return-path stash', () => {
	it('round-trips and CLEARS on read', () => {
		const session = memoryStorage();
		setStoragePorts({ session });

		stashReturnPath('/o/acme/p/firmware/snippets');
		expect(session.getItem(RETURN_PATH_KEY)).toBe('/o/acme/p/firmware/snippets');

		expect(takeReturnPath()).toBe('/o/acme/p/firmware/snippets');
		// Clearing on read is what stops one stale path from hijacking every later
		// sign-in.
		expect(takeReturnPath()).toBeNull();
	});

	it('refuses to STORE an unsafe path', () => {
		const session = memoryStorage();
		setStoragePorts({ session });

		stashReturnPath('//evil.example');
		expect(session.getItem(RETURN_PATH_KEY)).toBeNull();
	});

	it('refuses to RETURN an unsafe path somebody else wrote', () => {
		// Two cheap checks around an open-redirect surface is the right number:
		// anything running in this origin can write the key directly.
		setStoragePorts({ session: memoryStorage({ [RETURN_PATH_KEY]: '//evil.example' }) });

		expect(takeReturnPath()).toBeNull();
	});

	it('is inert when there is no storage at all', () => {
		setStoragePorts({ session: null, local: null });

		expect(() => stashReturnPath('/orgs')).not.toThrow();
		expect(takeReturnPath()).toBeNull();
		expect(lastOrg()).toBeNull();
	});
});

describe('last-used tenancy', () => {
	it('remembers and forgets', () => {
		const local = memoryStorage();
		setStoragePorts({ local });

		setLastOrg('acme');
		setLastProject('firmware');
		expect(lastOrg()).toBe('acme');
		expect(lastProject()).toBe('firmware');
		expect(local.getItem(LAST_ORG_KEY)).toBe('acme');
		expect(local.getItem(LAST_PROJECT_KEY)).toBe('firmware');

		// Sign-out drops it: the next person on a shared machine must not land in
		// a tenant they may not be able to see.
		clearLastTenancy();
		expect(lastOrg()).toBeNull();
		expect(lastProject()).toBeNull();
	});
});
