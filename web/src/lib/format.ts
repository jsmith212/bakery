/**
 * Presentation-only formatting: byte counts, retention windows, and
 * approximate timestamps.
 *
 * Pure functions with no Svelte/DOM imports, so they run under Vitest's
 * `node` environment untouched. `parseHumanBytes` is the one function that
 * goes the OTHER direction -- from a typed quota input back into the integer
 * byte count `CreateBackendRequest.quota_bytes` / `UpdateBackendRequest.quota_bytes`
 * want on the wire -- because unlike `retention_window`, there is no
 * server-side string parser for a quota: the wire field is a JSON number, so
 * the console must produce one.
 */

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB'] as const;

/** `null`/`undefined` render as an em dash: "no measurement", never "0 B". */
export function formatBytes(bytes: number | null | undefined): string {
	if (bytes === null || bytes === undefined) return '—';
	if (bytes === 0) return '0 B';

	let n = Math.abs(bytes);
	let unit = 0;
	while (n >= 1024 && unit < BYTE_UNITS.length - 1) {
		n /= 1024;
		unit++;
	}

	const sign = bytes < 0 ? '-' : '';
	const value = unit === 0 ? String(Math.round(n)) : trimTrailingZero(n.toFixed(1));

	return `${sign}${value} ${BYTE_UNITS[unit]}`;
}

function trimTrailingZero(s: string): string {
	return s.endsWith('.0') ? s.slice(0, -2) : s;
}

/** `null` is a real state -- "no cap" -- and must never fall through to `formatBytes(null)`'s "—". */
export function formatQuota(bytes: number | null | undefined): string {
	if (bytes === null || bytes === undefined) return 'no cap';

	return formatBytes(bytes);
}

/** Thousands-grouped, with a fixed locale so CI's `en-US` output cannot drift with the host. */
export function formatCount(n: number | null | undefined): string {
	if (n === null || n === undefined) return '—';

	return new Intl.NumberFormat('en-US').format(n);
}

const UNIT_MULTIPLIERS: Record<string, number> = {
	b: 1,
	kb: 1024,
	mb: 1024 ** 2,
	gb: 1024 ** 3,
	tb: 1024 ** 4,
	pb: 1024 ** 5
};

/**
 * Parses a human quota input ("500 GB", "1TB", "2097152") into an integer
 * byte count, binary units to match `formatBytes`. Throws a `TypeError`
 * naming the problem -- never coerces a bad input to `0` or `NaN`, both of
 * which the server's `quotaBytesPatch` would reject too, just later and with
 * a round trip in between.
 */
export function parseHumanBytes(input: string): number {
	const trimmed = input.trim();
	const match = /^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$/.exec(trimmed);

	if (!match) throw new TypeError(`"${input}" is not a size like "500 GB" or "1TB"`);

	const [, digits, rawUnit] = match;
	const lower = rawUnit.toLowerCase();
	const unit = lower === '' ? 'b' : lower.endsWith('b') ? lower : `${lower}b`;
	const multiplier = UNIT_MULTIPLIERS[unit];

	if (!multiplier) {
		throw new TypeError(`"${rawUnit}" is not a byte unit -- use B, KB, MB, GB, TB or PB`);
	}

	const bytes = Math.round(parseFloat(digits) * multiplier);

	if (!Number.isFinite(bytes) || bytes <= 0) {
		throw new TypeError(`"${input}" must be a positive size`);
	}

	return bytes;
}

/**
 * Renders a Go duration string (`"2160h0m0s"` -- the shape `time.Duration.String()`
 * produces and `time.ParseDuration` accepts back unchanged) as days/hours/years.
 * `null` is "retain forever", a real shipped state and not a missing value.
 * An unparseable string is returned verbatim rather than swallowed.
 */
export function formatRetentionWindow(window: string | null | undefined): string {
	if (window === null || window === undefined) return 'retain forever';

	const hours = totalHours(window);
	if (hours === null) return window;
	if (hours === 0) return '0 hours';
	if (hours % (24 * 365) === 0) return pluralize(hours / (24 * 365), 'year');
	if (hours % 24 === 0) return pluralize(hours / 24, 'day');

	return pluralize(hours, 'hour');
}

function totalHours(window: string): number | null {
	const match = /^(\d+)h(?:\d+m)?(?:\d+(?:\.\d+)?s)?$/.exec(window.trim());
	if (!match) return null;

	return parseInt(match[1], 10);
}

function pluralize(n: number, word: string): string {
	return `${n} ${word}${n === 1 ? '' : 's'}`;
}

/**
 * Parses a Go duration string (`time.Duration.String()`'s own format --
 * `"6h0m0s"`, `"30m0s"`, `"45s"`) into a whole number of seconds, or `null`
 * when the string does not match that grammar at all. Unlike
 * `totalHours` above, the hour component is OPTIONAL: `time.Duration.String()`
 * omits a unit entirely once it is the largest one present, so a sub-hour
 * value like 30 minutes renders as `"30m0s"`, with no leading `h` for
 * `totalHours`'s regex to anchor on.
 */
