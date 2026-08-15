/**
 * The error vocabulary, mirrored from `internal/api/errors.go`.
 *
 * The Go side calls this vocabulary CLOSED and STABLE for exactly one reason:
 * so a client can branch on `error.code` and never on `error.message` or on the
 * HTTP status. Two of the codes make that concrete:
 *
 *   - `scope_exceeds_role` is a **403**, not a 422, and it carries
 *     `field: "scope"`. A client that keyed its table on status would file it
 *     under the generic 403 arm and render "you are not allowed here" on the
 *     one screen whose whole job is to explain what to do about it.
 *   - `claim_derived_role` is a **409** and is the ORDINARY outcome of removing
 *     a member on a pure-LDAP deployment. It is not a red toast; it is an
 *     explanation, rendered in place, using the server's own message.
 *
 * `not_implemented` is in the Go constants and therefore in this union, even
 * though no handler returns it today: the union has to be exhaustive over the
 * server's vocabulary, not over the subset currently reachable.
 */

/** Every code `internal/api/errors.go` can put in an envelope. */
export type ApiErrorCode =
	| 'bad_request'
	| 'validation_failed'
	| 'reserved_slug'
	| 'invalid_slug'
	| 'unauthorized'
	| 'forbidden'
	| 'not_found'
	| 'conflict'
	| 'claim_derived_role'
	| 'unsupported_media_type'
	| 'scope_exceeds_role'
	| 'internal_error'
	| 'not_implemented';

/**
 * The same set as a runtime value, in `errors.go`'s declaration order. The
 * exhaustiveness test iterates this; if the Go constants grow a member and this
 * array does not, `treatmentFor`'s `never` check fails to compile the moment the
 * union is updated, and the count assertion fails if only the array is updated.
 */
export const API_ERROR_CODES: readonly ApiErrorCode[] = [
	'bad_request',
	'validation_failed',
	'reserved_slug',
	'invalid_slug',
	'unauthorized',
	'forbidden',
	'not_found',
	'conflict',
	'claim_derived_role',
	'unsupported_media_type',
	'scope_exceeds_role',
	'internal_error',
	'not_implemented'
] as const;

export function isApiErrorCode(value: unknown): value is ApiErrorCode {
	return typeof value === 'string' && (API_ERROR_CODES as readonly string[]).includes(value);
}

/**
 * How a screen should present one error.
 *
 * - `session`   the central `onUnauthorized` hook already ran; never a toast.
 * - `field`     attach `error.field` to the owning input.
 * - `scope`     the mint-path teaching copy plus the scope selector.
 * - `in_place`  render the server's own message where the action was, not a toast.
 * - `context`   EmptyState when it came from a load, toast when from a mutation.
 * - `toast`     a toast carrying the server's message.
 */
export type ErrorTreatment = 'session' | 'field' | 'scope' | 'in_place' | 'context' | 'toast';

/**
 * The dispatch table, keyed on CODE. Every arm is deliberate; the `never`
 * assignment in the default arm is what makes a new Go code a compile error here
 * rather than a silently generic toast.
 */
export function treatmentFor(code: ApiErrorCode): ErrorTreatment {
	switch (code) {
		case 'unauthorized':
			return 'session';
		case 'validation_failed':
		case 'reserved_slug':
		case 'invalid_slug':
			return 'field';
		case 'scope_exceeds_role':
			return 'scope';
		case 'forbidden':
		case 'claim_derived_role':
			return 'in_place';
		case 'not_found':
			return 'context';
		case 'bad_request':
		case 'conflict':
		case 'unsupported_media_type':
		case 'internal_error':
		case 'not_implemented':
			return 'toast';
		default: {
			const exhaustive: never = code;
			return exhaustive;
		}
	}
}

/**
 * Copy to render when the server sent no usable message (a proxy's HTML 502, a
 * truncated body). It is never preferred over `error.message` -- the server's
 * prose is written for the person reading it and this is only a floor.
 */
export function fallbackMessage(code: ApiErrorCode): string {
	switch (code) {
		case 'bad_request':
			return 'The server could not read that request.';
		case 'validation_failed':
			return 'That value is not valid.';
		case 'reserved_slug':
			return 'That slug is reserved by the cache URL grammar.';
		case 'invalid_slug':
			return 'That slug is not a valid slug.';
		case 'unauthorized':
			return 'Your session has expired.';
		case 'forbidden':
			return 'You are signed in, but not allowed to do that.';
		case 'not_found':
			return 'That does not exist, or you cannot see it.';
		case 'conflict':
			return 'That conflicts with the current state.';
		case 'claim_derived_role':
			return 'That role comes from an identity-provider group and cannot be changed here.';
		case 'unsupported_media_type':
			return 'The server refused that request body.';
		case 'scope_exceeds_role':
			return 'That scope is more than your role on this project allows.';
		case 'internal_error':
			return 'The server failed to handle that request.';
		case 'not_implemented':
			return 'This server does not implement that yet.';
		default: {
			const exhaustive: never = code;
			return exhaustive;
		}
	}
}

/**
 * One decoded `{"error":{...}}` envelope.
 *
 * `status` is carried for logging and for the load-vs-mutation distinction, and
 * for nothing else: no screen may branch on it.
 */
export class ApiError extends Error {
	readonly status: number;
	readonly code: ApiErrorCode;
	readonly field?: string;

	constructor(status: number, code: ApiErrorCode, message: string, field?: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.code = code;
		this.field = field;
	}

	get treatment(): ErrorTreatment {
		return treatmentFor(this.code);
	}
}

export function isApiError(value: unknown): value is ApiError {
	return value instanceof ApiError;
}

/**
 * The floor code for a response whose body carried no usable envelope -- a
 * gateway's HTML, a truncated stream, a 502 from something that is not Bakery.
 * Mapping by status here is not a violation of the branch-on-code rule: it is
 * how a NON-envelope is given a code in the first place, so that every consumer
 * downstream still only ever sees a code.
 */
export function codeForStatus(status: number): ApiErrorCode {
	switch (status) {
		case 400:
			return 'bad_request';
		case 401:
			return 'unauthorized';
		case 403:
			return 'forbidden';
		case 404:
			return 'not_found';
		case 409:
			return 'conflict';
		case 415:
			return 'unsupported_media_type';
		case 422:
			return 'validation_failed';
		case 501:
			return 'not_implemented';
		default:
			return 'internal_error';
	}
}
