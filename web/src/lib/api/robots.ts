import { del, get, post, seg, type RequestOptions } from './client';
import type {
	CreatedOrgToken,
	CreateOrgTokenRequest,
	CreateRobotRequest,
	ListResponse,
	Robot
} from './types';

/**
 * Robots (`bkro_`): `/orgs/{org}/robots`.
 *
 * Every route here is `AccessOrgAdmin`, admitted only to an interactive human
 * -- a robot token managing robots would be a self-renewing, org-wide,
 * write-everywhere credential with no human in the loop. `GET` returns each
 * robot with its tokens (live AND revoked) already attached (metadata only),
 * so there is no second per-robot fetch on this screen.
 */

export function listRobots(org: string, opts?: RequestOptions): Promise<ListResponse<Robot>> {
	return get<ListResponse<Robot>>(`/orgs/${seg(org)}/robots`, opts);
}

export function createRobot(
	org: string,
	body: CreateRobotRequest,
	opts?: RequestOptions
): Promise<Robot> {
	return post<Robot>(`/orgs/${seg(org)}/robots`, body, opts);
}

/** 204. Cascades every token the robot holds. */
export function deleteRobot(org: string, robot: string, opts?: RequestOptions): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/robots/${seg(robot)}`, opts);
}

/**
 * 201 with the plaintext token, exactly once. `expires_at` is REQUIRED here,
 * unlike a personal token or an API key -- a robot deliberately outlives its
 * creator, so expiry is the countervailing control.
 */
export function createOrgToken(
	org: string,
	robot: string,
	body: CreateOrgTokenRequest,
	opts?: RequestOptions
): Promise<CreatedOrgToken> {
	return post<CreatedOrgToken>(`/orgs/${seg(org)}/robots/${seg(robot)}/tokens`, body, opts);
}

/** 204, idempotent. */
export function revokeOrgToken(
	org: string,
	robot: string,
	token: string,
	opts?: RequestOptions
): Promise<void> {
	return del<void>(`/orgs/${seg(org)}/robots/${seg(robot)}/tokens/${seg(token)}`, opts);
}
