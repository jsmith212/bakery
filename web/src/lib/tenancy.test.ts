import { beforeEach, describe, expect, it } from 'vitest';

import type { Me, Org } from '$lib/api/types';
import { legacyTarget } from './legacy';
import {
	LAST_ORG_KEY,
	LAST_PROJECT_KEY,
	RETURN_PATH_KEY,
	memoryStorage,
	setStoragePorts
} from './storage';
import { orgPath, projectPath, resolveLanding, resolveNavScope, resolveTenancy } from './tenancy';

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

function org(slug: string): Org {
	return {
		id: slug,
		slug,
		name: slug,
		default_retention_window: null,
		default_quota_bytes: null,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z'
	};
}

describe('path helpers', () => {
	it('puts tenancy in the path, with the /o/ and /p/ literals', () => {
		expect(orgPath('acme')).toBe('/o/acme');
		expect(projectPath('acme', 'firmware')).toBe('/o/acme/p/firmware');
	});

	it('encodes a slug rather than trusting it', () => {
		expect(orgPath('a b')).toBe('/o/a%20b');
	});
});

describe('resolveTenancy', () => {
	const member = me({ orgs: [{ id: '1', slug: 'acme', name: 'Acme', role: 'admin' }] });

	it('prefers the remembered org when it is still visible', () => {
		expect(
			resolveTenancy({
				me: member,
				orgs: [org('acme'), org('robotics-lab')],
				lastOrg: 'robotics-lab',
				lastProject: 'arm'
			})
		).toEqual({ org: 'robotics-lab', project: 'arm' });
	});

	it('IGNORES a remembered org the caller can no longer see', () => {
		// localStorage outlives access. Trusting it blindly pins somebody to a 404
		// on every sign-in, forever, with nothing on screen saying why.
		expect(
			resolveTenancy({ me: member, orgs: [org('acme')], lastOrg: 'gone', lastProject: 'x' })
		).toEqual({ org: 'acme', project: null });
	});

	it('falls back to the first membership', () => {
		expect(resolveTenancy({ me: member, orgs: [org('acme')] })).toEqual({
			org: 'acme',
			project: null
		});
	});

	it('falls back to the first VISIBLE org for a site admin with no memberships', () => {
		// The site-admin case, and it is not optional: `me.orgs` is empty while
		// `GET /orgs` returns every org in the installation. Without this they land
		// on "create an organization" in an installation full of organizations.
		const admin = me({ is_site_admin: true, site_role: 'admin' });

		expect(resolveTenancy({ me: admin, orgs: [org('acme'), org('other')] })).toEqual({
			org: 'acme',
			project: null
		});
	});

	it('resolves to nothing when there is nothing', () => {
		expect(resolveTenancy({ me: me(), orgs: [] })).toEqual({ org: null, project: null });
	});
});

describe('resolveLanding', () => {
	const member = me({ orgs: [{ id: '1', slug: 'acme', name: 'Acme', role: 'admin' }] });

	it('a stashed return path wins over everything', () => {
		expect(
			resolveLanding({ me: member, orgs: [org('acme')], returnPath: '/o/x/p/y/keys' })
		).toBe('/o/x/p/y/keys');
	});

	it('rejects a protocol-relative return path rather than following it', () => {
		// `//evil.example` is an absolute URL wearing a relative path's clothes.
		expect(
			resolveLanding({ me: member, orgs: [org('acme')], returnPath: '//evil.example' })
		).toBe('/o/acme/projects');
	});

	it('lands on the remembered project when there is one', () => {
		expect(
			resolveLanding({
				me: member,
				orgs: [org('acme')],
				lastOrg: 'acme',
				lastProject: 'firmware'
			})
		).toBe('/o/acme/p/firmware/overview');
	});

	it('lands on the org project list when there is no remembered project', () => {
		expect(resolveLanding({ me: member, orgs: [org('acme')] })).toBe('/o/acme/projects');
	});

	it('lands on /orgs when the caller can see none', () => {
		expect(resolveLanding({ me: me(), orgs: [] })).toBe('/orgs');
	});
});

