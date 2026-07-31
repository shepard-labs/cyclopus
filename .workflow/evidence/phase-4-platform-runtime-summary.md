# Phase 4 Platform Runtime Evidence

Status: complete.

Source revision: 366371732c82e3323239f58f5ca917d8200c664e

## Slices delivered

- **Durable outbound webhooks** (`5e64825`): transactional outbox for six
  lifecycle events, stable HMAC-SHA256 signatures over a persisted first-send
  timestamp and raw body, fenced SKIP LOCKED claims, initial send plus six
  retries, endpoint auto-disable at 20 exhausted deliveries, SSRF guards,
  restart recovery, and a session-authenticated CRUD API.
- **HPA/KEDA scaling** (`0a99c8b`): KEDA v2.20.1 cron `ScaledObject` and HTTP
  add-on v0.15.0 `HTTPScaledObject` with interceptor routing, exactly-one
  controller transitions, `<app>-cron` naming, snapshot v3 with v2 backward
  compatibility, robfig/cron validation, and preview one-replica pinning.
- **Scoped reconciliation** (`ab753f5`): org/app/environment scopes from the
  immutable live release snapshot, dry-run plan and server-side apply,
  opt-in orphan prune, RuntimeClass and NetworkPolicy drift, lease-fenced
  evidence, and single-flight per scope.
- **Observability** (`3663717`): stable §8.2 structured events, tenant-scoped
  OpenObserve log/trace query API with a mandatory server-built tenant filter,
  operator-level Beszel metrics, and bounded HTTP clients with response caps.

## Passed evidence

- `go test ./...`: 435 tests.
- `go test -race -tags integration ./...`: 435 tests (PostgreSQL 18.4).
- `go vet ./...`: passed.
- `scripts/verify_generation.sh`: passed.
- `scripts/validate_workflow.sh`: passed — 6 phases, 23 audited version decisions.
- Focused gate `Scaling|Webhook|Telemetry|Reconcile`: passed.
- Adversarial review per slice: webhooks (C1,H1-H3,M1-M5,L2-L4),
  scaling (H1-H6,M1), reconciliation (H1-H3,M1-M4), observability (MEDIUM-1) —
  all critical/high findings fixed and gates rerun before commit.

## Version decisions recorded

- KEDA v2.20.1, KEDA HTTP add-on v0.15.0, robfig/cron v3.0.1,
  OpenTelemetry Collector v0.157.0, OpenObserve v0.91.5, Beszel v0.18.7.

## Documented deferrals

- `cert.issue_failed` (§8.2): certificate issuance is cert-manager-owned via
  HTTP-01; the v1 Go control plane has no issuance-observation hook, so the
  event is deferred to a Phase 5+ cert-manager status watcher rather than
  fabricated.
