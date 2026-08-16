import { describe, expect, it } from 'vitest';

import { fetchFake, jsonResponse } from '$lib/testing/fetchFake';

import * as backendsApi from './backends';
import * as gcApi from './gc';
import * as instanceApi from './instance';
import * as keysApi from './keys';
import * as membersApi from './members';
import * as objectsApi from './objects';
import * as orgsApi from './orgs';
import * as projectsApi from './projects';
import * as robotsApi from './robots';
import * as siteAdminsApi from './siteAdmins';
import * as snippetsApi from './snippets';
import * as tokensApi from './tokens';
import { getAuthConfig } from './auth';
import { getMe } from './me';
import { memberProvenance, siteAdminProvenance } from './types';
import { tri, TRI_STATE_EMPTY_STRING, omitUndefined } from './patch';

import authConfigFixture from './testdata/auth-config.json';
import backendsFixture from './testdata/backends.json';
import createdKeyFixture from './testdata/created-key.json';
import createdOrgTokenFixture from './testdata/created-org-token.json';
import createdUserTokenFixture from './testdata/created-user-token.json';
import gcActivityFixture from './testdata/gc-activity.json';
import gcRunsFixture from './testdata/gc-runs.json';
import instanceFixture from './testdata/instance.json';
import keysFixture from './testdata/keys.json';
import meFixture from './testdata/me.json';
import meNoAvatarFixture from './testdata/me-no-avatar.json';
import meRobotFixture from './testdata/me-robot.json';
import meSiteAdminFixture from './testdata/me-site-admin.json';
import objectsFixture from './testdata/objects.json';
import orgMembersFixture from './testdata/org-members.json';
import orgUsageFixture from './testdata/org-usage.json';
import orgsFixture from './testdata/orgs.json';
import projectUsageFixture from './testdata/project-usage.json';
import projectsFixture from './testdata/projects.json';
import robotsFixture from './testdata/robots.json';
import siteAdminsFixture from './testdata/site-admins.json';
import snippetMintedFixture from './testdata/snippet-minted.json';
import snippetPreviewFixture from './testdata/snippet-preview.json';
import userTokensFixture from './testdata/user-tokens.json';

import reqCreateBackend from './testdata/req-create-backend.json';
import reqCreateKey from './testdata/req-create-key.json';
import reqCreateOrg from './testdata/req-create-org.json';
import reqCreateOrgToken from './testdata/req-create-org-token.json';
import reqCreateProject from './testdata/req-create-project.json';
import reqCreateRobot from './testdata/req-create-robot.json';
import reqCreateUserToken from './testdata/req-create-user-token.json';
import reqPutOrgMember from './testdata/req-put-org-member.json';
import reqPutProjectMember from './testdata/req-put-project-member.json';
import reqSnippetMint from './testdata/req-snippet-mint.json';
import reqSnippetPreview from './testdata/req-snippet-preview.json';
import reqTriggerGCRun from './testdata/req-trigger-gc-run.json';
import reqUpdateBackendClearQuota from './testdata/req-update-backend-clear-quota.json';
import reqUpdateOrgClearDefaults from './testdata/req-update-org-clear-defaults.json';
import reqUpdateOrgRename from './testdata/req-update-org-rename.json';
import reqUpdateOrgSetDefaults from './testdata/req-update-org-set-defaults.json';

/**
 * Fixture contract tests.
 *
 * These are the ONLY guard against hand-written-type drift, and they work in
 * both directions:
 *
 *  - **Response fixtures** are served through the real client and decoded into
 *    the declared types, then asserted on their KEY SETS. A tag that is renamed
 *    on the Go side shows up here as a missing key, which is exactly the failure
 *    a field-by-field spot check misses. The org-members fixture deliberately
 *    contains a CLAIM-DERIVED member, because `org_role_source` (Go field:
 *    `Source`) is the single highest-drift-risk tag on this API.
 *  - **Request fixtures** exist because the server decodes with
 *    `DisallowUnknownFields`: one extra key is a hard 400, and one MISSING key
 *    is the difference between "leave this column alone" and "clear it".
 *
 * Regenerate them whenever `internal/api/types.go` or `snippets.go` changes.
 * This is a review-checklist item, not automation -- there is no cross-language
 * fixture sync to hook into and inventing one would cost more than it saves.
 */

