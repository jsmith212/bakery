# Bakery frontend — foundation contract

This file is the interface between the foundation layer and the component/screen
agents (phases 2–3). It documents the Tailwind utility vocabulary, theming, where
components live, target component prop APIs, and the route map. Treat it as binding.

## What is set up

- **Tailwind CSS v4** (`tailwindcss` 4.3.x + `@tailwindcss/vite`), wired into
  `web/vite.config.ts` (`tailwindcss()` plugin, before `sveltekit()`). No
  `tailwind.config.js` — configuration is CSS-first via `@theme` in `web/src/app.css`.
- **Design tokens**, copied verbatim to `web/src/lib/styles/tokens/`:
  `colors.css`, `typography.css`, `spacing.css`, `keyframes.css`, `fonts.css`.
  These are the source of truth. Do not edit them; do not add `components.css` to
  the bundle (it is a spec for you to rebuild as `.svelte` components).
- **`web/src/app.css`** — the global stylesheet. Import order: fonts, then
  `tailwindcss`, then the remaining token files, then the `@theme` mapping, then a
  base `@layer`. Imported once, in `web/src/routes/+layout.svelte`.
- **Theme store** `web/src/lib/theme.ts` + a no-FOUC inline script in
  `web/src/app.html`.
- **Route skeleton** — a `(console)` group with a left-nav placeholder, `/login`
  outside it, and placeholder pages for every screen.

Verified: `npm --prefix web run build` passes and emits `web/dist/_app/`;
`svelte-check` is clean (0 errors, 0 warnings); the Go embed test and `go build ./...`
stay green; `vitest` passes.

## Theming — how it works

Theming is **CSS variables**, not Tailwind `dark:` variants. Never use `dark:`.

- `colors.css` defines the palette twice: dark on `:root, [data-theme="dark"]`,
  light on `[data-theme="light"]`. These are the runtime layer.
- The `@theme` block in `app.css` maps Tailwind's `--color-*` namespace onto those
  vars by reference, e.g. `--color-bg-0: var(--bg-0)`. So a utility like `bg-bg-0`
  compiles to `background-color: var(--color-bg-0)` → `var(--bg-0)`, which the
  `[data-theme]` selector re-points. **Flipping `document.documentElement.dataset.theme`
  between `"dark"` and `"light"` recolors everything with no rebuild.** (Proven in the
  compiled CSS: `.bg-bg-0{background-color:var(--color-bg-0)}`,
  `--color-bg-0:var(--bg-0)`, `[data-theme=dark]{--bg-0:#0b0c0e}`,
  `[data-theme=light]{--bg-0:#fafafb}`.)
- **No raw hex in component/screen markup.** Every color must come through a token
  utility (or a token var in a scoped `<style>` for the few things utilities can't
  express — see below). The two literal `#FFFFFF`s in `components.css` (toggle knob,
  checkbox check) are component-internal and go in a scoped `<style>`, not markup.

### The theme store (`web/src/lib/theme.ts`)

```ts
type Theme = 'dark' | 'light' | 'system';        // default 'system'
type ResolvedTheme = 'dark' | 'light';

import { theme, setTheme, resolveTheme, applyTheme, THEME_STORAGE_KEY } from '$lib/theme';

theme            // writable<Theme> store — subscribe with $theme in .svelte
setTheme(v)      // set + persist (localStorage 'bakery-theme') + apply to <html>
resolveTheme(v)  // Theme -> 'dark'|'light' (resolves 'system' via matchMedia)
applyTheme(v)    // writes resolved value to document.documentElement.dataset.theme
```

The store auto-persists to `localStorage["bakery-theme"]`, applies to `<html>` on every
change, and re-applies when the OS scheme changes while in `system` mode. The no-FOUC
script in `app.html` sets `data-theme` before first paint from the same key.

**Both** the ConsoleNav footer theme control and the User Settings "Appearance" control
must drive this one store (`setTheme` / bind to `$theme`). Do not create a second source
of truth.

## Utility vocabulary (what to write in markup)

### Colors

