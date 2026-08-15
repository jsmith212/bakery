import { describe, expect, it } from 'vitest';

import {
	formatApproxAccessed,
	formatBytes,
	formatCount,
	formatDateTimeUTC,
	formatDuration,
	formatExpiry,
	formatQuota,
	formatRetentionWindow,
	parseHumanBytes
} from './format';

describe('formatBytes', () => {
	const cases: [number | null | undefined, string][] = [
		[null, '—'],
		[undefined, '—'],
		[0, '0 B'],
		[512, '512 B'],
		[228076715213, '212.4 GB'],
		[1073741824, '1 GB'],
		[549755813888, '512 GB'],
		[5 * 1024 ** 4, '5 TB']
	];

	for (const [input, expected] of cases) {
		it(`formats ${String(input)} as ${expected}`, () => {
			expect(formatBytes(input)).toBe(expected);
		});
	}
});

describe('formatBytes / parseHumanBytes round-trip', () => {
	// `formatBytes` rounds its displayed unit to one decimal place, so it is
	// NOT an inverse of `parseHumanBytes` for a value that isn't a round
	// number of the chosen unit. Callers that pre-fill an editable quota
	// field from a stored byte count (org defaults, per-backend quota) must
	// not blindly re-parse an UNCHANGED field on submit, or every save silently
	// drifts the stored value toward whatever the display string rounded to.
	it('parseHumanBytes(formatBytes(n)) is not always n', () => {
		const n = 228076715213;
		expect(formatBytes(n)).toBe('212.4 GB');
		expect(parseHumanBytes(formatBytes(n))).not.toBe(n);
	});
});

describe('formatQuota', () => {
	it('renders null as "no cap", never the formatBytes em dash', () => {
		expect(formatQuota(null)).toBe('no cap');
		expect(formatQuota(undefined)).toBe('no cap');
	});

	it('delegates a real value to formatBytes', () => {
		expect(formatQuota(1073741824)).toBe('1 GB');
	});
});

describe('formatCount', () => {
	it('groups thousands with a fixed locale', () => {
		expect(formatCount(184201)).toBe('184,201');
	});

	it('renders a missing count as an em dash', () => {
		expect(formatCount(null)).toBe('—');
		expect(formatCount(undefined)).toBe('—');
	});

	it('renders an honest zero as "0", not "—"', () => {
		expect(formatCount(0)).toBe('0');
	});
});

describe('parseHumanBytes', () => {
	it('parses a binary unit with a space', () => {
		expect(parseHumanBytes('500 GB')).toBe(500 * 1024 ** 3);
	});

	it('parses a unit with no space and mixed case', () => {
		expect(parseHumanBytes('1TiB'.replace('i', ''))).toBe(1024 ** 4);
		expect(parseHumanBytes('1Tb')).toBe(1024 ** 4);
	});

	it('parses a bare number as bytes', () => {
		expect(parseHumanBytes('2097152')).toBe(2097152);
	});

	it('parses fractional values', () => {
		expect(parseHumanBytes('1.5 GB')).toBe(Math.round(1.5 * 1024 ** 3));
	});

	it('throws on an unknown unit', () => {
		expect(() => parseHumanBytes('500 XB')).toThrow(TypeError);
	});

	it('throws on a non-numeric input', () => {
		expect(() => parseHumanBytes('lots')).toThrow(TypeError);
	});

	it('throws on zero or negative', () => {
		expect(() => parseHumanBytes('0 GB')).toThrow(TypeError);
		expect(() => parseHumanBytes('-5 GB')).toThrow(TypeError);
	});

	it('throws on an empty string -- the tri() "no fourth state" rule applies here too', () => {
		expect(() => parseHumanBytes('')).toThrow(TypeError);
	});
});

describe('formatRetentionWindow', () => {
	it('renders null as "retain forever", a real state', () => {
		expect(formatRetentionWindow(null)).toBe('retain forever');
		expect(formatRetentionWindow(undefined)).toBe('retain forever');
	});

	it('renders a whole number of days', () => {
		expect(formatRetentionWindow('2160h0m0s')).toBe('90 days');
		expect(formatRetentionWindow('24h0m0s')).toBe('1 day');
	});

	it('renders a whole number of years', () => {
		expect(formatRetentionWindow(`${24 * 365}h0m0s`)).toBe('1 year');
	});

	it('falls back to hours when not divisible by 24', () => {
		expect(formatRetentionWindow('25h0m0s')).toBe('25 hours');
		expect(formatRetentionWindow('1h0m0s')).toBe('1 hour');
	});

	it('returns an unparseable string verbatim rather than swallowing it', () => {
		expect(formatRetentionWindow('garbage')).toBe('garbage');
	});
});

