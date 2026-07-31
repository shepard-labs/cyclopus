#!/bin/sh
# Idempotent base-package install for Ubuntu 24.04 / Debian 12 nodes.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
detect_distro

log "installing base packages on $DISTRO $VERSION_ID"
export DEBIAN_FRONTEND=noninteractive
retry 3 $SUDO apt-get update -y || fail "apt metadata update failed"
retry 3 $SUDO apt-get install -y \
  ca-certificates coreutils curl gnupg jq tar iptables \
  || fail "base package installation failed"

log "base packages ready"
