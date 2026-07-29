#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DISTRO=${RECOVERY_DISTRO:-ubuntu}

case "$DISTRO" in
  ubuntu)
    IMAGE=${RECOVERY_IMAGE:-ubuntu:24.04}
    VM=${RECOVERY_VM_NAME:-cyclopus-recovery-ubuntu}
    ;;
  debian)
    IMAGE=${RECOVERY_IMAGE:-debian:13}
    VM=${RECOVERY_VM_NAME:-cyclopus-recovery-debian}
    ;;
  *)
    printf '%s\n' "recovery: RECOVERY_DISTRO must be ubuntu or debian, got '$DISTRO'" >&2
    exit 64
    ;;
esac

command -v orb >/dev/null 2>&1 || {
  printf '%s\n' "recovery: OrbStack CLI 'orb' is required" >&2
  exit 2
}

vm_exists=false
if orb info "$VM" >/dev/null 2>&1; then
  vm_exists=true
fi

if [ "$vm_exists" = true ]; then
  observed=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$ID"')
  if [ "$observed" != "$DISTRO" ] || [ "${RECOVERY_RECREATE:-0}" = 1 ]; then
    printf '%s\n' "recovery: recreating $VM (observed distro '$observed')"
    orb delete --force "$VM"
    vm_exists=false
  else
    printf '%s\n' "recovery: reusing $DISTRO VM $VM"
  fi
fi

if [ "$vm_exists" = false ]; then
  printf '%s\n' "recovery: creating $IMAGE VM $VM"
  orb create --cpus "${RECOVERY_CPUS:-4}" --memory "${RECOVERY_MEMORY:-6G}" --disk "${RECOVERY_DISK:-30G}" "$IMAGE" "$VM"
fi

printf '%s\n' "recovery: provisioning and testing in $VM"
stage="/tmp/cyclopus-recovery-source"
orb -m "$VM" rm -rf "$stage"
COPYFILE_DISABLE=1 tar -C "$ROOT" --exclude='.git' --exclude='.codegraph' -cf - . |
  orb -m "$VM" sh -c "mkdir -p '$stage' && tar -C '$stage' -xf -"
orb -m "$VM" env \
  CYCLOPUS_RECOVERY_ROOT="$stage" \
  CYCLOPUS_RECOVERY_DISTRO="$DISTRO" \
  sh "$stage/scripts/integration/recovery_guest.sh"
