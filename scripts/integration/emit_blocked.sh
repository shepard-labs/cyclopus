#!/bin/sh
# Emit schema-conformant BLOCKED gate evidence and exit 0 (the caller decides the
# process exit code — normally 2). Used by the OrbStack install/DR gates when the
# 'orb' CLI is unavailable so the audit trail distinguishes a genuinely blocked
# attempt from a gate that was never run (M4). Mirrors byo_vps_blocked.sh's schema.
#
# Usage: emit_blocked.sh <gate> <gate_type> <profile> <evidence_name> <reason> <command>
#   profile may be empty for gate_type=evidence; otherwise it must be a value the
#   workflow gate schema's profile enum accepts (e.g. orbstack_ubuntu).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

GATE=${1:?usage: emit_blocked.sh <gate> <gate_type> <profile> <evidence_name> <reason> <command>}
GATE_TYPE=${2:?gate_type is required}
PROFILE=${3:-}
EVIDENCE_NAME=${4:?evidence_name is required}
BLOCKING_REASON=${5:?reason is required}
COMMAND=${6:?command is required}

PHASE=phase-5
IMPLEMENTER=infrastructure-dr-implementer
EVIDENCE_DIR="${CYCLOPUS_EVIDENCE_DIR:-$ROOT/.workflow/evidence/$PHASE}"
EVIDENCE="$EVIDENCE_DIR/$EVIDENCE_NAME"

started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
finished_at=$started_at
source_revision=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf 'unknown')
input_digest=$(printf '%s' "$GATE$source_revision" | sha256sum | cut -d ' ' -f 1)

mkdir -p "$EVIDENCE_DIR"

CYCLOPUS_BLOCKED_META="$PHASE|$GATE|$GATE_TYPE|$PROFILE|$started_at|$finished_at|$source_revision|$input_digest|$IMPLEMENTER|$COMMAND|$BLOCKING_REASON" \
ruby -rjson -e '
  m = ENV.fetch("CYCLOPUS_BLOCKED_META").split("|", 11)
  phase, gate, gate_type, profile, started_at, finished_at, source_revision, input_digest, implementer, command, reason = m
  doc = {
    "schema_version" => 2,
    "workflow" => "cyclopus-api-platform-v1",
    "phase" => phase,
    "gate" => gate,
    "gate_type" => gate_type,
    "status" => "blocked",
    "attempt" => 1,
    "started_at" => started_at,
    "finished_at" => finished_at,
    "source_revision" => source_revision,
    "input_digest" => input_digest,
    "implementer_agent" => implementer,
    "command" => command,
    "exit_code" => 2,
    "artifacts" => ["scripts/integration"],
    "notes" => "blocking_reason: #{reason}. Offline equivalents are covered by scripts/test_infrastructure.sh (the unit gate).",
  }
  doc["profile"] = profile unless profile.nil? || profile.empty?
  print JSON.pretty_generate(doc)
' > "$EVIDENCE" || { printf 'emit_blocked: FAILED to write evidence\n' >&2; exit 1; }

printf 'emit_blocked: BLOCKED evidence written: %s (%s)\n' "$EVIDENCE" "$BLOCKING_REASON" >&2