Every color token is exposed under Tailwind's color namespace, so each works as
`bg-*`, `text-*`, `border-*`, `ring-*`, and (for SVG) `fill-*` / `stroke-*`.

| Group | Utility suffixes | Example |
|---|---|---|
| Surfaces | `bg-0 bg-1 bg-2 bg-3 bg-inset bg-overlay` | `bg-bg-1`, `bg-bg-inset` |
| Borders | `border-0 border-1 border-2` | `border-border-0` |
| Text | `text-1 text-2 text-3 text-disabled text-on-solid` | `text-text-1`, `text-text-3` |
| Accent | `accent-solid accent-solid-hover accent-solid-active accent-text accent-muted accent-border focus-ring` | `bg-accent-solid`, `text-accent-text`, `bg-accent-muted` |
| Semantic | `ok ok-muted ok-border warn warn-muted warn-border err err-muted err-border err-solid err-solid-hover idle idle-muted idle-border` | `text-ok`, `bg-err-muted`, `border-warn-border`, `bg-err-solid` |
| Chart | `chart-hit chart-miss chart-stale chart-idle chart-1 … chart-7 chart-grid chart-axis` | `stroke-chart-hit`, `fill-chart-2`, `text-chart-axis` |

Note the doubled word: text-color utilities are `text-text-1`, `text-text-2`,
`text-text-3` (the token suffix is `text-N`; the utility prefix is `text-`). Font-size
utilities are `text-xs … text-metric` (different set — see below). No collision:
`text-1` is not a color name; `text-text-1` is.

Series order in cache charts is fixed: series 1 = `chart-hit` (green), series 2 =
`chart-miss` (red). `--chart-fill-alpha` (0.10 dark / 0.08 light) is a raw var for area
fills.

### Radius

`rounded-1` = 4px (buttons, inputs, badges, cells), `rounded-2` = 6px (cards, panels,
modals, code blocks), `rounded-3` = 8px (rare page containers). `rounded-full` (Tailwind
built-in) is reserved for status dots and toggle knobs only. **No pills.**

### Fonts and type

- Family: `font-sans` (Geist), `font-mono` (JetBrains Mono). Body defaults to sans;
  add `font-mono` for hashes, keys, config, logs, timestamps.
- Size (size + line-height baked in): `text-xs` 11/16, `text-sm` 12/18,
  `text-base` 13/20 (UI default), `text-md` 14/20, `text-lg` 16/24, `text-xl` 20/28,
  `text-metric` 26/32 (stat-tile numbers).
- Weight (600 is the ceiling — no bolds): `font-normal` 400, `font-medium` 500,
  `font-semibold` 600.
- Tabular numerals: add `class="tabular"` (global helper from `typography.css`) to any
  column of figures — metrics, deltas, counts — so numbers do not jitter. Mono is
  tabular by construction.
- Uppercase micro-labels: `tracking-[var(--tracking-label)]` with `uppercase text-xs`.

### Spacing

Tailwind's default 4px numeric scale is intentionally identical to the `--space-*`
tokens — use the numbers directly on `p-/m-/gap-/px-/py-/space-*`:

`0.5`=2px (space-05) · `1`=4px · `1.5`=6px (space-15) · `2`=8px · `3`=12px · `4`=16px
· `5`=20px · `6`=24px · `8`=32px.

Dense defaults: 12px card padding (`p-3`), main content padding 16px 20px (`py-4 px-5`),
16px section gap (`gap-4`).

### Layout constants, motion, keyframes (arbitrary values via token vars)

These are raw CSS vars (from `spacing.css`); use them with arbitrary-value syntax:

- Widths/heights: `w-[var(--sidenav-w)]` (220px), `h-[var(--control-sm)]` (24px),
  `h-[var(--control-md)]` (28px), `h-[var(--control-lg)]` (32px),
  `h-[var(--table-row-h)]` (32px), `h-[var(--table-row-h-dense)]` (28px).
