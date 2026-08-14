# Plan: Direct Tailwind Plus + Elements migration

**Status:** clarified approved direction, pending execution  
**Date:** 2026-08-13  
**Owner:** divydeep

## Authority and goal

Migrate directly from the current Web Awesome (WA) working state to Tailwind CSS, locally adapted Tailwind Plus blocks, Tailwind Plus Elements, and inline Heroicons. Preserve commit `517e642` and all unrelated current work.

[`docs/spec-tailwind-plus-elements.md`](../spec-tailwind-plus-elements.md) is the executable master Definition of Done (DoD). Its allowlist, commands, tests, and success criteria govern execution if this summary omits detail.

Commit `8ef62c7` is reference material only. Do not reset, revert, check out, reconstruct, or create an intermediate checkpoint from it. There is no Phase 1 rollback, staged pre-migration state, or requirement to make such a tree green before migration.

## Confirmed decisions

- Use Tailwind Plus source at `/Users/divy/Downloads/tailwind_plus`, adapting the blocks closest to the current UI. Do not modify or redistribute that source.
- Load Tailwind Plus Elements from the exact module URL `https://cdn.jsdelivr.net/npm/@tailwindplus/elements@1`.
- Render Heroicons as inline templ SVG helpers: outline by default and solid only for filled states. Use no icon CDN or runtime icon library.
- Commit generated `internal/ui/static/css/output.css`; do not introduce Node, npm, PostCSS, a frontend bundler, or another application runtime.
- Preserve all eight presets—`obsidian`, `mono`, `ember`, `graphite`, `terminal`, `paper`, `console`, and `solar`—with the canonical `--color-*` tokens, localStorage persistence, separate remembered dark/light selections, the theme settings page, mode memory, HTMX resynchronization, and embedded-asset cache behavior.
- Preserve existing server, form, routing, HTMX, flash-cookie, and navigation contracts while changing the UI implementation.

## Execution plan

### 1. Protect the current baseline

Require a clean worktree before recording `BASELINE`, confirm `517e642` remains an ancestor, and create a pre-implementation SHA-256 manifest of the licensed Tailwind Plus source under the approved temporary root:

```sh
ROOT=/Users/divy/Projects/cyclopus
cd "$ROOT"
test -z "$(git status --porcelain)"
BASELINE=$(git rev-parse HEAD)
test -n "$BASELINE"
git merge-base --is-ancestor 517e642 "$BASELINE"
TEMP_PARENT=/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode
mkdir -p "$TEMP_PARENT"
TAILWIND_PLUS_BASELINE_MANIFEST="$TEMP_PARENT/tailwind-plus-before.sha256"
find /Users/divy/Downloads/tailwind_plus -type f -print0 | sort -z | xargs -0 shasum -a 256 > "$TAILWIND_PLUS_BASELINE_MANIFEST"
test -s "$TAILWIND_PLUS_BASELINE_MANIFEST"
```

Preserve unrelated work. Do not use reset, checkout, revert, history rewriting, a backup branch, or an `8ef62c7`-based intermediate tree.

### 2. Install and run pinned Tailwind

- Update setup tooling to download the official checksum-verified Tailwind standalone binary `v4.3.3` into `.cyclopus-dev/bin/tailwindcss` using testable platform-selection, checksum-verification, and atomic-install functions.
- Add the focused setup shell test required by the master DoD.
- Use this canonical build command everywhere:

  ```sh
  $ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify
  ```

- The development watcher uses the same command with only `--watch` appended, then runs Air without changing `.air.toml`.

### 3. Permanently remove preview infrastructure

Delete `CYCLOPUS_UI_PREVIEW`; both `/__theme-preview*` routes and their handlers; preview templates and generated files; `scripts/dev/check_ui.mjs`; and the complete `scripts/ui-check/**` tree, including package manifests and dependencies. Remove all remaining production references and ignored preview paths.

Removal is permanent. No preview application route, committed fixture, screenshot tool, package manifest, production preview harness, or replacement may remain. Focused Go handler tests must prove `/__theme-preview` and `/__theme-preview/flash` return 404.

### 4. Build two disjoint foundations

- **Tailwind/theme owner:** only `internal/ui/static/theme.js`, `internal/ui/static/css/**`, and `internal/ui/theme_tokens_test.go`. Create `input.css` and committed, minified `output.css`; delete every legacy CSS file, including `ui.css`; restrict handwritten CSS to approved Tailwind constructs and existing `@font-face` blocks; remove WA/FA styling; and preserve the eight canonical themes, persistence, mode memory, defaults, settings controls, and HTMX synchronization.
- **Embed/cache owner:** only `internal/ui/embed.go` and `internal/ui/embed_test.go`. Keep `/static/css/output.css` and `/static/theme.js` embedded and served with `Cache-Control: public, max-age=86400`.

These two foundation tasks run in parallel after tooling and preview removal. After both complete, run their focused Tailwind/theme and asset-serving checks as one shared verification gate. Only after that gate passes, create the shared inline Heroicon API required by every wrapper and page.

