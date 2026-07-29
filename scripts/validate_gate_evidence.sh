#!/bin/sh
set -eu

test "$#" -eq 2 || {
  printf '%s\n' "usage: $0 evidence.json current-source-revision" >&2
  exit 64
}

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
evidence=$1
current_revision=$2
schema="$ROOT/workflow/gate.schema.json"

test -s "$evidence" || { printf '%s\n' "missing evidence: $evidence" >&2; exit 1; }
test -s "$schema" || { printf '%s\n' "missing schema: $schema" >&2; exit 1; }
command -v uvx >/dev/null 2>&1 || {
  printf '%s\n' "BLOCKED: uvx is required for check-jsonschema==0.33.3" >&2
  exit 2
}

# check-jsonschema uses python-jsonschema's Draft 2020-12 validator selected by
# the schema's $schema URI. The exact CLI version is pinned for reproducibility.
uvx --from check-jsonschema==0.33.3 check-jsonschema \
  --schemafile "$schema" \
  "$evidence"

# JSON Schema handles shape and conditional constraints. These invariants need
# runner context or comparisons between values and are checked after validation.
ruby -rjson -rtime -e '
  evidence = JSON.parse(File.read(ARGV.fetch(0)))
  current_revision = ARGV.fetch(1)

  abort("source_revision is stale: expected #{current_revision.inspect}, got #{evidence["source_revision"].inspect}") unless evidence["source_revision"] == current_revision

  started_at = Time.iso8601(evidence.fetch("started_at"))
  finished_at = Time.iso8601(evidence.fetch("finished_at"))
  abort("finished_at precedes started_at") if finished_at < started_at

  if evidence["gate_type"] == "adversarial_review"
    abort("reviewer must differ from implementer") if evidence["reviewer_agent"] == evidence["implementer_agent"]
    unresolved = evidence.fetch("findings").select do |finding|
      %w[critical high].include?(finding["severity"]) && finding["resolved"] != true
    end
    abort("passed review retains unresolved critical/high findings") if evidence["status"] == "passed" && !unresolved.empty?
  end

  abort("passed evidence must have exit_code 0") if evidence["status"] == "passed" && evidence["exit_code"] != 0
' "$evidence" "$current_revision"
