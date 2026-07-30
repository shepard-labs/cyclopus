# Phase 2 Deploy Executor Evidence

Status: complete.

- 214 Go tests pass normally and under the race detector.
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
- Preview reservation uses globally non-reused generated slugs with more than
  36 bits of output space, stable branch identity, app-level caps, and atomic
  build/deploy coalescing.
- Provisioning creates a policies-pending namespace, one-pod ResourceQuota and
  LimitRange, both required NetworkPolicies, and removes the pending label only
  after policy read-back and durable readiness. Failed durable completion
  restores the pending label.
- Preview hostnames use an operator-configured DNS suffix. `sslip.io` remains
  disabled because the PSL launch-block probe confirms it is absent.
- Teardown handles branch deletion, operator requests, and idle expiry; it
  drains queued/leased work, continues active GitHub/deploy recovery to terminal
  state, verifies namespace ownership, and deletes with a UID precondition.
- Lifecycle mutations are request-bound for idempotency and write atomic audit
  events. Failed previews reject new pushes and are eligible for idle reaping.
- Debian 12 API security and deploy gates passed. The deploy gate exercised
  PostgreSQL 18.4, k3s `v1.33.13+k3s1`, live forward deployment, startup resume,
  and expired recovery.

Phase 4 dependency:

- Signed GitHub push/delete webhook ingestion must derive app, branch, commit,
  and delivery identity before these internal lifecycle operations are exposed
  as automatic repository events.
- The specification's durable per-`(app, branch)` hourly dispatch ceiling is
  implemented with the webhook/runtime slice rather than the environment saga.