describe('formatDuration', () => {
	it('renders missing input as the ordinary em dash, NOT "retain forever"', () => {
		expect(formatDuration(null)).toBe('—');
		expect(formatDuration(undefined)).toBe('—');
		expect(formatDuration('')).toBe('—');
	});

	// The named regression case: a sub-hour GC interval renders with no leading
	// `h` component at all (Go's time.Duration.String() omits a unit entirely
	// once it's the largest one present), which formatRetentionWindow's
	// hour-anchored parser cannot read -- it would fall back to the raw string.
	it('renders a whole number of minutes with no hour component ("30m0s")', () => {
		expect(formatDuration('30m0s')).toBe('30 minutes');
	});

	it('renders a single minute without pluralizing ("1m0s")', () => {
		expect(formatDuration('1m0s')).toBe('1 minute');
	});

	it('renders seconds alone, with no minute or hour component', () => {
		expect(formatDuration('45s')).toBe('45 seconds');
		expect(formatDuration('1s')).toBe('1 second');
	});

	it('renders a whole number of hours', () => {
		expect(formatDuration('6h0m0s')).toBe('6 hours');
		expect(formatDuration('24h0m0s')).toBe('24 hours');
		expect(formatDuration('1h0m0s')).toBe('1 hour');
	});

	it('does NOT fold to days -- an operational interval is not a retention window', () => {
		expect(formatDuration('24h0m0s')).toBe('24 hours');
		expect(formatDuration('48h0m0s')).toBe('48 hours');
	});

	it('falls back to minutes when hours do not divide evenly', () => {
		expect(formatDuration('1h30m0s')).toBe('90 minutes');
	});

	it('falls back to seconds when minutes do not divide evenly', () => {
		expect(formatDuration('1m30s')).toBe('90 seconds');
	});

	it('renders the zero duration explicitly', () => {
		expect(formatDuration('0s')).toBe('0 seconds');
	});

	it('returns an unparseable string verbatim rather than swallowing it', () => {
		expect(formatDuration('garbage')).toBe('garbage');
	});
});

describe('formatApproxAccessed', () => {
	const now = new Date('2026-08-15T12:00:00.000Z').getTime();

	it('renders null as an em dash', () => {
		expect(formatApproxAccessed(null, now)).toBe('—');
		expect(formatApproxAccessed(undefined, now)).toBe('—');
	});

	it('renders sub-minute as "just now"', () => {
		expect(formatApproxAccessed('2026-08-15T11:59:45.000Z', now)).toBe('~just now');
	});

	it('renders minutes, marked approximate', () => {
		expect(formatApproxAccessed('2026-08-15T11:55:00.000Z', now)).toBe('~5 minutes ago');
	});

	it('renders hours', () => {
		expect(formatApproxAccessed('2026-08-15T09:00:00.000Z', now)).toBe('~3 hours ago');
	});

	it('renders days', () => {
		expect(formatApproxAccessed('2026-08-13T12:00:00.000Z', now)).toBe('~2 days ago');
	});

	it('renders an unparseable timestamp as an em dash rather than "NaN days ago"', () => {
		expect(formatApproxAccessed('not-a-date', now)).toBe('—');
	});
});

describe('formatExpiry', () => {
	const now = new Date('2026-08-15T12:00:00.000Z').getTime();

	it('renders a missing expiry as "no expiry", kind none', () => {
		expect(formatExpiry(null, now)).toEqual({ kind: 'none', label: 'no expiry' });
		expect(formatExpiry(undefined, now)).toEqual({ kind: 'none', label: 'no expiry' });
	});

	it('renders a past timestamp as expired', () => {
		expect(formatExpiry('2026-08-01T00:00:00.000Z', now)).toEqual({
			kind: 'expired',
			label: 'expired'
		});
	});

	it('renders within 30 days as soon', () => {
		const soon = new Date(now + 10 * 24 * 60 * 60 * 1000).toISOString();
		expect(formatExpiry(soon, now)).toEqual({ kind: 'soon', label: 'in 10 days' });
	});

	it('renders beyond 30 days as none, but still labelled', () => {
		const far = new Date(now + 90 * 24 * 60 * 60 * 1000).toISOString();
		expect(formatExpiry(far, now)).toEqual({ kind: 'none', label: 'in 90 days' });
	});
});

describe('formatDateTimeUTC', () => {
	it('renders a fixed UTC format regardless of host locale', () => {
		expect(formatDateTimeUTC('2026-08-15T06:00:00.000Z')).toBe('2026-08-15 06:00 UTC');
	});

	it('renders null/undefined/garbage as an em dash', () => {
		expect(formatDateTimeUTC(null)).toBe('—');
		expect(formatDateTimeUTC(undefined)).toBe('—');
		expect(formatDateTimeUTC('not-a-date')).toBe('—');
	});
});
