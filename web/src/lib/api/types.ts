import type { TriState } from './patch';

/**
 * The wire types.
 *
 * **Every property name here is a Go struct TAG, never a Go field name.** That
 * distinction is not cosmetic on this API: `Member.Source` is on the wire as
 * `org_role_source`, `SiteAdmin.OIDCRole` as `site_role_oidc`, and
 * `SiteAdmin.OIDCGroup` as `site_oidc_group`. A hand-written mirror derived from
 * field names gets all three wrong, and the wrongness is invisible until a
 * claim-derived member appears -- which is why `testdata/` carries one.
 *
 * Nullability follows the tags too: a `*T` with a plain tag is `T | null`
 * (always present, sometimes null); a field with `,omitempty` is `T | undefined`
 * (absent when empty). Timestamps are RFC 3339 strings; the API never formats a
 * duration or a byte count for display, so neither does this file.
 */

/** Every collection response: an object with `items`, never a bare array. */
export interface ListResponse<T> {
	items: T[];
}

// ---------------------------------------------------------------------------
// auth (internal/auth/oidc.go, service.go, devlogin.go)
// ---------------------------------------------------------------------------

/** `GET /auth/config`. Public: it is fetched before anyone is signed in. */
export interface AuthConfig {
	issuer: string;
	client_id: string;
	scopes: string[];
	authorization_endpoint?: string;
	token_endpoint?: string;
	device_authorization_endpoint?: string;
	oidc_enabled: boolean;
	/** Mirrors `DEV_LOGIN_ENABLED`. Reported here, never settable from here. */
	dev_login_enabled: boolean;
	/**
	 * Mirrors `--allow-self-serve-orgs`. It lives on this PUBLIC document because
	 * the create-org affordance has to be decidable on the first screen a
	 * signed-out visitor sees.
	 */
	allow_self_serve_orgs: boolean;
}

/** `POST /auth/dev-login`. Takes no body and always seeds the same user. */
export interface DevLoginResponse {
	email: string;
	org: string;
	project: string;
}

// ---------------------------------------------------------------------------
// me (internal/api/types.go, me.go)
// ---------------------------------------------------------------------------

export type OrgRole = 'member' | 'admin' | 'owner';
export type ProjectRole = 'reader' | 'writer' | 'admin';
export type SiteRole = 'user' | 'admin';
export type KeyScope = 'read' | 'write';
export type BackendKind = 'sstate' | 'downloads' | 'hashserv' | 'bazel' | 'oci';

export interface MeOrg {
	id: string;
	slug: string;
	name: string;
	role: OrgRole;
}

export interface MeProject {
	id: string;
	slug: string;
	org_slug: string;
	role: ProjectRole;
}

export interface MeKeyGrant {
	key_id: string;
	project_id: string;
	scope: KeyScope;
}

/**
 * `GET /me`.
 *
 * `orgs` comes from the caller's own MEMBERSHIPS, so a site admin who is a
 * member of nothing has `orgs: []` while being able to read every org in the
 * installation. Nothing may gate on `orgs`/`role` without also consulting
 * `is_site_admin` -- see `$lib/roles`.
 *
 * `projects` is likewise membership-only, and an org owner with no project
 * membership has `projects: []` while being able to read every project in the
 * org. The nav therefore reads `GET /orgs/{org}/projects`, never this.
 */
export interface Me {
	user_id: string;
	email: string;
	display_name: string;
	/** The IdP `picture` claim, https-only. Absent when the IdP asserted none. */
	avatar_url?: string;
	/** session | bearer | api_key | user_token | org_token | dev */
	method: string;
	site_role: SiteRole;
	is_site_admin: boolean;
	orgs: MeOrg[];
	projects: MeProject[];
	/** Present only when the request authenticated WITH a key. */
	api_key?: MeKeyGrant;
	/**
	 * Present only when the request authenticated with a robot (`bkro_`) token.
	 * When it is set, `user_id`, `email` and `display_name` are empty -- a robot
	 * is owned by an org and has no user row behind it.
	 */
	robot?: MeRobotGrant;
}

/**
 * What a `bkro_` request reports about itself. There is no project id: the grant
 * is one ORG at one scope, covering every project in it, present and future.
 */
