import { describe, expect, it } from 'vitest';

import { appName } from './app';

describe('appName', () => {
	const cases: { name: string; input: string | undefined; want: string }[] = [
		{ name: 'default', input: undefined, want: 'Bakery' },
		{ name: 'override', input: 'Bakery (dev)', want: 'Bakery (dev)' },
		{ name: 'empty falls back to default', input: '', want: 'Bakery' }
	];

	for (const c of cases) {
		it(c.name, () => {
			expect(appName(c.input)).toBe(c.want);
		});
	}
});
