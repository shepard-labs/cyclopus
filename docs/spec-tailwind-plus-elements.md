## Problem

Cyclopus currently uses Web Awesome, Font Awesome, fragmented legacy CSS, and preview-only application routes and tooling that conflict with the approved Tailwind Plus architecture and complicate theme maintenance. The UI must migrate directly from the current working tree to Tailwind CSS, locally adapted Tailwind Plus blocks, Tailwind Plus Elements, and inline Heroicons while preserving unrelated current work—especially commit `517e642`—and retaining all server, form, HTMX, flash, routing, theme-persistence, and embedded-asset behavior.

## Approach

1. **Phase 0 — Repository safety:** Require a clean working tree, record the starting `HEAD` as `BASELINE`, confirm commit `517e642` is an ancestor, and use no reset, checkout, revert, history rewrite, or backup branch.
2. **Phase 1 — Plan correction:** Update `docs/plans/tailwind-plus-elements-2026-08-13.md` before implementation so it records all confirmed overrides and contains no preview-harness, intermediate rollback, pre-WA checkpoint, or preview-restoration contradiction.
3. **Phase 2 — Tooling and preview removal:** In one sequential phase, make `scripts/dev/setup.sh` expose testable download/install functions and install a pinned official checksum-verified Tailwind CSS `v4.3.3` standalone binary at `.cyclopus-dev/bin/tailwindcss`, add `scripts/dev/setup_test.sh` to verify platform mappings and atomic checksum failure, make `scripts/dev/run.sh` start the canonical Tailwind watcher and Air without modifying `.air.toml`, and remove the preview environment variable, routes, handlers, templates, generated files, scripts, existing preview package manifests, and ignored dependency tree.
4. **Phase 3 — Tailwind and theme foundation:** Create `input.css`, generate committed `output.css` with the canonical command, remove every legacy CSS file, migrate all eight presets to the canonical `--color-*` vocabulary, preserve localStorage and remembered dark/light selections, and retain only Tailwind directives, theme bridging, source declarations, and existing self-hosted font faces as handwritten CSS.
5. **Phase 4 — Asset serving:** Update the embedded static-asset documentation and behavior so `/static/css/output.css` and `/static/theme.js` are served with `Cache-Control: public, max-age=86400`.
6. **Phase 5 — Foundation verification gate and icon API:** After the disjoint Tailwind/theme and asset-serving phases both complete, run their focused checks as a shared gate, then create the inline templ Heroicon helper API before any wrapper or page migration, using outline icons by default and solid icons only for filled states.
7. **Phase 6 — Wrapper batch A:** Implement `Dialog`, `Dropdown`, and `Select` as three disjoint templ source/generated pairs adapted from the closest relevant blocks under `/Users/divy/Downloads/tailwind_plus`.
8. **Phase 7 — Wrapper batch B:** Implement `Combobox`, `Tabs`, and `Disclosure` as three disjoint templ source/generated pairs adapted from the closest relevant Plus blocks.
9. **Phase 8 — Wrapper batch C:** Implement `Popover`, `CommandPalette`, and `Tooltip` as three disjoint templ source/generated pairs, with Tooltip using Elements popover behavior rather than custom CSS.
10. **Phase 9 — Wrapper batch D:** Implement `Toast`, `Flash`, and `Alert` as three disjoint templ source/generated pairs, preserving the existing flash-cookie redirect flow and using utility-based transitions without custom keyframes.
11. **Phase 10 — Wrapper batch E:** Implement `Table`, `Badge`, and `EmptyState` as three disjoint templ source/generated pairs using only Tailwind utilities.
12. **Phase 11 — Shared shell:** After required wrappers exist, migrate `layout.templ` and `nav.templ`, reference only `/static/css/output.css`, `/static/theme.js`, local HTMX, inline icon helpers, and `https://cdn.jsdelivr.net/npm/@tailwindplus/elements@1`, and preserve current navigation behavior.
13. **Phase 12 — Page group A:** Migrate login, bootstrap, projects, and project pages directly from their current markup while preserving all server-visible contracts.
14. **Phase 13 — Page group B:** Migrate app, app settings, environment, environment-status fragment, and logs pages directly from their current markup while preserving all server-visible contracts.
15. **Phase 14 — Page group C:** Migrate all settings and admin reconciliation pages directly from their current markup while preserving all server-visible contracts.
16. **Phase 15 — UI tests:** Add focused `internal/ui` and `internal/ui/components` tests for CSS restrictions, theme behavior, asset caching, inline icons, wrapper structure, IDs, tags, ARIA relationships, form controls, and HTMX attributes.
17. **Phase 16 — Handler tests:** Add focused `internal/uihandler` tests for exact preview-route 404s, flash-cookie redirect/render/clear behavior, HTMX fragments, and preserved form field names and values.
18. **Phase 17 — Temporary browser verification:** Build a standalone fixture under the approved temporary root, copy `output.css` and `theme.js` into its server root, generate HTML that references `/output.css`, `/theme.js`, and the exact Elements CDN URL, serve it with Python on loopback, exercise it with the OpenChamber browser panel, and stop the server and delete the fixture afterward.
19. **Phase 18 — Final verification:** Run templ generation, Go formatting, canonical Tailwind reproducibility, build, vet, tests, whitespace checks, and the exact changed-path allowlist check.

