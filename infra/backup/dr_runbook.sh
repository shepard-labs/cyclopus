#!/bin/sh
# Full disaster-recovery runbook for Cyclopus (§9.1).
#
# Total VPS loss recovery, in order:
#   1. Provision/attach a replacement VPS, operator's choice of provider
#      (documented — the operator runs this).
#   2. Reattach the movable ingress IP to the new control-plane node, if held (§9.1).
#   3. helm-install Cyclopus onto the fresh cluster.
#   4. Restore Postgres from WAL-G.
#   5. Trigger POST /admin/reconcile as an org-scoped loop (§9.2) — there is no
#      unscoped "reconcile everything"; a full rebuild loops over org-scoped
#      calls, bounding blast radius and making progress observable.
#
# Because the floating IP is recovered, every default sslip.io hostname resolves
# unchanged and existing certificates stay valid (§9.1 "Hostname survival").
#
# This script drives steps 2-5 (step 1 is documented). Idempotent where possible.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
BOOTSTRAP="$ROOT/infra/bootstrap"

FLOATING_IP="${FLOATING_IP:-}"
CYCLOPUS_NAMESPACE="${CYCLOPUS_NAMESPACE:-cyclopus}"
CYCLOPUS_RELEASE="${CYCLOPUS_RELEASE:-cyclopus}"
ADMIN_API="${CYCLOPUS_ADMIN_API:-http://127.0.0.1:8080}"
ADMIN_TOKEN="${CYCLOPUS_ADMIN_TOKEN:-}"

log "STEP 1 (documented): provision replacement VPS"
cat <<'DOC'
  Provision or attach a replacement VPS (operator's choice of provider) meeting
  the node requirements (64-bit Linux; nested virtualization on app-workload
  nodes for gVisor runsc).
  Then bootstrap the control plane and agents:
    infra/bootstrap/base_packages.sh; infra/bootstrap/k3s_server.sh
    infra/bootstrap/k3s_agent.sh (each worker); infra/bootstrap/install_gvisor.sh
  If the operator holds a movable ingress IP (cloud floating IP, keepalived VIP,
  or BGP-advertised address), reattach it so default hostnames survive DR;
  otherwise default hostnames may change (best-effort DR) and reconcile emits the
  old->new mapping. Custom domains are the durable path.
DOC

log "STEP 2: reattach the movable ingress IP to this control-plane node (if held)"
if [ -n "$FLOATING_IP" ]; then
  sh "$BOOTSTRAP/floating_ip.sh" attach "$FLOATING_IP" \
    || warn "node-local movable IP bind failed; ensure the provider-side association was reapplied"
else
  warn "FLOATING_IP not set; skipping reattach (set it to recover default hostnames unchanged)"
fi

log "STEP 3: helm-install Cyclopus"
command -v helm >/dev/null 2>&1 || fail "helm is required for DR install"
HELM_VALUES="${HELM_VALUES:-}"
set -- upgrade --install "$CYCLOPUS_RELEASE" "$ROOT/charts/cyclopus" \
  --namespace "$CYCLOPUS_NAMESPACE" --create-namespace --wait
[ -n "$HELM_VALUES" ] && set -- "$@" -f "$HELM_VALUES"
helm "$@" || fail "helm install failed"

log "STEP 4: restore Postgres from WAL-G"
# Materialize /etc/cyclopus/walg.env from this outage-path's WAL-G S3 inputs so
# restore.sh's restore_command (which sources that file) works even though the
# fresh node has no ambient WAL-G env. Single source of truth: the WALG_S3_* +
# AWS_* scheme in common.sh (M3).
require_root
walg_env
write_walg_env /etc/cyclopus/walg.env
sh "$SCRIPT_DIR/restore.sh" || fail "WAL-G restore failed"

log "STEP 5: reconcile cluster state from Postgres (org-scoped loop, §9.2)"
[ -n "$ADMIN_TOKEN" ] || fail "CYCLOPUS_ADMIN_TOKEN is required to drive the reconcile loop"
command -v jq >/dev/null 2>&1 || fail "jq is required to enumerate orgs"

# Enumerate orgs, then reconcile each org scope (apply, prune off — a snapshot
# older than the last deploys is stale relative to the cluster; blind pruning
# would delete live workloads that are newer than the backup, §9.2).
orgs=$(curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" "$ADMIN_API/v1/organizations" \
  | jq -r '.organizations[].id' 2>/dev/null) || fail "failed to list organizations"
[ -n "$orgs" ] || warn "no organizations returned; nothing to reconcile"

for org in $orgs; do
  log "reconciling org $org (apply, prune=false)"
  curl -fsS -X POST -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"scope\":\"org\",\"id\":\"$org\",\"dry_run\":false,\"prune\":false}" \
    "$ADMIN_API/admin/reconcile" >/dev/null || fail "reconcile failed for org $org"
done

log "disaster recovery complete: floating IP recovered, Postgres restored, cluster reconciled"