- Motion: `--dur-1` 80ms, `--dur-2` 120ms, `--dur-3` 200ms, `--ease`. Use
  `duration-[80ms] ease-[cubic-bezier(0.25,0.1,0.25,1)]` or the var forms. Fades and
  ≤6px translates only; no bounce.
- Keyframes (from `keyframes.css`): `ds-shimmer` (skeleton), `ds-fade-in` (scrim),
  `ds-slide-up` (modal/toast). Use `animate-[ds-fade-in_200ms_var(--ease)]` etc.

### Focus

The base layer applies the design-system focus ring globally on `:focus-visible`
(`2px var(--focus-ring)`, 1px offset). Never remove it. Custom controls that reset
outlines must restore it.

## Where components go

Build each as a `.svelte` component under `web/src/lib/components/<group>/`, using the
utilities above. Match `tokens/components.css` (`.bk-*`) exactly for visuals/states and
`ui_kit_console_index.html` for the prop API. Do **not** import `components.css` or ship
`.bk-*` classes globally — rebuild them.

```
web/src/lib/components/
  buttons/     Button.svelte
  inputs/      Input.svelte  Select.svelte  Toggle.svelte  Checkbox.svelte
  badges/      Badge.svelte
  table/       Table.svelte
  navigation/  Tabs.svelte  ConsoleNav.svelte      (replaces the (console) nav placeholder)
  feedback/    Modal.svelte  Toast.svelte  EmptyState.svelte  Skeleton.svelte  Callout.svelte
  content/     CodeBlock.svelte  KeyValueList.svelte
  data/        StatTile.svelte  Sparkline.svelte  TimeSeriesChart.svelte
               Provenance.svelte
  feedback/    ... ToastHost.svelte
```

Three additions from the SPA→API wiring wave:

- **ToastHost** — mounted exactly once, in `(console)/+layout.svelte`, driven by
  `$lib/toasts`. `Toast` had zero importers before it, which meant no mutation in
  the console had any success or failure feedback.
- **Provenance** — `provenance: Provenance` (`$lib/api/types`), fed by
  `memberProvenance()` / `siteAdminProvenance()`. One component for both rosters
  because the two wire shapes share **no** provenance tag
  (`oidc_role`/`oidc_group`/`local_role`/`org_role_source` versus
  `site_role_oidc`/`site_oidc_group`/`site_role_local`/`site_role_source`).
  `org_role_source` is its own vocabulary and is **never** a `BadgeStatus`. It
  carries no `effective` field on purpose: both consuming rosters already render
  the effective role through their own dedicated control next to it (the
  members table's org-role `<Select>`; the site-admins roster needs no separate
  column at all), so `<Provenance>` answers WHY, never WHAT.
- **Callout** (`feedback/Callout.svelte`) — `variant: 'warn'|'error'|'info'`,
  optional `title` (a bold LEAD CLAUSE inline with the body, not a stacked
  heading), children. Replaces eight identical hand-rolled
  `border+glyph+px-3 py-2.5` blocks that had drifted into near-duplicates
  across `keys`, `snippets`, `backends/new` and `gc`; its glyph/color pairing
  is read from the same table `Toast` uses (`▲ warn`, `✕ error`, `○ info`) so
  a warning reads the same whether it lands as a toast or sits in place on the
  page.

`BadgeStatus` has exactly one producer from cache-backend data: `backendStatus()`
in `$lib/backendStatus`. There is no `as BadgeStatus` cast anywhere and none may
be reintroduced — there is no JS lint toolchain in this repo, so the guarantee is
structural.

Use a tiny scoped `<style>` ONLY for what utilities can't express: the toggle knob
`::after`, the checkbox check `clip-path` `::before`, the select chevron `::after`.
Reference token vars inside it — never raw hex except the two `#FFFFFF` knob/check fills.

### Target component prop APIs

Authoritative source: `ui_kit_console_index.html` (usage) + `tokens/components.css`
(visuals/states). Svelte port: React `children` → default snippet; React `onX` →
callback props or events; `render`/node props → snippets. Status is a typographic glyph
+ color, never an icon: `●` hit, `✕` miss, `▲` stale, `○` idle, `∅` empty.

