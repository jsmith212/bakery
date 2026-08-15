import { describe, expect, it } from 'vitest';

import type { Me } from '$lib/api/types';
import {
	canAdminOrg,
	canAdminProject,
	canCreateOrg,
	canMintKey,
	canOwnOrg,
	canViewOrg,
	canWriteProject,
	isSiteAdmin,
	orgRoleFor,
	projectRoleFor
} from './roles';

function me(over: Partial<Me> = {}): Me {
	return {
		user_id: 'u',
		email: 'a@b.dev',
		display_name: 'A',
		method: 'session',
		site_role: 'user',
		is_site_admin: false,
		orgs: [],
		projects: [],
		...over
	};
}

const siteAdmin = me({ is_site_admin: true, site_role: 'admin' });

describe('the site-admin short circuit', () => {
	// The failure this exists to prevent: `GET /me` builds `orgs` from
	// MEMBERSHIPS, so a site admin has `orgs: []`, and `GET /orgs` returns every
	// org to them with NO `role` field. Gate on the role alone and the most
	// privileged principal in the installation becomes the least privileged user
	// in the console.
	const orgWithNoRole = {};

	it('admits a site admin to every org gate despite a role-less org row', () => {
		expect(canViewOrg(siteAdmin, orgWithNoRole)).toBe(true);
		expect(canAdminOrg(siteAdmin, orgWithNoRole)).toBe(true);
		expect(canOwnOrg(siteAdmin, orgWithNoRole)).toBe(true);
	});

	it('admits a site admin to every project gate', () => {
		expect(canWriteProject(siteAdmin, {})).toBe(true);
		expect(canAdminProject(siteAdmin, {})).toBe(true);
	});

	it('refuses everyone else on a role-less row', () => {
		const plain = me();
		expect(canViewOrg(plain, orgWithNoRole)).toBe(false);
		expect(canAdminOrg(plain, orgWithNoRole)).toBe(false);
		expect(canOwnOrg(plain, orgWithNoRole)).toBe(false);
	});
});

describe('org role ladder', () => {
	const plain = me();
	const cases: {
		role: 'member' | 'admin' | 'owner';
		view: boolean;
		admin: boolean;
		own: boolean;
	}[] = [
		{ role: 'member', view: true, admin: false, own: false },
		{ role: 'admin', view: true, admin: true, own: false },
		{ role: 'owner', view: true, admin: true, own: true }
	];

	for (const c of cases) {
		it(c.role, () => {
			expect(canViewOrg(plain, { role: c.role })).toBe(c.view);
			expect(canAdminOrg(plain, { role: c.role })).toBe(c.admin);
			expect(canOwnOrg(plain, { role: c.role })).toBe(c.own);
		});
	}
});

describe('project role ladder', () => {
	const plain = me();
	const cases: {
		role: 'reader' | 'writer' | 'admin';
		write: boolean;
		admin: boolean;
		mint: boolean;
	}[] = [
		{ role: 'reader', write: false, admin: false, mint: true },
		{ role: 'writer', write: true, admin: false, mint: true },
		{ role: 'admin', write: true, admin: true, mint: true }
	];

	for (const c of cases) {
		it(c.role, () => {
			expect(canWriteProject(plain, { role: c.role })).toBe(c.write);
			expect(canAdminProject(plain, { role: c.role })).toBe(c.admin);
			expect(canMintKey({ role: c.role })).toBe(c.mint);
		});
	}
});

describe('canMintKey', () => {
	it('refuses WITHOUT a project role, site admin or not', () => {
		// The one gate `is_site_admin` deliberately does not short-circuit.
		// `auth.CreateAPIKey` requires a `project_memberships` row FOR THE CALLER
		// and caps the scope against it -- there is nothing to cap against when the
		// row is missing, so an org owner or site admin who did not create the
		// project is refused with 403 `scope_exceeds_role`. Hiding that behind an
		// enabled button is how the highest-value screen dead-ends.
		expect(canMintKey({})).toBe(false);
		expect(canMintKey(null)).toBe(false);
		expect(canMintKey(undefined)).toBe(false);
	});
});

describe('role lookups', () => {
	const member = me({
		orgs: [{ id: '1', slug: 'acme', name: 'Acme', role: 'admin' }],
		projects: [{ id: '2', slug: 'firmware', org_slug: 'acme', role: 'writer' }]
	});

	it('prefers the role the API already computed on the row', () => {
		expect(orgRoleFor(member, { slug: 'acme', role: 'owner' })).toBe('owner');
	});

	it('falls back to me.orgs by slug', () => {
		expect(orgRoleFor(member, { slug: 'acme' })).toBe('admin');
		expect(orgRoleFor(member, { slug: 'other' })).toBeUndefined();
	});

	it('matches a project on BOTH slugs, since project slugs are not globally unique', () => {
		expect(projectRoleFor(member, { slug: 'firmware', org_slug: 'acme' })).toBe('writer');
		expect(projectRoleFor(member, { slug: 'firmware', org_slug: 'other' })).toBeUndefined();
	});
});

describe('canCreateOrg', () => {
	it('follows the public allow_self_serve_orgs flag', () => {
		expect(canCreateOrg(me(), { allow_self_serve_orgs: true })).toBe(true);
		expect(canCreateOrg(me(), { allow_self_serve_orgs: false })).toBe(false);
	});

	it('always admits a site admin, whatever the flag says', () => {
		expect(canCreateOrg(siteAdmin, { allow_self_serve_orgs: false })).toBe(true);
	});

	it('is false when the config could not be read, rather than showing a button that 403s', () => {
		expect(canCreateOrg(me(), null)).toBe(false);
	});
});

describe('isSiteAdmin', () => {
	it('is total over a missing principal', () => {
		expect(isSiteAdmin(null)).toBe(false);
		expect(isSiteAdmin(undefined)).toBe(false);
		expect(isSiteAdmin(siteAdmin)).toBe(true);
	});
});