describe('legacy flat routes', () => {
	beforeEach(() => {
		setStoragePorts({
			local: memoryStorage({ [LAST_ORG_KEY]: 'acme', [LAST_PROJECT_KEY]: 'acme/firmware' }),
			session: memoryStorage()
		});
	});

	const ctx = {
		me: me({ orgs: [{ id: '1', slug: 'acme', name: 'Acme', role: 'admin' }] }),
		orgs: [org('acme')]
	};

	it('redirects an org-scoped legacy path', () => {
		expect(legacyTarget(ctx, { org: 'members' })).toBe('/o/acme/members');
	});

	it('redirects a project-scoped legacy path', () => {
		expect(legacyTarget(ctx, { org: 'projects', project: 'keys' })).toBe(
			'/o/acme/p/firmware/keys'
		);
	});

	it('preserves the backend kind from the legacy /backends/[type] route', () => {
		expect(legacyTarget(ctx, { org: 'projects', project: 'backends/oci' })).toBe(
			'/o/acme/p/firmware/backends/oci'
		);
	});

	it('sends a project-scoped legacy path to the project LIST when no project is remembered', () => {
		// Guessing a project would land somebody on a stranger's project and call
		// it their bookmark.
		setStoragePorts({ local: memoryStorage({ [LAST_ORG_KEY]: 'acme' }) });

		expect(legacyTarget(ctx, { org: 'projects', project: 'keys' })).toBe('/o/acme/projects');
	});

	it('sends everything to /orgs when the caller has no visible org', () => {
		setStoragePorts({ local: memoryStorage() });

		expect(legacyTarget({ me: me(), orgs: [] }, { org: 'members' })).toBe('/orgs');
	});

	it('does not read the return stash', () => {
		// The stash belongs to `/`, not to a legacy path: a legacy URL is itself an
		// explicit destination, and consuming the stash here would silently discard
		// the interrupted navigation.
		setStoragePorts({
			local: memoryStorage({ [LAST_ORG_KEY]: 'acme' }),
			session: memoryStorage({ [RETURN_PATH_KEY]: '/o/other/projects' })
		});

		expect(legacyTarget(ctx, { org: 'settings' })).toBe('/o/acme/settings');
	});
});

// The sticky-nav switcher's fallback (F9 / spec wave-1 §6): a global page
// (`/user`, `/gc`, ...) has no `[org]`/`[project]` path segment, so it must
// fall back to the last-remembered scope -- without leaking one org's
// remembered project onto another org's page.
describe('resolveNavScope', () => {
	const rememberedProject = (recordedUnder: string, project: string) => (org: string) =>
		org === recordedUnder ? project : null;

	it('/user remembers acme/fw', () => {
		expect(
			resolveNavScope({
				paramsOrg: null,
				paramsProject: null,
				visibleOrgs: ['acme'],
				rememberedOrg: 'acme',
				rememberedProject: rememberedProject('acme', 'fw')
			})
		).toEqual({ org: 'acme', project: 'fw' });
	});

	it('/o/acme/members keeps the remembered project sticky', () => {
		// An org-scoped route has no project SEGMENT, but that is the route's
		// shape, not a deselection: clearing the switcher here forced a
		// reselect on every trip through the org pages (owner-reported). The
		// org-namespaced remembered project is safe to keep showing.
		expect(
			resolveNavScope({
				paramsOrg: 'acme',
				paramsProject: null,
				visibleOrgs: ['acme'],
				rememberedOrg: 'acme',
				rememberedProject: rememberedProject('acme', 'fw')
			})
		).toEqual({ org: 'acme', project: 'fw' });
	});

	it('an org page for org B never shows a project remembered under org A', () => {
		expect(
			resolveNavScope({
				paramsOrg: 'beta',
				paramsProject: null,
				visibleOrgs: ['acme', 'beta'],
				rememberedOrg: 'acme',
				rememberedProject: rememberedProject('acme', 'fw')
			})
		).toEqual({ org: 'beta', project: null });
	});

	it('a stale remembered org (no longer visible) renders none', () => {
		expect(
			resolveNavScope({
				paramsOrg: null,
				paramsProject: null,
				visibleOrgs: ['acme'],
				rememberedOrg: 'ghost',
				rememberedProject: rememberedProject('ghost', 'fw')
			})
		).toEqual({ org: null, project: null });
	});

	it('a remembered org with no remembered project renders project none, not a guess', () => {
		// The state ConsoleNav's project switcher must render as "nothing to
		// show" rather than an enabled-but-empty menu: a global page, an org
		// remembered and still visible, and no project ever remembered under
		// it (a fresh org, or one only ever visited via /members or /settings).
		expect(
			resolveNavScope({
				paramsOrg: null,
				paramsProject: null,
				visibleOrgs: ['acme'],
				rememberedOrg: 'acme',
				rememberedProject: () => null
			})
		).toEqual({ org: 'acme', project: null });
	});

	it('a project remembered under org A never renders under org B', () => {
		// Namespacing lives in `storage.ts#lastProject`, but this proves the
		// caller (the console layout) resolves the org BEFORE asking for the
		// remembered project, so a same-named `rememberedProject` lookup keyed
		// on a different org can never leak through.
		expect(
			resolveNavScope({
				paramsOrg: null,
				paramsProject: null,
				visibleOrgs: ['org-b'],
				rememberedOrg: 'org-b',
				rememberedProject: rememberedProject('org-a', 'firmware')
			})
		).toEqual({ org: 'org-b', project: null });
	});
});
