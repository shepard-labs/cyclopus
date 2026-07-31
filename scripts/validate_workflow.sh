#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

ruby - "$ROOT" <<'RUBY'
require "json"
require "yaml"

root = ARGV.fetch(0)
required = %w[
  cyclopus-spec.md
  docs/implementation-workflow.md
  docs/decisions/phase-0-decisions.md
  versions.yaml
  workflow/run.yaml
  workflow/phases.yaml
  workflow/gate.schema.json
  workflow/checkpoint.schema.json
  workflow/orchestration-job.sql
  scripts/integration.sh
  scripts/test_infrastructure.sh
  scripts/probe_psl.sh
  scripts/validate_postgres_contract.sh
  scripts/validate_gate_evidence.sh
  tests/workflow/fixtures/pending_sha_pg18.sql
  tests/workflow/validate_gate_evidence_test.sh
  tests/schema/production_schema_test.sh
  tests/schema/production_schema_test.sql
]

errors = []
required.each do |path|
  full = File.join(root, path)
  errors << "missing required file: #{path}" unless File.file?(full) && !File.empty?(full)
end
unless errors.empty?
  warn errors.join("\n")
  exit 1
end

run = YAML.safe_load(File.read(File.join(root, "workflow/run.yaml")), aliases: false)
ledger = YAML.safe_load(File.read(File.join(root, "workflow/phases.yaml")), aliases: false)
versions = YAML.safe_load(File.read(File.join(root, "versions.yaml")), aliases: false)
schema = JSON.parse(File.read(File.join(root, "workflow/gate.schema.json")))
checkpoint_schema = JSON.parse(File.read(File.join(root, "workflow/checkpoint.schema.json")))
decisions = File.read(File.join(root, "docs/decisions/phase-0-decisions.md"))
normalized_decisions = decisions.gsub(/\s+/, " ")
orchestration_sql = File.read(File.join(root, "workflow/orchestration-job.sql"))
pending_sha_sql = File.read(File.join(root, "tests/workflow/fixtures/pending_sha_pg18.sql"))
spec = File.read(File.join(root, "cyclopus-spec.md"))

excluded = Array(run.dig("scope", "exclude")).map(&:downcase)
%w[ui frontend browser templ htmx tailwind].each do |term|
  errors << "run scope must exclude #{term}" unless excluded.include?(term)
end

errors << "workflow must resume from last success" unless run.dig("state", "resume_from_last_success") == true
errors << "workflow state writes must be atomic" unless run.dig("state", "atomic_writes") == true
errors << "workflow must declare checkpoint schema" unless run.dig("state", "checkpoint_schema") == "workflow/checkpoint.schema.json"
errors << "checkpoint digests must use sha256" unless run.dig("state", "input_digest_algorithm") == "sha256"
required_invalidations = %w[source_revision_change phase_definition_digest_change run_policy_digest_change versions_digest_change prerequisite_evidence_digest_change]
missing_invalidations = required_invalidations - Array(run.dig("state", "invalidate_on"))
errors << "checkpoint invalidation triggers missing: #{missing_invalidations.join(', ')}" unless missing_invalidations.empty?
errors << "checkpoint recovery must resume at earliest invalid or missing checkpoint" unless run.dig("state", "recovery") == "earliest_invalid_or_missing_checkpoint"
errors << "latest-version probe must run before each phase" unless run.dig("versions", "probe_before_each_phase") == true
errors << "pins must require compatibility review" unless run.dig("versions", "pin_only_after_compatibility_review") == true

%w[subagent network vm].each do |failure|
  policy = run.dig("retries", failure)
  unless policy.is_a?(Hash)
    errors << "missing retry policy for #{failure}"
    next
  end
  attempts = policy["max_attempts"]
  backoff = policy["backoff_seconds"]
  errors << "#{failure} max_attempts must be an integer greater than one" unless attempts.is_a?(Integer) && attempts > 1
  errors << "#{failure} backoff count must equal max_attempts minus one" unless backoff.is_a?(Array) && attempts.is_a?(Integer) && backoff.length == attempts - 1
  errors << "#{failure} backoff must be positive and strictly increasing" unless backoff.is_a?(Array) && backoff.all? { |v| v.is_a?(Integer) && v > 0 } && backoff.each_cons(2).all? { |a, b| b > a }
  errors << "#{failure} retry must use full jitter" unless policy["jitter"] == "full"
  retry_on = Array(policy["retry_on"])
  never_retry_on = Array(policy["never_retry_on"])
  errors << "#{failure} retry_on must be non-empty" if retry_on.empty?
  errors << "#{failure} never_retry_on must be non-empty" if never_retry_on.empty?
  errors << "#{failure} retry_on and never_retry_on overlap" unless (retry_on & never_retry_on).empty?
  errors << "#{failure} exhausted retries must block" unless policy["exhausted_status"] == "blocked"
  errors << "#{failure} retries must preserve successful evidence" unless policy["preserve_successful_evidence"] == true
  errors << "#{failure} retry must checkpoint attempts" unless policy["checkpoint_after_attempt"] == true