### 5. Build all five wrapper batches

Create every approved wrapper, whether or not a current page consumes it, in disjoint templ source/generated pairs adapted from the closest-current-UI Plus blocks:

- **Batch A:** `Dialog`, `Dropdown`, `Select`.
- **Batch B:** `Combobox`, `Tabs`, `Disclosure`.
- **Batch C:** `Popover`, `CommandPalette`, `Tooltip`; Tooltip uses Elements popover behavior.
- **Batch D:** `Toast`, `Flash`, `Alert`; preserve the flash-cookie redirect flow and use utility transitions without custom keyframes.
- **Batch E:** `Table`, `Badge`, `EmptyState`; use Tailwind utilities only.

After the shared foundation gate and icon API, batches A–C form parallel group A, and batches D–E form disjoint parallel group B. The shared shell waits for all five batches. Use semantic HTML, caller-provided IDs, valid ARIA relationships, successful form controls, and inline Heroicons.

### 6. Migrate the shell, then three page groups

After all wrapper batches, migrate the shared layout and navigation sequentially. The layout must reference only the approved local assets, local HTMX, inline icon output, and the exact Elements CDN module URL.

Then migrate these three disjoint page groups in parallel, directly from their current WA markup:

- **Page group A:** login, bootstrap, projects, and project.
- **Page group B:** app, app settings, environment, environment-status fragment, and logs.
- **Page group C:** settings/user, settings/tokens, settings/registries, settings/theme, settings/audit, settings/git connections, settings/webhooks, and admin reconciliation.

The `settings/theme_preview.templ` and `settings/theme_preview_templ.go` pair is deleted with preview infrastructure; it is not part of any migration group. Preserve route behavior, form names and values, HTMX targets/triggers/swaps, navigation, flash behavior, and the theme settings page.

### 7. Verify server and browser behavior

- Use focused Go tests for server-visible flow: asset caching, preview-route 404s, flash-cookie redirects and one-time rendering, HTMX fragments, form controls, page attributes, theme contracts, icon output, and wrapper markup.
- For client interactions and all themes, generate a temporary standalone static fixture by calling every production wrapper and production icon helper. Write it only below `/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode`, copy `output.css` and `theme.js` into its server root, and reference the exact Elements CDN URL.
- Serve the fixture separately on loopback with Python, verify themes and component interactions in the OpenChamber browser panel, stop the server, and delete the fixture. Do not commit a replacement browser harness.
- UI/component tests and handler tests form disjoint parallel work after page migration; temporary browser verification follows those tests, and final verification follows fixture cleanup.
- Finish with the master DoD's templ generation, formatting, canonical Tailwind reproducibility, build, vet, full tests, whitespace checks, commit-preservation check, and exact changed-path allowlist check.
- Prove the licensed source is unchanged, then remove both manifests:

  ```sh
  TAILWIND_PLUS_FINAL_MANIFEST="$TEMP_PARENT/tailwind-plus-after.sha256"
  find /Users/divy/Downloads/tailwind_plus -type f -print0 | sort -z | xargs -0 shasum -a 256 > "$TAILWIND_PLUS_FINAL_MANIFEST"
  cmp "$TAILWIND_PLUS_BASELINE_MANIFEST" "$TAILWIND_PLUS_FINAL_MANIFEST"
  ! find "$ROOT" -path "$ROOT/.git" -prune -o -type f -print | rg '/tailwind_plus/'
  rm -f "$TAILWIND_PLUS_BASELINE_MANIFEST" "$TAILWIND_PLUS_FINAL_MANIFEST"
  ```

## Out of scope

- Realtime graphs, chart libraries, or SSE/WebSocket log streaming.
- Marketing or marketing-page blocks from Tailwind Plus.
- Changes to the Scalar `/docs` application or its vendored assets.
- Removing or replacing the canonical daisyUI-style `--color-*` token vocabulary; daisyUI itself remains unadopted.
- Changes to `.air.toml`, Dockerfiles, containers, CI, release or deployment pipelines, Helm, or Kubernetes.
- Backup branches, history rewriting, or rollback/reconstruction from `8ef62c7`.
- Reverting, overwriting, or removing `517e642` or unrelated current work.
- Backend services, public APIs, authentication contracts, workers, adapters, databases, schemas, or migrations, except removal of preview-only handler code and focused handler tests.
- New application or preview routes, preview flags, committed preview fixtures, screenshot tooling, or production preview replacements.
- npm, Node, package manifests, Playwright, frontend bundlers, PostCSS, daisyUI, React, Vue, or another application runtime.
- Vendoring Tailwind Plus Elements, changing its confirmed CDN URL, using icon CDNs, or runtime-loading icon libraries.
- Modifying `/Users/divy/Downloads/tailwind_plus`, copying unrelated Plus blocks, or redistributing Plus source as a component library.
- Custom page, component, table, log, scrollbar, animation, toast-keyframe, or utility CSS outside generated Tailwind output.
- Adding theme presets, removing the theme settings page, changing persistence semantics, or replacing the canonical theme vocabulary.
