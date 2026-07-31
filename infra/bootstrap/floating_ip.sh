#!/bin/sh
# Attach/detach the ingress floating IP to this node (§9.1).
#
# The floating IP is the sslip.io-embedded ingress address; DR reattaches it to
# the replacement control-plane node so default hostnames and their certificates
# survive a rebuild without user action.
#
# On OVH Public Cloud the authoritative move is done by the OpenStack API (see
# infra/opentofu floating_ip module / infra/backup/dr_runbook.sh). This script
# handles the node-local side: binding the address to the primary interface so
# Traefik answers on it immediately, and detaching on teardown. Idempotent.
#
# Usage: floating_ip.sh attach <ip> [iface]
#        floating_ip.sh detach <ip> [iface]
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root

action=${1:-}
ip=${2:-}
iface=${3:-}

[ -n "$ip" ] || fail "usage: $0 {attach|detach} <ip> [iface]"

if [ -z "$iface" ]; then
  iface=$(ip -o -4 route show to default 2>/dev/null | awk '{print $5; exit}')
  [ -n "$iface" ] || fail "could not determine default interface; pass one explicitly"
fi

has_addr() {
  ip -o -4 addr show dev "$iface" 2>/dev/null | grep -qwF "$ip"
}

case "$action" in
  attach)
    log "attaching floating IP $ip to $iface"
    if has_addr; then
      log "$ip already present on $iface"
    else
      $SUDO ip addr add "$ip/32" dev "$iface" || fail "failed to add $ip to $iface"
    fi
    # Announce the move so upstream switches update their ARP cache promptly.
    command -v arping >/dev/null 2>&1 && $SUDO arping -c 2 -U -I "$iface" "$ip" >/dev/null 2>&1 || true
    log "floating IP attached"
    ;;
  detach)
    log "detaching floating IP $ip from $iface"
    if has_addr; then
      $SUDO ip addr del "$ip/32" dev "$iface" || fail "failed to remove $ip from $iface"
    else
      log "$ip not present on $iface"
    fi
    log "floating IP detached"
    ;;
  *)
    fail "usage: $0 {attach|detach} <ip> [iface]"
    ;;
esac
