# Learnings — install-as-upgrade converge

## Phase
Phase 3 (C) — install-as-upgrade: component helm charts converge to versions.env pins instead of failing. Commit 7b0d7a1.

## What Was Done
- scripts/install.sh (+161/−64): installed_chart_version + converge_chart helpers (0 at-pin / 1 not-deployed / 2 needs-upgrade / fail-unreadable); five component blocks (traefik, kyverno, cert-manager, keda, keda-add-ons-http) restructured to install / skip / helm-upgrade-to-pin
- TRAEFIK_SET_ARGS + KEDA_HTTP_SET_ARGS bash arrays shared by install and upgrade
- assert_chart_version kept as post-action verification at all five sites, now delegating parsing to installed_chart_version
- Upgrades: identical value flags as install (no --reuse-values), --cleanup-on-fail on all five, downgrade warn via sort -V

## Key Decisions
- No --reuse-values: install and upgrade pass identical value flags so the two paths can't drift apart when flags change
- keda-add-ons-http re-applies its --set block (config-as-code) rather than trusting release history
- helm list json parsed with anchored grep `"name":"REL"[^}]*"chart":"REL-..."` — no jq dependency on target hosts; [^}]* can't cross release objects because helm emits compact single-line json and intermediate fields contain no braces
- Cyclopus app release block (install vs upgrade --atomic) deliberately untouched

## Findings Fixed
- keda/keda-add-ons-http grep collision (same namespace, prefix name) — was alphabetical-order luck
- --reuse-values divergence trap between install and upgrade paths
- No --cleanup-on-fail on upgrades (failed --wait would strand release until uninstall-retry)
- Silent downgrades now warn (installed > pin)
- Duplicated fragile grep/sed parsing deduped into one helper

## DoD Results
- converge_chart three-way return + unreadable fail: PASS
- Five sites restructured, fresh-install flags unchanged, assert kept: PASS
- Traefik --set factored to array: PASS
- Cyclopus app block untouched: PASS
- bash -n + shellcheck exit 0: PASS
- No other files: PASS
- Adversarial re-review: REVIEW CLEAN

## Watch Out For
- Version comparison is string equality; today all pins match helm's chart-version rendering (cert-manager pin keeps its `v`). A pin/rendering drift → converge→upgrade→assert fails LOUDLY (not a loop) — correct failure mode, don't "fix" with fuzzy matching.
- helm never upgrades CRDs in a chart's crds/ dir — kyverno/cert-manager pin bumps with CRD schema changes need a manual CRD apply (comment at kyverno branch; out of scope).
- cert-manager chart version carries the `v` prefix; generate_versions_env.sh strips `v` only for keda — keep that asymmetry in mind when bumping pins.
- Stub-harness pattern for installer helm logic: PATH-prefixed fake `helm` recording argv + scripted `helm list/status -o json`; extract blocks verbatim, drive install/at-pin/upgrade/unreadable scenarios.
