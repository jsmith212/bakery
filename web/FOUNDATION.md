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
  feedback/    Modal.svelte  Toast.svelte  EmptyState.svelte  Skeleton.svelte
  content/     CodeBlock.svelte  KeyValueList.svelte
  data/        StatTile.svelte  Sparkline.svelte  TimeSeriesChart.svelte
```

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
- **Badge** — either `status: 'hit'|'miss'|'stale'|'idle'` (semantic color + glyph) or
  `variant: 'type'|'accent'` (`type` = lowercase mono id badge, e.g. `sstate`), slot
  children.
- **Input** — `size: 'sm'|'md'|'lg'`, `mono`, `error`, `placeholder`, `value` (bindable),
  `disabled`. Field chrome: `.bk-field` label/hint/error-text.
- **Select** — `size`, `error`, `disabled`, options; chevron via scoped `::after`.
- **Toggle** — `checked` (bindable, drives `aria-checked`), `disabled`, `onchange`;
  knob via scoped `::after`.
- **Checkbox** — `checked` (bindable), `disabled`, label; check via scoped `clip-path`.
- **Tabs** — `tabs: {id,label}[]`, `active`, `onchange`; optional count via
  `.bk-tab-count`. Underline uses `border-b-2 border-accent-text` on the selected tab.
- **SideNav / ConsoleNav** — `sections: {label, items: {id,label,badge?}[]}[]`,
  `active`, `onselect` / `<a>` links; `header` + `footer` snippets. In the app, nav items
  are `<a>`; `aria-current` derives from `$page.url.pathname`. The theme toggle lives in
  the footer and drives the theme store.
- **Table** — `dense`, `columns: {key,label,mono?,num?,width?,sortable?,render?}[]`,
  `rows`. `num` → right-aligned tabular; `mono` → mono cell; `render(row)` → snippet.
- **StatTile** — `label`, `value`, `unit`, `delta`, `deltaGood` (bool → up/down class),
  `spark: number[]`, `sparkColor`.
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

```
/                       -> redirect to /overview (routes/+page.ts, throw redirect 307;
                           routes/+page.svelte also gotos as a client fallback)
/login                  routes/login/+page.svelte      (full screen, NO nav rail)

(console) group — routes/(console)/+layout.svelte renders LEFT NAV + <main> in a
flex, min-h-screen, bg-bg-0 shell; main is `flex flex-col gap-4 px-5 py-4`.
The nav is a placeholder to be replaced by ConsoleNav.svelte.

  /overview             routes/(console)/overview/+page.svelte
  /projects             routes/(console)/projects/+page.svelte
  /backends/[type]      routes/(console)/backends/[type]/+page.svelte
                          (type: sstate|downloads|hashserv|bazel|oci; default sstate)
  /backends/new         routes/(console)/backends/new/+page.svelte   (static wins over [type])
  /snippets             routes/(console)/snippets/+page.svelte
  /keys                 routes/(console)/keys/+page.svelte
  /members              routes/(console)/members/+page.svelte
  /settings             routes/(console)/settings/+page.svelte
  /user                 routes/(console)/user/+page.svelte
```

All console pages currently hold a single placeholder heading; screen agents replace the
bodies. ConsoleNav items are `<a href>`; derive `aria-current` from `$page.url.pathname`.

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
