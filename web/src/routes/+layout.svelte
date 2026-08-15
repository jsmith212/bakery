<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { navigating } from '$app/state';

	import { setUnauthorizedHook } from '$lib/api/client';
	import { clearSession } from '$lib/session';
	import { stashReturnPath } from '$lib/storage';

	let { children } = $props();

	// The MID-SESSION 401. The console layout's guard only fires on navigation;
	// a session that expires while a screen sits open is caught here instead, on
	// the request itself. Both layers are needed and they cover different moments.
	//
	// `redirecting` collapses a burst -- three parallel loads that all 401 are
	// one expiry, not three -- so the user gets one navigation and one stashed
	// return path rather than a race between three.
	//
	// The path stashed is the IN-FLIGHT NAVIGATION's target when there is one,
	// not `location`. `location` only updates once a client-side navigation
	// COMMITS -- so a 401 raised by the next page's own `load` (its data fetch,
	// not a stale screen's) fires while `location` still names the page being
	// LEFT. Stashing that returns the user to where they came from instead of
	// where they were headed, silently discarding the interrupted navigation.
	// `navigating.to` is SvelteKit's own record of that in-flight target and is
	// set for exactly the window this matters in.
	let redirecting = false;

	onMount(() => {
		setUnauthorizedHook(() => {
			if (redirecting || location.pathname === '/login') return;
			redirecting = true;

			const target = navigating.to?.url;
			stashReturnPath(target ? target.pathname + target.search : location.pathname + location.search);
			clearSession();
			void goto('/login', { replaceState: true }).finally(() => {
				redirecting = false;
			});
		});

		return () => setUnauthorizedHook(null);
	});
</script>

{@render children()}