end

review = run["review_loop"] || {}
errors << "adversarial review must follow every phase" unless review["required_after_each_phase"] == true
errors << "reviewer must differ from implementer" unless review["reviewer_must_differ_from_implementer"] == true
errors << "review failures must enter a fix loop" unless review["fix_and_rerun_required"] == true
errors << "review max_cycles must be a positive integer" unless review["max_cycles"].is_a?(Integer) && review["max_cycles"] > 0

orchestration = run["orchestration"] || {}
errors << "orchestration must use Postgres" unless orchestration["durable_store"] == "postgres"
errors << "orchestration lease must precede GitHub dispatch" unless orchestration["lease_required_before_github_dispatch"] == true
errors << "orchestration lease TTL must be positive" unless orchestration["lease_ttl_seconds"].is_a?(Integer) && orchestration["lease_ttl_seconds"] > 0
errors << "orchestration must persist the observed run before leaving reconciliation" unless orchestration["persist_observed_run_before_reconciliation_exit"] == true
errors << "orchestration must recover expired leases" unless orchestration["recover_expired_leases_on_startup"] == true
errors << "orchestration single-flight scope is incomplete" unless %w[repository workflow ref environment].all? { |field| Array(orchestration["single_flight_scope"]).include?(field) }
errors << "orchestration idempotency scope is incomplete" unless %w[repository workflow ref environment commit_sha].all? { |field| Array(orchestration["idempotency_scope"]).include?(field) }
errors << "orchestration must coalesce to the latest SHA" unless orchestration["latest_sha_coalescing"] == true
errors << "orchestration must pass job ID and immutable commit SHA into GitHub" unless %w[orchestration_job_id commit_sha].all? { |field| Array(orchestration["dispatch_inputs"]).include?(field) }
errors << "orchestration workflow must checkout and attest the immutable SHA" unless %w[commit_sha checkout attest HEAD].all? { |term| orchestration["immutable_commit_contract"].to_s.include?(term) }
errors << "orchestration must correlate job ID and commit SHA" unless %w[orchestration_job_id commit_sha head_sha].all? { |term| orchestration["observable_run_correlation"].to_s.include?(term) }
errors << "ambiguous dispatch must have durable reconciliation state" unless orchestration["ambiguous_dispatch_state"] == "dispatch_reconciling"
errors << "ambiguous dispatch must never auto-redispatch" unless orchestration["ambiguous_dispatch_auto_redispatch"] == false
errors << "only a next push may retry a missing dispatch" unless orchestration["retry_trigger"] == "next_push"
reconciliation = orchestration["reconciliation"] || {}
errors << "orchestration reconciliation must run on startup" unless reconciliation["run_on_startup"] == true
errors << "orchestration reconciliation interval must be 30 seconds" unless reconciliation["periodic_interval_seconds"] == 30
errors << "orchestration reconciliation claim TTL must be 60 seconds" unless reconciliation["claim_ttl_seconds"] == 60
errors << "orchestration reconciliation must cover all nonterminal GitHub states" unless %w[dispatch_reconciling dispatched].all? { |state| Array(reconciliation["states"]).include?(state) }
timeouts = orchestration["timeouts"] || {}
errors << "orchestration hard deadlines must be store-owned" unless timeouts["hard_deadline_owner"] == "store"
errors << "dispatch observation timeout must be 300 seconds" unless timeouts["dispatch_observation_seconds"] == 300
errors << "dispatch observation poll must be 90 seconds" unless timeouts["dispatch_observation_poll_seconds"] == 90
errors << "GitHub run timeout must be 2700 seconds" unless timeouts["github_run_seconds"] == 2700
errors << "terminal GitHub conclusions are incomplete" unless %w[success failure cancelled timed_out].all? { |conclusion| Array(orchestration["terminal_run_conclusions"]).include?(conclusion) }
errors << "terminal failure conclusions are incomplete" unless %w[failure cancelled timed_out].all? { |conclusion| Array(orchestration["terminal_failure_conclusions"]).include?(conclusion) }
errors << "missing GitHub run after deadline must fail" unless orchestration["missing_run_after_dispatch_deadline"] == "failed"
errors << "orchestration must persist status and error evidence" unless orchestration["persist_status_and_error_evidence"] == true
errors << "terminal resolution must atomically promote pending SHA" unless orchestration["terminal_resolution_promotes_pending_sha_atomically"] == true
%w[orchestration_jobs orchestration_job_run_evidence idempotency_key pending_commit_sha pending_idempotency_key environment_id UNIQUE\ INDEX orchestration_jobs_scope_single_flight FOR\ UPDATE\ SKIP\ LOCKED dispatch_reconciling reconcile_after dispatch_deadline_at run_deadline_at reconciliation_owner reconciliation_lease_expires_at github_run_id github_run_status github_run_conclusion github_status_observed_at terminal_error_code terminal_error_message terminal_at orchestration_job_id github_run_missing].each do |term|
  literal = term.tr("\\", "")
  errors << "orchestration SQL contract missing: #{literal}" unless orchestration_sql.include?(literal)
