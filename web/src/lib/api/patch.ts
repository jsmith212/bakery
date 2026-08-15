/**
 * The three-state update encoding.
 *
 * `internal/api` models a patchable nullable column as a `json.RawMessage`
 * (`UpdateOrgRequest.DefaultRetentionWindow`, `UpdateBackendRequest.QuotaBytes`,
 * ...) precisely because such a field has THREE meanings and a pointer can only
 * carry two:
 *
 *   | wire            | meaning                                   |
 *   |-----------------|-------------------------------------------|
 *   | key absent      | leave the column exactly as it is         |
 *   | `null`          | CLEAR it (retain forever / no cap)        |
 *   | a value         | set it                                    |
 *
 * On this side that is `undefined` / `null` / value -- and `JSON.stringify`
 * drops an `undefined` property, so "absent" is free as long as nothing
 * substitutes a placeholder for it.
 *
 * **`''` is a fourth state and it does not exist.** A form control bound
 * straight to one of these fields produces `""` when the user clears it, which
 * is neither "absent" nor `null`; the server's decoder rejects it and the user
 * sees a 400 for having emptied a box. `tri()` refuses it here, at the boundary,
 * where the message can say what to do instead.
 *
 * The server decodes with `DisallowUnknownFields`, so an extra key is a hard 400
 * as well -- which is why request bodies are built from these helpers and typed
 * request interfaces rather than spread from component state.
 */

/** A patchable field: omit, clear, or set. */
export type TriState<T> = T | null | undefined;

export const TRI_STATE_EMPTY_STRING =
	'an empty string is not a valid patch value: omit the field to leave it unchanged, ' +
	'or send null to clear it';

/**
 * Validates one three-state field on its way into a request body.
 *
 * Pass the raw value (including whatever a form control produced) and the field
 * name; get back the value the wire accepts, or a thrown `TypeError` naming the
 * field. It is deliberately a throw and not a silent coercion: guessing between
 * "leave alone" and "clear" is the one thing a caller must not delegate.
 */
export function tri<T>(value: TriState<T> | '', field: string): TriState<T> {
	if (value === '') throw new TypeError(`${field}: ${TRI_STATE_EMPTY_STRING}`);

	return value as TriState<T>;
}

/**
 * Drops `undefined` properties, keeping `null`.
 *
 * `JSON.stringify` already does this on the way out; this exists so a request
 * fixture test can compare the object it built against the object the fixture
 * records, without an `undefined` key making two identical bodies unequal.
 */
export function omitUndefined<T extends Record<string, unknown>>(body: T): Partial<T> {
	const out: Record<string, unknown> = {};

	for (const [key, value] of Object.entries(body)) {
		if (value === undefined) continue;
		out[key] = value;
	}

	return out as Partial<T>;
}
