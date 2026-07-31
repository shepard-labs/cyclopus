# Phase-3 Completion & Any-VPS Genericization — Durable Workflow Ledger

**Purpose.** Restart-safe execution ledger for the effort that (1) completes the
partially-implemented Phase 3 (isolation/networking/domains/certs), (2) removes
provider-specific OpenTofu/OVH infrastructure in favor of a bring-your-own-VPS
(Coolify/Dokploy-style) install, and (3) closes the missing §6 v1 API surface.

**Resumption contract.** On restart, read this ledger and `git log --oneline`.
A workstream whose row is `committed` with a matching commit hash is DONE — do
not re-run it. Resume at the first row that is not `committed`. Each workstream
is landed as one commit after its own tests pass and adversarial review findings
are fixed.

**Per-phase conditions (all mandatory).**
1. Implementation delegated to a specialized subagent.
2. Tests added/updated and passing before the phase is considered done.
3. Work is durable: this ledger + git commits are the recovery points; subagent
   failures / network errors / restarts resume from the last committed row.
4. Adversarial review runs after each phase; findings are fixed before commit.
5. One commit per phase.

## Execution order & status

| # | Workstream | Impl subagent | Reviewer subagent | Status | Commit |
|---|---|---|---|---|---|
| A | Remove OpenTofu/OVH; genericize to any-VPS (spec + infra + workflow defs) | general | production-readiness review | committed | 3acee33 |
| C | Node public-IP egress exclusion (config + provisioner + reconcile parity) | general | tenant-escape review | committed | 38e7862 |
| B | Kyverno fail-closed ClusterPolicies (chart) | general | tenant-escape review | committed | 0c13e27 |
| D | Domain verification API (§7.5) | general | api-security review | committed | cd51a6c |
| E | Registries + Git connections API (§6) | general | api-security review | committed | 604916e |
| F | Read-only APIs: Aliases / Certs / Audit (§6) | general | api-security review | committed | cf38fc5 |
| G | Phase-3 checkpoint + evidence; phases.yaml deliverables | general | — | committed | a8b5aa8 |
| H | Full gate run + regenerate evidence | general | — | committed | (final) |

Status values: `pending` -> `in-progress` -> `review` -> `committed`.

## Verification gates (run per phase where applicable)
- `go build ./...`
- `go vet ./...`
- `go test ./...` (or affected packages)
- `helm template charts/cyclopus` (phases touching the chart)
- `./scripts/validate_workflow.sh` (phase A)
- `./scripts/test_infrastructure.sh` (phase A)
- `buf generate` (API phases D/E/F)

## Decisions locked with the user
- Install model: bring-your-own SSH-accessible 64-bit Linux VPS with nested
  virtualization (required for gVisor `runsc`); bootstrap script + Helm. No
  cloud-API provisioning. OpenTofu/OVH removed entirely.
- DR hostname survival is best-effort: default hostnames survive only if the
  operator holds a movable IP (cloud floating IP / keepalived VIP / BGP);
  otherwise they may change and reconcile emits the old->new mapping. Custom
  domains are the durable path. Keep the existing node-local `floating_ip.sh`;
  document VIP options in the spec, do NOT add a keepalived helper this pass.
- Integration profiles: replace the two OVH profiles with a single generic
  `byo_vps` profile (constrained 2 vCPU / 2048 MiB / 20 GiB).
- Domain DNS verification uses the Go stdlib resolver, configurable for tests.
- Node public IPs for the egress exclusion come from install-time config
  (operator-supplied), not live cluster discovery.
- §6 API scope: build Domains + all missing surfaces (GitConnections,
  Registries, Aliases-read, Certs-read, AuditEvents-read).
- UI (templ/htmx/Tailwind) remains out of scope (`run.yaml` excludes it).
