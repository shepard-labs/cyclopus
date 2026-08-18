# GitHub Webhook Routing — Master Definition of Done

Status: **PASS**

Approved specification: [`2026-08-17-github-webhook-routing.md`](2026-08-17-github-webhook-routing.md)

Operator documentation: [`../github-app.md`](../github-app.md), [`../operations.md`](../operations.md)

This artifact maps all 26 steps of the approved Phase 5 approach and all 27 success criteria to final repository evidence. The implementation landed across ultracode commits labeled phases 5–14; those labels are implementation checkpoints for this approved Phase 5 feature, not new product phases. Verification also includes the approved gate fixes still in the current tree; their final commit hash is not yet known.

## Implementation commits and migrations

| Checkpoint | Commit | Primary result |
|---|---|---|
| ultracode phase 5 | `efef5cb` | Durable encrypted webhook inbox and migration 10. |
| ultracode phase 6 | `0b16988` | Strict signed HTTP ingress. |
| ultracode phase 7 | `cb2ebcf` | Lifecycle/push reconciliation and migration 11. |
| ultracode phase 8 | `d4b94e9` | Delivery and intent workers. |
| ultracode phase 9 | `9a0dfff` | Operational hardening, metrics, readiness, migration 12. |
| ultracode phase 10 | `e459eb5` | Authority-call fencing and migration 13. |
| ultracode phase 11 | `f1aff7a` | Durable batched fan-out and migration 14. |
| ultracode phase 12 | `2fd0138` | Ordering, fan-out, and lifecycle integration matrices. |
| ultracode phase 13 | `12e706c` | Operator documentation and final DoD mapping. |
| ultracode phase 14 | `59d2e6c` | Transaction retries, distinct rotation version/ref validation, and an actual readiness HMAC probe. |
| approved gate fixes | current tree; final commit pending | Internal transaction-retry coverage, exhaustive PostgreSQL 18 retry/fan-out evidence, UI typography correction, and `gofmt` cleanup. |

Migrations apply in order:

1. `internal/store/migrations/000010_github_webhook_routing.{up,down}.sql`
2. `internal/store/migrations/000011_github_unverified_revocation.{up,down}.sql`
3. `internal/store/migrations/000012_github_webhook_signature_key_version.{up,down}.sql` adds persisted HMAC signature-key version metadata, validates its shape, backfills `legacy`, and blocks rollback while non-legacy metadata remains.
4. `internal/store/migrations/000013_github_webhook_conflict_error.{up,down}.sql` adds `github_conflict` to the durable delivery error taxonomy and blocks rollback while that error code remains.
5. `internal/store/migrations/000014_github_routing_fanout.{up,down}.sql`

## Approach mapping

