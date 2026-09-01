# UI reskin — proposal

**Status: proposal, nothing implemented.** This is a plan to argue with, not a spec to hand to a
contractor. Numbers in the "where we are" section were measured against the tree at the time of
writing; re-measure before acting on them.

## Where we are

- **Every design decision lives in one 871-line `<style>` block** at the top of
  `internal/web/templates/layout.html`, alongside the nav, the job-poll JavaScript and the footer.
  The CSS custom properties in it (`--bg-base`, `--accent`, `--text-primary`, …) are actually a
  decent token set with dark and light variants — they are just buried and undocumented.
- **Tailwind runs at runtime.** `static/js/tailwind-jit.js` is the play CDN vendored locally; the
  config is an inline `<script>` object. Utilities are generated in the browser on every page load,
  so there is no purge step, no build artifact, and a visible flash before styles settle on a cold
  load.
- **331 distinct utility classes** appear across the templates. 111 of them are used exactly once.
  The top ~20 (`flex`, `items-center`, `text-sm`, `text-muted`, `card`, `btn`, …) account for most
  of the markup. That distribution is the important fact: the templates are not really using
  Tailwind as a design system, they are using ~40 patterns over and over, with a long tail of
  one-offs that are mostly accidents.
- **There is no component vocabulary.** `card` and `btn` are hand-written classes in the style
  block; everything else is spelled out inline at each call site. The clearest symptom is
  `partials/broker-list.html`, where the desktop table row and the mobile card are ~45 duplicated
  lines of the same two controls — and have already drifted (the desktop copy carries a `title=`
  the mobile one lacks).
- **Dark-first, with a generic indigo accent** (`#6366f1`). It reads as "developer tool built in
  2026", which is honest but not distinctive, and not especially reassuring for a tool whose whole
  job is to be trusted with your name and address.

## Goals

1. Look like something a non-technical person would trust with their personal data. The current UI
   looks like an internal admin panel; the product is closer to a legal assistant.
2. Make a visual change cost one edit, not fifteen. Today a colour change means grepping the
   templates.
3. Keep the stack. Go templates + htmx, no SPA rewrite, no Node in the critical path if avoidable.
4. Lose nothing: the brokers page's filter/OOB-counter machinery is subtle and works; the reskin
   must not become a rewrite of behaviour.

**Non-goals:** a component framework, a design system for its own sake, animation for its own sake,
or a rebrand of the CLI.

## Direction

One sentence: **calm, evidentiary, boring in the way a good law office is boring.**

The product's emotional job is to make an anxious person feel like a process is under control. That
argues for:

- **Light-first, not dark-first.** Dark mode stays (the tokens already support it), but the default
  should be light. Dark UIs read as "hacker tool"; the audience here is someone who just found
  their home address on a people-search site.
- **A restrained, non-tech accent.** Move off indigo. Suggested: a deep ink/navy for structure with
  a single warm accent used *only* for the destructive/committal action (sending). Everything else
  is greyscale. A palette where colour means something is far more legible than one where six
  things are coloured.
- **Status colour with a strict vocabulary.** Sent / awaiting / needs-you / dead. Four states, four
  colours, used nowhere else. Right now green, amber, red and indigo appear both as status and as
  decoration, which makes the table hard to scan.
- **More type contrast, less border.** The current UI separates everything with 1px borders and
  cards. Replacing half of them with whitespace and type weight will do more for perceived quality
  than any colour change.
- **A real typeface.** The system stack is fine but anonymous. One self-hosted variable font for
  headings (something with a slightly editorial feel — Source Serif, Newsreader, or Instrument
  Serif for the display sizes) against a neutral sans for UI text would carry most of the reskin on
  its own. Self-host it; do not add a Google Fonts request to a privacy tool.
- **Iconography: one set, consistently sized.** Currently SVGs are pasted inline at whatever size
  each author felt like (`w-5 h-5` 68 times, `w-4 h-4` 52 times, plus one-offs).

## The mechanical spine

The visual direction is the easy half. The half that determines whether the reskin survives contact
with the next feature is the structure:

### 1. Extract the token layer

Move the CSS custom properties out of `layout.html` into `static/css/tokens.css`, and give them
**semantic** names rather than positional ones:

```
--surface-page / --surface-raised / --surface-sunken
--ink-strong / --ink / --ink-quiet
--edge-subtle / --edge-strong
--action / --action-hover / --action-quiet
--state-sent / --state-waiting / --state-attention / --state-dead
```

Both themes redefine the same names. Nothing else in the codebase is allowed to name a hex value.
This is the single highest-leverage change in the plan and it is independent of every aesthetic
decision below it — it can land first and alone.

### 2. Decide the Tailwind question

Three options, in order of my preference:

