/**
 * `/login?denied=<reason>` -- the terminal state every browser-facing failure of
 * the OIDC round trip lands on (B8, R8#2).
 *
 * `HandleLogin` and `HandleCallback` (`internal/auth/service.go`) redirect here
 * instead of writing the raw JSON error envelope at `/api/v1/auth/*`: a user
 * mid-flow has no console chrome to read a `{"error":{...}}` page with, nothing
 * to click, and nothing to do but close the tab. `deniedLoginGate`,
 * `deniedIdPRefused`, `deniedStaleRequest` and `deniedAuthFailed` in that file
 * are the closed set this module mirrors -- a value outside it (or none at all)
 * is simply not a denial, never a fourth "unknown" card.
 */

export const DENIED_REASONS = ['login_gate', 'idp_refused', 'stale_request', 'auth_failed'] as const;

export type DeniedReason = (typeof DENIED_REASONS)[number];

export interface DeniedCopy {
	title: string;
	body: string;
}

/**
 * Per-reason copy for the login screen's denial card.
 *
 * `login_gate` has no retry affordance ON PURPOSE: the account authenticated
 * successfully and Bakery refused it (absent from every `login_groups` entry, or
 * an unreadable groups claim -- an Azure AD `_claim_names` overage -- which is a
 * different fault with the same safe answer), and signing in again produces the
 * identical refusal. `stale_request` is the opposite -- an old or
 * double-submitted callback link -- so ITS copy is the one that suggests trying
 * again. `idp_refused` and `auth_failed` describe what happened without
 * prescribing an action; the "Continue with SSO" control is always still on the
 * page if the reader chooses to use it.
 */
export const DENIED_COPY: Record<DeniedReason, DeniedCopy> = {
	login_gate: {
		title: 'Not authorized',
		body: 'You signed in successfully, but this account is not permitted to use Bakery. Ask an administrator to add you to a group that grants access.'
	},
	idp_refused: {
		title: 'Sign-in declined',
		body: 'Your identity provider declined this sign-in. Contact your administrator if you believe this is wrong.'
	},
	stale_request: {
		title: 'Sign-in link expired',
		body: 'This sign-in link is stale, or was already used. Sign in again below.'
	},
	auth_failed: {
		title: 'Sign-in failed',
		body: 'Something went wrong completing sign-in. Contact an administrator if this keeps happening.'
	}
};

export function isDeniedReason(v: string | null): v is DeniedReason {
	return v !== null && (DENIED_REASONS as readonly string[]).includes(v);
}

/**
 * Resolves a raw `?denied=` query value to its copy, or `null` for anything
 * outside the closed set -- including absent, empty, or a value an older client
 * might send that this build no longer recognizes.
 */
export function deniedCopy(v: string | null): DeniedCopy | null {
	return isDeniedReason(v) ? DENIED_COPY[v] : null;
}