function totalSeconds(duration: string): number | null {
	const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(duration.trim());
	if (!match) return null;

	const [, h, m, s] = match;
	if (h === undefined && m === undefined && s === undefined) return null; // rejects ""

	const hours = h ? parseInt(h, 10) : 0;
	const minutes = m ? parseInt(m, 10) : 0;
	const seconds = s ? parseFloat(s) : 0;

	return hours * 3600 + minutes * 60 + Math.round(seconds);
}

/**
 * Renders a Go duration string as hours/minutes/seconds -- for an OPERATIONAL
 * interval (`--gc-interval`, `--gc-usage-interval`, `--gc-grace-period`), never
 * a retention window. Two differences from `formatRetentionWindow` above, both
 * deliberate:
 *
 *  - **Minutes and seconds are renderable.** A retention window is measured in
 *    days to years, so `formatRetentionWindow`'s parser only anchors on an `h`
 *    component and a sub-hour value falls through to its verbatim-string
 *    fallback. A GC interval is routinely sub-hour (`30m0s` for a fast dev
 *    loop, or `--gc-grace-period` tuned down for a test), and that must render
 *    as "30 minutes", not the raw Go string.
 *  - **There is NO "forever" branch.** `null`/`undefined` for a retention
 *    window is a real, shipped state -- "never expire". A GC interval has no
 *    such state (the three fields this renders are plain, always-present
 *    strings on `InstanceInfo`, never `null` on the wire) and reusing "retain
 *    forever" copy for a merely-missing value would be a lie. Missing input
 *    here renders the ordinary "no value" em dash instead.
 *
 * No day/year folding either, unlike `formatRetentionWindow`: these three
 * fields are tuning knobs in the minutes-to-day range, not multi-year spans,
 * so hour is the largest unit this renders.
 */
export function formatDuration(duration: string | null | undefined): string {
	if (!duration) return '—';

	const seconds = totalSeconds(duration);
	if (seconds === null) return duration;
	if (seconds === 0) return '0 seconds';
	if (seconds % 3600 === 0) return pluralize(seconds / 3600, 'hour');
	if (seconds % 60 === 0) return pluralize(seconds / 60, 'minute');

	return pluralize(seconds, 'second');
}

/**
 * `accessed_at` is written on a staleness ramp -- up to 24h coarse for the
 * first week after the M6 upgrade (CLAUDE.md) -- so it is rendered as an
 * approximate relative time, marked with `~`, never a precise clock reading.
 * `null` means never read since the upgrade, the ordinary day-one state.
 *
 * `now` is a parameter rather than an internal `Date.now()` read, so this is
 * deterministic under Vitest.
 */
export function formatApproxAccessed(
	iso: string | null | undefined,
	now: number = Date.now()
): string {
	if (!iso) return '—';

	const then = new Date(iso).getTime();
	if (Number.isNaN(then)) return '—';

	const diffMs = Math.max(0, now - then);
	const minutes = Math.round(diffMs / 60000);

	if (minutes < 1) return '~just now';
	if (minutes < 60) return `~${pluralize(minutes, 'minute')} ago`;

	const hours = Math.round(minutes / 60);
	if (hours < 24) return `~${pluralize(hours, 'hour')} ago`;

	const days = Math.round(hours / 24);

	return `~${pluralize(days, 'day')} ago`;
}

export type ExpiryKind = 'none' | 'soon' | 'expired';

export interface ExpiryInfo {
	kind: ExpiryKind;
	label: string;
}

const EXPIRY_SOON_MS = 30 * 24 * 60 * 60 * 1000;

/**
 * Classifies an API key's `expires_at` for the expiry column: `none` (never,
 * or comfortably far off), `soon` (within 30 days -- render as a warning so a
 * key does not silently go stale mid-build), `expired`. Unlike
 * `formatApproxAccessed`, `expires_at` is an exact stored timestamp, not a
 * staleness-rampped one, so this never carries the `~` qualifier.
 */
export function formatExpiry(
	iso: string | null | undefined,
	now: number = Date.now()
): ExpiryInfo {
	if (!iso) return { kind: 'none', label: 'no expiry' };

	const then = new Date(iso).getTime();
	if (Number.isNaN(then)) return { kind: 'none', label: 'no expiry' };

	const diff = then - now;
	if (diff <= 0) return { kind: 'expired', label: 'expired' };

	const days = Math.ceil(diff / (24 * 60 * 60 * 1000));
	const label = pluralize(days, 'day');

	return diff <= EXPIRY_SOON_MS ? { kind: 'soon', label: `in ${label}` } : { kind: 'none', label: `in ${label}` };
}

/** A deterministic `YYYY-MM-DD HH:MM UTC` rendering for `measured_at` / `created_at`. */
export function formatDateTimeUTC(iso: string | null | undefined): string {
	if (!iso) return '—';

	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return '—';

	const pad = (n: number) => String(n).padStart(2, '0');

	return (
		`${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ` +
		`${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`
	);
}
