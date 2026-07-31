#!/bin/sh
# Cyclopus upgrade procedure (§9.3).
#
# The control plane is one binary plus one schema, upgraded together by
# `helm upgrade`. Rules made explicit here:
#   - Migrations are embedded and run at boot under an advisory lock (§9.3):
#     no separate migration step, no window where a new binary serves an old
#     schema.
#   - Expand/contract only: the new schema must be backward compatible with the
#     previous release so `helm rollback` (revert the binary) still reads it.
#     There is NO automatic schema downgrade — undoing a migration is a restore
#     (§9.1), not a command. This script refuses a downgrade of appVersion.
#   - Brief downtime, single pod (Recreate strategy): the one API pod is
#     replaced; already-running customer apps served by Traefik are unaffected.
#   - In-flight deploys need no special handling: the interrupted deploy leaves
#     an expired lease, and the startup sweep (§4.7) fails the orphan, restores
#     the previous image, and drains the pending commit as part of normal boot.
#   - Kyverno is sequenced separately and deliberately (§7.1 fail-closed): its
#     upgrade pauses pod admission across app namespaces while it restarts, so
#     it must not ride along incidentally with the Cyclopus upgrade.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
CHART="$ROOT/charts/cyclopus"
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

RELEASE="${CYCLOPUS_RELEASE:-cyclopus}"
NAMESPACE="${CYCLOPUS_NAMESPACE:-cyclopus}"
NEW_VERSION="${CYCLOPUS_NEW_VERSION:-}"
HELM_VALUES="${HELM_VALUES:-}"

command -v helm >/dev/null 2>&1 || fail "helm is required"
[ -n "$NEW_VERSION" ] || fail "CYCLOPUS_NEW_VERSION (target appVersion / image tag) is required"

log "checking the migration is expand/contract compatible (no downgrade)"
current=$(helm get metadata "$RELEASE" -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.appVersion // empty' 2>/dev/null || true)
if [ -n "$current" ]; then
  # Refuse an appVersion downgrade: rollback reverts the binary, so the schema
  # must never move backward on upgrade (§9.3 — no automatic schema downgrade).
  lower=$(printf '%s\n%s\n' "$current" "$NEW_VERSION" | sort -V | head -n 1)
  if [ "$NEW_VERSION" != "$current" ] && [ "$lower" = "$NEW_VERSION" ]; then
    fail "refusing downgrade from $current to $NEW_VERSION; undo a migration via restore (§9.1), not helm"
  fi
  log "upgrading Cyclopus $current -> $NEW_VERSION"
else
  log "installing/upgrading Cyclopus to $NEW_VERSION"
fi

log "helm upgrade (single pod, brief downtime; migrations run at boot under advisory lock)"
set -- upgrade "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
  --set "image.tag=$NEW_VERSION" --wait --timeout 10m
[ -n "$HELM_VALUES" ] && set -- "$@" -f "$HELM_VALUES"
helm "$@" || {
  warn "upgrade failed; consider 'helm rollback $RELEASE' — the previous binary still reads the migrated schema (expand/contract, §9.3)"
  fail "helm upgrade failed"
}

log "waiting for the single API pod to become ready (migrations applied at boot)"
if command -v kubectl >/dev/null 2>&1; then
  # The Deployment name is the chart fullname (templates/_helpers.tpl): when the
  # release name already contains the chart name "cyclopus" the fullname
  # collapses to just the release name, so the default release "cyclopus"
  # produces Deployment "cyclopus" (NOT "cyclopus-cyclopus"). Derive it the same
  # way install_guest.sh does. Prefer the authoritative helm-rendered name.
  DEPLOY=$(helm get manifest "$RELEASE" -n "$NAMESPACE" 2>/dev/null \
    | awk '/^kind: Deployment$/{d=1} d&&/^  name:/{print $2; exit}')
  [ -n "$DEPLOY" ] || DEPLOY="$RELEASE"
  kubectl -n "$NAMESPACE" rollout status "deploy/$DEPLOY" --timeout=5m \
    || fail "API pod did not become ready after upgrade"
fi

log "startup sweep handles any in-flight deploy interrupted by the restart (§4.7)"
log "Cyclopus upgraded to $NEW_VERSION"
log "NOTE: upgrade Kyverno separately and deliberately (§7.1) — its restart pauses pod admission"
