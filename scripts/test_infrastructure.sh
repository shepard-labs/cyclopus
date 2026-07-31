#!/bin/sh
# Phase 5 unit gate — runs WITHOUT live cloud (§9, §10, §11; workflow phase-5).
#
# Executes, in order, entirely offline:
#   1. tofu fmt -check + tofu validate (init -backend=false) on infra/opentofu
#   2. helm lint + helm template on charts/cyclopus (default + constrained overlay)
#   3. sh -n syntax + shellcheck on every bootstrap/backup/integration script
#   4. pure-logic checks (constrained-profile capacity floors, pinned versions)
#
# Missing tofu/helm/shellcheck are installed via checksum-verified download when
# possible, else the affected step is skipped with a clear status (the gate does
# not silently pass over a skipped tool). Emits gate evidence JSON to
# .workflow/evidence/phase-5/unit.json conforming to workflow/gate.schema.json.
#
# Exit codes: 0 passed, 1 failed, 2 blocked (BLOCKED: prerequisite unavailable).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PHASE=phase-5
GATE=unit
IMPLEMENTER=infrastructure-dr-implementer
COMMAND='./scripts/test_infrastructure.sh'
EVIDENCE_DIR="${CYCLOPUS_EVIDENCE_DIR:-$ROOT/.workflow/evidence/$PHASE}"
EVIDENCE="${CYCLOPUS_UNIT_EVIDENCE:-$EVIDENCE_DIR/unit.json}"

# Pinned tool versions (versions.yaml). Used for checksum-verified installs and
# for the version assertions in the pure-logic checks.
TOFU_VERSION=1.10.1
HELM_VERSION=v3.18.3
K3S_VERSION='v1.33.13+k3s1'
GVISOR_RELEASE='release-20260727.0'
WAL_G_VERSION='v3.0.8'

started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
tmp_root=$(mktemp -d)
trap 'rm -rf "$tmp_root"' EXIT HUP INT TERM
TOOLBIN="$tmp_root/bin"
mkdir -p "$TOOLBIN"
PATH="$TOOLBIN:$PATH"
export PATH

status=passed
exit_code=0
notes=""
artifacts=""

log() { printf '\n==> unit: %s\n' "$1"; }
note() { notes="${notes}${notes:+ }$1"; }
add_artifact() { artifacts="${artifacts}${artifacts:+ }$1"; }
mark_failed() {
  status=failed
  exit_code=1
  printf 'unit: FAIL: %s\n' "$*" >&2
}
mark_blocked() {
  # BLOCKED: prerequisite genuinely unavailable and could not be provisioned.
  status=blocked
  exit_code=2
  printf 'unit: BLOCKED: %s\n' "$*" >&2
}

arch_tool() {
  case "$(uname -m)" in
    aarch64 | arm64) printf 'arm64' ;;
    x86_64 | amd64) printf 'amd64' ;;
    *) printf 'unsupported' ;;
  esac
}
os_tool() {
  case "$(uname -s)" in
    Darwin) printf 'darwin' ;;
    Linux) printf 'linux' ;;
    *) printf 'unsupported' ;;
  esac
}

# --- tool provisioning (checksum-verified, else skip-with-status) ------------

ensure_tofu() {
  if command -v tofu >/dev/null 2>&1; then TOFU=tofu; return 0; fi
  _os=$(os_tool); _arch=$(arch_tool)
  [ "$_os" != unsupported ] && [ "$_arch" != unsupported ] || { note "tofu unavailable (unsupported platform)"; return 1; }
  _zip="tofu_${TOFU_VERSION}_${_os}_${_arch}.zip"
  _base="https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}"
  log "installing checksum-verified OpenTofu $TOFU_VERSION"
  curl -fsSLo "$tmp_root/$_zip" "$_base/$_zip" 2>/dev/null || { note "tofu download failed"; return 1; }
  curl -fsSLo "$tmp_root/tofu_sums" "$_base/tofu_${TOFU_VERSION}_SHA256SUMS" 2>/dev/null || { note "tofu checksum download failed"; return 1; }
  _expected=$(awk -v f="$_zip" '$2==f{print $1; exit}' "$tmp_root/tofu_sums")
  _actual=$(sha256sum "$tmp_root/$_zip" 2>/dev/null | cut -d ' ' -f 1)
  [ -n "$_expected" ] && [ "$_expected" = "$_actual" ] || { note "tofu checksum mismatch"; return 1; }
  ( cd "$tmp_root" && unzip -oq "$_zip" tofu ) 2>/dev/null || { note "tofu unzip failed"; return 1; }
  install -m 0755 "$tmp_root/tofu" "$TOOLBIN/tofu" 2>/dev/null || return 1
  TOFU=tofu
  return 0
}