function keys(value: object): string[] {
	return Object.keys(value).sort();
}

/** Serves one fixture and returns whatever the api module decoded. */
async function serve<T>(fixture: unknown, call: (fetch: typeof globalThis.fetch) => Promise<T>) {
	const f = fetchFake(() => jsonResponse(fixture));
	const decoded = await call(f.fetch);

	return { decoded, call: f.only() };
}

describe('response fixtures', () => {
	it('GET /auth/config', async () => {
		const { decoded } = await serve(authConfigFixture, (fetch) => getAuthConfig({ fetch }));

		expect(keys(decoded)).toEqual([
			'allow_self_serve_orgs',
			'authorization_endpoint',
			'client_id',
			'dev_login_enabled',
			'device_authorization_endpoint',
			'issuer',
			'oidc_enabled',
			'scopes',
			'token_endpoint'
		]);
		// The field the create-org affordance is gated on. It lives on this public
		// document precisely so a signed-out visitor can decide.
		expect(decoded.allow_self_serve_orgs).toBe(true);
	});

	it('GET /me', async () => {
		const { decoded } = await serve(meFixture, (fetch) => getMe({ fetch }));

		expect(decoded).not.toBeNull();
		expect(keys(decoded!)).toEqual([
			'avatar_url',
			'display_name',
			'email',
			'is_site_admin',
			'method',
			'orgs',
			'projects',
			'site_role',
			'user_id'
		]);
		expect(keys(decoded!.orgs[0])).toEqual(['id', 'name', 'role', 'slug']);
		expect(keys(decoded!.projects[0])).toEqual(['id', 'org_slug', 'role', 'slug']);
	});

	it('GET /me with no avatar_url: the default wire shape (dev-login, no picture claim)', async () => {
		const { decoded } = await serve(meNoAvatarFixture, (fetch) => getMe({ fetch }));

		// `avatar_url` is `json:"avatar_url,omitempty"` on the Go side, so an
		// unset one is an ABSENT key, not a present null -- this is the shape
		// most installs actually emit, and it is the branch that drives the
		// monogram fallback the avatar `<img>` degrades to.
		expect('avatar_url' in decoded!).toBe(false);
		expect(decoded!.avatar_url).toBeUndefined();
	});

	it('GET /me for a site admin: no memberships, every visibility', async () => {
		const { decoded } = await serve(meSiteAdminFixture, (fetch) => getMe({ fetch }));

		// The shape that makes `$lib/roles` necessary: empty `orgs`, and no `role`
		// anywhere, on the most privileged principal in the installation.
		expect(decoded!.is_site_admin).toBe(true);
		expect(decoded!.orgs).toEqual([]);
		expect(decoded!.projects).toEqual([]);
	});

	it('GET /me for a robot: no identity fields, method org_token, a robot grant', async () => {
		const { decoded } = await serve(meRobotFixture, (fetch) => getMe({ fetch }));

		// A robot has no users row -- UserID/Email/DisplayName are empty, not
		// absent, and inventing a display name would be a lie about what the
		// credential is.
		expect(decoded!.method).toBe('org_token');
		expect(decoded!.user_id).toBe('');
		expect(decoded!.email).toBe('');
		expect(decoded!.robot).toEqual({
			robot_id: 'b2c3d4e5-5f6a-4b7c-8d9e-0f1a2b3c4d5e',
			org_id: '1c9c9e2a-3e4f-4a5b-8c7d-6e5f4a3b2c1d',
			scope: 'write'
		});
	});

	it('GET /orgs', async () => {
		const { decoded } = await serve(orgsFixture, (fetch) => orgsApi.listOrgs({ fetch }));

		expect(keys(decoded.items[0])).toEqual([
			'created_at',
			'default_quota_bytes',
			'default_retention_window',
			'id',
			'name',
			'role',
			'slug',
			'updated_at'
		]);
		// `role` is omitempty; the second org has none, and the two B4 defaults are
		// present-and-null rather than absent.
		expect(decoded.items[1].role).toBeUndefined();
		expect(decoded.items[1].default_retention_window).toBeNull();
		expect(decoded.items[1].default_quota_bytes).toBeNull();
	});

	it('GET /orgs/{org}/projects', async () => {
		const { decoded } = await serve(projectsFixture, (fetch) =>
			projectsApi.listProjects('acme', { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'backends',
			'created_at',
			'id',
			'name',
			'org_id',
			'org_slug',
			'role',
			'slug',
			'updated_at'
		]);
		expect(decoded.items[0].backends).toContain('hashserv');
		expect(decoded.items[1].role).toBeUndefined();
	});

	it('GET /orgs/{org}/members carries provenance under org_role_source', async () => {
		const { decoded } = await serve(orgMembersFixture, (fetch) =>
			membersApi.listOrgMembers('acme', { fetch })
		);

		const [claimDerived, localOnly, both] = decoded.items;

		// The drift trap. Go field `Source`, wire tag `org_role_source`; effective
		// role is `org_role`, NOT `role`.
		expect(claimDerived.org_role_source).toBe('oidc_groups');
		expect(claimDerived.org_role).toBe('admin');
		expect(claimDerived.local_role).toBeUndefined();
		expect(claimDerived.granted_at).toBeNull();

		expect(localOnly.org_role_source).toBe('local');
		expect(both.org_role_source).toBe('oidc_groups+local');

		// The Remove control is disabled when there is no local half; the 409
		// `claim_derived_role` is then a backstop and not the normal path.
		expect(claimDerived.local_role).toBeUndefined();
		expect(both.local_role).toBe('admin');

		const p = memberProvenance(claimDerived);
		expect(p.source).toBe('oidc_groups');
		expect(p.oidcGroup).toBe('cn=bakery-admins,ou=groups,dc=acme,dc=dev');
		expect(p.local).toBeUndefined();
	});

	it('GET /site-admins uses DIFFERENT provenance tags from Member', async () => {
		const { decoded } = await serve(siteAdminsFixture, (fetch) =>
			siteAdminsApi.listSiteAdmins({ fetch })
		);

		const [claimDerived, local] = decoded.items;

		// site_role_oidc / site_oidc_group / site_role_local / site_role_source --
		// not one of them matches Member's spelling.
		expect(claimDerived.site_role).toBe('admin');
		expect(claimDerived.site_role_oidc).toBe('admin');
		expect(claimDerived.site_oidc_group).toBe(
			'cn=platform-admins,ou=groups,dc=acme,dc=dev'
		);
		expect(claimDerived.site_role_source).toBe('oidc_groups');
		expect(local.site_role_local).toBe('admin');

		// The break-glass grant has no granter to name, and that emptiness is a
		// finding rather than a gap: someone with database access made it.
		expect(local.granted_by).toBe('');

		const p = siteAdminProvenance(claimDerived);
		expect(p.source).toBe('oidc_groups');
		expect(p.oidcGroup).toBe('cn=platform-admins,ou=groups,dc=acme,dc=dev');
	});

	it('GET .../keys returns metadata with no token field at all', async () => {
		const { decoded } = await serve(keysFixture, (fetch) =>
			keysApi.listKeys('acme', 'firmware', { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'created_at',
			'expires_at',
			'id',
			'last_used_at',
			'name',
			'owner_email',
			'owner_id',
			'owner_name',
			'project_id',
			'revoked_at',
			'scope',
			'token_prefix'
		]);
		expect('token' in decoded.items[0]).toBe(false);
	});

	it('POST .../keys is the one response carrying a secret', async () => {
		const { decoded } = await serve(createdKeyFixture, (fetch) =>
			keysApi.createKey('acme', 'firmware', { name: 'x', scope: 'write' }, { fetch })
		);

		expect(decoded.token.startsWith('bkry_')).toBe(true);
		expect(decoded.token_prefix.startsWith('bkry_')).toBe(true);
	});

	it('GET /user/tokens returns metadata with no token field at all', async () => {
		const { decoded } = await serve(userTokensFixture, (fetch) => tokensApi.listUserTokens({ fetch }));

		expect(keys(decoded.items[0])).toEqual([
			'created_at',
			'expires_at',
			'id',
			'last_used_at',
			'max_scope',
			'name',
			'revoked_at',
			'token_prefix'
		]);
		expect('token' in decoded.items[0]).toBe(false);
		expect(decoded.items[0].token_prefix.startsWith('bkru_')).toBe(true);
	});

	it('POST /user/tokens is the one response carrying a secret', async () => {
		const { decoded } = await serve(createdUserTokenFixture, (fetch) =>
			tokensApi.createUserToken({ name: 'x', scope: 'write' }, { fetch })
		);

		expect(decoded.token.startsWith('bkru_')).toBe(true);
	});

	it('GET .../robots carries each robot with its tokens, live and revoked, metadata only', async () => {
		const { decoded } = await serve(robotsFixture, (fetch) => robotsApi.listRobots('acme', { fetch }));

		const robot = decoded.items[0];
		expect(keys(robot)).toEqual([
			'created_at',
			'created_by',
			'created_by_email',
			'description',
			'id',
			'name',
			'org_id',
			'tokens'
		]);

		// One live, one revoked -- and org_tokens.expires_at is NOT NULL, unlike
		// every other credential expiry in the API.
		const [live, revoked] = robot.tokens;
		expect(live.revoked_at).toBeNull();
		expect(revoked.revoked_at).not.toBeNull();
		expect(live.expires_at).not.toBeNull();
		expect('token' in live).toBe(false);
		expect(live.token_prefix.startsWith('bkro_')).toBe(true);
		// A robot deliberately outlives its creator: the FK behind created_by goes
		// SET NULL, but created_by_email is a snapshot that survives it.
		expect(revoked.created_by).toBe('');
		expect(revoked.created_by_email).toBe('bo@acme.dev');
	});

	it('POST .../robots/{robot}/tokens is the one response carrying a secret', async () => {
		const { decoded } = await serve(createdOrgTokenFixture, (fetch) =>
			robotsApi.createOrgToken(
				'acme',
				'b2c3d4e5-5f6a-4b7c-8d9e-0f1a2b3c4d5e',
				{ name: 'ci-2026', scope: 'write', expires_at: '2027-08-16T10:11:12.000Z' },
				{ fetch }
			)
		);

		expect(decoded.token.startsWith('bkro_')).toBe(true);
	});

	it('GET .../backends', async () => {
		const { decoded } = await serve(backendsFixture, (fetch) =>
			backendsApi.listBackends('acme', 'firmware', { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'config',
			'created_at',
			'enabled',
			'id',
			'kind',
			'project_id',
			'quota_bytes',
			'read_auth_required',
			'retention_window',
			'updated_at'
		]);
		// No `write_auth_required`, ever: an unauthenticated write is not a
		// representable state.
		expect('write_auth_required' in decoded.items[0]).toBe(false);
		expect(decoded.items[1].quota_bytes).toBeNull();
	});

	it('GET /orgs/{org}/usage: counts are honest zeroes, only the timestamp is nullable', async () => {
		const { decoded } = await serve(orgUsageFixture, (fetch) =>
			orgsApi.getOrgUsage('acme', { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'logical_bytes',
			'measured_at',
			'objects_count',
			'project_slug'
		]);
		expect(decoded.items[1].objects_count).toBe(0);
		expect(decoded.items[1].measured_at).toBeNull();
	});

	it('GET .../{project}/usage: counts ARE nullable, and hashserv is all nulls', async () => {
		const { decoded } = await serve(projectUsageFixture, (fetch) =>
			projectsApi.getProjectUsage('acme', 'firmware', { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'kind',
			'logical_bytes',
			'measured_at',
			'objects_count',
			'quota_bytes',
			'retention_window'
		]);

		const hashserv = decoded.items.find((u) => u.kind === 'hashserv');
		// Structurally never measured: M6's planner gives hashserv no stages.
		expect(hashserv?.measured_at).toBeNull();
		expect(hashserv?.logical_bytes).toBeNull();

		const downloads = decoded.items.find((u) => u.kind === 'downloads');
		// Measured, and genuinely empty. A different fact from the one above.
		expect(downloads?.measured_at).not.toBeNull();
		expect(downloads?.logical_bytes).toBe(0);
	});

	it('GET .../backends/{kind}/objects is keyset-paginated', async () => {
		const { decoded, call } = await serve(objectsFixture, (fetch) =>
			objectsApi.listCacheObjects(
				'acme',
				'firmware',
				'sstate',
				{ prefix: 'a3/', limit: 50 },
				{ fetch }
			)
		);

		expect(keys(decoded.items[0])).toEqual([
			'accessed_at',
			'created_at',
			'digest',
			'key',
			'namespace',
			'size_bytes'
		]);
		// `accessed_at` null means "never read since the M6 upgrade" -- the
		// ordinary day-one state, not an error.
		expect(decoded.items[1].accessed_at).toBeNull();
		expect(decoded.next_cursor).toBe(decoded.items[1].key);
		expect(call.url).toContain('prefix=a3%2F');
		expect(call.url).toContain('limit=50');
	});

	it('GET /gc/runs', async () => {
		const { decoded, call } = await serve(gcRunsFixture, (fetch) =>
			gcApi.listGCRuns({ limit: 20 }, { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'blobs_deleted',
			'blobs_marked',
			'bytes_reclaimed',
			'dry_run',
			'finished_at',
			'hashserv_rows_deleted',
			'id',
			'objects_deleted',
			'started_at',
			'status',
			'trigger'
		]);
		// `error` is omitempty and appears only on the failed run.
		expect(decoded.items[0].error).toBeUndefined();
		expect(decoded.items[1].error).toContain('context deadline exceeded');
		expect(decoded.next_cursor).toBe(811);
		// include_usage is NOT sent unless asked for: usage runs mint a row every
		// six hours forever.
		expect(call.url).not.toContain('include_usage');
	});

	it('GET /orgs/{org}/gc/activity distinguishes swept-and-empty from not-swept', async () => {
		const { decoded } = await serve(gcActivityFixture, (fetch) =>
			orgsApi.getOrgGCActivity('acme', { limit: 20 }, { fetch })
		);

		expect(keys(decoded.items[0])).toEqual([
			'backends',
			'finished_at',
			'run_id',
			'started_at',
			'status'
		]);
		expect(keys(decoded.items[0].backends[0])).toEqual([
			'bytes_freed',
			'kind',
			'objects_deleted',
			'project_slug'
		]);

		// A row AT ZERO is "swept, nothing eligible" -- present, and different from
		// a backend with no row at all.
		const bazel = decoded.items[0].backends.find((b) => b.kind === 'bazel');
		expect(bazel?.objects_deleted).toBe(0);
		expect(decoded.items[0].backends.some((b) => b.kind === 'oci')).toBe(false);
	});

	it('GET /instance', async () => {
		const { decoded } = await serve(instanceFixture, (fetch) =>
			instanceApi.getInstance({ fetch })
		);

		expect(keys(decoded)).toEqual([
			'allow_local_site_admins',
			'allow_multi_instance',
			'allow_self_serve_orgs',
			'dev_login_enabled',
			'external_url',
			'gc_enabled',
			'gc_grace_period',
			'gc_interval',
			'gc_usage_interval',
			'grpc_addr',
			'grpc_external_endpoint',
			'metrics_addr',
			'oidc_issuer',
			'public_addr',
			'storage_driver',
			'version'
		]);
	});

	it('a snippet PREVIEW carries no api_key and a placeholder credential', async () => {
		const { decoded } = await serve(snippetPreviewFixture, (fetch) =>
			snippetsApi.previewSnippet('acme', 'firmware', 'yocto', { fetch })
		);

		expect(decoded.preview).toBe(true);
		// Absent, not a zero-valued object: a zero object renders as a one-time
		// reveal of nothing, which a user then pastes into a config.
		expect(decoded.api_key).toBeUndefined();
		expect(decoded.netrc).toContain('«create an API key»');

		// The regression this whole wave exists for: BB_HASHSERVE is emitted only
		// when a hashserv backend is configured, and the netrc block carries TWO
		// lines keyed differently -- hostname for HTTP Basic, full URL for hashserv.
		expect(decoded.local_conf).toContain('BB_SIGNATURE_HANDLER = "OEEquivHash"');
		expect(decoded.local_conf).toContain('BB_HASHSERVE');
		const netrcLines = decoded.netrc.split('\n').filter((l) => l.trim() !== '');
		expect(netrcLines).toHaveLength(2);
		expect(netrcLines[0]).toContain('machine bakery.corp ');
		expect(netrcLines[1]).toContain('machine wss://bakery.corp/cache/acme/firmware/hashserv');
	});

	it('a MINTED snippet carries the token once, and warnings the screen must render', async () => {
		const { decoded } = await serve(snippetMintedFixture, (fetch) =>
			snippetsApi.generateSnippet('acme', 'firmware', { tool: 'sccache' }, { fetch })
		);

		expect(decoded.preview).toBe(false);
		expect(decoded.api_key?.token.startsWith('bkry_')).toBe(true);
		// Blocks may legitimately be empty: an sccache snippet has no local.conf.
		expect(decoded.local_conf).toBe('');
		expect(decoded.warnings?.[0]).toContain('one opaque bkry_ token');
		// And never an id:secret pair, which is a credential shape that cannot exist.
		expect(JSON.stringify(decoded)).not.toMatch(/bks_/);
	});
});

describe('request fixtures', () => {
	async function bodyOf(call: (fetch: typeof globalThis.fetch) => Promise<unknown>) {
		const f = fetchFake(() => jsonResponse({}));
		await call(f.fetch);
		const raw = f.only().body;

		return raw === null ? null : (JSON.parse(raw) as unknown);
	}

	it('POST /orgs', async () => {
		expect(await bodyOf((fetch) => orgsApi.createOrg({ slug: 'acme', name: 'Acme' }, { fetch })))
			.toEqual(reqCreateOrg);
	});

	it('PATCH /orgs/{org}: a plain rename must NOT emit the two default_* keys', async () => {
		// The trap this test exists for: an explicit `"default_quota_bytes": null`
		// is a deliberate CLEAR. A rename that emits it wipes the org's seeds
		// silently -- the exact bug `omitempty` fixes on the Go caller side.
		const body = await bodyOf((fetch) =>
			orgsApi.updateOrg('acme', { name: 'Acme Corp' }, { fetch })
		);

		expect(body).toEqual(reqUpdateOrgRename);
		expect(Object.keys(body as object)).toEqual(['name']);
	});

	it('PATCH /orgs/{org}: an explicit null IS the clear', async () => {
		const body = await bodyOf((fetch) =>
			orgsApi.updateOrg(
				'acme',
				{ name: 'Acme Corp', default_retention_window: null, default_quota_bytes: null },
				{ fetch }
			)
		);

		expect(body).toEqual(reqUpdateOrgClearDefaults);
	});

	it('PATCH /orgs/{org}: a value sets', async () => {
		const body = await bodyOf((fetch) =>
			orgsApi.updateOrg(
				'acme',
				{
					name: 'Acme Corp',
					default_retention_window: '720h',
					default_quota_bytes: 549755813888
				},
				{ fetch }
			)
		);

		expect(body).toEqual(reqUpdateOrgSetDefaults);
	});

	it('POST /orgs/{org}/projects', async () => {
		expect(
			await bodyOf((fetch) =>
				projectsApi.createProject('acme', { slug: 'firmware', name: 'Firmware' }, { fetch })
			)
		).toEqual(reqCreateProject);
	});

	it('POST .../keys', async () => {
		expect(
			await bodyOf((fetch) =>
				keysApi.createKey('acme', 'firmware', { name: 'ci-runner', scope: 'write' }, { fetch })
			)
		).toEqual(reqCreateKey);
	});

	it('PUT .../members/{user} takes an EMAIL in the path', async () => {
		const f = fetchFake(() => jsonResponse({}));
		await membersApi.putOrgMember('acme', 'alice@corp.example', { role: 'admin' }, {
			fetch: f.fetch
		});

		const call = f.only();
		expect(call.url).toBe('/api/v1/orgs/acme/members/alice%40corp.example');
		expect(JSON.parse(call.body!)).toEqual(reqPutOrgMember);
	});

	it('PUT .../projects/{project}/members/{user}', async () => {
		expect(
			await bodyOf((fetch) =>
				membersApi.putProjectMember('acme', 'firmware', 'bo@acme.dev', { role: 'writer' }, {
					fetch
				})
			)
		).toEqual(reqPutProjectMember);
	});

	it('POST .../backends', async () => {
		expect(
			await bodyOf((fetch) =>
				backendsApi.createBackend(
					'acme',
					'firmware',
					{
						kind: 'sstate',
						enabled: true,
						read_auth_required: true,
						config: {},
						retention_window: '2160h'
					},
					{ fetch }
				)
			)
		).toEqual(reqCreateBackend);
	});

	it('PATCH .../backends/{kind}: clearing the quota is an explicit null', async () => {
		const body = await bodyOf((fetch) =>
			backendsApi.updateBackend('acme', 'firmware', 'sstate', { quota_bytes: null }, { fetch })
		);

		expect(body).toEqual(reqUpdateBackendClearQuota);
	});

	it('POST .../snippet preview vs mint are two different bodies', async () => {
		expect(
			await bodyOf((fetch) => snippetsApi.previewSnippet('acme', 'firmware', 'yocto', { fetch }))
		).toEqual(reqSnippetPreview);

		const mint = await bodyOf((fetch) =>
			snippetsApi.generateSnippet('acme', 'firmware', { tool: 'yocto', scope: 'write' }, {
				fetch
			})
		);

		// The mint path omits `preview` ENTIRELY. Sending `preview: false` would
		// be the same request with one more thing to get wrong.
		expect(mint).toEqual(reqSnippetMint);
		expect('preview' in (mint as object)).toBe(false);
	});

	it('POST /gc/run always states dry_run: its zero value is a REAL sweep', async () => {
		expect(await bodyOf((fetch) => gcApi.triggerGCRun({ dry_run: true }, { fetch }))).toEqual(
			reqTriggerGCRun
		);
	});

	it('POST /user/tokens: "Never" omits expires_at entirely, not null', async () => {
		// The interesting shape: `CreateUserTokenRequest.ExpiresAt` is a Go
		// `*time.Time` with no `omitempty`, and the console's "Never" option
		// (user/+page.svelte's expiresAtFromDays) returns `undefined` rather
		// than `null` for it -- JSON.stringify drops an undefined key, so the
		// wire body has no `expires_at` key at all. Both happen to decode the
		// same way server-side; this pins the shape either was free to drift.
		const body = await bodyOf((fetch) =>
			tokensApi.createUserToken({ name: 'build-host-1', scope: 'write' }, { fetch })
		);

		expect(body).toEqual(reqCreateUserToken);
		expect('expires_at' in (body as object)).toBe(false);
	});

	it('POST .../robots', async () => {
		expect(
			await bodyOf((fetch) =>
				robotsApi.createRobot(
					'acme',
					{ name: 'ci-runner', description: 'Bazel remote cache for the CI fleet' },
					{ fetch }
				)
			)
		).toEqual(reqCreateRobot);
	});

	it('POST .../robots/{robot}/tokens: expires_at is REQUIRED, unlike a user token', async () => {
		expect(
			await bodyOf((fetch) =>
				robotsApi.createOrgToken(
					'acme',
					'b2c3d4e5-5f6a-4b7c-8d9e-0f1a2b3c4d5e',
					{ name: 'ci-2026', scope: 'write', expires_at: '2027-08-16T10:11:12.000Z' },
					{ fetch }
				)
			)
		).toEqual(reqCreateOrgToken);
	});
});

describe('the three-state encoding', () => {
	it('refuses the empty string, which is the fourth state that does not exist', () => {
		expect(() => tri('', 'quota_bytes')).toThrowError(TRI_STATE_EMPTY_STRING);
	});

	it('passes undefined (omit), null (clear) and a value (set) through', () => {
		expect(tri(undefined, 'quota_bytes')).toBeUndefined();
		expect(tri(null, 'quota_bytes')).toBeNull();
		expect(tri(42, 'quota_bytes')).toBe(42);
	});

	it('omitUndefined drops undefined and KEEPS null', () => {
		expect(omitUndefined({ a: 1, b: undefined, c: null })).toEqual({ a: 1, c: null });
	});
});
