# Phase 2 Deploy Executor Evidence

Status: complete.

- 203 Go tests pass normally and under the race detector.
- `go vet ./...`, protobuf generation verification, workflow validation, and
  PostgreSQL 18 atomic contracts pass.
- Debian 12 and Ubuntu 24.04 OrbStack profiles pass with PostgreSQL 18.4 and
  k3s `v1.33.13+k3s1`.
- Live k3s tests prove forward deployment, immutable env Secret materialization,
  Service/Deployment/Ingress/HPA application, rollout readiness, alias
  finalization, startup resume, and full expired-deploy recovery.
- Failed deployment compensation restores the complete previous desired state
  before finalization; unprovable restoration is quarantined.
- Ordinary image deploys transactionally inherit the current environment
  revision, and Services use a stable named `http` target port during mixed
  rolling revisions.
- The final concurrency/failure adversarial reviewer returned `VERDICT: PASS`.

Remaining Phase 2 scope:

- Preview environment reservation and slug allocation.
- Cross-build/deploy push coalescing entry point.
- Blocked preview namespace, quota, and limit lifecycle.
- Branch-delete and idle-expiry teardown.

Preview activation is intentionally deferred until Phase 3 supplies and verifies
the fail-closed NetworkPolicies and hostname-provider capability.
