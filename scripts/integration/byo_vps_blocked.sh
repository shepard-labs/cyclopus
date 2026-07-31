#!/bin/sh
# Bring-your-own-VPS gates: emit well-formed BLOCKED gate evidence (exit 2).
#
# The byo-vps and byo-vps-capabilities contracts require a live, SSH-accessible
# bring-your-own-VPS host with nested virtualization, which is NOT available in
# this environment. Rather than faking a pass, each emits schema-conformant gate
# evidence JSON with status "blocked" and a blocking_reason, and exits with the
# BLOCKED code (2). The offline node bootstrap/join checks these gates would run
# are still covered by scripts/test_infrastructure.sh (the unit gate). See
# infra/bootstrap/.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
CONTRACT=${1:?usage: byo_vps_blocked.sh <byo-vps|byo-vps-capabilities>}

case "$CONTRACT" in
  byo-vps)
    GATE=byo-vps-constrained
    PROFILE=byo_vps
    EVIDENCE_NAME=byo-vps.json
    ;;
  byo-vps-capabilities)
    GATE=byo-vps-capability-decision
    PROFILE=
    EVIDENCE_NAME=byo-vps-capability-decision.json
    ;;
  *)
    printf '%s\n' "byo_vps_blocked: unknown contract '$CONTRACT'" >&2
    exit 64
    ;;
esac

PHASE=phase-5
IMPLEMENTER=infrastructure-dr-implementer
COMMAND="./scripts/integration.sh $CONTRACT"
BLOCKING_REASON="live bring-your-own-VPS host unavailable in this environment"
EVIDENCE_DIR="${CYCLOPUS_EVIDENCE_DIR:-$ROOT/.workflow/evidence/$PHASE}"
EVIDENCE="$EVIDENCE_DIR/$EVIDENCE_NAME"

started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
finished_at=$started_at
source_revision=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf 'unknown')
input_digest=$(printf '%s' "$CONTRACT$source_revision" | sha256sum | cut -d ' ' -f 1)

mkdir -p "$EVIDENCE_DIR"

# gate_type is "integration" for the profile-bearing BYO-VPS gate and "evidence"
# for the capability decision gate (which has no integration profile).
gate_type=integration
[ -z "$PROFILE" ] && gate_type=evidence

CYCLOPUS_BYO_VPS_META="$PHASE|$GATE|$gate_type|$PROFILE|$started_at|$finished_at|$source_revision|$input_digest|$IMPLEMENTER|$COMMAND|$BLOCKING_REASON" \
ruby -rjson -e '
  m = ENV.fetch("CYCLOPUS_BYO_VPS_META").split("|", 11)
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
    "artifacts" => ["infra/bootstrap"],
    "notes" => "blocking_reason: #{reason}. Offline node bootstrap/join checks for this profile are covered by scripts/test_infrastructure.sh (unit gate).",
  }
  doc["profile"] = profile unless profile.nil? || profile.empty?
  print JSON.pretty_generate(doc)
' > "$EVIDENCE" || { printf 'byo_vps_blocked: FAILED to write evidence\n' >&2; exit 1; }

printf 'BLOCKED: %s: %s\n' "$CONTRACT" "$BLOCKING_REASON" >&2
printf 'byo_vps_blocked: evidence written: %s\n' "$EVIDENCE" >&2
exit 2