- **Button** — `variant: 'primary'|'secondary'|'ghost'|'danger'`,
  `size: 'sm'|'md'|'lg'`, `disabled`, `onclick`, slot children (verb-first label).
  Optional `href` swaps the root element to `<a>` (same variant/size classes) so a
  link never wraps a `<button>` — invalid nesting, two tab stops, and `disabled`
  silently ignored on the button since an anchor has no such attribute. A
  disabled anchor is genuinely inert: no `href`, `tabindex="-1"`, `aria-disabled`,
  and a click guard that blocks Enter-activation too, not just a mouse click.
- **Badge** — either `status: 'hit'|'miss'|'stale'|'idle'` (semantic color + glyph) or
  `variant: 'type'|'accent'` (`type` = lowercase mono id badge, e.g. `sstate`), slot
  children.
- **Input** — `size: 'sm'|'md'|'lg'`, `mono`, `error`, `placeholder`, `value` (bindable),
  `disabled`. Field chrome: `.bk-field` label/hint/error-text.
- **Select** — `size`, `error`, `disabled`, options; chevron via scoped `::after`.
- **Toggle** — `checked` (bindable, drives `aria-checked`), `disabled`, `onchange`,
  optional `label` (renders as an adjacent span wired to the switch via
  `aria-labelledby`, minted with `$props.id()` — `role="switch"` gets nothing
  from a wrapping `<label>`, unlike a native control); knob via scoped `::after`.
- **Checkbox** — `checked` (bindable), `disabled`, label; check via scoped `clip-path`.
- **Tabs** — `tabs: {id,label}[]`, `active`, `onchange`, optional `mono` (identifier-like
  tab sets — namespace/kind switchers — render `font-mono text-sm` instead of the
  sans/base default); optional count via `.bk-tab-count`. Underline uses
  `border-b-2 border-accent-text` on the selected tab.
- **Field** — `label`, `hint`, `error`, optional `for` (an id override; unused so
  far). Mints its own id and passes `{id, 'aria-invalid', 'aria-describedby'}` to
  `children` as a snippet parameter — `{#snippet children(f)}<Input {...f} .../>{/snippet}` —
  so `Label`'s `for` actually resolves to the control, and the error/hint text is
  wired to it via `aria-describedby` (`aria-invalid` only on the error path).
- **Callout** — `variant: 'warn'|'error'|'info'`, optional `title` (bold, inline
  lead clause — not a stacked heading), children. See "Where components go" above.
- **SideNav / ConsoleNav** — `sections: {label, items: {id,label,badge?}[]}[]`,
  `active`, `onselect` / `<a>` links; `header` + `footer` snippets. In the app, nav items
  are `<a>`; `aria-current` derives from `$page.url.pathname`. The theme toggle lives in
  the footer and drives the theme store.
- **Table** — `dense`, `columns: {key,label,mono?,num?,width?,sortable?,render?}[]`,
  `rows`. `num` → right-aligned tabular; `mono` → mono cell; `render(row)` → snippet.
- **StatTile** — `label`, `value`, `unit`, `delta`, `deltaGood` (bool → up/down class),
  optional `caption` (a line under the value for a state numbers alone can't say —
  "not yet measured", "across 3 capped backends"), `spark: number[]`, `sparkColor`.
- **Sparkline** — `data: number[]`, `height`, `color`. Port the SVG path math verbatim.
- **TimeSeriesChart** — `height`, `width`, `yMax`, `yFormat: (v)=>string`,
  `xLabels: string[]`, `series: {label,color,data}[]`. Hairline grid, 10px mono axis
  labels, 1.5px lines, 0.10 area fill; series 1 hit-green, 2 miss-red.
- **Modal** — `title`, `onclose`, `footer` snippet, body children; `.bk-scrim` +
  `.bk-modal`, `ds-fade-in` / `ds-slide-up`.
- **Toast** — `variant: 'success'|'error'|'warning'|'info'`, `title`, `detail`,
  `onclose`; leading glyph colored per variant.
