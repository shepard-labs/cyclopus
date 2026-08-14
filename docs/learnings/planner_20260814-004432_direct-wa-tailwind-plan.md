# Ultracode Learnings: Phase 1

## Phase

Phase 1 — Approved the specification and corrected the authoritative plan for a direct Web Awesome-to-Tailwind migration.

## What Was Done

- Approved `docs/spec-tailwind-plus-elements.md` as the executable Definition of Done for the migration.
- Corrected `docs/plans/tailwind-plus-elements-2026-08-13.md` to describe a direct migration from the current Web Awesome state.
- Recorded the complete wrapper scope: Dialog, Dropdown, Select, Combobox, Tabs, Disclosure, Popover, CommandPalette, Tooltip, Toast, Flash, Alert, Table, Badge, and EmptyState.
- Defined temporary browser verification through a generated static fixture outside the repository, backed by production wrappers and icon helpers.
- Defined focused Go tests for server-visible flows, including preview-route removal, flash cookies, HTMX fragments, form contracts, embedded assets, themes, icons, and wrapper markup.
- Pinned the local standalone Tailwind binary to v4.3.3 with checksum verification and atomic installation requirements.
- Recorded the approved UI sources: Tailwind Plus Elements from its exact CDN module URL, inline Heroicons, and local Tailwind Plus source at `/Users/divy/Downloads/tailwind_plus`.
- Restricted handwritten CSS to Tailwind directives, source and theme declarations, and the existing `@font-face` exception.

## Key Decisions

- Remove the preview feature completely, including routes, handlers, templates, scripts, package artifacts, environment flags, and ignored paths. A replacement preview route or committed browser harness would preserve the architecture the migration is intended to eliminate.
- Migrate directly from the current Web Awesome implementation. Commit `8ef62c7` is reference material only; no rollback, intermediate pre-Web-Awesome tree, or staged restoration is permitted.
- Implement all approved wrappers, even when no current page consumes them, so the delivered component foundation matches the approved scope.
- Generate a temporary static fixture under the approved system temporary directory for browser checks, then stop its loopback server and delete it. This provides interaction coverage without adding Node, Playwright, or permanent preview infrastructure.
- Use Go tests for server flows because routing, HTMX, forms, flash cookies, and embedded-asset behavior are server contracts rather than browser-harness concerns.
- Download the official Tailwind CSS v4.3.3 standalone binary locally, verify its SHA-256 checksum before installation, and install it atomically. This makes builds pinned and reproducible without introducing npm or another frontend runtime.
- Load Tailwind Plus Elements from `https://cdn.jsdelivr.net/npm/@tailwindplus/elements@1` rather than vendoring it.
- Render Heroicons inline through templ helpers, using outline icons by default and solid icons only for filled states. No icon CDN or runtime icon library is allowed.
- Adapt the blocks closest to the current UI from `/Users/divy/Downloads/tailwind_plus`; preserve that licensed source unchanged and avoid copying unrelated blocks.
- Use utility classes for page and component styling. The only handwritten CSS exception is the existing self-hosted `@font-face` declarations, alongside approved Tailwind and theme constructs.

## Findings Fixed

- Removed the contradictory requirement to retain, restore, or replace the preview harness after approving its permanent deletion.
- Removed rollback and intermediate pre-Web-Awesome checkpoint language that conflicted with the direct-migration decision.
- Clarified that all wrappers are required, not only wrappers already used by current pages.
- Replaced permanent browser tooling with a temporary generated fixture that calls production wrappers and icon helpers.
- Added focused Go coverage for server-visible behavior that browser-only checks would miss.
- Replaced an unpinned or globally resolved Tailwind dependency with a checksum-verified local v4.3.3 binary and one canonical build command.
- Fixed ambiguity around Elements, icons, and licensed source by specifying the exact CDN URL, inline Heroicons, and the local Tailwind Plus path.
- Replaced broad custom-CSS permission with a utilities-only rule and a narrow `@font-face` exception.
- Required closest-current-UI Tailwind Plus blocks rather than unrelated or wholesale Plus source reuse.

## DoD Results

- [x] **PASS** — The migration specification is approved and authoritative.
- [x] **PASS** — The implementation plan describes a direct migration from the current Web Awesome state.
- [x] **PASS** — Preview infrastructure is specified for complete and permanent removal.
- [x] **PASS** — All required wrapper batches and sequencing constraints are documented.
- [x] **PASS** — Temporary fixture and Go server-flow test strategies are explicit.
- [x] **PASS** — Tailwind v4.3.3 pinning, checksum verification, local installation, and canonical commands are defined.
- [x] **PASS** — Elements CDN, inline Heroicons, local Plus source, and closest-block adaptation decisions are recorded.
- [x] **PASS** — Utilities-only CSS and the `@font-face` exception are explicit.
- [x] **PASS** — Adversarial review findings were resolved without changing implementation files.

## Watch Out For

- Treat the specification as the master DoD when the shorter plan omits detail.
- Do not recreate preview routes, flags, committed fixtures, package manifests, screenshot tooling, or another permanent browser harness.
- Do not reset, revert, check out, or reconstruct an intermediate tree from `8ef62c7`; preserve commit `517e642` and unrelated work.
- Do not modify or redistribute `/Users/divy/Downloads/tailwind_plus`; verify it with before-and-after manifests during implementation.
- Do not substitute a global Tailwind command, a different Tailwind version, an unverified download, or npm-based tooling.
- Keep Elements on the exact approved CDN module URL and keep icons inline.
- The temporary fixture must live outside the repository, bind only to loopback, call every production wrapper, and be deleted after verification.
- Preserve existing routes, form names and values, HTMX contracts, flash-cookie behavior, theme persistence, and embedded-asset cache headers while replacing the UI implementation.
- Avoid custom component, page, table, log, scrollbar, animation, toast-keyframe, or utility CSS; retain only approved Tailwind/theme constructs and existing `@font-face` declarations.