The canonical Tailwind build command for setup verification, manual builds, committed output, and reproducibility is:

`$ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify`

The development watcher uses the same command with only `--watch` appended.

The exact changed-path allowlist is below; paths ending in `/**` permit only recursive deletion of the named existing tree, generated `*_templ.go` files must be produced by `templ generate`, and no other path—including `.air.toml`—may change.

<!-- ALLOWLIST-START -->

- `.gitignore`
- `docs/plans/tailwind-plus-elements-2026-08-13.md`
- `docs/spec-tailwind-plus-elements.md`
- `scripts/dev/setup.sh`
- `scripts/dev/setup_test.sh`
- `scripts/dev/run.sh`
- `scripts/dev/check_ui.mjs`
- `scripts/ui-check/package.json`
- `scripts/ui-check/package-lock.json`
- `scripts/ui-check/node_modules/**`
- `internal/ui/embed.go`
- `internal/ui/embed_test.go`
- `internal/ui/theme_tokens_test.go`
- `internal/uihandler/handler.go`
- `internal/uihandler/flash_test.go`
- `internal/uihandler/ui_render_test.go`
- `internal/ui/static/theme.js`
- `internal/ui/static/css/input.css`
- `internal/ui/static/css/output.css`
- `internal/ui/static/css/alert.css`
- `internal/ui/static/css/badge.css`
- `internal/ui/static/css/base.css`
- `internal/ui/static/css/button.css`
- `internal/ui/static/css/fonts.css`
- `internal/ui/static/css/tokens.css`
- `internal/ui/static/css/ui.css`
- `internal/ui/static/css/utilities.css`
- `internal/ui/static/css/wa-overrides.css`
- `internal/ui/static/css/pages/logs.css`
- `internal/ui/static/css/components/card.css`
- `internal/ui/static/css/components/form.css`
- `internal/ui/static/css/components/sidebar.css`
- `internal/ui/static/css/components/status.css`
- `internal/ui/static/css/components/table.css`
- `internal/ui/static/css/components/topbar.css`
- `internal/ui/static/css/components/user-settings.css`
- `internal/ui/components/layout.templ`
- `internal/ui/components/layout_templ.go`
- `internal/ui/components/nav.templ`
- `internal/ui/components/nav_templ.go`
- `internal/ui/components/icons.templ`
- `internal/ui/components/icons_templ.go`
- `internal/ui/components/dialog.templ`
- `internal/ui/components/dialog_templ.go`
- `internal/ui/components/dropdown.templ`
- `internal/ui/components/dropdown_templ.go`
- `internal/ui/components/select.templ`
- `internal/ui/components/select_templ.go`
- `internal/ui/components/combobox.templ`
- `internal/ui/components/combobox_templ.go`
- `internal/ui/components/tabs.templ`
- `internal/ui/components/tabs_templ.go`
- `internal/ui/components/disclosure.templ`
- `internal/ui/components/disclosure_templ.go`
- `internal/ui/components/popover.templ`
- `internal/ui/components/popover_templ.go`
- `internal/ui/components/command_palette.templ`
- `internal/ui/components/command_palette_templ.go`
- `internal/ui/components/tooltip.templ`
- `internal/ui/components/tooltip_templ.go`
- `internal/ui/components/toast.templ`
- `internal/ui/components/toast_templ.go`
- `internal/ui/components/flash.templ`
- `internal/ui/components/flash_templ.go`
- `internal/ui/components/table.templ`
- `internal/ui/components/table_templ.go`
- `internal/ui/components/badge.templ`
- `internal/ui/components/badge_templ.go`
- `internal/ui/components/alert.templ`
- `internal/ui/components/alert_templ.go`
- `internal/ui/components/empty_state.templ`
- `internal/ui/components/empty_state_templ.go`
- `internal/ui/components/browser_fixture_test.go`
- `internal/ui/components/components_test.go`
- `internal/ui/pages/login.templ`
- `internal/ui/pages/login_templ.go`
- `internal/ui/pages/bootstrap.templ`
- `internal/ui/pages/bootstrap_templ.go`
- `internal/ui/pages/projects.templ`
- `internal/ui/pages/projects_templ.go`
- `internal/ui/pages/project.templ`
- `internal/ui/pages/project_templ.go`
- `internal/ui/pages/app.templ`
- `internal/ui/pages/app_templ.go`
- `internal/ui/pages/app_settings.templ`
- `internal/ui/pages/app_settings_templ.go`
- `internal/ui/pages/environment.templ`
- `internal/ui/pages/environment_templ.go`
- `internal/ui/pages/env_status.templ`
- `internal/ui/pages/env_status_templ.go`
- `internal/ui/pages/logs.templ`
- `internal/ui/pages/logs_templ.go`
- `internal/ui/pages/admin/reconcile.templ`
- `internal/ui/pages/admin/reconcile_templ.go`
- `internal/ui/pages/settings/user.templ`
- `internal/ui/pages/settings/user_templ.go`
- `internal/ui/pages/settings/tokens.templ`
- `internal/ui/pages/settings/tokens_templ.go`
- `internal/ui/pages/settings/registries.templ`
- `internal/ui/pages/settings/registries_templ.go`
- `internal/ui/pages/settings/theme.templ`
- `internal/ui/pages/settings/theme_templ.go`
- `internal/ui/pages/settings/audit.templ`
- `internal/ui/pages/settings/audit_templ.go`
- `internal/ui/pages/settings/git_connections.templ`
- `internal/ui/pages/settings/git_connections_templ.go`
- `internal/ui/pages/settings/webhooks.templ`
- `internal/ui/pages/settings/webhooks_templ.go`
- `internal/ui/pages/settings/theme_preview.templ`
- `internal/ui/pages/settings/theme_preview_templ.go`