| Step | Status | Final evidence |
|---:|:---:|---|
| 1 | PASS | Migration 10 defines deliveries, routing state, intents, constraints, retention, and guarded down migration; persistence is in `internal/store/github_webhooks.go`; migration coverage is in `internal/store/migrate_integration_test.go` and `internal/store/github_webhooks_integration_test.go`. |
| 2 | PASS | Partial due/lease/retention indexes are in migration 10; fan-out indexes are in migration 14; bounded claims are in `internal/store/github_webhooks.go`; plan/cardinality coverage is in `internal/store/github_webhooks_integration_test.go` and `internal/store/github_fanout_integration_test.go`. |
| 3 | PASS | Atomic receive/replay/conflict, claim, renewal, epoch fencing, retry, completion, reclaim, and cleanup are implemented in `internal/store/github_webhooks.go`; concurrent ownership/replay tests are in `internal/store/github_webhooks_integration_test.go`. |
| 4 | PASS | Claim-owned attempt increments and claim-12 exhaustion are enforced in `internal/store/github_webhooks.go` and `internal/worker/github_events.go`; covered by `internal/store/github_webhooks_integration_test.go`, `internal/store/github_fanout_integration_test.go`, and `internal/worker/github_events_test.go`. |
| 5 | PASS | Exact ingress, active/previous HMAC, key-version, worker, retry, fan-out, and retention configuration lives in `internal/config/config.go`; active and previous key versions and secret references must each be distinct. Keyring loading is in `cmd/cyclopus/main.go`; validation/rotation coverage is in `internal/config/config_test.go`, `cmd/cyclopus/main_test.go`, and webhook server/store tests. |
| 6 | PASS | `/webhooks/github` is mounted before UI/gateway routing in `internal/server/server.go`; listener limits are in `internal/config/config.go`; handler semaphore/rate limiting is in `internal/server/github_webhook.go`; bounded server/worker shutdown is in `internal/server/server.go`, `internal/worker/github_events.go`, and `cmd/cyclopus/main.go`. |
| 7 | PASS | Header, method, media, encoding, framing, body, duplicate-JSON, and raw-request rejection are implemented in `internal/server/github_webhook.go`; adversarial coverage is in `internal/server/github_webhook_test.go` and `internal/server/github_webhook_integration_test.go`. |
| 8 | PASS | Canonical payload extraction and raw-byte digest/storage are in `internal/server/github_webhook.go` and `internal/store/github_webhooks.go`; schema and persistence tests are in the corresponding server/store integration tests. |
| 9 | PASS | Fixed status mapping is in `internal/server/github_webhook.go`; exhaustive status and leakage assertions are in `internal/server/github_webhook_test.go` and `internal/server/github_webhook_integration_test.go`. |
| 10 | PASS | Stable installation/repository identity, rename/transfer rules, quarantine, and tombstones are in `internal/store/github_provider.go` and `internal/store/github_event_routing.go`; covered by `internal/store/github_provider_integration_test.go`, `internal/service/github_discovery_integration_test.go`, and lifecycle matrix tests. |
| 11 | PASS | Event/action processing and `unverified_revocation` reconciliation are in `internal/service/github_events.go` and `internal/store/github_event_routing.go`; covered by `internal/service/github_events_test.go` and `internal/store/github_webhooks_integration_test.go`. Stateless ping is terminated at ingress in `internal/server/github_webhook.go`. |
| 12 | PASS | Authoritative validation, repository/ref locking, generation snapshots, and atomic routing state are in `internal/store/github_event_routing.go`; integration evidence is in `internal/store/github_webhooks_integration_test.go` and `internal/store/github_fanout_integration_test.go`. |
| 13 | PASS | Lifecycle/ref generation ordering is implemented in `internal/store/github_event_routing.go`; reversed-order and recreation evidence is in `internal/store/github_lifecycle_push_matrix_integration_test.go` and `internal/store/github_fanout_integration_test.go`. |
| 14 | PASS | Migration 14 and `internal/store/github_event_routing.go` implement durable manifests/candidates, bounded cursor batches, unique sibling intents, and restart-safe continuation; durable fan-out is covered by `internal/store/github_fanout_integration_test.go` and `internal/worker/github_events_integration_test.go`. |
| 15 | PASS | Production, preview, stale-HEAD, and deletion execution is in `internal/service/github_events.go` and `internal/store/github_event_routing.go`; covered by service, webhook-store, fan-out, and lifecycle matrix integration tests. |
| 16 | PASS | The stored workflow path and exact dispatch inputs remain in the existing orchestration contract; intent execution/fencing is in `internal/store/github_event_routing.go`; workflow compatibility is enforced by `internal/service/github_workflow.go` and `internal/service/github_workflow_test.go`. |
| 17 | PASS | GitHub API error classification is in `internal/adapters/github/errors.go` and event handling in `internal/service/github_events.go`; table-driven coverage is in `internal/adapters/github/adapter_test.go` and `internal/service/github_events_test.go`. `internal/store/store.go` retries internal event transactions on SQLSTATE `40001` and `40P01`; unit coverage exercises function, commit, exhaustion, domain-error, and cancellation paths, and PostgreSQL 18 integration injects both codes inside a real routing transaction. |
| 18 | PASS | Independent delivery, fan-out, and intent concurrency, lease renewal, retries, startup scans, delayed-lease accounting, and readiness are in `internal/worker/github_events.go`; lifecycle wiring is in `cmd/cyclopus/main.go`; covered by worker unit/integration and main lifecycle tests. |
| 19 | PASS | AES-GCM payload storage and bounded redaction are in `internal/store/github_webhooks.go`; keyring loading is in `cmd/cyclopus/main.go`; cleanup/replay retention tests are in `internal/store/github_webhooks_integration_test.go`; policy is in `docs/operations.md`. |
| 20 | PASS | Bounded Prometheus metrics are in `internal/githubmetrics/metrics.go`; fixed structured event/audit records are in `internal/worker/github_events.go` and `internal/store/github_event_routing.go`; metric taxonomy tests are in `internal/githubmetrics/metrics_test.go`. |
| 21 | PASS | Adversarial ingress, overload, rotation, readiness, and retention coverage is in `internal/server/github_webhook_test.go`, `internal/server/github_webhook_integration_test.go`, `internal/server/github_webhook_readiness_test.go`, and `internal/store/github_webhooks_integration_test.go`. Readiness resolves the active secret and performs an actual HMAC-SHA-256 operation rather than checking only secret length. |
| 22 | PASS | Deterministic crash/lost-owner/sibling isolation and fan-out concurrency coverage is in `internal/store/github_webhooks_integration_test.go`, `internal/store/github_fanout_integration_test.go`, `internal/worker/github_events_test.go`, and `internal/worker/github_events_integration_test.go`. |
| 23 | PASS | The exhaustive PostgreSQL 18 lifecycle/push matrix covers reversed ordering, authoritative activation, branch delete/recreate, force-push, delayed destructive events, transfer authority, crash boundaries, and delayed reclaim in `internal/store/github_lifecycle_push_matrix_integration_test.go` and `internal/store/github_fanout_integration_test.go`. |
| 24 | PASS | Runtime migration/wiring is in `cmd/cyclopus/main.go` and `internal/store/migrate.go`; rollback guards and mixed migration states are covered by `internal/store/migrate_integration_test.go` and `internal/store/migrate_test.go`; rollout/rollback procedures are in `docs/operations.md`. |
| 25 | PASS | `docs/github-app.md` and `docs/operations.md` contain App setup, subscriptions, permissions, workflow contract, key rotation, every runtime default, metrics, retention/backup, readiness/shutdown, migrations 10–14, guarded rollback, signed verification, SQL/plans, alerts, and troubleshooting; README and Helm NOTES link them. |
| 26 | PASS | Final repository checks are listed under **Verification record** below, and this artifact maps every approved approach step and success criterion. |

