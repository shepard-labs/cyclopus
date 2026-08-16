#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
VALIDATOR="$ROOT/scripts/validate_workflow.sh"
assertions=0

fail() { printf '%s\n' "not ok - $1" >&2; exit 1; }
pass() { assertions=$((assertions + 1)); printf '%s\n' "ok - $1"; }

sh "$VALIDATOR" || fail "valid workflow is rejected"
pass "valid workflow is accepted"

assert_mutation_fails() {
  name=$1
  relative=$2
  expression=$3
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/cyclopus-workflow-test.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT HUP INT TERM
  cp -R "$ROOT/docs" "$ROOT/workflow" "$ROOT/scripts" "$ROOT/tests" "$ROOT/versions.yaml" "$tmp/"
  ruby -e "$expression" "$tmp/$relative"
  if sh "$tmp/scripts/validate_workflow.sh" >/dev/null 2>&1; then
    fail "$name mutation was accepted"
  fi
  rm -rf "$tmp"
  trap - EXIT HUP INT TERM
  pass "$name mutation is rejected"
}

assert_exit() {
  name=$1
  expected=$2
  shift 2
  set +e
  "$@" >/dev/null 2>&1
  actual=$?
  set -e
  test "$actual" -eq "$expected" || fail "$name returned $actual, expected $expected"
  pass "$name returns $expected"
}