end
job_table = orchestration_sql[/CREATE TABLE orchestration_jobs \(.*?\n\);/m]
required_job_columns = %w[dispatch_deadline_at run_deadline_at reconciliation_owner reconciliation_lease_expires_at github_run_status github_run_conclusion github_status_observed_at terminal_error_code terminal_error_message terminal_at]
required_job_columns.each do |column|
  errors << "orchestration job table missing persisted column: #{column}" unless job_table&.match?(/^\s*#{Regexp.escape(column)}\s+\w+/)
end
single_flight_index = orchestration_sql[/CREATE UNIQUE INDEX orchestration_jobs_scope_single_flight.*?;/m]
errors << "single-flight index must cover repository/workflow/ref/environment active states" unless single_flight_index && %w[repository_id workflow_id git_ref environment_id queued leased dispatch_reconciling dispatched].all? { |term| single_flight_index.include?(term) }
errors << "enqueue must use the scope partial unique index as conflict arbiter" unless orchestration_sql.match?(/ON CONFLICT \(repository_id, workflow_id, git_ref, environment_id\).*?WHERE status IN \('queued', 'leased', 'dispatch_reconciling', 'dispatched'\)/m)
ordinary_claim = orchestration_sql[/WITH candidate AS \(\n    SELECT id\n      FROM orchestration_jobs\n     WHERE status = 'queued'.*?RETURNING job\.id, job\.idempotency_key.*?;/m]
errors << "ordinary orchestration claim must lock with SKIP LOCKED" unless ordinary_claim && ordinary_claim.include?("FOR UPDATE SKIP LOCKED")
errors << "completion must atomically promote the latest pending SHA" unless orchestration_sql.include?("commit_sha = COALESCE(pending_commit_sha, commit_sha)") && orchestration_sql.include?("pending_commit_sha = NULL")
errors << "dispatch deadline must be persisted without extension across retries" unless orchestration_sql.include?("dispatch_deadline_at = COALESCE(dispatch_deadline_at, now() + interval '300 seconds')")
errors << "correlated runs must get a persisted run deadline" unless orchestration_sql.include?("run_deadline_at = now() + interval '2700 seconds'")
errors << "periodic reconciliation must poll every 30 seconds" unless orchestration_sql.include?("reconcile_after = now() + interval '30 seconds'")
reconciliation_claim = orchestration_sql[/WITH candidate AS \(\n    SELECT id\n      FROM orchestration_jobs\n     WHERE status IN \('dispatch_reconciling', 'dispatched'\).*?RETURNING job\.\*;/m]
errors << "startup/periodic reconciliation must lease every nonterminal GitHub job" unless reconciliation_claim && reconciliation_claim.include?("reconciliation_lease_expires_at = now() + interval '60 seconds'") && reconciliation_claim.include?("FOR UPDATE SKIP LOCKED")
errors << "run evidence must be immutable inserts" unless orchestration_sql.scan(/INSERT INTO orchestration_job_run_evidence/).length >= 2
errors << "nonterminal reconciliation must persist status before rescheduling" unless orchestration_sql.match?(/INSERT INTO orchestration_job_run_evidence.*?github_run_status = \$3.*?reconcile_after = now\(\) \+ interval '30 seconds'/m)
terminal_resolution = orchestration_sql[/WITH terminal_evidence AS \(.*?RETURNING job\.id, job\.status, job\.commit_sha, job\.terminal_error_code;/m]
errors << "terminal resolver must handle success, failure, cancellation, timeout, and missing runs" unless terminal_resolution && %w[success failure cancelled timed_out github_run_id dispatch_deadline_at run_deadline_at failed queued].all? { |term| terminal_resolution.include?(term) }
errors << "terminal resolver must persist error evidence" unless terminal_resolution && %w[terminal_error_code terminal_error_message terminal_at].all? { |term| terminal_resolution.include?(term) }
errors << "missing-run terminal resolution must require github_run_missing" unless terminal_resolution&.include?("$5 = 'github_run_missing'")
errors << "terminal resolver must atomically promote latest pending SHA" unless terminal_resolution && terminal_resolution.include?("commit_sha = COALESCE(pending_commit_sha, commit_sha)") && terminal_resolution.include?("pending_commit_sha = NULL") && terminal_resolution.include?("WHEN pending_commit_sha IS NOT NULL THEN 'queued'")
errors << "ambiguous dispatch SQL must not requeue before its deadline" if orchestration_sql.match?(/SET status = 'queued'.*?status = 'dispatch_reconciling'.*?dispatch_deadline_at > now\(\)/m)
errors << "ambiguous dispatch SQL must reschedule observation without mutation" unless orchestration_sql.include?("SET reconcile_after = LEAST(now() + interval '90 seconds', dispatch_deadline_at)")
errors << "retryable reconciliation must select the deadline by state" unless orchestration_sql.match?(/CASE status.*?WHEN 'dispatch_reconciling' THEN dispatch_deadline_at.*?WHEN 'dispatched' THEN run_deadline_at/m)
errors << "retryable reconciliation must reject expired deadlines" unless orchestration_sql.match?(/CASE status.*?WHEN 'dispatch_reconciling' THEN dispatch_deadline_at.*?WHEN 'dispatched' THEN run_deadline_at.*?END > now\(\)/m)
reconcile_pos = orchestration_sql.index("SET status = 'dispatch_reconciling'")
dispatch_guidance_pos = orchestration_sql.index("Before sending workflow_dispatch")
errors << "orchestration must enter reconciliation before dispatch" unless dispatch_guidance_pos && reconcile_pos && dispatch_guidance_pos < reconcile_pos

evidence_validation = run["evidence_validation"] || {}
errors << "gate evidence must declare Draft 2020-12" unless evidence_validation["schema_draft"] == "2020-12"
errors << "gate evidence validator must be check-jsonschema" unless evidence_validation["validator"] == "check-jsonschema"
errors << "gate evidence validator must be reproducibly version-pinned" unless evidence_validation["validator_version"] == "0.33.3" && evidence_validation["command"].to_s.include?("check-jsonschema==0.33.3")
errors << "gate evidence must require the current source revision" unless evidence_validation["require_current_source_revision"] == true

profiles = run["integration_profiles"] || {}
%w[orbstack_ubuntu orbstack_debian byo_vps].each do |profile|
  errors << "missing integration profile: #{profile}" unless profiles.key?(profile)
end
errors << "BYO VPS must be constrained" unless profiles.dig("byo_vps", "role") == "constrained"
%w[byo_vps].each do |profile|
  selection = profiles.dig(profile, "selection") || {}
  errors << "#{profile} must be an explicit user override" unless selection["kind"] == "user_override"
  %w[decided_by decided_at rationale capability_gate].each do |field|
    errors << "#{profile} selection lacks #{field}" if selection[field].to_s.empty?
  end
end

errors << "versions policy must probe before each phase" unless versions.dig("policy", "probe_before_each_phase") == true
errors << "versions policy must require review metadata" unless versions.dig("policy", "pin_rule") == "reviewed_or_documented_intentional_pin"
errors << "versions manifest lacks observation date" if versions.dig("policy", "observed_at").to_s.empty?
(versions["components"] || {}).each do |name, component|
  errors << "#{name} lacks a version pin" if component["pin"].to_s.empty?
  errors << "#{name} retains unaudited compatibility boolean" if component.key?("compatibility_reviewed")
  errors << "#{name} lacks observed latest version" if component["observed_latest"].to_s.empty?
  errors << "#{name} lacks a latest-version probe" if component["probe"].to_s.empty?
  metadata = component["review"] || {}
  errors << "#{name} review status is invalid" unless %w[approved intentional_pin].include?(metadata["status"])
  %w[reviewer reviewed_at].each do |field|
    errors << "#{name} review lacks #{field}" if metadata[field].to_s.empty?
  end
  rationale = metadata["rationale"].to_s.strip.gsub(/\s+/, " ")
  errors << "#{name} review lacks rationale" if rationale.empty?
  errors << "#{name} review lacks evidence" unless metadata["evidence"].is_a?(Array) && !metadata["evidence"].empty? && metadata["evidence"].all? { |item| !item.to_s.empty? }
  if metadata["status"] == "intentional_pin"
    errors << "#{name} intentional pin must differ from observed latest" if component["pin"] == component["observed_latest"]
    errors << "#{name} intentional pin rationale is too short" if rationale.length < 30
  end
end

phases = ledger["phases"]
errors << "phases must be a non-empty list" unless phases.is_a?(Array) && !phases.empty?
Array(phases).each do |phase|
  id = phase["id"] || "<unknown>"
  gates = phase["gates"] || {}
  errors << "#{id} lacks a specialized owner agent" if phase["owner_agent"].to_s.empty?
  errors << "#{id} lacks a specialized reviewer agent" if phase["reviewer_agent"].to_s.empty?
  errors << "#{id} uses the same implementer and reviewer" if phase["owner_agent"] == phase["reviewer_agent"]
  errors << "#{id} lacks unit gate" unless gates["unit"].is_a?(Hash) && !gates.dig("unit", "command").to_s.empty? && !gates.dig("unit", "evidence").to_s.empty?
  errors << "#{id} lacks evidence gates" unless gates["evidence"].is_a?(Array) && !gates["evidence"].empty?
  adversarial = gates["adversarial_review"]
  errors << "#{id} lacks adversarial review gate" unless adversarial.is_a?(Hash) && !adversarial["agent"].to_s.empty? && !adversarial["evidence"].to_s.empty?
  errors << "#{id} adversarial review lacks fix loop" unless adversarial && adversarial["fix_loop"] == true
  errors << "#{id} lacks version-probe checkpoint" unless Array(phase["checkpoints"]).include?("version-probe")
end

serialized_scope = [ledger["scope"], *Array(phases).flat_map { |p| [p["name"], p["deliverables"], p["spec_sections"]] }].flatten.compact.join(" ").downcase
%w[ui frontend browser templ htmx tailwind].each do |term|
  errors << "UI entered phase scope via term: #{term}" if serialized_scope.match?(/\b#{Regexp.escape(term)}\b/)
end

phase0 = Array(phases).find { |phase| phase["id"] == "phase-0" }
phase0_ids = Array(phase0&.dig("gates", "evidence")).map { |gate| gate["id"] }
errors << "phase-0 lacks PostgreSQL 18 pending SHA contract gate" unless phase0_ids.include?("postgres-18-pending-sha-contract")
errors << "phase-0 lacks Draft 2020-12 evidence validation gate" unless phase0_ids.include?("gate-evidence-draft-2020-12")
errors << "phase-0 lacks PSL probe gate" unless phase0_ids.include?("psl-probe")

phase3 = Array(phases).find { |phase| phase["id"] == "phase-3" }
psl_gate = Array(phase3&.dig("gates", "evidence")).find { |gate| gate["id"] == "psl-launch-block" }
errors << "phase-3 PSL gate must execute the PSL probe" unless psl_gate&.dig("command") == "./scripts/probe_psl.sh sslip.io absent"

phase5 = Array(phases).find { |phase| phase["id"] == "phase-5" }
phase5_gates = Array(phase5&.dig("gates", "evidence"))
phase5_profiles = phase5_gates.map { |gate| gate["profile"] }.compact
%w[orbstack_ubuntu orbstack_debian byo_vps].each do |profile|
  errors << "phase-5 lacks #{profile} evidence gate" unless phase5_profiles.include?(profile)
end
errors << "phase-5 lacks BYO-VPS capability decision gate" unless phase5_gates.map { |gate| gate["id"] }.include?("byo-vps-capability-decision")

decision_terms = [
  "same Go binary", "Postgres is the authoritative saga", "No transaction spans Kubernetes and Postgres",
  "atomic take-and-clear", "one user in one organization", "idempotent", "immutable release snapshots",
  "maxUnavailable=0", "mixed traffic", "absent from the Public Suffix List", "configurable provider seam",
  "RETURNING WITH (OLD AS old_row)", "durable orchestration job", "any VPS with nested virtualization",
  "Correlation does not make a GitHub run live forever", "terminal failures", "atomically promotes the latest pending SHA",
  "never automatically redispatches an ambiguous request", "Phase 0 deletion erratum", "audited tombstone lifecycle"
]
decision_terms.each do |term|
  errors << "Phase 0 decision missing: #{term}" unless normalized_decisions.downcase.include?(term.downcase)
end

required_schema_fields = %w[phase gate gate_type status attempt source_revision input_digest implementer_agent command exit_code artifacts]
schema_required = Array(schema["required"])
required_schema_fields.each do |field|
  errors << "gate schema does not require #{field}" unless schema_required.include?(field)
end
errors << "gate schema must reject unknown fields" unless schema["additionalProperties"] == false
errors << "gate schema must require reviewer identity" unless schema.to_s.include?("reviewer_agent")
errors << "gate schema must reject unresolved critical/high passed reviews" unless %w[critical high contains resolved].all? { |term| schema.to_s.include?(term) }

checkpoint_required = Array(checkpoint_schema["required"])
%w[phase checkpoint status source_revision input_digest prerequisite_evidence completed_at].each do |field|
  errors << "checkpoint schema does not require #{field}" unless checkpoint_required.include?(field)
end
errors << "checkpoint schema must reject unknown fields" unless checkpoint_schema["additionalProperties"] == false
errors << "checkpoint schema lacks invalidated status" unless checkpoint_schema.to_s.include?("invalidated")
errors << "checkpoint schema lacks digest invalidation reasons" unless required_invalidations.all? { |reason| checkpoint_schema.to_s.include?(reason) }

errors << "pending SHA fixture must use PostgreSQL 18 OLD row RETURNING" unless pending_sha_sql.match?(/RETURNING\s+WITH\s*\(OLD\s+AS\s+old_row/i)
errors << "pending SHA fixture must assert old and cleared values" unless pending_sha_sql.include?("taken_sha = repeat('a', 40)") && pending_sha_sql.include?("remaining_sha IS NULL")
errors << "canonical spec must establish Phase 0 errata precedence" unless spec.include?("Phase 0 Errata Precedence") && spec.index("workflow/orchestration-job.sql").to_i < spec.index("docs/decisions/phase-0-decisions.md").to_i
errors << "canonical spec pending SHA SQL must return OLD and NEW values" unless spec.match?(/RETURNING WITH \(OLD AS old_row, NEW AS new_row\).*?old_row\.pending_commit_sha AS taken_sha.*?new_row\.pending_commit_sha AS remaining_sha/m)

executable_scripts = %w[scripts/integration.sh scripts/test_infrastructure.sh scripts/probe_psl.sh scripts/validate_postgres_contract.sh scripts/validate_gate_evidence.sh tests/workflow/validate_gate_evidence_test.sh tests/schema/production_schema_test.sh]
executable_scripts.each do |path|
  errors << "#{path} must be executable" unless File.executable?(File.join(root, path))
end
integration_script = File.read(File.join(root, "scripts/integration.sh"))
infrastructure_script = File.read(File.join(root, "scripts/test_infrastructure.sh"))
errors << "integration script must report blocked" unless integration_script.include?("BLOCKED:") && integration_script.match?(/exit\s+2/)
errors << "infrastructure test script must report blocked" unless infrastructure_script.include?("BLOCKED:") && infrastructure_script.match?(/exit\s+2/)
errors << "PSL probe must fetch canonical list" unless File.read(File.join(root, "scripts/probe_psl.sh")).include?("publicsuffix.org/list/public_suffix_list.dat")

if errors.empty?
  puts "workflow validation passed: #{phases.length} phases, #{versions.fetch("components").length} audited version decisions"
else
  warn errors.join("\n")
  exit 1
end
RUBY