## Success Criteria mapping

| Criterion | Status | Passing evidence |
|---:|:---:|---|
| 1 | PASS | `internal/server/github_webhook_integration_test.go` and `internal/store/github_webhooks_integration_test.go`: commit/replay, exact digest, canonical metadata, matched key version, one row. |
| 2 | PASS | `internal/server/github_webhook_test.go`: missing/duplicate/malformed/mismatched authentication and malformed body/media/schema cases, fixed responses, zero store calls. |
| 3 | PASS | `internal/store/github_webhooks_integration_test.go`: concurrent receipt replay/conflict preserves original; server integration verifies `202/202/409`. |
| 4 | PASS | `internal/server/github_webhook_test.go` and `internal/server/github_webhook_integration_test.go`: timeout, framing, semaphore, limiter, and unsigned-flood cases. |
| 5 | PASS | `internal/config/config_test.go`, `internal/server/github_webhook_test.go`, `internal/server/github_webhook_integration_test.go`, and `internal/store/github_webhooks_integration_test.go`: distinct active/previous version and secret ref, active/previous overlap, exact signature-version recording independent of payload-encryption version, post-overlap rejection, and retained payload key versions. |
| 6 | PASS | `internal/store/github_webhooks_integration_test.go` and `internal/worker/github_events_integration_test.go`: increasing lease epochs, renewal/fencing, lost owner, reclaim, single completion. |
| 7 | PASS | `internal/store/github_webhooks_integration_test.go`, `internal/store/github_fanout_integration_test.go`, and `internal/worker/github_events_test.go`: claims consume attempts; retry/renewal do not; claim 12 terminalizes. |
| 8 | PASS | `internal/adapters/github/adapter_test.go`, `internal/service/github_events_test.go`, `internal/store/store_test.go`, and store integration tests cover GitHub HTTP classes, retry timing, malformed responses, PostgreSQL conflict/connectivity dispositions, and internal transaction retries for both `40001` and `40P01`. |
| 9 | PASS | `internal/store/github_provider_integration_test.go`, `internal/service/github_discovery_integration_test.go`, and `internal/store/github_lifecycle_push_matrix_integration_test.go`: stable identity, rename, transfer/quarantine, recreate isolation. |
| 10 | PASS | `internal/service/github_events_test.go` and server/store integration tests cover ping, five installation actions, two repository actions, push, unsupported combinations, and destructive no-match convergence. |
| 11 | PASS | Migration 11 plus `internal/store/github_event_routing.go`; reconciliation and blocked-routing cases in `internal/store/github_webhooks_integration_test.go` and lifecycle matrix tests. |
| 12 | PASS | `internal/store/github_event_routing.go` and `internal/store/github_fanout_integration_test.go`: locked eligibility snapshot, generation increments, uniqueness, atomic manifest/candidate/intent state. |
| 13 | PASS | `internal/store/github_lifecycle_push_matrix_integration_test.go`: reversed ordering, SHA away/back, deletion/recreation, and identity recreation. |
| 14 | PASS | `internal/store/github_fanout_integration_test.go` and `internal/worker/github_events_integration_test.go`: durable manifests and candidates, unique intents, bounded batches/cursor, sibling isolation, and continuation after worker restart. |
| 15 | PASS | `internal/service/github_events_test.go`, `internal/store/github_webhooks_integration_test.go`, and fan-out integration tests: production, preview, stored workflow/ref/SHA, stale HEAD. |
| 16 | PASS | `internal/store/github_webhooks_integration_test.go`, `internal/store/github_fanout_integration_test.go`, and lifecycle matrix tests: preview teardown/no-match/recreate and non-destructive production deletion. |
| 17 | PASS | `internal/store/github_event_routing.go` deterministic idempotency keys plus lost-result/idempotency assertions in `internal/store/github_fanout_integration_test.go`. |
| 18 | PASS | Ownership pins/fenced writes in service/store; two-worker and crash-boundary evidence in webhook/fan-out/worker integration tests. |
| 19 | PASS | `internal/store/github_lifecycle_push_matrix_integration_test.go` and `internal/store/github_fanout_integration_test.go`: both orderings and delayed reclaim converge to newest authoritative generation. |
| 20 | PASS | `internal/store/github_webhooks_integration_test.go`: bounded redaction and metadata-preserving replay; key/backup/access policy in `docs/operations.md`. |
| 21 | PASS | `internal/githubmetrics/metrics.go` and `internal/githubmetrics/metrics_test.go`; fixed HTTP tests and structured audit assertions in server/store integration tests. |
| 22 | PASS | `cmd/cyclopus/main_test.go`, `internal/server/github_webhook_readiness_test.go`, `internal/worker/github_events_test.go`, and worker integration tests: startup gate, active secret/DB, actual HMAC probe, cache failure/recovery, orphan scan, delayed leases, and shutdown. |
| 23 | PASS | Migrations 10–14 and `internal/store/migrate_integration_test.go`: indexes, guards, blocked down migrations, cleanup/mapping prerequisites, and successful retry. |
| 24 | PASS | Configured semaphore/worker/fan-out bounds are exercised by server, worker, webhook-store, and fan-out integration tests; accepted work remains durable in PostgreSQL. |
| 25 | PASS | `internal/service/github_workflow.go`, `internal/service/github_workflow_test.go`, and routing/orchestration store tests preserve stored path and exact `orchestration_job_id`/`commit_sha`; no repository-write implementation exists. |
| 26 | PASS | `docs/github-app.md`, `docs/operations.md`, README, and Helm NOTES cover every required operator topic and procedure. |
| 27 | PASS | This file is the required local mapping. Verification commands and results are recorded below. |

