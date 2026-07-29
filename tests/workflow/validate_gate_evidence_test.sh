#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
VALIDATOR="$ROOT/scripts/validate_gate_evidence.sh"
REVISION=phase-0-test-revision
assertions=0
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cyclopus-gate-evidence-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

fail() { printf '%s\n' "not ok - $1" >&2; exit 1; }
pass() { assertions=$((assertions + 1)); printf '%s\n' "ok - $1"; }

write_valid() {
  ruby -rjson -e '
    File.write(ARGV.fetch(0), JSON.pretty_generate({
      schema_version: 2,
      workflow: "cyclopus-api-platform-v1",
      phase: "phase-0",
      gate: "review",
      gate_type: "adversarial_review",
      status: "passed",
      attempt: 1,
      started_at: "2026-07-29T00:00:00Z",
      finished_at: "2026-07-29T00:01:00Z",
      source_revision: "phase-0-test-revision",
      input_digest: "a" * 64,
      implementer_agent: "builder",
      reviewer_agent: "reviewer",
      command: "review",
      exit_code: 0,
      findings: [{severity: "high", summary: "fixed", resolved: true}],
      artifacts: ["review.txt"]
    }))
  ' "$1"
}

assert_valid() {
  name=$1
  file=$2
  sh "$VALIDATOR" "$file" "$REVISION" >/dev/null 2>&1 || fail "$name was rejected"
  pass "$name is accepted"
}

assert_invalid() {
  name=$1
  expression=$2
  file="$tmp/invalid-$assertions.json"
  write_valid "$file"
  ruby -rjson -e "$expression" "$file"
  if sh "$VALIDATOR" "$file" "$REVISION" >/dev/null 2>&1; then
    fail "$name was accepted"
  fi
  pass "$name is rejected"
}

valid="$tmp/valid.json"
write_valid "$valid"
assert_valid "Draft 2020-12 valid evidence" "$valid"

assert_invalid "unknown property" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["extra"]=true; File.write(p,JSON.pretty_generate(j))'
assert_invalid "wrong schema version" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["schema_version"]=1; File.write(p,JSON.pretty_generate(j))'
assert_invalid "missing required field" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j.delete("command"); File.write(p,JSON.pretty_generate(j))'
assert_invalid "missing adversarial reviewer" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j.delete("reviewer_agent"); File.write(p,JSON.pretty_generate(j))'
assert_invalid "invalid status enum" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["status"]="complete"; File.write(p,JSON.pretty_generate(j))'
assert_invalid "invalid date-time format" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["started_at"]="not-a-date"; File.write(p,JSON.pretty_generate(j))'
assert_invalid "invalid SHA-256 digest" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["input_digest"]="abc"; File.write(p,JSON.pretty_generate(j))'
assert_invalid "non-positive attempt" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["attempt"]=0; File.write(p,JSON.pretty_generate(j))'
assert_invalid "duplicate artifacts" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["artifacts"]=["same","same"]; File.write(p,JSON.pretty_generate(j))'
assert_invalid "integration without profile" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["gate_type"]="integration"; j.delete("reviewer_agent"); j.delete("findings"); File.write(p,JSON.pretty_generate(j))'
assert_invalid "passed nonzero exit" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["exit_code"]=1; File.write(p,JSON.pretty_generate(j))'
assert_invalid "unresolved high passed review" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["findings"][0]["resolved"]=false; File.write(p,JSON.pretty_generate(j))'
assert_invalid "unknown finding property" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["findings"][0]["owner"]="nobody"; File.write(p,JSON.pretty_generate(j))'
assert_invalid "same implementer and reviewer" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["reviewer_agent"]=j["implementer_agent"]; File.write(p,JSON.pretty_generate(j))'
assert_invalid "stale source revision" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["source_revision"]="previous-revision"; File.write(p,JSON.pretty_generate(j))'
assert_invalid "finished before started" 'p=ARGV[0]; j=JSON.parse(File.read(p)); j["finished_at"]="2026-07-28T23:59:59Z"; File.write(p,JSON.pretty_generate(j))'

printf '%s\n' "gate evidence tests passed: $assertions assertions"
