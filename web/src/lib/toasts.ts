import { writable } from 'svelte/store';

import { isApiError } from '$lib/api/errors';

/**
 * Transient feedback.
 *
 * `Toast` was imported by zero routes before this wave, which meant no mutation
 * in the console had any success or failure feedback at all. One host component
 * mounted once in the console layout, one store, and free functions -- the
 * `theme.ts` idiom again.
 *
 * A toast is for something that happened elsewhere on the screen or is already
 * over. It is NOT the treatment for a field error (which attaches to the input),
 * a 401 (which is a redirect), or a 403/409-`claim_derived_role` (which is an
 * in-place explanation). See `$lib/api/errors`' dispatch table.
 */

export type ToastVariant = 'success' | 'error' | 'warning' | 'info';

export interface Toast {
	id: number;
	variant: ToastVariant;
	title: string;
	detail?: string;
}

export interface ToastInput {
	variant?: ToastVariant;
	title: string;
	detail?: string;
	/** Milliseconds before auto-dismiss. 0 keeps it until dismissed by hand. */
	ttl?: number;
}

export const TOAST_TTL_MS = 6000;

export const toasts = writable<Toast[]>([]);

let nextId = 1;

export function pushToast(input: ToastInput): number {
	const id = nextId++;
	const toast: Toast = {
		id,
		variant: input.variant ?? 'info',
		title: input.title,
		detail: input.detail
	};

	toasts.update((list) => [...list, toast]);

	const ttl = input.ttl ?? TOAST_TTL_MS;
	if (ttl > 0 && typeof setTimeout === 'function') {
		setTimeout(() => dismissToast(id), ttl);
	}

	return id;
}

export function dismissToast(id: number): void {
	toasts.update((list) => list.filter((t) => t.id !== id));
}

export function clearToasts(): void {
	toasts.set([]);
}

/**
 * Toasts an error whose treatment is `toast`.
 *
 * The SERVER's message is shown verbatim, because for the three codes that reach
 * here it is the whole value of the response: "a gc sweep is already running",
 * "gc is disabled on this server", and "this backend still holds objects" are
 * three different next actions, and a generic "conflict" is none of them.
 *
 * Anything that is not an `ApiError` -- a network failure, a bug -- gets generic
 * copy and never a raw stack or status line.
 */
export function toastError(err: unknown, fallbackTitle = 'Something went wrong'): void {
	if (isApiError(err)) {
		pushToast({ variant: 'error', title: fallbackTitle, detail: err.message });

		return;
	}

	pushToast({
		variant: 'error',
		title: fallbackTitle,
		detail: 'The request did not reach the server, or the server did not answer.'
	});
}