## Verification record

Executed from the repository root on 2026-08-18 against PostgreSQL 18 where a database was required:

| Command | Final result |
|---|---|
| `go test ./...` | PASS — 1,247 tests passed across all 24 packages. |
| `go test -race ./...` | PASS — 1,247 tests passed across all 24 packages. |
| `go test -count=1 -tags integration ./...` | PASS — all 24 packages passed against PostgreSQL 18. The package output reported each package's duration; the Go command did not display an aggregate test total or aggregate duration, so none is inferred here. |
| `go test -count=1 -race -tags integration ./...` | PASS — all 24 packages passed against PostgreSQL 18. The package output reported each package's duration; the Go command did not display an aggregate test total or aggregate duration. |
| `go vet ./...` | PASS. |
| `go build ./...` | PASS. |
| `gofmt -l cmd internal` | PASS — empty output in the verified current tree. |
| `git diff --check` | PASS. |
| `./scripts/verify_generation.sh` | PASS. |
| `./scripts/validate_workflow.sh` | PASS. |
| `go test -run '^$' -tags integration ./...` | PASS — integration-tag compile gate. |
| `./scripts/validate_postgres_contract.sh` | PASS against PostgreSQL 18. |

The PostgreSQL 18 runs include the exhaustive lifecycle/push and crash-boundary matrices, durable fan-out and restart behavior, and internal transaction retries injected after the first intent for SQLSTATE `40001` and `40P01`. They also retain the real PostgreSQL serializable-conflict assertion.

Documentation-specific validation:

```sh
test -s docs/github-app.md
test -s docs/operations.md
test -s docs/ultracode/2026-08-17-github-webhook-routing-dod.md
git check-ignore -v docs/github-app.md docs/operations.md docs/ultracode/2026-08-17-github-webhook-routing-dod.md
git ls-files --error-unmatch docs/github-app.md docs/operations.md docs/ultracode/2026-08-17-github-webhook-routing-dod.md
```

The documentation files are intentionally force-added because `/.gitignore` ignores `/docs` under the prior external-docs policy. Their tracked status is a final-verifier exception explicitly required for this phase.

## Residual risks

- Webhook throughput and retry latency still depend on PostgreSQL capacity, connection-pool sizing, and operator-selected worker and fan-out limits; metrics, bounded concurrency, durable queues, and readiness checks make this observable and recoverable.
- Secret rotation still requires operators to keep active and previous references and versions distinct, preserve the configured overlap window, and retain payload-decryption keys until stored payload retention expires.
- Guarded rollback of migrations 12–14 requires operators to archive, map, complete, or remove incompatible retained rows before running the corresponding down migrations.
