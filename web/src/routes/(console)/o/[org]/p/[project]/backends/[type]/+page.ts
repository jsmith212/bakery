import { error } from '@sveltejs/kit';

import { getBackend } from '$lib/api/backends';
import { getProjectUsage } from '$lib/api/projects';
import { isApiError } from '$lib/api/errors';
import type { BackendKind } from '$lib/api/types';

import type { PageLoad } from './$types';

const KNOWN_KINDS: readonly BackendKind[] = ['sstate', 'downloads', 'hashserv', 'bazel', 'oci'];

function isBackendKind(value: string): value is BackendKind {
	return (KNOWN_KINDS as readonly string[]).includes(value);
}

/**
 * `GET .../backends/{kind}` + B2b usage.
 *
 * `[type]` is an open path segment -- unlike `{org}`/`{project}`, which the
 * server itself turns into a 404 for an unreadable slug, an unknown kind here
 * never reaches the network at all. `backends/new` is registered as a
 * STATIC sibling and wins over this dynamic segment (SvelteKit's own
 * resolution order, mirrored by the server's own `/cache/{org}/{project}/{ac,cas}`
 * literal-over-dynamic routing), so "new" never reaches here either.
 *
 * A kind the project has not configured is a 404 from `getBackend` itself --
 * CLAUDE.md's invariant that an unconfigured backend never 500s and never
 * silently falls back to a default kind (the mock's bug: `backends[type] ??
 * backends.sstate`).
 */
export const load: PageLoad = async (event) => {
	const { org, project, type } = event.params;

	if (!isBackendKind(type)) {
		error(404, `"${type}" is not a backend kind`);
	}

	try {
		const [backend, usage] = await Promise.all([
			getBackend(org, project, type, { fetch: event.fetch }),
			getProjectUsage(org, project, { fetch: event.fetch })
		]);

		return { kind: type, backend, usage: usage.items };
	} catch (err) {
		if (isApiError(err)) error(err.status, err.message);

		throw err;
	}
};
