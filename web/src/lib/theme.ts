import { writable } from 'svelte/store';

export type Theme = 'dark' | 'light' | 'system';
export type ResolvedTheme = 'dark' | 'light';

export const THEME_STORAGE_KEY = 'bakery-theme';

function isTheme(value: unknown): value is Theme {
	return value === 'dark' || value === 'light' || value === 'system';
}

function readStored(): Theme {
	if (typeof localStorage === 'undefined') return 'system';
	const value = localStorage.getItem(THEME_STORAGE_KEY);
	return isTheme(value) ? value : 'system';
}

export function resolveTheme(theme: Theme): ResolvedTheme {
	if (theme !== 'system') return theme;
	if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return 'dark';
	return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function applyTheme(theme: Theme): void {
	if (typeof document === 'undefined') return;
	document.documentElement.dataset.theme = resolveTheme(theme);
}

export const theme = writable<Theme>(readStored());

let current: Theme = 'system';

theme.subscribe((value) => {
	current = value;
	if (typeof localStorage !== 'undefined') localStorage.setItem(THEME_STORAGE_KEY, value);
	applyTheme(value);
});

export function setTheme(value: Theme): void {
	theme.set(value);
}

if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
	window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
		if (current === 'system') applyTheme('system');
	});
}