<!-- ALLOWLIST-END -->

The CSS inventory was inspected before writing this specification: the currently present files are `base.css`, `fonts.css`, `tokens.css`, `ui.css`, `utilities.css`, `wa-overrides.css`, `pages/logs.css`, and the current files under `components/` listed above; `alert.css`, `badge.css`, and `button.css` are not currently present but are explicitly allowlisted for deletion if they exist when execution begins.

## Out of Scope

- Changing `.air.toml`.
- Creating a backup branch.
- Resetting, reverting, checking out, or constructing an intermediate tree from commit `8ef62c7`.
- Reverting, overwriting, or otherwise removing commit `517e642` or unrelated current work.
- Dockerfile, container, CI, release, deployment-pipeline, Helm, or Kubernetes changes.
- Backend service, public API, authentication contract, worker, adapter, database, schema, or migration changes, except deleting preview-only handler code and adding focused handler tests.
- New application routes, preview routes, preview handlers, preview environment flags, committed preview fixtures, screenshot tooling, or production preview replacements.
- Creating or retaining npm, Node, package manifests, Playwright, frontend bundlers, PostCSS, daisyUI, React, Vue, or another application runtime; deletion of the existing preview-only `scripts/ui-check/package.json` and `scripts/ui-check/package-lock.json` is required by this task.
- Vendoring Tailwind Plus Elements or replacing its confirmed CDN URL.
- Icon CDNs or runtime-loaded icon libraries.
- Modifying `/Users/divy/Downloads/tailwind_plus`, copying unrelated Plus blocks, or redistributing Plus source as a component library.
- Custom page, component, table, log, scrollbar, animation, toast-keyframe, or utility CSS outside generated Tailwind output.
- New theme presets, removal of the theme settings page, or replacement of the canonical `--color-*` vocabulary.
- Realtime charts, chart libraries, SSE or WebSocket log streaming.
- Changes to the Scalar `/docs` application or its vendored assets.

## Success Criteria

1. **Clean start and preserved baseline:** Before implementation, the following exact commands succeed and `BASELINE` remains available for final verification:
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
2. **Corrected implementation plan:** The following assertions succeed:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   PLAN="$ROOT/docs/plans/tailwind-plus-elements-2026-08-13.md"
   test -f "$PLAN"
   rg -q 'directly from the current|direct migration from the current' "$PLAN"
   rg -q '517e642' "$PLAN"
   rg -q '/Users/divy/Downloads/tailwind_plus' "$PLAN"
   ! rg -n 'restore.*__theme-preview|reuse.*__theme-preview|keep getThemePreview|intermediate pre-WA|tree must be green before Phase 2|revert commits|squashed revert' "$PLAN"
   ```
3. **Pinned official Tailwind installation:** After `scripts/dev/setup.sh` completes, these exact assertions succeed:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   test -x "$ROOT/.cyclopus-dev/bin/tailwindcss"
   "$ROOT/.cyclopus-dev/bin/tailwindcss" --version | rg -q 'v?4\.3\.3'
   rg -q 'v4\.3\.3' "$ROOT/scripts/dev/setup.sh"
   rg -q 'github\.com/tailwindlabs/tailwindcss/releases/download/v4\.3\.3/' "$ROOT/scripts/dev/setup.sh"
   rg -q 'sha256|shasum -a 256' "$ROOT/scripts/dev/setup.sh"
   rg -q '\.cyclopus-dev/bin/tailwindcss' "$ROOT/scripts/dev/setup.sh"
   ```
4. **Checksum-before-install behavior:** The focused executable shell test succeeds against testable functions in `setup.sh` without starting local infrastructure or using the network:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   test -x "$ROOT/scripts/dev/setup_test.sh"
   "$ROOT/scripts/dev/setup_test.sh"
   ! rg -n 'curl .*\|\s*(sh|bash)|wget .*\|\s*(sh|bash)' "$ROOT/scripts/dev/setup.sh"
   ```
   `setup.sh` must place platform selection, checksum verification, and atomic installation in sourceable functions guarded from executing `main` when sourced; `setup_test.sh` must create a sandbox below `/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode`, invoke the mapping function for every OS/architecture pair supported by `setup.sh`, assert each result contains the exact `v4.3.3` official asset URL and a literal 64-hex-character expected checksum, seed the sandbox install path with a known executable, invoke the install function with a deliberately mismatched downloaded payload and checksum, assert a nonzero result, and use `cmp` to prove the pre-existing installed executable is unchanged before deleting the sandbox.
5. **Canonical build command:** The non-watch build succeeds with the only approved command:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   $ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify
   test -s "$ROOT/internal/ui/static/css/output.css"
   ```