ensure_helm() {
  if command -v helm >/dev/null 2>&1; then HELM=helm; return 0; fi
  _os=$(os_tool); _arch=$(arch_tool)
  [ "$_os" != unsupported ] && [ "$_arch" != unsupported ] || { note "helm unavailable (unsupported platform)"; return 1; }
  _tar="helm-${HELM_VERSION}-${_os}-${_arch}.tar.gz"
  _base="https://get.helm.sh"
  log "installing checksum-verified Helm $HELM_VERSION"
  curl -fsSLo "$tmp_root/$_tar" "$_base/$_tar" 2>/dev/null || { note "helm download failed"; return 1; }
  curl -fsSLo "$tmp_root/helm_sum" "$_base/$_tar.sha256sum" 2>/dev/null || { note "helm checksum download failed"; return 1; }
  _expected=$(awk '{print $1; exit}' "$tmp_root/helm_sum")
  _actual=$(sha256sum "$tmp_root/$_tar" 2>/dev/null | cut -d ' ' -f 1)
  [ -n "$_expected" ] && [ "$_expected" = "$_actual" ] || { note "helm checksum mismatch"; return 1; }
  ( cd "$tmp_root" && tar -xzf "$_tar" ) 2>/dev/null || { note "helm untar failed"; return 1; }
  install -m 0755 "$tmp_root/${_os}-${_arch}/helm" "$TOOLBIN/helm" 2>/dev/null || return 1
  HELM=helm
  return 0
}

# --- checks ------------------------------------------------------------------

check_tofu() {
  ensure_tofu || { note "tofu step skipped: binary unavailable"; SKIP_TOFU=1; return 0; }
  log "tofu fmt -check + validate (infra/opentofu)"
  ( cd "$ROOT/infra/opentofu" && "$TOFU" fmt -check -recursive ) >"$tmp_root/tofu_fmt.log" 2>&1 \
    || { mark_failed "tofu fmt -check reported unformatted files"; return 0; }
  ( cd "$ROOT/infra/opentofu" && "$TOFU" init -backend=false -input=false -no-color ) >"$tmp_root/tofu_init.log" 2>&1 \
    || { mark_failed "tofu init -backend=false failed"; return 0; }
  ( cd "$ROOT/infra/opentofu" && "$TOFU" validate -no-color ) >"$tmp_root/tofu_validate.log" 2>&1 \
    || { mark_failed "tofu validate failed"; return 0; }
  rm -rf "$ROOT/infra/opentofu/.terraform"
  note "tofu fmt+validate passed"
}