- **EmptyState** — `glyph` (e.g. `∅`), `title`, `desc`, `action` snippet. Copy teaches
  the next step, never apologizes.
- **Skeleton** — shimmer block (`ds-shimmer`); use where layout is known instead of spinners.
- **CodeBlock** — `title`, `code`, copy button with a 1.5s "Copied" label; mono, `bg-inset`.
- **KeyValueList** — `pairs: {key,value,mono?}[]`; two-column grid, mono values for
  hashes/keys.

## Route map

Root `+layout.svelte` imports `app.css` and renders children. `+layout.ts` keeps
`ssr = false`, `prerender = false` (SPA). adapter-static fallback is `index.html`.

**Tenancy is in the PATH** (`/o/{org}/p/{project}/…`), as of the SPA→API wiring wave
(`docs/design/specs/2026-08-15-spa-api-wiring.md` §4). The two literal segments cost
nothing and buy immunity from slug collisions — an org named `projects` cannot be
confused with the projects page, and no slug has to join the reserved list.

```
/                       routes/+page.ts — resolves a tenant and redirects:
                          stashed return path -> remembered org -> me.orgs[0]
                          -> first org from GET /orgs (site admin) -> /orgs
/login                  routes/login/+page.svelte      (full screen, NO nav rail)
                          reads GET /auth/config; renders ?denied=login_gate as a
                          TERMINAL state; dev-login is a parameterless bodyless POST

(console) group — routes/(console)/+layout.ts is the GUARD (GET /me: 200 -> data,
401 -> stash + /login; there is NO 403 branch, it cannot happen). It also loads
GET /orgs and GET /auth/config for the nav and the create-org gate.
routes/(console)/+layout.svelte renders <ConsoleNav/> + <main> + <ToastHost/>.
routes/(console)/+error.svelte renders a load failure in place, in console chrome.

  /orgs                        org picker + create (gated on allow_self_serve_orgs)
  /user                        global
  /gc                          global, site admin      (lands in a later wave)
  /site-admins                 global, site admin      (lands in a later wave)

  o/[org]/+layout.ts           GET /orgs/{org} + GET /orgs/{org}/projects
                                 (NEVER me.projects — see $lib/api/projects)
  /o/[org]/projects
  /o/[org]/members
  /o/[org]/settings

  o/[org]/p/[project]/+layout.ts   picks the project out of the parent's list
  /o/[org]/p/[project]/overview
  /o/[org]/p/[project]/backends/[type]   (sstate|downloads|hashserv|bazel|oci)
  /o/[org]/p/[project]/backends/new      (static wins over [type])
  /o/[org]/p/[project]/keys
  /o/[org]/p/[project]/snippets
```

The pre-tenancy flat routes (`/overview`, `/projects`, `/backends/[type]`,
`/backends/new`, `/keys`, `/snippets`, `/members`, `/settings`) survive for one
release as `+page.ts` redirects (`$lib/legacy`). They exist because the SPA fallback
serves `index.html` for ANY path: without them a bookmarked URL does not 404 loudly,
it renders the console's own not-found and looks like the console broke.

ConsoleNav items are `<a href>`; `aria-current` derives from `page.url.pathname`.
The org and project switchers are `<a href>` navigation built from the layout data —
never local state.

## Notes / flags

- **Fonts**: `fonts.css` uses a Google Fonts `@import` (Geist + JetBrains Mono). This
  ships fine for now, but Bakery is an embedded, possibly-offline self-hosted console —
  **production should self-host the woff2** and drop the remote import. Not a blocker.
- **Dependencies**: only `tailwindcss` + `@tailwindcss/vite` were added (both dev). No
  `lucide-svelte` yet — the mockups use typographic glyphs, not icons. Add it only if a
  screen genuinely needs an icon, and flag it.
- **Do not break the embed**: `npm run build` must keep emitting `web/dist/` with
  `index.html` and `_app/`; `web/embed_test.go` must stay green.
- Voice/fidelity rules (sentence case, terse, no emoji, no exclamation points, second
  person, tabular numerals, status glyphs) come from `handoff/readme.md` — obey literally.
