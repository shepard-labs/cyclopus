#!/bin/sh
# Install integration scenario (OrbStack): provision an Ubuntu/Debian VM, run the
# Phase 5 node bootstrap + helm install end to end, and assert the API becomes
# ready, the gvisor RuntimeClass exists, and secrets encryption is enabled.
#
# Follows the deploy_orbstack.sh structure (host side: stage worktree, run the
# guest script in the VM).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DISTRO=${INSTALL_DISTRO:-${DEPLOY_DISTRO:-ubuntu}}

case "$DISTRO" in
  ubuntu)
    IMAGE=${INSTALL_IMAGE:-ubuntu:24.04}
    VM=${INSTALL_VM_NAME:-cyclopus-install-ubuntu}
    ;;
  debian)
    IMAGE=${INSTALL_IMAGE:-debian:12}
    VM=${INSTALL_VM_NAME:-cyclopus-install-debian}
    ;;
  *)
    printf '%s\n' "install: INSTALL_DISTRO must be ubuntu or debian, got '$DISTRO'" >&2
    exit 64
    ;;
esac

if ! command -v orb >/dev/null 2>&1; then
  # orb unavailable: emit schema-valid BLOCKED evidence (like byo_vps_blocked.sh) so
  # the audit trail distinguishes "blocked" from "never attempted" (M4).
  sh "$(dirname -- "$0")/emit_blocked.sh" \
    "install-orbstack-$DISTRO" integration "orbstack_$DISTRO" \
    "install-$DISTRO.json" "OrbStack CLI 'orb' unavailable in this environment" \
    "./scripts/integration.sh install" || true
  printf '%s\n' "install: BLOCKED: OrbStack CLI 'orb' is required" >&2
  exit 2
fi

vm_exists=false
if orb info "$VM" >/dev/null 2>&1; then
  vm_exists=true
fi
if [ "$vm_exists" = true ]; then
  observed=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$ID"')
  observed_version=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$VERSION_ID"')
  expected_version=24.04
  [ "$DISTRO" = debian ] && expected_version=12
  if [ "$observed" != "$DISTRO" ] || [ "$observed_version" != "$expected_version" ] || [ "${INSTALL_RECREATE:-0}" = 1 ]; then
    printf '%s\n' "install: recreating $VM (observed $observed $observed_version)"
    orb delete --force "$VM"
    vm_exists=false
  else
    printf '%s\n' "install: reusing $DISTRO $observed_version VM $VM"
  fi
fi
if [ "$vm_exists" = false ]; then
  printf '%s\n' "install: creating $IMAGE VM $VM"
  orb create --cpus "${INSTALL_CPUS:-4}" --memory "${INSTALL_MEMORY:-6G}" --disk "${INSTALL_DISK:-30G}" "$IMAGE" "$VM"
fi

stage=/tmp/cyclopus-install-source
printf '%s\n' "install: staging dirty worktree and installing in $VM"
orb -m "$VM" rm -rf "$stage"
COPYFILE_DISABLE=1 tar -C "$ROOT" --exclude='.git' --exclude='.codegraph' -cf - . |
  orb -m "$VM" sh -c "mkdir -p '$stage' && tar -C '$stage' -xf -"
orb -m "$VM" env CYCLOPUS_INSTALL_ROOT="$stage" CYCLOPUS_INSTALL_DISTRO="$DISTRO" \
  sh "$stage/scripts/integration/install_guest.sh"