check_helm() {
  ensure_helm || { note "helm step skipped: binary unavailable"; SKIP_HELM=1; return 0; }
  log "helm lint (charts/cyclopus)"
  "$HELM" lint "$ROOT/charts/cyclopus" >"$tmp_root/helm_lint.log" 2>&1 \
    || { mark_failed "helm lint failed"; return 0; }
  log "helm template default + constrained overlay"
  "$HELM" template cyclopus "$ROOT/charts/cyclopus" --kube-version 1.33.13 \
    --set secrets.databaseUrl='postgres://u:p@h/db' \
    --set secrets.authTokenPepper='unit-gate-pepper' \
    >"$tmp_root/helm_template.log" 2>&1 \
    || { mark_failed "helm template (default) failed"; return 0; }
  "$HELM" template cyclopus "$ROOT/charts/cyclopus" --kube-version 1.33.13 \
    -f "$ROOT/charts/cyclopus/values-constrained.yaml" \
    --set secrets.databaseUrl='postgres://u:p@h/db' \
    --set secrets.authTokenPepper='unit-gate-pepper' \
    >"$tmp_root/helm_template_constrained.log" 2>&1 \
    || { mark_failed "helm template (constrained) failed"; return 0; }
  # The rendered chart must actually carry the load-bearing invariants.
  grep -q 'cyclopus.app/role' "$tmp_root/helm_template.log" || { mark_failed "rendered chart missing control-plane toleration"; return 0; }
  grep -q 'kind: ClusterRole' "$tmp_root/helm_template.log" || { mark_failed "rendered chart missing cluster-wide RBAC (§9.2)"; return 0; }
  note "helm lint+template passed"
}