6. **Canonical watcher command:** `run.sh` contains the same command with only `--watch` appended, uses the exact local binary, starts Air without changing `.air.toml`, and does not use a globally resolved Tailwind command:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   rg -Fq '$ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify --watch' "$ROOT/scripts/dev/run.sh"
   rg -q 'air -c \.air\.toml|air.*\.air\.toml' "$ROOT/scripts/dev/run.sh"
   ! rg -n 'command -v tailwindcss|(^|[[:space:]])tailwindcss[[:space:]]+-i|CYCLOPUS_UI_PREVIEW' "$ROOT/scripts/dev/run.sh"
   git diff --exit-code "$BASELINE" -- "$ROOT/.air.toml"
   ```
7. **Preview tooling deleted:** All preview tooling and package artifacts are absent:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   test ! -e "$ROOT/scripts/dev/check_ui.mjs"
   test ! -e "$ROOT/scripts/ui-check"
   test ! -e "$ROOT/scripts/ui-check/package.json"
   test ! -e "$ROOT/scripts/ui-check/package-lock.json"
   test ! -e "$ROOT/scripts/ui-check/node_modules"
   test ! -e "$ROOT/internal/ui/pages/settings/theme_preview.templ"
   test ! -e "$ROOT/internal/ui/pages/settings/theme_preview_templ.go"
   ! rg -n 'scripts/ui-check/node_modules' "$ROOT/.gitignore"
   ```
8. **Preview code fully removed:** No production code, script, templ source, generated template, or runtime configuration references the deleted preview feature; focused `*_test.go` files are excluded because Criterion 9 intentionally requests both deleted URLs:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   ! rg -n 'CYCLOPUS_UI_PREVIEW|/__theme-preview|getThemePreview|ThemePreview' \
     "$ROOT/scripts" "$ROOT/internal/ui" "$ROOT/internal/uihandler" "$ROOT/.gitignore" \
     --glob '!**/*_test.go'
   ```
9. **Exact preview-route 404s:** The named focused handler tests make unauthenticated requests to both exact paths and assert HTTP 404:
   ```sh
   ROOT=/Users/divy/Projects/cyclopus
   cd "$ROOT"
   go test ./internal/uihandler -run '^TestPreviewRoutesReturn404$' -count=1
   ```
   `TestPreviewRoutesReturn404` must independently request `/__theme-preview` and `/__theme-preview/flash` from `uihandler.New(...)` and assert `response.Code == http.StatusNotFound` for each path.
10. **Legacy CSS eliminated:** The following command prints exactly two files and the comparison succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    actual=$(find "$ROOT/internal/ui/static/css" -type f -name '*.css' | sed "s#^$ROOT/##" | sort)
    expected=$(printf '%s\n' \
      'internal/ui/static/css/input.css' \
      'internal/ui/static/css/output.css')
    test "$actual" = "$expected"
    ```
11. **Input CSS structural restriction:** The focused parser test accepts only Tailwind imports or directives, Tailwind source declarations, theme bridge declarations, and the existing local `@font-face` blocks:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^TestInputCSSContainsOnlyApprovedConstructs$' -count=1
    ```
    The test must fail on any selector or at-rule representing a component, page, toast, log viewer, scrollbar, animation, or utility declaration outside `@font-face` and Tailwind’s approved directives or theme bridge.
12. **No prohibited handwritten CSS:** These exact searches return no matches:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    ! rg -n '@keyframes|scrollbar|::-(webkit|moz)|animation-name|\.toast([[:space:]:.{#]|$)|\.log([_-]|[[:space:]:.{#]|$)|\.sidebar([[:space:]:.{#]|$)|\.card([[:space:]:.{#]|$)|\.table([[:space:]:.{#]|$)|\.button([[:space:]:.{#]|$)' \
      "$ROOT/internal/ui/static/css/input.css"
    ```
13. **Web Awesome and Font Awesome fully removed:** The following command returns no matches:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    ! rg -n '<wa-|</wa-|--wa-|webawesome|fontawesome|font-awesome|Font Awesome' \
      "$ROOT/internal/ui" \
      "$ROOT/internal/uihandler" \
      "$ROOT/scripts/dev"
    ```
14. **Layout uses exact assets:** The layout render test asserts exactly one reference to each required asset and no legacy stylesheet or WA/FA resource:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui/components -run '^TestLayoutAssetContract$' -count=1
    ```
    The test must assert `/static/theme.js`, `/static/css/output.css`, `/static/htmx.min.js`, and `https://cdn.jsdelivr.net/npm/@tailwindplus/elements@1`, assert `type="module"` on the Elements script, and reject `/static/css/ui.css`, Web Awesome, Font Awesome, and external icon URLs.
15. **Exact embedded asset URLs and cache header:** These exact tests request both application asset URLs through `ui.StaticHandler()` and assert success and the exact cache value:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^(TestStaticOutputCSSCacheHeader|TestStaticThemeJSCacheHeader)$' -count=1
    ```
    `TestStaticOutputCSSCacheHeader` must request `/static/css/output.css`, and `TestStaticThemeJSCacheHeader` must request `/static/theme.js`; each must assert HTTP 200 and `Cache-Control == "public, max-age=86400"`.
16. **All required wrappers exist:** These exact symbol checks succeed:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    for name in Dialog Dropdown Select Combobox Tabs Disclosure Popover CommandPalette Tooltip Toast Flash Table Badge Alert EmptyState; do
      rg -q "^(templ|func)[[:space:]]+$name\\b" "$ROOT/internal/ui/components/"*.templ "$ROOT/internal/ui/components/"*_templ.go
    done
    ```
