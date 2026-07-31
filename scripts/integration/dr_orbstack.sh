#!/bin/sh
# Disaster-recovery scenario (OrbStack, host side): provision a VM and run the
# in-VM DR guest which takes a WAL-G backup, simulates loss, restores, runs
# reconcile, and asserts convergence (workflow phase-5 disaster-recovery contract).
#
# A local MinIO container inside the VM stands in for the storage_destinations
# S3 target so the scenario needs no live cloud.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DISTRO=${DR_DISTRO:-ubuntu}

case "$DISTRO" in
  ubuntu)
    IMAGE=${DR_IMAGE:-ubuntu:24.04}
    VM=${DR_VM_NAME:-cyclopus-dr-ubuntu}
    ;;
  debian)
    IMAGE=${DR_IMAGE:-debian:12}
    VM=${DR_VM_NAME:-cyclopus-dr-debian}
    ;;
  *)
    printf '%s\n' "dr: DR_DISTRO must be ubuntu or debian, got '$DISTRO'" >&2
    exit 64
    ;;
esac

if ! command -v orb >/dev/null 2>&1; then
  # orb unavailable: emit schema-valid BLOCKED evidence (like byo_vps_blocked.sh) so
  # the audit trail distinguishes "blocked" from "never attempted" (M4).
  sh "$(dirname -- "$0")/emit_blocked.sh" \
    "dr-orbstack-$DISTRO" integration "orbstack_$DISTRO" \
    "dr-$DISTRO.json" "OrbStack CLI 'orb' unavailable in this environment" \
    "./scripts/integration.sh disaster-recovery" || true
  printf '%s\n' "dr: BLOCKED: OrbStack CLI 'orb' is required" >&2
  exit 2
fi

vm_exists=false
if orb info "$VM" >/dev/null 2>&1; then
  vm_exists=true
fi
if [ "$vm_exists" = true ]; then
  observed=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$ID"')
  if [ "$observed" != "$DISTRO" ] || [ "${DR_RECREATE:-0}" = 1 ]; then
    printf '%s\n' "dr: recreating $VM (observed '$observed')"
    orb delete --force "$VM"
    vm_exists=false
  else
    printf '%s\n' "dr: reusing $DISTRO VM $VM"
  fi
fi
if [ "$vm_exists" = false ]; then
  printf '%s\n' "dr: creating $IMAGE VM $VM"
  orb create --cpus "${DR_CPUS:-4}" --memory "${DR_MEMORY:-6G}" --disk "${DR_DISK:-30G}" "$IMAGE" "$VM"
fi

stage=/tmp/cyclopus-dr-source
printf '%s\n' "dr: staging worktree and running DR scenario in $VM"
orb -m "$VM" rm -rf "$stage"
COPYFILE_DISABLE=1 tar -C "$ROOT" --exclude='.git' --exclude='.codegraph' -cf - . |
  orb -m "$VM" sh -c "mkdir -p '$stage' && tar -C '$stage' -xf -"
orb -m "$VM" env CYCLOPUS_DR_ROOT="$stage" CYCLOPUS_DR_DISTRO="$DISTRO" \
  sh "$stage/scripts/integration/dr_guest.sh"