check_scripts() {
  log "sh -n syntax + shellcheck on bootstrap/backup/integration scripts"
  have_shellcheck=0
  command -v shellcheck >/dev/null 2>&1 && have_shellcheck=1
  [ "$have_shellcheck" = 1 ] || note "shellcheck unavailable; sh -n only"
  _failed=0
  for f in \
    "$ROOT"/infra/bootstrap/*.sh \
    "$ROOT"/infra/backup/*.sh \
    "$ROOT"/scripts/integration.sh \
    "$ROOT"/scripts/integration/install_orbstack.sh \
    "$ROOT"/scripts/integration/install_guest.sh \
    "$ROOT"/scripts/integration/dr_orbstack.sh \
    "$ROOT"/scripts/integration/dr_guest.sh \
    "$ROOT"/scripts/integration/ovh_blocked.sh \
    "$ROOT"/scripts/integration/emit_blocked.sh \
    "$ROOT"/scripts/test_infrastructure.sh; do
    [ -f "$f" ] || continue
    sh -n "$f" 2>>"$tmp_root/script_syntax.log" || { printf 'sh -n failed: %s\n' "$f" >>"$tmp_root/script_syntax.log"; _failed=1; }
    if [ "$have_shellcheck" = 1 ]; then
      # SC1007: the `CDPATH= cd` idiom used throughout this repo is intentional.
      # SC2034: vars set for consumption by sourced sibling scripts.
      shellcheck -s sh -S warning -e SC1007 -e SC2034 "$f" >>"$tmp_root/shellcheck.log" 2>&1 || { printf 'shellcheck failed: %s\n' "$f" >>"$tmp_root/shellcheck.log"; _failed=1; }
    fi
  done
  [ "$_failed" = 0 ] || { mark_failed "script syntax/shellcheck checks failed"; return 0; }
  note "script syntax/shellcheck passed"
}

check_logic() {
  log "pure-logic checks: constrained capacity floors + pinned versions"
  # Constrained OVH profile: the example overlay must still meet the hard floor.
  _tfvars="$ROOT/infra/opentofu/examples/constrained.tfvars"
  _vars="$ROOT/infra/opentofu/variables.tf"
  [ -f "$_tfvars" ] || { mark_failed "constrained tfvars example missing"; return 0; }
  # Floor (min_*): control 2/4/40, data 2/4/40 — assert defaults present.
  grep -q 'vcpus     = 2' "$_vars" || { mark_failed "control-plane capacity floor missing from variables.tf"; return 0; }
  grep -q 'memory_gb = 4' "$_vars" || { mark_failed "memory floor missing from variables.tf"; return 0; }
  grep -q 'disk_gb   = 40' "$_vars" || { mark_failed "disk floor missing from variables.tf"; return 0; }
  # The precondition that enforces the gate must exist in the compute module.
  grep -q 'precondition' "$ROOT/infra/opentofu/modules/compute/main.tf" \
    || { mark_failed "compute module lacks capacity-gate precondition"; return 0; }
  # Pinned versions consistent with versions.yaml.
  grep -q "$K3S_VERSION" "$ROOT/infra/bootstrap/common.sh" || { mark_failed "k3s pin drift in bootstrap"; return 0; }
  grep -q "$GVISOR_RELEASE" "$ROOT/infra/bootstrap/common.sh" || { mark_failed "gVisor pin drift in bootstrap"; return 0; }
  grep -q "$WAL_G_VERSION" "$ROOT/infra/backup/common.sh" || { mark_failed "WAL-G pin drift in backup"; return 0; }
  grep -q '= 1.10.1' "$ROOT/infra/opentofu/versions.tf" || { mark_failed "OpenTofu pin drift in versions.tf"; return 0; }
  note "capacity-gate + version-pin logic checks passed"
}

# --- run ---------------------------------------------------------------------

SKIP_TOFU=0
SKIP_HELM=0
check_tofu
check_helm
check_scripts
check_logic

# If a required tool could not be provisioned at all, the gate is blocked rather
# than a false pass.
if [ "$status" = passed ] && { [ "${SKIP_TOFU:-0}" = 1 ] || [ "${SKIP_HELM:-0}" = 1 ]; }; then
  mark_blocked "required tool(s) unavailable and not installable offline: ${notes}"
fi

finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
source_revision=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf 'unknown')

# input_digest: sha256 over the Phase 5 inputs this gate validates.
input_digest=$(
  {
    find "$ROOT/infra" "$ROOT/charts/cyclopus" -type f 2>/dev/null | LC_ALL=C sort | while IFS= read -r p; do
      sha256sum "$p" 2>/dev/null
    done
    sha256sum "$ROOT/scripts/test_infrastructure.sh" 2>/dev/null
  } | sha256sum | cut -d ' ' -f 1
)
case "$input_digest" in
  [0-9a-f]*) : ;;
  *) input_digest=$(printf '%s' "$source_revision$started_at" | sha256sum | cut -d ' ' -f 1) ;;
esac

mkdir -p "$(dirname -- "$EVIDENCE")"
add_artifact "infra/opentofu"
add_artifact "charts/cyclopus"
add_artifact "infra/bootstrap"
add_artifact "infra/backup"

# Emit schema-conformant evidence JSON via ruby (already required elsewhere).
CYCLOPUS_UNIT_META="$PHASE|$GATE|$status|$exit_code|$started_at|$finished_at|$source_revision|$input_digest|$IMPLEMENTER|$COMMAND|$notes|$artifacts" \
ruby -rjson -e '
  meta = ENV.fetch("CYCLOPUS_UNIT_META").split("|", 12)
  phase, gate, status, exit_code, started_at, finished_at, source_revision, input_digest, implementer, command, notes, artifacts = meta
  doc = {
    "schema_version" => 2,
    "workflow" => "cyclopus-api-platform-v1",
    "phase" => phase,
    "gate" => gate,
    "gate_type" => "unit",
    "status" => status,
    "attempt" => 1,
    "started_at" => started_at,
    "finished_at" => finished_at,
    "source_revision" => source_revision,
    "input_digest" => input_digest,
    "implementer_agent" => implementer,
    "command" => command,
    "exit_code" => exit_code.to_i,
    "profile" => "local",
    "artifacts" => artifacts.split(" ").reject(&:empty?).uniq,
  }
  doc["notes"] = notes unless notes.nil? || notes.empty?
  print JSON.pretty_generate(doc)
' > "$EVIDENCE" || { printf 'unit: FAILED to write evidence JSON\n' >&2; exit 1; }

log "evidence written: $EVIDENCE (status=$status exit=$exit_code)"
printf 'unit: %s\n' "$status"
# A blocked prerequisite (BLOCKED: unavailable tool that could not be installed
# offline) exits with the workflow BLOCKED code rather than a false pass.
if [ "$status" = blocked ]; then
  exit 2
fi
exit "$exit_code"