17. **Wrapper tag, ID, and ARIA contracts:** The following focused test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui/components -run '^TestWrapperMarkupContracts$' -count=1
    ```
    The test must render every wrapper and assert unique caller-provided IDs, trigger-to-panel `aria-controls`, active or expanded `aria-selected`/`aria-expanded` values, label relationships through `for` or `aria-labelledby`, `role="dialog"` and `aria-modal="true"` for Dialog, `role="tablist"`/`role="tab"`/`role="tabpanel"` for Tabs, `role="tooltip"` for Tooltip, `role="status"` or `role="alert"` for Toast and Flash according to kind, semantic `<table>` markup for Table, and non-empty accessible text for Badge, Alert, and EmptyState.
18. **Elements-backed wrapper contracts:** This exact test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui/components -run '^TestElementsWrapperTags$' -count=1
    ```
    The test must assert the expected Tailwind Plus Elements custom-element tags and trigger relationships for Dialog, Dropdown, Select, Combobox, Tabs, Disclosure, Popover, CommandPalette, and Tooltip, and must assert that Toast, Flash, Table, Badge, Alert, and EmptyState do not depend on unavailable WA elements.
19. **Inline Heroicons only:** The following assertions succeed:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    test -f "$ROOT/internal/ui/components/icons.templ"
    rg -q '<svg' "$ROOT/internal/ui/components/icons.templ"
    rg -q 'viewBox=' "$ROOT/internal/ui/components/icons.templ"
    rg -q 'aria-hidden=' "$ROOT/internal/ui/components/icons.templ"
    ! rg -n 'heroicons.*(cdn|unpkg|jsdelivr)|unpkg\.com/.+heroicons|cdn\.jsdelivr\.net/.+heroicons' "$ROOT/internal/ui"
    cd "$ROOT"
    go test ./internal/ui/components -run '^TestInlineHeroiconContracts$' -count=1
    ```
20. **Tailwind Plus source safety:** A post-implementation SHA-256 manifest exactly matches the pre-implementation manifest created in Criterion 1, proving that no file was added, removed, or modified in the licensed source directory:
    ```sh
    test -d /Users/divy/Downloads/tailwind_plus
    test -s "$TAILWIND_PLUS_BASELINE_MANIFEST"
    TAILWIND_PLUS_FINAL_MANIFEST="$TEMP_PARENT/tailwind-plus-after.sha256"
    find /Users/divy/Downloads/tailwind_plus -type f -print0 | sort -z | xargs -0 shasum -a 256 > "$TAILWIND_PLUS_FINAL_MANIFEST"
    cmp "$TAILWIND_PLUS_BASELINE_MANIFEST" "$TAILWIND_PLUS_FINAL_MANIFEST"
    ! find "$ROOT" -path "$ROOT/.git" -prune -o -type f -print | rg '/tailwind_plus/'
    rm -f "$TAILWIND_PLUS_BASELINE_MANIFEST" "$TAILWIND_PLUS_FINAL_MANIFEST"
    ```
21. **Eight exact themes:** The named test parses `theme.js` and asserts exactly the confirmed presets, with no additional preset:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^TestThemePresetNames$' -count=1
    ```
    The sorted expected set must be exactly `console`, `ember`, `graphite`, `mono`, `obsidian`, `paper`, `solar`, and `terminal`.
22. **Canonical theme vocabulary:** These exact assertions succeed:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    rg -q -- '--color-base-100' "$ROOT/internal/ui/static/theme.js"
    rg -q -- '--color-base-content' "$ROOT/internal/ui/static/theme.js"
    rg -q -- '--color-primary' "$ROOT/internal/ui/static/theme.js"
    rg -q -- '--color-success' "$ROOT/internal/ui/static/theme.js"
    rg -q -- '--color-warning' "$ROOT/internal/ui/static/theme.js"
    rg -q -- '--color-error' "$ROOT/internal/ui/static/theme.js"
    ! rg -n -- '--wa-|wa-dark|wa-light' "$ROOT/internal/ui/static/theme.js"
    cd "$ROOT"
    go test ./internal/ui -run '^TestThemeCanonicalTokenContract$' -count=1
    ```
23. **Theme persistence and remembered modes:** The exact focused tests succeed:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^(TestThemeLocalStoragePersistence|TestThemeRemembersDarkAndLightSelections|TestThemeDefaultsToObsidianAndPaper|TestThemeHTMXResync)$' -count=1
    ```
    The tests must assert the persisted current preset key, separate remembered dark and light selections, `obsidian` as the dark fallback, `paper` as the light fallback, and re-synchronization after `htmx:load`.
