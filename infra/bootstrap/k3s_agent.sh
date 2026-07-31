#!/bin/sh
# Bootstrap a data-plane node (VPS2-4) as a hardened k3s agent (§2.2, §7).
#
# App pods land here (VPS1 is tainted). gVisor must be installed on this node
# (install_gvisor.sh) so sandboxed pods can start; §7.1's fail-closed policy
# turns a missing runsc into pods that fail to start, not unsandboxed pods.
#
# Required env:
#   K3S_URL    https://<control-plane-or-floating-ip>:6443
#   K3S_TOKEN  the server node-token
#
# Idempotent: an agent already at the pinned version keeps its install.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
detect_distro

: "${K3S_URL:?K3S_URL (https://<server>:6443) is required}"
: "${K3S_TOKEN:?K3S_TOKEN (server node-token) is required}"

log "writing hardened k3s agent config"
$SUDO install -d -m 0755 /etc/rancher/k3s
{
  printf '%s\n' 'kubelet-arg:'
  printf '%s\n' '  - "protect-kernel-defaults=true"'
  printf '%s\n' '  - "streaming-connection-idle-timeout=5m"'
  printf '%s\n' '  - "make-iptables-util-chains=true"'
} | $SUDO tee /etc/rancher/k3s/config.yaml >/dev/null

log "installing k3s agent $K3S_VERSION"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
observed=$(k3s --version 2>/dev/null | sed -n '1s/^k3s version \([^ ]*\).*/\1/p' || true)
if ! command -v k3s >/dev/null 2>&1 || [ "$observed" != "$K3S_VERSION" ]; then
  retry 3 curl -sfL https://get.k3s.io -o "$tmp_dir/install-k3s.sh" || fail "k3s installer download failed"
  $SUDO env INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_EXEC="agent" \
    K3S_URL="$K3S_URL" K3S_TOKEN="$K3S_TOKEN" sh "$tmp_dir/install-k3s.sh"
fi
$SUDO systemctl enable k3s-agent
$SUDO systemctl restart k3s-agent

observed=$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')
[ "$observed" = "$K3S_VERSION" ] || fail "expected k3s $K3S_VERSION, observed $observed"
log "agent joined; ensure gVisor is installed here (install_gvisor.sh)"