export interface MeRobotGrant {
	robot_id: string;
	org_id: string;
	scope: KeyScope;
}

// ---------------------------------------------------------------------------
// orgs and projects
// ---------------------------------------------------------------------------

export interface Org {
	id: string;
	slug: string;
	name: string;
	/** The CALLER's role. Absent for a site admin who is not a member. */
	role?: OrgRole;
	/** B4. The seed a new backend in this org inherits; null means per-kind default. */
	default_retention_window: string | null;
	default_quota_bytes: number | null;
	created_at: string;
	updated_at: string;
}

export interface CreateOrgRequest {
	slug: string;
	name: string;
}

/** `PATCH /orgs/{org}`. The slug is immutable by design. */
export interface UpdateOrgRequest {
	name: string;
	default_retention_window?: TriState<string>;
	default_quota_bytes?: TriState<number>;
}

export interface Project {
	id: string;
	org_id: string;
	org_slug: string;
	slug: string;
	name: string;
	/** The caller's PROJECT role; absent when access comes from an org role. */
	role?: ProjectRole;
	/** The kinds configured on this project. */
	backends: BackendKind[];
	created_at: string;
	updated_at: string;
}

export interface CreateProjectRequest {
	slug: string;
	name: string;
}

export interface UpdateProjectRequest {
	name: string;
}

// ---------------------------------------------------------------------------
// members and site admins
//
// The two provenance shapes DO NOT share tag names. `<Provenance>`-style
// rendering goes through the adapters below, never through a cast.
// ---------------------------------------------------------------------------

/** `oidc_groups` | `local` | `oidc_groups+local`. */
export type RoleSource = 'oidc_groups' | 'local' | 'oidc_groups+local';

export interface Member {
	user_id: string;
	email: string;
	display_name: string;
	/** The EFFECTIVE role: greatest(oidc_role, local_role), generated by the database. */
	org_role: OrgRole;
	oidc_role?: OrgRole;
	oidc_group?: string;
	local_role?: OrgRole;
	granted_by?: string;
	granted_by_email?: string;
	granted_at: string | null;
	/** Only on a project's member list; `''` there means "no role in this project". */
	project_role?: ProjectRole;
	/** Note the tag: `org_role_source`, not `source`. */
	org_role_source?: RoleSource;
}

export interface SiteAdmin {
	user_id: string;
	email: string;
	display_name: string;
	site_role: SiteRole;
	/** Note the tags: `site_role_oidc` and `site_oidc_group`, not `oidc_*`. */
	site_role_oidc?: SiteRole;
	site_oidc_group?: string;
	site_role_local?: SiteRole;
	granted_by?: string;
	granted_by_email?: string;
	granted_at: string | null;
	site_role_source: RoleSource;
}

/**
 * The normalized provenance shape both rosters render through.
 *
 * It exists so one presentational component can serve both listings without
 * either of the two tag vocabularies leaking into markup -- and so the drift, if
 * a tag ever changes, is a type error in one adapter instead of an empty badge
 * on a screen.
 */
/**
 * Deliberately carries no `effective` field. Both consuming rosters already
 * render the effective role through their own dedicated control next to this
 * component -- `members`'s org-role `<Select>` shows `Member.org_role`,
 * `site-admins` is a roster of admins and needs no separate column -- so a
 * second copy here would just be one more place the same value could drift.
 * `<Provenance>` answers WHY somebody holds a role, never WHAT it is.
 */
export interface Provenance {
	oidc: string | undefined;
	oidcGroup: string | undefined;
	local: string | undefined;
	source: RoleSource | undefined;
	grantedBy: string | undefined;
	grantedByEmail: string | undefined;
	grantedAt: string | null;
}

export function memberProvenance(m: Member): Provenance {
	return {
		oidc: m.oidc_role,
		oidcGroup: m.oidc_group,
		local: m.local_role,
		source: m.org_role_source,
		grantedBy: m.granted_by,
		grantedByEmail: m.granted_by_email,
		grantedAt: m.granted_at
	};
}