24. **Theme settings page retained:** This exact render test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^TestThemeSettingsRendersAllEightPresets$' -count=1
    ```
    The test must assert `/settings/theme` remains linked from navigation and that the page renders controls for all eight exact preset names with `aria-pressed`.
25. **Flash cookie set on redirect:** This exact handler test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/uihandler -run '^TestFlashCookieSetOnRedirect$' -count=1
    ```
    The test must POST a representative mutation, assert the expected clean `Location` without flash query parameters, assert one `cyclopus_flash` cookie, and assert `HttpOnly`, a non-empty encoded message, and the expected redirect status.
26. **Flash rendered once and cleared:** This exact handler test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/uihandler -run '^TestFlashCookieRenderedOnceAndCleared$' -count=1
    ```
    The test must issue the redirected GET with the cookie, assert the message appears exactly once in rendered HTML, assert a clearing cookie with `MaxAge == -1`, issue a second GET without the original cookie, and assert the message is absent.
27. **HTMX fragment contracts:** These exact tests succeed:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/uihandler -run '^(TestEnvironmentStatusHTMXFragmentContract|TestFormErrorHTMXFragmentContract)$' -count=1
    ```
    The environment-status test must request the exact status-fragment route with `HX-Request: true`, assert HTTP 200, assert the expected fragment root ID and polling attributes, and reject a full `<html>` document; the form-error test must assert the expected targetable error fragment, status, and preserved submitted values.
28. **Select and form field contracts:** This exact render test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/uihandler -run '^TestSelectAndFormFieldContracts$' -count=1
    ```
    The test must assert the unchanged names and representative values used by current handlers, including `email`, `password`, `bootstrap_token`, `name`, `slug`, `avatar_key`, `dry_run=true`, and `prune=true`, and must assert that selected values are represented by successful form controls.
29. **Page-level HTMX attributes preserved:** This exact UI test succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    go test ./internal/ui -run '^TestPageHTMXContracts$' -count=1
    ```
    The test must assert the current `hx-get`, `hx-post`, `hx-trigger`, `hx-target`, and `hx-swap` contracts used for boosted navigation, environment polling, and server-rendered fragments.
30. **Temporary fixture creation from production wrappers and copied assets:** Browser verification begins with these exact shell operations, and the focused test renders the fixture by calling every production wrapper rather than duplicating wrapper markup:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    TEMP_PARENT=/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode
    mkdir -p "$TEMP_PARENT"
    FIXTURE_ROOT=$(mktemp -d "$TEMP_PARENT/cyclopus-tailwind.XXXXXX")
    cp "$ROOT/internal/ui/static/css/output.css" "$FIXTURE_ROOT/output.css"
    cp "$ROOT/internal/ui/static/theme.js" "$FIXTURE_ROOT/theme.js"
    cd "$ROOT"
    CYCLOPUS_BROWSER_FIXTURE_OUTPUT="$FIXTURE_ROOT/index.html" \
      go test ./internal/ui/components -run '^TestWriteBrowserFixture$' -count=1
    test -s "$FIXTURE_ROOT/output.css"
    test -s "$FIXTURE_ROOT/theme.js"
    test -s "$FIXTURE_ROOT/index.html"
    ```
    `internal/ui/components/browser_fixture_test.go` must define `TestWriteBrowserFixture`, require `CYCLOPUS_BROWSER_FIXTURE_OUTPUT` to be an absolute path below `/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode`, render a complete standalone document through templ, and directly call the production `Dialog`, `Dropdown`, `Select`, `Combobox`, `Tabs`, `Disclosure`, `Popover`, `CommandPalette`, `Tooltip`, `Toast`, `Flash`, `Table`, `Badge`, `Alert`, and `EmptyState` wrappers plus production inline icon helpers; the helper may add fixture-only controls and output elements around those calls but must not reimplement their markup.
31. **Temporary fixture HTML contract:** The generated `$FIXTURE_ROOT/index.html` contains all required wrapper examples and these exact external and local references:
    ```sh
    test -s "$FIXTURE_ROOT/index.html"
    test "$(rg -o 'href="/output\.css"' "$FIXTURE_ROOT/index.html" | wc -l | tr -d ' ')" = "1"
    test "$(rg -o 'src="/theme\.js"' "$FIXTURE_ROOT/index.html" | wc -l | tr -d ' ')" = "1"
    test "$(rg -o 'src="https://cdn\.jsdelivr\.net/npm/@tailwindplus/elements@1"' "$FIXTURE_ROOT/index.html" | wc -l | tr -d ' ')" = "1"
    rg -q 'type="module"[^>]*src="https://cdn\.jsdelivr\.net/npm/@tailwindplus/elements@1"|src="https://cdn\.jsdelivr\.net/npm/@tailwindplus/elements@1"[^>]*type="module"' "$FIXTURE_ROOT/index.html"
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    CYCLOPUS_BROWSER_FIXTURE_OUTPUT="$FIXTURE_ROOT/index.html" \
      go test ./internal/ui/components -run '^TestWriteBrowserFixture$' -count=1
    ```
    The HTML must provide stable IDs `theme-obsidian`, `theme-paper`, `dialog-trigger`, `dialog-panel`, `dropdown-trigger`, `dropdown-menu`, `select-control`, `combobox-input`, `tabs-list`, `disclosure-trigger`, `popover-trigger`, `command-trigger`, `tooltip-trigger`, `toast-trigger`, and `toast-region`; the test must fail before writing if any production wrapper call is omitted.
32. **Loopback-only fixture server:** The fixture is served on an available port using the exact server command:
    ```sh
    PORT=$(python3 - <<'PY'
    import socket
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
    s.close()
    PY
    )
    python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$FIXTURE_ROOT" >"$FIXTURE_ROOT/http.log" 2>&1 &
    FIXTURE_SERVER_PID=$!
    curl --fail --silent --show-error "http://127.0.0.1:$PORT/" >/dev/null
    curl --fail --silent --show-error "http://127.0.0.1:$PORT/output.css" >/dev/null
    curl --fail --silent --show-error "http://127.0.0.1:$PORT/theme.js" >/dev/null
    ```
33. **OpenChamber browser interactions against production wrapper output:** Using the OpenChamber browser panel against `http://127.0.0.1:$PORT/` generated by `TestWriteBrowserFixture`, verification must perform and record all of these binary assertions:
    - Open the fixture and obtain a snapshot with no page errors.
    - Click `#theme-paper` and assert `<html data-theme="paper" data-mode="light">`.
    - Click `#theme-obsidian` and assert `<html data-theme="obsidian" data-mode="dark">`.
    - Repeat theme selection for `mono`, `ember`, `graphite`, `terminal`, `console`, and `solar`, asserting the matching `data-theme` after each click.
    - Click `#dialog-trigger`, assert `#dialog-panel` is visible, and assert focus is inside the dialog.
    - Close the dialog and assert focus returns to `#dialog-trigger`.
    - Click `#dropdown-trigger`, assert `#dropdown-menu` is visible, choose its second action, and assert the selected-action output changes to that action’s value.
    - Select the second option through `#select-control`, submit the fixture form, and assert the rendered form output contains the exact selected field name and value.
    - Type into `#combobox-input`, choose a result, and assert the corresponding hidden or successful form-control value.
    - Activate the second tab under `#tabs-list` and assert only its matching tab panel is active.
    - Click `#disclosure-trigger` and assert `aria-expanded="true"` and visible disclosure content.
    - Click `#popover-trigger` and assert the anchored popover becomes visible.
    - Click `#command-trigger`, type a query into the command input, choose a result, and assert the command output changes.
    - Hover or focus `#tooltip-trigger` through the browser panel and assert the referenced `role="tooltip"` content is visible.
    - Click `#toast-trigger`, assert a toast appears within `#toast-region`, and assert its accessible role and message.
