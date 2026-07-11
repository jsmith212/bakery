# Claude Design brief

Two prompts for [claude.ai/design](https://claude.ai/design). Run **A** first (the design system), then **B** (the screens) — B assumes A's tokens exist.

Claude Design outputs **live HTML**, not a picture. That's the point: the tokens and component markup are extractable, so the handoff to SvelteKit is a port, not a re-draw.

## Workflow

1. **A → design system.** Nothing to sync from the repo yet (no frontend exists), so the system is born here.
2. **B → screens.** Iterate in the canvas: chat for broad changes, inline comments for targeted ones, direct drag/resize for layout.
3. **Export → "Handoff to Claude Code."** Materialize as `web/tailwind.config.js` tokens + Svelte components in `web/src/lib/components/`.
4. **Then `/design-sync`** to push the real component library back up, so future screens are designed against components that exist. From here the repo is the source of truth.

Known preview limitations: large repos lag, multi-person editing is unreliable, inline comments sometimes don't render.

---

## Prompt A — the design system

> I'm building **Bakery**, a self-hosted build cache server that engineering orgs deploy internally. Its web UI is an admin and observability console — not a marketing site, not a consumer app. The users are platform/build engineers: technical, impatient, living in a terminal all day, and looking at this screen to answer "is the cache working, and why is my build slow."
>
> Build me a **design system** for it.
>
> **Aesthetic:** Linear is the north star. Also in the neighborhood: Vercel's dashboard, Planetscale, Grafana Cloud's newer UI. Dense, quiet, precise. Flat — no drop shadows, no gradients, no rounded-pill buttons, no illustration, no marketing gloss. Restraint over personality. Information density over whitespace. It should feel like a well-made instrument, not a product tour.
>
> **Dark mode is the primary theme**, light mode is a first-class secondary. Design dark first.
>
> **Color:** a near-neutral base (true grays or very slightly cool), one restrained accent for interactive elements, and a semantic set that must survive being used as *data* colors in charts and status badges, not just as UI chrome: success/hit, warning/stale, error/miss, neutral/idle. Cache hit-rate is the single most-looked-at number in this product, so hit-green and miss-red need to be unambiguous at a glance, distinguishable for the ~8% of my users with red-green color vision deficiency, and legible at 11px in a table cell.
>
> **Typography:** one UI sans, one true monospace. Monospace is not decorative here — it carries hashes, digests, object keys, config snippets, and log lines, and it appears constantly. Pick a mono with unambiguous `0/O`, `1/l/I`, and design a tabular-numerals treatment for metrics so columns of numbers don't jitter.
>
> **Deliver:**
> - Color tokens (dark + light), semantic naming, with contrast ratios stated
> - Type scale, including the mono scale and tabular figures
> - Spacing/radius/border scale — tight, this is a dense UI
> - Components: button (primary/secondary/ghost/danger, 3 sizes), text input, select, toggle, checkbox, badge/pill (for status + backend type), table (dense, sortable, with a monospace cell variant), tabs, side nav, modal, toast, empty state, skeleton/loading, code block with a copy button, key-value spec list, stat tile (big number + delta + sparkline), and a time-series chart treatment
> - Focus, hover, active, disabled, and error states for every interactive component
> - The chart/data-viz palette, explicitly, as its own thing
>
> Show every component in both themes, with variants and states laid out side by side.

---

## Prompt B — the screens

Run after A. Paste the domain context, then ask for screens in batches of 2–3 so each gets real attention.

> Now design the screens, using the design system exactly. Don't invent new tokens.
>
> **The domain — use this vocabulary literally, don't generalize it:**
> - An **Organization** contains **Projects**. A Project contains **Cache Backends**.
> - There are five backend types, and they are *not* interchangeable — each has its own config and its own metrics:
>   - `sstate` — Yocto shared-state cache. A blob store. Metric that matters: hit rate on `HEAD` requests, and total size.
>   - `downloads` — Yocto source premirror. A flat blob store of tarballs.
>   - `hashserv` — Yocto hash-equivalence server. Not a blob store at all: it's a database of taskhash→unihash mappings over a WebSocket. Metrics: lookups/sec, equivalences found, connected builders.
>   - `bazel` — Bazel remote-cache API (gRPC + HTTP). Serves moon, ccache, and sccache simultaneously. Has two sub-stores: an Action Cache (`/ac`) and a Content-Addressable Store (`/cas`).
>   - `oci` — Docker/OCI pull-through proxy. Metrics: cache hit rate, upstream registries proxied, bytes saved, Docker Hub rate-limit headroom.
> - **API keys** are project-scoped and per-user, with read and/or write scope, an optional expiry, and are **shown exactly once** at creation.
> - Users have roles at both the org and project level.
>
> **Screens, in priority order:**
>
> 1. **Project overview.** The screen people leave open on a second monitor. Hit rate over time (the hero number), storage used vs. quota, requests/sec, and a row of per-backend cards each showing its own hit rate and size. Must answer "is the cache healthy" in under two seconds.
>
> 2. **Backend detail.** One per backend type — they should share a skeleton but differ in the middle. Hit/miss over time, size over time, recent requests (a dense monospace table: timestamp, method, key, status, bytes, duration), and the backend's config.
>
> 3. **⭐ The config snippet generator.** *This is the most important screen in the product — design it like it's the product.* Given a project and a backend, it emits the exact client configuration the user must paste, with a freshly-minted API key already baked in. Yocto users get a `local.conf` block; moon users get `.moon/workspace.yml`; ccache users get a `ccache.conf` line; containerd users get a `hosts.toml`. Each of these has a notorious footgun (e.g. Yocto's `~/.netrc` needs the *full URL* as its `machine` token, not the hostname — everyone gets this wrong). So the screen needs: a client picker, a syntax-highlighted copyable block, a one-click "copy", and room for a prominent inline warning callout per client. The emotional goal is "oh thank god, I don't have to read the docs."
>
> 4. **API keys.** List (name, scope, created, last used, expiry, revoke) plus a creation flow with a hard, unmissable **shown-once** secret reveal. Make the "you will never see this again" moment impossible to click past on autopilot.
>
> 5. **Project list** and **org switcher.**
>
> 6. **Backend config forms** — five different shapes, one consistent frame. Include the "require authentication" toggle, which is **on by default**, and which must clearly communicate that turning it off still leaves *writes* protected.
>
> 7. **Login.** OIDC provider buttons (Google, Authelia, GitHub). There is also a dev-login mode that only exists when an env var is set — show that variant as a visually distinct, obviously-not-production state.
>
> **Cross-cutting:** design the empty states (a brand-new project has no data and no traffic — this is the first thing every user sees, and it should teach them what to do next), the loading states, and the error state for "backend is configured but has never received a request," which is the single most common support question this product will generate.