export function siteAdminProvenance(a: SiteAdmin): Provenance {
	return {
		oidc: a.site_role_oidc,
		oidcGroup: a.site_oidc_group,
		local: a.site_role_local,
		source: a.site_role_source,
		grantedBy: a.granted_by,
		grantedByEmail: a.granted_by_email,
		grantedAt: a.granted_at
	};
}

export interface PutOrgMemberRequest {
	role: OrgRole;
}

export interface PutProjectMemberRequest {
	role: ProjectRole;
}

/**
 * `DELETE /orgs/{org}/members/{user}`.
 *
 * The API owns only the LOCAL half. `still_a_member` true means the person is
 * still in the org on a group claim, still holds their project roles and still
 * holds every key they minted -- so this response is rendered as a WARNING, not
 * a success.
 */
export interface OrgMemberRemoval {
	user_id: string;
	local_role_revoked: boolean;
	still_a_member: boolean;
	membership?: Member;
	message: string;
}

export interface SiteAdminRemoval {
	user_id: string;
	local_role_revoked: boolean;
	still_a_site_admin: boolean;
	admin?: SiteAdmin;
	message: string;
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

export interface APIKey {
	id: string;
	name: string;
	project_id: string;
	/** `bkry_` plus 8 characters. Non-secret; the handle after the one-time reveal. */
	token_prefix: string;
	scope: KeyScope;
	owner_id: string;
	owner_email: string;
	owner_name: string;
	created_at: string;
	expires_at: string | null;
	last_used_at: string | null;
	revoked_at: string | null;
}

/**
 * The ONE response in the API that carries a secret. `token` exists here and
 * nowhere else -- the schema stores only its SHA-256 -- so it is shown once and
 * never re-fetched.
 */
export interface CreatedAPIKey extends APIKey {
	token: string;
}

export interface CreateKeyRequest {
	name: string;
	scope: KeyScope;
	/** Absent or null means the key never expires. */
	expires_at?: string | null;
}

// ---------------------------------------------------------------------------
// cache backends
// ---------------------------------------------------------------------------

export interface Backend {
	/** A bigint, not a uuid: `cache_backends.id`. */
	id: number;
	project_id: string;
	kind: BackendKind;
	enabled: boolean;
	/**
	 * There is deliberately no `write_auth_required`: writes ALWAYS require a
	 * key, and an unauthenticated write is not a representable state.
	 */
	read_auth_required: boolean;
	/** Kind-specific; opaque here. Carries NO secrets by design. */
	config: unknown;
	/** A Go duration string ("2160h"). null is "retain forever", never "0s". */
	retention_window: string | null;
	/** LOGICAL bytes, not a disk figure. null is "no cap". */
	quota_bytes: number | null;
	created_at: string;
	updated_at: string;
}

export interface CreateBackendRequest {
	kind: BackendKind;
	enabled?: boolean;
	read_auth_required?: boolean;
	config: unknown;
	retention_window?: TriState<string>;
	quota_bytes?: TriState<number>;
}

export interface UpdateBackendRequest {
	enabled?: boolean;
	read_auth_required?: boolean;
	config?: unknown;
	retention_window?: TriState<string>;
	quota_bytes?: TriState<number>;
}

// ---------------------------------------------------------------------------
// snippets (internal/api/snippets.go)
// ---------------------------------------------------------------------------

export type SnippetTool =
	| 'yocto'
	| 'moon'
	| 'ccache'
	| 'sccache'
	| 'bazel'
	| 'containerd'
	| 'buildkit'
	| 'podman'
	| 'docker';

export const SNIPPET_TOOLS: readonly SnippetTool[] = [
	'yocto',
	'moon',
	'ccache',
	'sccache',
	'bazel',
	'containerd',
	'buildkit',
	'podman',
	'docker'
] as const;

/**
 * `POST .../snippet`.
 *
 * `preview: true` returns the full config with a placeholder credential and
 * **mints nothing** (200). A request WITHOUT the field is the mint path and
 * creates exactly one key (201). Never send `preview: false` as a separate
 * thing to reason about -- it is the same request as omitting it, and the
 * server decodes with `DisallowUnknownFields` so no other key is admissible.
 */
export interface SnippetRequest {
	tool?: SnippetTool;
	scope?: KeyScope;
	key_name?: string;
	preview?: boolean;
}

export interface SnippetFile {
	path: string;
	language: string;
	content: string;
}

export interface SnippetEnvVar {
	name: string;
	value: string;
}

/**
 * `SnippetResponse`.
 *
 * EVERY BLOCK IS OPTIONAL and an absent block means the project has no backend
 * to serve it. `warnings` then names the omission -- so the screen renders
 * `warnings` FIRST and unconditionally. A snippet with a missing block and an
 * unrendered warning is worse than no snippet, because it looks complete and
 * produces a green build that caches nothing.
 */
export interface SnippetResponse {
	tool: SnippetTool;
	/** Bare hostname, no scheme and no port: what a `~/.netrc` machine line keys on. */
	host: string;
	base_url: string;
	/** May be `''` when no backend this tool needs is configured. */
	local_conf: string;
	/** Up to TWO lines, keyed differently on purpose (hostname vs full URL). */
	netrc: string;
	push_commands: string[];
	files?: SnippetFile[];
	env?: SnippetEnvVar[];
	/** Absent on a preview. A zero-valued object here would be a reveal of nothing. */
	api_key?: CreatedAPIKey;
	preview: boolean;
	warnings?: string[];
}

// ---------------------------------------------------------------------------
// usage (B2)
// ---------------------------------------------------------------------------

/** `GET /orgs/{org}/usage`. Counts are honest zeroes; only the timestamp is nullable. */
export interface OrgProjectUsage {
	project_slug: string;
	objects_count: number;
	logical_bytes: number;
	/** null when NOT ONE backend of this project has ever reported. */
	measured_at: string | null;
}

/**
 * `GET .../{project}/usage`.
 *
 * Note the shape difference from `OrgProjectUsage`: here the counts are
 * NULLABLE, because "no `cache_backend_usage` row yet" and "measured, and it is
 * zero" are different facts and only the second may be rendered as `0 B`.
 */
export interface ProjectBackendUsage {
	kind: BackendKind;
	objects_count: number | null;
	logical_bytes: number | null;
	measured_at: string | null;
	quota_bytes: number | null;
	retention_window: string | null;
}

// ---------------------------------------------------------------------------
// object browser (B3)
// ---------------------------------------------------------------------------

export interface CacheObject {
	namespace: string;
	key: string;
	/** Lower-case hex sha256. */
	digest: string;
	size_bytes: number;
	created_at: string;
	/**
	 * APPROXIMATE. The toucher writes on a staleness ramp -- up to 24h coarse for
	 * the first week after the M6 upgrade -- so this is rendered with a
	 * qualifier, never as a precise timestamp. null means "never read since the
	 * upgrade", which is the ordinary day-one state.
	 */
	accessed_at: string | null;
}

export interface CacheObjectList {
	items: CacheObject[];
	/** Pass as the next request's `after_key`. null means this was the last page. */
	next_cursor: string | null;
}

// A `type`, not an `interface`: an interface has no implicit index signature,
// so it is not assignable to the client's `Record<string, QueryValue>` query bag.
export type ListObjectsQuery = {
	namespace?: string;
	prefix?: string;
	after_key?: string;
	/** CLAMPED server-side to 200, never rejected. Default 50. */
	limit?: number;
}

// ---------------------------------------------------------------------------
// gc
// ---------------------------------------------------------------------------

export type GCRunStatus = 'running' | 'succeeded' | 'failed';
export type GCRunTrigger = 'interval' | 'api' | 'usage';

export interface GCRun {
	id: number;
	status: GCRunStatus;
	trigger: GCRunTrigger;
	dry_run: boolean;
	started_at: string;
	finished_at: string | null;
	error?: string;
	objects_deleted: number;
	/** Broken out from `objects_deleted`: hashserv is the GC root. */
	hashserv_rows_deleted: number;
	blobs_marked: number;
	blobs_deleted: number;
	bytes_reclaimed: number;
}

export interface GCRunList {
	items: GCRun[];
	/** Pass as the next request's `before`. */
	next_cursor: number | null;
}

// A `type` for the same reason as ListObjectsQuery above.
export type ListGCRunsQuery = {
	status?: GCRunStatus;
	before?: number;
	limit?: number;
	/** Default false: usage runs mint a row every six hours forever. */
	include_usage?: boolean;
}

export interface TriggerGCRunRequest {
	dry_run: boolean;
}

export interface TriggerGCRunResponse {
	id: number;
	status: GCRunStatus;
}

/** B7: one backend's slice of one run, scoped to the caller's own org. */
export interface GCActivityBackend {
	project_slug: string;
	kind: BackendKind;
	objects_deleted: number;
	/** LOGICAL bytes, and undercounted for OCI manifest deletions by design. */
	bytes_freed: number;
}

export interface GCActivityRun {
	run_id: number;
	started_at: string;
	finished_at: string | null;
	status: GCRunStatus;
	backends: GCActivityBackend[];
}

export interface GCActivityList {
	items: GCActivityRun[];
	next_cursor: number | null;
}

// ---------------------------------------------------------------------------
// instance (B6)
// ---------------------------------------------------------------------------

/** `GET /instance`. Site admin. A boot-time config echo; never Prometheus. */
export interface InstanceInfo {
	version: string;
	storage_driver: string;
	public_addr: string;
	metrics_addr: string;
	grpc_addr: string;
	external_url: string;
	oidc_issuer: string;
	dev_login_enabled: boolean;
	/** Empty while `grpc_addr` is set means the snippet generator is GUESSING. */
	grpc_external_endpoint: string;
	allow_self_serve_orgs: boolean;
	allow_local_site_admins: boolean;
	allow_multi_instance: boolean;
	gc_enabled: boolean;
	gc_interval: string;
	gc_usage_interval: string;
	gc_grace_period: string;
}

// ---------------------------------------------------------------------------
// personal access tokens (`bkru_`) and robots (`bkro_`)
// ---------------------------------------------------------------------------

/**
 * A personal access token, metadata only. The plaintext exists in exactly one
 * response, ever -- `CreatedUserToken.token`.
 */
export interface UserToken {
	id: string;
	name: string;
	/** `bkru_` plus 8 characters. Non-secret; the handle after the one-time reveal. */
	token_prefix: string;
	/**
	 * The CEILING on what this token may be used for, not a grant. Its real
	 * authority is its owner's live roles, narrowed by this.
	 */
	max_scope: KeyScope;
	created_at: string;
	expires_at: string | null;
	last_used_at: string | null;
	revoked_at: string | null;
}

export interface CreatedUserToken extends UserToken {
	/** Shown exactly once. There is no query that can return it again. */
	token: string;
}

/** An org-owned machine identity. Not a user: it has no console session. */
export interface Robot {
	id: string;
	org_id: string;
	name: string;
	description: string;
	created_by: string;
	/**
	 * Snapshotted at creation. `created_by` goes null when its referent is
	 * deleted, and a robot deliberately outlives its creator -- so this is the
	 * half of the audit trail that survives the human.
	 */
	created_by_email: string;
	created_at: string;
	tokens: OrgToken[];
}

/** A robot's credential, metadata only. */
export interface OrgToken {
	id: string;
	robot_id: string;
	org_id: string;
	name: string;
	/** `bkro_` plus 8 characters. */
	token_prefix: string;
	scope: KeyScope;
	/**
	 * NOT nullable, unlike every other expiry in the API. A robot survives its
	 * creator, so expiry is the countervailing control and "never" is not
	 * representable.
	 */
	expires_at: string;
	created_by: string;
	created_by_email: string;
	created_at: string;
	last_used_at: string | null;
	revoked_at: string | null;
}

export interface CreatedOrgToken extends OrgToken {
	/** Shown exactly once. */
	token: string;
}

export interface CreateUserTokenRequest {
	name: string;
	scope: KeyScope;
	/** Absent or null means never expires. Console default: 90 days. */
	expires_at?: string | null;
}

export interface CreateRobotRequest {
	name: string;
	description: string;
}

export interface CreateOrgTokenRequest {
	name: string;
	scope: KeyScope;
	/** REQUIRED, unlike every other expiry request in the API -- see OrgToken. */
	expires_at: string;
}
