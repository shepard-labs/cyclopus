#!/bin/sh
# Register the `gvisor` (and baseline `runc`) RuntimeClass in the cluster.
# Run once against the control plane after k3s is up and gVisor is installed on
# the app nodes (§7.1). Idempotent via server-side apply.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root

KUBECTL="${KUBECTL:-$SUDO k3s kubectl}"

log "registering gvisor and runc RuntimeClasses"
$KUBECTL apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
handler: runc
YAML

log "RuntimeClasses registered"
