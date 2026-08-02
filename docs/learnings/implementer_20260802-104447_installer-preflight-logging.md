## Phase
Phase 1 (A) — installer preflight hardening: bash conversion, install log file, disk-space pre-check, IPv4 validation. Commit 7bf87b1.

## What Was Done
- scripts/install.sh (+49/−14): #!/bin/sh → #!/bin/bash, `set -euo pipefail`, header one-liner `| sh` → `| bash`
- LOG_FILE=/var/log/cyclopus-install-<ts>.log with `exec > >(tee -a "$LOG_FILE") 2>&1`; startup prints log path; fail() prints it guarded by ${LOG_FILE:-}
- Root check + "Checking prerequisites..." moved BEFORE the tee redirect (non-root runs must not spawn tee against /var/log)
- Disk pre-check after arch detection: fail <10G avail, warn <20G avail or <30G total, df -PBG (POSIX one-line mode), parse-failure tolerant
- IPv4 validation after PUBLIC_IP detection: regex + octet ≤255 in one condition, hoisted NOT_IPV4_MSG, fail says "IPv6-only hosts are not supported"
- pipefail-safety `|| true` inside $() pipelines: assert_chart_version, NODE_IP detection, helm_deployed, NODE_COUNT (+ guard)
- shellcheck hygiene: SC1091 disables for /etc/os-release + staged common.sh, removed VERSION_ID self-assign, quoted postgresql-"$PG_MAJOR"

## Key Decisions
- tee WITHOUT `</dev/null`: process substitution already wires tee's fd 0 to the pipe; adding </dev/null overrides that and silently kills all logging. An adversarial finding claimed tee would race bash for script stdin under `curl | bash` — empirically disproved; comment in code documents why.
- df -PBG not -BG: Ubuntu Server LVM default device names (/dev/mapper/ubuntu--vg-ubuntu--lv) wrap df output to two lines, breaking awk NR==2 parsing.
- Pre-existing shellcheck info warnings fixed in passing (SC1091/SC2269/SC2086) to make the DoD "shellcheck exits 0" achievable.

## Findings Fixed
- Root check ordering (non-root run would SIGPIPE silently against /var/log)
- df LVM wrap gotcha
- helm_deployed pipefail landmine
- NODE_COUNT pipefail landmine (kubectl fail → silent set -e abort; caveat: wc emits "0" so guard delegates to the existing CP_NODES check two lines later)
- Duplicated IPv4 fail message collapsed

## DoD Results
- bash shebang + pipefail + header | bash: PASS
- bash -n + shellcheck exit 0: PASS
- Disk check 3 branches + parse tolerance: PASS
- IPv4 validation + IPv6 message: PASS
- LOG_FILE + tee + fail() path: PASS
- No other files modified: PASS

## Watch Out For
- `set -o pipefail` retroactively arms EVERY pipeline in the script: any `$(... | grep/head ...)` without `|| true` is a silent-abort landmine. Sweep when adding new pipelines.
- The log file misses everything printed before the exec redirect (root check, "Checking prerequisites...") — by design.
- Smoke-testing installer logic: extract the block into /tmp with mocks; `cat scripts/install.sh | bash` reproduces the curl-pipe path locally.
- Commit convention in this repo: explicit paths, never `git add -A`.
