# API Phase Evidence

Status: complete.

Passed evidence:

- `go test ./...`: 169 tests.
- `go test -race ./...`: 169 tests.
- `go vet ./...`: passed.
- `scripts/verify_generation.sh`: passed.
- `tests/workflow/validate_workflow_test.sh`: 54 assertions passed.
- PostgreSQL 18.4 integration for auth, service, server, and store: passed.
- Debian 12 OrbStack API gate: passed with PostgreSQL 18.4, the latest
  compatible stable Go release (`1.26.5`), integration tests, race tests, vet,
  and protobuf generation verification.
- API contract adversarial review: passed.
- API acceptance adversarial review: passed.
- GitHub recovery adversarial review: passed.
- Kubernetes recovery adversarial review: passed.
- Ubuntu and Debian OrbStack recovery scenarios: passed with PostgreSQL 18.4
  and k3s `v1.33.13+k3s1`.

Blocked evidence:

- Resolved. Replacement authentication and transport security reviewers returned
  explicit PASS verdicts after body-limit wiring, rate-limit saturation,
  readiness amplification, shutdown ordering, rollback target, secret-reference,
  and cursor-binding findings were fixed.

Review disposition:

- A tenant reviewer classified the production `users_singleton` index as a
  multi-tenancy blocker. This finding is rejected because
  `docs/decisions/phase-0-decisions.md` decision 5 and
  `workflow/phases.yaml` Phase 1 explicitly require one user in one organization
  for v1. Organization columns and predicates remain the forward-compatible
  tenancy boundary; multi-user membership and RBAC are deferred.
