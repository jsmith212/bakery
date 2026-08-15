/**
 * The typed `/api/v1` client.
 *
 * One module per section of `internal/api/api.go`'s own route table, so a new
 * endpoint has exactly one obvious home. Hand-written, not generated: there is
 * no OpenAPI document in this repo, and generating one would mean ADDING a
 * schema-authoring step to re-derive about twenty small, stable, same-repo Go
 * structs. The drift risk is real and is answered by the fixture contract tests
 * under `testdata/`, not by a pipeline.
 */

export * from './types';
export * from './errors';
export * from './patch';
export {
	API_PREFIX,
	del,
	get,
	patch,
	post,
	put,
	request,
	resetUnauthorizedHook,
	seg,
	setUnauthorizedHook,
	type FetchLike,
	type RequestOptions
} from './client';

export * as auth from './auth';
export * as backends from './backends';
export * as gc from './gc';
export * as instance from './instance';
export * as keys from './keys';
export * as members from './members';
export * as objects from './objects';
export * as orgs from './orgs';
export * as projects from './projects';
export * as siteAdmins from './siteAdmins';
export * as snippets from './snippets';

export { getMe } from './me';
