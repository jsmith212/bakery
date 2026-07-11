const defaultAppName = 'Bakery';

export function appName(override?: string): string {
	return override?.trim() ? override : defaultAppName;
}