34. **Temporary fixture cleanup:** Cleanup always stops the standalone server and deletes the fixture, after which these exact assertions succeed:
    ```sh
    kill "$FIXTURE_SERVER_PID"
    wait "$FIXTURE_SERVER_PID" 2>/dev/null || true
    rm -rf "$FIXTURE_ROOT"
    test ! -e "$FIXTURE_ROOT"
    ! curl --silent --max-time 1 "http://127.0.0.1:$PORT/" >/dev/null 2>&1
    ```
35. **No committed browser harness:** The following assertions succeed after browser verification; focused Go tests are excluded from the deleted-preview reference search because Criterion 9 intentionally exercises both exact 404 URLs:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    ! find "$ROOT" -type f \( -name 'playwright.config.*' -o -name 'package.json' -o -name 'package-lock.json' \) -not -path "$ROOT/.git/*" -print | rg .
    ! rg -n 'playwright|puppeteer|__theme-preview|CYCLOPUS_UI_PREVIEW' "$ROOT/scripts" "$ROOT/internal" --glob '!**/*_test.go'
    ```
36. **Generated templ files current:** After the first generation, checksum manifests of all changed or generated `internal/ui/**/*_templ.go` files are identical across a second generation, without comparing generated files to the baseline:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    TEMP_PARENT=/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode
    cd "$ROOT"
    templ generate
    git diff --check
    TEMPL_MANIFEST_BEFORE=$(mktemp "$TEMP_PARENT/templ-before.XXXXXX")
    TEMPL_MANIFEST_AFTER=$(mktemp "$TEMP_PARENT/templ-after.XXXXXX")
    {
      git diff --name-only --diff-filter=ACMR "$BASELINE" -- 'internal/ui/**/*_templ.go'
      git ls-files --others --exclude-standard -- 'internal/ui/**/*_templ.go'
    } | sort -u | while IFS= read -r file; do
      test -n "$file" || continue
      test -f "$file"
      shasum -a 256 "$file"
    done > "$TEMPL_MANIFEST_BEFORE"
    templ generate
    {
      git diff --name-only --diff-filter=ACMR "$BASELINE" -- 'internal/ui/**/*_templ.go'
      git ls-files --others --exclude-standard -- 'internal/ui/**/*_templ.go'
    } | sort -u | while IFS= read -r file; do
      test -n "$file" || continue
      test -f "$file"
      shasum -a 256 "$file"
    done > "$TEMPL_MANIFEST_AFTER"
    cmp "$TEMPL_MANIFEST_BEFORE" "$TEMPL_MANIFEST_AFTER"
    rm -f "$TEMPL_MANIFEST_BEFORE" "$TEMPL_MANIFEST_AFTER"
    ```
    The manifest must include every changed tracked or untracked generated file below `internal/ui`, and the second `templ generate` must leave both the file set and every file checksum unchanged.