- **(A) Drop Tailwind; hand-write `app.css` against the tokens.** 331 utilities with a very steep
  usage curve means the real surface is small. A ~400-line stylesheet with a dozen component
  classes and a handful of layout utilities would cover the templates, delete a 400KB runtime
  dependency, remove the flash-of-unstyled-content, and make the CSS greppable. Cost: a mechanical
  pass over ~4,900 lines of template.
- **(B) Keep Tailwind, add a build step.** Precompile to a static CSS file in CI. Removes the FOUC
  and shrinks the payload, but puts Node in the build path of a Go project that currently has a
  one-line build.
- **(C) Keep the runtime JIT.** Zero work, keeps every current downside.

Recommendation: **(A)**, done incrementally — introduce `app.css`, migrate page by page, delete the
JIT script last.

### 3. Build a component vocabulary as partials

Today `partials/` holds fragments that happen to be reused by htmx. It should also hold the visual
primitives:

| Partial | Replaces |
|---|---|
| `partials/ui/button.html` | ~66 hand-spelled `btn`+utility combinations |
| `partials/ui/card.html` | 63 `card` sites with divergent padding |
| `partials/ui/badge.html` | the status pills, currently inline in four templates |
| `partials/ui/stat.html` | the dashboard/brokers counters |
| `partials/ui/empty.html` | the "nothing here yet" blocks, currently ad-hoc per page |
| `partials/ui/page-header.html` | the title/subtitle/actions row, repeated on every page |

**Blocker to resolve first:** Go templates take a single argument, and this repo has no `dict`
helper in its `FuncMap`, so a partial cannot be invoked with several parameters. Either add a
`dict` function (three lines, standard) or give each component a small Go struct built by the
handler. The `dict` route is less code; the struct route is type-checked. Either is fine — but the
decision blocks every component partial, so make it first.

### 4. Collapse the desktop/mobile duplication

With components in place, `partials/broker-list.html`'s two copies of each row become one partial
invoked twice with a different id prefix. This is already a known defect (flagged during the review
of #16) and the reskin is the natural moment to fix it rather than restyling both copies.

## Page treatment

| Page | What it needs |
|---|---|
| **Start Here** (`/welcome`) | Closest to the new direction already. Needs the type treatment and a proper hero; the identity-resolver table wants to become cards on mobile. |
| **Dashboard** | Currently a stat grid with no narrative. Should answer "what should I do next?" above the fold — one primary action, then the numbers. |
| **Brokers** | The hardest page and the most valuable. 850 rows, eight filters. Needs: sticky filter bar, a denser row, status as a leading indicator rather than a trailing badge, and a real empty/zero-results state. Behaviour (htmx includes, OOB counter) must not change. |
| **Pipeline / Tasks** | These are the "needs you" queue and should feel like an inbox: one item per row, the action inline, done items collapsing away. |
| **History** | Fine as a table. Wants date grouping and a filter that matches the brokers page's vocabulary. |
| **Settings / Setup** | The setup wizard is where a non-technical user is most likely to bail (Gmail app passwords). Deserves its own pass: progress, plain-language help, and an obvious "test this" affordance at each step. |

## Sequencing

Five PRs, each independently mergeable and independently revertable:

1. **Tokens.** Extract to `tokens.css`, rename to semantic roles, no visual change. Screenshot-diff
   proves it.
2. **`dict` (or component structs) + the first three components** (button, card, badge), migrating
   one page as the proof.
3. **The stylesheet decision** — introduce `app.css`, migrate the remaining pages off runtime
   Tailwind, delete `tailwind-jit.js`.
4. **The visual pass**: palette, typeface, spacing, iconography. This is the one that "looks like a
   reskin"; it should be nearly mechanical by this point.
5. **Page-level rework** for brokers, pipeline/tasks and setup, where layout — not styling — is the
   problem.

Steps 1–3 are refactors with no visual delta and can land while other feature work continues.
Step 4 is the one to review with fresh eyes, in both themes, on a phone.

## How to verify

- Screenshot every page in light and dark, before and after, at 1280px and 390px. The repo has
  headless Chromium available; this can be a script rather than a chore.
- Click through the brokers page after each step: filters, tag select, exclude/include, the
  "Emailable in this view" counter, and a send job's row-status polling. These are htmx-driven and
  a stray class rename can break them silently.
- Check contrast ratios against WCAG AA for both themes as part of step 4, not after.

## Open questions

1. Is there a brand — name, logo, wordmark — or does the reskin get to invent one?
2. Light-first by default: agreed, or do you want to keep dark as the default?
3. Is a self-hosted webfont acceptable (adds ~40KB to the binary via `embed`)?
4. Is accessibility a stated target (AA) or a best-effort?
5. Does the CLI's output get the same attention, or is this web-only?
