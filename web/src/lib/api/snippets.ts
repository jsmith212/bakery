import { post, seg, type RequestOptions } from './client';
import type { KeyScope, SnippetRequest, SnippetResponse, SnippetTool } from './types';

/**
 * `POST .../{project}/snippet`.
 *
 * Two calls, one route, and conflating them is a key faucet: every non-preview
 * POST mints a live project-scoped credential, and the default key name carries
 * entropy specifically so repeated calls do not collide. A screen that fetched
 * one snippet per tool tile on mount would mint nine.
 */

/**
 * Renders the config with a `«create an API key»` placeholder and **mints
 * nothing**. 200. `api_key` is absent and `preview` is true.
 *
 * This is what a tile selection calls. It is also the only safe thing to call
 * from a `load`.
 */
export function previewSnippet(
	org: string,
	project: string,
	tool: SnippetTool,
	opts?: RequestOptions
): Promise<SnippetResponse> {
	const body: SnippetRequest = { tool, preview: true };

	return post<SnippetResponse>(
		`/orgs/${seg(org)}/projects/${seg(project)}/snippet`,
		body,
		opts
	);
}

/**
 * Mints exactly one key and returns the config with the plaintext token in it.
 * 201. **Only ever from an explicit user gesture**, never from a `load`, never
 * on mount, and never re-run by an `invalidate()`.
 *
 * `scope` omitted defaults to the CALLER'S OWN CEILING (write if they may write
 * this project, read otherwise), so a project reader is not refused for
 * accepting the default. An explicit scope above their role is still a
 * `403 scope_exceeds_role` -- never a quiet downgrade.
 *
 * `preview` is deliberately not settable here: the mint path is a request
 * WITHOUT the field, and sending `preview: false` is the same request with one
 * more thing to get wrong.
 */
export function generateSnippet(
	org: string,
	project: string,
	body: { tool: SnippetTool; scope?: KeyScope; key_name?: string },
	opts?: RequestOptions
): Promise<SnippetResponse> {
	const request: SnippetRequest = { tool: body.tool };
	if (body.scope !== undefined) request.scope = body.scope;
	if (body.key_name !== undefined) request.key_name = body.key_name;

	return post<SnippetResponse>(
		`/orgs/${seg(org)}/projects/${seg(project)}/snippet`,
		request,
		opts
	);
}