37. **Go formatting:** The changed Go files are formatted and the check succeeds:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    changed_go=$(git diff --name-only --diff-filter=ACMR "$BASELINE" -- '*.go')
    if test -n "$changed_go"; then
      printf '%s\n' "$changed_go" | xargs gofmt -w
      test -z "$(gofmt -l $changed_go)"
    fi
    ```
38. **Tailwind output reproducibility:** Rebuilding with the canonical non-watch command produces byte-for-byte identical output:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    BEFORE=$(mktemp)
    cp "$ROOT/internal/ui/static/css/output.css" "$BEFORE"
    $ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify
    cmp "$BEFORE" "$ROOT/internal/ui/static/css/output.css"
    rm -f "$BEFORE"
    ```
39. **Full required validation:** Every required repository command exits successfully:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    templ generate
    gofmt -w $(git diff --name-only --diff-filter=ACMR "$BASELINE" -- '*.go')
    $ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify
    go build ./...
    go vet ./...
    go test ./...
    git diff --check
    ```
40. **Commit preservation:** The current final `HEAD` still contains commit `517e642`:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    git merge-base --is-ancestor 517e642 HEAD
    ```
41. **Exact changed-path allowlist:** This command compares all tracked changes and non-ignored untracked files against the allowlist embedded in this specification and exits nonzero for any unlisted path:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    python3 - "$BASELINE" "$ROOT/docs/spec-tailwind-plus-elements.md" <<'PY'
    import fnmatch
    import pathlib
    import subprocess
    import sys

    baseline, spec_path = sys.argv[1:]
    lines = pathlib.Path(spec_path).read_text().splitlines()
    start = lines.index("<!-- ALLOWLIST-START -->") + 1
    end = lines.index("<!-- ALLOWLIST-END -->")
    allowed = []
    for line in lines[start:end]:
        line = line.strip()
        if line.startswith("- `") and line.endswith("`"):
            allowed.append(line[3:-1])

    tracked = subprocess.check_output(
        ["git", "diff", "--name-only", baseline, "--"],
        text=True,
    ).splitlines()
    untracked = subprocess.check_output(
        ["git", "ls-files", "--others", "--exclude-standard"],
        text=True,
    ).splitlines()
    changed = sorted(set(tracked + untracked))

    def permitted(path):
        for rule in allowed:
            if rule.endswith("/**"):
                prefix = rule[:-3].rstrip("/")
                if path == prefix or path.startswith(prefix + "/"):
                    return True
            elif path == rule or fnmatch.fnmatchcase(path, rule):
                return True
        return False

    forbidden = [path for path in changed if not permitted(path)]
    if forbidden:
        print("Forbidden changed paths:", file=sys.stderr)
        for path in forbidden:
            print(path, file=sys.stderr)
        raise SystemExit(1)
    PY
    ```
42. **Explicit forbidden-path checks:** These exact assertions confirm that `.air.toml` and prohibited application subsystems did not change:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    test -z "$(git diff --name-only "$BASELINE" -- \
      .air.toml \
      Dockerfile \
      .github \
      go.mod \
      go.sum \
      charts \
      cmd \
      migrations \
      internal/adapters \
      internal/api \
      internal/auth \
      internal/server \
      internal/store \
      internal/worker)"
    ```
43. **Final repository scope and cleanliness:** The final checks succeed, allowing only the approved implementation diff:
    ```sh
    ROOT=/Users/divy/Projects/cyclopus
    cd "$ROOT"
    git diff --check
    test -z "$(git status --porcelain --ignored=no | awk '$1 == "??" && $2 ~ /^\/var\/folders\// {print}')"
    ```

## Parallel Groups

- Steps 1, 2: sequential
- Step 3: sequential (owns `scripts/dev/setup.sh`, `scripts/dev/setup_test.sh`, `scripts/dev/run.sh`, preview handler removal, preview files, `.gitignore`, and all old preview tooling; `.air.toml` remains untouched)
- Steps 4, 5: parallel-group foundation after Step 3; Step 4 owns only `internal/ui/static/theme.js`, `internal/ui/static/css/**`, and `internal/ui/theme_tokens_test.go`, while Step 5 owns only `internal/ui/embed.go` and `internal/ui/embed_test.go`
- Step 6: sequential after parallel-group foundation; first runs the shared Tailwind/theme and asset-serving verification gate, then defines the shared inline icon API required by wrappers and pages
- Steps 7, 8, 9: parallel-group A after Step 6; each step owns at most three disjoint wrapper source/generated pairs
- Steps 10, 11: parallel-group B after Step 6; each step owns at most three disjoint wrapper source/generated pairs and has no shared files with group A
- Step 12: sequential after parallel-groups A and B (owns shared `layout.templ`, `nav.templ`, and generated files)
- Steps 13, 14, 15: parallel-group C after Step 12; each step owns a disjoint page family and its generated files
- Steps 16, 17: parallel-group D after parallel-group C; Step 16 owns `internal/ui` and `internal/ui/components` test files, while Step 17 owns only `internal/uihandler` test files
- Step 18: sequential after parallel-group D
- Step 19: sequential after Step 18