assert_mutation_fails "missing unit gate" workflow/phases.yaml 'p=ARGV[0]; s=File.read(p); s.sub!(/^      unit:\n(?:        .*\n)+/, ""); File.write(p,s)'
assert_mutation_fails "missing adversarial review gate" workflow/phases.yaml 'p=ARGV[0]; s=File.read(p); i=s.index("      adversarial_review:"); j=s.index("\n\n  - id:", i); s.slice!(i, (j || s.length)-i); File.write(p,s)'
assert_mutation_fails "UI scope" workflow/phases.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("    deliverables:\n", "    deliverables:\n      - UI dashboard\n"); File.write(p,s)'
assert_mutation_fails "retry count mismatch" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("backoff_seconds: [5, 15]", "backoff_seconds: [5]"); File.write(p,s)'
assert_mutation_fails "retry overlap" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("never_retry_on: [invalid_request", "never_retry_on: [timeout, invalid_request"); File.write(p,s)'
assert_mutation_fails "missing checkpoint invalidation" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("    - versions_digest_change\n", ""); File.write(p,s)'
assert_mutation_fails "dispatch without lease requirement" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("lease_required_before_github_dispatch: true", "lease_required_before_github_dispatch: false"); File.write(p,s)'
assert_mutation_fails "observed run not persisted" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("persist_observed_run_before_reconciliation_exit: true", "persist_observed_run_before_reconciliation_exit: false"); File.write(p,s)'
assert_mutation_fails "BYO-VPS inferred default" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("kind: user_override", "kind: inferred_default"); File.write(p,s)'
assert_mutation_fails "unaudited version boolean" versions.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("    observed_latest: \"1.26.5\"", "    compatibility_reviewed: true\n    observed_latest: \"1.26.5\""); File.write(p,s)'
assert_mutation_fails "intentional pin without rationale" versions.yaml 'require "yaml"; p=ARGV[0]; d=YAML.safe_load(File.read(p), aliases: false); pair=d.fetch("components").find { |_name, component| component.dig("review", "status") == "intentional_pin" }; raise "no intentional pin found" unless pair; pair.last.fetch("review").delete("rationale"); File.write(p, YAML.dump(d))'
assert_mutation_fails "missing PSL gate" workflow/phases.yaml 'p=ARGV[0]; s=File.read(p); s.sub!(/        - id: psl-probe\n(?:          .*\n)+/, ""); File.write(p,s)'
assert_mutation_fails "wrong pending SHA RETURNING" tests/workflow/fixtures/pending_sha_pg18.sql 'p=ARGV[0]; s=File.read(p); s.sub!(/RETURNING WITH \(OLD AS old_row, NEW AS new_row\)\n\s+old_row.pending_commit_sha AS taken_sha,\n\s+new_row.pending_commit_sha AS remaining_sha/, "RETURNING pending_commit_sha AS taken_sha, pending_commit_sha AS remaining_sha"); File.write(p,s)'
assert_mutation_fails "lease SQL removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("FOR UPDATE SKIP LOCKED", "FOR UPDATE"); File.write(p,s)'
assert_mutation_fails "scope single-flight index removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight", "CREATE INDEX orchestration_jobs_scope_single_flight"); File.write(p,s)'
assert_mutation_fails "environment omitted from single-flight" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("org_id, app_id, environment_id", "org_id, app_id"); File.write(p,s)'
assert_mutation_fails "latest checkout promotion removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("desired_checkout_sha = COALESCE(pending_checkout_sha, desired_checkout_sha)", "desired_checkout_sha = desired_checkout_sha"); File.write(p,s)'
assert_mutation_fails "dispatch reconciliation state removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.gsub!("dispatch_reconciling", "leased"); File.write(p,s)'
assert_mutation_fails "observable job input removed" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("dispatch_inputs: [orchestration_job_id, commit_sha]", "dispatch_inputs: [commit_sha]"); File.write(p,s)'
assert_mutation_fails "commit SHA input removed" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("dispatch_inputs: [orchestration_job_id, commit_sha]", "dispatch_inputs: [orchestration_job_id]"); File.write(p,s)'
assert_mutation_fails "commit SHA input changed to persistence name" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("dispatch_inputs: [orchestration_job_id, commit_sha]", "dispatch_inputs: [orchestration_job_id, desired_checkout_sha]"); File.write(p,s)'
assert_mutation_fails "immutable checkout attestation removed" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("required commit_sha input; checkout commit_sha; attest HEAD equals commit_sha before build", "required commit_sha input"); File.write(p,s)'
assert_mutation_fails "ambiguous auto-redispatch enabled" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("ambiguous_dispatch_auto_redispatch: false", "ambiguous_dispatch_auto_redispatch: true"); File.write(p,s)'
assert_mutation_fails "retry trigger changed" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("retry_trigger: next_push", "retry_trigger: timer"); File.write(p,s)'
assert_mutation_fails "startup reconciliation disabled" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("run_on_startup: true", "run_on_startup: false"); File.write(p,s)'
assert_mutation_fails "periodic reconciliation removed" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("periodic_interval_seconds: 30", "periodic_interval_seconds: 0"); File.write(p,s)'
assert_mutation_fails "deploying state omitted from reconciliation policy" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("states: [dispatch_reconciling, dispatched, resolving_image, deploying]", "states: [dispatch_reconciling, dispatched, resolving_image]"); File.write(p,s)'
assert_mutation_fails "dispatch deadline removed from policy" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("dispatch_observation_seconds: 300", "dispatch_observation_seconds: 0"); File.write(p,s)'
assert_mutation_fails "run deadline removed from policy" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("github_run_seconds: 2700", "github_run_seconds: 0"); File.write(p,s)'
assert_mutation_fails "hard deadlines made caller-owned" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("hard_deadline_owner: store", "hard_deadline_owner: caller"); File.write(p,s)'
assert_mutation_fails "cancelled conclusion is nonterminal" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("terminal_failure_conclusions: [failure, cancelled, timed_out]", "terminal_failure_conclusions: [failure, timed_out]"); File.write(p,s)'
assert_mutation_fails "missing run does not fail" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("missing_run_after_dispatch_deadline: failed", "missing_run_after_dispatch_deadline: queued"); File.write(p,s)'
assert_mutation_fails "status evidence disabled" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("persist_status_and_error_evidence: true", "persist_status_and_error_evidence: false"); File.write(p,s)'
assert_mutation_fails "atomic terminal promotion disabled" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("terminal_resolution_promotes_pending_tuple_atomically: true", "terminal_resolution_promotes_pending_tuple_atomically: false"); File.write(p,s)'
assert_mutation_fails "persisted dispatch deadline removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("dispatch_deadline_at timestamptz,", ""); File.write(p,s)'
assert_mutation_fails "persisted run deadline removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("run_deadline_at timestamptz,", ""); File.write(p,s)'
assert_mutation_fails "reconciliation lease removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("reconciliation_lease_expires_at timestamptz,", ""); File.write(p,s)'
assert_mutation_fails "dispatched runs omitted from SQL reconciliation" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("WHERE status IN (\x27dispatch_reconciling\x27, \x27dispatched\x27)\n       AND (reconciliation_lease_expires_at", "WHERE status IN (\x27dispatch_reconciling\x27)\n       AND (reconciliation_lease_expires_at"); File.write(p,s)'
assert_mutation_fails "immutable run evidence removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.gsub!("INSERT INTO orchestration_job_run_evidence", "SELECT"); File.write(p,s)'
assert_mutation_fails "terminal cancellation handling removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("\x27success\x27, \x27failure\x27, \x27cancelled\x27, \x27timed_out\x27", "\x27success\x27, \x27failure\x27, \x27timed_out\x27"); File.write(p,s)'
assert_mutation_fails "terminal error evidence removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.gsub!("terminal_error_message", "discarded_error_message"); File.write(p,s)'
assert_mutation_fails "terminal pending tuple promotion removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("WHEN pending_checkout_sha IS NOT NULL THEN \x27queued\x27", "WHEN pending_checkout_sha IS NOT NULL THEN \x27failed\x27"); File.write(p,s)'
assert_mutation_fails "ambiguous dispatch observation mutates to queued" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("SET reconcile_after = LEAST(now() + interval \x2790 seconds\x27, dispatch_deadline_at)", "SET status = \x27queued\x27, reconcile_after = NULL"); File.write(p,s)'
# shellcheck disable=SC2016
assert_mutation_fails "missing-run terminal code guard removed" workflow/orchestration-job.sql 'p=ARGV[0]; s=File.read(p); s.sub!("AND $6 IN (\x27github_run_missing\x27, \x27github_run_ambiguous\x27)", "AND $6 IS NOT NULL"); File.write(p,s)'
assert_mutation_fails "gate implementer identity optional" workflow/gate.schema.json 'p=ARGV[0]; s=File.read(p); s.sub!(%Q[    "implementer_agent",\n], ""); File.write(p,s)'
assert_mutation_fails "gate validator unpinned" workflow/run.yaml 'p=ARGV[0]; s=File.read(p); s.sub!("validator_version: \"0.33.3\"", "validator_version: latest"); File.write(p,s)'
assert_mutation_fails "checkpoint digest optional" workflow/checkpoint.schema.json 'p=ARGV[0]; s=File.read(p); s.sub!(%Q[    "input_digest",\n], ""); File.write(p,s)'

assert_exit "Postgres contract without database" 2 env -u POSTGRES_CONTRACT_URL sh "$ROOT/scripts/validate_postgres_contract.sh"

sh "$ROOT/tests/workflow/validate_gate_evidence_test.sh" || fail "gate evidence negative tests failed"
pass "Draft 2020-12 gate evidence negative tests pass"

if [ -n "${POSTGRES_CONTRACT_URL:-}" ]; then
  sh "$ROOT/scripts/validate_postgres_contract.sh" || fail "live PostgreSQL 18 contract failed"
  pass "live PostgreSQL 18 OLD row contract passes"
else
  pass "live PostgreSQL 18 contract is available when POSTGRES_CONTRACT_URL is set"
fi

printf '%s\n' "workflow tests passed: $assertions assertions"
