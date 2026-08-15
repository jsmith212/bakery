import { get, type RequestOptions } from './client';
import type { InstanceInfo } from './types';

/**
 * `GET /instance`. Site admin.
 *
 * A boot-time config echo, resolved once in `server.Boot` and served back
 * verbatim. **It never touches Prometheus**: `/metrics` is `--metrics-addr`-only
 * by hard invariant, and this endpoint reports what the process was booted with,
 * not what it is currently counting.
 */
export function getInstance(opts?: RequestOptions): Promise<InstanceInfo> {
	return get<InstanceInfo>('/instance', opts);
}
