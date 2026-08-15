<script lang="ts" module>
	/**
	 * What a `Field` hands its child control through the `children` snippet
	 * parameter: the id to put on the control (so `Label`'s `for` actually
	 * resolves to something), plus the error-path ARIA pair. `aria-invalid`
	 * and `aria-describedby` are computed here rather than at each of the 29
	 * call sites because the failure mode of leaving that to callers is not
	 * "a missing attribute" -- it is 29 chances to get the describedby target
	 * wrong (hint id vs error id) with no test that would catch it.
	 */
	export interface FieldChildProps {
		id: string;
		'aria-invalid': true | undefined;
		'aria-describedby': string | undefined;
	}
</script>

<script lang="ts">
	import type { Snippet } from 'svelte';
	import Label from './Label.svelte';
	import Hint from './Hint.svelte';
	import ErrorText from './ErrorText.svelte';

	interface Props {
		label?: string;
		hint?: string;
		error?: string;
		for?: string;
		/**
		 * Receives `FieldChildProps` -- spread the id (and, on the error path,
		 * the aria pair) onto the field's own control:
		 * `{#snippet children(f)}<Input id={f.id} aria-invalid={f['aria-invalid']} aria-describedby={f['aria-describedby']} .../>{/snippet}`.
		 */
		children?: Snippet<[FieldChildProps]>;
	}

	let { label, hint, error, for: forId, children }: Props = $props();

	// Svelte 5's own per-instance id primitive -- consistent between server and
	// client, unlike `Modal`'s `Math.random()` (fine there; it labels nothing
	// external to itself). `for`/`forId` is an escape hatch for a caller that
	// already owns an id; nothing in this codebase uses it, but dropping it
	// would make `Field` unable to wrap a control it does not control the id
	// of, which is the one case a mint-only id can't cover.
	const uid = $props.id();
	const id = $derived(forId ?? uid);
	const hintId = `${uid}-hint`;
	const errorId = `${uid}-error`;
	const describedBy = $derived(error ? errorId : hint ? hintId : undefined);
</script>

<div class="flex flex-col gap-1">
	{#if label}<Label for={id}>{label}</Label>{/if}
	{@render children?.({ id, 'aria-invalid': error ? true : undefined, 'aria-describedby': describedBy })}
	{#if error}<ErrorText id={errorId}>{error}</ErrorText>{:else if hint}<Hint id={hintId}>{hint}</Hint>{/if}
</div>
