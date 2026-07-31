#!/bin/sh
# Bootstrap the control-plane node (VPS1) as a hardened k3s server (§2.2, §7, §9).
#
# Load-bearing settings:
#   --disable=traefik            Traefik version is managed by Cyclopus's Helm
#                                chart, not by k3s upgrades (§9).
#   secrets-encryption: true     Secrets encryption at rest (§7.3, §9).
#   single-stack IPv4            No IPv6; §7.2 policies are IPv4-only.
#   control-plane taint          cyclopus.app/role=control-plane:NoSchedule so
#                                only Cyclopus API + Postgres land here (§2.2).
#   hardened kubelet/API flags   protect-kernel-defaults, anonymous-auth off,
#                                event/audit rate limiting.
#
# Idempotent: re-running keeps an already-correct install and reconciles config.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
detect_distro

# Pod/Service CIDRs are single-stack IPv4. The floating IP (if known) is added
# to the API cert SANs so the kubeconfig can point at the stable address (§9.1).
CLUSTER_CIDR="${CLUSTER_CIDR:-10.42.0.0/16}"
SERVICE_CIDR="${SERVICE_CIDR:-10.43.0.0/16}"
NODE_LABEL_ROLE="${NODE_LABEL_ROLE:-cyclopus.app/role=control-plane}"
TAINT="${CONTROL_PLANE_TAINT:-cyclopus.app/role=control-plane:NoSchedule}"
FLOATING_IP="${FLOATING_IP:-}"

log "writing hardened k3s server config"
$SUDO install -d -m 0755 /etc/rancher/k3s
{
  printf '%s\n' 'disable:'
  printf '%s\n' '  - traefik'
  printf '%s\n' '  - servicelb'
  printf '%s\n' 'secrets-encryption: true'
  printf '%s\n' 'write-kubeconfig-mode: "0600"'
  printf '%s\n' 'flannel-ipv6-masq: false'
  printf 'cluster-cidr: "%s"\n' "$CLUSTER_CIDR"
  printf 'service-cidr: "%s"\n' "$SERVICE_CIDR"
  printf 'node-label:\n  - "%s"\n' "$NODE_LABEL_ROLE"
  printf 'node-taint:\n  - "%s"\n' "$TAINT"
  printf '%s\n' 'kubelet-arg:'
  printf '%s\n' '  - "protect-kernel-defaults=true"'
  printf '%s\n' '  - "streaming-connection-idle-timeout=5m"'
  printf '%s\n' '  - "make-iptables-util-chains=true"'
  printf '%s\n' 'kube-apiserver-arg:'
  printf '%s\n' '  - "anonymous-auth=false"'
  printf '%s\n' '  - "event-ttl=1h"'
  printf '%s\n' 'kube-controller-manager-arg:'
  printf '%s\n' '  - "terminated-pod-gc-threshold=100"'
  if [ -n "$FLOATING_IP" ]; then
    printf 'tls-san:\n  - "%s"\n' "$FLOATING_IP"
  fi
} | $SUDO tee /etc/rancher/k3s/config.yaml >/dev/null

log "installing k3s server $K3S_VERSION"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
observed=$(k3s --version 2>/dev/null | sed -n '1s/^k3s version \([^ ]*\).*/\1/p' || true)
if ! command -v k3s >/dev/null 2>&1 || [ "$observed" != "$K3S_VERSION" ]; then
  retry 3 curl -sfL https://get.k3s.io -o "$tmp_dir/install-k3s.sh" || fail "k3s installer download failed"
  $SUDO env INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_EXEC="server" sh "$tmp_dir/install-k3s.sh"
fi
$SUDO systemctl enable k3s
$SUDO systemctl restart k3s

log "waiting for the k3s API to become ready"
retry 40 $SUDO k3s kubectl get --raw=/readyz >/dev/null || fail "k3s API did not become ready"
observed=$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')
[ "$observed" = "$K3S_VERSION" ] || fail "expected k3s $K3S_VERSION, observed $observed"

log "asserting hardening invariants"
$SUDO k3s kubectl -n kube-system get helmchart traefik >/dev/null 2>&1 && fail "bundled Traefik exists despite --disable=traefik"
retry 10 sh -c '"$0" k3s secrets-encrypt status 2>&1 | grep -q "^Encryption Status: Enabled$"' "$SUDO" \
  || fail "k3s secrets encryption is not enabled"

log "exporting node token and kubeconfig hint"
$SUDO cat /var/lib/rancher/k3s/server/node-token > "$tmp_dir/node-token" 2>/dev/null || true
log "control-plane node bootstrapped; join agents with the node-token and the server URL"
