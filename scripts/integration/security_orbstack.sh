#!/bin/sh
set -eu

# shellcheck disable=SC1007
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
IMAGE=${SECURITY_IMAGE:-debian:12}
VM=${SECURITY_VM_NAME:-cyclopus-security-debian12}

case "$IMAGE" in
  debian:12|debian:12.*) ;;
  *)
    printf '%s\n' "security: SECURITY_IMAGE must be Debian 12, got '$IMAGE'" >&2
    exit 64
    ;;
esac

command -v orb >/dev/null 2>&1 || {
  printf '%s\n' "security: OrbStack CLI 'orb' is required" >&2
  exit 2
}

vm_exists=false
if orb info "$VM" >/dev/null 2>&1; then
  vm_exists=true
fi

if [ "$vm_exists" = true ]; then
  # The quoted program is evaluated inside the guest, not by this shell.
  # shellcheck disable=SC2016
  observed=$(orb -m "$VM" sh -c '. /etc/os-release && printf "%s:%s" "$ID" "$VERSION_ID"' 2>/dev/null || true)
  if [ "$observed" != "debian:12" ] || [ "${SECURITY_RECREATE:-0}" = 1 ]; then
    printf '%s\n' "security: recreating $VM (observed '$observed', required 'debian:12')"
    orb delete --force "$VM"
    vm_exists=false
  else
    printf '%s\n' "security: reusing Debian 12 VM $VM"
  fi
fi

if [ "$vm_exists" = false ]; then
  printf '%s\n' "security: creating $IMAGE VM $VM"
  orb create --cpus "${SECURITY_CPUS:-4}" --memory "${SECURITY_MEMORY:-6G}" --disk "${SECURITY_DISK:-30G}" "$IMAGE" "$VM"
fi

printf '%s\n' "security: staging the current worktree and running the Phase 1 API gate in $VM"
stage=/tmp/cyclopus-security-source
orb -m "$VM" rm -rf "$stage"
COPYFILE_DISABLE=1 tar -C "$ROOT" --exclude='.git' --exclude='.codegraph' -cf - . |
  orb -m "$VM" sh -c "mkdir -p '$stage' && tar -C '$stage' -xf -"
orb -m "$VM" env CYCLOPUS_SECURITY_ROOT="$stage" sh "$stage/scripts/integration/security_guest.sh"
