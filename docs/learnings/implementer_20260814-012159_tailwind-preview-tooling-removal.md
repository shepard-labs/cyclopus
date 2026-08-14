## Phase

Phase 2 — Replace the temporary UI-preview stack with pinned Tailwind standalone tooling and permanently remove the preview endpoints.

## What Was Done

- Added a pinned Tailwind CSS standalone installer to `scripts/dev/setup.sh` for supported macOS and Linux architectures.
- Added sourceable, testable setup functions for platform selection, checksum calculation, download verification, exact version detection, approved-binary reuse, and atomic installation.
- Added `scripts/dev/setup_test.sh` coverage for the platform matrix, unsupported platforms, exact version parsing, untrusted-binary replacement, checksum-failure preservation, and successful installation.
- Updated `scripts/dev/run.sh` to validate Tailwind `v4.3.3`, run Tailwind watch and Air together, propagate child failure, and stop/reap the surviving child.
- Permanently removed the unauthenticated `/__theme-preview` and `/__theme-preview/flash` routes, handlers, environment gate, and generated preview component.
- Added `internal/uihandler/ui_render_test.go` to prove both removed preview paths return `404 Not Found` even when `CYCLOPUS_UI_PREVIEW=1`.
- Deleted the temporary Playwright UI-check stack: `scripts/dev/check_ui.mjs`, `scripts/ui-check/package.json`, and `scripts/ui-check/package-lock.json`.
- Removed the obsolete preview-related `.gitignore` entry.

## Key Decisions

- Pinned Tailwind to `v4.3.3` and selected release assets through an explicit OS/architecture matrix so setup fails clearly on unsupported platforms.
- Reused an existing Tailwind binary only when both its SHA-256 checksum and parsed version exactly match the approved release. Version output alone does not establish trust.
- Verified downloads against both repository-pinned checksums and the release checksum manifest. This prevents a changed manifest from silently approving an unexpected binary.
- Staged the verified executable beside its destination and replaced the destination atomically only after checksum, executable, and version checks passed. A failed download or verification leaves the prior binary intact.
- Guarded setup execution with `if [[ "${BASH_SOURCE[0]}" == "$0" ]]` so tests can source individual functions without provisioning the full local environment.
- Removed preview routes unconditionally rather than retaining a dormant environment switch. The regression test treats permanent `404` behavior as the contract.
- Kept the development runner in Bash and managed both children directly, avoiding a new process-supervisor dependency for two local processes.

## Findings Fixed

- Fixed a runner lifecycle race where one child could exit while the other remained alive: the runner now detects the first exit, terminates the survivor, and waits for both children.
- Fixed the simultaneous-exit race by collecting both statuses when neither child is still running and preserving a non-zero failure instead of losing it through cleanup ordering.
- Fixed signal and shell-exit cleanup gaps by trapping `EXIT`, `INT`, and `TERM`, terminating all active jobs, and reaping them to avoid leaked Tailwind or Air processes.
- Fixed insecure reuse of a same-version but unapproved executable by requiring its checksum to match the pinned platform asset before reuse.
- Fixed partial-install risk by verifying a staged download before moving it over an existing binary and cleaning temporary download artifacts on failure.
- Fixed the possibility of preview endpoints returning after an environment toggle by deleting their registration and asserting permanent `404` responses with the old toggle enabled.

## DoD Results

- [x] PASS — Tailwind standalone tooling is pinned, platform-specific, and checksum verified.
- [x] PASS — An existing binary is reused only when its checksum and exact version are approved.
- [x] PASS — Setup logic is sourceable and covered through testable functions.
- [x] PASS — Tailwind watch and Air share a coordinated lifecycle without leaving a surviving child behind.
- [x] PASS — Preview routes and their environment-gated registration are permanently removed.
- [x] PASS — A regression test proves both former preview endpoints return `404 Not Found`.
- [x] PASS — Temporary Playwright and Node UI-check tooling is deleted.
- [x] PASS — `bash scripts/dev/setup_test.sh` passed all six test groups.
- [x] PASS — `bash -n scripts/dev/setup.sh scripts/dev/run.sh scripts/dev/setup_test.sh` passed.
- [x] PASS — `go test ./internal/uihandler` passed.
- [x] PASS — `go test ./...` passed: 441 tests across 23 packages.
- [x] PASS — Adversarial review completed clean after the lifecycle, reuse, atomic-install, and permanent-404 corrections.

## Watch Out For

- A Tailwind upgrade requires changing the pinned version, every platform checksum, setup tests, and the runner's required version together.
- GitHub release checksum formatting is part of the verification contract: the approved manifest entry must be `<hash>  ./<asset>`.
- Tailwind emits more than one historical `--help` version prefix; parsing accepts only the known line forms and still requires an exact version match.
- Do not weaken approved-binary reuse to a version-only check. A locally replaced executable can report the expected version while containing different bytes.
- Keep `setup.sh` source-safe. New top-level provisioning commands would make the function tests mutate developer machines.
- Changes to runner cleanup must preserve both first-exit handling and the simultaneous-exit branch; testing only the common one-child-first path can reintroduce status-loss races.
- Do not restore `CYCLOPUS_UI_PREVIEW`; the former routes must remain absent in every environment.
